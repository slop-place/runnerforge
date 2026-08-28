package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// CheckCloud verifies a cloud's credentials with a live call and records the
// result on the record, so an operator learns their credentials are wrong when
// they save them rather than when a job is already waiting.
func (c *Controller) CheckCloud(ctx context.Context, cl *store.Cloud) {
	now := time.Now().UTC()
	cl.StatusCheckAt = &now
	err := c.res.checkCloud(ctx, cl, c.Owner(""))
	metrics.CredentialCheck("cloud", cl.Name, err)
	if err != nil {
		cl.Status, cl.StatusDetail = "error", err.Error()
	} else {
		cl.Status, cl.StatusDetail = "ok", ""
	}
	if err := c.db.WithContext(ctx).Save(cl).Error; err != nil {
		c.log.Warn("could not record cloud status", "cloud", cl.Name, "err", err)
	}
}

// CheckForge verifies a forge connection with a live call.
func (c *Controller) CheckForge(ctx context.Context, f *store.Forge) {
	now := time.Now().UTC()
	f.StatusCheckAt = &now
	err := c.res.checkForge(ctx, f)
	metrics.CredentialCheck("forge", f.Name, err)
	if err != nil {
		f.Status, f.StatusDetail = "error", err.Error()
	} else {
		f.Status, f.StatusDetail = "ok", ""
	}
	if err := c.db.WithContext(ctx).Save(f).Error; err != nil {
		c.log.Warn("could not record forge status", "forge", f.Name, "err", err)
	}
}

// DestroyInstance tears down one machine on request, from the UI.
//
// It degrades rather than refusing. An operator reaching for this is usually
// dealing with something already broken, so a misconfigured forge must not
// stop a machine being destroyed, and a row with nothing behind it must be
// closeable without any client at all.
func (c *Controller) DestroyInstance(ctx context.Context, id uint) error {
	var inst store.Instance
	if err := c.db.WithContext(ctx).First(&inst, id).Error; err != nil {
		return fmt.Errorf("load instance %d: %w", id, err)
	}
	if inst.State == store.StateDeleted {
		return nil
	}

	// Nothing was ever created, so there is nothing to call anyone about.
	if inst.ProviderID == "" && inst.ForgeRunnerID == "" {
		return c.db.SetState(ctx, &inst, store.StateDeleted)
	}

	pool, err := c.db.PoolByID(ctx, inst.PoolID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if pool == nil || pool.Cloud == nil {
		// The pool is gone, so there is no provider to resolve. Close the row
		// out and leave the machine to the reaper, which finds it by its tags.
		c.db.Logf(ctx, "warn", "destroy", nil, &inst.ID,
			"closing out %s from the UI; its pool is gone, the reaper will collect the machine",
			inst.Name)
		return c.db.SetState(ctx, &inst, store.StateDeleted)
	}

	prov, err := c.res.Cloud(pool.Cloud)
	if err != nil {
		// Without a provider the machine cannot be destroyed, and pretending
		// otherwise would leak it.
		return fmt.Errorf("destroy %s: %w", inst.Name, err)
	}

	// The forge is best-effort: a stale registration is harmless where a stray
	// machine is not, so a broken forge connection must not block the destroy.
	fg, ferr := c.res.Forge(pool.Forge)
	if ferr != nil {
		c.log.Warn("destroying without deregistering: the forge could not be reached",
			"instance", inst.Name, "err", ferr)
		fg = nil
	}

	c.db.Logf(ctx, "warn", "destroy", &pool.ID, &inst.ID, "%s destroyed from the UI", inst.Name)
	if err := c.destroy(ctx, pool, prov, fg, &inst, "operator"); err != nil {
		return fmt.Errorf("destroy %s: %w", inst.Name, err)
	}
	return nil
}

// Provider returns a live provider for a cloud record.
//
// Exposed so the UI can ask a cloud what it can build with, rather than making
// an operator look a flavor id up somewhere else and type it in.
func (c *Controller) Provider(cl *store.Cloud) (cloud.Provider, error) {
	return c.res.Cloud(cl)
}
