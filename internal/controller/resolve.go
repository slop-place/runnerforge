package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
	"github.com/slop-place/runnerforge/internal/metrics"
	"github.com/slop-place/runnerforge/internal/store"
)

// resolver builds live cloud and forge clients from database records and caches
// them, so a reconcile pass does not rebuild an HTTP client per tick.
//
// The cache key includes the record's UpdatedAt, so editing a cloud or forge in
// the UI transparently invalidates the cached client on the next pass. There is
// no explicit invalidation path to forget.
type resolver struct {
	mu     sync.Mutex
	clouds map[string]cloud.Provider
	forges map[string]forge.Forge
}

func newResolver() *resolver {
	return &resolver{
		clouds: map[string]cloud.Provider{},
		forges: map[string]forge.Forge{},
	}
}

func cacheKey(id uint, rev any) string { return fmt.Sprintf("%d/%v", id, rev) }

// Cloud returns a provider for the given record, building it if needed.
func (r *resolver) Cloud(c *store.Cloud) (cloud.Provider, error) {
	if c == nil {
		return nil, errors.New("resolve: nil cloud")
	}
	key := cacheKey(c.ID, c.UpdatedAt.UnixNano())
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.clouds[key]; ok {
		return p, nil
	}
	cfg := mergeSettings(c.Settings, c.Credentials)
	p, err := cloud.New(c.Driver, cfg)
	if err != nil {
		return nil, fmt.Errorf("cloud %q: %w", c.Name, err)
	}
	// Wrapping here means every driver is measured, including one written
	// later by someone who never reads the metrics package.
	p = metrics.WrapCloud(c.Name, p)
	// Only the newest build of a given record is kept; older revisions fall out.
	r.prune(r.clouds, c.ID)
	r.clouds[key] = p
	return p, nil
}

// Forge returns a forge client for the given record, building it if needed.
func (r *resolver) Forge(f *store.Forge) (forge.Forge, error) {
	if f == nil {
		return nil, errors.New("resolve: nil forge")
	}
	key := cacheKey(f.ID, f.UpdatedAt.UnixNano())
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.forges[key]; ok {
		return c, nil
	}
	cfg := mergeSettings(f.Settings, f.Credentials)
	cfg["name"] = f.Name
	c, err := forge.New(forge.Kind(f.Kind), cfg)
	if err != nil {
		return nil, fmt.Errorf("forge %q: %w", f.Name, err)
	}
	c = metrics.WrapForge(c)
	r.prune(r.forges, f.ID)
	r.forges[key] = c
	return c, nil
}

// prune drops cached entries for an id whose revision has changed. Generic over
// the cached value so clouds and forges share one implementation.
func (r *resolver) prune[T any](m map[string]T, id uint) {
	prefix := fmt.Sprintf("%d/", id)
	for k := range m {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(m, k)
		}
	}
}

// mergeSettings flattens a record's public settings and decrypted credentials
// into the single map the driver constructors take. Credentials win on conflict
// so a stale plaintext setting can never shadow a real secret.
func mergeSettings(settings store.Params, creds store.Secret) map[string]any {
	out := map[string]any{}
	maps.Copy(out, settings)
	// Credentials win on conflict, so a stale plaintext setting can never
	// shadow a real secret.
	for k, v := range creds {
		out[k] = v
	}
	return out
}

// checkCloud verifies a cloud's credentials by performing a live list. Used by
// the UI so an operator learns their credentials are wrong when they save them,
// not when a job is already waiting.
func (r *resolver) checkCloud(ctx context.Context, c *store.Cloud, owner cloud.Owner) error {
	p, err := r.Cloud(c)
	if err != nil {
		return err
	}
	_, err = p.List(ctx, owner)
	return err
}

// checkForge verifies a forge's credentials with a live call.
func (r *resolver) checkForge(ctx context.Context, f *store.Forge) error {
	c, err := r.Forge(f)
	if err != nil {
		return err
	}
	_, err = c.ListRunners(ctx)
	return err
}
