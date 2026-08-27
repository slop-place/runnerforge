package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/store"
)

func TestBilledDuration(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	tests := []struct {
		name string
		inst store.Instance
		want time.Duration
	}{
		{
			// A machine that never came up cost nothing, however long its row
			// sat there.
			name: "never became active",
			inst: store.Instance{CreatedAt: now.Add(-time.Hour)},
			want: 0,
		},
		{
			// Billing starts when the machine is running, not when the row was
			// created, so the boot time is not charged.
			name: "boot time is not billed",
			inst: store.Instance{
				CreatedAt:   now.Add(-10 * time.Minute),
				ActiveAt:    new(now.Add(-5 * time.Minute)),
				DestroyedAt: new(now),
			},
			want: 5 * time.Minute,
		},
		{
			// Most per-second clouds bill a one-minute floor, and a per-job
			// machine lives close enough to it for the difference to matter.
			name: "the provider minimum applies",
			inst: store.Instance{
				ActiveAt:    new(now.Add(-3 * time.Second)),
				DestroyedAt: new(now),
			},
			want: time.Minute,
		},
		{
			name: "a destroy timestamp before active is nonsense and bills nothing",
			inst: store.Instance{
				ActiveAt:    new(now),
				DestroyedAt: new(now.Add(-time.Hour)),
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.inst.BilledDuration(); got != tt.want {
				t.Errorf("BilledDuration = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestBilledDurationOfALiveMachineKeepsRunning(t *testing.T) {
	t.Parallel()
	// A machine still running has a bill that is still growing; showing zero
	// until it is destroyed would hide exactly the thing worth watching.
	inst := store.Instance{ActiveAt: new(time.Now().UTC().Add(-10 * time.Minute))}
	if got := inst.BilledDuration(); got < 9*time.Minute {
		t.Errorf("BilledDuration = %s, want roughly 10m", got)
	}
}

func TestEstimatedCost(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	t.Run("computed from the rate and the clock", func(t *testing.T) {
		t.Parallel()
		inst := store.Instance{
			HourlyUSD:   1.0,
			ActiveAt:    new(now.Add(-30 * time.Minute)),
			DestroyedAt: new(now),
		}
		got := inst.EstimatedCost()
		if got < 0.49 || got > 0.51 {
			t.Errorf("EstimatedCost = %v, want about 0.50", got)
		}
	})

	t.Run("a settled cost is not recomputed", func(t *testing.T) {
		t.Parallel()
		// Once recorded, the figure is history. Recomputing it from an edited
		// rate would rewrite a bill that already happened.
		inst := store.Instance{
			CostUSD: 0.25, HourlyUSD: 99,
			ActiveAt: new(now.Add(-time.Hour)), DestroyedAt: new(now),
		}
		if got := inst.EstimatedCost(); got != 0.25 {
			t.Errorf("EstimatedCost = %v, want the recorded 0.25", got)
		}
	})

	t.Run("an unpriced machine costs nothing", func(t *testing.T) {
		t.Parallel()
		inst := store.Instance{ActiveAt: new(now.Add(-time.Hour)), DestroyedAt: new(now)}
		if got := inst.EstimatedCost(); got != 0 {
			t.Errorf("EstimatedCost = %v, want 0 when no rate is set", got)
		}
	})
}

func TestSettle(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inst := store.Instance{
		HourlyUSD:   0.06,
		ActiveAt:    new(now.Add(-2 * time.Minute)),
		DestroyedAt: new(now),
	}
	inst.Settle()

	if inst.BilledSeconds != 120 {
		t.Errorf("BilledSeconds = %d, want 120", inst.BilledSeconds)
	}
	want := 0.06 * 2 / 60
	if diff := inst.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %v, want %v", inst.CostUSD, want)
	}

	// Settling a machine that never ran must not invent a charge.
	never := store.Instance{HourlyUSD: 1}
	never.Settle()
	if never.CostUSD != 0 || never.BilledSeconds != 0 {
		t.Errorf("a machine that never ran was billed: %+v", never)
	}
}

func TestSpendSince(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	pool := seedPool(t, db)
	now := time.Now().UTC()

	rows := []store.Instance{
		// Two settled machines with a rate.
		{Name: "rf-a", PoolID: pool.ID, State: store.StateDeleted, HourlyUSD: 0.06,
			ActiveAt: new(now.Add(-time.Hour)), DestroyedAt: new(now.Add(-30 * time.Minute)),
			CostUSD: 0.03},
		{Name: "rf-b", PoolID: pool.ID, State: store.StateDeleted, HourlyUSD: 0.06,
			ActiveAt: new(now.Add(-time.Hour)), DestroyedAt: new(now.Add(-30 * time.Minute)),
			CostUSD: 0.03},
		// One that ran but was never priced.
		{Name: "rf-c", PoolID: pool.ID, State: store.StateDeleted,
			ActiveAt: new(now.Add(-time.Hour)), DestroyedAt: new(now)},
		// One that never came up, so it is neither counted nor unpriced.
		{Name: "rf-d", PoolID: pool.ID, State: store.StateFailed},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.SpendSince(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if diff := got.USD - 0.06; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("USD = %v, want 0.06", got.USD)
	}
	if got.Machines != 3 {
		t.Errorf("Machines = %d, want the 3 that actually ran", got.Machines)
	}
	// A total of zero because nobody set a rate must not read the same as a
	// total of zero because nothing ran, so unpriced machines are reported.
	if got.Unpriced != 1 {
		t.Errorf("Unpriced = %d, want 1", got.Unpriced)
	}

	// A window that excludes everything totals nothing.
	empty, err := db.SpendSince(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Machines != 0 || empty.USD != 0 {
		t.Errorf("a future window returned %+v", empty)
	}

	perPool, err := db.PoolSpendSince(ctx, pool.ID, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if perPool.Machines != 3 {
		t.Errorf("PoolSpendSince Machines = %d, want 3", perPool.Machines)
	}
	other, err := db.PoolSpendSince(ctx, pool.ID+999, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if other.Machines != 0 {
		t.Errorf("another pool's spend leaked in: %+v", other)
	}
}

// TestTimestampsAreStoredInUTC is a regression test.
//
// GORM stamps rows in local time by default, while the code that reads them
// back works in UTC. A range query then compares across two offsets and
// silently matches nothing — which is how a cost total reads as zero while
// machines are running.
func TestTimestampsAreStoredInUTC(t *testing.T) {
	db := newDB(t)
	pool := seedPool(t, db)

	inst := &store.Instance{Name: "rf-utc", PoolID: pool.ID, State: store.StateIdle}
	if err := db.Create(inst).Error; err != nil {
		t.Fatal(err)
	}

	var back store.Instance
	if err := db.First(&back, inst.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, offset := back.CreatedAt.Zone(); offset != 0 {
		t.Errorf("created_at came back with a %d second offset; it should be UTC", offset)
	}

	// The behaviour that actually breaks: a UTC range query must find it.
	got, err := db.SpendSince(context.Background(), time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = got
	var found []store.Instance
	if err := db.Where("created_at >= ?", time.Now().UTC().Add(-time.Minute)).
		Find(&found).Error; err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("a UTC range query found %d rows, want 1", len(found))
	}
}
