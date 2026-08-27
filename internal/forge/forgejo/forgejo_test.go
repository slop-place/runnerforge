package forgejo_test

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
	_ "github.com/slop-place/runnerforge/internal/forge/forgejo"
)

func newForge(t *testing.T, cfg map[string]any) forge.Forge {
	t.Helper()
	base := map[string]any{
		"url": "http://forge.internal", "token": "t",
		"scope": "repo", "owner": "o", "repo": "r",
	}
	maps.Copy(base, cfg)
	f, err := forge.New(forge.KindForgejo, base)
	if err != nil {
		t.Fatalf("build forge: %v", err)
	}
	return f
}

func TestNewValidatesScope(t *testing.T) {
	tests := []struct {
		name    string
		cfg     map[string]any
		wantErr string
	}{
		{name: "repo scope", cfg: map[string]any{"scope": "repo"}},
		{name: "org scope", cfg: map[string]any{"scope": "org", "repo": ""}},
		{name: "user scope", cfg: map[string]any{"scope": "user"}},
		{name: "admin scope", cfg: map[string]any{"scope": "admin"}},
		{
			name:    "repo scope without a repo",
			cfg:     map[string]any{"scope": "repo", "repo": ""},
			wantErr: "requires owner and repo",
		},
		{
			name:    "org scope without an owner",
			cfg:     map[string]any{"scope": "org", "owner": ""},
			wantErr: "requires owner",
		},
		{name: "unknown scope", cfg: map[string]any{"scope": "nonsense"}, wantErr: "unknown scope"},
		{name: "no url", cfg: map[string]any{"url": ""}, wantErr: "url is required"},
		{name: "no token", cfg: map[string]any{"token": ""}, wantErr: "token is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{
				"url": "http://f", "token": "t", "owner": "o", "repo": "r",
			}
			maps.Copy(cfg, tt.cfg)
			_, err := forge.New(forge.KindForgejo, cfg)
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
	cred := &forge.Credential{
		Kind: forge.KindForgejo, RunnerID: "7", UUID: "uuid-1", Token: "tok-1",
	}
	b, err := f.Bootstrap(cred, cloud.CredentialEnv, forge.BootstrapOptions{
		RunnerName: "rf-a", Labels: []string{"linux"}, JobID: "handle-9",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The credential must travel in the environment, not on the command line,
	// where anything able to read /proc could see it.
	rf := b.Env["RUNNERFORGE_RUNNER_FILE"]
	if rf == "" {
		t.Fatal("runner file was not passed through the environment")
	}
	for _, arg := range b.Args {
		if strings.Contains(arg, "tok-1") {
			t.Error("the runner token appears in the command line")
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(rf), &parsed); err != nil {
		t.Fatalf("runner file is not valid JSON: %v", err)
	}
	if parsed["uuid"] != "uuid-1" || parsed["token"] != "tok-1" {
		t.Errorf("runner file lost the credential: %v", parsed)
	}
	if parsed["address"] != "http://forge.internal" {
		t.Errorf("address = %v, want the runner-facing URL", parsed["address"])
	}

	// --wait stops a runner that starts a moment early from exiting, and
	// --handle pins it to the job it was created for.
	args := b.Env["RUNNERFORGE_ONE_JOB_ARGS"]
	if !strings.Contains(args, "--wait") {
		t.Error("one-job args are missing --wait")
	}
	if !strings.Contains(args, "--handle handle-9") {
		t.Errorf("one-job args = %q, want --handle handle-9", args)
	}
}

func TestBootstrapWithoutJobOmitsHandle(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{UUID: "u", Token: "t"},
		cloud.CredentialEnv,
		forge.BootstrapOptions{RunnerName: "rf-a", Labels: []string{"linux"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	args := b.Env["RUNNERFORGE_ONE_JOB_ARGS"]
	if strings.Contains(args, "--handle") {
		t.Errorf("a warm machine should not pin a handle, got %q", args)
	}
	if !strings.Contains(args, "--wait") {
		t.Error("a warm machine still needs --wait")
	}
}

func TestBootstrapCloudInit(t *testing.T) {
	f := newForge(t, nil)
	b, err := f.Bootstrap(
		&forge.Credential{UUID: "u", Token: "t"},
		cloud.CredentialCloudInit,
		forge.BootstrapOptions{RunnerName: "rf-a", Labels: []string{"linux"}, SSHKey: "ssh-ed25519 AAAA"},
	)
	if err != nil {
		t.Fatal(err)
	}
	ci := string(b.CloudInit)
	if !strings.HasPrefix(ci, "#cloud-config") {
		t.Error("cloud-init must start with the #cloud-config header")
	}
	// The self-destruct is the machine's own dead-man switch for when the
	// controller is not around to reap it.
	if !strings.Contains(ci, "shutdown -h") {
		t.Error("cloud-init has no shutdown timer")
	}
	if !strings.Contains(ci, "poweroff") {
		t.Error("cloud-init does not power off after the job")
	}
	if !strings.Contains(ci, "ssh-ed25519 AAAA") {
		t.Error("the SSH key was not installed")
	}
}

func TestBootstrapRejectsIncompleteCredential(t *testing.T) {
	f := newForge(t, nil)
	for _, cred := range []*forge.Credential{
		nil,
		{UUID: "", Token: "t"},
		{UUID: "u", Token: ""},
	} {
		if _, err := f.Bootstrap(cred, cloud.CredentialEnv, forge.BootstrapOptions{}); err == nil {
			t.Errorf("expected an error for credential %+v", cred)
		}
	}
}

func TestBootstrapRejectsUnknownMode(t *testing.T) {
	f := newForge(t, nil)
	_, err := f.Bootstrap(&forge.Credential{UUID: "u", Token: "t"}, "telepathy", forge.BootstrapOptions{})
	if err == nil {
		t.Fatal("expected an error for an unsupported credential mode")
	}
}

// TestAPICalls exercises the HTTP surface against a stub instance, so the
// request shapes are pinned without needing a real Forgejo.
func TestAPICalls(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runners") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":11,"uuid":"u-11","token":"t-11"}`))
		case strings.HasSuffix(r.URL.Path, "/actions/runners") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":11,"uuid":"u-11","name":"rf-a","status":"online","labels":["linux"],"ephemeral":true}]`))
		case strings.HasSuffix(r.URL.Path, "/actions/runners/jobs"):
			_, _ = w.Write([]byte(`[
			  {"id":1,"handle":"h-1","runs_on":["linux"],"status":"waiting","repo_id":3},
			  {"id":2,"handle":"h-2","runs_on":["linux"],"status":"running","repo_id":3}
			]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := newForge(t, map[string]any{"api_url": srv.URL, "token": "secret-token"})
	ctx := context.Background()

	t.Run("provision asks for an ephemeral runner", func(t *testing.T) {
		cred, err := f.Provision(ctx, forge.RunnerRequest{Name: "rf-a", Labels: []string{"linux"}})
		if err != nil {
			t.Fatal(err)
		}
		if cred.RunnerID != "11" || cred.UUID != "u-11" || cred.Token != "t-11" {
			t.Errorf("credential = %+v", cred)
		}
		if gotBody["ephemeral"] != true {
			t.Error("ephemeral was not requested; the runner could take more than one job")
		}
		if gotAuth != "token secret-token" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotPath != "/api/v1/repos/o/r/actions/runners" {
			t.Errorf("path = %q", gotPath)
		}
	})

	t.Run("demand reports only waiting jobs", func(t *testing.T) {
		jobs, err := f.Demand(ctx, []string{"linux"})
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 {
			t.Fatalf("got %d jobs, want only the waiting one", len(jobs))
		}
		// The handle identifies one attempt at one job, which is what a runner
		// binds to; the numeric id is not attempt-specific.
		if jobs[0].ID != "h-1" {
			t.Errorf("job ID = %q, want the handle", jobs[0].ID)
		}
	})

	t.Run("list runners", func(t *testing.T) {
		rs, err := f.ListRunners(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 1 || rs[0].ID != "11" || !rs[0].Online {
			t.Errorf("runners = %+v", rs)
		}
	})

	t.Run("deprovision is idempotent", func(t *testing.T) {
		// The stub 404s on delete, which is what a runner Forgejo has already
		// retired looks like. That has to read as success.
		if err := f.Deprovision(ctx, "11"); err != nil {
			t.Errorf("deprovision of a missing runner should succeed, got %v", err)
		}
		if err := f.Deprovision(ctx, ""); err != nil {
			t.Errorf("deprovision with no id should be a no-op, got %v", err)
		}
		_ = gotMethod
	})
}
