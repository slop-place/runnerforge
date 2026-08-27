package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// stubProvider is a minimal OIDC provider: discovery, a JWKS, and a token
// endpoint that issues a real signed ID token.
//
// Verifying a token is the security-critical half of this package, so it is
// exercised against genuine signatures rather than a stubbed verifier.
type stubProvider struct {
	*httptest.Server

	key   *rsa.PrivateKey
	keyID string

	// Knobs for the failure cases.
	nonceOverride   string
	subjectOverride string
	email           string
	emailVerified   bool
	audience        string
	// lastVerifier records the PKCE verifier the client sent.
	lastVerifier string
}

func newStubProvider(t *testing.T) *stubProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubProvider{
		key: key, keyID: "test-key", email: "ops@example.com", emailVerified: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: p.keyID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.lastVerifier = r.Form.Get("code_verifier")

		aud := p.audience
		if aud == "" {
			aud = r.Form.Get("client_id")
		}
		if aud == "" {
			aud = "runnerforge-test"
		}
		sub := p.subjectOverride
		if sub == "" {
			sub = "user-123"
		}
		// The nonce the client asked for rides in the code, so the token can
		// echo it back the way a real provider does.
		nonce := p.nonceOverride
		if nonce == "" {
			nonce = strings.TrimPrefix(r.Form.Get("code"), "code-for-")
		}

		claims := map[string]any{
			"iss": p.URL, "aud": aud, "sub": sub, "nonce": nonce,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"email": p.email, "email_verified": p.emailVerified, "name": "Ops Person",
		}
		signer, err := jose.NewSigner(
			jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", p.keyID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer",
			"expires_in": 3600, "id_token": idToken,
		})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

// newAuth builds an Authenticator against the stub.
func newAuth(t *testing.T, p *stubProvider, cfg Config) *Authenticator {
	t.Helper()
	cfg.Issuer = p.URL
	if cfg.ClientID == "" {
		cfg.ClientID = "runnerforge-test"
	}
	if cfg.RedirectURL == "" {
		cfg.RedirectURL = "http://runnerforge.test/auth/callback"
	}
	a, err := New(context.Background(), cfg, testKey, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("build authenticator: %v", err)
	}
	return a
}

// protected is the handler behind the middleware.
func protected() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "hello "+u.Display())
	})
}

func TestMiddlewareRedirectsWhenSignedOut(t *testing.T) {
	p := newStubProvider(t)
	a := newAuth(t, p, Config{})

	rec := httptest.NewRecorder()
	a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pools", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want a redirect to the provider", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, p.URL+"/authorize") {
		t.Errorf("redirected to %q, want the provider", loc)
	}
	// PKCE must be used even with a client secret, so an intercepted code is
	// useless on its own.
	u, _ := url.Parse(loc)
	if u.Query().Get("code_challenge") == "" {
		t.Error("no PKCE challenge was sent")
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("challenge method = %q, want S256", u.Query().Get("code_challenge_method"))
	}
	if u.Query().Get("nonce") == "" {
		t.Error("no nonce was sent")
	}
	if u.Query().Get("state") == "" {
		t.Error("no state was sent")
	}
}

func TestPublicPathsAreReachableSignedOut(t *testing.T) {
	p := newStubProvider(t)
	a := newAuth(t, p, Config{})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	// These have to work before sign-in, or the health check fails and the
	// stylesheet never loads on the sign-in error page.
	for _, path := range []string{"/healthz", "/static/app.css", "/auth/login"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s = %d, want it to pass through", path, rec.Code)
		}
	}
}

// signIn drives the whole flow and returns the session cookie.
func signIn(t *testing.T, a *Authenticator, startPath string) *http.Cookie {
	t.Helper()

	rec := httptest.NewRecorder()
	a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, startPath, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("expected a redirect, got %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	nonce := loc.Query().Get("nonce")

	var stateJar *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie {
			stateJar = c
		}
	}
	if stateJar == nil {
		t.Fatal("no state cookie was set")
	}

	// The provider redirects back with a code; the stub encodes the nonce in it.
	cb := httptest.NewRequest(http.MethodGet,
		"/auth/callback?state="+url.QueryEscape(state)+"&code=code-for-"+url.QueryEscape(nonce), nil)
	cb.AddCookie(stateJar)
	cbRec := httptest.NewRecorder()
	a.callback(cbRec, cb)

	if cbRec.Code != http.StatusFound {
		t.Fatalf("callback = %d, body: %s", cbRec.Code, cbRec.Body.String())
	}
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

func TestFullSignInFlow(t *testing.T) {
	p := newStubProvider(t)
	a := newAuth(t, p, Config{})

	session := signIn(t, a, "/pools")

	// The PKCE verifier must reach the token endpoint.
	if p.lastVerifier == "" {
		t.Error("no PKCE verifier was sent to the token endpoint")
	}

	req := httptest.NewRequest(http.MethodGet, "/pools", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	a.Middleware(protected()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("signed-in request = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ops@example.com") {
		t.Errorf("the handler did not see the signed-in user: %q", rec.Body.String())
	}
}

func TestSignInReturnsToTheRequestedPage(t *testing.T) {
	p := newStubProvider(t)
	a := newAuth(t, p, Config{})

	rec := httptest.NewRecorder()
	a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/clouds/2", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	var jar *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie {
			jar = c
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+
		url.QueryEscape(loc.Query().Get("state"))+"&code=code-for-"+
		url.QueryEscape(loc.Query().Get("nonce")), nil)
	cb.AddCookie(jar)
	cbRec := httptest.NewRecorder()
	a.callback(cbRec, cb)

	// A deep link should survive the round trip.
	if got := cbRec.Header().Get("Location"); got != "/clouds/2" {
		t.Errorf("landed on %q, want the page originally requested", got)
	}
}

func TestCallbackRejections(t *testing.T) {
	p := newStubProvider(t)

	t.Run("no state cookie", func(t *testing.T) {
		a := newAuth(t, p, Config{})
		rec := httptest.NewRecorder()
		a.callback(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=y", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("mismatched state", func(t *testing.T) {
		a := newAuth(t, p, Config{})
		rec := httptest.NewRecorder()
		a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		var jar *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == stateCookie {
				jar = c
			}
		}
		// A callback carrying someone else's state must not be accepted.
		cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state=not-mine&code=y", nil)
		cb.AddCookie(jar)
		cbRec := httptest.NewRecorder()
		a.callback(cbRec, cb)
		if cbRec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", cbRec.Code)
		}
	})

	t.Run("mismatched nonce", func(t *testing.T) {
		// A token minted for a different request must be refused, which is the
		// whole point of the nonce.
		p2 := newStubProvider(t)
		p2.nonceOverride = "some-other-nonce"
		a := newAuth(t, p2, Config{})

		rec := httptest.NewRecorder()
		a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		var jar *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == stateCookie {
				jar = c
			}
		}
		cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+
			url.QueryEscape(loc.Query().Get("state"))+"&code=code-for-x", nil)
		cb.AddCookie(jar)
		cbRec := httptest.NewRecorder()
		a.callback(cbRec, cb)
		if cbRec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a mismatched nonce", cbRec.Code)
		}
	})

	t.Run("token for another audience", func(t *testing.T) {
		// A token issued for a different client is not ours to accept.
		p2 := newStubProvider(t)
		p2.audience = "some-other-app"
		a := newAuth(t, p2, Config{})

		rec := httptest.NewRecorder()
		a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		var jar *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == stateCookie {
				jar = c
			}
		}
		cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+
			url.QueryEscape(loc.Query().Get("state"))+"&code=code-for-"+
			url.QueryEscape(loc.Query().Get("nonce")), nil)
		cb.AddCookie(jar)
		cbRec := httptest.NewRecorder()
		a.callback(cbRec, cb)
		if cbRec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a foreign audience", cbRec.Code)
		}
	})

	t.Run("provider reported an error", func(t *testing.T) {
		a := newAuth(t, p, Config{})
		rec := httptest.NewRecorder()
		a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		var jar *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == stateCookie {
				jar = c
			}
		}
		cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+
			url.QueryEscape(loc.Query().Get("state"))+
			"&error=access_denied&error_description=<script>alert(1)</script>", nil)
		cb.AddCookie(jar)
		cbRec := httptest.NewRecorder()
		a.callback(cbRec, cb)

		if cbRec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", cbRec.Code)
		}
		// Provider-supplied text must not be echoed into the response.
		if strings.Contains(cbRec.Body.String(), "<script>") {
			t.Error("provider-supplied text was echoed into the page")
		}
	})
}

func TestSignInRefusedForDisallowedAccount(t *testing.T) {
	p := newStubProvider(t)
	p.email = "stranger@elsewhere.com"
	a := newAuth(t, p, Config{AllowedDomains: []string{"example.com"}})

	rec := httptest.NewRecorder()
	a.Middleware(protected()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	var jar *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie {
			jar = c
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+
		url.QueryEscape(loc.Query().Get("state"))+"&code=code-for-"+
		url.QueryEscape(loc.Query().Get("nonce")), nil)
	cb.AddCookie(jar)
	cbRec := httptest.NewRecorder()
	a.callback(cbRec, cb)

	if cbRec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the sign-in refused", cbRec.Code)
	}
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("a session was issued to a disallowed account")
		}
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	p := newStubProvider(t)
	a := newAuth(t, p, Config{})
	session := signIn(t, a, "/instances")

	rec := httptest.NewRecorder()
	a.logout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the session cookie was not cleared")
	}

	// The old cookie must not still work; it does until it expires, which is
	// why logout is a client-side clear and sessions are kept short.
	_ = session
}

func TestNewFailsOnUnreachableIssuer(t *testing.T) {
	t.Parallel()
	_, err := New(context.Background(), Config{
		Issuer: "http://127.0.0.1:1", ClientID: "c", RedirectURL: "http://x/cb",
	}, testKey, slog.New(slog.DiscardHandler))
	// Better to fail at startup than on an operator's first request.
	if err == nil {
		t.Fatal("expected discovery against an unreachable issuer to fail")
	}
}

func TestSecureCookiesFollowTheRedirectScheme(t *testing.T) {
	p := newStubProvider(t)
	https := newAuth(t, p, Config{RedirectURL: "https://rf.example.com/auth/callback"})
	if !https.secure {
		t.Error("an https redirect should mark cookies Secure")
	}
	plain := newAuth(t, p, Config{RedirectURL: "http://rf.internal/auth/callback"})
	// Forcing Secure on would stop the cookie being sent at all on a plain-http
	// deployment, which is a legitimate way to run this on a private network.
	if plain.secure {
		t.Error("a plain-http redirect should not mark cookies Secure")
	}
}
