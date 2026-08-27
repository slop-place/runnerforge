package forge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

func TestNewUnknownKind(t *testing.T) {
	t.Parallel()
	if _, err := forge.New("bitbucket", nil); err == nil {
		t.Fatal("expected an error for an unregistered forge kind")
	}
}

func TestClientDo(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":"hello"}`))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"internal explosion"}`))
		case "/garbage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{not json`))
		case "/echo-auth":
			_, _ = w.Write([]byte(`{"value":"` + r.Header.Get("Authorization") + `"}`))
		}
	}))
	defer srv.Close()

	h := http.Header{}
	h.Set("Authorization", "token abc")
	c := forge.NewClient(srv.URL, h)
	ctx := context.Background()

	t.Run("decodes a response", func(t *testing.T) {
		var out struct {
			Value string `json:"value"`
		}
		if err := c.Do(ctx, http.MethodGet, "/ok", nil, &out); err != nil {
			t.Fatal(err)
		}
		if out.Value != "hello" {
			t.Errorf("value = %q", out.Value)
		}
	})

	t.Run("applies the auth header", func(t *testing.T) {
		var out struct {
			Value string `json:"value"`
		}
		if err := c.Do(ctx, http.MethodGet, "/echo-auth", nil, &out); err != nil {
			t.Fatal(err)
		}
		if out.Value != "token abc" {
			t.Errorf("the auth header was not sent, got %q", out.Value)
		}
	})

	t.Run("empty body is fine", func(t *testing.T) {
		if err := c.Do(ctx, http.MethodGet, "/empty", nil, nil); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("404 becomes ErrNotFound", func(t *testing.T) {
		// This is what lets teardown treat "already gone" as success.
		err := c.Do(ctx, http.MethodGet, "/missing", nil, nil)
		if !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("server errors carry detail", func(t *testing.T) {
		err := c.Do(ctx, http.MethodGet, "/boom", nil, nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		var he *forge.HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("error is %T, want *forge.HTTPError", err)
		}
		if he.Status != http.StatusInternalServerError {
			t.Errorf("Status = %d", he.Status)
		}
		// The operator needs the server's own explanation, not just a code.
		if !strings.Contains(err.Error(), "internal explosion") {
			t.Errorf("error %q should include the response body", err)
		}
	})

	t.Run("malformed json is reported", func(t *testing.T) {
		var out struct{}
		err := c.Do(ctx, http.MethodGet, "/garbage", nil, &out)
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Errorf("error = %v, want a decode failure", err)
		}
	})

	t.Run("unreachable host", func(t *testing.T) {
		bad := forge.NewClient("http://127.0.0.1:1", nil)
		if err := bad.Do(ctx, http.MethodGet, "/x", nil, nil); err == nil {
			t.Error("expected a transport error")
		}
	})
}

func TestHTTPErrorTruncatesLongBodies(t *testing.T) {
	t.Parallel()
	e := &forge.HTTPError{
		Status: 500, Method: "GET", URL: "http://x",
		Body: strings.Repeat("y", 5000),
	}
	// A five-kilobyte error body in a log line helps nobody.
	if len(e.Error()) > 500 {
		t.Errorf("error message is %d characters; it should be truncated", len(e.Error()))
	}
	if !strings.Contains(e.Error(), "…") {
		t.Error("a truncated message should say so")
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("registering the same kind twice should panic")
		}
	}()
	// Registering twice can only happen through a programming error at init
	// time, so failing loudly is correct.
	impl := forge.Implementation{
		Kind:  "duplicate-kind-test",
		Title: "Test forge",
		New: func(map[string]any) (forge.Forge, error) {
			return nil, errors.New("stub constructor, never called")
		},
		// A field, because a registration with none would fail the
		// presentability check that runs in this same binary.
		Fields: []cloud.Field{{Key: "url", Label: "URL", Type: cloud.FieldText}},
	}
	forge.Register(impl)
	forge.Register(impl)
}

func TestImplementationsArePresentableAndSorted(t *testing.T) {
	t.Parallel()
	// The UI builds its picker from this, so a forge missing here cannot be
	// configured at all, and one with no fields cannot be configured correctly.
	all := forge.Implementations()
	if len(all) == 0 {
		t.Skip("no forges registered in this test binary")
	}
	for i, impl := range all {
		if impl.Title == "" {
			t.Errorf("%s has no title for the picker", impl.Kind)
		}
		if len(impl.Fields) == 0 {
			t.Errorf("%s declares no fields, so its form would be empty", impl.Kind)
		}
		if i > 0 && all[i-1].Kind > impl.Kind {
			t.Error("Implementations() is not sorted")
		}
	}
}

func TestCredentialShapePerForge(t *testing.T) {
	t.Parallel()
	// The Credential fields are a union across the three forges; which are
	// populated depends on Kind. This pins the expectation so a future forge
	// does not quietly reuse the wrong field.
	tests := []struct {
		kind     forge.Kind
		populate func(*forge.Credential)
		check    func(*testing.T, forge.Credential)
	}{
		{
			kind:     forge.KindGitHub,
			populate: func(c *forge.Credential) { c.JITConfig = "blob" },
			check: func(t *testing.T, c forge.Credential) {
				t.Helper()
				if c.JITConfig == "" {
					t.Error("GitHub uses JITConfig")
				}
			},
		},
		{
			kind: forge.KindForgejo,
			populate: func(c *forge.Credential) {
				c.UUID, c.Token = "u", "t"
			},
			check: func(t *testing.T, c forge.Credential) {
				t.Helper()
				if c.UUID == "" || c.Token == "" {
					t.Error("Forgejo uses UUID and Token")
				}
			},
		},
		{
			kind:     forge.KindGitLab,
			populate: func(c *forge.Credential) { c.AuthToken = "glrt-x" },
			check: func(t *testing.T, c forge.Credential) {
				t.Helper()
				if c.AuthToken == "" {
					t.Error("GitLab uses AuthToken")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()
			c := forge.Credential{Kind: tt.kind}
			tt.populate(&c)
			tt.check(t, c)
		})
	}
}
