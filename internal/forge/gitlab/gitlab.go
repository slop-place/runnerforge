// Package gitlab implements the Forge interface for GitLab CI.
//
// GitLab is the awkward one of the three. It has no per-job credential: a runner
// authentication token is reusable by design, and there is no equivalent of
// GitHub's JIT config or Forgejo's ephemeral registration. Nor is there a
// reliable push signal for "a job is waiting" — the job webhook has historically
// not fired on the transition into pending, and the endpoint a runner uses to
// ask for work *consumes* a job, so it cannot be used to measure demand.
//
// runnerforge therefore does two things GitLab does not do for it:
//
//   - single-use is enforced on the machine, with `run-single --max-builds 1`,
//     and by destroying the machine afterwards;
//   - the runner registration is deleted by the reaper rather than by GitLab.
//
// The alternative design is a fleeting plugin driving GitLab's own autoscaler,
// which gets queue semantics for free but requires the controller to hold SSH
// access into every runner machine. That is a worse trade for a controller that
// also has to work on container-mode clouds, so it is not the default; the Forge
// interface leaves room for it as a second provisioning path.
package gitlab

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

func init() { forge.Register(forge.KindGitLab, New) }

// GitLab talks to one GitLab instance at one scope.
type GitLab struct {
	name        string
	url         string // instance URL as the runner will see it
	runnerType  string // project_type, group_type or instance_type
	projectID   string
	groupID     string
	client      *forge.Client
	runnerImage string
	jobImage    string
	untagged    bool
}

// Runner types, as GitLab names them.
const (
	runnerTypeProject  = "project_type"
	runnerTypeGroup    = "group_type"
	runnerTypeInstance = "instance_type"
)

// maxIdleWait caps how long a runner waits for its first job before giving up,
// so a machine created for a job that was cancelled does not sit idle until the
// reaper's lifetime ceiling.
const maxIdleWait = 600

// DefaultRunnerImage is the image used for container-mode clouds.
const DefaultRunnerImage = "gitlab/gitlab-runner:latest"

// DefaultJobImage is the image jobs run in when .gitlab-ci.yml does not say.
const DefaultJobImage = "alpine:latest"

// New builds a GitLab connection. Recognised settings:
//
//	url          instance URL as runners will reach it (required)
//	api_url      instance URL as the controller will reach it (default: url)
//	scope        "project", "group" or "instance" (default "project")
//	project_id   project id or url-encoded path, for project scope
//	group_id     group id, for group scope
//	token        personal or group access token with api scope (required)
//	runner_image container image for container-mode clouds
//	job_image    default image jobs run in
//	run_untagged whether runners accept untagged jobs (default false)
func New(cfg map[string]any) (forge.Forge, error) {
	get := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }

	base := strings.TrimRight(get("url"), "/")
	if base == "" {
		return nil, errors.New("gitlab: url is required")
	}
	apiBase := strings.TrimRight(get("api_url"), "/")
	if apiBase == "" {
		apiBase = base
	}
	token := get("token")
	if token == "" {
		return nil, errors.New("gitlab: token is required")
	}

	g := &GitLab{
		name:        get("name"),
		url:         base,
		projectID:   get("project_id"),
		groupID:     get("group_id"),
		runnerImage: get("runner_image"),
		jobImage:    get("job_image"),
	}
	if g.runnerImage == "" {
		g.runnerImage = DefaultRunnerImage
	}
	if g.jobImage == "" {
		g.jobImage = DefaultJobImage
	}
	if b, ok := cfg["run_untagged"].(bool); ok {
		g.untagged = b
	}

	switch get("scope") {
	case "", "project":
		if g.projectID == "" {
			return nil, errors.New("gitlab: project scope requires project_id")
		}
		g.runnerType = runnerTypeProject
	case "group":
		if g.groupID == "" {
			return nil, errors.New("gitlab: group scope requires group_id")
		}
		g.runnerType = runnerTypeGroup
	case "instance":
		g.runnerType = runnerTypeInstance
	default:
		return nil, fmt.Errorf("gitlab: unknown scope %q (want project, group or instance)", get("scope"))
	}

	h := http.Header{}
	h.Set("Private-Token", token)

	g.client = forge.NewClient(apiBase+"/api/v4", h)
	return g, nil
}

// Name returns the operator-assigned name of this connection.
func (g *GitLab) Name() string { return g.name }

// Kind identifies which forge this connection talks to.
func (g *GitLab) Kind() forge.Kind { return forge.KindGitLab }

// ---- demand ----

type apiJob struct {
	ID       int64    `json:"id"`
	Status   string   `json:"status"`
	Name     string   `json:"name"`
	TagList  []string `json:"tag_list"`
	Ref      string   `json:"ref"`
	Pipeline struct {
		ID int64 `json:"id"`
	} `json:"pipeline"`
}

// Demand lists jobs waiting for a runner.
//
// This polls, because GitLab offers nothing better to a third party: the runner
// job-request endpoint would consume the job it reports, so it cannot be used to
// observe the queue without also emptying it.
func (g *GitLab) Demand(ctx context.Context, labels []string) ([]forge.Job, error) {
	if g.runnerType != runnerTypeProject {
		// Group and instance scope have no cross-project pending-jobs endpoint.
		// Those deployments need one pool per project, or the webhook path.
		return nil, nil
	}
	var raw []apiJob
	path := fmt.Sprintf("/projects/%s/jobs?scope[]=pending&per_page=50",
		url.PathEscape(g.projectID))
	if err := g.client.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("gitlab: list pending jobs: %w", err)
	}
	out := make([]forge.Job, 0, len(raw))
	for _, j := range raw {
		if j.Status != "pending" {
			continue
		}
		out = append(out, forge.Job{
			ID:     strconv.FormatInt(j.ID, 10),
			Labels: j.TagList,
			Ref:    j.Ref,
		})
	}
	return out, nil
}

// ---- registration ----

type createRunnerReq struct {
	RunnerType string `json:"runner_type"`
	// GitLab expects these as numbers. They are configured as strings because
	// a project can equally be addressed by its url-encoded path, so a numeric
	// value is converted rather than quoted.
	ProjectID   any      `json:"project_id,omitempty"`
	GroupID     any      `json:"group_id,omitempty"`
	Description string   `json:"description,omitempty"`
	TagList     []string `json:"tag_list,omitempty"`
	RunUntagged bool     `json:"run_untagged"`
	// Locked keeps a project runner from being shared elsewhere.
	Locked bool `json:"locked"`
}

type createRunnerResp struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
}

// Provision creates a runner and returns its authentication token.
//
// Unlike the other two forges this token is not inherently single-use, so the
// machine is started with --max-builds 1 and the registration is deleted when
// the machine goes away. Both halves matter: the flag stops a second job being
// taken, and the deletion stops the token outliving the machine.
func (g *GitLab) Provision(ctx context.Context, req forge.RunnerRequest) (*forge.Credential, error) {
	body := createRunnerReq{
		RunnerType:  g.runnerType,
		Description: req.Name,
		TagList:     req.Labels,
		RunUntagged: g.untagged,
		Locked:      g.runnerType == runnerTypeProject,
	}
	switch g.runnerType {
	case runnerTypeProject:
		body.ProjectID = numericOrString(g.projectID)
	case runnerTypeGroup:
		body.GroupID = numericOrString(g.groupID)
	}

	var r createRunnerResp
	if err := g.client.Do(ctx, http.MethodPost, "/user/runners", body, &r); err != nil {
		return nil, fmt.Errorf("gitlab: create runner: %w", err)
	}
	return &forge.Credential{
		Kind:      forge.KindGitLab,
		RunnerID:  strconv.FormatInt(r.ID, 10),
		AuthToken: r.Token,
	}, nil
}

// numericOrString returns an int when s is a plain number, so the JSON body
// carries the number GitLab expects rather than a quoted string.
func numericOrString(s string) any {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}

// Deprovision deletes a runner registration.
//
// Unlike the other two forges this is not optional cleanup: GitLab never removes
// runners on its own, so a registration left behind is a token that outlives the
// machine it was minted for.
func (g *GitLab) Deprovision(ctx context.Context, runnerID string) error {
	if runnerID == "" {
		return nil
	}
	err := g.client.Do(ctx, http.MethodDelete, "/runners/"+url.PathEscape(runnerID), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	return err
}

type apiRunner struct {
	ID          int64    `json:"id"`
	Description string   `json:"description"`
	Online      bool     `json:"online"`
	Status      string   `json:"status"`
	TagList     []string `json:"tag_list"`
}

// ListRunners returns the runners registered in this connection's scope.
//
// The name runnerforge uses for a machine is stored in GitLab's description
// field, since GitLab runners have no name of their own. The reaper matches on
// it, which is why it is set on creation and never edited afterwards.
func (g *GitLab) ListRunners(ctx context.Context) ([]forge.Runner, error) {
	var path string
	switch g.runnerType {
	case runnerTypeProject:
		path = fmt.Sprintf("/projects/%s/runners?per_page=100", url.PathEscape(g.projectID))
	case runnerTypeGroup:
		path = fmt.Sprintf("/groups/%s/runners?per_page=100", url.PathEscape(g.groupID))
	default:
		path = "/runners/all?per_page=100"
	}
	var raw []apiRunner
	if err := g.client.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, fmt.Errorf("gitlab: list runners: %w", err)
	}
	out := make([]forge.Runner, 0, len(raw))
	for _, r := range raw {
		out = append(out, forge.Runner{
			ID:     strconv.FormatInt(r.ID, 10),
			Name:   r.Description,
			Labels: r.TagList,
			Online: r.Online,
			Busy:   r.Status == "active" && r.Online,
		})
	}
	return out, nil
}

// ---- bootstrap ----

// Bootstrap turns a credential into the instructions that launch the runner.
func (g *GitLab) Bootstrap(
	cred *forge.Credential, mode cloud.CredentialMode, opts forge.BootstrapOptions,
) (cloud.Bootstrap, error) {
	if cred == nil || cred.AuthToken == "" {
		return cloud.Bootstrap{}, errors.New("gitlab: incomplete credential")
	}
	timeout := opts.JobTimeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	// --wait-timeout bounds how long the runner sits idle before giving up, so
	// a machine created for a job that has since been cancelled does not linger
	// until the reaper's lifetime ceiling.
	waitFor := int(timeout.Seconds())
	waitFor = min(waitFor, maxIdleWait)

	switch mode {
	case cloud.CredentialEnv:
		img := opts.ContainerImage
		if img == "" {
			img = g.runnerImage
		}
		env := map[string]string{
			"CI_SERVER_URL":            g.url,
			"CI_SERVER_TOKEN":          cred.AuthToken,
			"RUNNERFORGE_JOB_IMAGE":    g.jobImage,
			"RUNNERFORGE_WAIT_TIMEOUT": strconv.Itoa(waitFor),
		}
		// The runner starts each job in a container of its own. Those inherit
		// nothing from the runner's own networking, so without this the job
		// cannot resolve the GitLab host and fails while cloning.
		if opts.Network != "" {
			env["DOCKER_NETWORK_MODE"] = opts.Network
		}
		return cloud.Bootstrap{
			ContainerImage: img,
			Entrypoint:     []string{"/bin/sh", "-c"},
			Args: []string{
				`set -e
exec gitlab-runner run-single \
  --executor docker \
  --docker-image "$RUNNERFORGE_JOB_IMAGE" \
  --docker-privileged \
  --max-builds 1 \
  --wait-timeout "$RUNNERFORGE_WAIT_TIMEOUT"`,
			},
			// The runner reads the instance URL and token from these two
			// documented variables, which keeps the token off the command line.
			Env: env,
		}, nil

	case cloud.CredentialCloudInit:
		return cloud.Bootstrap{CloudInit: g.cloudInit(cred, opts, timeout, waitFor)}, nil

	default:
		return cloud.Bootstrap{}, fmt.Errorf("gitlab: unsupported credential mode %q", mode)
	}
}

// cloudInit renders a #cloud-config that runs exactly one job and powers off.
//
// The poweroff is the machine's own dead-man switch: if the controller dies
// between creating this machine and reaping it, the machine still stops billing
// on its own.
func (g *GitLab) cloudInit(cred *forge.Credential, opts forge.BootstrapOptions, timeout time.Duration, waitFor int) []byte {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /opt/runnerforge/env\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: |\n")
	fmt.Fprintf(&b, "      CI_SERVER_URL=%s\n", g.url)
	fmt.Fprintf(&b, "      CI_SERVER_TOKEN=%s\n", cred.AuthToken)

	if opts.SSHKey != "" {
		b.WriteString("ssh_authorized_keys:\n  - ")
		b.WriteString(opts.SSHKey)
		b.WriteString("\n")
	}

	b.WriteString("runcmd:\n")
	fmt.Fprintf(&b, "  - [ sh, -c, \"shutdown -h +%d\" ]\n", int(timeout.Minutes())+shutdownGraceMinutes)
	b.WriteString("  - [ sh, -c, \"command -v docker >/dev/null 2>&1 || (curl -fsSL https://get.docker.com | sh)\" ]\n")
	b.WriteString("  - [ sh, -c, \"command -v gitlab-runner >/dev/null 2>&1 || " +
		"(curl -fsSL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh | bash && " +
		"apt-get install -y gitlab-runner)\" ]\n")
	fmt.Fprintf(&b, "  - [ sh, -c, \". /opt/runnerforge/env && export CI_SERVER_URL CI_SERVER_TOKEN && "+
		"timeout %d gitlab-runner run-single --executor docker --docker-image %s --docker-privileged "+
		"--max-builds 1 --wait-timeout %d; poweroff\" ]\n",
		int(timeout.Seconds()), g.jobImage, waitFor)
	return []byte(b.String())
}
