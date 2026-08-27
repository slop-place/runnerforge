// Package github implements the Forge interface for GitHub Actions.
//
// GitHub's just-in-time runner configuration is the cleanest of the three
// ephemeral mechanisms: one API call returns an opaque blob that a runner
// consumes with `run.sh --jitconfig`, and the resulting runner is bound to a
// single job and removed by GitHub afterwards. The blob expires in one hour,
// which bounds the damage if a machine never boots.
package github

import (
	"context"
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
		Kind:  forge.KindGitHub,
		Title: "GitHub Actions",
		New:   New,
		Fields: []cloud.Field{
			{
				Key: "url", Label: "API URL", Type: cloud.FieldText,
				Default: "https://api.github.com",
				Help:    "Change only for GitHub Enterprise Server.",
			},
			{
				Key: "scope", Label: "Scope", Type: cloud.FieldSelect, Required: true,
				Default: scopeRepo,
				Options: []cloud.Option{
					{Value: scopeRepo, Label: "One repository"},
					{Value: scopeOrg, Label: "An organisation"},
				},
			},
			{Key: "owner", Label: "Owner", Type: cloud.FieldText, Required: true,
				Placeholder: "my-org"},
			{Key: "repo", Label: "Repository", Type: cloud.FieldText, Placeholder: "my-repo",
				Help: "For the repo scope."},
			{
				Key: "runner_group_id", Label: "Runner group", Type: cloud.FieldNumber,
				Default: "1", Help: "1 is the Default group.",
			},
			{
				Key: "runner_image", Label: "Runner image", Type: cloud.FieldText,
				Placeholder: DefaultRunnerImage,
			},
			{
				Key: "token", Label: "Token", Type: cloud.FieldPassword,
				Required: true, Secret: true,
				Help: "A repo-scoped token is enough for the repo scope; an org scope " +
					"needs admin:org.",
			},
		},
	})
}

// GitHub talks to one GitHub installation at one scope.
type GitHub struct {
	name   string
	scope  string // API path prefix: /repos/owner/repo or /orgs/org
	group  int
	client *forge.Client
	// runnerImage is used when the cloud provider runs runners as containers.
	runnerImage string
}

// DefaultRunnerImage is the container image used for container-mode clouds.
// This is the image GitHub's own Actions Runner Controller uses.
const DefaultRunnerImage = "ghcr.io/actions/actions-runner:latest"

// The scopes a connection can target.
const (
	scopeRepo = "repo"
	scopeOrg  = "org"
)

// New builds a GitHub connection. Recognised settings:
//
//	url             API base URL (default https://api.github.com; set for GHES)
//	scope           "repo" or "org" (default "repo")
//	owner           org or repository owner (required)
//	repo            repository name, for repo scope
//	token           PAT or installation token (required)
//	runner_group_id runner group to place runners in (default 1, "Default")
//	runner_image    container image for container-mode clouds
func New(cfg map[string]any) (forge.Forge, error) {
	get := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }

	base := strings.TrimRight(get("url"), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	token := get("token")
	if token == "" {
		return nil, errors.New("github: token is required")
	}
	owner := get("owner")
	if owner == "" {
		return nil, errors.New("github: owner is required")
	}

	var scope string
	switch get("scope") {
	case "", scopeRepo:
		repo := get("repo")
		if repo == "" {
			return nil, errors.New("github: repo scope requires repo")
		}
		scope = "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	case scopeOrg:
		scope = "/orgs/" + url.PathEscape(owner)
	default:
		return nil, fmt.Errorf("github: unknown scope %q (want repo or org)", get("scope"))
	}

	group := 1
	if g := get("runner_group_id"); g != "" {
		n, err := strconv.Atoi(g)
		if err != nil {
			return nil, fmt.Errorf("github: runner_group_id must be a number: %w", err)
		}
		group = n
	}

	img := get("runner_image")
	if img == "" {
		img = DefaultRunnerImage
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-Github-Api-Version", "2022-11-28")

	return &GitHub{
		name:        get("name"),
		scope:       scope,
		group:       group,
		client:      forge.NewClient(base, h),
		runnerImage: img,
	}, nil
}

// Name returns the operator-assigned name of this connection.
func (g *GitHub) Name() string { return g.name }

// Kind identifies which forge this connection talks to.
func (g *GitHub) Kind() forge.Kind { return forge.KindGitHub }

// ---- demand ----

type wfRun struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type wfJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
	Name   string   `json:"name"`
	Repo   string   `json:"repository_full_name"`
}

// Demand lists queued jobs by polling.
//
// Polling is the fallback path. GitHub's `workflow_job` webhook is both faster
// and cheaper on rate limit, and when one is configured the controller learns
// about work from there instead. This exists so a deployment that cannot expose
// a public endpoint still functions, and so the webhook path has something to
// reconcile against if a delivery is missed.
func (g *GitHub) Demand(ctx context.Context, labels []string) ([]forge.Job, error) {
	var runs struct {
		Runs []wfRun `json:"workflow_runs"`
	}
	// Only queued runs can contain queued jobs, so this bounds the fan-out
	// below to work that is actually waiting.
	err := g.client.Do(ctx, http.MethodGet, g.scope+"/actions/runs?status=queued&per_page=30", nil, &runs)
	if err != nil {
		if g.scope[:6] == "/orgs/" {
			// Org scope has no runs endpoint; webhooks are required there.
			return nil, nil
		}
		return nil, fmt.Errorf("github: list queued runs: %w", err)
	}

	var out []forge.Job
	for _, r := range runs.Runs {
		var jobs struct {
			Jobs []wfJob `json:"jobs"`
		}
		p := fmt.Sprintf("/actions/runs/%d/jobs?filter=latest&per_page=50", r.ID)
		if err := g.client.Do(ctx, http.MethodGet, g.scope+p, nil, &jobs); err != nil {
			return nil, fmt.Errorf("github: list jobs for run %d: %w", r.ID, err)
		}
		for _, j := range jobs.Jobs {
			if j.Status != "queued" {
				continue
			}
			out = append(out, forge.Job{
				ID:     strconv.FormatInt(j.ID, 10),
				Labels: j.Labels,
				Repo:   j.Repo,
			})
		}
	}
	return out, nil
}

// ---- registration ----

type jitReq struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

type jitResp struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// Provision mints a just-in-time runner configuration.
//
// JIT runners are always ephemeral — there is no way to ask for a reusable one
// through this endpoint — so single-use is guaranteed by GitHub rather than by
// anything runnerforge does.
func (g *GitHub) Provision(ctx context.Context, req forge.RunnerRequest) (*forge.Credential, error) {
	labels := req.Labels
	if len(labels) == 0 {
		return nil, errors.New("github: at least one label is required")
	}
	var r jitResp
	err := g.client.Do(ctx, http.MethodPost, g.scope+"/actions/runners/generate-jitconfig", jitReq{
		Name:          req.Name,
		RunnerGroupID: g.group,
		Labels:        labels,
	}, &r)
	if err != nil {
		return nil, fmt.Errorf("github: generate jitconfig: %w", err)
	}
	return &forge.Credential{
		Kind:      forge.KindGitHub,
		RunnerID:  strconv.FormatInt(r.Runner.ID, 10),
		JITConfig: r.EncodedJITConfig,
		// GitHub documents a one-hour validity for the returned config.
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// Deprovision removes a runner registration. GitHub retires JIT runners itself
// once they finish a job, so this is only reached for ones that never started.
func (g *GitHub) Deprovision(ctx context.Context, runnerID string) error {
	if runnerID == "" {
		return nil
	}
	err := g.client.Do(ctx, http.MethodDelete,
		g.scope+"/actions/runners/"+url.PathEscape(runnerID), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	return err
}

type apiRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListRunners returns the registrations GitHub currently holds in this scope.
func (g *GitHub) ListRunners(ctx context.Context) ([]forge.Runner, error) {
	var resp struct {
		Runners []apiRunner `json:"runners"`
	}
	if err := g.client.Do(ctx, http.MethodGet, g.scope+"/actions/runners?per_page=100", nil, &resp); err != nil {
		return nil, fmt.Errorf("github: list runners: %w", err)
	}
	out := make([]forge.Runner, 0, len(resp.Runners))
	for _, r := range resp.Runners {
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, forge.Runner{
			ID:     strconv.FormatInt(r.ID, 10),
			Name:   r.Name,
			Labels: labels,
			Busy:   r.Busy,
			Online: r.Status == "online",
		})
	}
	return out, nil
}

// ---- bootstrap ----

// Bootstrap turns a credential into the instructions that launch the runner.
func (g *GitHub) Bootstrap(
	cred *forge.Credential, mode cloud.CredentialMode, opts forge.BootstrapOptions,
) (cloud.Bootstrap, error) {
	if cred == nil || cred.JITConfig == "" {
		return cloud.Bootstrap{}, errors.New("github: incomplete credential")
	}
	switch mode {
	case cloud.CredentialEnv:
		img := opts.ContainerImage
		if img == "" {
			img = g.runnerImage
		}
		// The official runner image already contains run.sh; passing the config
		// through the environment keeps it off the process command line, where
		// it would be visible to anything that can read /proc.
		return cloud.Bootstrap{
			ContainerImage: img,
			Entrypoint:     []string{"/bin/bash", "-c"},
			Args: []string{
				`set -e
cd "${RUNNER_HOME:-/home/runner}"
exec ./run.sh --jitconfig "$RUNNERFORGE_JITCONFIG"`,
			},
			Env: map[string]string{"RUNNERFORGE_JITCONFIG": cred.JITConfig},
		}, nil

	case cloud.CredentialCloudInit:
		return cloud.Bootstrap{CloudInit: g.cloudInit(cred, opts)}, nil

	default:
		return cloud.Bootstrap{}, fmt.Errorf("github: unsupported credential mode %q", mode)
	}
}

// cloudInit renders a #cloud-config that installs the runner if the image does
// not already carry it, runs exactly one job, then powers the machine off.
//
// The poweroff is the machine's own dead-man switch: if the controller dies
// between creating this machine and reaping it, the machine still stops billing
// on its own. The reaper is the primary defence; this is the one that survives
// the reaper not running.
func (g *GitHub) cloudInit(cred *forge.Credential, opts forge.BootstrapOptions) []byte {
	timeout := opts.JobTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /opt/runnerforge/jitconfig\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: ")
	b.WriteString(cred.JITConfig)
	b.WriteString("\n")

	if opts.SSHKey != "" {
		b.WriteString("ssh_authorized_keys:\n  - ")
		b.WriteString(opts.SSHKey)
		b.WriteString("\n")
	}

	b.WriteString("runcmd:\n")
	fmt.Fprintf(&b, "  - [ sh, -c, \"shutdown -h +%d\" ]\n", int(timeout.Minutes())+shutdownGraceMinutes)
	b.WriteString("  - [ sh, -c, \"command -v docker >/dev/null 2>&1 || (curl -fsSL https://get.docker.com | sh)\" ]\n")
	const runLine = "  - [ sh, -c, \"cd /opt/actions-runner && timeout %d " +
		"./run.sh --jitconfig \\\"$(cat /opt/runnerforge/jitconfig)\\\"; poweroff\" ]\n"
	fmt.Fprintf(&b, runLine, int(timeout.Seconds()))
	return []byte(b.String())
}
