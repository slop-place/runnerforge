package controller_test

import (
	"context"
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
	_ "github.com/slop-place/runnerforge/internal/forge/github"
	"github.com/slop-place/runnerforge/internal/store"
)

// TestGitHubEndToEnd drives the whole loop against a real GitHub repository.
//
// Unlike the other two forges there is no self-hostable substitute for GitHub,
// so this test needs a real repository and a token. It is skipped when those
// are not configured, which keeps the rest of the suite runnable offline.
//
// The repository needs a workflow_dispatch workflow taking a `label` input and
// running on [self-hosted, <label>]; see testdata/github-e2e-workflow.yml.
func TestGitHubEndToEnd(t *testing.T) {
	env := requireEnv(t, "RF_TEST_GITHUB_TOKEN", "RF_TEST_GITHUB_OWNER", "RF_TEST_GITHUB_REPO")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	gh := &githubTestClient{
		token: env["RF_TEST_GITHUB_TOKEN"],
		owner: env["RF_TEST_GITHUB_OWNER"],
		repo:  env["RF_TEST_GITHUB_REPO"],
	}

	label := fmt.Sprintf("rf-e2e-%d", time.Now().Unix())
	db, ctrl := newGitHubController(t, env, label)

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if n, err := ctrl.DestroyAll(c); err != nil {
			t.Errorf("cleanup: %v", err)
		} else if n > 0 {
			t.Logf("cleanup destroyed %d leftover machine(s)", n)
		}
	})

	before := time.Now().Add(-time.Minute)
	if err := gh.dispatch(ctx, label); err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}
	runID, err := gh.findRun(ctx, before)
	if err != nil {
		t.Fatalf("find dispatched run: %v", err)
	}
	t.Logf("dispatched run %d for label %s", runID, label)

	// GitHub's API is rate limited, so this polls more gently than the
	// self-hosted forges do.
	deadline := time.Now().Add(6 * time.Minute)
	var launched bool
	for time.Now().Before(deadline) {
		if err := ctrl.ReconcileAll(ctx); err != nil {
			t.Logf("reconcile: %v", err)
		}
		insts, err := db.RecentInstances(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(insts) > 0 && !launched {
			launched = true
			t.Logf("controller launched %s", insts[0].Name)
		}
		status, conclusion, err := gh.runStatus(ctx, runID)
		if err == nil && status == "completed" {
			t.Logf("run completed with conclusion %q", conclusion)
			if conclusion != "success" {
				dumpInstances(t, db)
				t.Fatalf("workflow run concluded %q, want success", conclusion)
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !launched {
		t.Fatal("controller never launched a machine for the queued job")
	}

	status, conclusion, err := gh.runStatus(ctx, runID)
	if err != nil {
		t.Fatalf("final run status: %v", err)
	}
	if status != "completed" || conclusion != "success" {
		dumpInstances(t, db)
		t.Fatalf("workflow did not succeed: status=%q conclusion=%q", status, conclusion)
	}

	if err := drainUntilEmpty(ctx, ctrl, db); err != nil {
		dumpInstances(t, db)
		t.Fatalf("machines were not cleaned up: %v", err)
	}

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

	if err := ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	runners, err := gh.listRunners(ctx)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Name, "rf-") {
			t.Errorf("orphaned runner registration left behind: %s (id %d)", r.Name, r.ID)
		}
	}
}

func newGitHubController(t *testing.T, env map[string]string, label string) (*store.DB, *controller.Controller) {
	t.Helper()

	key, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID:                fmt.Sprintf("rf-ghtest-%d", time.Now().UnixNano()),
		SecretKey:         key,
		ReconcileInterval: 5 * time.Second,
		ReapInterval:      30 * time.Second,
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
		t.Fatal(err)
	}

	cl := &store.Cloud{Name: "local-docker", Driver: "docker", Enabled: true, Settings: store.Params{}}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{
		CloudID: cl.ID, Name: "small", VCPUs: 2, MemoryMB: 2048,
		Spec: store.Params{"cpus": 2.0, "memory_mb": 2048},
	}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	img := &store.Image{
		CloudID: cl.ID, Name: "runner",
		Spec: store.Params{"image": "ghcr.io/actions/actions-runner:latest"},
	}
	if err := db.Create(img).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{
		Name: "github", Kind: "github", Enabled: true,
		Settings: store.Params{
			"scope": "repo",
			"owner": env["RF_TEST_GITHUB_OWNER"],
			"repo":  env["RF_TEST_GITHUB_REPO"],
		},
		Credentials: store.Secret{"token": env["RF_TEST_GITHUB_TOKEN"]},
	}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "e2e-gh", Enabled: true,
		ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID, ImageID: &img.ID,
		// GitHub adds "self-hosted" to every job targeting a self-hosted runner,
		// so the pool has to offer it or nothing matches.
		Labels:         store.StringList{"self-hosted", label},
		MaxInstances:   2,
		JobTimeoutSec:  300,
		MaxLifetimeSec: 900,
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return db, controller.New(db, cfg, log)
}

// ---- minimal GitHub client for the test's own setup ----

type githubTestClient struct{ token, owner, repo string }

func (c *githubTestClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
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

func (c *githubTestClient) dispatch(ctx context.Context, label string) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/actions/workflows/e2e.yml/dispatches", c.owner, c.repo),
		map[string]any{"ref": "main", "inputs": map[string]string{"label": label}}, nil)
}

// findRun locates the run the dispatch created. workflow_dispatch returns no
// body, so the newest run created after the dispatch is the one.
func (c *githubTestClient) findRun(ctx context.Context, after time.Time) (int64, error) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var runs struct {
			Runs []struct {
				ID        int64     `json:"id"`
				CreatedAt time.Time `json:"created_at"`
				Event     string    `json:"event"`
			} `json:"workflow_runs"`
		}
		err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/%s/actions/runs?per_page=10", c.owner, c.repo), nil, &runs)
		if err == nil {
			for _, r := range runs.Runs {
				if r.Event == "workflow_dispatch" && r.CreatedAt.After(after) {
					return r.ID, nil
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return 0, fmt.Errorf("no workflow_dispatch run appeared")
}

func (c *githubTestClient) runStatus(ctx context.Context, id int64) (status, conclusion string, err error) {
	var run struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	err = c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/actions/runs/%d", c.owner, c.repo, id), nil, &run)
	return run.Status, run.Conclusion, err
}

type ghRunner struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (c *githubTestClient) listRunners(ctx context.Context) ([]ghRunner, error) {
	var resp struct {
		Runners []ghRunner `json:"runners"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/actions/runners?per_page=100", c.owner, c.repo), nil, &resp)
	return resp.Runners, err
}
