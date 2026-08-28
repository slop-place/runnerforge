package web_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

// The browser tests need a cloud and a forge that behave like the real ones
// without needing either. They are registered under names no real driver uses,
// and each test looks its own up by an id passed through the connection form —
// which is also how the browser gets to choose one, since the test drives the
// same form a person would.

var (
	uiMu     sync.Mutex
	uiClouds = map[string]*uiCloud{}
	uiForges = map[string]*uiForge{}
)

func init() {
	cloud.Register(cloud.Driver{
		Name:  "uifake",
		Title: "Browser test cloud",
		New: func(cfg map[string]any) (cloud.Provider, error) {
			id, _ := cfg["test_id"].(string)
			uiMu.Lock()
			defer uiMu.Unlock()
			c, ok := uiClouds[id]
			if !ok {
				return nil, fmt.Errorf("no browser-test cloud %q", id)
			}
			return c, nil
		},
		Schema: cloud.Schema{
			Connection: []cloud.Field{{
				Key: "test_id", Label: "Test ID", Type: cloud.FieldText, Required: true,
				Help: "Which in-process fake to talk to.",
			}, {
				Key: "region", Label: "Region", Type: cloud.FieldText,
				Placeholder: "us-east-1",
			}, {
				Key: "api_key", Label: "API key", Type: cloud.FieldText, Secret: true,
			}},
			Size:  []cloud.Field{{Key: "flavor", Label: "Flavor", Type: cloud.FieldText, Required: true}},
			Image: []cloud.Field{{Key: "image_id", Label: "Image ID", Type: cloud.FieldText, Required: true}},
		},
	})

	forge.Register(forge.Implementation{
		Kind:  "uifake",
		Title: "Browser test forge",
		New: func(cfg map[string]any) (forge.Forge, error) {
			id, _ := cfg["test_id"].(string)
			uiMu.Lock()
			defer uiMu.Unlock()
			f, ok := uiForges[id]
			if !ok {
				return nil, fmt.Errorf("no browser-test forge %q", id)
			}
			return f, nil
		},
		Fields: []cloud.Field{{
			Key: "test_id", Label: "Test ID", Type: cloud.FieldText, Required: true,
		}, {
			Key: "token", Label: "Token", Type: cloud.FieldText, Secret: true,
		}},
	})
}

// uiCloud is a cloud that keeps its machines in a map. Provisioning succeeds
// immediately and machines come up running, because what these tests are about
// is what the page does once a machine exists.
type uiCloud struct {
	mu   sync.Mutex
	n    int
	inst map[string]*cloud.Instance
}

func newUICloud(id string) *uiCloud {
	c := &uiCloud{inst: map[string]*cloud.Instance{}}
	uiMu.Lock()
	defer uiMu.Unlock()
	uiClouds[id] = c
	return c
}

func (c *uiCloud) Name() string { return "uifake" }

func (c *uiCloud) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{
		CredentialMode: cloud.CredentialEnv,
		Tags:           true,
		PublicIPv4:     true,
		TypicalBoot:    time.Second,
	}
}

func (c *uiCloud) Provision(_ context.Context, req cloud.ProvisionRequest) (*cloud.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	tags := map[string]string{}
	maps.Copy(tags, req.Tags)
	maps.Copy(tags, req.Owner.Tags())
	in := &cloud.Instance{
		ID: fmt.Sprintf("ui-%d", c.n), Name: req.Name, State: cloud.StateRunning,
		PublicIP: "203.0.113.10", CreatedAt: time.Now().UTC(), Tags: tags,
	}
	c.inst[in.ID] = in
	return in, nil
}

func (c *uiCloud) Get(_ context.Context, id string) (*cloud.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.inst[id]
	if !ok {
		return nil, cloud.ErrNotFound
	}
	return in, nil
}

func (c *uiCloud) Delete(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inst, id)
	return nil
}

func (c *uiCloud) List(_ context.Context, owner cloud.Owner) ([]*cloud.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*cloud.Instance
	for _, in := range c.inst {
		if owner.Matches(in.Tags) {
			out = append(out, in)
		}
	}
	return out, nil
}

// live reports how many machines the cloud still holds, which is how the
// browser tests assert that tearing a pool down through the UI actually
// reached the cloud rather than only the database.
func (c *uiCloud) live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inst)
}

// uiForge is a forge with a queue the test controls: push a job, and the
// controller has a reason to provision.
type uiForge struct {
	mu      sync.Mutex
	name    string
	queue   []forge.Job
	runners map[string]forge.Runner
	n       int
}

func newUIForge(id string) *uiForge {
	f := &uiForge{name: id, runners: map[string]forge.Runner{}}
	uiMu.Lock()
	defer uiMu.Unlock()
	uiForges[id] = f
	return f
}

func (f *uiForge) Name() string     { return f.name }
func (f *uiForge) Kind() forge.Kind { return "uifake" }

func (f *uiForge) Demand(_ context.Context, labels []string) ([]forge.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.Job
	for _, j := range f.queue {
		if subset(labels, j.Labels) {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *uiForge) Provision(_ context.Context, req forge.RunnerRequest) (*forge.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A real forge hands the job to the runner it was created for, so it stops
	// being demand. Without this the queue never drains and the controller
	// keeps building replacements for a job nobody ever takes.
	if req.JobID != "" {
		f.queue = slices.DeleteFunc(f.queue, func(j forge.Job) bool { return j.ID == req.JobID })
	}
	f.n++
	id := fmt.Sprintf("r-%d", f.n)
	f.runners[id] = forge.Runner{ID: id, Name: req.Name, Labels: req.Labels, Online: true}
	return &forge.Credential{Kind: "uifake", RunnerID: id, Token: "t-" + id}, nil
}

func (f *uiForge) Deprovision(_ context.Context, runnerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runners, runnerID)
	return nil
}

func (f *uiForge) ListRunners(_ context.Context) ([]forge.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]forge.Runner, 0, len(f.runners))
	for _, r := range f.runners {
		out = append(out, r)
	}
	return out, nil
}

func (f *uiForge) Bootstrap(
	cred *forge.Credential, _ cloud.CredentialMode, opts forge.BootstrapOptions,
) (cloud.Bootstrap, error) {
	return cloud.Bootstrap{
		ContainerImage: opts.ContainerImage,
		Env:            map[string]string{"RUNNER_TOKEN": cred.Token},
	}, nil
}

// enqueue makes a job wait for a runner with these labels.
func (f *uiForge) enqueue(id string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, forge.Job{ID: id, Labels: labels, Repo: "acme/site", QueuedAt: time.Now().UTC()})
}

// subset reports whether every label the job asks for is one the pool offers.
func subset(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
