package forge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	stub := func(map[string]any) (forge.Forge, error) {
		return nil, errors.New("stub constructor, never called")
	}
	forge.Register("duplicate-kind-test", stub)
	forge.Register("duplicate-kind-test", stub)
}
