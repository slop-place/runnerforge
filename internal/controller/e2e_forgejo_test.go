package controller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/slop-place/runnerforge/internal/cloud/dockerdrv"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	_ "github.com/slop-place/runnerforge/internal/forge/forgejo"
	"github.com/slop-place/runnerforge/internal/store"
)

// TestForgejoEndToEnd drives the whole loop against a real Forgejo instance and
// a real Docker daemon: a job is queued, the controller provisions a runner for
// it, the job runs, and every resource is gone afterwards.
//
// Nothing here is mocked. The only substitution is that runners are containers
// instead of cloud VMs, which is what makes this cheap enough to run on
// every change.
//
// Requires a Forgejo instance; see testdata/e2e-up.sh. Skipped when unset.
func TestForgejoEndToEnd(t *testing.T) {
	env := requireEnv(t, "RF_TEST_FORGEJO_API", "RF_TEST_FORGEJO_TOKEN",
		"RF_TEST_FORGEJO_INTERNAL", "RF_TEST_FORGEJO_OWNER", "RF_TEST_FORGEJO_REPO",
		"RF_TEST_DOCKER_NETWORK")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	fj := &forgejoTestClient{
		api:   env["RF_TEST_FORGEJO_API"],
		token: env["RF_TEST_FORGEJO_TOKEN"],
		owner: env["RF_TEST_FORGEJO_OWNER"],
		repo:  env["RF_TEST_FORGEJO_REPO"],
	}

	// A label unique to this test run, so concurrent runs cannot steal each
	// other's jobs and the assertions stay meaningful.
	label := fmt.Sprintf("rf-e2e-%d", time.Now().UnixNano())

	db, ctrl := newTestController(t, env, label)

	if err := fj.putWorkflow(ctx, label); err != nil {
		t.Fatalf("install workflow: %v", err)
	}
	runID, err := fj.dispatch(ctx, label)
	if err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}
	t.Logf("dispatched workflow run %d with label %s", runID, label)

	// Always tear down, even on failure: a test that leaks machines is worse
	// than a test that fails.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		n, err := ctrl.DestroyAll(c)
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if n > 0 {
			t.Logf("cleanup destroyed %d leftover machine(s)", n)
		}
	})

	// Drive the loop until the job reaches a terminal state.
	deadline := time.Now().Add(4 * time.Minute)
	var launched bool
	for time.Now().Before(deadline) {
		if err := ctrl.ReconcileAll(ctx); err != nil {
			t.Logf("reconcile: %v", err)
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

	status, err := fj.runStatus(ctx, runID)
	if err != nil {
		t.Fatalf("final run status: %v", err)
	}
	if status != "success" {
		dumpInstances(t, db)
		t.Fatalf("workflow did not succeed: status=%q", status)
	}

	// The job is done; let the controller notice and tear the machine down.
	if err := drainUntilEmpty(ctx, ctrl, db); err != nil {
		dumpInstances(t, db)
		t.Fatalf("machines were not cleaned up: %v", err)
	}

	// The assertion that matters most: nothing is left running anywhere.
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
			t.Errorf("cloud %s still has %d machine(s) after the run; this is a leak",
				clouds[i].Name, n)
		}
	}

	// Finally, prove the reaper cleans up registrations whose machine is gone.
	// This run deliberately starts against whatever the shared test instance
	// already contains, so anything stale from an earlier run is fair game and
	// must be collected too.
	if err := ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	// And no runner registration is left behind on the forge.
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

// drainUntilEmpty reconciles until every instance row is closed out.
func drainUntilEmpty(ctx context.Context, ctrl *controller.Controller, db *store.DB) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctrl.ReconcileAll(ctx); err != nil {
			return err
		}
		live, err := db.AllLiveInstances(ctx)
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	// Fall back to the reaper, which is allowed to be the thing that saves us.
	if err := ctrl.Reap(ctx); err != nil {
		return err
	}
	live, err := db.AllLiveInstances(ctx)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("%d instance(s) still live after draining", len(live))
	}
	return nil
}

func dumpInstances(t *testing.T, db *store.DB) {
	t.Helper()
	insts, err := db.RecentInstances(context.Background(), 20)
	if err != nil {
		return
	}
	for _, in := range insts {
		t.Logf("instance %s state=%s provider=%s runner=%s err=%s",
			in.Name, in.State, in.ProviderID, in.ForgeRunnerID, in.Error)
	}
	evs, err := db.Events(context.Background(), 30)
	if err != nil {
		return
	}
	for _, e := range evs {
		t.Logf("event [%s] %s: %s", e.Level, e.Kind, e.Message)
	}
}

// newTestController builds a controller wired to the local Docker daemon and
// the local Forgejo instance.
func newTestController(t *testing.T, env map[string]string, label string) (*store.DB, *controller.Controller) {
	t.Helper()

	key, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID:                fmt.Sprintf("rf-test-%d", time.Now().UnixNano()),
		SecretKey:         key,
		ReconcileInterval: time.Second,
		ReapInterval:      10 * time.Second,
		Database:          config.Database{Driver: "sqlite", DSN: t.TempDir() + "/test.db"},
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
		t.Fatalf("open store: %v", err)
	}

	cl := &store.Cloud{
		Name: "local-docker", Driver: "docker", Enabled: true,
		Settings: store.Params{},
	}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{
		CloudID: cl.ID, Name: "small", VCPUs: 2, MemoryMB: 2048,
		Spec: store.Params{
			"cpus":      2.0,
			"memory_mb": 2048,
			// The runner starts job containers of its own, so it needs the
			// daemon socket and root to use it.
			"docker_socket": "/var/run/docker.sock",
			"user":          "0:0",
			"network":       env["RF_TEST_DOCKER_NETWORK"],
		},
	}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	img := &store.Image{
		CloudID: cl.ID, Name: "runner",
		Spec: store.Params{"image": "code.forgejo.org/forgejo/runner:12"},
	}
	if err := db.Create(img).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{
		Name: "local-forgejo", Kind: "forgejo", Enabled: true,
		Settings: store.Params{
			"url":     env["RF_TEST_FORGEJO_INTERNAL"],
			"api_url": env["RF_TEST_FORGEJO_API"],
			"scope":   "repo",
			"owner":   env["RF_TEST_FORGEJO_OWNER"],
			"repo":    env["RF_TEST_FORGEJO_REPO"],
		},
		Credentials: store.Secret{"token": env["RF_TEST_FORGEJO_TOKEN"]},
	}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "e2e", Enabled: true,
		ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID, ImageID: &img.ID,
		Labels:         store.StringList{label},
		MinIdle:        0,
		MaxInstances:   2,
		JobTimeoutSec:  240,
		MaxLifetimeSec: 480,
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return db, controller.New(db, cfg, log)
}

func requireEnv(t *testing.T, keys ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("%s not set; run testdata/e2e-up.sh and source its output", k)
		}
		out[k] = v
	}
	return out
}

// ---- minimal Forgejo client for the test's own setup ----

type forgejoTestClient struct {
	api, token, owner, repo string
}

func (c *forgejoTestClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+"/api/v1"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// putWorkflow writes a workflow whose runs-on matches this run's unique label.
func (c *forgejoTestClient) putWorkflow(ctx context.Context, label string) error {
	content := fmt.Sprintf(`name: e2e
on: [workflow_dispatch]
jobs:
  hello:
    runs-on: %s
    steps:
      - run: echo "runnerforge e2e ok"
`, label)
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	path := fmt.Sprintf("/repos/%s/%s/contents/.forgejo/workflows/e2e.yml", c.owner, c.repo)

	// Create, or update in place if a previous run left one behind.
	var existing struct {
		SHA string `json:"sha"`
	}
	_ = c.do(ctx, http.MethodGet, path, nil, &existing)
	body := map[string]any{"content": enc, "message": "e2e workflow", "branch": "main"}
	method := http.MethodPost
	if existing.SHA != "" {
		body["sha"] = existing.SHA
		method = http.MethodPut
	}
	return c.do(ctx, method, path, body, nil)
}

func (c *forgejoTestClient) dispatch(ctx context.Context, label string) (int64, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e.yml/dispatches", c.owner, c.repo)
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"ref": "main"}, nil); err != nil {
		return 0, err
	}
	// The dispatch endpoint returns no body, so find the run it created.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var runs struct {
			WorkflowRuns []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"workflow_runs"`
		}
		err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/%s/actions/runs?limit=5", c.owner, c.repo), nil, &runs)
		if err == nil && len(runs.WorkflowRuns) > 0 {
			return runs.WorkflowRuns[0].ID, nil
		}
		time.Sleep(time.Second)
	}
	return 0, fmt.Errorf("no workflow run appeared after dispatch")
}

// runStatus returns a run's status.
//
// Forgejo has no separate conclusion field: status carries the outcome directly
// and moves from waiting/running to one of the terminal values below.
func (c *forgejoTestClient) runStatus(ctx context.Context, id int64) (string, error) {
	var run struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/actions/runs/%d", c.owner, c.repo, id), nil, &run)
	return run.Status, err
}

// terminalRunStatus reports whether a run has finished, one way or another.
func terminalRunStatus(s string) bool {
	switch s {
	case "success", "failure", "cancelled", "skipped":
		return true
	}
	return false
}

type testRunner struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (c *forgejoTestClient) listRunners(ctx context.Context) ([]testRunner, error) {
	var out []testRunner
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/actions/runners", c.owner, c.repo), nil, &out)
	return out, err
}
