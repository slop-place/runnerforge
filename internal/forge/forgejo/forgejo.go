// Package forgejo implements the Forge interface for Forgejo Actions.
//
// Forgejo has the cleanest ephemeral model of the three forges: a runner can be
// created through the API with ephemeral set, Forgejo then hands it at most one
// job and deletes the registration itself once that job ends. Nothing on our
// side has to deregister a runner that did its work — only ones that died first.
//
// Requires Forgejo v15 or newer, which is where POST .../actions/runners and
// GET .../actions/runners/jobs appeared at every scope.
package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

// shutdownGraceMinutes is how much slack the machine's own shutdown timer gets
// beyond the job timeout, so the runner has a chance to exit cleanly first.
const shutdownGraceMinutes = 5

func init() {
	forge.Register(forge.Implementation{
		Kind:  forge.KindForgejo,
		Title: "Forgejo Actions",
		New:   New,
		Fields: []cloud.Field{
			{
				Key: "url", Label: "Instance URL", Type: cloud.FieldText, Required: true,
				Placeholder: "https://forgejo.example.com",
				Help:        "As the runner machines will reach it.",
			},
			{
				Key: "api_url", Label: "API URL", Type: cloud.FieldText,
				Help: "Only if the controller reaches the instance at a different " +
					"address than the runners do.",
			},
			{
				Key: "scope", Label: "Scope", Type: cloud.FieldSelect, Required: true,
				Default: scopeRepo,
				Options: []cloud.Option{
					{Value: scopeRepo, Label: "One repository"},
					{Value: scopeOrg, Label: "An organisation"},
					{Value: scopeUser, Label: "A user's repositories"},
					{Value: scopeAdmin, Label: "The whole instance"},
				},
				Help: "Runners are only ever created inside this scope.",
			},
			{Key: "owner", Label: "Owner", Type: cloud.FieldText, Placeholder: "my-org",
				Help: "The account or organisation, for the org and repo scopes."},
			{Key: "repo", Label: "Repository", Type: cloud.FieldText, Placeholder: "my-repo",
				Help: "For the repo scope."},
			{
				Key: "runner_image", Label: "Runner image", Type: cloud.FieldText,
				Placeholder: DefaultRunnerImage,
				Help: "Must be runner 12 or newer: older ones exit immediately when no " +
					"task is available at the instant they start.",
			},
			{
				Key: "token", Label: "API token", Type: cloud.FieldPassword,
				Required: true, Secret: true,
				Help: "Needs permission to manage runners in the chosen scope.",
			},
		},
	})
}

// Forgejo talks to one Forgejo instance at one scope.
type Forgejo struct {
	name string
	// url is the instance address as the *runner* will see it, which is written
	// into the machine's registration file. apiURL is how the *controller*
	// reaches the instance. They are usually the same, but they differ whenever
	// the controller and the runners sit on different networks — which is the
	// normal case for container-mode clouds and for any split-horizon DNS setup.
	url    string
	apiURL string
	scope  string // API path prefix, e.g. /repos/owner/repo or /admin
	client *forge.Client
	// runnerImage is the container image used when the cloud provider runs
	// runners as containers rather than VMs.
	runnerImage string
}

// New builds a Forgejo connection. Recognised settings:
//
//	url          base URL of the instance as runners will reach it (required)
//	api_url      base URL as the controller will reach it (default: url)
//	scope        "admin", "user", "org" or "repo" (default "repo")
//	owner        account or organisation login, for org and repo scopes
//	repo         repository name, for repo scope
//	token        API token with permission to manage runners (required)
//	runner_image container image for container-mode clouds
func New(cfg map[string]any) (forge.Forge, error) {
	get := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }

	base := strings.TrimRight(get("url"), "/")
	if base == "" {
		return nil, errors.New("forgejo: url is required")
	}
	token := get("token")
	if token == "" {
		return nil, errors.New("forgejo: token is required")
	}
	apiBase := strings.TrimRight(get("api_url"), "/")
	if apiBase == "" {
		apiBase = base
	}

	scope, err := scopePath(get("scope"), get("owner"), get("repo"))
	if err != nil {
		return nil, err
	}

	img := get("runner_image")
	if img == "" {
		img = DefaultRunnerImage
	}

	h := http.Header{}
	h.Set("Authorization", "token "+token)

	return &Forgejo{
		name:        get("name"),
		url:         base,
		apiURL:      apiBase,
		scope:       scope,
		client:      forge.NewClient(apiBase+"/api/v1", h),
		runnerImage: img,
	}, nil
}

// DefaultRunnerImage is the image used for container-mode clouds.
//
// Pinned to a major version deliberately: this package depends on the .runner
// file layout and on `one-job` supporting --wait and --handle, which arrived in
// runner 12. Older runners exit immediately when no task is available at the
// instant they start, which turns every launch into a race.
const DefaultRunnerImage = "code.forgejo.org/forgejo/runner:12"

// The scopes a connection can target, as the API paths name them.
const (
	scopeRepo  = "repo"
	scopeOrg   = "org"
	scopeUser  = "user"
	scopeAdmin = "admin"
)

// scopePath builds the API prefix for the configured scope. Getting this wrong
// is the difference between a runner that sees every repository on the instance
// and one that sees a single project, so it is validated rather than assumed.
func scopePath(scope, owner, repo string) (string, error) {
	switch scope {
	case "", scopeRepo:
		if owner == "" || repo == "" {
			return "", errors.New("forgejo: repo scope requires owner and repo")
		}
		return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo), nil
	case scopeOrg:
		if owner == "" {
			return "", errors.New("forgejo: org scope requires owner")
		}
		return "/orgs/" + url.PathEscape(owner), nil
	case scopeUser:
		return "/user", nil
	case scopeAdmin:
		return "/admin", nil
	default:
		return "", fmt.Errorf("forgejo: unknown scope %q (want admin, user, org or repo)", scope)
	}
}

// Name returns the operator-assigned name of this connection.
func (f *Forgejo) Name() string { return f.name }

// Kind identifies which forge this connection talks to.
func (f *Forgejo) Kind() forge.Kind { return forge.KindForgejo }

// ---- demand ----

type apiJob struct {
	ID     int64    `json:"id"`
	Handle string   `json:"handle"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
	RepoID int64    `json:"repo_id"`
}

// Demand returns jobs Forgejo reports as waiting for a runner.
//
// A caveat worth knowing: "waiting" does not always mean "schedulable". A job
// blocked by a concurrency group also reports as waiting, so this can overcount.
// The machine absorbs that: its runner waits for a job to actually become
// available and the pool's timeout eventually reclaims it if none does.
func (f *Forgejo) Demand(ctx context.Context, labels []string) ([]forge.Job, error) {
	path := f.scope + "/actions/runners/jobs"
	if len(labels) > 0 {
		path += "?labels=" + url.QueryEscape(strings.Join(labels, ","))
	}
	var raw []apiJob
	if err := f.client.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("forgejo: list waiting jobs: %w", err)
	}
	out := make([]forge.Job, 0, len(raw))
	for _, j := range raw {
		if j.Status != "waiting" {
			continue
		}
		out = append(out, forge.Job{
			// The handle identifies one attempt at one job, which is what a
			// runner binds to; the numeric id is not attempt-specific.
			ID:     j.Handle,
			Labels: j.RunsOn,
			Repo:   strconv.FormatInt(j.RepoID, 10),
		})
	}
	return out, nil
}

// ---- registration ----

type registerReq struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Ephemeral   bool   `json:"ephemeral"`
}

type registerResp struct {
	ID    int64  `json:"id"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

// Provision creates an ephemeral runner registration.
//
// Ephemeral is enforced by Forgejo itself and cannot be disabled by the runner,
// so a leaked token is worth at most one job.
func (f *Forgejo) Provision(ctx context.Context, req forge.RunnerRequest) (*forge.Credential, error) {
	var r registerResp
	err := f.client.Do(ctx, http.MethodPost, f.scope+"/actions/runners", registerReq{
		Name:        req.Name,
		Description: "runnerforge ephemeral runner",
		Ephemeral:   true,
	}, &r)
	if err != nil {
		return nil, fmt.Errorf("forgejo: create ephemeral runner: %w", err)
	}
	return &forge.Credential{
		Kind:     forge.KindForgejo,
		RunnerID: strconv.FormatInt(r.ID, 10),
		UUID:     r.UUID,
		Token:    r.Token,
	}, nil
}

// Deprovision removes a runner registration. Forgejo deletes ephemeral runners
// itself once they finish a job, so this is only reached for runners whose
// machine died before claiming anything — and a missing runner is success.
func (f *Forgejo) Deprovision(ctx context.Context, runnerID string) error {
	if runnerID == "" {
		return nil
	}
	err := f.client.Do(ctx, http.MethodDelete, f.scope+"/actions/runners/"+url.PathEscape(runnerID), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	return err
}

type apiRunner struct {
	ID        int64    `json:"id"`
	UUID      string   `json:"uuid"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Labels    []string `json:"labels"`
	Ephemeral bool     `json:"ephemeral"`
}

// ListRunners returns the registrations Forgejo currently holds in this scope.
func (f *Forgejo) ListRunners(ctx context.Context) ([]forge.Runner, error) {
	var raw []apiRunner
	if err := f.client.Do(ctx, http.MethodGet, f.scope+"/actions/runners", nil, &raw); err != nil {
		return nil, fmt.Errorf("forgejo: list runners: %w", err)
	}
	out := make([]forge.Runner, 0, len(raw))
	for _, r := range raw {
		out = append(out, forge.Runner{
			ID:     strconv.FormatInt(r.ID, 10),
			Name:   r.Name,
			Labels: r.Labels,
			Online: r.Status == "online" || r.Status == "active",
			Busy:   r.Status == "active",
		})
	}
	return out, nil
}

// ---- bootstrap ----

// runnerFile is the on-disk registration forgejo-runner reads at startup.
//
// The runner has no flags for passing credentials to `one-job`; it reads this
// file. Rather than shell out to `register` (which would mint a second runner),
// runnerforge writes the file directly from the credential the API returned.
// The field names and layout match what `forgejo-runner register` produces.
type runnerFile struct {
	WARNING string   `json:"WARNING"`
	ID      int64    `json:"id"`
	UUID    string   `json:"uuid"`
	Name    string   `json:"name"`
	Token   string   `json:"token"`
	Address string   `json:"address"`
	Labels  []string `json:"labels"`
}

const runnerFileWarning = "Generated by runnerforge. Single-use ephemeral runner."

// runnerLabels renders pool labels into forgejo-runner's label syntax.
//
// A bare label means "run this job in a container from the default image"; the
// `label:docker://image` form pins the image. Labels that already carry a `:`
// are passed through untouched so operators keep full control.
func runnerLabels(labels []string, defaultImage string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if strings.Contains(l, ":") {
			out = append(out, l)
			continue
		}
		out = append(out, l+":docker://"+defaultImage)
	}
	return out
}

// DefaultJobImage is the container image jobs run in when a label does not pin one.
const DefaultJobImage = "node:20-bookworm"

// Bootstrap turns a credential into the instructions that launch the runner.
func (f *Forgejo) Bootstrap(
	cred *forge.Credential, mode cloud.CredentialMode, opts forge.BootstrapOptions,
) (cloud.Bootstrap, error) {
	if cred == nil || cred.UUID == "" || cred.Token == "" {
		return cloud.Bootstrap{}, errors.New("forgejo: incomplete credential")
	}
	id, _ := strconv.ParseInt(cred.RunnerID, 10, 64)
	rf := runnerFile{
		WARNING: runnerFileWarning,
		ID:      id,
		UUID:    cred.UUID,
		Name:    opts.RunnerName,
		Token:   cred.Token,
		Address: f.url,
		Labels:  runnerLabels(opts.Labels, DefaultJobImage),
	}
	blob, err := json.Marshal(rf)
	if err != nil {
		return cloud.Bootstrap{}, fmt.Errorf("forgejo: encode runner file: %w", err)
	}

	switch mode {
	case cloud.CredentialEnv:
		img := opts.ContainerImage
		if img == "" {
			img = f.runnerImage
		}
		// The runner reads .runner from its working directory and the image's
		// own entrypoint expects to run a daemon, so both are overridden: write
		// the credential from the environment, then exec one-job in its place.
		return cloud.Bootstrap{
			ContainerImage: img,
			Entrypoint:     []string{"/bin/sh", "-c"},
			Args: []string{
				`set -e
mkdir -p /tmp/runnerforge && cd /tmp/runnerforge
printf '%s' "$RUNNERFORGE_RUNNER_FILE" > .runner
exec forgejo-runner one-job $RUNNERFORGE_ONE_JOB_ARGS`,
			},
			Env: map[string]string{
				// The credential travels in the environment rather than on the
				// command line, where anything able to read /proc could see it.
				"RUNNERFORGE_RUNNER_FILE":  string(blob),
				"RUNNERFORGE_ONE_JOB_ARGS": strings.Join(oneJobArgs(opts), " "),
			},
		}, nil

	case cloud.CredentialCloudInit:
		return cloud.Bootstrap{CloudInit: f.cloudInit(blob, opts)}, nil

	default:
		return cloud.Bootstrap{}, fmt.Errorf("forgejo: unsupported credential mode %q", mode)
	}
}

// oneJobArgs builds the flags for `forgejo-runner one-job`.
//
// --wait is always passed: a runner that starts a moment before its job becomes
// schedulable would otherwise exit immediately and waste the whole machine.
// --handle pins the runner to the exact job attempt it was created for, which
// is what stops a runner sized and labelled for one job from picking up another.
func oneJobArgs(opts forge.BootstrapOptions) []string {
	args := []string{"--wait"}
	if opts.JobID != "" {
		args = append(args, "--handle", opts.JobID)
	}
	return args
}

// cloudInit renders a #cloud-config that drops the registration on disk and runs
// exactly one job, then powers the machine off.
//
// The poweroff matters: it is the machine's own dead-man switch. If the
// controller dies between creating this machine and reaping it, the machine
// still stops on its own instead of billing indefinitely. The reaper is the
// primary defence; this is the one that works when the reaper is not running.
func (f *Forgejo) cloudInit(runnerFileJSON []byte, opts forge.BootstrapOptions) []byte {
	timeout := opts.JobTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /opt/runnerforge/.runner\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: |\n      ")
	b.Write(runnerFileJSON)
	b.WriteString("\n")

	if opts.SSHKey != "" {
		b.WriteString("ssh_authorized_keys:\n  - ")
		b.WriteString(opts.SSHKey)
		b.WriteString("\n")
	}

	b.WriteString("runcmd:\n")
	// Hard ceiling independent of anything the job does.
	fmt.Fprintf(&b, "  - [ sh, -c, \"shutdown -h +%d\" ]\n", int(timeout.Minutes())+shutdownGraceMinutes)
	b.WriteString("  - [ sh, -c, \"command -v docker >/dev/null 2>&1 || (curl -fsSL https://get.docker.com | sh)\" ]\n")
	fmt.Fprintf(&b, "  - [ sh, -c, \"cd /opt/runnerforge && timeout %d forgejo-runner one-job %s; poweroff\" ]\n",
		int(timeout.Seconds()), strings.Join(oneJobArgs(opts), " "))
	return []byte(b.String())
}
