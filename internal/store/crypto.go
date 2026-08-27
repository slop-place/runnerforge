package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// keyBytes is the AES-256 key length expected by SetKey.
const keyBytes = 32

// keyring holds the process-wide encryption key. Credentials for clouds and
// forges live in the database because the UI manages them, so they are encrypted
// at rest with a key that lives in the bootstrap config instead.
var keyring []byte

// SetKey installs the encryption key. Must be called once before any Secret is
// read or written.
func SetKey(k []byte) error {
	if len(k) != keyBytes {
		return fmt.Errorf("key must be %d bytes, got %d", keyBytes, len(k))
	}
	keyring = k
	return nil
}

// Secret is a map of credentials stored encrypted in a single column.
//
// the pointer; the mixed receivers are the interface contract, not an oversight.
//
// It behaves like a normal Go map in memory and as an opaque base64 blob in the
// database, so a leaked database dump does not leak API tokens. The values are
// only ever decrypted in-process.
//
//nolint:recvcheck // database/sql requires Value on the value type and Scan on
type Secret map[string]string

// Value encrypts the map for storage.
func (s Secret) Value() (driver.Value, error) {
	if len(s) == 0 {
		return "", nil
	}
	if keyring == nil {
		return nil, errors.New("store: encryption key not set")
	}
	plain, err := json.Marshal(map[string]string(s))
	if err != nil {
		return nil, fmt.Errorf("store: encode secret: %w", err)
	}
	block, err := aes.NewCipher(keyring)
	if err != nil {
		return nil, fmt.Errorf("store: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: build GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: generate nonce: %w", err)
	}
	// Nonce is prefixed to the ciphertext so each row carries its own.
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Scan decrypts a stored blob.
func (s *Secret) Scan(v any) error {
	*s = Secret{}
	var enc string
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		enc = t
	case []byte:
		enc = string(t)
	default:
		return fmt.Errorf("store: cannot scan %T into Secret", v)
	}
	if enc == "" {
		return nil
	}
	if keyring == nil {
		return errors.New("store: encryption key not set")
	}
	sealed, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return fmt.Errorf("store: secret is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(keyring)
	if err != nil {
		return fmt.Errorf("store: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("store: build GCM: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return errors.New("store: secret ciphertext is too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Almost always a changed secret_key rather than corruption; say so,
		// because the fix is different.
		return fmt.Errorf("store: cannot decrypt secret (wrong secret_key?): %w", err)
	}
	if err := json.Unmarshal(plain, (*map[string]string)(s)); err != nil {
		return fmt.Errorf("store: decode secret: %w", err)
	}
	return nil
}

// Redacted returns the secret's keys with values masked, for display and logs.
func (s Secret) Redacted() map[string]string {
	out := make(map[string]string, len(s))
	for k, v := range s {
		if v == "" {
			out[k] = ""
			continue
		}
		out[k] = "••••••••"
	}
	return out
}
