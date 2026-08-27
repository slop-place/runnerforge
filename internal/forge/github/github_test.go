package github_test

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
	_ "github.com/slop-place/runnerforge/internal/forge/github"
)

func newForge(t *testing.T, cfg map[string]any) forge.Forge {
	t.Helper()
	base := map[string]any{"token": "t", "owner": "o", "repo": "r"}
	maps.Copy(base, cfg)
	f, err := forge.New(forge.KindGitHub, base)
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
		{name: "repo scope", cfg: map[string]any{}},
		{name: "org scope", cfg: map[string]any{"scope": "org"}},
		{name: "GHES base url", cfg: map[string]any{"url": "https://ghe.example.com/api/v3"}},
		{name: "no token", cfg: map[string]any{"token": ""}, wantErr: "token is required"},
		{name: "no owner", cfg: map[string]any{"owner": ""}, wantErr: "owner is required"},
		{name: "repo scope without repo", cfg: map[string]any{"repo": ""}, wantErr: "requires repo"},
		{name: "unknown scope", cfg: map[string]any{"scope": "wat"}, wantErr: "unknown scope"},
		{
			name:    "non-numeric runner group",
			cfg:     map[string]any{"runner_group_id": "abc"},
			wantErr: "must be a number",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{"token": "t", "owner": "o", "repo": "r"}
			maps.Copy(cfg, tt.cfg)
			_, err := forge.New(forge.KindGitHub, cfg)
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

func TestBootstrapContainerMode(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{Kind: forge.KindGitHub, RunnerID: "5", JITConfig: "BASE64BLOB"},
		cloud.CredentialEnv,
		forge.BootstrapOptions{RunnerName: "rf-a", Labels: []string{"self-hosted"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if b.Env["RUNNERFORGE_JITCONFIG"] != "BASE64BLOB" {
		t.Error("the JIT config was not passed through the environment")
	}
	// The config is a credential; it must not appear on the command line.
	for _, arg := range b.Args {
		if strings.Contains(arg, "BASE64BLOB") {
			t.Error("the JIT config appears in the command line")
		}
	}
	if !strings.Contains(strings.Join(b.Args, " "), "run.sh --jitconfig") {
		t.Errorf("args do not invoke run.sh --jitconfig: %v", b.Args)
	}
}

func TestBootstrapCloudInit(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{JITConfig: "BLOB"},
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
	if !strings.Contains(ci, "shutdown -h") || !strings.Contains(ci, "poweroff") {
		t.Error("cloud-init lacks a self-destruct path")
	}
}

func TestBootstrapRejectsMissingJITConfig(t *testing.T) {
	f := newForge(t, nil)
	if _, err := f.Bootstrap(nil, cloud.CredentialEnv, forge.BootstrapOptions{}); err == nil {
		t.Error("expected an error for a nil credential")
	}
	if _, err := f.Bootstrap(&forge.Credential{}, cloud.CredentialEnv, forge.BootstrapOptions{}); err == nil {
		t.Error("expected an error for a credential with no JIT config")
	}
}

func TestAPICalls(t *testing.T) {
	var jitBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-jitconfig"):
			_ = json.NewDecoder(r.Body).Decode(&jitBody)
			_, _ = w.Write([]byte(`{"runner":{"id":42,"name":"rf-a"},"encoded_jit_config":"CFG"}`))
		case strings.HasSuffix(r.URL.Path, "/actions/runners"):
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[
			  {"id":42,"name":"rf-a","status":"online","busy":true,"labels":[{"name":"self-hosted"}]}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":9,"status":"queued"}]}`))
		case strings.Contains(r.URL.Path, "/actions/runs/9/jobs"):
			_, _ = w.Write([]byte(`{"jobs":[
			  {"id":100,"run_id":9,"status":"queued","labels":["self-hosted","linux"]},
			  {"id":101,"run_id":9,"status":"completed","labels":["self-hosted"]}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := newForge(t, map[string]any{"url": srv.URL})
	ctx := context.Background()

	t.Run("provision requests a JIT config", func(t *testing.T) {
		cred, err := f.Provision(ctx, forge.RunnerRequest{Name: "rf-a", Labels: []string{"self-hosted"}})
		if err != nil {
			t.Fatal(err)
		}
		if cred.JITConfig != "CFG" || cred.RunnerID != "42" {
			t.Errorf("credential = %+v", cred)
		}
		if cred.ExpiresAt.IsZero() {
			t.Error("GitHub documents a one-hour validity; ExpiresAt should reflect it")
		}
		if jitBody["name"] != "rf-a" {
			t.Errorf("request body = %v", jitBody)
		}
	})

	t.Run("provision requires labels", func(t *testing.T) {
		// Without labels the runner would match nothing and the machine would
		// be wasted, so this fails early rather than at the forge.
		if _, err := f.Provision(ctx, forge.RunnerRequest{Name: "rf-a"}); err == nil {
			t.Error("expected an error when no labels are given")
		}
	})

	t.Run("demand reports only queued jobs", func(t *testing.T) {
		jobs, err := f.Demand(ctx, []string{"self-hosted"})
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].ID != "100" {
			t.Errorf("jobs = %+v, want only the queued one", jobs)
		}
	})

	t.Run("list runners maps labels and busy", func(t *testing.T) {
		rs, err := f.ListRunners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 1 || !rs[0].Busy || !rs[0].Online {
			t.Fatalf("runners = %+v", rs)
		}
		if len(rs[0].Labels) != 1 || rs[0].Labels[0] != "self-hosted" {
			t.Errorf("labels = %v", rs[0].Labels)
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
