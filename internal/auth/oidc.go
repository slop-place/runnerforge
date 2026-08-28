package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config is what an operator sets to gate the UI.
type Config struct {
	// Issuer is the OIDC provider's base URL. Empty disables authentication.
	Issuer string `yaml:"issuer"`
	// ClientID and ClientSecret identify runnerforge to the provider. A public
	// client may leave the secret empty; PKCE is always used either way.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// RedirectURL must exactly match what is registered with the provider.
	RedirectURL string `yaml:"redirect_url"`
	// Scopes beyond openid. Defaults to profile and email.
	Scopes []string `yaml:"scopes"`

	// AllowedEmails and AllowedDomains restrict who may sign in. With neither
	// set, anyone the provider will authenticate is allowed — which is the
	// right default only when the provider is already restricted to your
	// organisation.
	AllowedEmails  []string `yaml:"allowed_emails"`
	AllowedDomains []string `yaml:"allowed_domains"`

	// SessionTTL is how long a sign-in lasts. Default 12h.
	SessionTTL time.Duration `yaml:"session_ttl"`
}

// Enabled reports whether authentication is configured.
func (c Config) Enabled() bool { return strings.TrimSpace(c.Issuer) != "" }

// Validate checks the settings an enabled config needs.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "client_id")
	}
	if c.RedirectURL == "" {
		missing = append(missing, "redirect_url")
	}
	if len(missing) > 0 {
		return fmt.Errorf("oidc: %s required when an issuer is set", strings.Join(missing, " and "))
	}
	return nil
}

// defaultSessionTTL is how long a sign-in lasts when the operator sets no value.
const defaultSessionTTL = 12 * time.Hour

// stateCookie holds the CSRF state, PKCE verifier and return path across the
// redirect to the provider. It is short-lived and separate from the session.
const stateCookie = "runnerforge_oidc"

// stateTTL bounds how long a sign-in may take.
const stateTTL = 10 * time.Minute

// Authenticator gates HTTP handlers behind an OIDC provider.
type Authenticator struct {
	cfg      Config
	key      []byte
	log      *slog.Logger
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	secure   bool
}

// New builds an Authenticator. It contacts the issuer for discovery, so it can
// fail when the provider is unreachable.
//
// A disabled config returns a nil Authenticator, which every method tolerates:
// running without authentication stays possible, it is just loudly announced.
func New(ctx context.Context, cfg Config, key []byte, log *slog.Logger) (*Authenticator, error) {
	if !cfg.Enabled() {
		return nil, nil //nolint:nilnil // a disabled authenticator is not an error
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &Authenticator{
		cfg:      cfg,
		key:      key,
		log:      log,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		// Cookies are marked Secure whenever the redirect is https, which is
		// the only case where the browser would send them back over TLS.
		secure: strings.HasPrefix(cfg.RedirectURL, "https://"),
	}, nil
}

// Enabled reports whether requests are gated.
func (a *Authenticator) Enabled() bool { return a != nil }

// Middleware gates a handler.
//
// Paths that must work before sign-in — the health check, static assets and the
// sign-in flow itself — are allowed through. Everything else redirects to the
// provider.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, err := a.session(r)
		if err != nil {
			a.startLogin(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// isPublicPath reports whether a path is reachable without signing in.
func isPublicPath(p string) bool {
	switch {
	case p == "/healthz":
		return true
	case strings.HasPrefix(p, "/static/"):
		return true
	case strings.HasPrefix(p, "/auth/"):
		return true
	case strings.HasPrefix(p, "/api/"):
		// The JSON API authenticates with a bearer token of its own. Sending a
		// browser redirect to a Terraform provider would be useless anyway.
		return true
	default:
		return false
	}
}

// Routes registers the sign-in endpoints.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	if !a.Enabled() {
		return
	}
	mux.HandleFunc("GET /auth/login", a.startLogin)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

// startLogin sends the browser to the provider.
func (a *Authenticator) startLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomString()
	if err != nil {
		http.Error(w, "could not start sign-in", http.StatusInternalServerError)
		return
	}
	verifier, err := randomString()
	if err != nil {
		http.Error(w, "could not start sign-in", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString()
	if err != nil {
		http.Error(w, "could not start sign-in", http.StatusInternalServerError)
		return
	}

	// Where to land afterwards, so a deep link survives the round trip. Only a
	// same-origin path is kept; anything else would be an open redirect.
	ret := r.URL.RequestURI()
	if r.URL.Path == "/auth/login" {
		ret = r.URL.Query().Get("next")
	}
	ret = safeReturnPath(ret)

	pending := User{Subject: state, Name: verifier, Email: nonce,
		Expires: time.Now().Add(stateTTL)}
	blob, err := encodeSession(a.key, pending)
	if err != nil {
		http.Error(w, "could not start sign-in", http.StatusInternalServerError)
		return
	}
	// Secure follows the redirect URL's scheme: forcing it on would stop the
	// cookie being sent at all on a plain-http deployment, which is a common
	// and legitimate way to run this on a private network.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set from the deployment's scheme
		Name: stateCookie, Value: blob + "|" + ret, Path: "/",
		Expires: time.Now().Add(stateTTL), HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})

	// PKCE is used even with a client secret: it binds this redirect to this
	// browser, so an intercepted code is useless on its own.
	challenge := sha256.Sum256([]byte(verifier))
	url := a.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// callback completes the flow.
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		a.deny(w, r, "sign-in expired; try again")
		return
	}
	blob, ret, ok := strings.Cut(c.Value, "|")
	if !ok {
		a.deny(w, r, "sign-in state is malformed")
		return
	}
	// Only the blob is signed; the return path rides alongside it. Validate it
	// again here rather than trusting that nothing touched the cookie, or this
	// is an open redirect with extra steps.
	ret = safeReturnPath(ret)
	pending, err := decodeSession(a.key, blob)
	if err != nil {
		a.deny(w, r, "sign-in expired; try again")
		return
	}
	// The state must match, or this callback belongs to someone else's flow.
	if r.URL.Query().Get("state") != pending.Subject {
		a.deny(w, r, "sign-in state did not match")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		// Logged rather than shown: it is attacker-controllable text, and an
		// operator gets more from the log than from the page anyway.
		a.log.Warn("oidc: provider refused the sign-in", "error", errParam)
		a.deny(w, r, "the identity provider refused the sign-in")
		return
	}

	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.SetAuthURLParam("code_verifier", pending.Name))
	if err != nil {
		a.log.Warn("oidc: code exchange failed", "err", err)
		a.deny(w, r, "could not complete sign-in")
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		a.deny(w, r, "the identity provider returned no ID token")
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		a.log.Warn("oidc: ID token rejected", "err", err)
		a.deny(w, r, "the ID token could not be verified")
		return
	}
	// The nonce ties the token to the request that started this flow.
	if idToken.Nonce != pending.Email {
		a.deny(w, r, "the ID token nonce did not match")
		return
	}

	var claims struct {
		Email    string `json:"email"`
		Verified *bool  `json:"email_verified"`
		Name     string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		a.deny(w, r, "could not read the ID token claims")
		return
	}
	if err := a.allow(claims.Email, claims.Verified); err != nil {
		a.log.Warn("oidc: sign-in refused", "email", claims.Email, "err", err)
		a.deny(w, r, err.Error())
		return
	}

	user := User{
		Subject: idToken.Subject, Email: claims.Email, Name: claims.Name,
		Expires: time.Now().Add(a.cfg.SessionTTL),
	}
	if err := setSession(w, a.key, user, a.secure); err != nil {
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	clearStateCookie(w, a.secure)
	a.log.Info("operator signed in", "user", user.Display())
	//nolint:gosec // G710: safeReturnPath admits only same-origin absolute paths.
	http.Redirect(w, r, ret, http.StatusFound)
}

// errNotAllowed is returned when a verified identity is not on the list.
var errNotAllowed = errors.New("this account is not allowed to sign in")

// allow applies the operator's allow-lists.
func (a *Authenticator) allow(email string, verified *bool) error {
	if len(a.cfg.AllowedEmails) == 0 && len(a.cfg.AllowedDomains) == 0 {
		return nil
	}
	// An unverified address must never satisfy an allow-list: on some providers
	// anyone can claim any address until it is verified.
	if verified != nil && !*verified {
		return fmt.Errorf("%w: the address is unverified", errNotAllowed)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, want := range a.cfg.AllowedEmails {
		if strings.EqualFold(strings.TrimSpace(want), email) {
			return nil
		}
	}
	_, domain, ok := strings.Cut(email, "@")
	if ok {
		for _, want := range a.cfg.AllowedDomains {
			if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(want), "@"), domain) {
				return nil
			}
		}
	}
	return errNotAllowed
}

// logout ends the session.
func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, a.secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// deny reports a failed sign-in without starting a redirect loop.
//
// Every message it emits is written by this package. Nothing the provider or
// the query string supplied is echoed back, so there is nothing here to inject
// into.
func (a *Authenticator) deny(w http.ResponseWriter, _ *http.Request, msg string) {
	clearStateCookie(w, a.secure)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, "runnerforge: "+msg+"\n")
}

// safeReturnPath keeps only a same-origin absolute path. Anything else becomes
// the root, so a sign-in can never be steered off-site.
func safeReturnPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

func clearStateCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the deployment's scheme
		Name: stateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

// randomStringBytes is the entropy in a state, nonce or PKCE verifier.
const randomStringBytes = 32

func randomString() (string, error) {
	b := make([]byte, randomStringBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// session reads and verifies the session cookie.
func (a *Authenticator) session(r *http.Request) (User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, ErrNoSession
	}
	return decodeSession(a.key, c.Value)
}
