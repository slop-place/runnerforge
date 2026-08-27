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
	_ "github.com/slop-place/runnerforge/internal/forge/gitlab"
	"github.com/slop-place/runnerforge/internal/store"
)

// TestGitLabEndToEnd drives the whole loop against a real GitLab instance.
//
// GitLab is the forge with no per-job credential, so this is also the test that
// proves runnerforge's own single-use enforcement works: the machine is started
// with --max-builds 1 and the runner registration is deleted afterwards, neither
// of which GitLab does for us.
//
// Requires a GitLab instance; see testdata/e2e-up.sh. Skipped when unset.
func TestGitLabEndToEnd(t *testing.T) {
	env := requireEnv(t, "RF_TEST_GITLAB_API", "RF_TEST_GITLAB_TOKEN",
		"RF_TEST_GITLAB_INTERNAL", "RF_TEST_GITLAB_PROJECT", "RF_TEST_DOCKER_NETWORK")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	gl := &gitlabTestClient{
		api:     env["RF_TEST_GITLAB_API"],
		token:   env["RF_TEST_GITLAB_TOKEN"],
		project: env["RF_TEST_GITLAB_PROJECT"],
	}

	tag := fmt.Sprintf("rf-e2e-%d", time.Now().UnixNano())
	db, ctrl := newGitLabController(t, env, tag)

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if n, err := ctrl.DestroyAll(c); err != nil {
			t.Errorf("cleanup: %v", err)
		} else if n > 0 {
			t.Logf("cleanup destroyed %d leftover machine(s)", n)
		}
	})

	if err := gl.putCI(ctx, tag); err != nil {
		t.Fatalf("install .gitlab-ci.yml: %v", err)
	}
	pipelineID, err := gl.trigger(ctx)
	if err != nil {
		t.Fatalf("trigger pipeline: %v", err)
	}
	t.Logf("triggered pipeline %d for tag %s", pipelineID, tag)

	deadline := time.Now().Add(4 * time.Minute)
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
		status, err := gl.pipelineStatus(ctx, pipelineID)
		if err == nil && terminalPipelineStatus(status) {
			t.Logf("pipeline finished with status %q", status)
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !launched {
		t.Fatal("controller never launched a machine for the pending job")
	}

	status, err := gl.pipelineStatus(ctx, pipelineID)
	if err != nil {
		t.Fatalf("final pipeline status: %v", err)
	}
	if status != "success" {
		dumpInstances(t, db)
		t.Fatalf("pipeline did not succeed: status=%q", status)
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

	// GitLab does not remove runners itself, so this asserts runnerforge's own
	// cleanup rather than the forge's.
	if err := ctrl.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	runners, err := gl.listRunners(ctx)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	for _, r := range runners {
		if strings.HasPrefix(r.Description, "rf-") {
			t.Errorf("orphaned runner registration left behind: %s (id %d)", r.Description, r.ID)
		}
	}
}

func terminalPipelineStatus(s string) bool {
	switch s {
	case "success", "failed", "canceled", "skipped":
		return true
	}
	return false
}

func newGitLabController(t *testing.T, env map[string]string, tag string) (*store.DB, *controller.Controller) {
	t.Helper()

	key, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID:                fmt.Sprintf("rf-gltest-%d", time.Now().UnixNano()),
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
		t.Fatal(err)
	}

	cl := &store.Cloud{Name: "local-docker", Driver: "docker", Enabled: true, Settings: store.Params{}}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{
		CloudID: cl.ID, Name: "small", VCPUs: 2, MemoryMB: 2048,
		Spec: store.Params{
			"cpus": 2.0, "memory_mb": 2048,
			"docker_socket": "/var/run/docker.sock",
			"network":       env["RF_TEST_DOCKER_NETWORK"],
		},
	}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	img := &store.Image{
		CloudID: cl.ID, Name: "runner",
		Spec: store.Params{"image": "gitlab/gitlab-runner:latest"},
	}
	if err := db.Create(img).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{
		Name: "local-gitlab", Kind: "gitlab", Enabled: true,
		Settings: store.Params{
			"url":        env["RF_TEST_GITLAB_INTERNAL"],
			"api_url":    env["RF_TEST_GITLAB_API"],
			"scope":      "project",
			"project_id": env["RF_TEST_GITLAB_PROJECT"],
			"job_image":  "alpine:latest",
		},
		Credentials: store.Secret{"token": env["RF_TEST_GITLAB_TOKEN"]},
	}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "e2e-gl", Enabled: true,
		ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID, ImageID: &img.ID,
		Labels:         store.StringList{tag},
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

// ---- minimal GitLab client for the test's own setup ----

type gitlabTestClient struct{ api, token, project string }

func (c *gitlabTestClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+"/api/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Private-Token", c.token)
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

// putCI writes a pipeline whose only job carries this run's unique tag.
func (c *gitlabTestClient) putCI(ctx context.Context, tag string) error {
	content := fmt.Sprintf(`hello:
  tags: [%s]
  image: alpine:latest
  script:
    - echo "runnerforge e2e ok"
`, tag)
	body := map[string]any{
		"branch":         "main",
		"content":        base64.StdEncoding.EncodeToString([]byte(content)),
		"encoding":       "base64",
		"commit_message": "e2e pipeline",
	}
	path := fmt.Sprintf("/projects/%s/repository/files/%s", c.project, ".gitlab-ci.yml")
	// Create, or update in place if a previous run left one behind.
	if err := c.do(ctx, http.MethodPost, path, body, nil); err != nil {
		return c.do(ctx, http.MethodPut, path, body, nil)
	}
	return nil
}

func (c *gitlabTestClient) trigger(ctx context.Context) (int64, error) {
	var p struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/projects/%s/pipeline?ref=main", c.project), nil, &p)
	return p.ID, err
}

func (c *gitlabTestClient) pipelineStatus(ctx context.Context, id int64) (string, error) {
	var p struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/pipelines/%d", c.project, id), nil, &p)
	return p.Status, err
}

type glRunner struct {
	ID          int64  `json:"id"`
	Description string `json:"description"`
}

func (c *gitlabTestClient) listRunners(ctx context.Context) ([]glRunner, error) {
	var out []glRunner
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/runners?per_page=100", c.project), nil, &out)
	return out, err
}
