package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/store"
)

func TestReconcileLaunchesForQueuedJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := h.cloud.count(); got != 1 {
		t.Fatalf("expected 1 machine, got %d", got)
	}
	if got := h.forge.runnerCount(); got != 1 {
		t.Fatalf("expected 1 runner registration, got %d", got)
	}

	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 instance row, got %d", len(live))
	}
	// The machine is bound to the job it was created for, which is what stops a
	// second machine being launched for the same work.
	if live[0].JobID != "j1" {
		t.Errorf("JobID = %q, want j1", live[0].JobID)
	}
	if live[0].ProviderID == "" {
		t.Error("the provider id was not recorded")
	}
	if live[0].ForgeRunnerID == "" {
		t.Error("the forge runner id was not recorded")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	for range 5 {
		if err := h.ctrl.ReconcileAll(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	// Repeated passes over the same queued job must not pile up machines.
	if got := h.cloud.count(); got != 1 {
		t.Errorf("after 5 passes there are %d machines, want 1", got)
	}
}

func TestReconcileRespectsCeiling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pool.MaxInstances = 2
	if err := h.db.Save(h.pool).Error; err != nil {
		t.Fatal(err)
	}
	h.forge.setJobs(
		forge.Job{ID: "j1", Labels: []string{"linux"}},
		forge.Job{ID: "j2", Labels: []string{"linux"}},
		forge.Job{ID: "j3", Labels: []string{"linux"}},
		forge.Job{ID: "j4", Labels: []string{"linux"}},
	)

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The ceiling is the account-quota guard; exceeding it is how a bug becomes
	// a bill.
	if got := h.cloud.count(); got != 2 {
		t.Errorf("launched %d machines, want the ceiling of 2", got)
	}
}

func TestReconcileIgnoresJobsWithUnmatchedLabels(t *testing.T) {
	h := newHarness(t)
	h.forge.setJobs(
		forge.Job{ID: "j1", Labels: []string{"windows"}},
		forge.Job{ID: "j2", Labels: []string{"linux", "gpu"}},
	)
	if err := h.ctrl.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("launched %d machines for jobs this pool cannot serve", got)
	}
}

func TestReconcileSkipsDisabledPool(t *testing.T) {
	h := newHarness(t)
	h.pool.Enabled = false
	if err := h.db.Save(h.pool).Error; err != nil {
		t.Fatal(err)
	}
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	if err := h.ctrl.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("a disabled pool launched %d machines", got)
	}
}

func TestReconcileMaintainsMinIdle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pool.MinIdle = 2
	if err := h.db.Save(h.pool).Error; err != nil {
		t.Fatal(err)
	}

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.cloud.count(); got != 2 {
		t.Fatalf("expected 2 warm machines, got %d", got)
	}
	// Warm machines are unbound, so any job can take them.
	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range live {
		if in.JobID != "" {
			t.Errorf("warm machine %s is bound to job %q", in.Name, in.JobID)
		}
	}
}

func TestLaunchRollsBackRunnerWhenProvisionFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.cloud.failProvision = errors.New("quota exceeded")
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	if err := h.ctrl.ReconcileAll(ctx); err == nil {
		t.Fatal("expected reconcile to report the provisioning failure")
	}
	// The credential was already minted; leaving it behind would put an offline
	// runner in the operator's forge forever.
	if got := h.forge.runnerCount(); got != 0 {
		t.Errorf("%d runner registration(s) left after a failed provision", got)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("%d machine(s) exist after a failed provision", got)
	}
}

func TestLaunchRecordsFailureWhenCredentialFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.provisionErr = errors.New("forge is down")
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	if err := h.ctrl.ReconcileAll(ctx); err == nil {
		t.Fatal("expected an error")
	}
	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].State != store.StateFailed {
		t.Fatalf("expected one failed row, got %+v", live)
	}
	// The row records why, so an operator is not left guessing.
	if live[0].Error == "" {
		t.Error("the failure reason was not recorded")
	}
}

func TestMachineIsTornDownWhenItStops(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}

	// The job finished and the machine powered itself off.
	for _, m := range mustList(t, h) {
		m.State = cloud.StateStopped
		h.cloud.add(m)
	}
	h.forge.setJobs() // queue is empty now

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("%d stopped machine(s) were not destroyed", got)
	}
	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("%d instance row(s) still live after teardown", len(live))
	}
}

func TestMachineIsTornDownWhenProviderLosesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}

	// Someone deleted the machine outside runnerforge. The row must be closed
	// out, or it would count against the ceiling forever and throttle the pool.
	for _, m := range mustList(t, h) {
		_ = h.cloud.Delete(ctx, m.ID)
	}
	h.forge.setJobs()

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("%d row(s) still live after the machine vanished", len(live))
	}
	// The registration goes with it.
	if got := h.forge.runnerCount(); got != 0 {
		t.Errorf("%d runner registration(s) left behind", got)
	}
}

// TestForgeListFailureDoesNotDestroyMachines is a regression guard.
//
// A vanished runner registration is how runnerforge learns a job finished. A
// failed API call also produces an empty list, so without distinguishing the
// two, one network blip would look like "every job finished at once" and
// destroy machines mid-build.
func TestForgeListFailureDoesNotDestroyMachines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	if h.cloud.count() != 1 {
		t.Fatalf("setup: expected 1 machine, got %d", h.cloud.count())
	}

	h.forge.listErr = errors.New("forge unreachable")
	h.forge.setJobs()

	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if got := h.cloud.count(); got != 1 {
		t.Error("a machine was destroyed because the forge was briefly unreachable")
	}
}

func TestDestroyInstanceFromUI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("setup: %v %d", err, len(live))
	}

	if err := h.ctrl.DestroyInstance(ctx, live[0].ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("%d machine(s) survived an explicit destroy", got)
	}

	// Destroying twice is a no-op rather than an error.
	if err := h.ctrl.DestroyInstance(ctx, live[0].ID); err != nil {
		t.Errorf("second destroy should be a no-op, got %v", err)
	}
}

func TestDestroyFailureKeepsRowAlive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}

	h.cloud.failDelete = true
	live, _ := h.db.LiveInstances(ctx, h.pool.ID)
	if err := h.ctrl.DestroyInstance(ctx, live[0].ID); err == nil {
		t.Fatal("expected the delete failure to be reported")
	}

	// The row must stay alive so the next pass tries again. Closing it out
	// while the machine still runs is exactly how a leak happens.
	after, err := h.db.LiveInstances(ctx, h.pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Error("the row was closed out even though the machine still exists")
	}
}

func TestCheckCloudAndForgeRecordStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	clouds, err := h.db.Clouds(ctx)
	if err != nil || len(clouds) != 1 {
		t.Fatalf("setup: %v", err)
	}
	h.ctrl.CheckCloud(ctx, &clouds[0])
	if clouds[0].Status != "ok" {
		t.Errorf("cloud status = %q, want ok", clouds[0].Status)
	}

	// A failing provider must be reported, with the reason, so the operator
	// learns at save time rather than when a job is already waiting.
	h.cloud.listErr = errors.New("bad credentials")
	h.ctrl.CheckCloud(ctx, &clouds[0])
	if clouds[0].Status != "error" || clouds[0].StatusDetail == "" {
		t.Errorf("cloud status = %q detail = %q", clouds[0].Status, clouds[0].StatusDetail)
	}

	forges, err := h.db.Forges(ctx)
	if err != nil || len(forges) != 1 {
		t.Fatalf("setup: %v", err)
	}
	h.ctrl.CheckForge(ctx, &forges[0])
	if forges[0].Status != "ok" {
		t.Errorf("forge status = %q, want ok", forges[0].Status)
	}
}

func TestDestroyInstanceDegradesGracefully(t *testing.T) {
	ctx := context.Background()

	t.Run("nothing provisioned needs no clients", func(t *testing.T) {
		h := newHarness(t)
		inst := &store.Instance{Name: "rf-empty", PoolID: h.pool.ID, State: store.StatePending}
		if err := h.db.Create(inst).Error; err != nil {
			t.Fatal(err)
		}
		// A row with nothing behind it must be closeable even if every client
		// is broken, or an operator cannot clear a stuck pool.
		if err := h.ctrl.DestroyInstance(ctx, inst.ID); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		live, _ := h.db.LiveInstances(ctx, h.pool.ID)
		if len(live) != 0 {
			t.Error("the row was not closed out")
		}
	})

	t.Run("a broken forge does not block the machine", func(t *testing.T) {
		h := newHarness(t)
		h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
		if err := h.ctrl.ReconcileAll(ctx); err != nil {
			t.Fatal(err)
		}
		live, _ := h.db.LiveInstances(ctx, h.pool.ID)
		if len(live) != 1 {
			t.Fatalf("setup: %d instances", len(live))
		}

		// Break the forge connection the way a rotated token would.
		var fg store.Forge
		if err := h.db.First(&fg, h.pool.ForgeID).Error; err != nil {
			t.Fatal(err)
		}
		fg.Settings = store.Params{"fake_id": "no-such-forge"}
		if err := h.db.Save(&fg).Error; err != nil {
			t.Fatal(err)
		}

		if err := h.ctrl.DestroyInstance(ctx, live[0].ID); err != nil {
			t.Fatalf("destroy with a broken forge: %v", err)
		}
		// The machine is what costs money; it must go regardless.
		if h.cloud.count() != 0 {
			t.Error("the machine survived because the forge was unreachable")
		}
	})

	t.Run("a missing instance is an error", func(t *testing.T) {
		h := newHarness(t)
		if err := h.ctrl.DestroyInstance(ctx, 9999); err == nil {
			t.Error("expected an error for an unknown instance")
		}
	})
}

func TestCountMachines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	clouds, err := h.db.Clouds(ctx)
	if err != nil || len(clouds) != 1 {
		t.Fatalf("setup: %v", err)
	}
	n, err := h.ctrl.CountMachines(ctx, &clouds[0])
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d on an empty cloud", n)
	}

	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ = h.ctrl.CountMachines(ctx, &clouds[0]); n != 1 {
		t.Errorf("count = %d after launching one machine", n)
	}

	// Machines that are not ours are not counted, even in the same account.
	h.cloud.add(&cloud.Instance{
		ID: "foreign", Name: "someone-elses-database", State: cloud.StateRunning,
		Tags: map[string]string{cloud.TagController: h.id, cloud.TagPool: "p"},
	})
	if n, _ = h.ctrl.CountMachines(ctx, &clouds[0]); n != 1 {
		t.Errorf("count = %d; a machine without our name prefix was counted", n)
	}
}

func TestOwnerHelper(t *testing.T) {
	h := newHarness(t)
	o := h.ctrl.Owner("mypool")
	if o.Controller != h.id || o.Pool != "mypool" {
		t.Errorf("Owner = %+v", o)
	}
	// An empty pool is the controller-wide sweep the reaper uses.
	if h.ctrl.Owner("").Pool != "" {
		t.Error("an empty pool should stay empty")
	}
}

func TestResolverRebuildsAfterAnEdit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}

	// Editing a cloud in the UI must transparently invalidate the cached
	// client, or the controller would keep using the old credentials.
	var cl store.Cloud
	if err := h.db.First(&cl, h.pool.CloudID).Error; err != nil {
		t.Fatal(err)
	}
	cl.Settings = store.Params{"fake_id": "no-such-cloud"}
	if err := h.db.Save(&cl).Error; err != nil {
		t.Fatal(err)
	}

	err := h.ctrl.ReconcileAll(ctx)
	if err == nil {
		t.Error("after pointing the cloud at a driver that cannot be built, reconcile should fail")
	}
}

func TestRunReapsAtStartupThenLoops(t *testing.T) {
	h := newHarness(t)

	// A machine left behind by a previous process, with nothing watching it.
	// Reaping on startup matters because it is billing for every second we
	// wait for the first tick.
	h.cloud.add(&cloud.Instance{
		ID: "orphan", Name: "rf-orphan-1", State: cloud.StateRunning,
		CreatedAt: time.Now().Add(-time.Minute),
		Tags: map[string]string{
			cloud.TagController: h.id, cloud.TagPool: h.pool.Name,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ctrl.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for h.cloud.count() > 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("the startup reap did not collect the orphaned machine")
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		// Cancellation is how the process stops; it must be reported as
		// context cancellation rather than as a controller failure.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want a context cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestRunKeepsGoingAfterAFailedPass(t *testing.T) {
	h := newHarness(t)
	// A broken cloud must not stop the loop: the next pass has to run, or a
	// transient outage would wedge the controller until someone restarts it.
	h.cloud.listErr = errors.New("cloud is down")
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := h.ctrl.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned %v; it should have kept going until the deadline", err)
	}
}
