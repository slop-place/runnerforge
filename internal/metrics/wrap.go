package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

// Wrapping the provider and forge interfaces here means every driver is
// measured without any driver knowing about it, and a driver added later is
// measured the day it is written. The alternative — a timer in each
// implementation — is the same code five times over and is wrong the moment
// somebody forgets it.

// WrapCloud returns p instrumented under the operator's name for it.
//
// Capturing a machine's logs is an optional capability discovered by type
// assertion, so a wrapper that always offered it would make every provider
// claim it. The wrapper only grows that method when the provider underneath
// actually has it.
func WrapCloud(name string, p cloud.Provider) cloud.Provider {
	c := &instrumentedCloud{Provider: p, name: name, driver: p.Name()}
	if lp, ok := p.(cloud.LogProvider); ok {
		return &instrumentedLogCloud{instrumentedCloud: c, logs: lp}
	}
	return c
}

type instrumentedCloud struct {
	cloud.Provider

	name   string
	driver string
}

func (c *instrumentedCloud) Provision(
	ctx context.Context, req cloud.ProvisionRequest,
) (*cloud.Instance, error) {
	start := time.Now()
	in, err := c.Provider.Provision(ctx, req)
	c.observe("provision", start, err)
	return in, err
}

func (c *instrumentedCloud) Get(ctx context.Context, id string) (*cloud.Instance, error) {
	start := time.Now()
	in, err := c.Provider.Get(ctx, id)
	c.observe("get", start, err)
	return in, err
}

func (c *instrumentedCloud) Delete(ctx context.Context, id string) error {
	start := time.Now()
	err := c.Provider.Delete(ctx, id)
	c.observe("delete", start, err)
	return err
}

func (c *instrumentedCloud) List(ctx context.Context, owner cloud.Owner) ([]*cloud.Instance, error) {
	start := time.Now()
	out, err := c.Provider.List(ctx, owner)
	c.observe("list", start, err)
	return out, err
}

// observe times one call and records it.
func (c *instrumentedCloud) observe(op string, start time.Time, err error) {
	CloudCall(c.name, c.driver, op, time.Since(start), notFoundIsFine(err))
}

// notFoundIsFine keeps an expected absence out of the error rate. Asking about
// a machine that is already gone is how the controller learns it is gone, and
// counting that as a provider failure would make a healthy teardown look like
// an outage.
func notFoundIsFine(err error) error {
	if errors.Is(err, cloud.ErrNotFound) {
		return nil
	}
	return err
}

// instrumentedLogCloud is the wrapper for a provider that can also fetch logs.
type instrumentedLogCloud struct {
	*instrumentedCloud

	logs cloud.LogProvider
}

func (c *instrumentedLogCloud) Logs(ctx context.Context, id string, tail int) (string, error) {
	start := time.Now()
	out, err := c.logs.Logs(ctx, id, tail)
	c.observe("logs", start, err)
	return out, err
}

// WrapForge returns f instrumented.
func WrapForge(f forge.Forge) forge.Forge {
	return &instrumentedForge{Forge: f, name: f.Name(), kind: string(f.Kind())}
}

type instrumentedForge struct {
	forge.Forge

	name string
	kind string
}

func (f *instrumentedForge) Demand(ctx context.Context, labels []string) ([]forge.Job, error) {
	start := time.Now()
	jobs, err := f.Forge.Demand(ctx, labels)
	f.observe("demand", start, err)
	return jobs, err
}

func (f *instrumentedForge) Provision(
	ctx context.Context, req forge.RunnerRequest,
) (*forge.Credential, error) {
	start := time.Now()
	cred, err := f.Forge.Provision(ctx, req)
	f.observe("provision", start, err)
	return cred, err
}

func (f *instrumentedForge) Deprovision(ctx context.Context, runnerID string) error {
	start := time.Now()
	err := f.Forge.Deprovision(ctx, runnerID)
	f.observe("deprovision", start, err)
	return err
}

func (f *instrumentedForge) ListRunners(ctx context.Context) ([]forge.Runner, error) {
	start := time.Now()
	rs, err := f.Forge.ListRunners(ctx)
	f.observe("list_runners", start, err)
	return rs, err
}

func (f *instrumentedForge) observe(op string, start time.Time, err error) {
	ForgeCall(f.name, f.kind, op, time.Since(start), err)
}
