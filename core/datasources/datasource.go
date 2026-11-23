package datasources

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is what Datasource.Get returns when a key has no value
// (never written, or expired). errors.Is(err, ErrNotFound) is the
// canonical check; backends wrap this so the underlying cause stays
// inspectable in logs.
var ErrNotFound = errors.New("datasources: not found")

// ErrNegativeTTL is what Datasource.Set returns when called with a
// negative duration. Sentinel rather than ad-hoc text so callers can
// distinguish a programming error (caught in tests / staging) from
// a real backend failure.
var ErrNegativeTTL = errors.New("datasources: negative ttl")

// Datasource is the unified key/value contract every persistence
// backend in this umbrella implements. Methods take []byte rather
// than encoded strings so callers control the wire format (JSON,
// gob, protobuf, raw bytes — the store doesn't care).
//
// Limits of the abstraction (knowingly accepted):
//
//   - No transactions across calls. A backend that has them
//     (Postgres) exposes them through its typed client; cross-key
//     consistency is not part of this interface.
//   - No conditional writes (CAS). Add a method only when a
//     real consumer needs it — premature primitives leak.
//   - No range / prefix scans. Backends differ wildly here (Redis
//     SCAN cursors vs Postgres SELECT…LIKE); callers that need
//     scanning take the typed client.
//
// In short: this is the "store an opaque blob under a key, maybe
// with a TTL" API. Anything more specific lives in the typed client
// or in a domain-level repository.
type Datasource interface {
	// Get returns the bytes at key, or ErrNotFound. Any other error
	// is a real backend failure — handlers MUST NOT treat it as a
	// miss, or a flapping backend will silently regenerate work
	// while the underlying problem hides.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set writes value at key. ttl=0 means no expiration; negative
	// ttl returns an error. Callers that want "set without changing
	// the existing TTL" should fetch + delete + set, or reach for
	// the typed client.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key. Idempotent: deleting a missing key returns
	// nil. Errors only on a real backend failure.
	Delete(ctx context.Context, key string) error

	// Exists reports whether key currently holds a non-expired value.
	// Faster than Get when the caller only needs the boolean — Redis
	// returns a single integer, Postgres a count(*) — but the choice
	// between them is style; both work.
	Exists(ctx context.Context, key string) (bool, error)

	// Ping verifies the underlying connection is healthy. Suitable
	// for observability.Healthz; ctx timeout caps the check so a
	// hung backend doesn't take the readiness probe down with it.
	Ping(ctx context.Context) error

	// Close releases the underlying connection pool. Call from a
	// service's graceful-shutdown path. Safe to call once and only
	// once — concrete impls don't promise idempotency.
	Close() error
}
