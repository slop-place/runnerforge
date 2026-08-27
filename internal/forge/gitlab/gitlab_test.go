package gitlab_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	_ "github.com/slop-place/runnerforge/internal/forge/gitlab"
)

func newForge(t *testing.T, cfg map[string]any) forge.Forge {
	t.Helper()
	base := map[string]any{
		"url": "http://gitlab.internal", "token": "t",
		"scope": "project", "project_id": "42",
	}
	maps.Copy(base, cfg)
	f, err := forge.New(forge.KindGitLab, base)
	if err != nil {
		t.Fatalf("build forge: %v", err)
	}
	return f
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     map[string]any
		wantErr string
	}{
		{name: "project scope", cfg: map[string]any{}},
		{name: "group scope", cfg: map[string]any{"scope": "group", "group_id": "9"}},
		{name: "instance scope", cfg: map[string]any{"scope": "instance"}},
		{name: "no url", cfg: map[string]any{"url": ""}, wantErr: "url is required"},
		{name: "no token", cfg: map[string]any{"token": ""}, wantErr: "token is required"},
		{
			name:    "project scope without project_id",
			cfg:     map[string]any{"project_id": ""},
			wantErr: "requires project_id",
		},
		{
			name:    "group scope without group_id",
			cfg:     map[string]any{"scope": "group"},
			wantErr: "requires group_id",
		},
		{name: "unknown scope", cfg: map[string]any{"scope": "wat"}, wantErr: "unknown scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{
				"url": "http://g", "token": "t", "scope": "project", "project_id": "42",
			}
			maps.Copy(cfg, tt.cfg)
			_, err := forge.New(forge.KindGitLab, cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestBootstrapEnforcesSingleUse(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{Kind: forge.KindGitLab, RunnerID: "3", AuthToken: "glrt-xyz"},
		cloud.CredentialEnv,
		forge.BootstrapOptions{RunnerName: "rf-a", Network: "rf-net"},
	)
	if err != nil {
		t.Fatal(err)
	}

	// GitLab has no per-job credential, so single-use is entirely on our side:
	// without --max-builds 1 the runner would happily take a second job.
	joined := strings.Join(b.Args, " ")
	if !strings.Contains(joined, "--max-builds 1") {
		t.Errorf("args do not limit the runner to one build: %q", joined)
	}
	if b.Env["CI_SERVER_TOKEN"] != "glrt-xyz" {
		t.Error("the runner token was not passed through the environment")
	}
	if b.Env["CI_SERVER_URL"] != "http://gitlab.internal" {
		t.Errorf("CI_SERVER_URL = %q, want the runner-facing URL", b.Env["CI_SERVER_URL"])
	}
	if strings.Contains(joined, "glrt-xyz") {
		t.Error("the token appears in the command line")
	}
	// Job containers must join the runner's network or they cannot clone.
	if b.Env["DOCKER_NETWORK_MODE"] != "rf-net" {
		t.Errorf("DOCKER_NETWORK_MODE = %q, want the runner's network", b.Env["DOCKER_NETWORK_MODE"])
	}
}

func TestBootstrapOmitsNetworkWhenUnset(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{AuthToken: "t"}, cloud.CredentialEnv, forge.BootstrapOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Env["DOCKER_NETWORK_MODE"]; ok {
		t.Error("DOCKER_NETWORK_MODE should be absent when no network is configured")
	}
}

func TestBootstrapCloudInit(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{AuthToken: "glrt-xyz"},
		cloud.CredentialCloudInit,
		forge.BootstrapOptions{RunnerName: "rf-a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	ci := string(b.CloudInit)
	if !strings.HasPrefix(ci, "#cloud-config") {
		t.Error("missing the #cloud-config header")
	}
	if !strings.Contains(ci, "--max-builds 1") {
		t.Error("cloud-init does not limit the runner to one build")
	}
	if !strings.Contains(ci, "poweroff") {
		t.Error("cloud-init does not power off after the job")
	}
}

func TestBootstrapRejectsMissingToken(t *testing.T) {
	f := newForge(t, nil)
	if _, err := f.Bootstrap(nil, cloud.CredentialEnv, forge.BootstrapOptions{}); err == nil {
		t.Error("expected an error for a nil credential")
	}
	if _, err := f.Bootstrap(&forge.Credential{}, cloud.CredentialEnv, forge.BootstrapOptions{}); err == nil {
		t.Error("expected an error for a credential with no token")
	}
}

func TestAPICalls(t *testing.T) {
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/user/runners"):
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"id":7,"token":"glrt-new"}`))
		case strings.Contains(r.URL.Path, "/jobs"):
			_, _ = w.Write([]byte(`[
			  {"id":1,"status":"pending","tag_list":["linux"],"ref":"main"},
			  {"id":2,"status":"running","tag_list":["linux"],"ref":"main"}
			]`))
		case strings.Contains(r.URL.Path, "/runners"):
			_, _ = w.Write([]byte(`[{"id":7,"description":"rf-a","online":true,"status":"active","tag_list":["linux"]}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := newForge(t, map[string]any{"api_url": srv.URL})
	ctx := context.Background()

	t.Run("provision creates a project runner", func(t *testing.T) {
		cred, err := f.Provision(ctx, forge.RunnerRequest{Name: "rf-a", Labels: []string{"linux"}})
		if err != nil {
			t.Fatal(err)
		}
		if cred.RunnerID != "7" || cred.AuthToken != "glrt-new" {
			t.Errorf("credential = %+v", cred)
		}
		if createBody["runner_type"] != "project_type" {
			t.Errorf("runner_type = %v", createBody["runner_type"])
		}
		// GitLab expects a number here; a quoted string is rejected.
		if got, ok := createBody["project_id"].(float64); !ok || got != 42 {
			t.Errorf("project_id = %#v, want the number 42", createBody["project_id"])
		}
	})

	t.Run("demand reports only pending jobs", func(t *testing.T) {
		jobs, err := f.Demand(ctx, []string{"linux"})
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].ID != "1" {
			t.Errorf("jobs = %+v, want only the pending one", jobs)
		}
	})

	t.Run("list runners uses description as the name", func(t *testing.T) {
		rs, err := f.ListRunners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// GitLab runners have no name field, so runnerforge stores the machine
		// name in description and the reaper matches on it.
		if len(rs) != 1 || rs[0].Name != "rf-a" {
			t.Errorf("runners = %+v", rs)
		}
	})

	t.Run("deprovision is idempotent", func(t *testing.T) {
		if err := f.Deprovision(ctx, "999"); err != nil {
			t.Errorf("deprovision of a missing runner should succeed, got %v", err)
		}
		if err := f.Deprovision(ctx, ""); err != nil {
			t.Errorf("deprovision with no id should be a no-op, got %v", err)
		}
	})
}

func TestDemandIsEmptyOutsideProjectScope(t *testing.T) {
	// Group and instance scope have no cross-project pending-jobs endpoint, so
	// polling reports nothing rather than guessing. Those deployments need one
	// pool per project.
	f := newForge(t, map[string]any{"scope": "instance"})
	jobs, err := f.Demand(context.Background(), []string{"linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected no jobs outside project scope, got %d", len(jobs))
	}
}
