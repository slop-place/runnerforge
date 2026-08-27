package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("version output = %q, want %q", out.String(), version)
	}
}

func TestRunGenkeyProducesAUsableKey(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"genkey"}, &out); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(out.String())
	raw, err := hex.DecodeString(key)
	if err != nil {
		t.Fatalf("genkey did not produce hex: %v", err)
	}
	// The key encrypts credentials at rest; the wrong length would be rejected
	// at startup, which is a bad time to find out.
	if len(raw) != 32 {
		t.Errorf("key decodes to %d bytes, want 32", len(raw))
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		// The usage text has to say where clouds and forges are configured, or
		// an operator will look for them in the config file and not find them.
		body := out.String()
		for _, want := range []string{"serve", "genkey", "reap", "destroy-all", "web UI"} {
			if !strings.Contains(body, want) {
				t.Errorf("usage for %v does not mention %q", args, want)
			}
		}
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"frobnicate"}, &out)
	if err == nil {
		t.Fatal("expected an error for an unknown command")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error %q should name the unknown command", err)
	}
	// It should also show what the valid commands are.
	if !strings.Contains(err.Error(), "serve") {
		t.Error("an unknown command should be answered with the usage text")
	}
}

// writeConfig produces a valid bootstrap file in a temp directory.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	var key bytes.Buffer
	if err := run([]string{"genkey"}, &key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := "id: cli-test\nsecret_key: " + strings.TrimSpace(key.String()) + "\n" +
		"database:\n  driver: sqlite\n  dsn: " + filepath.Join(dir, "t.db") + "\n" + extra
	p := filepath.Join(dir, "runnerforge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReapWithNoCloudsIsANoOp(t *testing.T) {
	var out bytes.Buffer
	// A fresh deployment has nothing configured; reaping must succeed rather
	// than fail on an empty database.
	if err := run([]string{"reap", "-config", writeConfig(t, "")}, &out); err != nil {
		t.Errorf("reap on an empty deployment: %v", err)
	}
}

func TestDestroyAllWithNoCloudsReportsZero(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"destroy-all", "-config", writeConfig(t, "")}, &out); err != nil {
		t.Errorf("destroy-all on an empty deployment: %v", err)
	}
	if !strings.Contains(out.String(), "destroyed 0 machine(s)") {
		t.Errorf("output = %q", out.String())
	}
}

func TestSetupRejectsABadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p, []byte("id: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run([]string{"reap", "-config", p}, &out)
	if err == nil {
		t.Fatal("expected an error for a config with no secret_key")
	}
	if !strings.Contains(err.Error(), "secret_key") {
		t.Errorf("error %q should name the missing setting", err)
	}
}

func TestSetupRejectsAMissingConfig(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"reap", "-config", filepath.Join(t.TempDir(), "nope.yaml")}, &out)
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestUnknownFlagIsReported(t *testing.T) {
	var out bytes.Buffer
	// flag prints its own message to stderr; the error still has to propagate
	// so the process exits non-zero.
	if err := run([]string{"reap", "-nonsense"}, &out); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

func TestServeStartsAndShutsDownCleanly(t *testing.T) {
	cfgPath := writeConfig(t, "listen: \"127.0.0.1:0\"\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []string{"-config", cfgPath}) }()

	// Give it a moment to bind and start the controller, then ask it to stop.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// A clean shutdown must not look like a failure to whatever supervises
		// the process.
		if err != nil {
			t.Errorf("serve returned %v on a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}
}

func TestServeRejectsABadConfig(t *testing.T) {
	err := serve(t.Context(), []string{"-config", filepath.Join(t.TempDir(), "missing.yaml")})
	if err == nil {
		t.Error("expected an error for a missing config")
	}
}
