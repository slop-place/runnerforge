// Package forge defines the abstraction over the code forges runnerforge serves:
// GitHub Actions, GitLab CI and Forgejo Actions.
//
// All three converged on the same primitive — hand out a credential that is good
// for exactly one job, let a runner claim that job, then discard the runner — so
// the interface below is shaped around that lifecycle rather than around any one
// forge's API.
package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
)

// ErrNotFound is returned when a runner registration no longer exists on the
// forge. Like cloud.ErrNotFound this makes teardown idempotent.
var ErrNotFound = errors.New("runner not found")

// Kind identifies a forge implementation.
type Kind string

const (
	KindGitHub  Kind = "github"
	KindGitLab  Kind = "gitlab"
	KindForgejo Kind = "forgejo"
)

// Job is a unit of CI work waiting for a runner.
type Job struct {
	// ID is the forge's identifier for the job. It is used to deduplicate
	// demand across polls and webhook deliveries, so it must be stable.
	ID string
	// Labels are the runner labels/tags the job requires.
	Labels []string
	Repo   string
	Ref    string
	// QueuedAt is when the forge says the job started waiting. Zero if unknown.
	QueuedAt time.Time
}

// Credential is a single-use runner registration, valid for exactly one job.
//
// The fields are a union across the three forges; which are populated depends on
// Kind. Callers should not read them directly — pass the Credential back to
// Forge.Bootstrap, which knows how to turn it into a machine launch.
type Credential struct {
	Kind Kind

	// RunnerID is the forge-side handle used to deprovision a runner that never
	// claimed a job. Empty when the forge cleans up on its own and exposes no id.
	RunnerID string

	// GitHub: the base64 --jitconfig blob. Always ephemeral, expires in one hour.
	JITConfig string

	// Forgejo: the runner's UUID and secret from the ephemeral registration.
	UUID  string
	Token string

	// GitLab: a runner authentication token. Unlike the other two this is not
	// inherently single-use; runnerforge enforces one job via --max-builds 1.
	AuthToken string

	// ExpiresAt is when the credential stops being usable, if the forge says so.
	ExpiresAt time.Time
}

// RunnerRequest asks a forge for a new single-use credential.
type RunnerRequest struct {
	Name   string
	Labels []string
	// JobID, when set, is the specific job this runner is being created for.
	// Forgejo can bind a runner to one job with it; the others ignore it.
	JobID string
}

// Runner is a registration that currently exists on the forge. Used by the
// reaper to find registrations whose machine has disappeared.
type Runner struct {
	ID     string
	Name   string
	Labels []string
	Busy   bool
	Online bool
}

// Forge is implemented by each code forge.
//
// Implementations must be safe for concurrent use.
type Forge interface {
	// Name is the operator-assigned name of this connection, so that two
	// connections to the same forge kind stay distinguishable in logs and UI.
	Name() string
	Kind() Kind

	// Demand returns the jobs currently waiting for a runner with these labels.
	//
	// For forges with no queue API this may be driven by webhooks instead; see
	// each implementation. Returning an empty slice with a nil error means
	// "nothing is waiting", which is different from an error and must not cause
	// the controller to scale down anything already in flight.
	Demand(ctx context.Context, labels []string) ([]Job, error)

	// Provision mints a credential good for one job.
	Provision(ctx context.Context, req RunnerRequest) (*Credential, error)

	// Deprovision removes a runner registration. It must be idempotent and
	// return nil when the registration is already gone — the common case, since
	// all three forges delete ephemeral runners themselves once a job finishes.
	Deprovision(ctx context.Context, runnerID string) error

	// ListRunners returns the registrations the forge currently holds for this
	// connection's scope. Used to find registrations orphaned by a machine that
	// died before claiming its job.
	ListRunners(ctx context.Context) ([]Runner, error)

	// Bootstrap turns a credential into the instructions that launch the runner
	// on a machine. It fills whichever half of cloud.Bootstrap matches mode, so
	// that forge-specific launch details stay inside the forge package.
	Bootstrap(cred *Credential, mode cloud.CredentialMode, opts BootstrapOptions) (cloud.Bootstrap, error)
}

// BootstrapOptions carries machine-side settings into the launch instructions.
type BootstrapOptions struct {
	// RunnerName is the name the runner registers under; matches Credential's request.
	RunnerName string
	Labels     []string
	// SSHKey, if set, is added to the machine's authorized_keys. Only the GitLab
	// docker-autoscaler path needs this.
	SSHKey string
	// JobID binds this runner to one specific queued job. Forgejo supports this
	// directly and it removes an entire class of race: without it a runner
	// created for job A can be handed job B, and the machine sized for A ends
	// up running something else.
	JobID string
	// JobTimeout bounds how long the machine may live even if nothing else
	// tears it down; rendered into a self-destruct timer on the machine.
	JobTimeout time.Duration
	// ContainerImage is the runner image for container-mode providers.
	ContainerImage string
}

// Registry maps forge kinds to constructors.
type Registry map[Kind]func(cfg map[string]any) (Forge, error)

// Forges is the process-wide forge registry, populated by each package's init.
var Forges = Registry{}

// Register adds a forge constructor. Panics on duplicate registration.
func Register(k Kind, fn func(cfg map[string]any) (Forge, error)) {
	if _, dup := Forges[k]; dup {
		panic("forge: registered twice: " + string(k))
	}
	Forges[k] = fn
}

// New constructs a forge by kind.
func New(k Kind, cfg map[string]any) (Forge, error) {
	fn, ok := Forges[k]
	if !ok {
		return nil, fmt.Errorf("unknown forge kind %q", k)
	}
	return fn(cfg)
}
