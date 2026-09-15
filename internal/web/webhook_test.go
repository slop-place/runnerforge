package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/slop-place/runnerforge/internal/store"
)

const hookSecret = "s3cret-hook"

// webhookForge stores a GitHub forge with a webhook secret and returns it.
func webhookForge(t *testing.T, db *store.DB, name string, secret string) *store.Forge {
	t.Helper()
	f := &store.Forge{
		Name: name, Kind: "github", Enabled: true,
		Settings:    store.Params{"scope": "org", "owner": "acme"},
		Credentials: store.Secret{"token": "t"},
	}
	if secret != "" {
		f.WebhookSecret = store.Secret{"secret": secret}
	}
	if err := db.Create(f).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func workflowJob(action string, id int64, labels ...string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"workflow_job": map[string]any{
			"id": id, "run_id": 99, "status": action, "labels": labels,
			"head_sha": "abc123", "created_at": "2026-09-15T10:00:00Z",
		},
		"repository": map[string]any{"full_name": "acme/widgets"},
	})
	return b
}

func deliver(t *testing.T, h http.Handler, path, event string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Event", event)
	if sig != "" {
		req.Header.Set(signatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func webhookRows(t *testing.T, db *store.DB, forgeID uint) []store.WebhookJob {
	t.Helper()
	rows, err := db.WebhookJobs(context.Background(), forgeID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestWebhookRecordsQueuedJobAndRetiresIt(t *testing.T) {
	db, h := newServer(t)
	f := webhookForge(t, db, "gh", hookSecret)

	body := workflowJob("queued", 42, "self-hosted", "ovh-small")
	rec := deliver(t, h, "/webhooks/gh", "workflow_job", body, signBody(hookSecret, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued delivery = %d: %s", rec.Code, rec.Body)
	}
	rows := webhookRows(t, db, f.ID)
	if len(rows) != 1 || rows[0].JobID != "42" || rows[0].Repo != "acme/widgets" {
		t.Fatalf("rows = %+v, want job 42 in acme/widgets", rows)
	}
	if strings.Join(rows[0].Labels, ",") != "self-hosted,ovh-small" {
		t.Errorf("labels = %v", rows[0].Labels)
	}

	// Redelivery is idempotent.
	deliver(t, h, "/webhooks/gh", "workflow_job", body, signBody(hookSecret, body))
	if n := len(webhookRows(t, db, f.ID)); n != 1 {
		t.Errorf("%d rows after a redelivery, want 1", n)
	}

	// The job starts elsewhere or finishes: the row goes.
	for _, action := range []string{"in_progress", "completed"} {
		b := workflowJob(action, 42)
		rec := deliver(t, h, "/webhooks/gh", "workflow_job", b, signBody(hookSecret, b))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s delivery = %d", action, rec.Code)
		}
	}
	if n := len(webhookRows(t, db, f.ID)); n != 0 {
		t.Errorf("%d rows after the job finished, want 0", n)
	}
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	db, h := newServer(t)
	f := webhookForge(t, db, "gh", hookSecret)
	body := workflowJob("queued", 1, "x")

	for name, sig := range map[string]string{
		"missing":      "",
		"wrong secret": signBody("other", body),
		"not hex":      "sha256=zz",
		"no prefix":    strings.TrimPrefix(signBody(hookSecret, body), "sha256="),
	} {
		rec := deliver(t, h, "/webhooks/gh", "workflow_job", body, sig)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s signature: %d, want 401", name, rec.Code)
		}
	}
	// A body altered after signing is a different body.
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = 'X'
	if rec := deliver(t, h, "/webhooks/gh", "workflow_job", tampered, signBody(hookSecret, body)); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered body: %d, want 401", rec.Code)
	}
	if n := len(webhookRows(t, db, f.ID)); n != 0 {
		t.Errorf("%d rows were written by unauthenticated deliveries", n)
	}
}

func TestWebhookAddressing(t *testing.T) {
	db, h := newServer(t)
	f := webhookForge(t, db, "runnerforge/github", hookSecret)
	webhookForge(t, db, "silent", "")
	body := workflowJob("queued", 5, "x")
	sig := signBody(hookSecret, body)

	// By the name a manifest knows, slash and all, and by id.
	for _, path := range []string{"/webhooks/runnerforge/github", "/webhooks/" + itoa(f.ID)} {
		if rec := deliver(t, h, path, "ping", []byte("{}"), signBody(hookSecret, []byte("{}"))); rec.Code != http.StatusOK {
			t.Errorf("%s ping = %d, want 200", path, rec.Code)
		}
	}
	if rec := deliver(t, h, "/webhooks/nope", "workflow_job", body, sig); rec.Code != http.StatusNotFound {
		t.Errorf("unknown forge = %d, want 404", rec.Code)
	}
	// A forge with no secret has no endpoint, whatever the signature.
	if rec := deliver(t, h, "/webhooks/silent", "workflow_job", body, sig); rec.Code != http.StatusNotFound {
		t.Errorf("forge without secret = %d, want 404", rec.Code)
	}
	// Other events are acknowledged and ignored.
	if rec := deliver(t, h, "/webhooks/runnerforge/github", "push", []byte("{}"), signBody(hookSecret, []byte("{}"))); rec.Code != http.StatusAccepted {
		t.Errorf("push event = %d, want 202", rec.Code)
	}
	// A payload that is not a workflow_job is refused, not half-applied.
	if rec := deliver(t, h, "/webhooks/runnerforge/github", "workflow_job", []byte("{}"), signBody(hookSecret, []byte("{}"))); rec.Code != http.StatusInternalServerError {
		t.Errorf("empty workflow_job = %d, want 500", rec.Code)
	}
	if n := len(webhookRows(t, db, f.ID)); n != 0 {
		t.Errorf("%d rows, want none from ping/push/empty deliveries", n)
	}
}

func TestWebhookIsOutsideTheSignInGate(t *testing.T) {
	// The handler is registered on the outer mux; a GET there must not be
	// swallowed by the console's routes either.
	_, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhooks/gh", nil))
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET /webhooks/gh = %d, want 405 or 404", rec.Code)
	}
}

func itoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }
