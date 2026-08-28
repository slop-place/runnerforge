package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// apiServer builds a handler with an API token configured.
func apiServer(t *testing.T) http.Handler {
	t.Helper()
	_, h := newServer(t)
	return h
}

const testAPIToken = "test-api-token-0123456789"

// call makes an authenticated API request.
func call(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func TestAPIRequiresAToken(t *testing.T) {
	h := apiServer(t)

	// No credential at all.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pools", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}

	// A wrong one.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pools", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", rec.Code)
	}

	if rec := call(t, h, http.MethodGet, "/api/v1/pools", nil); rec.Code != http.StatusOK {
		t.Errorf("good token = %d, want 200", rec.Code)
	}
}

func TestAPIRefusedWithNoTokensConfigured(t *testing.T) {
	db, h := newServerWithTokens(t, nil)
	_ = db
	// An open machine endpoint is worse than a closed one: refuse outright
	// rather than serving anyone who asks.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pools", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when no tokens are configured", rec.Code)
	}
}

func TestAPICloudLifecycle(t *testing.T) {
	h := apiServer(t)

	rec := call(t, h, http.MethodPost, "/api/v1/clouds", map[string]any{
		"name": "api-cloud", "driver": "docker",
		"settings": map[string]any{"socket": "/var/run/docker.sock"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeBody[map[string]any](t, rec)
	id := int(created["id"].(float64))

	rec = call(t, h, http.MethodGet, "/api/v1/clouds/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}

	rec = call(t, h, http.MethodPut, "/api/v1/clouds/1", map[string]any{
		"name": "renamed", "enabled": false,
		"settings": map[string]any{"socket": "/other.sock"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	updated := decodeBody[map[string]any](t, rec)
	if updated["name"] != "renamed" {
		t.Errorf("name = %v", updated["name"])
	}
	// Omitting a boolean must mean "leave it", but sending false must set it.
	if updated["enabled"] != false {
		t.Errorf("enabled = %v, want false", updated["enabled"])
	}

	rec = call(t, h, http.MethodDelete, "/api/v1/clouds/1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodGet, "/api/v1/clouds/1", nil); rec.Code != http.StatusNotFound {
		t.Errorf("after delete, get = %d, want 404", rec.Code)
	}
	_ = id
}

func TestAPISecretsAreSplitAndNeverReturned(t *testing.T) {
	h := apiServer(t)

	rec := call(t, h, http.MethodPost, "/api/v1/clouds", map[string]any{
		"name": "os", "driver": "openstack",
		"settings": map[string]any{
			"auth_url": "https://auth.example/v3", "region": "R1",
			"project_id": "p", "username": "u", "password": "hunter2",
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	// A caller declares what it wants configured; runnerforge decides which
	// values are secret and where they live.
	if bytes.Contains(rec.Body.Bytes(), []byte("hunter2")) {
		t.Error("the API returned a stored secret")
	}
	body := decodeBody[map[string]any](t, rec)
	settings, _ := body["settings"].(map[string]any)
	if settings["password"] != nil {
		t.Error("a secret was stored in the plain settings column")
	}
	if settings["region"] != "R1" {
		t.Errorf("a non-secret setting was lost: %v", settings)
	}
}

func TestAPIUpdateKeepsUnmentionedSecrets(t *testing.T) {
	h := apiServer(t)
	call(t, h, http.MethodPost, "/api/v1/clouds", map[string]any{
		"name": "os", "driver": "openstack",
		"settings": map[string]any{
			"auth_url": "https://auth.example/v3", "region": "R1",
			"project_id": "p", "username": "u", "password": "keepme",
		},
	})
	// A Terraform plan that changes the region must not erase the password it
	// was never told.
	rec := call(t, h, http.MethodPut, "/api/v1/clouds/1", map[string]any{
		"settings": map[string]any{
			"auth_url": "https://auth.example/v3", "region": "R2",
			"project_id": "p", "username": "u",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	db, _ := currentDB(t)
	c, err := db.CloudByID(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Credentials["password"] != "keepme" {
		t.Error("an update that did not mention the password erased it")
	}
	if c.Settings.String("region") != "R2" {
		t.Error("the update did not apply")
	}
}

func TestAPIRejectsUnknownFields(t *testing.T) {
	h := apiServer(t)
	// A typo in a Terraform resource should fail loudly rather than becoming a
	// setting that never takes effect.
	rec := call(t, h, http.MethodPost, "/api/v1/clouds", map[string]any{
		"name": "x", "driver": "docker", "notAField": "typo",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestAPIRejectsUnknownDriverAndKind(t *testing.T) {
	h := apiServer(t)
	rec := call(t, h, http.MethodPost, "/api/v1/clouds",
		map[string]any{"name": "x", "driver": "nope"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown driver = %d, want 400", rec.Code)
	}
	rec = call(t, h, http.MethodPost, "/api/v1/forges",
		map[string]any{"name": "x", "kind": "bitbucket"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown forge kind = %d, want 400", rec.Code)
	}
}

func TestAPIPoolValidation(t *testing.T) {
	h := apiServer(t)
	seedForAPI(t, h)

	// The same rule the forms enforce: a lifetime below the job timeout would
	// have the reaper destroying machines mid-job.
	rec := call(t, h, http.MethodPost, "/api/v1/pools", map[string]any{
		"name": "bad", "forge_id": 1, "cloud_id": 1, "size_id": 1,
		"labels": []string{"linux"}, "max_instances": 2,
		"job_timeout_sec": 900, "max_lifetime_sec": 600,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("max lifetime")) {
		t.Errorf("the error does not say what is wrong: %s", rec.Body.String())
	}
}

func TestAPIRefusesDeletingWhatIsInUse(t *testing.T) {
	h := apiServer(t)
	seedForAPI(t, h)
	rec := call(t, h, http.MethodPost, "/api/v1/pools", map[string]any{
		"name": "p", "forge_id": 1, "cloud_id": 1, "size_id": 1,
		"labels": []string{"linux"}, "max_instances": 2,
		"job_timeout_sec": 600, "max_lifetime_sec": 1200,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool = %d: %s", rec.Code, rec.Body.String())
	}

	// Code and clicks must agree about what is safe to remove.
	for _, path := range []string{"/api/v1/clouds/1", "/api/v1/forges/1", "/api/v1/sizes/1"} {
		if rec := call(t, h, http.MethodDelete, path, nil); rec.Code != http.StatusConflict {
			t.Errorf("DELETE %s = %d, want 409 while a pool uses it", path, rec.Code)
		}
	}
	if rec := call(t, h, http.MethodDelete, "/api/v1/pools/1", nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete pool = %d", rec.Code)
	}
	// With the pool gone the rest can go.
	if rec := call(t, h, http.MethodDelete, "/api/v1/sizes/1", nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete size after the pool = %d", rec.Code)
	}
}

func TestAPIListsAndInstances(t *testing.T) {
	h := apiServer(t)
	seedForAPI(t, h)
	for _, path := range []string{
		"/api/v1/clouds", "/api/v1/forges", "/api/v1/pools", "/api/v1/instances",
	} {
		if rec := call(t, h, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
}

func TestAPISizeAndImageLifecycle(t *testing.T) {
	h := apiServer(t)
	call(t, h, http.MethodPost, "/api/v1/clouds",
		map[string]any{"name": "c", "driver": "docker"})

	rec := call(t, h, http.MethodPost, "/api/v1/sizes", map[string]any{
		"cloud_id": 1, "name": "small",
		"spec": map[string]any{"cpus": "2"}, "vcpus": 2, "hourly_usd": 0.01,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create size = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodGet, "/api/v1/sizes/1", nil); rec.Code != http.StatusOK {
		t.Errorf("get size = %d", rec.Code)
	}
	rec = call(t, h, http.MethodPut, "/api/v1/sizes/1",
		map[string]any{"cloud_id": 1, "name": "renamed", "vcpus": 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("update size = %d: %s", rec.Code, rec.Body.String())
	}

	rec = call(t, h, http.MethodPost, "/api/v1/images", map[string]any{
		"cloud_id": 1, "name": "img", "spec": map[string]any{"image": "alpine"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create image = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodGet, "/api/v1/images/1", nil); rec.Code != http.StatusOK {
		t.Errorf("get image = %d", rec.Code)
	}
	rec = call(t, h, http.MethodPut, "/api/v1/images/1",
		map[string]any{"cloud_id": 1, "name": "img2", "username": "root"})
	if rec.Code != http.StatusOK {
		t.Errorf("update image = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodDelete, "/api/v1/images/1", nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete image = %d", rec.Code)
	}
}

func TestAPIMissingRecords(t *testing.T) {
	h := apiServer(t)
	for _, path := range []string{
		"/api/v1/clouds/999", "/api/v1/forges/999", "/api/v1/pools/999",
		"/api/v1/sizes/999", "/api/v1/images/999",
	} {
		if rec := call(t, h, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// seedForAPI creates a cloud, a size and a forge for pool tests.
func seedForAPI(t *testing.T, h http.Handler) {
	t.Helper()
	call(t, h, http.MethodPost, "/api/v1/clouds",
		map[string]any{"name": "c", "driver": "docker"})
	call(t, h, http.MethodPost, "/api/v1/sizes",
		map[string]any{"cloud_id": 1, "name": "small"})
	call(t, h, http.MethodPost, "/api/v1/forges", map[string]any{
		"name": "f", "kind": "forgejo",
		"settings": map[string]any{
			"url": "http://f", "scope": "repo", "owner": "o", "repo": "r", "token": "t",
		},
	})
}
