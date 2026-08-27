package controller

import (
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/store"
)

func job(id string, labels ...string) forge.Job {
	return forge.Job{ID: id, Labels: labels}
}

func inst(state store.InstanceState, jobID string) store.Instance {
	return store.Instance{State: state, JobID: jobID}
}

func TestPlanScale(t *testing.T) {
	t.Parallel()
	pool := func(maxInst, minIdle int) *store.Pool {
		return &store.Pool{MaxInstances: maxInst, MinIdle: minIdle}
	}

	tests := []struct {
		name        string
		pool        *store.Pool
		matching    []forge.Job
		live        []store.Instance
		wantLaunch  []string
		wantWarm    int
		wantCeiling int
	}{
		{
			name:       "one job, nothing running",
			pool:       pool(5, 0),
			matching:   []forge.Job{job("j1")},
			wantLaunch: []string{"j1"},
		},
		{
			// The rule that stops double-provisioning: a job already covered by
			// a machine is not counted as demand again.
			name:       "job already covered",
			pool:       pool(5, 0),
			matching:   []forge.Job{job("j1")},
			live:       []store.Instance{inst(store.StateBooting, "j1")},
			wantLaunch: nil,
		},
		{
			name:       "one covered, one not",
			pool:       pool(5, 0),
			matching:   []forge.Job{job("j1"), job("j2")},
			live:       []store.Instance{inst(store.StateBooting, "j1")},
			wantLaunch: []string{"j2"},
		},
		{
			// A warm machine is unbound and can take anything, so it absorbs
			// demand before a new machine is created.
			name:       "warm machine absorbs demand",
			pool:       pool(5, 0),
			matching:   []forge.Job{job("j1")},
			live:       []store.Instance{inst(store.StateIdle, "")},
			wantLaunch: nil,
		},
		{
			name:       "two jobs, one warm machine",
			pool:       pool(5, 0),
			matching:   []forge.Job{job("j1"), job("j2")},
			live:       []store.Instance{inst(store.StateIdle, "")},
			wantLaunch: []string{"j2"},
		},
		{
			// The ceiling is the account-quota guard as much as a scaling knob.
			name:        "ceiling caps launches",
			pool:        pool(2, 0),
			matching:    []forge.Job{job("j1"), job("j2"), job("j3")},
			wantLaunch:  []string{"j1", "j2"},
			wantCeiling: 1,
		},
		{
			name:     "ceiling already reached",
			pool:     pool(1, 0),
			matching: []forge.Job{job("j1"), job("j2")},
			live:     []store.Instance{inst(store.StateBusy, "j0")},
			// Both jobs are blocked: the single slot is taken by a busy machine.
			wantLaunch:  nil,
			wantCeiling: 2,
		},
		{
			// A busy machine still occupies a slot even though it cannot take
			// new work.
			name:       "busy machines count against the ceiling",
			pool:       pool(2, 0),
			matching:   []forge.Job{job("j1")},
			live:       []store.Instance{inst(store.StateBusy, "j0")},
			wantLaunch: []string{"j1"},
		},
		{
			// Failed machines still hold resources until the reaper gets them.
			name:        "failed machines count against the ceiling",
			pool:        pool(1, 0),
			matching:    []forge.Job{job("j1")},
			live:        []store.Instance{inst(store.StateFailed, "")},
			wantLaunch:  nil,
			wantCeiling: 1,
		},
		{
			name:     "min idle tops up when there is no demand",
			pool:     pool(5, 2),
			matching: nil,
			wantWarm: 2,
		},
		{
			name:     "min idle accounts for existing warm machines",
			pool:     pool(5, 2),
			matching: nil,
			live:     []store.Instance{inst(store.StateIdle, "")},
			wantWarm: 1,
		},
		{
			name:       "min idle and demand together",
			pool:       pool(5, 1),
			matching:   []forge.Job{job("j1")},
			wantLaunch: []string{"j1"},
			wantWarm:   1,
		},
		{
			// Demand comes first; warming is what is left of the ceiling.
			name:       "ceiling leaves no room to warm",
			pool:       pool(1, 3),
			matching:   []forge.Job{job("j1")},
			wantLaunch: []string{"j1"},
			wantWarm:   0,
		},
		{
			name:       "deleted machines do not occupy a slot",
			pool:       pool(1, 0),
			matching:   []forge.Job{job("j1")},
			live:       []store.Instance{inst(store.StateDeleted, "j0")},
			wantLaunch: []string{"j1"},
		},
		{
			name:       "nothing queued, nothing to do",
			pool:       pool(5, 0),
			wantLaunch: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := planScale(tt.pool, tt.matching, tt.live)

			if len(got.forJobs) != len(tt.wantLaunch) {
				t.Fatalf("launching %d jobs %v, want %d %v",
					len(got.forJobs), jobIDs(got.forJobs), len(tt.wantLaunch), tt.wantLaunch)
			}
			for i, j := range got.forJobs {
				if j.ID != tt.wantLaunch[i] {
					t.Errorf("launch[%d] = %q, want %q", i, j.ID, tt.wantLaunch[i])
				}
			}
			if got.warm != tt.wantWarm {
				t.Errorf("warm = %d, want %d", got.warm, tt.wantWarm)
			}
			if got.atCeiling != tt.wantCeiling {
				t.Errorf("atCeiling = %d, want %d", got.atCeiling, tt.wantCeiling)
			}
		})
	}
}

func jobIDs(jobs []forge.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}

func TestMatchingJobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		poolLabels []string
		jobs       []forge.Job
		wantIDs    []string
	}{
		{
			name:       "exact match",
			poolLabels: []string{"linux"},
			jobs:       []forge.Job{job("j1", "linux")},
			wantIDs:    []string{"j1"},
		},
		{
			// Extra labels on the pool are fine; a job only needs its own
			// requirements covered.
			name:       "pool offers more than the job needs",
			poolLabels: []string{"self-hosted", "linux", "x64"},
			jobs:       []forge.Job{job("j1", "self-hosted", "linux")},
			wantIDs:    []string{"j1"},
		},
		{
			name:       "job needs a label the pool lacks",
			poolLabels: []string{"linux"},
			jobs:       []forge.Job{job("j1", "linux", "gpu")},
			wantIDs:    nil,
		},
		{
			name:       "case insensitive",
			poolLabels: []string{"Linux"},
			jobs:       []forge.Job{job("j1", "linux")},
			wantIDs:    []string{"j1"},
		},
		{
			// A pool configured with Forgejo's image-pinned label syntax still
			// matches a job asking for the bare label.
			name:       "image-pinned pool label",
			poolLabels: []string{"docker:docker://node:20"},
			jobs:       []forge.Job{job("j1", "docker")},
			wantIDs:    []string{"j1"},
		},
		{
			name:       "a job with no labels matches anything",
			poolLabels: []string{"linux"},
			jobs:       []forge.Job{job("j1")},
			wantIDs:    []string{"j1"},
		},
		{
			name:       "mixed",
			poolLabels: []string{"linux"},
			jobs:       []forge.Job{job("j1", "linux"), job("j2", "windows"), job("j3", "linux")},
			wantIDs:    []string{"j1", "j3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := matchingJobs(tt.jobs, tt.poolLabels)
			ids := jobIDs(got)
			if len(ids) != len(tt.wantIDs) {
				t.Fatalf("matched %v, want %v", ids, tt.wantIDs)
			}
			for i := range ids {
				if ids[i] != tt.wantIDs[i] {
					t.Errorf("[%d] = %q, want %q", i, ids[i], tt.wantIDs[i])
				}
			}
		})
	}
}

func TestNormalizeLabel(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"linux":                  "linux",
		"Linux":                  "linux",
		"docker:docker://alpine": "docker",
		"UBUNTU:docker://x":      "ubuntu",
		":leading":               ":leading",
	}
	for in, want := range tests {
		if got := normalizeLabel(in); got != want {
			t.Errorf("normalizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstanceName(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 50 {
		got, err := instanceName("My Pool/With Junk")
		if err != nil {
			t.Fatal(err)
		}
		// The name is the reaper's ownership fallback, so the prefix has to be
		// there and collisions have to be impossible in practice.
		if !strings.HasPrefix(got, "rf-") {
			t.Fatalf("name %q lacks the rf- prefix the reaper matches on", got)
		}
		if seen[got] {
			t.Fatalf("duplicate name generated: %q", got)
		}
		seen[got] = true

		for _, r := range got {
			isOK := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !isOK {
				t.Fatalf("name %q contains %q, which is not DNS-safe", got, r)
			}
		}
		if len(got) > 63 {
			t.Fatalf("name %q is too long for a hostname", got)
		}
	}
}

func TestInstanceNameTruncatesLongPools(t *testing.T) {
	t.Parallel()
	got, err := instanceName(strings.Repeat("x", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 63 {
		t.Errorf("name is %d characters: %q", len(got), got)
	}
}
