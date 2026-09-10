package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/k8s"
	"github.com/slop-place/runnerforge/internal/k8s/k8stest"
)

// TestKubernetesEndToEnd is the Forgejo end-to-end run with the configuration
// coming from the cluster instead of the database: Cloud, Forge and Pool
// objects and a Secret are created in a namespace, the reconciler applies them
// into an empty runnerforge, and the controller then serves a real job from a
// real Forgejo on a real Docker daemon exactly as before. Along the way the
// Pool's status is checked against what the controller is doing, and at the
// end the Pool is deleted from the cluster and its record has to follow.
//
// Needs a cluster (RF_TEST_KUBECONFIG) as well as the Forgejo environment;
// skipped otherwise.
func TestKubernetesEndToEnd(t *testing.T) {
	env := requireEnv(t, "RF_TEST_FORGEJO_API", "RF_TEST_FORGEJO_TOKEN",
		"RF_TEST_FORGEJO_INTERNAL", "RF_TEST_FORGEJO_OWNER", "RF_TEST_FORGEJO_REPO",
		"RF_TEST_DOCKER_NETWORK")
	cluster := k8stest.Connect(t)
	cluster.InstallCRDs(t)
	ns := cluster.Namespace(t)

	ctx := t.Context()
	fj := &forgejoTestClient{
		api:   env["RF_TEST_FORGEJO_API"],
		token: env["RF_TEST_FORGEJO_TOKEN"],
		owner: env["RF_TEST_FORGEJO_OWNER"],
		repo:  env["RF_TEST_FORGEJO_REPO"],
	}
	label := fmt.Sprintf("rf-k8s-%d", time.Now().UnixNano())

	// The same records newTestController writes by hand, as manifests. Every
	// spec value is a string here, which is what the CRD schema allows.
	cluster.Secret(t, ns, "forge-token", map[string]string{"token": env["RF_TEST_FORGEJO_TOKEN"]})
	cluster.Create(t, ns, "clouds", "local-docker", map[string]any{
		"driver": "docker",
		"sizes": []any{map[string]any{
			"name": "small", "vcpus": int64(2), "memoryMB": int64(2048),
			"spec": map[string]any{
				"cpus": "2", "memory_mb": "2048",
				"docker_socket": "/var/run/docker.sock", "user": "0:0",
				"network": env["RF_TEST_DOCKER_NETWORK"],
			},
		}},
		"images": []any{map[string]any{
			"name": "runner", "spec": map[string]any{"image": "code.forgejo.org/forgejo/runner:12"},
		}},
	})
	cluster.Create(t, ns, "forges", "local-forgejo", map[string]any{
		"kind": "forgejo",
		"settings": map[string]any{
			"url":     env["RF_TEST_FORGEJO_INTERNAL"],
			"api_url": env["RF_TEST_FORGEJO_API"],
			"scope":   "repo",
			"owner":   env["RF_TEST_FORGEJO_OWNER"],
			"repo":    env["RF_TEST_FORGEJO_REPO"],
		},
		"secretRef": map[string]any{"name": "forge-token"},
	})
	cluster.Create(t, ns, "pools", "e2e", map[string]any{
		"forgeRef": "local-forgejo", "cloudRef": "local-docker",
		"size": "small", "image": "runner",
		"labels":             []any{label},
		"maxInstances":       int64(2),
		"jobTimeoutSeconds":  int64(240),
		"maxLifetimeSeconds": int64(480),
	})

	db, cfg := newTestStore(t)
	rec, err := k8s.New(k8s.Config{Namespace: ns, Kubeconfig: cluster.Kubeconfig}, db, testLogger())
	if err != nil {
		t.Fatalf("build reconciler: %v", err)
	}
	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, r := range []string{"clouds", "forges", "pools"} {
		name := map[string]string{"clouds": "local-docker", "forges": "local-forgejo", "pools": "e2e"}[r]
		if st := cluster.Status(t, ns, r, name); st.Phase() != "Ready" {
			t.Fatalf("%s %s: %s %s", r, name, st.Phase(), st.Message())
		}
	}
	ctrl := controller.New(db, cfg, testLogger())

	if err := fj.putWorkflow(ctx, label); err != nil {
		t.Fatalf("install workflow: %v", err)
	}
	runID, err := fj.dispatch(ctx)
	if err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}
	t.Logf("dispatched workflow run %d with label %s", runID, label)

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		n, err := ctrl.DestroyAll(c)
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if n > 0 {
			t.Logf("cleanup destroyed %d leftover machine(s)", n)
		}
	})

	// Drive both loops. The reconciler keeps the Pool's status current, so
	// the machine the controller launches has to show up there.
	deadline := time.Now().Add(4 * time.Minute)
	var launched bool
	var machinesSeen int64
	for time.Now().Before(deadline) {
		if err := ctrl.ReconcileAll(ctx); err != nil {
			t.Logf("reconcile: %v", err)
		}
		if err := rec.Reconcile(ctx); err != nil {
			t.Logf("kubernetes reconcile: %v", err)
		}
		if n := cluster.Status(t, ns, "pools", "e2e").Int("machines"); n > machinesSeen {
			machinesSeen = n
		}
		insts, err := db.RecentInstances(ctx, 20)
		if err != nil {
			t.Fatalf("list instances: %v", err)
		}
		if len(insts) > 0 && !launched {
			launched = true
			t.Logf("controller launched %s", insts[0].Name)
		}
		status, err := fj.runStatus(ctx, runID)
		if err == nil && terminalRunStatus(status) {
			t.Logf("run finished with status %q", status)
			if status != "success" {
				dumpInstances(t, db)
				t.Fatalf("workflow run finished %q, want success", status)
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !launched {
		t.Fatal("controller never launched a machine for the queued job")
	}
	if machinesSeen == 0 {
		t.Error("the Pool's status never reported the running machine")
	}

	// The reconciler must not have re-created or duplicated anything while
	// the controller was mutating the same rows.
	if pools, err := db.Pools(ctx); err != nil || len(pools) != 1 {
		t.Fatalf("pools after the run = %d (%v), want 1", len(pools), err)
	}

	if err := drainUntilEmpty(ctx, ctrl, db); err != nil {
		dumpInstances(t, db)
		t.Fatalf("machines were not cleaned up: %v", err)
	}
	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := cluster.Status(t, ns, "pools", "e2e").Int("machines"); n != 0 {
		t.Errorf("pool status still reports %d machine(s) after the drain", n)
	}

	// Retire the pool from the cluster; with nothing running, the record goes.
	cluster.Delete(t, ns, "pools", "e2e")
	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	if pools, err := db.Pools(ctx); err != nil || len(pools) != 0 {
		t.Errorf("pool record survived the object's deletion: %d (%v)", len(pools), err)
	}

	// Nothing left anywhere, same as the plain Forgejo run demands.
	clouds, err := db.Clouds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range clouds {
		n, err := ctrl.CountMachines(ctx, &clouds[i])
		if err != nil {
			t.Fatalf("count machines: %v", err)
		}
		if n != 0 {
			t.Errorf("cloud %s still has %d machine(s) after the run; this is a leak", clouds[i].Name, n)
		}
	}
	if err := ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	runners, err := fj.listRunners(ctx)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, "rf-") {
			t.Errorf("orphaned runner registration left on the forge: %s (id %d)", r.Name, r.ID)
		}
	}
}
