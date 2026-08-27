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
	"github.com/slop-place/runnerforge/internal/store"
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
			return ctx.Err()
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

	pools, err := c.db.EnabledPools(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range pools {
		p := &pools[i]
		if err := c.reconcilePool(ctx, p); err != nil {
			// One broken pool must not stop the others; a pool whose forge is
			// unreachable should not prevent another pool's machines from
			// being torn down.
			errs = append(errs, fmt.Errorf("pool %s: %w", p.Name, err))
			c.db.Log(ctx, "error", "reconcile", &p.ID, nil, "%v", err)
		}
	}
	return errors.Join(errs...)
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
	matching := matchingJobs(jobs, pool.Labels)

	// Machines are bound to a specific job, so demand is measured as the jobs
	// nothing is working on yet — not the raw queue length. Counting the queue
	// instead would launch a second machine for a job that already has one.
	covered := make(map[string]bool, len(live))
	var idle, total int
	for _, in := range live {
		if in.JobID != "" {
			covered[in.JobID] = true
		}
		switch in.State {
		case store.StatePending, store.StateProvisioning, store.StateBooting, store.StateIdle:
			if in.JobID == "" {
				idle++ // an unbound warm machine, able to take anything
			}
			total++
		case store.StateBusy, store.StateDraining, store.StateFailed:
			total++
		}
	}

	uncovered := make([]forge.Job, 0, len(matching))
	for _, j := range matching {
		if !covered[j.ID] {
			uncovered = append(uncovered, j)
		}
	}

	// Warm machines soak up demand before anything new is created.
	toLaunch := uncovered
	if idle > 0 {
		if idle >= len(toLaunch) {
			toLaunch = nil
		} else {
			toLaunch = toLaunch[idle:]
		}
	}

	// Never exceed the pool ceiling, which is also the account-quota guard.
	room := pool.MaxInstances - total
	if room < 0 {
		room = 0
	}
	if len(toLaunch) > room {
		c.db.Log(ctx, "warn", "scale", &pool.ID, nil,
			"%d job(s) waiting but pool is at its ceiling of %d machines",
			len(toLaunch), pool.MaxInstances)
		toLaunch = toLaunch[:room]
	}

	// Top up to min_idle with unbound machines once demand is covered.
	var warm int
	if spare := room - len(toLaunch); spare > 0 {
		if want := pool.MinIdle - idle; want > 0 {
			warm = min(want, spare)
		}
	}

	if len(toLaunch) == 0 && warm == 0 {
		return nil
	}
	c.log.Info("scaling up", "pool", pool.Name,
		"queued", len(matching), "uncovered", len(uncovered),
		"idle", idle, "launching", len(toLaunch), "warm", warm)

	for _, j := range toLaunch {
		if err := c.launch(ctx, pool, prov, fg, j.ID); err != nil {
			c.db.Log(ctx, "error", "launch", &pool.ID, nil, "launch failed: %v", err)
			return err
		}
	}
	for range warm {
		if err := c.launch(ctx, pool, prov, fg, ""); err != nil {
			c.db.Log(ctx, "error", "launch", &pool.ID, nil, "warm launch failed: %v", err)
			return err
		}
	}
	return nil
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
	inst := &store.Instance{Name: name, PoolID: pool.ID, State: store.StatePending, JobID: jobID}
	if err := c.db.WithContext(ctx).Create(inst).Error; err != nil {
		return fmt.Errorf("record instance: %w", err)
	}

	fail := func(err error) error {
		inst.Error = err.Error()
		_ = c.db.SetState(ctx, inst, store.StateFailed)
		c.db.Log(ctx, "error", "launch", &pool.ID, &inst.ID, "%v", err)
		return err
	}

	cred, err := fg.Provision(ctx, forge.RunnerRequest{Name: name, Labels: pool.Labels, JobID: jobID})
	if err != nil {
		return fail(fmt.Errorf("mint runner credential: %w", err))
	}
	inst.ForgeRunnerID = cred.RunnerID
	if err := c.db.SetState(ctx, inst, store.StateProvisioning); err != nil {
		return fail(err)
	}

	caps := prov.Capabilities()
	boot, err := fg.Bootstrap(cred, caps.CredentialMode, forge.BootstrapOptions{
		RunnerName:     name,
		Labels:         pool.Labels,
		JobID:          jobID,
		JobTimeout:     pool.JobTimeout(),
		ContainerImage: pool.ContainerImage,
	})
	if err != nil {
		return fail(fmt.Errorf("build bootstrap: %w", err))
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
		return fail(fmt.Errorf("provision machine: %w", err))
	}

	inst.ProviderID = created.ID
	if err := c.db.SetState(ctx, inst, store.StateBooting); err != nil {
		return fail(err)
	}
	c.db.Log(ctx, "info", "launch", &pool.ID, &inst.ID,
		"created %s on %s (%s)", name, pool.Cloud.Name, sizeName(pool))
	return nil
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
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, pool)
	if len(safe) > 24 {
		safe = safe[:24]
	}
	return fmt.Sprintf("rf-%s-%s", strings.Trim(safe, "-"), hex.EncodeToString(b)), nil
}
