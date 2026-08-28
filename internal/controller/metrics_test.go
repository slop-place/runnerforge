package controller_test

import (
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// The labels on a machine metric have caught two bugs of the same shape now:
// an instance carries its pool but not that pool's cloud, so any code that
// reads the cloud off the row reports an empty label. Both times the series
// looked perfectly plausible — a real number under a blank name — which is
// exactly why it needs a test rather than a careful reading.

// samplesFor returns every sample of a metric family as label-set strings.
func samplesFor(t *testing.T, name string) []string {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			var parts []string
			for _, l := range m.GetLabel() {
				parts = append(parts, l.GetName()+"="+l.GetValue())
			}
			out = append(out, strings.Join(parts, ","))
		}
	}
	return out
}

// TestTeardownIsAttributedToItsPoolAndCloud is the regression test. Every
// teardown must name the pool and the cloud it happened on; a blank label means
// the metric cannot answer the only question anyone asks of it.
func TestTeardownIsAttributedToItsPoolAndCloud(t *testing.T) {
	h := newHarness(t)

	runOneJob(t, h)

	for _, name := range []string{
		"runnerforge_machines_destroyed_total",
		"runnerforge_machine_lifetime_seconds",
	} {
		samples := samplesFor(t, name)
		if len(samples) == 0 {
			t.Errorf("%s recorded nothing", name)
			continue
		}
		if !anyContains(samples, "pool=p") {
			t.Errorf("%s never names the pool: %v", name, samples)
		}
		if !anyContains(samples, "cloud=fake-cloud") {
			t.Errorf("%s never names the cloud: %v", name, samples)
		}
		for _, s := range samples {
			if strings.Contains(s, "pool=,") || strings.HasSuffix(s, "pool=") {
				t.Errorf("%s has a sample with a blank pool: %q", name, s)
			}
			if strings.Contains(s, "cloud=,") || strings.HasSuffix(s, "cloud=") {
				t.Errorf("%s has a sample with a blank cloud: %q", name, s)
			}
		}
	}
}

// TestCreatedAndDestroyedAgree checks the two counters an operator subtracts to
// spot a leak stay comparable: same pool, same cloud, same spelling.
func TestCreatedAndDestroyedAgree(t *testing.T) {
	h := newHarness(t)

	runOneJob(t, h)

	created := labelSets(samplesFor(t, "runnerforge_machines_created_total"), "pool", "cloud")
	destroyed := labelSets(samplesFor(t, "runnerforge_machines_destroyed_total"), "pool", "cloud")
	for k := range created {
		if !destroyed[k] {
			t.Errorf("machines created under %q but destroyed under a different label set %v",
				k, keys(destroyed))
		}
	}
}

// TestReapedMachineReasonsAreBounded guards the label that most easily becomes
// unbounded: the reaper's reason carries an age and a pool name in its text.
func TestReapedMachineReasonsAreBounded(t *testing.T) {
	h := newHarness(t)

	// Three strays of different ages. If the age reached the label these would
	// be three distinct series.
	for i, age := range []time.Duration{2 * time.Hour, 3 * time.Hour, 4 * time.Hour} {
		h.cloud.add(h.owned("rf-stray-"+string(rune('a'+i)), age))
	}
	if err := h.ctrl.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	reasons := map[string]bool{}
	for _, s := range samplesFor(t, "runnerforge_reaped_machines_total") {
		for part := range strings.SplitSeq(s, ",") {
			if r, ok := strings.CutPrefix(part, "reason="); ok {
				reasons[r] = true
			}
		}
	}
	if len(reasons) == 0 {
		t.Fatal("the reaper destroyed machines but recorded no reason")
	}
	for r := range reasons {
		if strings.ContainsAny(r, " (") {
			t.Errorf("reason %q reads like a sentence; labels must be categories", r)
		}
	}
	if len(reasons) > len(store.InstanceStates()) {
		t.Errorf("%d distinct reasons from 3 machines: %v", len(reasons), keys(reasons))
	}
}

// runOneJob takes a machine through the whole ordinary path: a job turns up,
// a machine is created for it, the job finishes and the machine powers off.
func runOneJob(t *testing.T, h *harness) {
	t.Helper()
	ctx := t.Context()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	for _, m := range mustList(t, h) {
		m.State = cloud.StateStopped
		h.cloud.add(m)
	}
	h.forge.setJobs()
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Fatalf("%d machine(s) survived the run", got)
	}
}

func anyContains(ss []string, want string) bool {
	for _, s := range ss {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// labelSets reduces samples to just the named labels, so two metrics can be
// compared on the labels they are supposed to share.
func labelSets(samples []string, want ...string) map[string]bool {
	out := map[string]bool{}
	for _, s := range samples {
		var kept []string
		for part := range strings.SplitSeq(s, ",") {
			for _, w := range want {
				if strings.HasPrefix(part, w+"=") {
					kept = append(kept, part)
				}
			}
		}
		out[strings.Join(kept, ",")] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
