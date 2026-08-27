package web

import (
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/store"
)

func TestInternalRedirectRejectsForeignDestinations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		path  string
		key   string
		value string
		want  string
	}{
		{name: "plain path", path: "/pools", want: "/pools"},
		{name: "path with message", path: "/pools", key: "ok", value: "saved", want: "/pools?ok=saved"},
		{
			// The message is operator-supplied error text; it must be escaped
			// rather than able to add parameters of its own.
			name: "message with query characters",
			path: "/clouds", key: "err", value: "bad & worse?",
			want: "/clouds?err=bad+%26+worse%3F",
		},
		{
			// An absolute URL would send the browser off-site.
			name: "absolute url is refused",
			path: "https://evil.example.com/steal",
			want: "/",
		},
		{
			// A protocol-relative URL is the classic open-redirect payload.
			name: "protocol-relative url is refused",
			path: "//evil.example.com",
			want: "/",
		},
		{name: "relative path is refused", path: "pools", want: "/"},
		{name: "empty is refused", path: "", want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := internalRedirect(tt.path, tt.key, tt.value); got != tt.want {
				t.Errorf("internalRedirect(%q, %q, %q) = %q, want %q",
					tt.path, tt.key, tt.value, got, tt.want)
			}
		})
	}
}

func TestParseParams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
		check   func(*testing.T, store.Params)
	}{
		{name: "empty is an empty object", in: "", check: func(t *testing.T, p store.Params) {
			t.Helper()
			if len(p) != 0 {
				t.Errorf("want empty, got %v", p)
			}
		}},
		{name: "object", in: `{"flavor":"c3-8"}`, check: func(t *testing.T, p store.Params) {
			t.Helper()
			if p.String("flavor") != "c3-8" {
				t.Errorf("got %v", p)
			}
		}},
		{name: "whitespace only", in: "   \n ", check: func(t *testing.T, p store.Params) {
			t.Helper()
			if len(p) != 0 {
				t.Errorf("want empty, got %v", p)
			}
		}},
		{name: "malformed", in: "{not json", wantErr: true},
		{name: "array instead of object", in: `["a"]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseParams(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestParseSecret(t *testing.T) {
	t.Parallel()
	got, err := parseSecret(`{"token":"abc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got["token"] != "abc" {
		t.Errorf("got %v", got)
	}
	if _, err := parseSecret(`{"token":123}`); err == nil {
		t.Error("a non-string value should be rejected")
	}
	empty, err := parseSecret("")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input should give an empty secret, got %v %v", empty, err)
	}
}

func TestSplitLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{in: "a,b,c", want: []string{"a", "b", "c"}},
		{in: " a , b ", want: []string{"a", "b"}},
		{in: "a,,b", want: []string{"a", "b"}},
		{in: "", want: nil},
		{in: " , , ", want: nil},
		{in: "single", want: []string{"single"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got := splitLabels(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("splitLabels(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidatePool(t *testing.T) {
	t.Parallel()
	valid := func() *store.Pool {
		return &store.Pool{
			Name: "p", ForgeID: 1, CloudID: 1, SizeID: 1,
			Labels:       store.StringList{"linux"},
			MaxInstances: 5, MinIdle: 0,
			JobTimeoutSec: 600, MaxLifetimeSec: 1200,
		}
	}

	if err := validatePool(valid()); err != nil {
		t.Fatalf("a valid pool was rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*store.Pool)
		wantErr string
	}{
		{name: "no name", mutate: func(p *store.Pool) { p.Name = "" }, wantErr: "name is required"},
		{name: "no forge", mutate: func(p *store.Pool) { p.ForgeID = 0 }, wantErr: "forge is required"},
		{name: "no cloud", mutate: func(p *store.Pool) { p.CloudID = 0 }, wantErr: "cloud is required"},
		{name: "no size", mutate: func(p *store.Pool) { p.SizeID = 0 }, wantErr: "size is required"},
		{
			// A pool with no labels can never be selected by a job.
			name:    "no labels",
			mutate:  func(p *store.Pool) { p.Labels = nil },
			wantErr: "at least one label",
		},
		{
			name:    "zero max instances",
			mutate:  func(p *store.Pool) { p.MaxInstances = 0 },
			wantErr: "at least 1",
		},
		{
			name:    "min idle above max",
			mutate:  func(p *store.Pool) { p.MinIdle = 10 },
			wantErr: "cannot exceed",
		},
		{
			// The check that stops the reaper destroying machines mid-job.
			name:    "lifetime below job timeout",
			mutate:  func(p *store.Pool) { p.MaxLifetimeSec = 300 },
			wantErr: "must exceed the job timeout",
		},
		{
			name:    "lifetime equal to job timeout",
			mutate:  func(p *store.Pool) { p.MaxLifetimeSec = p.JobTimeoutSec },
			wantErr: "must exceed the job timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := valid()
			tt.mutate(p)
			err := validatePool(p)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestTemplatesParse(t *testing.T) {
	t.Parallel()
	// mustParse panics on a malformed template, so a broken page is caught here
	// rather than as a blank screen at runtime.
	tpl := mustParse()
	for _, p := range pages {
		if _, ok := tpl[p]; !ok {
			t.Errorf("page %q did not parse", p)
		}
	}
	if _, ok := tpl["_partials"]; !ok {
		t.Error("partials did not parse")
	}
}

func TestScopeHelper(t *testing.T) {
	t.Parallel()
	fn, ok := funcs["scope"].(func(store.Params) string)
	if !ok {
		t.Fatal("scope helper has an unexpected signature")
	}
	tests := []struct {
		name string
		in   store.Params
		want string
	}{
		{name: "repo", in: store.Params{"owner": "o", "repo": "r"}, want: "o/r"},
		{name: "org", in: store.Params{"owner": "o"}, want: "o"},
		{name: "gitlab project", in: store.Params{"project_id": "42"}, want: "project 42"},
		{name: "gitlab group", in: store.Params{"group_id": "9"}, want: "group 9"},
		{name: "fallback", in: store.Params{"scope": "instance"}, want: "instance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fn(tt.in); got != tt.want {
				t.Errorf("scope(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
