// Package auth signs operators in with OIDC and gates the web UI behind it.
//
// The UI holds cloud credentials and can destroy machines, so leaving it open
// is only reasonable on a network where everyone who can reach it is already
// trusted. Configure an issuer and it is gated; leave it unconfigured and
// runnerforge says so, loudly, at startup and on every page.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// sessionCookie is the name of the cookie holding a signed session.
const sessionCookie = "runnerforge_session"

// ErrNoSession means the request carried no usable session.
var ErrNoSession = errors.New("no session")

// User is who is signed in.
type User struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	// Expires is when the session stops being accepted, independent of the
	// cookie's own expiry, which a client controls.
	Expires time.Time `json:"exp"`
}

// Display is what to show in the UI.
func (u User) Display() string {
	switch {
	case u.Email != "":
		return u.Email
	case u.Name != "":
		return u.Name
	default:
		return u.Subject
	}
}

// sign returns the HMAC of a payload.
//
// The session is signed rather than encrypted: it carries no secret, only who
// the user is, and a signature is what stops that being forged.
func sign(key []byte, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return m.Sum(nil)
}

// encodeSession renders a user as a signed cookie value.
func encodeSession(key []byte, u User) (string, error) {
	payload, err := json.Marshal(u)
	if err != nil {
		return "", fmt.Errorf("encode session: %w", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := base64.RawURLEncoding.EncodeToString(sign(key, payload))
	return b64 + "." + mac, nil
}

// decodeSession verifies and parses a cookie value.
func decodeSession(key []byte, raw string) (User, error) {
	body, mac, ok := strings.Cut(raw, ".")
	if !ok {
		return User{}, ErrNoSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return User{}, ErrNoSession
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return User{}, ErrNoSession
	}
	// Constant time, so a wrong signature cannot be found a byte at a time.
	if !hmac.Equal(got, sign(key, payload)) {
		return User{}, fmt.Errorf("%w: bad signature", ErrNoSession)
	}
	var u User
	if err := json.Unmarshal(payload, &u); err != nil {
		return User{}, ErrNoSession
	}
	// The cookie's own expiry is set by the client and cannot be trusted, so
	// the session carries its own and that is what is enforced.
	if time.Now().After(u.Expires) {
		return User{}, fmt.Errorf("%w: expired", ErrNoSession)
	}
	return u, nil
}

// setSession writes the session cookie.
func setSession(w http.ResponseWriter, key []byte, u User, secure bool) error {
	v, err := encodeSession(key, u)
	if err != nil {
		return err
	}
	// Secure follows the deployment's scheme; see the note in oidc.go.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set from the deployment's scheme
		Name:     sessionCookie,
		Value:    v,
		Path:     "/",
		Expires:  u.Expires,
		HttpOnly: true,
		Secure:   secure,
		// Lax keeps the cookie off cross-site POSTs, which is what stops
		// another site submitting the destroy form on an operator's behalf.
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// clearSession removes the session cookie.
func clearSession(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the deployment's scheme
		Name: sessionCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
