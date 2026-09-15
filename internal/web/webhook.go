package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// A webhook delivery is the forge telling us a job is waiting, which is the
// only way to hear about work at a scope that has no queue to poll (GitHub at
// organisation level). Deliveries are authenticated by the HMAC the forge
// computes over the body with the secret the operator set on the forge; a
// forge with no secret accepts no deliveries at all.

const (
	// maxWebhookBody bounds what is read from a delivery. A workflow_job
	// payload is a few kilobytes; a megabyte leaves room for the largest
	// job matrices GitHub sends.
	maxWebhookBody = 1 << 20
	// signatureHeader carries GitHub's HMAC-SHA256 of the body.
	signatureHeader = "X-Hub-Signature-256"
	// signaturePrefix precedes the hex digest in that header.
	signaturePrefix = "sha256="
	// Delivery outcomes, as the metric labels them.
	outcomeError  = "error"
	actionWaiting = "waiting"
	actionQueued  = "queued"
)

// webhook handles POST /webhooks/{forge}, where {forge} is the forge's name
// or its numeric id. The name is what a deployment managed as code should
// register with the forge, since ids are assigned by whichever database the
// deployment happens to be running on.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ref := strings.Trim(r.PathValue("forge"), "/")
	f, err := s.forgeByRef(r, ref)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	forgeName := f.Name
	secret := f.WebhookSecret["secret"]
	if secret == "" {
		// Not a 401: without a secret nothing can be authenticated, so the
		// endpoint does not exist for this forge.
		metrics.WebhookDelivery(forgeName, "", "no_secret")
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil || len(body) > maxWebhookBody {
		metrics.WebhookDelivery(forgeName, "", "bad_body")
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(secret, body, r.Header.Get(signatureHeader)) {
		metrics.WebhookDelivery(forgeName, "", "bad_signature")
		s.log.Warn("webhook delivery with a bad signature", "forge", forgeName, "remote", r.RemoteAddr)
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-Github-Event")
	switch event {
	case "ping":
		metrics.WebhookDelivery(forgeName, event, "ok")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	case "workflow_job":
		outcome, err := s.applyWorkflowJob(ctx, f, body)
		metrics.WebhookDelivery(forgeName, event, outcome)
		if err != nil {
			s.log.Error("webhook delivery failed", "forge", forgeName, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		// Subscribed to more than we use; acknowledge so the forge does not
		// count it as a failed delivery.
		metrics.WebhookDelivery(forgeName, event, "ignored")
		w.WriteHeader(http.StatusAccepted)
	}
}

// workflowJobEvent is the part of GitHub's workflow_job payload we act on.
type workflowJobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID        int64     `json:"id"`
		RunID     int64     `json:"run_id"`
		Status    string    `json:"status"`
		Labels    []string  `json:"labels"`
		HeadSHA   string    `json:"head_sha"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// applyWorkflowJob records a queued job and retires one that has moved on.
// It returns the outcome label for the metric.
func (s *Server) applyWorkflowJob(ctx context.Context, f *store.Forge, body []byte) (string, error) {
	var ev workflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "bad_payload", errors.New("workflow_job payload does not parse")
	}
	if ev.WorkflowJob.ID == 0 {
		return "bad_payload", errors.New("workflow_job payload has no job id")
	}
	jobID := strconv.FormatInt(ev.WorkflowJob.ID, 10)
	switch ev.Action {
	case actionWaiting:
		// A job blocked on a deployment approval; it is delivered again as
		// queued when released, which is when it becomes demand.
		return actionWaiting, nil
	case actionQueued:
		err := s.db.RecordWebhookJob(ctx, &store.WebhookJob{
			ForgeID:    f.ID,
			JobID:      jobID,
			Labels:     store.StringList(ev.WorkflowJob.Labels),
			Repo:       ev.Repository.FullName,
			Ref:        ev.WorkflowJob.HeadSHA,
			QueuedAt:   ev.WorkflowJob.CreatedAt,
			ReceivedAt: time.Now().UTC(),
		})
		if err != nil {
			return outcomeError, err
		}
		s.db.Logf(ctx, "info", "webhook", nil, nil,
			"%s: job %s queued in %s with labels %s", f.Name, jobID, ev.Repository.FullName,
			strings.Join(ev.WorkflowJob.Labels, ","))
		return actionQueued, nil
	case "in_progress", "completed":
		if err := s.db.DeleteWebhookJob(ctx, f.ID, jobID); err != nil {
			return outcomeError, err
		}
		return ev.Action, nil
	default:
		return "ignored", nil
	}
}

// forgeByRef resolves the path segment to a forge, by id when it is numeric.
func (s *Server) forgeByRef(r *http.Request, ref string) (*store.Forge, error) {
	ctx := r.Context()
	if ref == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if id, err := strconv.ParseUint(ref, 10, 32); err == nil {
		if f, err := s.db.ForgeByID(ctx, uint(id)); err == nil {
			return f, nil
		}
	}
	return s.db.ForgeByName(ctx, ref)
}

// validSignature checks GitHub's HMAC-SHA256 header against the body.
func validSignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// signBody computes the header value GitHub would send for a body, for the
// console to show operators and for tests.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}
