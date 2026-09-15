package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ErrNotFound is returned by the lookup helpers when a row does not exist.
// Returning a sentinel rather than a nil value with a nil error keeps callers
// from having to distinguish "absent" from "zero" by inspection.
var ErrNotFound = errors.New("record not found")

// DB wraps the GORM handle with the queries the controller and UI need.
type DB struct {
	*gorm.DB
}

// Open connects to the database and runs migrations.
func Open(driver, dsn string) (*DB, error) {
	var dial gorm.Dialector
	switch driver {
	case "sqlite":
		// _busy_timeout and WAL keep the reconcile loop and the UI from
		// tripping over each other on a single file.
		if dsn != ":memory:" {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
		}
		dial = sqlite.Open(dsn)
	case "postgres":
		dial = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	db, err := gorm.Open(dial, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// Every timestamp is stored in UTC.
		//
		// GORM otherwise stamps rows in local time while the code that reads
		// them back works in UTC, and a range query then compares across two
		// offsets and silently matches nothing. That is how a cost total reads
		// as zero while machines are running.
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if driver == "sqlite" {
		// One writer avoids SQLITE_BUSY under concurrent reconciles.
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("open database handle: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db}, nil
}

// ---- clouds ----

// Clouds returns every cloud with its size and image catalogues loaded.
func (d *DB) Clouds(ctx context.Context) ([]Cloud, error) {
	var out []Cloud
	err := d.WithContext(ctx).Preload("Sizes").Preload("Images").Order("name").Find(&out).Error
	return out, err
}

// CloudByID returns one cloud with its catalogues.
func (d *DB) CloudByID(ctx context.Context, id uint) (*Cloud, error) {
	var c Cloud
	err := d.WithContext(ctx).Preload("Sizes").Preload("Images").First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ---- forges ----

// Forges returns every configured forge connection.
func (d *DB) Forges(ctx context.Context) ([]Forge, error) {
	var out []Forge
	err := d.WithContext(ctx).Order("name").Find(&out).Error
	return out, err
}

// ForgeByID returns one forge connection, or ErrNotFound.
func (d *DB) ForgeByID(ctx context.Context, id uint) (*Forge, error) {
	var f Forge
	err := d.WithContext(ctx).First(&f, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &f, err
}

// ---- pools ----

// Pools returns every pool with its forge, cloud, size and image resolved.
func (d *DB) Pools(ctx context.Context) ([]Pool, error) {
	var out []Pool
	err := d.WithContext(ctx).
		Preload("Forge").Preload("Cloud").Preload("Size").Preload("Image").
		Order("name").Find(&out).Error
	return out, err
}

// EnabledPools returns pools that are enabled and whose forge and cloud are too.
// A pool whose dependencies are disabled is skipped rather than failing.
func (d *DB) EnabledPools(ctx context.Context) ([]Pool, error) {
	all, err := d.Pools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Pool, 0, len(all))
	for _, p := range all {
		if !p.Enabled || p.Forge == nil || p.Cloud == nil || p.Size == nil {
			continue
		}
		if !p.Forge.Enabled || !p.Cloud.Enabled {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// PoolByID returns one pool with its forge, cloud, size and image resolved,
// or ErrNotFound.
func (d *DB) PoolByID(ctx context.Context, id uint) (*Pool, error) {
	var p Pool
	err := d.WithContext(ctx).
		Preload("Forge").Preload("Cloud").Preload("Size").Preload("Image").
		First(&p, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &p, err
}

// ---- webhook jobs ----

// RecordWebhookJob stores or refreshes a job announced by webhook.
func (d *DB) RecordWebhookJob(ctx context.Context, j *WebhookJob) error {
	var existing WebhookJob
	err := d.WithContext(ctx).Where("forge_id = ? AND job_id = ?", j.ForgeID, j.JobID).First(&existing).Error
	if err == nil {
		j.ID = existing.ID
		j.CheckedAt = nil
	}
	if err := d.WithContext(ctx).Save(j).Error; err != nil {
		return fmt.Errorf("record webhook job %s: %w", j.JobID, err)
	}
	return nil
}

// DeleteWebhookJob forgets a job; a job never recorded is not an error.
func (d *DB) DeleteWebhookJob(ctx context.Context, forgeID uint, jobID string) error {
	err := d.WithContext(ctx).Where("forge_id = ? AND job_id = ?", forgeID, jobID).Delete(&WebhookJob{}).Error
	if err != nil {
		return fmt.Errorf("delete webhook job %s: %w", jobID, err)
	}
	return nil
}

// WebhookJobs returns the jobs a forge has announced and not yet retired.
func (d *DB) WebhookJobs(ctx context.Context, forgeID uint) ([]WebhookJob, error) {
	var out []WebhookJob
	err := d.WithContext(ctx).Where("forge_id = ?", forgeID).Order("received_at").Find(&out).Error
	return out, err
}

// ForgeByName finds a forge by its unique name, which is what a webhook URL
// carries: names are stable across deployments in a way database ids are not.
func (d *DB) ForgeByName(ctx context.Context, name string) (*Forge, error) {
	var f Forge
	err := d.WithContext(ctx).Where("name = ?", name).First(&f).Error
	return &f, err
}

// ---- instances ----

// LiveInstances returns instances in a pool that still hold resources, i.e.
// everything that is not yet Deleted. This is the set the controller counts
// against MaxInstances and the set the reaper owes a destroy call for.
func (d *DB) LiveInstances(ctx context.Context, poolID uint) ([]Instance, error) {
	var out []Instance
	err := d.WithContext(ctx).
		Where("pool_id = ? AND state <> ?", poolID, StateDeleted).
		Order("created_at").Find(&out).Error
	return out, err
}

// ErrPoolBusy is returned by DeletePool while machines are still running: the
// reaper finds them by the pool name written on them, so the row has to
// outlive them.
var ErrPoolBusy = errors.New("machines are still running in this pool")

// DeletePool removes a pool together with the rows of the machines it ran.
//
// The machine rows have to go with it: they reference the pool and the
// database enforces that, so a pool that ever ran a job could otherwise never
// be deleted. What is lost is that pool's line in the cost history; the event
// log keeps its text. Refuses with ErrPoolBusy while any machine is live.
func (d *DB) DeletePool(ctx context.Context, id uint) error {
	live, err := d.LiveInstances(ctx, id)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("%w: %d machine(s); wait for them or destroy them first", ErrPoolBusy, len(live))
	}
	err = d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pool_id = ?", id).Delete(&Instance{}).Error; err != nil {
			return fmt.Errorf("machines: %w", err)
		}
		return tx.Delete(&Pool{}, id).Error
	})
	if err != nil {
		return fmt.Errorf("delete pool %d: %w", id, err)
	}
	return nil
}

// AllLiveInstances returns every non-deleted instance across all pools.
func (d *DB) AllLiveInstances(ctx context.Context) ([]Instance, error) {
	var out []Instance
	err := d.WithContext(ctx).Preload("Pool").
		Where("state <> ?", StateDeleted).Order("created_at").Find(&out).Error
	return out, err
}

// InstanceByName finds an instance by its unique machine name. Used to reconcile
// a machine discovered in the cloud back to the row that created it.
func (d *DB) InstanceByName(ctx context.Context, name string) (*Instance, error) {
	var i Instance
	err := d.WithContext(ctx).Where("name = ?", name).First(&i).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &i, err
}

// RecentInstances returns the most recent instances for the UI, including
// deleted ones so operators can see history.
func (d *DB) RecentInstances(ctx context.Context, limit int) ([]Instance, error) {
	var out []Instance
	err := d.WithContext(ctx).Preload("Pool").
		Order("created_at desc").Limit(limit).Find(&out).Error
	return out, err
}

// SetState moves an instance to a new state and stamps the matching timestamp.
func (d *DB) SetState(ctx context.Context, inst *Instance, s InstanceState) error {
	now := time.Now().UTC()
	inst.State = s
	switch s {
	case StateIdle:
		if inst.ReadyAt == nil {
			inst.ReadyAt = &now
		}
	case StateBusy:
		if inst.ClaimedAt == nil {
			inst.ClaimedAt = &now
		}
	case StateDraining:
		if inst.FinishedAt == nil {
			inst.FinishedAt = &now
		}
	case StateDeleted:
		if inst.DestroyedAt == nil {
			inst.DestroyedAt = &now
		}
	case StatePending, StateProvisioning, StateBooting, StateFailed:
		// No timestamp of their own: these are entry and error states, and
		// CreatedAt already records when the row appeared.
	}
	return d.WithContext(ctx).Save(inst).Error
}

// ---- events ----

// Logf records an audit event. Failures to log are swallowed: an audit write
// must never be the reason a teardown does not happen.
func (d *DB) Logf(ctx context.Context, level, kind string, poolID, instanceID *uint, format string, a ...any) {
	e := Event{
		At: time.Now().UTC(), Level: level, Kind: kind,
		PoolID: poolID, InstanceID: instanceID,
		Message: fmt.Sprintf(format, a...),
	}
	_ = d.WithContext(ctx).Create(&e).Error
}

// Events returns the most recent audit events.
func (d *DB) Events(ctx context.Context, limit int) ([]Event, error) {
	var out []Event
	err := d.WithContext(ctx).Order("at desc").Limit(limit).Find(&out).Error
	return out, err
}

// PruneEvents deletes audit events older than age, so the table does not grow
// without bound on a long-running deployment.
func (d *DB) PruneEvents(ctx context.Context, age time.Duration) error {
	cutoff := time.Now().UTC().Add(-age)
	return d.WithContext(ctx).Where("at < ?", cutoff).Delete(&Event{}).Error
}
