package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runnerforge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func validKey(t *testing.T) string {
	t.Helper()
	k, err := config.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestLoadAppliesDefaults(t *testing.T) {
	p := writeConfig(t, "id: rf-1\nsecret_key: "+validKey(t)+"\n")
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Listen != config.DefaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, config.DefaultListen)
	}
	if c.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q, want sqlite", c.Database.Driver)
	}
	if c.ReconcileInterval != config.DefaultReconcileInterval {
		t.Errorf("ReconcileInterval = %s", c.ReconcileInterval)
	}
	if c.ReapInterval != config.DefaultReapInterval {
		t.Errorf("ReapInterval = %s", c.ReapInterval)
	}
}

func TestLoadExpandsEnvironment(t *testing.T) {
	key := validKey(t)
	t.Setenv("RF_TEST_KEY", key)
	p := writeConfig(t, "id: rf-1\nsecret_key: ${RF_TEST_KEY}\n")
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SecretKey != key {
		t.Error("secret_key was not expanded from the environment")
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	p := writeConfig(t, "id: rf-1\nsecret_key: "+validKey(t)+`
listen: "127.0.0.1:9999"
reconcile_interval: 45s
reap_interval: 7m
database:
  driver: postgres
  dsn: "host=db"
`)
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.ReconcileInterval != 45*time.Second {
		t.Errorf("ReconcileInterval = %s", c.ReconcileInterval)
	}
	if c.ReapInterval != 7*time.Minute {
		t.Errorf("ReapInterval = %s", c.ReapInterval)
	}
	if c.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q", c.Database.Driver)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	key := validKey(t)
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			// The id is the ownership tag written to every machine; without it
			// the reaper cannot tell its machines from anyone else's.
			name: "missing id",
			body: "secret_key: " + key + "\n",
			want: "id is required",
		},
		{
			name: "missing secret key",
			body: "id: rf-1\n",
			want: "secret_key is required",
		},
		{
			name: "secret key not hex",
			body: "id: rf-1\nsecret_key: nothexatall\n",
			want: "not valid hex",
		},
		{
			name: "secret key wrong length",
			body: "id: rf-1\nsecret_key: abcd\n",
			want: "must decode to 32 bytes",
		},
		{
			name: "unsupported database driver",
			body: "id: rf-1\nsecret_key: " + key + "\ndatabase:\n  driver: mysql\n",
			want: "unsupported database driver",
		},
		{
			name: "malformed yaml",
			body: "id: [rf-1\n",
			want: "parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestGenerateSecretKeyIsUsableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		k, err := config.GenerateSecretKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k] {
			t.Fatal("GenerateSecretKey returned a duplicate")
		}
		seen[k] = true

		c := &config.Config{ID: "x", SecretKey: k}
		raw, err := c.Key()
		if err != nil {
			t.Fatalf("generated key does not decode: %v", err)
		}
		if len(raw) != 32 {
			t.Fatalf("generated key decodes to %d bytes, want 32", len(raw))
		}
	}
}
