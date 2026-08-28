// Package config loads runnerforge's bootstrap configuration.
//
// Deliberately small. Clouds, forges, pools, instance sizes and images are not
// configured here — they are records in the database, created and edited through
// the web UI. This file holds only what the process needs before it can open
// that database and serve that UI.
package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/slop-place/runnerforge/internal/auth"
	"github.com/slop-place/runnerforge/internal/k8s"
	"github.com/slop-place/runnerforge/internal/metrics"
)

// Config is the bootstrap file.
type Config struct {
	// ID is the stable identity of this deployment. It is written onto every
	// machine as an ownership tag, so two runnerforge deployments sharing a
	// cloud account must not share an ID or they will reap each other's
	// machines. Changing it orphans every machine created under the old value.
	ID string `yaml:"id"`

	// Listen is the address for the web UI and webhook receiver.
	Listen string `yaml:"listen"`

	// BaseURL is the externally reachable URL of this deployment. The UI uses it
	// to show operators the webhook endpoint to register with their forge.
	BaseURL string `yaml:"base_url"`

	Database Database `yaml:"database"`

	// SecretKey encrypts forge tokens and cloud credentials at rest, since those
	// live in the database rather than in a file the operator controls. It is a
	// 32-byte hex string; see GenerateSecretKey.
	SecretKey string `yaml:"secret_key"`

	// Kubernetes reconciles custom resources into this deployment. Enabling it
	// makes the cluster the source of truth for whatever it manages; anything
	// created in the UI or through the API is left alone.
	Kubernetes k8s.Config `yaml:"kubernetes"`

	// APITokens grant full control through the JSON API, which is how the
	// Terraform provider and the Kubernetes reconciler drive runnerforge. They
	// live here rather than in the database because they grant control over the
	// thing that would store them.
	APITokens []string `yaml:"api_tokens"`

	// OIDC gates the web UI behind an identity provider. Leaving the issuer
	// empty leaves the UI open, which runnerforge warns about at startup and on
	// every page — reasonable on a trusted network, not otherwise.
	OIDC auth.Config `yaml:"oidc"`

	// Metrics publishes a Prometheus endpoint. On unless turned off.
	Metrics metrics.Config `yaml:"metrics"`

	// ReconcileInterval is how often each pool is evaluated.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// ReapInterval is how often the reaper sweeps every cloud for stray machines.
	ReapInterval time.Duration `yaml:"reap_interval"`
}

// Database selects the state store.
type Database struct {
	// Driver is "sqlite" (default) or "postgres".
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// secretKeyBytes is the AES-256 key length the secret key must decode to.
const secretKeyBytes = 32

// Defaults.
const (
	DefaultListen            = ":8080"
	DefaultReconcileInterval = 10 * time.Second
	DefaultReapInterval      = 2 * time.Minute
)

// Load reads and validates the bootstrap file. ${VAR} references are expanded
// from the environment so secrets need not be written into the file.
func Load(path string) (*Config, error) {
	// The path comes from the operator's own command line, which is the whole
	// point of a config flag.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	expanded := os.Expand(string(raw), func(k string) string {
		if k == "$" {
			return "$"
		}
		return os.Getenv(k)
	})
	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the bootstrap settings.
func (c *Config) Validate() error {
	if c.ID == "" {
		return errors.New("id is required: it is the ownership tag written to every machine")
	}
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		return fmt.Errorf("unsupported database driver %q (want sqlite or postgres)", c.Database.Driver)
	}
	if c.SecretKey == "" {
		return errors.New("secret_key is required: it encrypts forge and cloud credentials at rest " +
			"(generate one with `runnerforge genkey`)")
	}
	if _, err := c.Key(); err != nil {
		return err
	}
	if err := c.OIDC.Validate(); err != nil {
		return err
	}
	return nil
}

// HasAPIToken reports whether a presented token is configured.
//
// The comparison is constant time so a valid token cannot be discovered a
// character at a time.
func (c *Config) HasAPIToken(presented string) bool {
	if presented == "" {
		return false
	}
	var ok bool
	for _, want := range c.APITokens {
		if subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1 {
			ok = true
		}
	}
	return ok
}

// Key decodes SecretKey into raw bytes.
func (c *Config) Key() ([]byte, error) {
	k, err := hex.DecodeString(c.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("secret_key is not valid hex: %w", err)
	}
	if len(k) != secretKeyBytes {
		return nil, fmt.Errorf("secret_key must decode to %d bytes, got %d", secretKeyBytes, len(k))
	}
	return k, nil
}

// GenerateSecretKey returns a new random key as hex.
func GenerateSecretKey() (string, error) {
	b := make([]byte, secretKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = DefaultReconcileInterval
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = DefaultReapInterval
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" {
		c.Database.DSN = "runnerforge.db"
	}
}
