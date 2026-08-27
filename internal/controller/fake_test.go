package controller_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

// The fakes below let the reaper and reconcile logic be tested exhaustively,
// including states a real cloud is hard to hold still in: a machine that exists
// with no database record, one whose provider reports no creation time, one
// belonging to a different deployment.
//
// Instances are looked up from a registry by an id passed through config, so
// each test gets its own and they can run in parallel.

var (
	fakeMu     sync.Mutex
	fakeClouds = map[string]*fakeProvider{}
	fakeForges = map[string]*fakeForge{}
)

func init() {
	cloud.Register("fake", func(cfg map[string]any) (cloud.Provider, error) {
		id, _ := cfg["fake_id"].(string)
		fakeMu.Lock()
		defer fakeMu.Unlock()
		p, ok := fakeClouds[id]
		if !ok {
			return nil, fmt.Errorf("fake cloud %q not registered by the test", id)
		}
		return p, nil
	})
	forge.Register("fake", func(cfg map[string]any) (forge.Forge, error) {
		id, _ := cfg["fake_id"].(string)
		fakeMu.Lock()
		defer fakeMu.Unlock()
		f, ok := fakeForges[id]
		if !ok {
			return nil, fmt.Errorf("fake forge %q not registered by the test", id)
		}
		return f, nil
	})
}

func newFakeProvider(id string) *fakeProvider {
	p := &fakeProvider{id: id, instances: map[string]*cloud.Instance{}}
	fakeMu.Lock()
	fakeClouds[id] = p
	fakeMu.Unlock()
	return p
}

type fakeProvider struct {
	id string

	mu        sync.Mutex
	instances map[string]*cloud.Instance
	deleted   []string
	// failDelete makes Delete return an error, to check that a row is not
	// closed out while its machine is still alive.
	failDelete bool
	// failProvision makes Provision fail, to check the forge registration is
	// rolled back rather than left dangling.
	failProvision error
	listErr       error
	nextID        int
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{
		CredentialMode: cloud.CredentialEnv,
		Tags:           true,
		TypicalBoot:    time.Millisecond,
	}
}

func (p *fakeProvider) Provision(_ context.Context, req cloud.ProvisionRequest) (*cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failProvision != nil {
		return nil, p.failProvision
	}
	p.nextID++
	inst := &cloud.Instance{
		ID:        fmt.Sprintf("fake-%d", p.nextID),
		Name:      req.Name,
		State:     cloud.StateRunning,
		CreatedAt: time.Now().UTC(),
		Tags:      map[string]string{},
	}
	maps.Copy(inst.Tags, req.Tags)
	maps.Copy(inst.Tags, req.Owner.Tags())
	p.instances[inst.ID] = inst
	return inst, nil
}

func (p *fakeProvider) Get(_ context.Context, id string) (*cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[id]
	if !ok {
		return nil, cloud.ErrNotFound
	}
	cp := *inst
	return &cp, nil
}

func (p *fakeProvider) Delete(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failDelete {
		return errors.New("fake: delete refused")
	}
	delete(p.instances, id)
	p.deleted = append(p.deleted, id)
	return nil
}

func (p *fakeProvider) List(_ context.Context, owner cloud.Owner) ([]*cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	var out []*cloud.Instance
	for _, inst := range p.instances {
		if !owner.Matches(inst.Tags) {
			continue
		}
		cp := *inst
		out = append(out, &cp)
	}
	return out, nil
}

// add places a machine directly, bypassing Provision, so a test can construct
// situations the controller would not create on its own.
func (p *fakeProvider) add(inst *cloud.Instance) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances[inst.ID] = inst
}

func (p *fakeProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.instances)
}

func (p *fakeProvider) has(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.instances[id]
	return ok
}

// ---- fake forge ----

func newFakeForge(id string) *fakeForge {
	f := &fakeForge{id: id, runners: map[string]forge.Runner{}}
	fakeMu.Lock()
	fakeForges[id] = f
	fakeMu.Unlock()
	return f
}

type fakeForge struct {
	id string

	mu           sync.Mutex
	runners      map[string]forge.Runner
	jobs         []forge.Job
	nextID       int
	listErr      error
	provisionErr error
	deprovisions []string
}

func (f *fakeForge) Name() string     { return "fake" }
func (f *fakeForge) Kind() forge.Kind { return "fake" }

func (f *fakeForge) Demand(_ context.Context, _ []string) ([]forge.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Job(nil), f.jobs...), nil
}

func (f *fakeForge) Provision(_ context.Context, req forge.RunnerRequest) (*forge.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	f.nextID++
	id := fmt.Sprintf("r%d", f.nextID)
	f.runners[id] = forge.Runner{ID: id, Name: req.Name, Labels: req.Labels, Online: true}
	return &forge.Credential{Kind: "fake", RunnerID: id, Token: "t-" + id}, nil
}

func (f *fakeForge) Deprovision(_ context.Context, runnerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runners, runnerID)
	f.deprovisions = append(f.deprovisions, runnerID)
	return nil
}

func (f *fakeForge) ListRunners(_ context.Context) ([]forge.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]forge.Runner, 0, len(f.runners))
	for _, r := range f.runners {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeForge) Bootstrap(cred *forge.Credential, _ cloud.CredentialMode, opts forge.BootstrapOptions) (cloud.Bootstrap, error) {
	return cloud.Bootstrap{
		ContainerImage: "fake:latest",
		Env:            map[string]string{"TOKEN": cred.Token, "NAME": opts.RunnerName},
	}, nil
}

func (f *fakeForge) addRunner(r forge.Runner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runners[r.ID] = r
}

func (f *fakeForge) runnerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runners)
}

func (f *fakeForge) setJobs(jobs ...forge.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = jobs
}
