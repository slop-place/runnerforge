package openstack_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	_ "github.com/slop-place/runnerforge/internal/cloud/openstack"
)

// TestOpenStackLifecycle provisions a real machine on a real OpenStack cloud,
// waits for it to boot, confirms it can be found by its ownership tags, and
// destroys it.
//
// This costs actual money, so it is skipped unless the RF_TEST_OS_* variables
// are set, and it is written to leave nothing behind even when it fails: the
// final check asserts the account holds none of our machines.
//
// Point it at a disposable project with a low instance quota. Never a shared one.
func TestOpenStackLifecycle(t *testing.T) {
	cfg := map[string]any{}
	for _, k := range []string{"auth_url", "region", "project_id", "username", "password", "network"} {
		if v := os.Getenv("RF_TEST_OS_" + strings.ToUpper(k)); v != "" {
			cfg[k] = v
		}
	}
	for _, k := range []string{"auth_url", "region", "project_id", "username", "password"} {
		if cfg[k] == nil {
			t.Skipf("RF_TEST_OS_%s not set; skipping the real-cloud test", strings.ToUpper(k))
		}
	}
	flavor := os.Getenv("RF_TEST_OS_FLAVOR")
	image := os.Getenv("RF_TEST_OS_IMAGE")
	if flavor == "" || image == "" {
		t.Skip("RF_TEST_OS_FLAVOR and RF_TEST_OS_IMAGE are required")
	}

	prov, err := cloud.New("openstack", cfg)
	if err != nil {
		t.Fatalf("build driver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	owner := cloud.Owner{
		Controller: "rf-integration-" + time.Now().UTC().Format("20060102150405"),
		Pool:       "itest",
	}
	name := "rf-itest-" + time.Now().UTC().Format("150405")

	// Registered before the machine exists, so an early failure still cleans up.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		left, err := prov.List(c, owner)
		if err != nil {
			t.Errorf("cleanup list: %v", err)
			return
		}
		for _, m := range left {
			t.Logf("cleanup: destroying %s", m.Name)
			if err := prov.Delete(c, m.ID); err != nil {
				t.Errorf("cleanup delete %s: %v", m.Name, err)
			}
		}
	})

	// A machine that shuts itself down is the last line of defence if this test
	// dies before its cleanup runs.
	userData := []byte("#cloud-config\nruncmd:\n  - [ sh, -c, \"shutdown -h +10\" ]\n")

	created, err := prov.Provision(ctx, cloud.ProvisionRequest{
		Name:      name,
		Owner:     owner,
		Size:      "itest",
		SizeSpec:  map[string]any{"flavor": flavor},
		Image:     "itest",
		ImageSpec: map[string]any{"id": image},
		Bootstrap: cloud.Bootstrap{CloudInit: userData},
		Network:   cloud.NetworkSpec{PublicIPv4: true},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("provisioned %s (%s)", created.Name, created.ID)

	// List must find it by tag even while it is still building; the reaper
	// depends on that being true from the moment the machine exists.
	found, err := prov.List(ctx, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen bool
	for _, m := range found {
		if m.ID == created.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("List did not return the machine we just created; the reaper would not find it either")
	}

	// A different controller id must not match, or two deployments sharing an
	// account would destroy each other's machines.
	others, err := prov.List(ctx, cloud.Owner{Controller: owner.Controller + "-other"})
	if err != nil {
		t.Fatalf("list for a different owner: %v", err)
	}
	for _, m := range others {
		if m.ID == created.ID {
			t.Fatal("a different controller id matched our machine; ownership tagging is broken")
		}
	}

	// Wait for boot. OVHcloud typically reaches ACTIVE in well under a minute.
	deadline := time.Now().Add(5 * time.Minute)
	var active bool
	for time.Now().Before(deadline) {
		got, err := prov.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.State == cloud.StateError {
			t.Fatalf("machine entered an error state: %s", got.Err)
		}
		if got.State == cloud.StateRunning {
			active = true
			t.Logf("machine active after %s, public IP %q",
				time.Since(created.CreatedAt).Round(time.Second), got.PublicIP)
			if got.PublicIP == "" {
				t.Error("machine is running but reported no public IPv4")
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !active {
		t.Fatal("machine never reached a running state")
	}

	if err := prov.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Deleting twice must be a no-op, since teardown is retried.
	if err := prov.Delete(ctx, created.ID); err != nil {
		t.Errorf("second delete should be a no-op, got: %v", err)
	}

	// The assertion that matters: nothing of ours survives.
	gone := false
	for range 30 {
		left, err := prov.List(ctx, owner)
		if err != nil {
			t.Fatalf("final list: %v", err)
		}
		if len(left) == 0 {
			gone = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !gone {
		t.Error("machines still present after delete; this would be a leak")
	}
}
