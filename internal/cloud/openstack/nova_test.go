package openstack

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/slop-place/runnerforge/internal/cloud"
)

// stubCloud is a fake Nova and Neutron.
//
// Building the ServiceClients directly skips the Keystone handshake, which is
// not what these tests are about, and exercises every request the driver makes
// against the compute and network APIs.
type stubCloud struct {
	*httptest.Server

	mu       sync.Mutex
	servers  map[string]map[string]any
	networks []map[string]any
	requests []string
	console  string

	createStatus int
	listStatus   int
}

func newStubCloud(t *testing.T) *stubCloud {
	t.Helper()
	s := &stubCloud{
		servers: map[string]map[string]any{},
		networks: []map[string]any{
			{"id": "3f2b1a44-9c7e-4d61-b8a3-0e5f6d7c8b9a", "name": "Ext-Net"},
		},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *stubCloud) driver() *Driver {
	client := func() *gophercloud.ServiceClient {
		return &gophercloud.ServiceClient{
			ProviderClient: &gophercloud.ProviderClient{TokenID: "stub-token"},
			Endpoint:       s.URL + "/",
		}
	}
	return &Driver{
		compute: client(), network: client(),
		region: "TEST-1", defaultNetwork: "Ext-Net",
		netIDs: map[string]string{},
	}
}

func (s *stubCloud) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		if s.createStatus != 0 {
			w.WriteHeader(s.createStatus)
			_, _ = w.Write([]byte(`{"badRequest":{"message":"invalid flavor"}}`))
			return
		}
		var body struct {
			Server map[string]any `json:"server"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := "srv-1"
		meta, _ := body.Server["metadata"].(map[string]any)
		s.mu.Lock()
		s.servers[id] = map[string]any{
			"id": id, "name": body.Server["name"], "status": "BUILD",
			"metadata": meta, "addresses": map[string]any{},
			// Nova's create response famously omits "created"; the driver has
			// to stamp its own or the reaper reads it as infinitely old.
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"server": s.servers[id]})

	case r.Method == http.MethodGet && r.URL.Path == "/servers/detail":
		if s.listStatus != 0 {
			w.WriteHeader(s.listStatus)
			return
		}
		s.mu.Lock()
		out := make([]map[string]any, 0, len(s.servers))
		for _, srv := range s.servers {
			out = append(out, srv)
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": out})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/servers/")
		s.mu.Lock()
		srv, ok := s.servers[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"server": srv})

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/servers/")
		s.mu.Lock()
		_, ok := s.servers[id]
		delete(s.servers, id)
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/action"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/servers/"), "/action")
		s.mu.Lock()
		_, ok := s.servers[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"output": s.console})

	case r.Method == http.MethodGet && r.URL.Path == "/networks":
		name := r.URL.Query().Get("name")
		s.mu.Lock()
		out := []map[string]any{}
		for _, n := range s.networks {
			if name == "" || n["name"] == name {
				out = append(out, n)
			}
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"networks": out})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *stubCloud) setServer(id string, fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv := map[string]any{"id": id, "name": "rf-" + id, "status": "ACTIVE"}
	maps.Copy(srv, fields)
	s.servers[id] = srv
}

func (s *stubCloud) sawRequest(substr string) bool {
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

func TestProvisionStampsCreationTime(t *testing.T) {
	stub := newStubCloud(t)
	d := stub.driver()

	inst, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		Owner:     cloud.Owner{Controller: "rf-1", Pool: "p"},
		SizeSpec:  map[string]any{"flavor": "flavor-id"},
		ImageSpec: map[string]any{"id": "image-id"},
		Bootstrap: cloud.Bootstrap{CloudInit: []byte("#cloud-config\n")},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// This is the regression that matters most in this package. Nova's create
	// response carries no creation time, and the reaper ages machines off by
	// that field — a zero timestamp reads as infinitely old and would destroy
	// the whole fleet mid-job.
	if inst.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero; the reaper would treat this machine as infinitely old")
	}
	// Ownership metadata must be on the server from the moment it exists.
	if !(cloud.Owner{Controller: "rf-1", Pool: "p"}).Matches(inst.Tags) {
		t.Errorf("ownership metadata = %v", inst.Tags)
	}
}

func TestProvisionResolvesNetworkNameToUUID(t *testing.T) {
	stub := newStubCloud(t)
	d := stub.driver()
	ctx := context.Background()

	req := cloud.ProvisionRequest{
		Name:      "rf-a",
		SizeSpec:  map[string]any{"flavor": "f"},
		ImageSpec: map[string]any{"id": "i"},
	}
	if _, err := d.Provision(ctx, req); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Nova only accepts a uuid, so a configured name has to be looked up.
	if !stub.sawRequest("GET /networks") {
		t.Error("no Neutron lookup was made for the network name")
	}

	// The result is cached, so a second provision does not look it up again.
	before := len(stub.requests)
	if _, err := d.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	stub.mu.Lock()
	for _, r := range stub.requests[before:] {
		if strings.Contains(r, "/networks") {
			lookups++
		}
	}
	stub.mu.Unlock()
	if lookups != 0 {
		t.Errorf("the network name was looked up again despite the cache (%d times)", lookups)
	}
}

func TestProvisionAcceptsNetworkUUIDDirectly(t *testing.T) {
	stub := newStubCloud(t)
	d := stub.driver()

	_, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		SizeSpec:  map[string]any{"flavor": "f", "network": "3f2b1a44-9c7e-4d61-b8a3-0e5f6d7c8b9a"},
		ImageSpec: map[string]any{"id": "i"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A value that already looks like a uuid needs no lookup.
	if stub.sawRequest("GET /networks") {
		t.Error("a uuid should not trigger a Neutron lookup")
	}
}

func TestProvisionUnknownNetworkIsReported(t *testing.T) {
	stub := newStubCloud(t)
	d := stub.driver()
	d.defaultNetwork = "does-not-exist"

	_, err := d.Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		SizeSpec:  map[string]any{"flavor": "f"},
		ImageSpec: map[string]any{"id": "i"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown network")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q should name the missing network", err)
	}
}

func TestProvisionReportsAPIFailure(t *testing.T) {
	stub := newStubCloud(t)
	stub.createStatus = http.StatusBadRequest
	_, err := stub.driver().Provision(context.Background(), cloud.ProvisionRequest{
		Name:      "rf-a",
		SizeSpec:  map[string]any{"flavor": "bad"},
		ImageSpec: map[string]any{"id": "i"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetAndDelete(t *testing.T) {
	stub := newStubCloud(t)
	stub.setServer("srv-x", map[string]any{
		"status": "ACTIVE",
		"addresses": map[string]any{"Ext-Net": []any{
			map[string]any{"addr": "15.204.216.3", "OS-EXT-IPS:type": "fixed"},
		}},
	})
	d := stub.driver()
	ctx := context.Background()

	got, err := d.Get(ctx, "srv-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != cloud.StateRunning {
		t.Errorf("state = %q", got.State)
	}
	if got.PublicIP != "15.204.216.3" {
		t.Errorf("PublicIP = %q", got.PublicIP)
	}

	if err := d.Delete(ctx, "srv-x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Teardown is retried, so a second delete must be success.
	if err := d.Delete(ctx, "srv-x"); err != nil {
		t.Errorf("deleting an already-gone server should succeed, got %v", err)
	}
	if _, err := d.Get(ctx, "srv-x"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestListFiltersByOwnerMetadata(t *testing.T) {
	stub := newStubCloud(t)
	stub.setServer("mine", map[string]any{
		"metadata": map[string]any{cloud.TagController: "rf-1", cloud.TagPool: "p"},
	})
	stub.setServer("theirs", map[string]any{
		"metadata": map[string]any{cloud.TagController: "rf-2", cloud.TagPool: "p"},
	})
	stub.setServer("untagged", map[string]any{"metadata": map[string]any{}})

	got, err := stub.driver().List(context.Background(), cloud.Owner{Controller: "rf-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mine" {
		t.Fatalf("listed %d servers, want only ours: %+v", len(got), got)
	}
}

func TestListReportsFailureRatherThanEmptiness(t *testing.T) {
	stub := newStubCloud(t)
	stub.listStatus = http.StatusInternalServerError
	// An unreachable cloud must not look like a cloud containing nothing, or
	// the reaper would conclude every machine had already gone.
	if _, err := stub.driver().List(context.Background(), cloud.Owner{Controller: "rf-1"}); err == nil {
		t.Fatal("expected an error when the API fails")
	}
}

func TestLogsReturnsConsoleOutput(t *testing.T) {
	stub := newStubCloud(t)
	stub.console = "cloud-init: failed to fetch runner\n"
	stub.setServer("srv-x", nil)
	d := stub.driver()
	ctx := context.Background()

	got, err := d.Logs(ctx, "srv-x", 50)
	if err != nil {
		t.Fatal(err)
	}
	// This is what makes a machine that never registered explain itself.
	if !strings.Contains(got, "failed to fetch runner") {
		t.Errorf("console output = %q", got)
	}

	if _, err := d.Logs(ctx, "missing", 50); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	// A zero tail falls back to a default rather than asking for nothing.
	if _, err := d.Logs(ctx, "srv-x", 0); err != nil {
		t.Errorf("default tail: %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()
	if isNotFound(errors.New("something else")) {
		t.Error("a plain error is not a 404")
	}
	if isNotFound(nil) {
		t.Error("nil is not a 404")
	}
	err := gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}
	if !isNotFound(err) {
		t.Error("a 404 response code should be recognised")
	}
	err = gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusInternalServerError}
	if isNotFound(err) {
		t.Error("a 500 must not be mistaken for a 404")
	}
}
