package web

import (
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	// Drivers and forges register themselves on import; without these the
	// handlers would reject every cloud and forge as an unknown kind.
	_ "github.com/slop-place/runnerforge/internal/cloud/dockerdrv"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	_ "github.com/slop-place/runnerforge/internal/forge/forgejo"
	_ "github.com/slop-place/runnerforge/internal/forge/github"
	"github.com/slop-place/runnerforge/internal/store"
)

// newServer builds the real router over a real database, so these exercise the
// handlers end to end rather than in isolation.
func newServer(t *testing.T) (*store.DB, http.Handler) {
	t.Helper()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKey(key); err != nil {
		t.Fatal(err)
	}
	k, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ID: "rf-web-test", SecretKey: k, BaseURL: "https://rf.example.com",
		Database: config.Database{Driver: "sqlite", DSN: t.TempDir() + "/w.db"},
	}
	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	return db, New(db, controller.New(db, cfg, log), cfg, log).Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPagesRender(t *testing.T) {
	_, h := newServer(t)
	for _, path := range []string{
		"/", "/clouds", "/forges", "/pools", "/instances", "/events",
		"/partials/pools", "/partials/instances", "/partials/events",
		"/healthz",
	} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, body: %s", path, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	_, h := newServer(t)
	for _, path := range []string{"/static/htmx.min.js", "/static/app.css"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

func TestCreateCloudAndCatalogue(t *testing.T) {
	db, h := newServer(t)

	rec := post(t, h, "/clouds", url.Values{
		"name":        {"ovh-test"},
		"driver":      {"docker"},
		"settings":    {`{"region":"US-EAST-VA-1"}`},
		"credentials": {`{"password":"hunter2"}`},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create cloud = %d, body: %s", rec.Code, rec.Body.String())
	}

	clouds, err := db.Clouds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(clouds) != 1 {
		t.Fatalf("expected 1 cloud, got %d", len(clouds))
	}
	cl := clouds[0]
	if cl.Settings.String("region") != "US-EAST-VA-1" {
		t.Errorf("settings = %v", cl.Settings)
	}
	if cl.Credentials["password"] != "hunter2" {
		t.Error("credentials did not round-trip through encryption")
	}
	if !cl.Enabled {
		t.Error("a newly created cloud should be enabled")
	}

	// The edit page must not render the secret back to the browser.
	rec = get(t, h, "/clouds/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("edit page = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("the edit page leaked a stored credential")
	}

	t.Run("add a size and an image", func(t *testing.T) {
		rec := post(t, h, "/clouds/1/sizes", url.Values{
			"name": {"small"}, "spec": {`{"cpus":2}`},
			"vcpus": {"2"}, "memory_mb": {"2048"}, "hourly_usd": {"0.02"},
		})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("add size = %d, body: %s", rec.Code, rec.Body.String())
		}
		rec = post(t, h, "/clouds/1/images", url.Values{
			"name": {"ci-base"}, "spec": {`{"image":"alpine"}`}, "username": {"debian"},
		})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("add image = %d", rec.Code)
		}

		cl, err := db.CloudByID(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(cl.Sizes) != 1 || cl.Sizes[0].Name != "small" {
			t.Errorf("sizes = %+v", cl.Sizes)
		}
		if len(cl.Images) != 1 || cl.Images[0].Username != "debian" {
			t.Errorf("images = %+v", cl.Images)
		}
	})
}

func TestCreateCloudRejectsBadInput(t *testing.T) {
	_, h := newServer(t)

	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "unknown driver",
			form: url.Values{"name": {"x"}, "driver": {"not-a-driver"}},
			want: "unknown+driver",
		},
		{
			name: "malformed settings",
			form: url.Values{"name": {"x"}, "driver": {"docker"}, "settings": {"{oops"}},
			want: "settings",
		},
		{
			name: "malformed credentials",
			form: url.Values{"name": {"x"}, "driver": {"docker"}, "credentials": {"[1,2]"}},
			want: "credentials",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, h, "/clouds", tt.form)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d", rec.Code)
			}
			loc := rec.Header().Get("Location")
			if !strings.Contains(loc, tt.want) {
				t.Errorf("redirect %q should carry an error about %q", loc, tt.want)
			}
			// However bad the input, the redirect stays on this site.
			if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
				t.Errorf("redirect left the site: %q", loc)
			}
		})
	}
}

func TestUpdateCloudKeepsCredentialsWhenBlank(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{
		"name": {"c"}, "driver": {"docker"}, "credentials": {`{"password":"keepme"}`},
	})

	// Editing the region must not require re-entering a secret the operator
	// cannot see.
	rec := post(t, h, "/clouds/1", url.Values{
		"name": {"c"}, "enabled": {"on"},
		"settings": {`{"region":"eu"}`}, "credentials": {""},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update = %d, body: %s", rec.Code, rec.Body.String())
	}
	cl, err := db.CloudByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Credentials["password"] != "keepme" {
		t.Error("blank credentials should preserve the stored ones")
	}
	if cl.Settings.String("region") != "eu" {
		t.Error("the settings update did not apply")
	}
}

func TestDisablingPersists(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})

	// Unchecking the box omits the field entirely, which must read as false.
	rec := post(t, h, "/clouds/1", url.Values{"name": {"c"}, "settings": {"{}"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update = %d", rec.Code)
	}
	cl, err := db.CloudByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Enabled {
		t.Error("a cloud disabled in the UI came back enabled")
	}
}

func TestPoolLifecycle(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/forges", url.Values{
		"name": {"f"}, "kind": {"forgejo"},
		"settings":    {`{"url":"http://f","scope":"repo","owner":"o","repo":"r"}`},
		"credentials": {`{"token":"t"}`},
	})

	rec := post(t, h, "/pools", url.Values{
		"name": {"p"}, "forge_id": {"1"}, "cloud_id": {"1"}, "size_id": {"1"},
		"labels": {"self-hosted, linux"}, "max_instances": {"3"}, "min_idle": {"0"},
		"job_timeout_sec": {"600"}, "max_lifetime_sec": {"1200"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create pool = %d, body: %s", rec.Code, rec.Body.String())
	}
	p, err := db.PoolByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Labels) != 2 || p.Labels[0] != "self-hosted" {
		t.Errorf("labels = %v", p.Labels)
	}

	t.Run("a cloud in use cannot be deleted", func(t *testing.T) {
		// Deleting it would strand any machine running on it, with nothing left
		// to reap them.
		rec := get(t, h, "/clouds/1/delete")
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "err=") {
			t.Errorf("expected a refusal, got redirect %q", loc)
		}
		if _, err := db.CloudByID(t.Context(), 1); err != nil {
			t.Error("the cloud was deleted despite being in use")
		}
	})

	t.Run("invalid pool edits are refused", func(t *testing.T) {
		rec := post(t, h, "/pools/1", url.Values{
			"name": {"p"}, "labels": {"linux"}, "max_instances": {"3"},
			"job_timeout_sec": {"900"}, "max_lifetime_sec": {"600"},
		})
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "err=") {
			t.Error("a max_lifetime below job_timeout should be refused")
		}
	})

	t.Run("pool can be deleted once nothing runs in it", func(t *testing.T) {
		rec := get(t, h, "/pools/1/delete")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("delete = %d", rec.Code)
		}
	})
}

func TestPoolDeletionBlockedByLiveMachines(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/forges", url.Values{"name": {"f"}, "kind": {"forgejo"}})
	post(t, h, "/pools", url.Values{
		"name": {"p"}, "forge_id": {"1"}, "cloud_id": {"1"}, "size_id": {"1"},
		"labels": {"linux"}, "max_instances": {"3"},
		"job_timeout_sec": {"600"}, "max_lifetime_sec": {"1200"},
	})
	if err := db.Create(&store.Instance{
		Name: "rf-live", PoolID: 1, State: store.StateBusy,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/pools/1/delete")
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Error("deleting a pool with running machines should be refused")
	}
}

func TestNotFound(t *testing.T) {
	_, h := newServer(t)
	for _, path := range []string{"/clouds/999", "/forges/999", "/pools/999", "/instances/999"} {
		if rec := get(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestInstancePageShowsLogs(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/forges", url.Values{"name": {"f"}, "kind": {"forgejo"}})
	post(t, h, "/pools", url.Values{
		"name": {"p"}, "forge_id": {"1"}, "cloud_id": {"1"}, "size_id": {"1"},
		"labels": {"linux"}, "max_instances": {"3"},
		"job_timeout_sec": {"600"}, "max_lifetime_sec": {"1200"},
	})
	if err := db.Create(&store.Instance{
		Name: "rf-x", PoolID: 1, State: store.StateFailed,
		Logs: "boot failed: no such image", Error: "provision failed",
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/instances/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// Captured output is the whole reason a failed machine is diagnosable.
	if !strings.Contains(body, "boot failed: no such image") {
		t.Error("the instance page does not show the captured output")
	}
	if !strings.Contains(body, "provision failed") {
		t.Error("the instance page does not show the error")
	}
}

func TestForgeEditShowsWebhookURL(t *testing.T) {
	_, h := newServer(t)
	post(t, h, "/forges", url.Values{
		"name": {"gh"}, "kind": {"github"},
		"settings": {`{"owner":"o","repo":"r"}`},
	})
	rec := get(t, h, "/forges/1")
	if !strings.Contains(rec.Body.String(), "https://rf.example.com/webhooks/1") {
		t.Error("the forge page should show the webhook endpoint to register")
	}
}

func TestReapEndpoint(t *testing.T) {
	_, h := newServer(t)
	rec := post(t, h, "/reap", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /reap = %d", rec.Code)
	}
	_ = io.Discard
}

func TestDeleteSizeAndImage(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/clouds/1/images", url.Values{"name": {"i"}, "spec": {"{}"}})

	if rec := post(t, h, "/sizes/1/delete", nil); rec.Code != http.StatusOK {
		t.Errorf("delete size = %d", rec.Code)
	}
	if rec := post(t, h, "/images/1/delete", nil); rec.Code != http.StatusOK {
		t.Errorf("delete image = %d", rec.Code)
	}

	cl, err := db.CloudByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.Sizes) != 0 || len(cl.Images) != 0 {
		t.Errorf("catalogue entries survived deletion: %d sizes, %d images",
			len(cl.Sizes), len(cl.Images))
	}
}

func TestCheckCloudRecordsStatus(t *testing.T) {
	db, h := newServer(t)
	// A docker cloud pointed at a socket that does not exist: the check must
	// report the failure rather than silently pass.
	post(t, h, "/clouds", url.Values{
		"name": {"c"}, "driver": {"docker"},
		"settings": {`{"socket":"/nonexistent/docker.sock"}`},
	})

	rec := post(t, h, "/clouds/1/check", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("check = %d", rec.Code)
	}
	cl, err := db.CloudByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Status == "" {
		t.Error("the check did not record a status")
	}
	if cl.StatusCheckAt == nil {
		t.Error("the check did not record when it ran")
	}
	// An operator needs to know why, not just that it failed.
	if cl.Status == "error" && cl.StatusDetail == "" {
		t.Error("a failed check recorded no detail")
	}
}

func TestCheckForgeRecordsStatus(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/forges", url.Values{
		"name": {"f"}, "kind": {"forgejo"},
		"settings":    {`{"url":"http://127.0.0.1:1","scope":"repo","owner":"o","repo":"r"}`},
		"credentials": {`{"token":"t"}`},
	})

	rec := post(t, h, "/forges/1/check", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("check = %d", rec.Code)
	}
	f, err := db.ForgeByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "error" {
		t.Errorf("status = %q, want error for an unreachable forge", f.Status)
	}
	if f.StatusDetail == "" {
		t.Error("no detail was recorded for the failure")
	}
}

func TestUpdateAndDeleteForge(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/forges", url.Values{
		"name": {"f"}, "kind": {"forgejo"}, "credentials": {`{"token":"original"}`},
	})

	rec := post(t, h, "/forges/1", url.Values{
		"name": {"renamed"}, "enabled": {"on"},
		"settings": {`{"scope":"org","owner":"o"}`},
		// Blank keeps the stored token; the webhook secret is set for the
		// first time.
		"credentials":    {""},
		"webhook_secret": {"whsec"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update = %d, body: %s", rec.Code, rec.Body.String())
	}
	f, err := db.ForgeByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "renamed" {
		t.Errorf("name = %q", f.Name)
	}
	if f.Credentials["token"] != "original" {
		t.Error("blank credentials should preserve the stored token")
	}
	if f.WebhookSecret["secret"] != "whsec" {
		t.Error("the webhook secret was not stored")
	}

	if rec := get(t, h, "/forges/1/delete"); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := db.ForgeByID(t.Context(), 1); err == nil {
		t.Error("the forge was not deleted")
	}
}

func TestUpdateForgeRejectsMalformedSettings(t *testing.T) {
	_, h := newServer(t)
	post(t, h, "/forges", url.Values{"name": {"f"}, "kind": {"forgejo"}})

	rec := post(t, h, "/forges/1", url.Values{"name": {"f"}, "settings": {"{oops"}})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("malformed settings should be refused with a message")
	}
	rec = post(t, h, "/forges/1", url.Values{
		"name": {"f"}, "settings": {"{}"}, "credentials": {"[not an object]"},
	})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("malformed credentials should be refused with a message")
	}
}

func TestDestroyInstanceEndpoint(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/forges", url.Values{"name": {"f"}, "kind": {"forgejo"}})
	post(t, h, "/pools", url.Values{
		"name": {"p"}, "forge_id": {"1"}, "cloud_id": {"1"}, "size_id": {"1"},
		"labels": {"linux"}, "max_instances": {"3"},
		"job_timeout_sec": {"600"}, "max_lifetime_sec": {"1200"},
	})
	// An instance with no provider id never got as far as creating anything,
	// so destroying it is bookkeeping only.
	if err := db.Create(&store.Instance{
		Name: "rf-x", PoolID: 1, State: store.StatePending,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := post(t, h, "/instances/1/destroy", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy = %d, body: %s", rec.Code, rec.Body.String())
	}
	live, err := db.LiveInstances(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("%d instance(s) still live after destroy", len(live))
	}
}

func TestPartialsRenderWithData(t *testing.T) {
	db, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"c"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"s"}, "spec": {"{}"}})
	post(t, h, "/forges", url.Values{"name": {"f"}, "kind": {"forgejo"}})
	post(t, h, "/pools", url.Values{
		"name": {"mypool"}, "forge_id": {"1"}, "cloud_id": {"1"}, "size_id": {"1"},
		"labels": {"linux"}, "max_instances": {"3"},
		"job_timeout_sec": {"600"}, "max_lifetime_sec": {"1200"},
	})
	if err := db.Create(&store.Instance{
		Name: "rf-visible", PoolID: 1, State: store.StateBusy, JobID: "job-7",
	}).Error; err != nil {
		t.Fatal(err)
	}
	db.Logf(t.Context(), "warn", "reap", nil, nil, "a thing happened")

	if body := get(t, h, "/partials/pools").Body.String(); !strings.Contains(body, "mypool") {
		t.Error("the pools partial does not show the pool")
	}
	body := get(t, h, "/partials/instances").Body.String()
	if !strings.Contains(body, "rf-visible") || !strings.Contains(body, "job-7") {
		t.Error("the machines partial does not show the instance and its job")
	}
	if body := get(t, h, "/partials/events").Body.String(); !strings.Contains(body, "a thing happened") {
		t.Error("the events partial does not show the event")
	}
}

func TestCloudOptionsPartialFollowsTheSelectedCloud(t *testing.T) {
	_, h := newServer(t)
	post(t, h, "/clouds", url.Values{"name": {"a"}, "driver": {"docker"}})
	post(t, h, "/clouds/1/sizes", url.Values{"name": {"only-on-a"}, "spec": {"{}"}})
	post(t, h, "/clouds", url.Values{"name": {"b"}, "driver": {"docker"}})

	// Sizes are per cloud, so the picker has to follow the selection.
	body := get(t, h, "/partials/cloud-options?cloud_id=1").Body.String()
	if !strings.Contains(body, "only-on-a") {
		t.Error("the size picker does not show the selected cloud's sizes")
	}
	body = get(t, h, "/partials/cloud-options?cloud_id=2").Body.String()
	if strings.Contains(body, "only-on-a") {
		t.Error("the size picker shows another cloud's sizes")
	}
	if !strings.Contains(body, "no sizes") {
		t.Error("a cloud with no sizes should say so rather than showing an empty picker")
	}

	// An unknown cloud must not panic.
	if rec := get(t, h, "/partials/cloud-options?cloud_id=999"); rec.Code != http.StatusOK {
		t.Errorf("unknown cloud = %d", rec.Code)
	}
}

func TestPoolCreationRefusesIncompleteInput(t *testing.T) {
	_, h := newServer(t)
	rec := post(t, h, "/pools", url.Values{"name": {"p"}})
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "err=") {
		t.Error("a pool with no forge, cloud or size should be refused")
	}
}
