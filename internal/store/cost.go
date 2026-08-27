package store

import (
	"context"
	"time"
)

// minimumBilledSeconds is the floor most per-second clouds apply. OVHcloud
// meters to the second beyond an initial one-minute minimum, and a per-job
// machine lives close enough to that floor for the difference to matter.
const minimumBilledSeconds = 60

// BillableWindow returns when a machine started and stopped costing money.
//
// Billing starts when the provider reports the machine running, not when the
// row was created: time spent building is not charged, and counting it would
// overstate every figure on the dashboard. A machine that never came up costs
// nothing.
func (i *Instance) BillableWindow() (time.Time, time.Time, bool) {
	if i.ActiveAt == nil {
		return time.Time{}, time.Time{}, false
	}
	end := time.Now().UTC()
	if i.DestroyedAt != nil {
		end = *i.DestroyedAt
	}
	if end.Before(*i.ActiveAt) {
		return time.Time{}, time.Time{}, false
	}
	return *i.ActiveAt, end, true
}

// BilledDuration is how long a machine has been billable so far, with the
// provider's minimum applied.
func (i *Instance) BilledDuration() time.Duration {
	start, end, ok := i.BillableWindow()
	if !ok {
		return 0
	}
	return max(end.Sub(start), minimumBilledSeconds*time.Second)
}

// EstimatedCost is what this machine has cost so far.
//
// For a destroyed machine this is the recorded figure. For a live one it is
// computed from the clock, so the dashboard shows a bill that is still running
// rather than a zero that becomes a number later.
func (i *Instance) EstimatedCost() float64 {
	if i.CostUSD > 0 {
		return i.CostUSD
	}
	if i.HourlyUSD <= 0 {
		return 0
	}
	return i.HourlyUSD * i.BilledDuration().Hours()
}

// Settle records what a machine cost, at the moment it is destroyed.
func (i *Instance) Settle() {
	d := i.BilledDuration()
	if d <= 0 {
		return
	}
	i.BilledSeconds = int(d.Seconds())
	i.CostUSD = i.HourlyUSD * d.Hours()
}

// Spend is a cost total over some window.
type Spend struct {
	// USD is the total. Machines with no rate configured contribute nothing,
	// which is why Unpriced is reported alongside: a total of zero because
	// nobody set a rate should not read as a total of zero because nothing ran.
	USD      float64
	Machines int
	Unpriced int
}

// SpendSince totals what every machine created since a point in time has cost,
// including machines still running.
func (d *DB) SpendSince(ctx context.Context, since time.Time) (Spend, error) {
	var rows []Instance
	err := d.WithContext(ctx).Where("created_at >= ?", since).Find(&rows).Error
	if err != nil {
		return Spend{}, err
	}
	return totalSpend(rows), nil
}

// PoolSpendSince totals one pool's machines.
func (d *DB) PoolSpendSince(ctx context.Context, poolID uint, since time.Time) (Spend, error) {
	var rows []Instance
	err := d.WithContext(ctx).
		Where("pool_id = ? AND created_at >= ?", poolID, since).Find(&rows).Error
	if err != nil {
		return Spend{}, err
	}
	return totalSpend(rows), nil
}

func totalSpend(rows []Instance) Spend {
	var s Spend
	for i := range rows {
		// A machine that never became active never cost anything, so it is not
		// counted as unpriced either.
		if rows[i].ActiveAt == nil {
			continue
		}
		s.Machines++
		if rows[i].HourlyUSD <= 0 {
			s.Unpriced++
			continue
		}
		s.USD += rows[i].EstimatedCost()
	}
	return s
}
