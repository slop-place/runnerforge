package dockerdrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/slop-place/runnerforge/internal/cloud"
)

// stubEngine is a fake Docker Engine API.
//
// The driver's real transport dials a unix socket, which makes it awkward to
// point at a test server; building the Driver directly instead exercises every
// request it makes without needing a daemon. That covers the half of this
// package that is HTTP rather than logic.
type stubEngine struct {
	*httptest.Server

	mu sync.Mutex
	// requests records every path the driver asked for, so tests can assert on
	// the shape of the calls and not just their results.
	requests []string

	containers map[string]*stubContainer
	images     map[string]bool
	pulls      []string
	nextID     int

	// failCreate, failStart and failInspect make the corresponding call fail.
	failCreate  bool
	failStart   bool
	inspectCode int
}

type stubContainer struct {
	ID      string
	Name    string
	Labels  map[string]string
	State   string
	Exit    int
	Logs    []byte
	Created int64
	Env     []string
	Cmd     []string
	Entry   []string
	Host    hostConfig
}

func newStubEngine(t *testing.T) *stubEngine {
	t.Helper()
	s := &stubEngine{
		containers: map[string]*stubContainer{},
		images:     map[string]bool{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// driver builds a Driver pointed at this stub.
func (s *stubEngine) driver() *Driver {
	return &Driver{host: s.URL, http: s.Client()}
}

func (s *stubEngine) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		decoded, _ := url.PathUnescape(name)
		s.mu.Lock()
		_, ok := s.images[decoded]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))

	case r.Method == http.MethodPost && r.URL.Path == "/images/create":
		img := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
		s.mu.Lock()
		s.pulls = append(s.pulls, img)
		s.images[img] = true
		s.mu.Unlock()
		// A real pull streams newline-delimited progress objects.
		_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Complete"}` + "\n"))

	case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
		if s.failCreate {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"no such image"}`))
			return
		}
		var body createBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.nextID++
		id := "c" + string(rune('0'+s.nextID))
		s.containers[id] = &stubContainer{
			ID: id, Name: r.URL.Query().Get("name"), Labels: body.Labels,
			State: "created", Env: body.Env, Cmd: body.Cmd, Entry: body.Entrypoint,
			Host: body.HostConfig, Created: 1700000000,
		}
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"Id":"` + id + `","Warnings":[]}`))

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		if s.failStart {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"port already allocated"}`))
			return
		}
		id := containerID(r.URL.Path)
		s.mu.Lock()
		if c, ok := s.containers[id]; ok {
			c.State = "running"
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	// This must come before the inspect case: /containers/json is the list
	// endpoint, not an inspect of a container named "json".
	case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []map[string]any{}
		wanted := parseLabelFilter(r.URL.Query().Get("filters"))
		for _, c := range s.containers {
			if !matchesLabels(c.Labels, wanted) {
				continue
			}
			out = append(out, map[string]any{
				"Id": c.ID, "Names": []string{"/" + c.Name}, "Labels": c.Labels,
				"State": c.State, "Created": c.Created,
			})
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") &&
		strings.HasPrefix(r.URL.Path, "/containers/"):
		if s.inspectCode != 0 {
			w.WriteHeader(s.inspectCode)
			return
		}
		id := containerID(r.URL.Path)
		s.mu.Lock()
		c, ok := s.containers[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": c.ID, "Name": "/" + c.Name,
			"Config": map[string]any{"Labels": c.Labels},
			"State": map[string]any{
				"Status": c.State, "Running": c.State == "running",
				"ExitCode": c.Exit, "Error": "",
			},
			"Created":         "2023-11-14T22:13:20Z",
			"NetworkSettings": map[string]any{"IPAddress": "172.17.0.2"},
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
		id := containerID(r.URL.Path)
		s.mu.Lock()
		_, ok := s.containers[id]
		delete(s.containers, id)
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
		id := containerID(r.URL.Path)
		s.mu.Lock()
		c, ok := s.containers[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(c.Logs)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// containerID pulls the container id out of a /containers/{id}/... path.
func containerID(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func parseLabelFilter(raw string) []string {
	if raw == "" {
		return nil
	}
	var f map[string][]string
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil
	}
	return f["label"]
}

func matchesLabels(have map[string]string, wanted []string) bool {
	for _, w := range wanted {
		k, v, _ := strings.Cut(w, "=")
		if have[k] != v {
			return false
		}
	}
	return true
}

func (s *stubEngine) sawRequest(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

// ---- tests ----

func TestProvisionCreatesAndStarts(t *testing.T) {
	eng := newStubEngine(t)
	eng.images["runner:12"] = true
	d := eng.driver()

	inst, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:  "rf-pool-abc",
		Owner: cloud.Owner{Controller: "rf-1", Pool: "pool"},
		Tags:  map[string]string{"extra": "value"},
		Bootstrap: cloud.Bootstrap{
			ContainerImage: "runner:12",
			Entrypoint:     []string{"/bin/sh", "-c"},
			Args:           []string{"echo hi"},
			Env:            map[string]string{"TOKEN": "secret"},
		},
		SizeSpec: map[string]any{"cpus": 2.0, "memory_mb": 1024},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if inst.Name != "rf-pool-abc" || inst.ID == "" {
		t.Fatalf("instance = %+v", inst)
	}

	eng.mu.Lock()
	c := eng.containers[inst.ID]
	eng.mu.Unlock()

	if c.State != "running" {
		t.Errorf("container was created but not started: %q", c.State)
	}
	// Ownership tags are what the reaper claims machines by, so they must be
	// on the container from the moment it exists.
	if c.Labels[cloud.TagController] != "rf-1" || c.Labels[cloud.TagPool] != "pool" {
		t.Errorf("ownership labels = %v", c.Labels)
	}
	// Caller tags are kept, but owner keys always win.
	if c.Labels["extra"] != "value" {
		t.Errorf("caller tags were dropped: %v", c.Labels)
	}
	if c.Labels[cloud.TagCreatedAt] == "" {
		t.Error("no creation timestamp label")
	}
	if len(c.Env) != 1 || c.Env[0] != "TOKEN=secret" {
		t.Errorf("env = %v", c.Env)
	}
	if c.Host.NanoCPUs != 2*nanoCPUsPerCore || c.Host.Memory != 1024*bytesPerMB {
		t.Errorf("size limits were not applied: %+v", c.Host)
	}
	// AutoRemove must stay off, or List would stop reporting exited containers
	// and the reaper would go blind exactly when a job has just failed.
	if c.Host.AutoRemove {
		t.Error("AutoRemove is on; the reaper would lose sight of exited containers")
	}
}

func TestProvisionOwnerTagsBeatCallerTags(t *testing.T) {
	eng := newStubEngine(t)
	eng.images["img:latest"] = true
	d := eng.driver()

	inst, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:  "rf-a",
		Owner: cloud.Owner{Controller: "real", Pool: "real-pool"},
		// A caller trying to spoof ownership must not win.
		Tags:      map[string]string{cloud.TagController: "spoofed"},
		Bootstrap: cloud.Bootstrap{ContainerImage: "img:latest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	got := eng.containers[inst.ID].Labels[cloud.TagController]
	eng.mu.Unlock()
	if got != "real" {
		t.Errorf("controller label = %q, want the owner's value", got)
	}
}

func TestProvisionPullsMissingImage(t *testing.T) {
	eng := newStubEngine(t)
	d := eng.driver()

	_, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		Bootstrap: cloud.Bootstrap{ContainerImage: "ghcr.io/org/runner:12"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	eng.mu.Lock()
	pulls := append([]string(nil), eng.pulls...)
	eng.mu.Unlock()
	if len(pulls) != 1 || pulls[0] != "ghcr.io/org/runner:12" {
		t.Errorf("pulls = %v, want the registry path and tag split correctly", pulls)
	}
}

func TestProvisionRollsBackWhenStartFails(t *testing.T) {
	eng := newStubEngine(t)
	eng.images["img:latest"] = true
	eng.failStart = true
	d := eng.driver()

	_, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		Bootstrap: cloud.Bootstrap{ContainerImage: "img:latest"},
	})
	if err == nil {
		t.Fatal("expected an error when start fails")
	}
	// A container that was created but never started still costs a name and
	// still needs destroying, so the driver must clean up after itself.
	eng.mu.Lock()
	left := len(eng.containers)
	eng.mu.Unlock()
	if left != 0 {
		t.Errorf("%d container(s) left behind after a failed start", left)
	}
	if !eng.sawRequest("DELETE /containers/") {
		t.Error("no rollback delete was issued")
	}
}

func TestProvisionReportsCreateFailure(t *testing.T) {
	eng := newStubEngine(t)
	eng.images["img:latest"] = true
	eng.failCreate = true

	_, err := eng.driver().Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		Bootstrap: cloud.Bootstrap{ContainerImage: "img:latest"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	// The daemon's own explanation is what an operator needs.
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("error %q should carry the daemon's message", err)
	}
}

func TestGetMapsState(t *testing.T) {
	eng := newStubEngine(t)
	d := eng.driver()

	tests := []struct {
		state string
		exit  int
		want  cloud.State
	}{
		{state: "running", want: cloud.StateRunning},
		{state: "created", want: cloud.StateCreating},
		{state: "exited", exit: 0, want: cloud.StateStopped},
		{state: "exited", exit: 1, want: cloud.StateError},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			eng.mu.Lock()
			eng.containers["x"] = &stubContainer{ID: "x", Name: "rf-x", State: tt.state, Exit: tt.exit}
			eng.mu.Unlock()

			got, err := d.Get(context.Background(), "x")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tt.want {
				t.Errorf("state %q exit %d mapped to %q, want %q", tt.state, tt.exit, got.State, tt.want)
			}
			// A non-zero exit is the signal that a job failed, so the reason
			// has to survive into the instance.
			if tt.want == cloud.StateError && got.Err == "" {
				t.Error("an errored container should carry an explanation")
			}
			if got.PrivateIP != "172.17.0.2" {
				t.Errorf("PrivateIP = %q", got.PrivateIP)
			}
		})
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	eng := newStubEngine(t)
	_, err := eng.driver().Get(context.Background(), "nope")
	if !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	eng := newStubEngine(t)
	eng.mu.Lock()
	eng.containers["x"] = &stubContainer{ID: "x", Name: "rf-x", State: "running"}
	eng.mu.Unlock()
	d := eng.driver()
	ctx := context.Background()

	if err := d.Delete(ctx, "x"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Teardown is retried, so deleting an already-gone container must be
	// success rather than an error that wedges the loop.
	if err := d.Delete(ctx, "x"); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

func TestListFiltersByOwner(t *testing.T) {
	eng := newStubEngine(t)
	mine := map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "p"}
	other := map[string]string{cloud.TagController: "rf-2", cloud.TagPool: "p"}
	eng.mu.Lock()
	eng.containers["a"] = &stubContainer{ID: "a", Name: "rf-a", Labels: mine, State: "running"}
	eng.containers["b"] = &stubContainer{ID: "b", Name: "rf-b", Labels: mine, State: "exited"}
	eng.containers["c"] = &stubContainer{ID: "c", Name: "rf-c", Labels: other, State: "running"}
	eng.mu.Unlock()

	got, err := eng.driver().List(context.Background(), cloud.Owner{Controller: "rf-1"})
	if err != nil {
		t.Fatal(err)
	}
	// Exited containers must appear: one that has stopped still holds a name
	// and still needs to be destroyed.
	if len(got) != 2 {
		t.Fatalf("listed %d containers, want both of ours including the exited one", len(got))
	}
	for _, inst := range got {
		if inst.Tags[cloud.TagController] != "rf-1" {
			t.Errorf("listed a container belonging to %q", inst.Tags[cloud.TagController])
		}
		if !strings.HasPrefix(inst.Name, "rf-") {
			t.Errorf("name %q lost its leading slash trim", inst.Name)
		}
	}
	// all=1 is what makes exited containers visible.
	if !eng.sawRequest("GET /containers/json") {
		t.Error("no list request was made")
	}
}

func TestListNarrowsToOnePool(t *testing.T) {
	eng := newStubEngine(t)
	eng.mu.Lock()
	eng.containers["a"] = &stubContainer{
		ID: "a", Name: "rf-a", State: "running",
		Labels: map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "small"},
	}
	eng.containers["b"] = &stubContainer{
		ID: "b", Name: "rf-b", State: "running",
		Labels: map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "large"},
	}
	eng.mu.Unlock()

	got, err := eng.driver().List(context.Background(), cloud.Owner{Controller: "rf-1", Pool: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("listed %+v, want only the small pool", got)
	}
}

func TestLogsDeframesAndReportsMissing(t *testing.T) {
	eng := newStubEngine(t)
	eng.mu.Lock()
	eng.containers["x"] = &stubContainer{
		ID: "x", Name: "rf-x", State: "exited",
		Logs: append(frame(1, "line one\n"), frame(2, "line two\n")...),
	}
	eng.mu.Unlock()
	d := eng.driver()
	ctx := context.Background()

	got, err := d.Logs(ctx, "x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != "line one\nline two\n" {
		t.Errorf("logs = %q", got)
	}

	if _, err := d.Logs(ctx, "missing", 10); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestLogsDefaultsTail(t *testing.T) {
	eng := newStubEngine(t)
	eng.mu.Lock()
	eng.containers["x"] = &stubContainer{ID: "x", Name: "rf-x", Logs: frame(1, "hi")}
	eng.mu.Unlock()
	if _, err := eng.driver().Logs(context.Background(), "x", 0); err != nil {
		t.Fatal(err)
	}
}

func TestDoSurfacesServerErrors(t *testing.T) {
	eng := newStubEngine(t)
	eng.inspectCode = http.StatusInternalServerError
	_, err := eng.driver().Get(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, cloud.ErrNotFound) {
		t.Error("a 500 must not be mistaken for a missing container")
	}
}

func TestDoHandlesUnreachableDaemon(t *testing.T) {
	d := &Driver{host: "http://127.0.0.1:1", http: http.DefaultClient}
	if _, err := d.Get(context.Background(), "x"); err == nil {
		t.Error("expected a transport error when the daemon is unreachable")
	}
}
