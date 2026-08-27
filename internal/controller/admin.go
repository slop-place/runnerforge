package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/slop-place/runnerforge/internal/store"
)

// CheckCloud verifies a cloud's credentials with a live call and records the
// result on the record, so an operator learns their credentials are wrong when
// they save them rather than when a job is already waiting.
func (c *Controller) CheckCloud(ctx context.Context, cl *store.Cloud) {
	now := time.Now().UTC()
	cl.StatusCheckAt = &now
	if err := c.res.checkCloud(ctx, cl, c.Owner("")); err != nil {
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
	if err := c.res.checkForge(ctx, f); err != nil {
		f.Status, f.StatusDetail = "error", err.Error()
	} else {
		f.Status, f.StatusDetail = "ok", ""
	}
	if err := c.db.WithContext(ctx).Save(f).Error; err != nil {
		c.log.Warn("could not record forge status", "forge", f.Name, "err", err)
	}
}

// DestroyInstance tears down one machine on request, from the UI.
func (c *Controller) DestroyInstance(ctx context.Context, id uint) error {
	var inst store.Instance
	if err := c.db.WithContext(ctx).First(&inst, id).Error; err != nil {
		return err
	}
	if inst.State == store.StateDeleted {
		return nil
	}
	pool, err := c.db.PoolByID(ctx, inst.PoolID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if pool == nil || pool.Cloud == nil || pool.Forge == nil {
		// The pool is gone, so there is nothing to resolve a provider from.
		// Close the row out and let the reaper find the machine by its tags.
		return c.db.SetState(ctx, &inst, store.StateDeleted)
	}
	prov, err := c.res.Cloud(pool.Cloud)
	if err != nil {
		return err
	}
	fg, err := c.res.Forge(pool.Forge)
	if err != nil {
		return err
	}
	c.db.Logf(ctx, "warn", "destroy", &pool.ID, &inst.ID, "%s destroyed from the UI", inst.Name)
	if err := c.destroy(ctx, prov, fg, &inst); err != nil {
		return fmt.Errorf("destroy %s: %w", inst.Name, err)
	}
	return nil
}
