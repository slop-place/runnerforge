package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slop-place/runnerforge/internal/store"
)

func newDB(t *testing.T) *store.DB {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKey(key); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func TestSecretRoundTrip(t *testing.T) {
	newDB(t) // installs a key

	original := store.Secret{"token": "s3cr3t", "user": "admin"}
	encoded, err := original.Value()
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// The stored form must not contain the plaintext, or a database dump would
	// leak every credential.
	blob, ok := encoded.(string)
	if !ok {
		t.Fatalf("Value returned %T, want string", encoded)
	}
	if strings.Contains(blob, "s3cr3t") {
		t.Fatal("the encrypted column still contains the plaintext secret")
	}

	var decoded store.Secret
	if err := decoded.Scan(blob); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decoded["token"] != "s3cr3t" || decoded["user"] != "admin" {
		t.Fatalf("round trip lost data: %v", decoded)
	}
}

func TestSecretEncryptionIsNondeterministic(t *testing.T) {
	newDB(t)
	s := store.Secret{"token": "same"}
	a, err := s.Value()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Value()
	if err != nil {
		t.Fatal(err)
	}
	// A fresh nonce per write means equal plaintexts do not produce equal
	// ciphertexts, so an observer cannot tell two rows hold the same token.
	if a == b {
		t.Error("two encryptions of the same secret produced identical ciphertext")
	}
}

func TestSecretScanRejectsWrongKey(t *testing.T) {
	newDB(t)
	blob, err := store.Secret{"token": "x"}.Value()
	if err != nil {
		t.Fatal(err)
	}

	// Rotate to a different key, as if secret_key had been changed.
	other := make([]byte, 32)
	other[0] = 1
	if err := store.SetKey(other); err != nil {
		t.Fatal(err)
	}
	var s store.Secret
	err = s.Scan(blob)
	if err == nil {
		t.Fatal("decrypting with the wrong key should fail")
	}
	// The message should point at the likely cause, since the fix differs from
	// the fix for genuine corruption.
	if !strings.Contains(err.Error(), "secret_key") {
		t.Errorf("error %q should mention secret_key", err)
	}
}

func TestSecretScanEdgeCases(t *testing.T) {
	newDB(t)
	tests := []struct {
		name    string
		input   any
		wantErr bool
	}{
		{name: "nil", input: nil},
		{name: "empty string", input: ""},
		{name: "empty bytes", input: []byte{}},
		{name: "not base64", input: "!!!not base64!!!", wantErr: true},
		{name: "too short", input: "YWJj", wantErr: true},
		{name: "wrong type", input: 42, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s store.Secret
			err := s.Scan(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestSecretRedacted(t *testing.T) {
	s := store.Secret{"token": "supersecret", "blank": ""}
	got := s.Redacted()
	if got["token"] == "supersecret" {
		t.Error("Redacted leaked the value")
	}
	if got["token"] == "" {
		t.Error("Redacted should still show that a value is present")
	}
	if got["blank"] != "" {
		t.Error("an empty value should stay empty")
	}
}

func TestStringListRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		list store.StringList
	}{
		{name: "values", list: store.StringList{"self-hosted", "linux", "x64"}},
		{name: "empty", list: store.StringList{}},
		{name: "nil", list: nil},
		{name: "awkward characters", list: store.StringList{`a"b`, "c,d", "e\nf"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := tt.list.Value()
			if err != nil {
				t.Fatal(err)
			}
			var got store.StringList
			if err := got.Scan(v); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.list) {
				t.Fatalf("length %d, want %d", len(got), len(tt.list))
			}
			for i := range got {
				if got[i] != tt.list[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.list[i])
				}
			}
		})
	}
}

func TestParamsRoundTripAndAccessors(t *testing.T) {
	p := store.Params{"flavor": "c3-8", "count": 4, "on": true}
	v, err := p.Value()
	if err != nil {
		t.Fatal(err)
	}
	var got store.Params
	if err := got.Scan(v); err != nil {
		t.Fatal(err)
	}
	if got.String("flavor") != "c3-8" {
		t.Errorf("String(flavor) = %q", got.String("flavor"))
	}
	if got.String("count") != "" {
		t.Error("String on a non-string should return empty")
	}
	if got.String("missing") != "" {
		t.Error("String on a missing key should return empty")
	}
}

func TestParamsScanRejectsGarbage(t *testing.T) {
	var p store.Params
	if err := p.Scan("{not json"); err == nil {
		t.Error("expected an error for malformed JSON")
	}
	if err := p.Scan(12345); err == nil {
		t.Error("expected an error for an unsupported type")
	}
}

func TestLookupsReturnErrNotFound(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	if _, err := db.CloudByID(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CloudByID error = %v, want ErrNotFound", err)
	}
	if _, err := db.ForgeByID(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ForgeByID error = %v, want ErrNotFound", err)
	}
	if _, err := db.PoolByID(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PoolByID error = %v, want ErrNotFound", err)
	}
	if _, err := db.InstanceByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("InstanceByName error = %v, want ErrNotFound", err)
	}
}

func TestSetStateStampsTimestamps(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	pool := seedPool(t, db)

	inst := &store.Instance{Name: "rf-a", PoolID: pool.ID, State: store.StatePending}
	if err := db.Create(inst).Error; err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		state store.InstanceState
		check func() *time.Time
		field string
	}{
		{store.StateIdle, func() *time.Time { return inst.ReadyAt }, "ReadyAt"},
		{store.StateBusy, func() *time.Time { return inst.ClaimedAt }, "ClaimedAt"},
		{store.StateDraining, func() *time.Time { return inst.FinishedAt }, "FinishedAt"},
		{store.StateDeleted, func() *time.Time { return inst.DestroyedAt }, "DestroyedAt"},
	}
	for _, s := range steps {
		if err := db.SetState(ctx, inst, s.state); err != nil {
			t.Fatalf("SetState(%s): %v", s.state, err)
		}
		if s.check() == nil {
			t.Errorf("%s was not stamped on transition to %s", s.field, s.state)
		}
	}

	// Re-entering a state must not move the original timestamp: it records when
	// the machine first got there.
	first := *inst.ReadyAt
	if err := db.SetState(ctx, inst, store.StateIdle); err != nil {
		t.Fatal(err)
	}
	if !inst.ReadyAt.Equal(first) {
		t.Error("ReadyAt moved on a repeat transition")
	}
}

func TestLiveInstancesExcludesDeleted(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	pool := seedPool(t, db)

	for _, st := range []store.InstanceState{
		store.StateIdle, store.StateBusy, store.StateFailed, store.StateDeleted,
	} {
		inst := &store.Instance{Name: "rf-" + string(st), PoolID: pool.ID, State: st}
		if err := db.Create(inst).Error; err != nil {
			t.Fatal(err)
		}
	}

	live, err := db.LiveInstances(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Failed still holds a machine, so it counts as live; deleted does not.
	if len(live) != 3 {
		t.Fatalf("LiveInstances returned %d, want 3", len(live))
	}
	for _, in := range live {
		if in.State == store.StateDeleted {
			t.Error("LiveInstances returned a deleted instance")
		}
	}
}

func TestEnabledPoolsSkipsDisabledDependencies(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	cl := &store.Cloud{Name: "c", Driver: "fake", Enabled: true}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{CloudID: cl.ID, Name: "s"}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{Name: "f", Kind: "fake", Enabled: false} // disabled
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "p", Enabled: true, ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID,
		Labels: store.StringList{"linux"},
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}

	got, err := db.EnabledPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A pool whose forge is switched off is skipped rather than failing the
	// whole reconcile pass.
	if len(got) != 0 {
		t.Errorf("EnabledPools returned %d pools, want 0 when the forge is disabled", len(got))
	}
}

func TestEventsAndPruning(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	db.Logf(ctx, "info", "test", nil, nil, "hello %s", "world")
	evs, err := db.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Message != "hello world" {
		t.Fatalf("Events = %+v", evs)
	}

	// Nothing is old enough to prune yet.
	if err := db.PruneEvents(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if evs, _ := db.Events(ctx, 10); len(evs) != 1 {
		t.Error("PruneEvents removed a recent event")
	}

	// Everything older than zero is everything.
	if err := db.PruneEvents(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if evs, _ := db.Events(ctx, 10); len(evs) != 0 {
		t.Error("PruneEvents left old events behind")
	}
}

func TestPoolDurationHelpers(t *testing.T) {
	p := &store.Pool{JobTimeoutSec: 90, MaxLifetimeSec: 300}
	if p.JobTimeout() != 90*time.Second {
		t.Errorf("JobTimeout = %s", p.JobTimeout())
	}
	if p.MaxLifetime() != 300*time.Second {
		t.Errorf("MaxLifetime = %s", p.MaxLifetime())
	}
}

func TestInstanceStateTerminal(t *testing.T) {
	if !store.StateDeleted.Terminal() {
		t.Error("StateDeleted should be terminal")
	}
	for _, s := range []store.InstanceState{
		store.StatePending, store.StateProvisioning, store.StateBooting,
		store.StateIdle, store.StateBusy, store.StateDraining, store.StateFailed,
	} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal: it still owes a destroy", s)
		}
	}
}

func TestOpenRejectsUnknownDriver(t *testing.T) {
	if _, err := store.Open("mysql", "x"); err == nil {
		t.Fatal("expected an error for an unsupported driver")
	}
}

func seedPool(t *testing.T, db *store.DB) *store.Pool {
	t.Helper()
	cl := &store.Cloud{Name: "c", Driver: "fake", Enabled: true}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{CloudID: cl.ID, Name: "s"}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{Name: "f", Kind: "fake", Enabled: true}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "p", Enabled: true, ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID,
		Labels: store.StringList{"linux"}, MaxInstances: 5,
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

// TestBooleanFieldsPersistFalse is a regression test.
//
// GORM omits Go zero values from an INSERT, so a `default:true` column rewrites
// an explicit false into true. On Enabled that means a cloud or forge created
// as disabled comes back enabled, and the controller starts provisioning
// against a connection the operator meant to be off.
func TestBooleanFieldsPersistFalse(t *testing.T) {
	db := newDB(t)

	cl := &store.Cloud{Name: "c", Driver: "fake", Enabled: false}
	if err := db.Create(cl).Error; err != nil {
		t.Fatal(err)
	}
	fg := &store.Forge{Name: "f", Kind: "fake", Enabled: false}
	if err := db.Create(fg).Error; err != nil {
		t.Fatal(err)
	}
	size := &store.Size{CloudID: cl.ID, Name: "s"}
	if err := db.Create(size).Error; err != nil {
		t.Fatal(err)
	}
	pool := &store.Pool{
		Name: "p", Enabled: false, ForgeID: fg.ID, CloudID: cl.ID, SizeID: size.ID,
		Labels: store.StringList{"linux"}, PublicIPv4: false,
	}
	if err := db.Create(pool).Error; err != nil {
		t.Fatal(err)
	}

	var (
		gotCloud store.Cloud
		gotForge store.Forge
		gotPool  store.Pool
	)
	db.First(&gotCloud, cl.ID)
	db.First(&gotForge, fg.ID)
	db.First(&gotPool, pool.ID)

	if gotCloud.Enabled {
		t.Error("Cloud.Enabled was created as false but stored as true")
	}
	if gotForge.Enabled {
		t.Error("Forge.Enabled was created as false but stored as true")
	}
	if gotPool.Enabled {
		t.Error("Pool.Enabled was created as false but stored as true")
	}
	if gotPool.PublicIPv4 {
		t.Error("Pool.PublicIPv4 was created as false but stored as true")
	}
}
