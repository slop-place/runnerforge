package cloud_test

import (
	"testing"

	"github.com/slop-place/runnerforge/internal/cloud"
)

func TestOwnerMatches(t *testing.T) {
	t.Parallel()
	owner := cloud.Owner{Controller: "rf-1", Pool: "small"}

	tests := []struct {
		name  string
		owner cloud.Owner
		tags  map[string]string
		want  bool
	}{
		{
			name:  "exact match",
			owner: owner,
			tags:  map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "small"},
			want:  true,
		},
		{
			name:  "different pool",
			owner: owner,
			tags:  map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "large"},
			want:  false,
		},
		{
			// The guard that stops two deployments sharing a cloud account from
			// reaping each other's machines.
			name:  "different controller",
			owner: owner,
			tags:  map[string]string{cloud.TagController: "rf-2", cloud.TagPool: "small"},
			want:  false,
		},
		{
			// An empty pool is the reaper's controller-wide sweep, which must
			// find machines whose pool has since been deleted.
			name:  "empty pool matches any pool",
			owner: cloud.Owner{Controller: "rf-1"},
			tags:  map[string]string{cloud.TagController: "rf-1", cloud.TagPool: "anything"},
			want:  true,
		},
		{
			name:  "no tags at all",
			owner: owner,
			tags:  nil,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.owner.Matches(tt.tags); got != tt.want {
				t.Errorf("Matches(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestOwnerTags(t *testing.T) {
	t.Parallel()
	got := cloud.Owner{Controller: "rf-1", Pool: "p"}.Tags()
	if got[cloud.TagController] != "rf-1" || got[cloud.TagPool] != "p" {
		t.Fatalf("Tags() = %v", got)
	}
}

func TestSpecHelpers(t *testing.T) {
	t.Parallel()
	// JSON round-trips every number as float64, which is how specs arrive from
	// the database, so the int helpers must accept that.
	spec := map[string]any{
		"flavor":    "c3-8",
		"memory_mb": float64(4096),
		"count":     7,
		"cpus":      2.5,
		"priv":      true,
	}

	if got := cloud.SpecString(spec, "flavor"); got != "c3-8" {
		t.Errorf("SpecString = %q", got)
	}
	if got := cloud.SpecString(spec, "missing"); got != "" {
		t.Errorf("SpecString(missing) = %q, want empty", got)
	}
	if got := cloud.SpecString(nil, "flavor"); got != "" {
		t.Errorf("SpecString(nil) = %q, want empty", got)
	}

	if n, ok := cloud.SpecInt(spec, "memory_mb"); !ok || n != 4096 {
		t.Errorf("SpecInt(float64) = %d, %v", n, ok)
	}
	if n, ok := cloud.SpecInt(spec, "count"); !ok || n != 7 {
		t.Errorf("SpecInt(int) = %d, %v", n, ok)
	}
	if _, ok := cloud.SpecInt(spec, "flavor"); ok {
		t.Error("SpecInt on a string should report not-ok")
	}

	if f, ok := cloud.SpecFloat(spec, "cpus"); !ok || f != 2.5 {
		t.Errorf("SpecFloat = %v, %v", f, ok)
	}
	if f, ok := cloud.SpecFloat(spec, "count"); !ok || f != 7 {
		t.Errorf("SpecFloat(int) = %v, %v", f, ok)
	}

	if !cloud.SpecBool(spec, "priv") {
		t.Error("SpecBool = false, want true")
	}
	if cloud.SpecBool(spec, "missing") {
		t.Error("SpecBool(missing) = true, want false")
	}
}

func TestNewUnknownDriver(t *testing.T) {
	t.Parallel()
	if _, err := cloud.New("nope", nil); err == nil {
		t.Fatal("expected an error for an unregistered driver")
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	// The registries are written at init and read afterwards, so production is
	// safe by construction. Guarding them anyway means the API is safe for any
	// caller rather than safe only by convention — this is the test that says
	// so, and it fails under -race without the lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = cloud.DriverNames()
			_ = cloud.HasDriver("docker")
		}
	}()
	for range 200 {
		_, _ = cloud.New("definitely-not-registered", nil)
	}
	<-done
}

func TestDriverNamesIsSorted(t *testing.T) {
	t.Parallel()
	names := cloud.DriverNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("DriverNames is not sorted: %v", names)
		}
	}
}
