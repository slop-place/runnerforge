package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// namePrefix marks every machine and runner registration runnerforge creates.
// It is the fallback identity when a provider cannot store tags, and the guard
// that stops the reaper touching anything it did not create.
const namePrefix = "rf-"

// Reap is the safety net.
//
// Reconcile handles the happy path; this handles everything else. It asks each
// cloud what it is actually running — rather than trusting the database — and
// destroys anything this deployment owns that it can no longer justify. That
// inversion is deliberate: the database can be lost, restored from a backup, or
// simply be wrong, and the machines still have to go away.
//
// It is the first thing to run at startup and it runs on a timer thereafter.
func (c *Controller) Reap(ctx context.Context) error {
	start := time.Now()
	var errs []error
	if err := c.reapClouds(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.reapForges(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.reapRows(ctx); err != nil {
		errs = append(errs, err)
	}
	joined := errors.Join(errs...)
	metrics.ReapPass(time.Since(start), joined)
	return joined
}

// reapClouds destroys machines that exist in a cloud but should not.
func (c *Controller) reapClouds(ctx context.Context) error {
	clouds, err := c.db.Clouds(ctx)
	if err != nil {
		return err
	}
	pools, err := c.db.Pools(ctx)
	if err != nil {
		return err
	}
	// Lifetime ceilings are per pool, so index them by the tag written on the
	// machine rather than by anything the database has to still agree about.
	lifetimeByPool := map[string]time.Duration{}
	for i := range pools {
		lifetimeByPool[pools[i].Name] = pools[i].MaxLifetime()
	}

	var errs []error
	for i := range clouds {
		cl := &clouds[i]
		if !cl.Enabled {
			// A disabled cloud is still swept. Disabling a cloud in the UI must
			// not strand machines that are already running on it.
			c.log.Debug("sweeping disabled cloud", "cloud", cl.Name)
		}
		prov, err := c.res.Cloud(cl)
		if err != nil {
			errs = append(errs, fmt.Errorf("cloud %s: %w", cl.Name, err))
			continue
		}
		// Empty pool means "everything this deployment owns here", including
		// machines whose pool has since been deleted from the config.
		owner := cloud.Owner{Controller: c.cfg.ID}
		machines, err := prov.List(ctx, owner)
		if err != nil {
			errs = append(errs, fmt.Errorf("cloud %s: list: %w", cl.Name, err))
			continue
		}
		for _, m := range machines {
			if err := c.reapMachine(ctx, cl, prov, m, lifetimeByPool); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// reapMachine decides the fate of one machine found in a cloud.
func (c *Controller) reapMachine(
	ctx context.Context,
	cl *store.Cloud,
	prov cloud.Provider,
	m *cloud.Instance,
	lifetimeByPool map[string]time.Duration,
) error {
	// Refuse to touch anything not obviously ours, even if a tag says otherwise.
	// The tag and the name have to agree before this destroys something.
	if !strings.HasPrefix(m.Name, namePrefix) {
		c.log.Warn("ignoring machine with our owner tag but a foreign name",
			"cloud", cl.Name, "name", m.Name)
		return nil
	}

	inst, err := c.db.InstanceByName(ctx, m.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	reason := ""
	switch {
	case inst == nil:
		// No record at all: created by a process that died before it could
		// write one, or left behind by a restored-from-backup database.
		reason = "no database record"
	case inst.State == store.StateDeleted:
		// We believed this was destroyed and it is still here. Almost always a
		// delete call that failed after the row was closed out.
		reason = "already marked deleted"
	default:
		pool := m.Tags[cloud.TagPool]
		lifetime, known := lifetimeByPool[pool]
		if !known {
			// The pool is gone from the config but the machine is still running.
			reason = fmt.Sprintf("pool %q no longer exists", pool)
			break
		}
		// A provider that does not report a creation time must not be aged off:
		// a zero timestamp is infinitely old, and acting on it would destroy
		// every machine in the account, mid-job. The database's own record is
		// the fallback, and reconcile enforces the ceiling from there.
		if m.CreatedAt.IsZero() {
			c.log.Debug("provider reported no creation time; leaving the age check to reconcile",
				"cloud", cl.Name, "name", m.Name)
			break
		}
		if age := time.Since(m.CreatedAt); age > lifetime {
			reason = fmt.Sprintf("exceeded max lifetime (%s old)", age.Round(time.Second))
		}
	}
	if reason == "" {
		return nil
	}

	c.log.Warn("reaping machine", "cloud", cl.Name, "name", m.Name, "reason", reason)
	// The counter's label is the category, not the sentence: an age in the
	// message would be a new label value for every machine.
	metrics.ReapedMachine(cl.Name, reapCategory(reason))
	if err := prov.Delete(ctx, m.ID); err != nil {
		return fmt.Errorf("reap %s: %w", m.Name, err)
	}
	var instID *uint
	if inst != nil {
		instID = &inst.ID
		if inst.State != store.StateDeleted {
			_ = c.db.SetState(ctx, inst, store.StateDeleted)
		}
	}
	c.db.Logf(ctx, "warn", "reap", nil, instID, "reaped %s from %s: %s", m.Name, cl.Name, reason)
	return nil
}

// reapCategory maps a reap reason to a bounded label. The reasons carry detail
// an operator wants to read — an age, a pool name — which is exactly what a
// label must not contain.
func reapCategory(reason string) string {
	switch {
	case strings.HasPrefix(reason, "no database record"):
		return "no_record"
	case strings.HasPrefix(reason, "already marked deleted"):
		return "delete_failed"
	case strings.HasPrefix(reason, "pool "):
		return "pool_gone"
	case strings.HasPrefix(reason, "exceeded max lifetime"):
		return "max_lifetime"
	default:
		return "other"
	}
}

// reapForges removes runner registrations whose machine is gone.
//
// These cost nothing but they accumulate as offline entries in the forge's UI,
// and each one is a credential that was minted and never spent.
func (c *Controller) reapForges(ctx context.Context) error {
	forges, err := c.db.Forges(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range forges {
		f := &forges[i]
		fg, err := c.res.Forge(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("forge %s: %w", f.Name, err))
			continue
		}
		runners, err := fg.ListRunners(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("forge %s: list runners: %w", f.Name, err))
			continue
		}
		for _, r := range runners {
			if !strings.HasPrefix(r.Name, namePrefix) {
				continue // not ours
			}
			if r.Busy {
				continue // running a job right now
			}
			inst, err := c.db.InstanceByName(ctx, r.Name)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
				continue
			}
			// Keep registrations whose machine is still live; those are either
			// waiting for work or working.
			if inst != nil && inst.State != store.StateDeleted && inst.State != store.StateFailed {
				continue
			}
			c.log.Warn("removing orphaned runner registration",
				"forge", f.Name, "runner", r.Name)
			if err := fg.Deprovision(ctx, r.ID); err != nil {
				errs = append(errs, fmt.Errorf("forge %s: deprovision %s: %w", f.Name, r.Name, err))
				continue
			}
			metrics.ReapedRunner(f.Name)
			c.db.Logf(ctx, "warn", "reap", nil, nil,
				"removed orphaned runner registration %s from %s", r.Name, f.Name)
		}
	}
	return errors.Join(errs...)
}

// reapRows closes out database rows whose machine no longer exists.
//
// Without this, a machine deleted outside runnerforge would leave a row counting
// against the pool's ceiling forever, quietly throttling the pool to nothing.
func (c *Controller) reapRows(ctx context.Context) error {
	live, err := c.db.AllLiveInstances(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range live {
		inst := &live[i]
		if inst.Pool == nil {
			// Pool deleted underneath it: nothing left to reconcile against.
			if inst.State != store.StateDeleted {
				c.db.Logf(ctx, "warn", "reap", nil, &inst.ID,
					"closing out %s: its pool no longer exists", inst.Name)
				metrics.ReapedRow("pool_gone")
				_ = c.db.SetState(ctx, inst, store.StateDeleted)
			}
			continue
		}
		if inst.ProviderID == "" {
			// Never got as far as creating anything. Give it a grace period in
			// case a launch is in flight right now, then close it out.
			if time.Since(inst.CreatedAt) > 10*time.Minute {
				metrics.ReapedRow("never_created")
				_ = c.db.SetState(ctx, inst, store.StateDeleted)
			}
			continue
		}
		pool, err := c.db.PoolByID(ctx, inst.PoolID)
		if err != nil || pool.Cloud == nil {
			continue
		}
		prov, err := c.res.Cloud(pool.Cloud)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, err = prov.Get(ctx, inst.ProviderID)
		if errors.Is(err, cloud.ErrNotFound) {
			c.db.Logf(ctx, "info", "reap", &pool.ID, &inst.ID,
				"closing out %s: machine no longer exists", inst.Name)
			metrics.ReapedRow("machine_gone")
			_ = c.db.SetState(ctx, inst, store.StateDeleted)
		}
	}
	return errors.Join(errs...)
}

// DestroyAll tears down every machine this deployment owns, across every cloud.
//
// This is what an operator reaches for when something has gone wrong, and what
// the end-to-end tests call to assert that a run leaves nothing behind.
func (c *Controller) DestroyAll(ctx context.Context) (int, error) {
	clouds, err := c.db.Clouds(ctx)
	if err != nil {
		return 0, err
	}
	destroyed := 0
	var errs []error
	for i := range clouds {
		prov, err := c.res.Cloud(&clouds[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		machines, err := prov.List(ctx, cloud.Owner{Controller: c.cfg.ID})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, m := range machines {
			if !strings.HasPrefix(m.Name, namePrefix) {
				continue
			}
			if err := prov.Delete(ctx, m.ID); err != nil {
				errs = append(errs, err)
				continue
			}
			destroyed++
		}
	}
	return destroyed, errors.Join(errs...)
}

// CountMachines reports how many machines this deployment currently owns in a
// cloud. End-to-end tests assert this is zero at teardown.
func (c *Controller) CountMachines(ctx context.Context, cl *store.Cloud) (int, error) {
	prov, err := c.res.Cloud(cl)
	if err != nil {
		return 0, err
	}
	machines, err := prov.List(ctx, cloud.Owner{Controller: c.cfg.ID})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range machines {
		if strings.HasPrefix(m.Name, namePrefix) {
			n++
		}
	}
	return n, nil
}
