package siteassets

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// staticLoader returns the same bytes (and error) for every call,
// while counting invocations so tests can assert on cache hits vs.
// misses without timing tricks.
type staticLoader struct {
	raw   []byte
	err   error
	calls atomic.Int64
}

func (s *staticLoader) load(_ context.Context, _, _ string) ([]byte, error) {
	s.calls.Add(1)
	return s.raw, s.err
}

func TestManifestCache_Get_nilReceiver(t *testing.T) {
	t.Parallel()
	var c *ManifestCache
	m, err := c.Get(context.Background(), "org", "site")
	if err != nil || m != nil {
		t.Fatalf("nil receiver: want (nil, nil), got (%v, %v)", m, err)
	}
}

func TestManifestCache_Get_nilLoader(t *testing.T) {
	t.Parallel()
	c := NewManifestCache(nil, time.Minute)
	m, err := c.Get(context.Background(), "org", "site")
	if err != nil || m != nil {
		t.Fatalf("nil loader: want (nil, nil), got (%v, %v)", m, err)
	}
}

func TestManifestCache_Get_miss_thenHit(t *testing.T) {
	t.Parallel()
	loader := &staticLoader{raw: []byte(`{"_v":1,"assets":{"logo.png":"logo-abc12345.png"}}`)}
	c := NewManifestCache(loader.load, time.Minute)

	m1, err := c.Get(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if m1.Resolve("logo.png") != "logo-abc12345.png" {
		t.Fatalf("first Get: missing asset")
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("after first Get: want 1 loader call, got %d", got)
	}

	// Second Get within TTL — should not invoke the loader.
	m2, err := c.Get(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if m1 != m2 {
		t.Fatal("second Get should return cached pointer")
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("after second Get: want 1 loader call, got %d", got)
	}
}

func TestManifestCache_Get_TTLExpire(t *testing.T) {
	t.Parallel()
	loader := &staticLoader{raw: []byte(`{"_v":1,"assets":{}}`)}
	c := NewManifestCache(loader.load, 100*time.Millisecond)

	clock := time.Now()
	c.now = func() time.Time { return clock }

	if _, err := c.Get(context.Background(), "org", "site"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	// Advance past TTL.
	clock = clock.Add(time.Second)
	if _, err := c.Get(context.Background(), "org", "site"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := loader.calls.Load(); got != 2 {
		t.Fatalf("after TTL expiry: want 2 loader calls, got %d", got)
	}
}

func TestManifestCache_Get_TTLZero_neverExpires(t *testing.T) {
	t.Parallel()
	loader := &staticLoader{raw: []byte(`{"_v":1,"assets":{}}`)}
	c := NewManifestCache(loader.load, 0)

	clock := time.Now()
	c.now = func() time.Time { return clock }

	for i := 0; i < 5; i++ {
		if _, err := c.Get(context.Background(), "org", "site"); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		clock = clock.Add(time.Hour)
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("ttl=0: want 1 loader call, got %d", got)
	}
}

func TestManifestCache_Get_loaderError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("network down")
	loader := &staticLoader{err: wantErr}
	c := NewManifestCache(loader.load, time.Minute)

	m, err := c.Get(context.Background(), "org", "site")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err: want %v, got %v", wantErr, err)
	}
	if m != nil {
		t.Fatalf("m on error: want nil, got %+v", m)
	}
	// Cached error: second call doesn't re-invoke.
	if _, err := c.Get(context.Background(), "org", "site"); !errors.Is(err, wantErr) {
		t.Fatalf("second err: want %v, got %v", wantErr, err)
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("loader called %d times, want 1", got)
	}
}

func TestManifestCache_Get_parseError(t *testing.T) {
	t.Parallel()
	loader := &staticLoader{raw: []byte(`not json`)}
	c := NewManifestCache(loader.load, time.Minute)

	m, err := c.Get(context.Background(), "org", "site")
	if err == nil {
		t.Fatal("want parse error")
	}
	if m != nil {
		t.Fatalf("m on parse error: want nil, got %+v", m)
	}
}

func TestManifestCache_Get_noManifest(t *testing.T) {
	t.Parallel()
	// Loader returns (nil, nil): no manifest exists for this scope.
	// Should cache as "no manifest" — no error surfaced.
	loader := &staticLoader{}
	c := NewManifestCache(loader.load, time.Minute)

	m, err := c.Get(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("(nil,nil) loader: want no err, got %v", err)
	}
	if m != nil {
		t.Fatalf("(nil,nil) loader: want nil manifest, got %+v", m)
	}
	// Cached: second call doesn't re-invoke.
	if _, err := c.Get(context.Background(), "org", "site"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := loader.calls.Load(); got != 1 {
		t.Fatalf("loader called %d times, want 1", got)
	}
}

func TestManifestCache_Invalidate_forcesRefetch(t *testing.T) {
	t.Parallel()
	loader := &staticLoader{raw: []byte(`{"_v":1,"assets":{}}`)}
	c := NewManifestCache(loader.load, time.Minute)

	if _, err := c.Get(context.Background(), "org", "site"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	c.Invalidate("org", "site")
	if _, err := c.Get(context.Background(), "org", "site"); err != nil {
		t.Fatalf("post-invalidate Get: %v", err)
	}
	if got := loader.calls.Load(); got != 2 {
		t.Fatalf("after Invalidate + Get: want 2 loader calls, got %d", got)
	}
}

func TestManifestCache_Invalidate_nilReceiver(t *testing.T) {
	t.Parallel()
	// Should not panic.
	var c *ManifestCache
	c.Invalidate("org", "site")
}

func TestManifestCache_Invalidate_notCached(t *testing.T) {
	t.Parallel()
	c := NewManifestCache(func(context.Context, string, string) ([]byte, error) {
		return nil, nil
	}, time.Minute)
	c.Invalidate("never-cached-org", "never-cached-site") // no-op, no panic
}
