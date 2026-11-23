package og

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// ErrNotFound is what Storage.Get returns when a key has never been
// written. Callers compare with errors.Is — concrete impls (MinIO,
// in-process) wrap their underlying error so the cause is still
// inspectable in logs.
var ErrNotFound = errors.New("og: storage: not found")

// Storage is the put-once / read-by-key contract OG needs from a
// blob store. Two pods rendering the same Card race-write the same
// bytes; PutIfAbsent makes that idempotent so neither pod overwrites
// the other's identical content. Implementations:
//
//   - MemoryStorage (this file) — in-process map, suitable for local
//     dev and tests.
//   - MinIO/S3 (in packages/storageclient, when it lands) — uses
//     `If-None-Match: *` on the PUT so the second writer in a race
//     gets a 412 the impl swallows.
type Storage interface {
	// Get returns the bytes stored at key. Returns ErrNotFound when
	// the key has never been written; any other error is a real
	// storage failure (network, auth, quota) and the handler treats
	// it as a 500 rather than re-rendering — re-rendering on a
	// transient outage would just paper over the problem.
	Get(ctx context.Context, key string) ([]byte, string, error)

	// PutIfAbsent stores body+contentType at key only if no object
	// exists there. The second of two concurrent writes returns
	// nil (idempotent) — both wrote identical bytes anyway. A
	// non-nil error indicates a real failure, not a race.
	PutIfAbsent(ctx context.Context, key string, body []byte, contentType string) error

	// URL returns a public URL for key. For MinIO behind a CDN
	// this is the cdn.example.com URL pages embed in og:image
	// meta tags directly; for MemoryStorage it's the relative path
	// served by the bundled handler.
	URL(key string) string
}

// MemoryStorage is the in-process Storage implementation: a map
// guarded by a RWMutex, with URLs that point at the bundled HTTP
// handler. Suitable for local dev and unit tests; do not use in prod
// (cards regenerate on every restart and don't sync between pods).
type MemoryStorage struct {
	urlPrefix string

	mu    sync.RWMutex
	items map[string]memoryItem
}

type memoryItem struct {
	body  []byte
	ctype string
}

// NewMemoryStorage builds an empty store. urlPrefix is the path the
// service mounts the OG handler under (e.g. "/og"); URL appends
// "/<key>.png" so the same NewHandler reads back what was written.
// Trailing slashes are tolerated.
func NewMemoryStorage(urlPrefix string) *MemoryStorage {
	return &MemoryStorage{
		urlPrefix: strings.TrimSuffix(urlPrefix, "/"),
		items:     map[string]memoryItem{},
	}
}

// URLPrefix is the prefix passed to NewMemoryStorage with any
// trailing slash trimmed. Exposed so the auto-mount in render.Site
// can derive the route path from the same source of truth.
func (m *MemoryStorage) URLPrefix() string { return m.urlPrefix }

// Get returns the stored bytes + contentType, or ErrNotFound.
func (m *MemoryStorage) Get(_ context.Context, key string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if it, ok := m.items[key]; ok {
		return it.body, it.ctype, nil
	}
	return nil, "", ErrNotFound
}

// PutIfAbsent stores body under key when no value exists; on
// duplicate keys it returns nil silently (consistent with how the
// MinIO impl will handle the 412 from `If-None-Match: *`). Caller
// owns body before the call; we keep our own copy so a later mutation
// of the input slice doesn't corrupt the cache.
func (m *MemoryStorage) PutIfAbsent(_ context.Context, key string, body []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[key]; ok {
		return nil
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	m.items[key] = memoryItem{body: cp, ctype: contentType}
	return nil
}

// URL returns "<URLPrefix>/<key>.png".
func (m *MemoryStorage) URL(key string) string {
	return m.urlPrefix + "/" + key + ".png"
}
