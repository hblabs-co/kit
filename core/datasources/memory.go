package datasources

import (
	"context"
	"sync"
	"time"
)

// MemoryDatasource is an in-process Datasource implementation for
// unit tests and local development. Honours TTL by stamping each
// entry with an expiration time; expired entries are filtered at
// read time (no background sweep — the test runs short enough that
// the map fits in memory).
//
// Not for production. The production code path goes through the
// mongo subpackage.
type MemoryDatasource struct {
	now func() time.Time

	mu    sync.RWMutex
	items map[string]memoryEntry
}

type memoryEntry struct {
	value     []byte
	expiresAt time.Time // zero = no expiration
}

// NewMemoryDatasource builds an empty store. Tests that want to
// assert TTL behaviour deterministically can override the clock by
// reassigning `Now` after construction.
func NewMemoryDatasource() *MemoryDatasource {
	return &MemoryDatasource{
		now:   time.Now,
		items: map[string]memoryEntry{},
	}
}

// SetClock swaps the time source. Useful for tests that want to
// fast-forward TTL boundaries without sleeping.
func (m *MemoryDatasource) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Get implements Datasource.
func (m *MemoryDatasource) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.items[key]
	if !ok || m.expired(e) {
		return nil, ErrNotFound
	}
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

// Set implements Datasource.
func (m *MemoryDatasource) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return ErrNegativeTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(value))
	copy(cp, value)
	e := memoryEntry{value: cp}
	if ttl > 0 {
		e.expiresAt = m.now().Add(ttl)
	}
	m.items[key] = e
	return nil
}

// Delete implements Datasource. Idempotent.
func (m *MemoryDatasource) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

// Exists implements Datasource.
func (m *MemoryDatasource) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.items[key]
	if !ok || m.expired(e) {
		return false, nil
	}
	return true, nil
}

// Ping implements Datasource. Always nil — the in-memory store has
// no remote dependency to check.
func (m *MemoryDatasource) Ping(context.Context) error { return nil }

// Close implements Datasource. Drops every entry.
func (m *MemoryDatasource) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = map[string]memoryEntry{}
	return nil
}

// expired reports whether e's expiration is in the past. Holds the
// shared lock at call site so it stays trivially correct.
func (m *MemoryDatasource) expired(e memoryEntry) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return !m.now().Before(e.expiresAt)
}
