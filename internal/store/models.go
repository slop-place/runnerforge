// Package store holds runnerforge's persistent state: the clouds, forges and
// pools an operator configures through the UI, and the machines the controller
// creates from them.
//
// SQLite is the default and needs no external service. Postgres is supported for
// deployments that want it; the schema is deliberately plain enough to work on
// both without dialect-specific features.
package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// wrapJSON annotates a JSON decoding failure with the package it came from, so
// a malformed column is traceable without a stack trace.
func wrapJSON(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("store: decode column: %w", err)
}

// StringList is a []string stored as a JSON column, for labels and CIDR lists.
//
// database/sql interface contract.
//
//nolint:recvcheck // Value on the value type, Scan on the pointer: the
type StringList []string

// Value renders the list as a JSON array for storage.
func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(l))
	return string(b), err
}

// Scan parses a stored JSON array.
func (l *StringList) Scan(v any) error {
	*l = nil
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return wrapJSON(json.Unmarshal([]byte(t), (*[]string)(l)))
	case []byte:
		if len(t) == 0 {
			return nil
		}
		return wrapJSON(json.Unmarshal(t, (*[]string)(l)))
	default:
		return fmt.Errorf("store: cannot scan %T into StringList", v)
	}
}

// Params is a free-form JSON column used for driver-specific settings that
// runnerforge itself does not interpret — a flavor id here, a datacenter there.
//
// database/sql interface contract.
//
//nolint:recvcheck // Value on the value type, Scan on the pointer: the
type Params map[string]any

// Value renders the params as a JSON object for storage.
func (p Params) Value() (driver.Value, error) {
	if p == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]any(p))
	return string(b), err
}

// Scan parses a stored JSON object.
func (p *Params) Scan(v any) error {
	*p = Params{}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return wrapJSON(json.Unmarshal([]byte(t), (*map[string]any)(p)))
	case []byte:
		if len(t) == 0 {
			return nil
		}
		return wrapJSON(json.Unmarshal(t, (*map[string]any)(p)))
	default:
		return fmt.Errorf("store: cannot scan %T into Params", v)
	}
}

// String returns a string-typed param, or "".
func (p Params) String(k string) string {
	if s, ok := p[k].(string); ok {
		return s
	}
	return ""
}

// Configuration records are hard-deleted rather than soft-deleted. What
// happened is recorded in Events; a tombstone row would keep its unique name,
// so a pool could not be recreated under the same name, and would still satisfy
// foreign keys, so the size it referenced could never be removed.

// A note on the Enabled fields below.
//
// They, and Pool.PublicIPv4, deliberately carry no `default:true` tag. GORM omits Go zero values from
// an INSERT, so a column defaulting to true silently rewrites an explicit
// `Enabled: false` into `true` — which would mean a cloud or forge created as
// disabled comes back enabled, and the controller starts provisioning against a
// connection the operator meant to be off. Callers set Enabled explicitly.

// Cloud is a configured provider account.
type Cloud struct {
	ID      uint   `gorm:"primarykey" json:"id"`
	Name    string `gorm:"uniqueIndex;not null" json:"name"`
	Driver  string `gorm:"not null" json:"driver"`
	Enabled bool   `json:"enabled"`

	// Settings are non-secret driver options (region, auth URL, project id).
	Settings Params `gorm:"type:text" json:"settings"`
	// Credentials are encrypted at rest. Never rendered to the UI.
	Credentials Secret `gorm:"type:text" json:"-"`

	// Status is refreshed by a connectivity check so the UI can show whether
	// these credentials actually work, rather than waiting for a job to fail.
	Status        string     `json:"status"`
	StatusDetail  string     `json:"status_detail"`
	StatusCheckAt *time.Time `json:"status_checked_at"`

	Sizes  []Size  `gorm:"constraint:OnDelete:CASCADE" json:"sizes"`
	Images []Image `gorm:"constraint:OnDelete:CASCADE" json:"images"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Size is one entry in a cloud's instance-size catalogue.
//
// Pools reference sizes by name, so "large" can mean a c3-8 on OVHcloud and a
// cpx41 on Hetzner without any pool needing to know.
type Size struct {
	ID      uint   `gorm:"primarykey" json:"id"`
	CloudID uint   `gorm:"index;not null;uniqueIndex:idx_size_cloud_name" json:"cloud_id"`
	Name    string `gorm:"not null;uniqueIndex:idx_size_cloud_name" json:"name"`

	// Spec is what this size means to the driver: {"flavor":"c3-8"} for
	// OpenStack, {"server_type":"cpx41"} for Hetzner, {"cpus":2,"memory":"4g"}
	// for Docker, {"cpu":"2","memory":"4Gi"} for Kubernetes.
	Spec Params `gorm:"type:text" json:"spec"`

	// Descriptive fields, shown in the UI and used for cost display. They are
	// not authoritative — the driver's Spec is.
	VCPUs     int     `json:"vcpus"`
	MemoryMB  int     `json:"memory_mb"`
	DiskGB    int     `json:"disk_gb"`
	HourlyUSD float64 `json:"hourly_usd"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Image is one entry in a cloud's image catalogue, including custom images the
// operator has built.
type Image struct {
	ID      uint   `gorm:"primarykey" json:"id"`
	CloudID uint   `gorm:"index;not null;uniqueIndex:idx_image_cloud_name" json:"cloud_id"`
	Name    string `gorm:"not null;uniqueIndex:idx_image_cloud_name" json:"name"`

	// Spec identifies the image to the driver: {"id":"<uuid>"} or {"name":"..."}.
	Spec Params `gorm:"type:text" json:"spec"`

	// Username is the account cloud-init creates, needed for the SSH path.
	Username string `json:"username"`
	// PreinstalledDocker records whether this image already has a container
	// runtime, which lets the bootstrap script skip a slow install step.
	PreinstalledDocker bool `json:"preinstalled_docker"`

	Notes string `json:"notes"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Forge is a configured connection to a code forge.
type Forge struct {
	ID      uint   `gorm:"primarykey" json:"id"`
	Name    string `gorm:"uniqueIndex;not null" json:"name"`
	Kind    string `gorm:"not null" json:"kind"`
	Enabled bool   `json:"enabled"`

	// Settings hold the non-secret connection details: base URL, and the scope
	// this connection manages runners for (owner/repo, org, group).
	Settings Params `gorm:"type:text" json:"settings"`
	// Credentials are encrypted at rest.
	Credentials Secret `gorm:"type:text" json:"-"`

	// WebhookSecret authenticates inbound webhook deliveries for this forge.
	WebhookSecret Secret `gorm:"type:text" json:"-"`

	Status        string     `json:"status"`
	StatusDetail  string     `json:"status_detail"`
	StatusCheckAt *time.Time `json:"status_checked_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Pool is a set of interchangeable runners: one forge, one cloud, one shape.
type Pool struct {
	ID      uint   `gorm:"primarykey" json:"id"`
	Name    string `gorm:"uniqueIndex;not null" json:"name"`
	Enabled bool   `json:"enabled"`

	ForgeID uint   `gorm:"index;not null" json:"forge_id"`
	Forge   *Forge `json:"forge,omitempty"`
	CloudID uint   `gorm:"index;not null" json:"cloud_id"`
	Cloud   *Cloud `json:"cloud,omitempty"`
	SizeID  uint   `gorm:"index;not null" json:"size_id"`
	Size    *Size  `json:"size,omitempty"`
	ImageID *uint  `gorm:"index" json:"image_id"`
	Image   *Image `json:"image,omitempty"`

	// Labels are the runner labels this pool registers with. A job is served by
	// this pool when the job's required labels are a subset of these.
	Labels StringList `gorm:"type:text" json:"labels"`

	// MinIdle keeps this many runners registered and waiting. Costs money while
	// idle; buys latency.
	MinIdle int `gorm:"default:0" json:"min_idle"`
	// MaxInstances caps concurrent machines. A safety ceiling as much as a
	// scaling knob: it should sit at or below the cloud account's quota.
	MaxInstances int `gorm:"default:5" json:"max_instances"`

	// JobTimeoutSec bounds a single job; the machine self-destructs after it.
	JobTimeoutSec int `gorm:"default:3600" json:"job_timeout_sec"`
	// MaxLifetimeSec is the reaper's hard ceiling. Any machine older than this
	// is destroyed regardless of what the database believes about it.
	MaxLifetimeSec int `gorm:"default:7200" json:"max_lifetime_sec"`

	// ContainerImage is the runner image for container-mode clouds (docker, k8s).
	ContainerImage string `json:"container_image"`

	// Same reasoning as Enabled: no default, or a pool created without a
	// public IP would silently get one.
	PublicIPv4   bool       `json:"public_ipv4"`
	AllowSSHFrom StringList `gorm:"type:text" json:"allow_ssh_from"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// JobTimeout returns the pool's per-job ceiling.
func (p *Pool) JobTimeout() time.Duration {
	return time.Duration(p.JobTimeoutSec) * time.Second
}

// MaxLifetime returns the pool's hard machine-age ceiling.
func (p *Pool) MaxLifetime() time.Duration {
	return time.Duration(p.MaxLifetimeSec) * time.Second
}

// InstanceState is the controller's view of one machine's progress.
//
// The states form a line, not a graph: an instance only ever moves forward, and
// every terminal path ends at Deleted. Keeping it linear is what makes the
// reaper's job decidable — anything not Deleted is something we still owe a
// destroy call for.
type InstanceState string

const (
	// StatePending means the row exists but nothing has been provisioned yet.
	StatePending InstanceState = "pending"
	// StateProvisioning means the forge credential is minted and the cloud
	// create call has been issued.
	StateProvisioning InstanceState = "provisioning"
	// StateBooting means the machine exists and is coming up.
	StateBooting InstanceState = "booting"
	// StateIdle means the runner has registered and is waiting for a job.
	StateIdle InstanceState = "idle"
	// StateBusy means the runner has claimed a job.
	StateBusy InstanceState = "busy"
	// StateDraining means teardown is in progress.
	StateDraining InstanceState = "draining"
	// StateDeleted means the machine and its registration are both gone.
	StateDeleted InstanceState = "deleted"
	// StateFailed means something went wrong. It still owes a destroy, so the
	// reaper treats it exactly like the live states.
	StateFailed InstanceState = "failed"
)

// InstanceStates lists every state a machine can be in, in lifecycle order.
//
// Anything that reports per-state totals needs this: a state with nothing in it
// still has to report zero, because a series that vanishes when a pool goes
// quiet is a series nothing can alert on.
func InstanceStates() []InstanceState {
	return []InstanceState{
		StatePending, StateProvisioning, StateBooting, StateIdle,
		StateBusy, StateDraining, StateFailed, StateDeleted,
	}
}

// Terminal reports whether no further cleanup is owed for this state.
func (s InstanceState) Terminal() bool { return s == StateDeleted }

// Instance is one machine, and the runner registration that belongs to it.
type Instance struct {
	ID     uint   `gorm:"primarykey" json:"id"`
	Name   string `gorm:"uniqueIndex;not null" json:"name"`
	PoolID uint   `gorm:"index;not null" json:"pool_id"`
	Pool   *Pool  `json:"pool,omitempty"`

	State InstanceState `gorm:"index;not null" json:"state"`

	// ProviderID is the cloud's identifier. Empty until the create call returns;
	// a row with a credential but no ProviderID is the one genuinely dangerous
	// gap, which is why the reaper also sweeps by name.
	ProviderID string `gorm:"index" json:"provider_id"`
	// ForgeRunnerID is the forge-side registration, used to clean up a runner
	// whose machine died before it claimed anything.
	ForgeRunnerID string `gorm:"index" json:"forge_runner_id"`

	// JobID is the forge job this machine was created for, when known.
	JobID string `gorm:"index" json:"job_id"`

	PublicIP  string `json:"public_ip"`
	PrivateIP string `json:"private_ip"`

	// SSHPrivateKey is generated per machine for the GitLab path and discarded
	// with it. Encrypted at rest like any other credential.
	SSHKey Secret `gorm:"type:text" json:"-"`

	Error string `json:"error"`

	// HourlyUSD is the rate this machine was launched at, snapshotted from its
	// size. Rates get edited; a bill that already happened does not change, so
	// the rate is copied rather than looked up later.
	HourlyUSD float64 `json:"hourly_usd"`
	// BilledSeconds is how long the machine was billable, and CostUSD is what
	// that came to. Both are set once, when the machine is destroyed.
	BilledSeconds int     `json:"billed_seconds"`
	CostUSD       float64 `json:"cost_usd"`
	// Logs is a truncated tail of the machine's output, captured when it stops
	// or fails. Without this the controller destroys the only evidence of why a
	// run did not work.
	Logs string `json:"logs"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ActiveAt is when the provider first reported the machine running. This is
	// the point billing starts on the clouds that bill by the second — OVHcloud
	// does not charge for time spent building — so it is the honest start for a
	// cost figure, rather than when the row was created.
	ActiveAt    *time.Time `json:"active_at"`
	ReadyAt     *time.Time `json:"ready_at"`
	ClaimedAt   *time.Time `json:"claimed_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	DestroyedAt *time.Time `json:"destroyed_at"`
}

// Event is an audit record, shown in the UI so an operator can see what the
// controller did and why without reading logs.
type Event struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	At         time.Time `gorm:"index" json:"at"`
	Level      string    `json:"level"`
	PoolID     *uint     `gorm:"index" json:"pool_id"`
	InstanceID *uint     `gorm:"index" json:"instance_id"`
	Kind       string    `gorm:"index" json:"kind"`
	Message    string    `json:"message"`
}

// AllModels is the migration set.
func AllModels() []any {
	return []any{
		&Cloud{}, &Size{}, &Image{}, &Forge{}, &Pool{}, &Instance{}, &Event{},
	}
}
