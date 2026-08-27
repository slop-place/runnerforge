package openstack

import (
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	"github.com/slop-place/runnerforge/internal/cloud"
)

func TestNewValidatesSettings(t *testing.T) {
	t.Parallel()
	// Missing settings must be reported together, so an operator fixes them in
	// one pass rather than one per attempt.
	_, err := cloud.New("openstack", map[string]any{"region": "r"})
	if err == nil {
		t.Fatal("expected an error for incomplete settings")
	}
	for _, want := range []string{"auth_url", "project_id", "username", "password"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should name the missing setting %q", err, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestFromServerStateMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status string
		want   cloud.State
	}{
		{status: "ACTIVE", want: cloud.StateRunning},
		{status: "active", want: cloud.StateRunning},
		{status: "BUILD", want: cloud.StateCreating},
		{status: "BUILDING", want: cloud.StateCreating},
		{status: "SHUTOFF", want: cloud.StateStopped},
		{status: "STOPPED", want: cloud.StateStopped},
		{status: "SUSPENDED", want: cloud.StateStopped},
		{status: "SHELVED_OFFLOADED", want: cloud.StateStopped},
		{status: "ERROR", want: cloud.StateError},
		{status: "DELETED", want: cloud.StateGone},
		{status: "SOFT_DELETED", want: cloud.StateGone},
		// A machine mid-transition must not be mistaken for finished work, or
		// the controller would tear it down during a resize.
		{status: "RESIZE", want: cloud.StateRunning},
		{status: "MIGRATING", want: cloud.StateRunning},
		{status: "SOMETHING_NEW", want: cloud.StateRunning},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()
			got := fromServer(&servers.Server{ID: "i", Name: "n", Status: tt.status})
			if got.State != tt.want {
				t.Errorf("status %q mapped to %q, want %q", tt.status, got.State, tt.want)
			}
		})
	}
}

func TestFromServerCarriesErrorDetail(t *testing.T) {
	t.Parallel()
	s := &servers.Server{ID: "i", Status: "ERROR"}
	s.Fault.Message = "no valid host was found"
	got := fromServer(s)
	if got.Err != "no valid host was found" {
		t.Errorf("Err = %q, want the fault message", got.Err)
	}

	// Even with no fault detail, an ERROR server must say something.
	got = fromServer(&servers.Server{ID: "i", Status: "ERROR"})
	if got.Err == "" {
		t.Error("an ERROR server should carry some explanation")
	}
}

func TestFromServerPreservesMetadata(t *testing.T) {
	t.Parallel()
	meta := map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "p"}
	got := fromServer(&servers.Server{ID: "i", Status: "ACTIVE", Metadata: meta})
	// The reaper claims machines by these tags, so they must survive the mapping.
	if !(cloud.Owner{Controller: "rf-1", Pool: "p"}).Matches(got.Tags) {
		t.Errorf("ownership tags were lost: %v", got.Tags)
	}
}

func TestAddresses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		addrs       map[string]any
		wantPublic  string
		wantPrivate string
	}{
		{
			name: "floating ip",
			addrs: map[string]any{"net": []any{
				map[string]any{"addr": "203.0.113.5", "OS-EXT-IPS:type": "floating"},
			}},
			wantPublic: "203.0.113.5",
		},
		{
			name: "private fixed ip",
			addrs: map[string]any{"net": []any{
				map[string]any{"addr": "10.0.0.4", "OS-EXT-IPS:type": "fixed"},
			}},
			wantPrivate: "10.0.0.4",
		},
		{
			// OVHcloud's Ext-Net hands out routable addresses as fixed IPs
			// rather than floating ones, so a routable fixed IP is public.
			name: "routable fixed ip counts as public",
			addrs: map[string]any{"Ext-Net": []any{
				map[string]any{"addr": "15.204.216.3", "OS-EXT-IPS:type": "fixed"},
			}},
			wantPublic: "15.204.216.3",
		},
		{
			name: "ipv6 is skipped",
			addrs: map[string]any{"net": []any{
				map[string]any{"addr": "2001:db8::1", "OS-EXT-IPS:type": "fixed"},
			}},
		},
		{
			name:  "no addresses",
			addrs: map[string]any{},
		},
		{
			name:  "malformed entries are ignored",
			addrs: map[string]any{"net": "not a list"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pub, priv := addresses(&servers.Server{Addresses: tt.addrs})
			if pub != tt.wantPublic {
				t.Errorf("public = %q, want %q", pub, tt.wantPublic)
			}
			if priv != tt.wantPrivate {
				t.Errorf("private = %q, want %q", priv, tt.wantPrivate)
			}
		})
	}
}

func TestIsPrivate(t *testing.T) {
	t.Parallel()
	private := []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "172.31.255.254"}
	public := []string{"15.204.216.3", "203.0.113.1", "8.8.8.8"}
	for _, ip := range private {
		if !isPrivate(ip) {
			t.Errorf("%s should be private", ip)
		}
	}
	for _, ip := range public {
		if isPrivate(ip) {
			t.Errorf("%s should be public", ip)
		}
	}
}

func TestLooksLikeUUID(t *testing.T) {
	t.Parallel()
	// A network can be configured as either an id or a name, so this is what
	// decides whether a Neutron lookup is needed.
	valid := []string{
		"22ef146b-0143-4d9b-9ca0-853e1c859990",
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
	}
	invalid := []string{
		"Ext-Net",
		"",
		"22ef146b0143-4d9b-9ca0-853e1c859990",   // dash in the wrong place
		"22ef146b-0143-4d9b-9ca0-853e1c85999",   // too short
		"22ef146b-0143-4d9b-9ca0-853e1c8599900", // too long
		"gggggggg-0143-4d9b-9ca0-853e1c859990",  // not hex
	}
	for _, s := range valid {
		if !looksLikeUUID(s) {
			t.Errorf("%q should look like a UUID", s)
		}
	}
	for _, s := range invalid {
		if looksLikeUUID(s) {
			t.Errorf("%q should not look like a UUID", s)
		}
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	caps := d.Capabilities()
	// VMs take user-data, not environment.
	if caps.CredentialMode != cloud.CredentialCloudInit {
		t.Errorf("CredentialMode = %q", caps.CredentialMode)
	}
	// Server metadata is what the reaper claims machines by.
	if !caps.Tags {
		t.Error("OpenStack supports server metadata and should report Tags")
	}
	if !caps.SecurityGroups || !caps.PublicIPv4 {
		t.Error("OpenStack supports security groups and public IPv4")
	}
	if caps.TypicalBoot < 10*time.Second {
		t.Error("TypicalBoot looks implausibly short for a VM")
	}
	if d.Name() != "openstack" {
		t.Errorf("Name = %q", d.Name())
	}
}

func TestProvisionRequiresFlavorAndImage(t *testing.T) {
	t.Parallel()
	d := &Driver{netIDs: map[string]string{}}

	_, err := d.Provision(t.Context(), cloud.ProvisionRequest{
		Name: "rf-a", ImageSpec: map[string]any{"id": "img"},
	})
	if err == nil || !contains(err.Error(), "flavor") {
		t.Errorf("expected a flavor error, got %v", err)
	}

	_, err = d.Provision(t.Context(), cloud.ProvisionRequest{
		Name: "rf-a", SizeSpec: map[string]any{"flavor": "c3-8"},
	})
	if err == nil || !contains(err.Error(), "image") {
		t.Errorf("expected an image error, got %v", err)
	}
}
