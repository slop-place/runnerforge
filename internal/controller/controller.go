// Package controller runs the reconcile loop that turns queued CI jobs into
// machines, and the reaper that guarantees those machines go away again.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

const (
	// nameRandomBytes is the entropy in a machine name's suffix. The name is
	// the reaper's ownership fallback, so it has to be collision-free.
	nameRandomBytes = 5
	// maxPoolNameInMachineName keeps generated names within the length limits
	// every provider imposes.
	maxPoolNameInMachineName = 24
	// lowerCaseOffset converts an ASCII upper-case letter to lower case.
	lowerCaseOffset = 'a' - 'A'
)

// Controller owns the reconcile and reap loops.
type Controller struct {
	db  *store.DB
	cfg *config.Config
	res *resolver
	log *slog.Logger

	// mu serialises reconcile passes so a slow cloud API cannot cause two
	// passes to overlap and double-provision for the same demand.
	mu sync.Mutex
}

// New builds a controller.
func New(db *store.DB, cfg *config.Config, log *slog.Logger) *Controller {
	return &Controller{db: db, cfg: cfg, res: newResolver(), log: log}
}

// Owner returns the ownership tag for a pool under this deployment.
func (c *Controller) Owner(pool string) cloud.Owner {
	return cloud.Owner{Controller: c.cfg.ID, Pool: pool}
}

// Run drives both loops until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	reconcile := time.NewTicker(c.cfg.ReconcileInterval)
	defer reconcile.Stop()
	reap := time.NewTicker(c.cfg.ReapInterval)
	defer reap.Stop()

	c.log.Info("controller started",
		"id", c.cfg.ID,
		"reconcile", c.cfg.ReconcileInterval,
		"reap", c.cfg.ReapInterval)

	// Reap immediately on startup. If the previous process died mid-flight
	// there may already be machines running with nothing watching them, and
	// they are billing for every second we wait.
	if err := c.Reap(ctx); err != nil {
		c.log.Error("startup reap failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("controller stopped: %w", ctx.Err())
		case <-reconcile.C:
			if err := c.ReconcileAll(ctx); err != nil {
				c.log.Error("reconcile failed", "err", err)
			}
		case <-reap.C:
			if err := c.Reap(ctx); err != nil {
				c.log.Error("reap failed", "err", err)
			}
		}
	}
}

// ReconcileAll runs one pass over every enabled pool.
func (c *Controller) ReconcileAll(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	start := time.Now()

	pools, err := c.db.EnabledPools(ctx)
	if err != nil {
		metrics.ReconcilePass(time.Since(start), err)
		return err
	}
	var errs []error
	for i := range pools {
		p := &pools[i]
		poolStart := time.Now()
		err := c.reconcilePool(ctx, p)
		metrics.PoolReconcile(p.Name, time.Since(poolStart), err)
		if err != nil {
			// One broken pool must not stop the others; a pool whose forge is
			// unreachable should not prevent another pool's machines from
			// being torn down.
			errs = append(errs, fmt.Errorf("pool %s: %w", p.Name, err))
			c.db.Logf(ctx, "error", "reconcile", &p.ID, nil, "%v", err)
		}
	}
	joined := errors.Join(errs...)
	metrics.ReconcilePass(time.Since(start), joined)
	return joined
}

// reconcilePool advances every machine in one pool, then scales to demand.
func (c *Controller) reconcilePool(ctx context.Context, pool *store.Pool) error {
	prov, err := c.res.Cloud(pool.Cloud)
	if err != nil {
		return err
	}
	fg, err := c.res.Forge(pool.Forge)
	if err != nil {
		return err
	}

	live, err := c.db.LiveInstances(ctx, pool.ID)
	if err != nil {
		return err
	}

	// Take one snapshot of the forge's runner list per pass rather than asking
	// per machine. Disappearance from this list is how we learn a job finished,
	// so it is read once and shared to keep that judgement consistent.
	snap := c.snapshotRunners(ctx, fg)

	// Advance existing machines first. This releases capacity that the scale-up
	// below might otherwise think it needs to create.
	for i := range live {
		if err := c.advance(ctx, pool, prov, fg, snap, &live[i]); err != nil {
			c.log.Warn("advance instance", "instance", live[i].Name, "err", err)
		}
	}

	// Re-read: advance may have deleted some.
	live, err = c.db.LiveInstances(ctx, pool.ID)
	if err != nil {
		return err
	}

	jobs, err := fg.Demand(ctx, pool.Labels)
	if err != nil {
		return fmt.Errorf("demand: %w", err)
	}

	plan := planScale(pool, matchingJobs(jobs, pool.Labels), live)
	metrics.ScaleDecision(pool.Name, plan.queued, plan.uncovered, plan.atCeiling)
	if plan.atCeiling > 0 {
		c.db.Logf(ctx, "warn", "scale", &pool.ID, nil,
			"%d job(s) waiting but pool is at its ceiling of %d machines",
			plan.atCeiling, pool.MaxInstances)
	}
	if len(plan.forJobs) == 0 && plan.warm == 0 {
		return nil
	}

	c.log.Info("scaling up", "pool", pool.Name,
		"queued", plan.queued, "uncovered", plan.uncovered,
		"idle", plan.idle, "launching", len(plan.forJobs), "warm", plan.warm)

	for _, j := range plan.forJobs {
		metrics.Launch(pool.Name, "job")
		if err := c.launch(ctx, pool, prov, fg, j.ID); err != nil {
			c.db.Logf(ctx, "error", "launch", &pool.ID, nil, "launch failed: %v", err)
			return err
		}
	}
	for range plan.warm {
		metrics.Launch(pool.Name, "warm")
		if err := c.launch(ctx, pool, prov, fg, ""); err != nil {
			c.db.Logf(ctx, "error", "launch", &pool.ID, nil, "warm launch failed: %v", err)
			return err
		}
	}
	return nil
}

// scalePlan is what one reconcile pass decided to do about a pool.
type scalePlan struct {
	// forJobs are the jobs to create a machine for, each bound to that job.
	forJobs []forge.Job
	// warm is how many unbound machines to add to reach the pool's min_idle.
	warm int

	// The rest is reporting: what the decision was based on.
	queued    int
	uncovered int
	idle      int
	// atCeiling is how many jobs had to be left waiting because the pool is
	// already at max_instances.
	atCeiling int
}

// planScale decides how many machines to create, and for which jobs.
//
// It is a pure function of the queue and the pool's current machines, which is
// what makes the scaling rules testable without a cloud or a forge. The rule
// that matters most: demand is measured as jobs nothing is working on yet, not
// as queue length, so a second machine is never launched for a job that already
// has one.
func planScale(pool *store.Pool, matching []forge.Job, live []store.Instance) scalePlan {
	plan := scalePlan{queued: len(matching)}

	covered := make(map[string]bool, len(live))
	var total int
	for _, in := range live {
		if in.JobID != "" {
			covered[in.JobID] = true
		}
		switch in.State {
		case store.StatePending, store.StateProvisioning, store.StateBooting, store.StateIdle:
			if in.JobID == "" {
				plan.idle++ // an unbound warm machine, able to take anything
			}
			total++
		case store.StateBusy, store.StateDraining, store.StateFailed:
			total++
		case store.StateDeleted:
			// Holds no resources and is excluded from LiveInstances anyway.
		}
	}

	uncovered := make([]forge.Job, 0, len(matching))
	for _, j := range matching {
		if !covered[j.ID] {
			uncovered = append(uncovered, j)
		}
	}
	plan.uncovered = len(uncovered)

	// Warm machines soak up demand before anything new is created.
	toLaunch := uncovered
	if plan.idle >= len(toLaunch) {
		toLaunch = nil
	} else if plan.idle > 0 {
		toLaunch = toLaunch[plan.idle:]
	}

	// Never exceed the pool ceiling, which is also the account-quota guard.
	room := max(pool.MaxInstances-total, 0)
	if len(toLaunch) > room {
		plan.atCeiling = len(toLaunch) - room
		toLaunch = toLaunch[:room]
	}
	plan.forJobs = toLaunch

	// Top up to min_idle with unbound machines once demand is covered.
	if spare := room - len(toLaunch); spare > 0 {
		if want := pool.MinIdle - plan.idle; want > 0 {
			plan.warm = min(want, spare)
		}
	}
	return plan
}

// matchingJobs returns the jobs this pool's labels can satisfy.
//
// A pool serves a job when it offers every label the job asks for. Extra labels
// on the pool are fine; a missing one is not.
func matchingJobs(jobs []forge.Job, poolLabels []string) []forge.Job {
	have := make(map[string]bool, len(poolLabels))
	for _, l := range poolLabels {
		have[normalizeLabel(l)] = true
	}
	out := make([]forge.Job, 0, len(jobs))
	for _, j := range jobs {
		ok := true
		for _, need := range j.Labels {
			if !have[normalizeLabel(need)] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, j)
		}
	}
	return out
}

// normalizeLabel strips a Forgejo-style `label:docker://image` suffix so that a
// pool configured with an image-pinned label still matches a job that just asks
// for the bare label.
func normalizeLabel(l string) string {
	if i := strings.Index(l, ":"); i > 0 {
		return strings.ToLower(l[:i])
	}
	return strings.ToLower(l)
}

// launch creates one machine for a pool.
//
// The ordering here is the safety property that matters most in this file. The
// database row is written before anything is created at the forge or the cloud,
// and its name is what the reaper matches on. A crash at any point after this
// leaves a record that the reaper can act on; a crash before it leaves nothing,
// because nothing exists yet.
func (c *Controller) launch(ctx context.Context, pool *store.Pool, prov cloud.Provider, fg forge.Forge, jobID string) error {
	name, err := instanceName(pool.Name)
	if err != nil {
		return err
	}
	inst := &store.Instance{
		Name: name, PoolID: pool.ID, State: store.StatePending, JobID: jobID,
		// The rate is copied rather than looked up later: rates get edited, and
		// a bill that already happened does not change.
		HourlyUSD: hourlyRate(pool),
	}
	if err := c.db.WithContext(ctx).Create(inst).Error; err != nil {
		return fmt.Errorf("record instance: %w", err)
	}

	// The stage is the label; the error text is not. A stage is a bounded set
	// an alert can be written against, where provider error strings are
	// unbounded and would multiply the series count by every distinct message
	// a cloud ever returns. The message itself goes to the event log.
	fail := func(stage string, err error) error {
		metrics.MachineFailed(pool.Name, poolCloudName(pool), stage)
		inst.Error = err.Error()
		_ = c.db.SetState(ctx, inst, store.StateFailed)
		c.db.Logf(ctx, "error", "launch", &pool.ID, &inst.ID, "%v", err)
		return err
	}

	cred, err := fg.Provision(ctx, forge.RunnerRequest{Name: name, Labels: pool.Labels, JobID: jobID})
	if err != nil {
		return fail("credential", fmt.Errorf("mint runner credential: %w", err))
	}
	inst.ForgeRunnerID = cred.RunnerID
	if err := c.db.SetState(ctx, inst, store.StateProvisioning); err != nil {
		return fail("record", err)
	}

	caps := prov.Capabilities()
	boot, err := fg.Bootstrap(cred, caps.CredentialMode, forge.BootstrapOptions{
		RunnerName:     name,
		Labels:         pool.Labels,
		JobID:          jobID,
		JobTimeout:     pool.JobTimeout(),
		ContainerImage: pool.ContainerImage,
		Network:        cloud.SpecString(specOf(pool.Size), "network"),
	})
	if err != nil {
		return fail("bootstrap", fmt.Errorf("build bootstrap: %w", err))
	}

	req := cloud.ProvisionRequest{
		Name:      name,
		Owner:     c.Owner(pool.Name),
		Size:      sizeName(pool),
		SizeSpec:  specOf(pool.Size),
		Image:     imageName(pool),
		ImageSpec: imageSpecOf(pool.Image),
		Bootstrap: boot,
		Network: cloud.NetworkSpec{
			PublicIPv4:   pool.PublicIPv4,
			AllowSSHFrom: pool.AllowSSHFrom,
		},
	}

	created, err := prov.Provision(ctx, req)
	if err != nil {
		// The forge registration is already minted; drop it so it does not sit
		// there as an offline runner forever.
		if derr := fg.Deprovision(ctx, cred.RunnerID); derr != nil {
			c.log.Warn("could not roll back runner registration",
				"runner", cred.RunnerID, "err", derr)
		}
		return fail("provision", fmt.Errorf("provision machine: %w", err))
	}

	inst.ProviderID = created.ID
	if err := c.db.SetState(ctx, inst, store.StateBooting); err != nil {
		return fail("record", err)
	}
	metrics.MachineCreated(pool.Name, poolCloudName(pool))
	c.db.Logf(ctx, "info", "launch", &pool.ID, &inst.ID,
		"created %s on %s (%s)", name, pool.Cloud.Name, sizeName(pool))
	return nil
}

// poolCloudName is the cloud a pool runs on, for labelling. A pool with no
// cloud cannot launch anything, but the label still has to have a value.
func poolCloudName(p *store.Pool) string {
	if p.Cloud != nil {
		return p.Cloud.Name
	}
	return ""
}

// hourlyRate is what one machine in this pool costs per hour, or zero when the
// operator has not priced the size.
func hourlyRate(p *store.Pool) float64 {
	if p.Size == nil {
		return 0
	}
	return p.Size.HourlyUSD
}

func sizeName(p *store.Pool) string {
	if p.Size != nil {
		return p.Size.Name
	}
	return ""
}

func imageName(p *store.Pool) string {
	if p.Image != nil {
		return p.Image.Name
	}
	return ""
}

func specOf(s *store.Size) map[string]any {
	if s == nil {
		return nil
	}
	return map[string]any(s.Spec)
}

func imageSpecOf(i *store.Image) map[string]any {
	if i == nil {
		return nil
	}
	return map[string]any(i.Spec)
}

// instanceName builds a unique, DNS-safe machine name.
//
// The pool name is included so an operator reading a cloud console can tell what
// a machine is for, and the random suffix makes the name safe to use as the
// reaper's ownership fallback when tags are unavailable.
func instanceName(pool string) (string, error) {
	b := make([]byte, nameRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate machine name: %w", err)
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + lowerCaseOffset
		default:
			return '-'
		}
	}, pool)
	if len(safe) > maxPoolNameInMachineName {
		safe = safe[:maxPoolNameInMachineName]
	}
	return fmt.Sprintf("rf-%s-%s", strings.Trim(safe, "-"), hex.EncodeToString(b)), nil
}
