package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()
	want := User{
		Subject: "sub-1", Email: "ops@example.com", Name: "Ops",
		Expires: time.Now().Add(time.Hour).Truncate(time.Second),
	}
	raw, err := encodeSession(testKey, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSession(testKey, raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Subject != want.Subject || got.Email != want.Email {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	t.Parallel()
	raw, err := encodeSession(testKey, User{
		Subject: "sub-1", Email: "ops@example.com",
		Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, mac, _ := strings.Cut(raw, ".")

	tests := []struct {
		name  string
		value string
	}{
		{name: "no signature", value: body},
		{name: "wrong signature", value: body + ".AAAA"},
		{
			// The payload is base64 of JSON, so an attacker who could edit it
			// would be editing who they are. The signature is what stops that.
			name:  "edited payload",
			value: "eyJzdWIiOiJhZG1pbiIsImV4cCI6IjIwOTktMDEtMDFUMDA6MDA6MDBaIn0." + mac,
		},
		{name: "not base64", value: "!!!.???"},
		{name: "empty", value: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeSession(testKey, tt.value); err == nil {
				t.Error("a tampered session was accepted")
			}
		})
	}
}

func TestSessionRejectsAnotherKey(t *testing.T) {
	t.Parallel()
	raw, err := encodeSession(testKey, User{Subject: "s", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	other := []byte("ffffffffffffffffffffffffffffffff")
	if _, err := decodeSession(other, raw); err == nil {
		t.Error("a session signed with a different key was accepted")
	}
}

func TestSessionExpiryIsEnforcedServerSide(t *testing.T) {
	t.Parallel()
	// The cookie's own expiry is set by the client and cannot be trusted, so
	// the session carries its own and that is what is checked.
	raw, err := encodeSession(testKey, User{Subject: "s", Expires: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSession(testKey, raw); err == nil {
		t.Error("an expired session was accepted")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	err := setSession(rec, testKey, User{Subject: "s", Expires: time.Now().Add(time.Hour)}, true)
	if err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if !c.Secure {
		t.Error("Secure was requested but not set")
	}
	// Lax is what keeps the cookie off a cross-site POST, which is what stops
	// another site submitting the destroy form on an operator's behalf.
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
}

func TestUserDisplayPrefersEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		user User
		want string
	}{
		{User{Subject: "s", Email: "a@b.c", Name: "N"}, "a@b.c"},
		{User{Subject: "s", Name: "N"}, "N"},
		{User{Subject: "s"}, "s"},
	}
	for _, tt := range tests {
		if got := tt.user.Display(); got != tt.want {
			t.Errorf("Display() = %q, want %q", got, tt.want)
		}
	}
}

func TestSafeReturnPath(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"/pools":                   "/pools",
		"/clouds?driver=docker":    "/clouds?driver=docker",
		"//evil.example.com":       "/",
		"https://evil.example.com": "/",
		"pools":                    "/",
		"":                         "/",
	}
	for in, want := range tests {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllow(t *testing.T) {
	t.Parallel()
	yes, no := true, false

	tests := []struct {
		name     string
		cfg      Config
		email    string
		verified *bool
		wantErr  bool
	}{
		{
			// With no lists, the provider's own restrictions are the policy.
			name: "no lists allows anyone", cfg: Config{}, email: "a@b.c",
		},
		{
			name:  "email on the list",
			cfg:   Config{AllowedEmails: []string{"ops@example.com"}},
			email: "ops@example.com", verified: &yes,
		},
		{
			name:  "email match is case insensitive",
			cfg:   Config{AllowedEmails: []string{"Ops@Example.com"}},
			email: "ops@example.com", verified: &yes,
		},
		{
			name:  "domain on the list",
			cfg:   Config{AllowedDomains: []string{"example.com"}},
			email: "anyone@example.com", verified: &yes,
		},
		{
			name:  "domain written with an at sign",
			cfg:   Config{AllowedDomains: []string{"@example.com"}},
			email: "anyone@example.com", verified: &yes,
		},
		{
			name:  "not on any list",
			cfg:   Config{AllowedDomains: []string{"example.com"}},
			email: "someone@evil.com", verified: &yes, wantErr: true,
		},
		{
			// On some providers anyone can claim any address until it is
			// verified, so an unverified address must never satisfy a list.
			name:  "unverified address is refused",
			cfg:   Config{AllowedDomains: []string{"example.com"}},
			email: "anyone@example.com", verified: &no, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := &Authenticator{cfg: tt.cfg}
			err := a.allow(tt.email, tt.verified)
			if tt.wantErr && err == nil {
				t.Error("expected the sign-in to be refused")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

func TestDisabledAuthenticatorIsTransparent(t *testing.T) {
	t.Parallel()
	var a *Authenticator
	if a.Enabled() {
		t.Error("a nil authenticator should report disabled")
	}
	// Every method must tolerate the nil case: running ungated stays possible.
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pools", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("a disabled authenticator blocked a request: %d", rec.Code)
	}
	a.Routes(http.NewServeMux()) // must not panic
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("an empty config is valid (auth disabled), got %v", err)
	}
	if err := (Config{Issuer: "https://x"}).Validate(); err == nil {
		t.Error("an issuer with no client_id should be refused")
	}
	err := Config{Issuer: "https://x", ClientID: "c"}.Validate()
	if err == nil {
		t.Error("an issuer with no redirect_url should be refused")
	}
	ok := Config{Issuer: "https://x", ClientID: "c", RedirectURL: "https://y/auth/callback"}
	if err := ok.Validate(); err != nil {
		t.Errorf("a complete config was refused: %v", err)
	}
}
