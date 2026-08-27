package controller_test

import (
	"context"
	"errors"
	"testing"

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
