package datasources

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryGetMissingReturnsErrNotFound(t *testing.T) {
	ds := NewMemoryDatasource()
	if _, err := ds.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestMemorySetGetRoundtrip(t *testing.T) {
	ds := NewMemoryDatasource()
	ctx := context.Background()
	if err := ds.Set(ctx, "k", []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := ds.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "hello" {
		t.Fatalf("got %q, want hello", v)
	}
}

func TestMemoryTTLExpiry(t *testing.T) {
	ds := NewMemoryDatasource()
	ctx := context.Background()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ds.SetClock(func() time.Time { return now })

	if err := ds.Set(ctx, "k", []byte("v"), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Get(ctx, "k"); err != nil {
		t.Fatalf("immediate get failed: %v", err)
	}

	now = now.Add(10 * time.Second)
	ds.SetClock(func() time.Time { return now })
	if _, err := ds.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired get err = %v, want ErrNotFound", err)
	}
	exists, err := ds.Exists(ctx, "k")
	if err != nil || exists {
		t.Fatalf("Exists after expiry = (%v, %v), want (false, nil)", exists, err)
	}
}

func TestMemoryDeleteIsIdempotent(t *testing.T) {
	ds := NewMemoryDatasource()
	ctx := context.Background()
	if err := ds.Delete(ctx, "missing"); err != nil {
		t.Fatalf("delete missing returned %v", err)
	}
	_ = ds.Set(ctx, "k", []byte("v"), 0)
	if err := ds.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func TestMemoryNegativeTTLIsAnError(t *testing.T) {
	ds := NewMemoryDatasource()
	if err := ds.Set(context.Background(), "k", []byte("v"), -time.Second); !errors.Is(err, ErrNegativeTTL) {
		t.Fatalf("err = %v, want ErrNegativeTTL", err)
	}
}

func TestMemorySatisfiesDatasourceInterface(t *testing.T) {
	var _ Datasource = NewMemoryDatasource()
}

// TestMemoryExistsTrueForLiveKey — Exists must return (true, nil)
// for a non-expired key. The TTL test above only covers the
// missing/expired branch; this pins the happy path so a refactor
// can't silently invert the boolean.
func TestMemoryExistsTrueForLiveKey(t *testing.T) {
	ds := NewMemoryDatasource()
	ctx := context.Background()
	if err := ds.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	exists, err := ds.Exists(ctx, "k")
	if err != nil {
		t.Fatalf("Exists err = %v", err)
	}
	if !exists {
		t.Error("Exists = false, want true for a live key")
	}
}

// TestMemoryPingAlwaysNil — Ping is documented as always nil for the
// in-memory store (no remote to probe). Locking it in keeps the
// Datasource contract uniform with whatever the mongo path does.
func TestMemoryPingAlwaysNil(t *testing.T) {
	ds := NewMemoryDatasource()
	if err := ds.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v, want nil", err)
	}
}

// TestMemoryCloseDropsEntries — Close must clear the store and
// return nil; Get on a previously-set key after Close should miss.
// This is what callers rely on when they replace a Datasource with
// a fresh one mid-test.
func TestMemoryCloseDropsEntries(t *testing.T) {
	ds := NewMemoryDatasource()
	ctx := context.Background()
	_ = ds.Set(ctx, "k", []byte("v"), 0)

	if err := ds.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if _, err := ds.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Close err = %v, want ErrNotFound", err)
	}
}
