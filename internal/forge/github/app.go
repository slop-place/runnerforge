package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slop-place/runnerforge/internal/forge"
)

// A GitHub App is the credential an organisation should give a service:
// it belongs to the org rather than to whoever set it up, its permissions are
// enumerated, and the tokens it mints last an hour. runnerforge holds the
// App's private key, signs a short-lived JWT with it, and exchanges that for
// an installation token whenever the cached one is about to expire.

const (
	// jwtLifetime is how long an App JWT is valid; GitHub caps it at ten
	// minutes, and clock skew between us and GitHub eats into that.
	jwtLifetime = 9 * time.Minute
	// jwtBackdate guards against GitHub seeing the token before its issue
	// time, which it rejects.
	jwtBackdate = 60 * time.Second
	// tokenRefreshMargin is how long before expiry a cached installation
	// token is replaced, so a call made on the boundary does not fail.
	tokenRefreshMargin = 5 * time.Minute
	// defaultTokenLifetime is assumed when GitHub's response lacks an expiry.
	defaultTokenLifetime = time.Hour
)

// appAuth mints installation tokens for one App installation.
type appAuth struct {
	appID          string
	installationID string
	key            *rsa.PrivateKey
	api            string
	http           *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newAppAuth(api, appID, installationID, privateKey string) (*appAuth, error) {
	key, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &appAuth{
		appID: appID, installationID: installationID, key: key,
		api:  strings.TrimRight(api, "/"),
		http: &http.Client{Timeout: requestTimeout},
	}, nil
}

// requestTimeout bounds the token exchange.
const requestTimeout = 30 * time.Second

// Authorization returns a bearer header value, minting a new installation
// token when the cached one is missing or close to expiry.
func (a *appAuth) Authorization(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > tokenRefreshMargin {
		return "Bearer " + a.token, nil
	}
	tok, exp, err := a.mint(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = tok, exp
	return "Bearer " + tok, nil
}

// mint exchanges an App JWT for an installation token.
func (a *appAuth) mint(ctx context.Context) (string, time.Time, error) {
	jwt, err := a.jwt(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", a.api, a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Message   string    `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && resp.StatusCode < http.StatusMultipleChoices {
		return "", time.Time{}, fmt.Errorf("github app: decode installation token: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, &forge.HTTPError{
			Status: resp.StatusCode, Method: http.MethodPost, URL: url, Body: body.Message,
		}
	}
	if body.Token == "" {
		return "", time.Time{}, errors.New("github app: installation token response carried no token")
	}
	exp := body.ExpiresAt
	if exp.IsZero() {
		exp = time.Now().Add(defaultTokenLifetime)
	}
	return body.Token, exp, nil
}

// jwt signs the App's identity: RS256, issued a minute ago to absorb skew,
// valid for nine minutes.
func (a *appAuth) jwt(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", fmt.Errorf("github app: encode claims: %w", err)
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign jwt: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// pemBody finds the base64 between PEM armour lines, whatever became of the
// newlines: a key pasted into a single-line form field arrives with them
// collapsed to spaces, and one wrapped in a Secret may carry them as "\n".
var pemBody = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(.*?)-----END [A-Z ]*PRIVATE KEY-----`)

// parsePrivateKey accepts a PKCS#1 or PKCS#8 RSA key in PEM, tolerating the
// newline damage a form field or a YAML string inflicts on it.
func parsePrivateKey(s string) (*rsa.PrivateKey, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), `\n`, "\n")
	if s == "" {
		return nil, errors.New("github app: private_key is required")
	}
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return parseDER(block.Type, block.Bytes)
	}
	m := pemBody.FindStringSubmatch(strings.ReplaceAll(s, "\n", " "))
	if m == nil {
		return nil, errors.New("github app: private_key is not a PEM-encoded private key")
	}
	raw := strings.NewReplacer(" ", "", "\t", "", "\r", "").Replace(m[1])
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("github app: private_key is not valid PEM: %w", err)
	}
	return parseDER("", der)
}

func parseDER(blockType string, der []byte) (*rsa.PrivateKey, error) {
	if blockType != "PRIVATE KEY" {
		if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
			return key, nil
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("github app: parse private_key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app: private_key is not an RSA key; GitHub Apps use RSA")
	}
	return key, nil
}
