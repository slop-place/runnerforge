package controller_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/store"
)

// harness wires a controller to a fake cloud and forge with one pool.
type harness struct {
	db    *store.DB
	ctrl  *controller.Controller
	cloud *fakeProvider
	forge *fakeForge
	pool  *store.Pool
	id    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	id := fmt.Sprintf("h-%s-%d", t.Name(), time.Now().UnixNano())
	key, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID: id, SecretKey: key,
		ReconcileInterval: time.Second, ReapInterval: time.Second,
		Database: config.Database{Driver: "sqlite", DSN: t.TempDir() + "/t.db"},
	}
	raw, err := cfg.Key()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetKey(raw); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{db: db, cloud: newFakeProvider(id), forge: newFakeForge(id), id: id}

	cl := &store.Cloud{
		Name: "fake-cloud", Driver: "fake", Enabled: true,
		Settings: store.Params{"fake_id": id},
	}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{CloudID: cl.ID, Name: "small", Spec: store.Params{}}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{
		Name: "fake-forge", Kind: "fake", Enabled: true,
		Settings: store.Params{"fake_id": id},
	}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "p", Enabled: true,
		ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID,
		Labels:         store.StringList{"linux"},
		MaxInstances:   5,
		JobTimeoutSec:  600,
		MaxLifetimeSec: 1200,
		ContainerImage: "fake:latest",
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}
	h.pool = pool

	log := slog.New(slog.DiscardHandler)
	h.ctrl = controller.New(db, cfg, log)
	return h
}

// owned builds a machine tagged as belonging to this harness's deployment.
func (h *harness) owned(name string, age time.Duration) *cloud.Instance {
	return &cloud.Instance{
		ID:        "m-" + name,
		Name:      name,
		State:     cloud.StateRunning,
		CreatedAt: time.Now().Add(-age),
		Tags: map[string]string{
			cloud.TagController: h.id,
			cloud.TagPool:       h.pool.Name,
		},
	}
}

func TestReapDestroysMachineWithNoDatabaseRecord(t *testing.T) {
	h := newHarness(t)
	// The classic crash case: the process died between creating the machine
	// and recording it. Nothing but the cloud knows this exists.
	h.cloud.add(h.owned("rf-orphan", time.Minute))

	if err := h.ctrl.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if h.cloud.has("m-rf-orphan") {
		t.Error("machine with no database record survived the reaper")
	}
}

func TestReapKeepsHealthyMachine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A machine the controller created and still believes in must survive.
	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := h.cloud.count(); got != 1 {
		t.Fatalf("expected 1 machine after reconcile, got %d", got)
	}

	if err := h.ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := h.cloud.count(); got != 1 {
		t.Errorf("reaper destroyed a healthy machine; %d left", got)
	}
}

// TestReapIgnoresMachineWithoutCreationTime is a regression test.
//
// Nova's create response does not populate a creation time. Treating a zero
// timestamp as an age would make every machine infinitely old, and the reaper
// would destroy the entire fleet mid-job on its first pass.
func TestReapIgnoresMachineWithoutCreationTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Blank the creation time the way a provider that does not report one would.
	for _, m := range mustList(t, h) {
		m.CreatedAt = time.Time{}
		h.cloud.add(m)
	}

	if err := h.ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := h.cloud.count(); got != 1 {
		t.Errorf("machine with no creation time was destroyed; %d left, want 1", got)
	}
}

func TestReapDestroysMachinePastMaxLifetime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forge.setJobs(forge.Job{ID: "j1", Labels: []string{"linux"}})
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Age it past the pool's ceiling.
	for _, m := range mustList(t, h) {
		m.CreatedAt = time.Now().Add(-2 * h.pool.MaxLifetime())
		h.cloud.add(m)
	}

	if err := h.ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := h.cloud.count(); got != 0 {
		t.Errorf("machine past its max lifetime survived; %d left", got)
	}
}

func TestReapIgnoresForeignNames(t *testing.T) {
	h := newHarness(t)
	// Tagged as ours but not named by us. Refusing to touch it is the guard
	// against destroying someone else's machine on a shared account.
	foreign := h.owned("production-database", time.Hour)
	h.cloud.add(foreign)

	if err := h.ctrl.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !h.cloud.has(foreign.ID) {
		t.Error("the reaper destroyed a machine that does not carry our name prefix")
	}
}

func TestReapIgnoresOtherDeployments(t *testing.T) {
	h := newHarness(t)
	other := &cloud.Instance{
		ID: "m-other", Name: "rf-other-abc", State: cloud.StateRunning,
		CreatedAt: time.Now().Add(-time.Hour),
		Tags: map[string]string{
			cloud.TagController: "a-different-runnerforge",
			cloud.TagPool:       "p",
		},
	}
	h.cloud.add(other)

	if err := h.ctrl.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !h.cloud.has("m-other") {
		t.Error("the reaper destroyed another deployment's machine")
	}
}

func TestReapRemovesOrphanedRunnerRegistration(t *testing.T) {
	h := newHarness(t)
	// A runner registered for a machine that never came up. It costs nothing
	// but it is a credential that was minted and never spent.
	h.forge.addRunner(forge.Runner{ID: "stale", Name: "rf-gone-123", Online: false})

	if err := h.ctrl.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if h.forge.runnerCount() != 0 {
		t.Error("orphaned runner registration survived the reaper")
	}
}

func TestReapKeepsBusyRunnerRegistration(t *testing.T) {
	h := newHarness(t)
	// Busy means it is running a job right now, even if we have no record of
	// the machine. Removing it would kill a live build.
	h.forge.addRunner(forge.Runner{ID: "busy", Name: "rf-busy-1", Online: true, Busy: true})

	if err := h.ctrl.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if h.forge.runnerCount() != 1 {
		t.Error("the reaper removed a runner that was executing a job")
	}
}

func TestReapSurvivesProviderListFailure(t *testing.T) {
	h := newHarness(t)
	h.cloud.listErr = errors.New("cloud is down")

	// The reaper must report the failure rather than concluding that an
	// unreachable cloud contains nothing.
	if err := h.ctrl.Reap(context.Background()); err == nil {
		t.Error("expected an error when the provider cannot be listed")
	}
}

func TestDestroyAllRemovesEverything(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.forge.setJobs(
		forge.Job{ID: "j1", Labels: []string{"linux"}},
		forge.Job{ID: "j2", Labels: []string{"linux"}},
	)
	if err := h.ctrl.ReconcileAll(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if h.cloud.count() != 2 {
		t.Fatalf("expected 2 machines, got %d", h.cloud.count())
	}

	n, err := h.ctrl.DestroyAll(ctx)
	if err != nil {
		t.Fatalf("destroy all: %v", err)
	}
	if n != 2 {
		t.Errorf("DestroyAll reported %d, want 2", n)
	}
	if h.cloud.count() != 0 {
		t.Errorf("%d machine(s) survived DestroyAll", h.cloud.count())
	}
}

func mustList(t *testing.T, h *harness) []*cloud.Instance {
	t.Helper()
	out, err := h.cloud.List(context.Background(), cloud.Owner{Controller: h.id})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
