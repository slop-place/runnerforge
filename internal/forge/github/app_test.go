package github_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/forge"
)

// appStub is a GitHub that only knows how to mint installation tokens and
// list runners, and checks the App JWT the way GitHub does: RS256 over the
// header and claims, issuer equal to the App id, expiry in the future.
type appStub struct {
	*httptest.Server

	pub      *rsa.PublicKey
	mints    atomic.Int32
	expiry   time.Duration
	lastAuth atomic.Value // the Authorization header seen on /orgs/... calls
}

func newAppStub(t *testing.T, pub *rsa.PublicKey) *appStub {
	t.Helper()
	s := &appStub{pub: pub, expiry: time.Hour}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if err := s.verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
				return
			}
			n := s.mints.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_installation_" + strings.Repeat("x", int(n)),
				"expires_at": time.Now().Add(s.expiry).UTC().Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runners"):
			s.lastAuth.Store(r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"runners": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *appStub) verifyJWT(tok string) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return badJWTError("not three parts")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return badJWTError("signature is not base64url")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(s.pub, crypto.SHA256, digest[:], sig); err != nil {
		return badJWTError("signature does not verify")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return badJWTError("claims do not parse")
	}
	now := time.Now().Unix()
	switch {
	case claims.Iss != "12345":
		return badJWTError("issuer is not the app id")
	case claims.Exp <= now:
		return badJWTError("expired")
	case claims.Exp-claims.Iat > 600+60:
		return badJWTError("lifetime over ten minutes")
	case claims.Iat > now:
		return badJWTError("issued in the future")
	}
	return nil
}

type badJWTError string

func (e badJWTError) Error() string { return "bad jwt: " + string(e) }

func newKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(pemBytes)
}

func appForge(t *testing.T, stub *appStub, pemKey string) forge.Forge {
	t.Helper()
	cfg := map[string]any{
		"url": stub.URL, "scope": "org", "owner": "acme",
		"app_id": "12345", "installation_id": "67890", "private_key": pemKey,
	}
	f, err := forge.New(forge.KindGitHub, cfg)
	if err != nil {
		t.Fatalf("build forge: %v", err)
	}
	return f
}

func TestAppAuthMintsAndReusesInstallationToken(t *testing.T) {
	key, pemKey := newKey(t)
	stub := newAppStub(t, &key.PublicKey)
	f := appForge(t, stub, pemKey)

	for range 3 {
		if _, err := f.ListRunners(t.Context()); err != nil {
			t.Fatalf("list runners: %v", err)
		}
	}
	if got := stub.mints.Load(); got != 1 {
		t.Errorf("minted %d installation tokens for three calls, want 1", got)
	}
	if got, _ := stub.lastAuth.Load().(string); got != "Bearer ghs_installation_x" {
		t.Errorf("API call carried %q, want the installation token", got)
	}
}

func TestAppAuthRefreshesNearExpiry(t *testing.T) {
	key, pemKey := newKey(t)
	stub := newAppStub(t, &key.PublicKey)
	// A token that GitHub says expires in two minutes is inside the refresh
	// margin, so every call mints again rather than risking the boundary.
	stub.expiry = 2 * time.Minute
	f := appForge(t, stub, pemKey)
	for range 2 {
		if _, err := f.ListRunners(t.Context()); err != nil {
			t.Fatalf("list runners: %v", err)
		}
	}
	if got := stub.mints.Load(); got != 2 {
		t.Errorf("minted %d tokens, want 2 (refresh inside the margin)", got)
	}
}

func TestAppAuthRejectedTokenSurfaces(t *testing.T) {
	_, pemKey := newKey(t)
	other, _ := newKey(t)
	// The stub verifies against a different key: GitHub would say 401.
	stub := newAppStub(t, &other.PublicKey)
	f := appForge(t, stub, pemKey)
	_, err := f.ListRunners(t.Context())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401 from the token exchange", err)
	}
}

func TestAppPrivateKeyTolerantOfNewlineDamage(t *testing.T) {
	key, pemKey := newKey(t)
	stub := newAppStub(t, &key.PublicKey)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8PEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	for name, k := range map[string]string{
		"pkcs1":            pemKey,
		"pkcs8":            pkcs8PEM,
		"spaces for lines": strings.ReplaceAll(pemKey, "\n", " "),
		"escaped newlines": strings.ReplaceAll(pemKey, "\n", `\n`),
		"crlf":             strings.ReplaceAll(pemKey, "\n", "\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			f := appForge(t, stub, k)
			if _, err := f.ListRunners(t.Context()); err != nil {
				t.Fatalf("key in %s form was not accepted: %v", name, err)
			}
		})
	}
}

func TestAppCredentialValidation(t *testing.T) {
	_, pemKey := newKey(t)
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"nothing at all", map[string]any{"owner": "o", "repo": "r"}, "token is required"},
		{"partial app", map[string]any{"owner": "o", "repo": "r", "app_id": "1"}, "together"},
		{"both", map[string]any{"owner": "o", "repo": "r", "token": "t", "app_id": "1",
			"installation_id": "2", "private_key": pemKey}, "not both"},
		{"garbage key", map[string]any{"owner": "o", "repo": "r", "app_id": "1",
			"installation_id": "2", "private_key": "not a key"}, "PEM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := forge.New(forge.KindGitHub, tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestJobQueuedAsksTheRepository(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobs/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "status": "queued"})
		case strings.HasSuffix(r.URL.Path, "/jobs/8"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 8, "status": "completed"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f := newForge(t, map[string]any{"url": srv.URL, "scope": "org"})
	checker, ok := f.(forge.JobChecker)
	if !ok {
		t.Fatal("the GitHub forge should be able to check a job")
	}
	for id, want := range map[string]bool{"7": true, "8": false, "9": false} {
		got, err := checker.JobQueued(t.Context(), forge.Job{ID: id, Repo: "acme/widgets"})
		if err != nil {
			t.Fatalf("job %s: %v", id, err)
		}
		if got != want {
			t.Errorf("job %s queued = %v, want %v", id, got, want)
		}
	}
	if !strings.HasPrefix(path, "/repos/acme/widgets/actions/jobs/") {
		t.Errorf("checked at %q, want the repository's jobs endpoint", path)
	}
	if _, err := checker.JobQueued(t.Context(), forge.Job{ID: "7"}); err == nil {
		t.Error("a job without a repository cannot be checked and should say so")
	}
}
