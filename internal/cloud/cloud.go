// Package cloud defines the provider abstraction runnerforge uses to create and
// destroy the short-lived machines that CI jobs run on.
//
// The interface is deliberately small. Everything a provider needs to know about
// a runner arrives in the ProvisionRequest, and the only state runnerforge trusts
// is what List reports back — see the note on ownership below.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by Get and Delete when the instance no longer exists.
// Deleting an already-gone instance is not an error anywhere else in the system;
// providers should return this rather than inventing their own sentinel.
var ErrNotFound = errors.New("instance not found")

// State is the provider-independent lifecycle of a machine. Providers map their
// own vocabulary onto these; the controller never sees a provider's raw status.
type State string

// The lifecycle states a machine can be in, as the controller sees them.
const (
	// StateCreating means the provider has accepted the create call.
	StateCreating State = "creating"
	// StateRunning means the machine is up.
	StateRunning State = "running"
	// StateStopped means the machine halted, usually by powering itself off
	// after finishing its job.
	StateStopped State = "stopped"
	// StateError means the provider reported a failure.
	StateError State = "error"
	// StateGone means the machine no longer exists.
	StateGone State = "gone"
)

// Instance is a machine the provider created for us.
type Instance struct {
	ID        string // provider-assigned identifier
	Name      string // ci-<pool>-<token>, also the ownership fallback
	State     State
	PublicIP  string // may be empty before the machine is running
	PrivateIP string
	CreatedAt time.Time
	Tags      map[string]string // echoed back from ProvisionRequest.Tags
	Err       string            // provider-reported failure detail, if State is StateError
}

// Owner identifies which runnerforge deployment and pool an instance belongs to.
// It is written onto every instance at creation time and is the basis for
// reclaiming machines after a crash: the controller's database can be lost
// entirely and the reaper will still find every machine it is responsible for.
type Owner struct {
	Controller string // stable id for this runnerforge deployment
	Pool       string
}

// Tag keys used for ownership. These are part of the contract with the reaper:
// changing them orphans every machine created by an older build.
const (
	TagController = "runnerforge-controller"
	TagPool       = "runnerforge-pool"
	TagCreatedAt  = "runnerforge-created-at"
)

// Tags renders the owner as provider tags/metadata.
func (o Owner) Tags() map[string]string {
	return map[string]string{
		TagController: o.Controller,
		TagPool:       o.Pool,
	}
}

// Matches reports whether an instance's tags identify it as belonging to o.
// A zero Pool matches any pool belonging to the same controller, which is how
// the reaper sweeps for machines whose pool was deleted from the config.
func (o Owner) Matches(tags map[string]string) bool {
	if tags[TagController] != o.Controller {
		return false
	}
	return o.Pool == "" || tags[TagPool] == o.Pool
}

// Bootstrap carries the runner's single-use credential and launch instructions
// to the machine, in every form a provider might need.
//
// The controller populates all of these from one source of truth and each
// provider consumes whichever matches its CredentialMode. A VM provider writes
// CloudInit; a container or Pod provider sets Env and Args on the container and
// ignores CloudInit entirely. Providers must never try to synthesise one form
// from another.
type Bootstrap struct {
	// CloudInit is a complete #cloud-config document. Used by CredentialCloudInit.
	CloudInit []byte
	// Entrypoint, Args and Env describe the same launch as structured data, for
	// providers that start a process directly. Used by CredentialEnv.
	//
	// Entrypoint overrides the image's own, which matters because the runner
	// images ship an entrypoint that assumes a long-lived daemon.
	Entrypoint []string
	Args       []string
	Env        map[string]string
	// Image to run the runner in, for container-style providers. VM providers
	// use ProvisionRequest.Image instead.
	ContainerImage string
}

// ProvisionRequest describes one machine to create.
type ProvisionRequest struct {
	Name  string
	Owner Owner

	// Size and Image are the operator's abstract names, carried for logging and
	// error messages only. SizeSpec and ImageSpec are the driver-specific
	// meanings the operator recorded against this cloud, and are what the driver
	// actually reads: {"flavor":"c3-8"} on OpenStack, {"server_type":"cpx41"} on
	// Hetzner, {"cpus":2,"memory_mb":4096} on Docker.
	Size      string
	SizeSpec  map[string]any
	Image     string
	ImageSpec map[string]any

	Bootstrap Bootstrap
	Tags      map[string]string // merged with Owner.Tags(); owner keys win
	SSHKey    string            // authorized public key, optional
	Network   NetworkSpec
}

// NetworkSpec is the small subset of networking runnerforge actually needs.
type NetworkSpec struct {
	PublicIPv4 bool
	// AllowSSHFrom restricts inbound 22 to these CIDRs. Empty means no inbound
	// SSH at all, which is the correct setting for every forge except the
	// GitLab docker-autoscaler path.
	AllowSSHFrom []string
}

// CredentialMode is how a provider gets the runner credential onto the machine.
type CredentialMode string

const (
	// CredentialCloudInit means the provider boots a VM image and passes
	// Bootstrap.CloudInit as user-data. Used by openstack, hetzner, digitalocean.
	CredentialCloudInit CredentialMode = "cloud-init"
	// CredentialEnv means the provider starts a container directly from
	// Bootstrap.ContainerImage with Bootstrap.Env and Bootstrap.Args.
	// Used by docker and kubernetes.
	CredentialEnv CredentialMode = "env"
)

// Capabilities lets the controller reject an impossible pool at startup rather
// than discovering the problem when a job is already waiting.
type Capabilities struct {
	// CredentialMode selects which half of Bootstrap this provider reads.
	CredentialMode CredentialMode

	// Tags reports whether the provider can store and return arbitrary key/value
	// metadata on an instance. This is not a nice-to-have: the reaper claims
	// machines by tag, and a provider without it can only fall back to matching
	// on name prefix. See Config.AllowNamePrefixOwnership.
	Tags bool

	SecurityGroups bool
	PublicIPv4     bool
	TypicalBoot    time.Duration
}

// Provider is implemented by each cloud backend.
//
// Implementations must be safe for concurrent use.
type Provider interface {
	// Name is the driver name as it appears in config ("openstack", "docker").
	Name() string

	Capabilities() Capabilities

	// Provision creates a machine and returns as soon as the create call is
	// accepted. It does not wait for the machine to become usable; the
	// controller polls Get for that.
	Provision(ctx context.Context, req ProvisionRequest) (*Instance, error)

	Get(ctx context.Context, id string) (*Instance, error)

	// Delete destroys the machine. It must be idempotent and must return nil
	// (not ErrNotFound) when the machine is already gone, so that a retried
	// teardown converges instead of wedging.
	Delete(ctx context.Context, id string) error

	// List returns every instance the provider believes belongs to owner.
	//
	// This is the reaper's ground truth and the reason runnerforge can lose its
	// database without leaking machines. It must query the provider live, must
	// not consult any local cache, and must include instances in every state
	// including failed and still-building ones.
	List(ctx context.Context, owner Owner) ([]*Instance, error)
}

// SpecString reads a string field from a driver spec, returning "" if absent.
func SpecString(spec map[string]any, key string) string {
	if spec == nil {
		return ""
	}
	switch v := spec[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	return ""
}

// SpecInt reads a numeric field from a driver spec. JSON round-trips numbers as
// float64, so both are accepted.
func SpecInt(spec map[string]any, key string) (int64, bool) {
	if spec == nil {
		return 0, false
	}
	switch v := spec[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// SpecFloat reads a fractional field from a driver spec.
func SpecFloat(spec map[string]any, key string) (float64, bool) {
	if spec == nil {
		return 0, false
	}
	switch v := spec[key].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

// SpecBool reads a boolean field from a driver spec.
func SpecBool(spec map[string]any, key string) bool {
	if spec == nil {
		return false
	}
	b, _ := spec[key].(bool)
	return b
}

// LogProvider is an optional Provider capability: retrieving a machine's console
// or container output.
//
// This is what makes a failed run diagnosable. Without it the controller
// destroys a machine that failed and takes the only evidence with it, leaving an
// operator with a pool that silently produces nothing. Providers that cannot
// offer logs simply do not implement it.
type LogProvider interface {
	// Logs returns up to tail lines of the machine's output. Implementations
	// should return whatever they have rather than failing when a machine is
	// partially gone, since this is called precisely when things went wrong.
	Logs(ctx context.Context, id string, tail int) (string, error)
}

// Driver is a registered cloud backend: how to build one, and what it needs
// configured.
type Driver struct {
	// Name is how config and the UI refer to this driver.
	Name string
	// Title is the human-readable name shown in the UI.
	Title string
	// New builds a provider from a settings map.
	New func(cfg map[string]any) (Provider, error)
	// Schema drives the configuration forms.
	Schema Schema
}

// Registry maps driver names to their registration.
type Registry map[string]Driver

// drivers is the process-wide driver registry, populated by each driver's init.
//
// It is guarded even though registration happens at init, before any reader
// exists. The lock costs nothing on a map read and means the API is safe for
// any caller rather than safe only by convention — the same choice
// database/sql makes for its driver registry.
var (
	driversMu sync.RWMutex
	drivers   = Registry{}
)

// Register adds a driver. It panics on duplicate registration, which can only
// happen through a programming error at init time.
func Register(d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if _, dup := drivers[d.Name]; dup {
		panic("cloud: driver registered twice: " + d.Name)
	}
	drivers[d.Name] = d
}

// DriverByName returns a registered driver.
func DriverByName(name string) (Driver, bool) {
	driversMu.RLock()
	defer driversMu.RUnlock()
	d, ok := drivers[name]
	return d, ok
}

// Drivers returns every registered driver, sorted by name, for the UI.
func Drivers() []Driver {
	driversMu.RLock()
	defer driversMu.RUnlock()
	out := make([]Driver, 0, len(drivers))
	for _, d := range drivers {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DriverNames returns the registered driver names in sorted order, for the
// UI's pickers.
func DriverNames() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	out := make([]string, 0, len(drivers))
	for name := range drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HasDriver reports whether a driver name is registered.
func HasDriver(name string) bool {
	driversMu.RLock()
	defer driversMu.RUnlock()
	_, ok := drivers[name]
	return ok
}

// New constructs a provider by driver name.
func New(name string, cfg map[string]any) (Provider, error) {
	driversMu.RLock()
	d, ok := drivers[name]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown cloud driver %q", name)
	}
	return d.New(cfg)
}
