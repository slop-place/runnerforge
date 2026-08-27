package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/store"
)

const (
	// eventLogLines is how many lines of a failed machine's output are quoted
	// into an event message.
	eventLogLines = 6
	// capturedLogLines is how many lines are fetched from a provider.
	capturedLogLines = 200
)

// runnerSnapshot is one reading of a forge's runner registrations.
//
// The ok field is load-bearing. A runner vanishing from the forge is how
// runnerforge learns its job finished, but a failed API call also produces an
// empty list. Without distinguishing the two, one network blip would look like
// "every job finished at once" and destroy machines mid-build. When ok is
// false, every disappearance-based conclusion is suppressed.
type runnerSnapshot struct {
	byID map[string]forge.Runner
	ok   bool
}

// has reports whether a registration is still present. The second result is
// false when the snapshot is untrustworthy and the caller must not act on it.
func (s runnerSnapshot) has(id string) (bool, bool) {
	if !s.ok {
		return false, false
	}
	_, ok := s.byID[id]
	return ok, true
}

func (c *Controller) snapshotRunners(ctx context.Context, fg forge.Forge) runnerSnapshot {
	rs, err := fg.ListRunners(ctx)
	if err != nil {
		c.log.Warn("could not list forge runners; "+
			"job-completion detection is paused this pass", "err", err)
		return runnerSnapshot{ok: false}
	}
	m := make(map[string]forge.Runner, len(rs))
	for _, r := range rs {
		m[r.ID] = r
	}
	return runnerSnapshot{byID: m, ok: true}
}

// advance moves one machine forward by one step, destroying it when its work is
// done or when it has outlived its welcome.
func (c *Controller) advance(
	ctx context.Context,
	pool *store.Pool,
	prov cloud.Provider,
	fg forge.Forge,
	snap runnerSnapshot,
	inst *store.Instance,
) error {
	// The hard ceiling comes first and is checked against nothing but the
	// clock, so that a machine stuck in any state still goes away.
	if age := time.Since(inst.CreatedAt); age > pool.MaxLifetime() {
		c.db.Logf(ctx, "warn", "reap", &pool.ID, &inst.ID,
			"destroying %s: exceeded max lifetime (%s old)", inst.Name, age.Round(time.Second))
		return c.destroy(ctx, prov, fg, inst)
	}

	// A machine that failed before the cloud create call has nothing to poll.
	if inst.ProviderID == "" {
		if inst.State == store.StateFailed {
			return c.destroy(ctx, prov, fg, inst)
		}
		return nil
	}

	got, err := prov.Get(ctx, inst.ProviderID)
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		// The machine is gone from the cloud. Whatever removed it, we still owe
		// the forge a cleanup for the registration it was carrying.
		c.db.Logf(ctx, "info", "lifecycle", &pool.ID, &inst.ID,
			"%s no longer exists at the provider", inst.Name)
		return c.destroy(ctx, prov, fg, inst)
	case err != nil:
		return fmt.Errorf("get instance %s: %w", inst.Name, err)
	}

	if got.PublicIP != "" && got.PublicIP != inst.PublicIP {
		inst.PublicIP = got.PublicIP
	}
	if got.PrivateIP != "" && got.PrivateIP != inst.PrivateIP {
		inst.PrivateIP = got.PrivateIP
	}

	switch got.State {
	case cloud.StateError:
		inst.Error = got.Err
		c.captureLogs(ctx, prov, inst)
		c.db.Logf(ctx, "error", "lifecycle", &pool.ID, &inst.ID,
			"%s failed at the provider: %s", inst.Name, got.Err)
		return c.destroy(ctx, prov, fg, inst)

	case cloud.StateStopped:
		// A stopped machine has finished its job and powered itself off, which
		// is the runner's normal exit path. It is also what a machine that
		// failed on startup looks like, so the output is captured either way
		// before the machine is destroyed with it.
		c.captureLogs(ctx, prov, inst)
		// A short job can start and finish between two reconcile passes, so
		// ClaimedAt alone would report plenty of healthy runs as failures. The
		// forge is the better witness: an ephemeral registration only
		// disappears once it has been handed work.
		claimed := inst.ClaimedAt != nil
		if present, trustworthy := snap.has(inst.ForgeRunnerID); trustworthy && !present {
			claimed = true
		}
		if !claimed {
			c.db.Logf(ctx, "warn", "lifecycle", &pool.ID, &inst.ID,
				"%s stopped without ever claiming a job; output: %s",
				inst.Name, firstLines(inst.Logs, eventLogLines))
		} else {
			c.db.Logf(ctx, "info", "lifecycle", &pool.ID, &inst.ID,
				"%s stopped; tearing down", inst.Name)
		}
		return c.destroy(ctx, prov, fg, inst)

	case cloud.StateRunning, cloud.StateCreating:
		return c.advanceRunning(ctx, pool, prov, fg, snap, inst)
	case cloud.StateGone:
		// Reported gone rather than absent; same obligation either way.
		return c.destroy(ctx, prov, fg, inst)
	}
	return nil
}

// advanceRunning interprets a live machine against the forge's view of it.
func (c *Controller) advanceRunning(
	ctx context.Context,
	pool *store.Pool,
	prov cloud.Provider,
	fg forge.Forge,
	snap runnerSnapshot,
	inst *store.Instance,
) error {
	// Without a forge-side id there is nothing to correlate; wait for the
	// lifetime ceiling rather than guessing.
	if inst.ForgeRunnerID == "" {
		return c.db.WithContext(ctx).Save(inst).Error
	}

	present, trustworthy := snap.has(inst.ForgeRunnerID)
	if !trustworthy {
		return c.db.WithContext(ctx).Save(inst).Error
	}

	if !present {
		// The registration is gone. For an ephemeral runner that means the forge
		// handed it a job and retired it, so the machine's work is done.
		//
		// One exception: a machine that has not yet been seen registered may
		// simply not have registered yet. Only conclude "finished" once we know
		// it was there.
		if inst.ReadyAt == nil && inst.ClaimedAt == nil {
			return c.db.WithContext(ctx).Save(inst).Error
		}
		c.db.Logf(ctx, "info", "lifecycle", &pool.ID, &inst.ID,
			"%s finished its job; tearing down", inst.Name)
		return c.destroy(ctx, prov, fg, inst)
	}

	r := snap.byID[inst.ForgeRunnerID]
	switch {
	case r.Busy && inst.State != store.StateBusy:
		return c.db.SetState(ctx, inst, store.StateBusy)
	case !r.Busy && inst.State == store.StateBooting && r.Online:
		return c.db.SetState(ctx, inst, store.StateIdle)
	}
	return c.db.WithContext(ctx).Save(inst).Error
}

// maxStoredLogs bounds what is kept per instance so a chatty runner cannot
// grow the database without limit.
const maxStoredLogs = 16 << 10

// captureLogs saves a machine's output before it is destroyed. Failures are
// non-fatal: losing the logs is bad, but not destroying the machine is worse.
func (c *Controller) captureLogs(ctx context.Context, prov cloud.Provider, inst *store.Instance) {
	lp, ok := prov.(cloud.LogProvider)
	if !ok || inst.ProviderID == "" {
		return
	}
	out, err := lp.Logs(ctx, inst.ProviderID, capturedLogLines)
	if err != nil {
		c.log.Debug("could not capture logs", "instance", inst.Name, "err", err)
		return
	}
	if len(out) > maxStoredLogs {
		out = out[len(out)-maxStoredLogs:]
	}
	inst.Logs = out
	if err := c.db.WithContext(ctx).Save(inst).Error; err != nil {
		c.log.Debug("could not save logs", "instance", inst.Name, "err", err)
	}
}

// firstLines returns at most n non-empty lines, for one-line event messages.
func firstLines(s string, n int) string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return strings.Join(out, " | ")
}

// destroy removes a machine and its forge registration, then marks the row
// deleted.
//
// Order matters: the machine goes first, because it is the thing that costs
// money and the thing that can still be executing untrusted code. The forge
// registration is tidied afterwards and its failure does not block the row from
// being closed out, since a stale registration is harmless where a stray machine
// is not.
func (c *Controller) destroy(ctx context.Context, prov cloud.Provider, fg forge.Forge, inst *store.Instance) error {
	if inst.State != store.StateDraining && inst.State != store.StateDeleted {
		if err := c.db.SetState(ctx, inst, store.StateDraining); err != nil {
			return err
		}
	}

	if inst.ProviderID != "" {
		if err := prov.Delete(ctx, inst.ProviderID); err != nil {
			// Leave the row alive so the next pass tries again. This is the one
			// failure that must never be swallowed.
			return fmt.Errorf("delete machine %s: %w", inst.Name, err)
		}
	}

	if inst.ForgeRunnerID != "" {
		if err := fg.Deprovision(ctx, inst.ForgeRunnerID); err != nil {
			c.log.Warn("could not remove forge runner registration",
				"instance", inst.Name, "runner", inst.ForgeRunnerID, "err", err)
		}
	}

	return c.db.SetState(ctx, inst, store.StateDeleted)
}
