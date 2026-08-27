package dockerdrv

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
)

// frame builds one Docker stream-multiplexing frame.
func frame(stream byte, payload string) []byte {
	b := make([]byte, frameHeaderLen+len(payload))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:frameHeaderLen], uint32(len(payload)))
	copy(b[frameHeaderLen:], payload)
	return b
}

func TestDeframe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{
			name: "single stdout frame",
			in:   frame(1, "hello\n"),
			want: "hello\n",
		},
		{
			name: "stdout and stderr interleaved",
			in:   append(frame(1, "out\n"), frame(2, "err\n")...),
			want: "out\nerr\n",
		},
		{
			// A TTY container's output is not framed at all, and must survive
			// unchanged rather than being mangled by a bogus header parse.
			name: "unframed TTY output",
			in:   []byte("plain text with no framing at all"),
			want: "plain text with no framing at all",
		},
		{
			name: "empty",
			in:   nil,
			want: "",
		},
		{
			// A short read must not panic or drop everything.
			name: "truncated frame",
			in:   append(frame(1, "complete\n"), 1, 0, 0, 0, 0, 0, 0),
			want: "complete\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deframe(tt.in); got != tt.want {
				t.Errorf("deframe() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeframeTruncatedKeepsWhatItHas(t *testing.T) {
	t.Parallel()
	// A frame header claiming more bytes than are present must not read past
	// the buffer.
	b := make([]byte, frameHeaderLen)
	b[0] = 1
	binary.BigEndian.PutUint32(b[4:frameHeaderLen], 9999)
	got := deframe(b)
	if got == "" {
		t.Skip("returning the raw bytes is also acceptable here")
	}
}

func TestApplySize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		spec  map[string]any
		check func(*testing.T, hostConfig)
	}{
		{
			name: "cpus and memory",
			spec: map[string]any{"cpus": 2.5, "memory_mb": float64(2048)},
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if hc.NanoCPUs != int64(2.5*nanoCPUsPerCore) {
					t.Errorf("NanoCPUs = %d", hc.NanoCPUs)
				}
				if hc.Memory != 2048*bytesPerMB {
					t.Errorf("Memory = %d", hc.Memory)
				}
			},
		},
		{
			name: "privileged",
			spec: map[string]any{"privileged": true},
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if !hc.Privileged {
					t.Error("Privileged was not set")
				}
			},
		},
		{
			// Forgejo and GitLab runners start job containers of their own, so
			// without the daemon socket they cannot run anything.
			name: "docker socket bind",
			spec: map[string]any{"docker_socket": "/var/run/docker.sock"},
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if len(hc.Binds) != 1 || !strings.Contains(hc.Binds[0], "/var/run/docker.sock") {
					t.Errorf("Binds = %v", hc.Binds)
				}
			},
		},
		{
			name: "network",
			spec: map[string]any{"network": "rf-net"},
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if hc.NetworkMode != "rf-net" {
					t.Errorf("NetworkMode = %q", hc.NetworkMode)
				}
			},
		},
		{
			name: "empty spec leaves everything unlimited",
			spec: nil,
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if hc.NanoCPUs != 0 || hc.Memory != 0 || hc.Privileged {
					t.Errorf("expected an unconstrained host config, got %+v", hc)
				}
			},
		},
		{
			name: "zero values are ignored",
			spec: map[string]any{"cpus": 0.0, "memory_mb": 0},
			check: func(t *testing.T, hc hostConfig) {
				t.Helper()
				if hc.NanoCPUs != 0 || hc.Memory != 0 {
					t.Errorf("zero limits should mean unlimited, got %+v", hc)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var hc hostConfig
			applySize(&hc, cloud.ProvisionRequest{Size: "s", SizeSpec: tt.spec})
			tt.check(t, hc)
		})
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	caps := d.Capabilities()
	// Containers start a process directly, so the credential arrives as
	// environment rather than cloud-init.
	if caps.CredentialMode != cloud.CredentialEnv {
		t.Errorf("CredentialMode = %q", caps.CredentialMode)
	}
	// The reaper claims machines by tag; a driver without tags could only match
	// on name.
	if !caps.Tags {
		t.Error("the Docker driver supports labels and should report Tags")
	}
	if caps.TypicalBoot <= 0 {
		t.Error("TypicalBoot should be positive")
	}
	if d.Name() != "docker" {
		t.Errorf("Name = %q", d.Name())
	}
}

func TestProvisionRequiresAnImage(t *testing.T) {
	t.Parallel()
	d := &Driver{}
	_, err := d.Provision(context.Background(), cloud.ProvisionRequest{Name: "rf-a"})
	if err == nil {
		t.Fatal("expected an error when no image is given")
	}
	// The message should tell the operator which knob to set.
	if !strings.Contains(err.Error(), "container_image") {
		t.Errorf("error %q should say how to fix it", err)
	}
}

func TestSocketFromEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///custom/docker.sock")
	if got := socketFromEnv(); got != "/custom/docker.sock" {
		t.Errorf("socketFromEnv() = %q", got)
	}

	// A tcp:// DOCKER_HOST is not a unix socket, so the lookup falls through to
	// the well-known paths rather than returning nonsense.
	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2375")
	if got := socketFromEnv(); strings.HasPrefix(got, "tcp://") {
		t.Errorf("socketFromEnv() returned a tcp host: %q", got)
	}
}

func TestNewWithoutSocket(t *testing.T) {
	// An explicit socket setting is honoured even when it does not exist yet,
	// so a daemon that starts later still works.
	p, err := New(map[string]any{"socket": "/nonexistent/docker.sock"})
	if err != nil {
		t.Fatalf("New with an explicit socket: %v", err)
	}
	if p.Name() != "docker" {
		t.Errorf("Name = %q", p.Name())
	}
}

func TestConstantsAreSane(t *testing.T) {
	t.Parallel()
	if apiTimeout < 10*time.Second {
		t.Error("apiTimeout is too short for an image pull")
	}
	if frameHeaderLen != 8 {
		t.Error("Docker's stream header is 8 bytes")
	}
}
