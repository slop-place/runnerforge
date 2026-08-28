package metrics_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// scrape renders the registry the way Prometheus would read it.
func scrape(t *testing.T) string {
	t.Helper()
	h := metrics.Handler(metrics.Config{}, nil)
	if h == nil {
		t.Fatal("metrics are off by default; they should be on")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestScrapeParses checks the endpoint produces something Prometheus accepts.
// Nothing else in this package is worth much if the output does not parse.
func TestScrapeParses(t *testing.T) {
	metrics.SetBuildInfo("v1.2.3", "rf-test")
	body := scrape(t)

	problems, err := testutil.GatherAndLint(metrics.Registry)
	if err != nil {
		t.Fatalf("lint the registry: %v", err)
	}
	for _, p := range problems {
		// Prometheus's own linter on our own metric names: a unit missing
		// from a name, a counter not ending in _total, and so on.
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
	for _, want := range []string{
		`runnerforge_build_info{`,
		`version="v1.2.3"`,
		`controller_id="rf-test"`,
		// The runtime and process collectors have to be there: a controller
		// that leaks goroutines is a controller that stops provisioning.
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not contain %q", want)
		}
	}
}

// TestEveryMetricIsDocumented checks each metric carries help text. An
// undocumented metric is one nobody can use six months later.
func TestEveryMetricIsDocumented(t *testing.T) {
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "runnerforge_") {
			continue
		}
		seen++
		if strings.TrimSpace(f.GetHelp()) == "" {
			t.Errorf("%s has no help text", f.GetName())
		}
	}
	if seen == 0 {
		t.Fatal("no runnerforge metrics are registered")
	}
}

// TestHTTPMiddlewareLabelsByRouteNotPath is the guard against the failure that
// makes a metrics endpoint worse than none: a label whose values are unbounded.
func TestHTTPMiddlewareLabelsByRouteNotPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /instances/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := metrics.Middleware(mux)

	for _, id := range []string{"1", "2", "3"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/instances/"+id, nil))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("handler returned %d", rec.Code)
		}
	}

	body := scrape(t)
	if !strings.Contains(body, `route="/instances/{id}"`) {
		t.Error("requests are not labelled by the matched route")
	}
	for _, id := range []string{"/instances/1", "/instances/2"} {
		if strings.Contains(body, `route="`+id+`"`) {
			t.Errorf("the path %q became a label value; ids must not be labels", id)
		}
	}
	if !strings.Contains(body, `code="418"`) {
		t.Error("the status code was not recorded")
	}
}

// TestUnmatchedRoutesShareOneLabel checks a scanner walking random URLs cannot
// create a series per guess.
func TestUnmatchedRoutesShareOneLabel(t *testing.T) {
	h := metrics.Middleware(http.NewServeMux())
	for _, p := range []string{"/wp-admin", "/.env", "/phpmyadmin"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	body := scrape(t)
	if !strings.Contains(body, `route="other"`) {
		t.Error("unmatched requests are not collapsed into one label")
	}
	if strings.Contains(body, `route="/wp-admin"`) {
		t.Error("an unmatched path became a label value")
	}
}

// TestTokenGate checks the endpoint can be closed, and that closing it works.
func TestTokenGate(t *testing.T) {
	h := metrics.Handler(metrics.Config{RequireToken: true}, []string{"s3cret"})
	if h == nil {
		t.Fatal("no handler")
	}
	tests := []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"not a bearer", "s3cret", http.StatusUnauthorized},
		{"right token", "Bearer s3cret", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tt.want {
				t.Errorf("got %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestDisabled checks metrics can be turned off entirely.
func TestDisabled(t *testing.T) {
	off := false
	if h := metrics.Handler(metrics.Config{Enabled: &off}, nil); h != nil {
		t.Error("metrics are disabled but a handler was returned")
	}
}

// TestRoute checks the path can be moved and is always absolute.
func TestRoute(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/metrics"},
		{"/internal/metrics", "/internal/metrics"},
		{"telemetry", "/telemetry"},
	}
	for _, tt := range tests {
		if got := (metrics.Config{Path: tt.in}).Route(); got != tt.want {
			t.Errorf("Route(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCloudWrapperCountsAndTimes checks a driver is measured without knowing it.
func TestCloudWrapperCountsAndTimes(t *testing.T) {
	p := metrics.WrapCloud("test-cloud", &countingCloud{})
	ctx := t.Context()

	if _, err := p.Provision(ctx, cloud.ProvisionRequest{Name: "rf-x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, "gone"); !errors.Is(err, cloud.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := p.Delete(ctx, "boom"); err == nil {
		t.Fatal("want an error")
	}

	body := scrape(t)
	for _, want := range []string{
		`cloud_requests_total{cloud="test-cloud",driver="counting",operation="provision",result="success"} 1`,
		// A machine that is already gone is how the controller learns it is
		// gone. Counting that as a provider failure would make every healthy
		// teardown look like an outage.
		`cloud_requests_total{cloud="test-cloud",driver="counting",operation="get",result="success"} 1`,
		`cloud_requests_total{cloud="test-cloud",driver="counting",operation="delete",result="error"} 1`,
	} {
		if !strings.Contains(body, "runnerforge_"+want) {
			t.Errorf("the scrape does not contain %q", want)
		}
	}
}

// TestWrapperOnlyOffersLogsWhenTheDriverCan is a regression guard. Log capture
// is discovered by type assertion, so a wrapper that always implemented it
// would make every driver claim it and quietly break the one thing that makes
// a failed run diagnosable.
func TestWrapperOnlyOffersLogsWhenTheDriverCan(t *testing.T) {
	plain := metrics.WrapCloud("plain", &countingCloud{})
	if _, ok := plain.(cloud.LogProvider); ok {
		t.Error("a driver with no log support was wrapped into claiming it has some")
	}
	withLogs := metrics.WrapCloud("chatty", &loggingCloud{})
	lp, ok := withLogs.(cloud.LogProvider)
	if !ok {
		t.Fatal("a driver that can fetch logs lost the ability when wrapped")
	}
	out, err := lp.Logs(t.Context(), "x", 10)
	if err != nil || out != "hello" {
		t.Errorf("Logs() = %q, %v", out, err)
	}
	if !strings.Contains(scrape(t), `operation="logs"`) {
		t.Error("log fetches are not timed")
	}
}

// TestStateCollectorReportsEveryPoolAndState checks a quiet pool still reports
// zeroes. A series that vanishes when a pool goes idle cannot be alerted on.
func TestStateCollectorReportsEveryPoolAndState(t *testing.T) {
	db := newDB(t)
	metrics.NewStateCollector(db, slog.New(slog.DiscardHandler))

	body := scrape(t)
	for _, st := range store.InstanceStates() {
		want := `runnerforge_instances{cloud="c1",pool="p1",state="` + string(st) + `"}`
		if !strings.Contains(body, want) {
			t.Errorf("no series for state %q", st)
		}
	}
	for _, want := range []string{
		`runnerforge_pools{cloud="c1",enabled="true",forge="f1",pool="p1"} 1`,
		`runnerforge_pool_max_instances{pool="p1"} 4`,
		`runnerforge_clouds{driver="fake",enabled="true",status="unchecked"} 1`,
		`runnerforge_cloud_sizes{cloud="c1"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not contain %q", want)
		}
	}
}

// TestStateCollectorAttributesMachinesToOneCloud is a regression test.
//
// The cloud label used to be read off the instance's own pool, which does not
// carry its cloud. Every running machine was reported twice: once under an
// empty cloud label with the real count, and once under the right cloud with a
// zero. Both series looked plausible and neither was correct.
func TestStateCollectorAttributesMachinesToOneCloud(t *testing.T) {
	db := newDB(t)
	metrics.NewStateCollector(db, slog.New(slog.DiscardHandler))

	pools, err := db.Pools(t.Context())
	if err != nil || len(pools) != 1 {
		t.Fatalf("pools: %v, %v", pools, err)
	}
	in := &store.Instance{
		Name: "rf-p1-abc", PoolID: pools[0].ID, State: store.StateBusy, HourlyUSD: 0.5,
	}
	if err := db.WithContext(t.Context()).Create(in).Error; err != nil {
		t.Fatal(err)
	}

	body := scrape(t)
	if !strings.Contains(body, `runnerforge_instances{cloud="c1",pool="p1",state="busy"} 1`) {
		t.Error("the running machine is not attributed to its cloud")
	}
	if strings.Contains(body, `runnerforge_instances{cloud="",pool="p1"`) {
		t.Error("a machine was reported under an empty cloud label")
	}
	if !strings.Contains(body, `runnerforge_burn_rate_usd_per_hour{pool="p1"} 0.5`) {
		t.Error("the burn rate does not reflect the running machine")
	}
}

// newDB builds a store with one cloud, forge and pool configured.
func newDB(t *testing.T) *store.DB {
	t.Helper()
	if err := store.SetKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite", t.TempDir()+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	cl := &store.Cloud{Name: "c1", Driver: "fake", Enabled: true}
	if err := db.WithContext(ctx).Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	sz := &store.Size{CloudID: cl.ID, Name: "small", HourlyUSD: 0.5}
	if err := db.WithContext(ctx).Create(sz).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{Name: "f1", Kind: "fake", Enabled: true}
	if err := db.WithContext(ctx).Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "p1", CloudID: cl.ID, ForgeID: fg.ID, SizeID: sz.ID,
		Labels: store.StringList{"x"}, MaxInstances: 4, Enabled: true,
	}
	if err := db.WithContext(ctx).Create(pool).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// countingCloud is a provider that answers without doing anything.
type countingCloud struct{}

func (*countingCloud) Name() string                     { return "counting" }
func (*countingCloud) Capabilities() cloud.Capabilities { return cloud.Capabilities{} }

func (*countingCloud) Provision(context.Context, cloud.ProvisionRequest) (*cloud.Instance, error) {
	return &cloud.Instance{ID: "i-1"}, nil
}
func (*countingCloud) Get(context.Context, string) (*cloud.Instance, error) {
	return nil, cloud.ErrNotFound
}
func (*countingCloud) Delete(context.Context, string) error { return io.ErrUnexpectedEOF }
func (*countingCloud) List(context.Context, cloud.Owner) ([]*cloud.Instance, error) {
	return nil, nil
}

// loggingCloud can also fetch a machine's output.
type loggingCloud struct{ countingCloud }

func (*loggingCloud) Logs(context.Context, string, int) (string, error) { return "hello", nil }
