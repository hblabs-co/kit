package siteassets

import (
	"context"
	"sync"
	"time"
)

// Loader fetches the raw JSON bytes of a manifest for one (orgID,
// siteID) pair. Injected into ManifestCache so this package stays
// decoupled from any specific storage backend (minio-go directly,
// a future service-storage HTTP client, an in-memory fixture).
//
// Contract:
//   - On success: returns the manifest bytes and a nil error.
//   - When no manifest exists for the pair (storage-sync hasn't run
//     yet, or the site has no assets): returns (nil, nil) — NOT an
//     error. This is the normal "absent" state; callers fall back to
//     logical names.
//   - On real failures (network, auth, parse upstream): returns
//     (nil, err).
type Loader func(ctx context.Context, orgID, siteID string) ([]byte, error)

// ManifestCache is a goroutine-safe in-memory cache of Manifests
// keyed by (orgID, siteID). Misses trigger Loader; subsequent hits
// within `ttl` serve from memory.
//
// TTL semantics:
//   - ttl > 0: entries expire after `ttl` and are re-fetched on the
//     next access.
//   - ttl == 0: entries never expire automatically — refresh only via
//     Invalidate. Useful in services that subscribe to a NATS
//     "manifest updated" message and invalidate on demand.
type ManifestCache struct {
	loader Loader
	ttl    time.Duration
	now    func() time.Time

	mu    sync.RWMutex
	items map[string]cacheEntry
}

// cacheEntry is one row in the cache: the parsed manifest (or nil
// when absent), the error from the last fetch (or nil), and when the
// fetch completed.
type cacheEntry struct {
	manifest  *Manifest
	err       error
	fetchedAt time.Time
}

// NewManifestCache returns a fresh cache. A nil loader is allowed
// and produces a cache whose Get always returns (nil, nil) — handy
// as a no-op binding when the service has no asset host configured
// yet, without forcing every call site to nil-check.
func NewManifestCache(loader Loader, ttl time.Duration) *ManifestCache {
	return &ManifestCache{
		loader: loader,
		ttl:    ttl,
		now:    time.Now,
		items:  map[string]cacheEntry{},
	}
}

// Get returns the Manifest for (orgID, siteID), loading via the
// Loader on miss or after TTL expiry. A nil Manifest with a nil
// error means "no manifest exists for this pair" — callers treat it
// as "fall back to logical names".
//
// A nil receiver also returns (nil, nil) so a service that hasn't
// wired a cache yet can still pass `nil` through every helper
// without nil-guards at each call site.
func (c *ManifestCache) Get(ctx context.Context, orgID, siteID string) (*Manifest, error) {
	if c == nil || c.loader == nil {
		return nil, nil
	}
	key := orgID + "/" + siteID

	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if ok && (c.ttl == 0 || c.now().Sub(entry.fetchedAt) < c.ttl) {
		return entry.manifest, entry.err
	}

	return c.fetch(ctx, key, orgID, siteID)
}

// fetch invokes the Loader, parses the result, and stores the entry.
// Pulled out of Get so the hot-path read stays small.
func (c *ManifestCache) fetch(ctx context.Context, key, orgID, siteID string) (*Manifest, error) {
	raw, err := c.loader(ctx, orgID, siteID)
	var m *Manifest
	if err == nil {
		m, err = ParseManifest(raw)
	}
	c.mu.Lock()
	c.items[key] = cacheEntry{manifest: m, err: err, fetchedAt: c.now()}
	c.mu.Unlock()
	return m, err
}

// Invalidate drops the cached entry for (orgID, siteID), forcing the
// next Get to re-fetch via the Loader. No-op when the key isn't
// cached, or on a nil receiver. Safe to call concurrently with Get.
func (c *ManifestCache) Invalidate(orgID, siteID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.items, orgID+"/"+siteID)
	c.mu.Unlock()
}
