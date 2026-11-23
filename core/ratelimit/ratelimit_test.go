package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/hblabs-co/kit/core/log"
)

// TestAllow_EmptyKeyBypasses — Allow with key "" must always permit;
// that's how callers exempt internal traffic (keyFn returns "").
func TestAllow_EmptyKeyBypasses(t *testing.T) {
	l := New(Options{Rate: rate.Every(time.Hour), Burst: 1, Logger: log.Quiet()})
	for i := 0; i < 100; i++ {
		if !l.Allow(context.Background(), "") {
			t.Fatalf("empty key was throttled at iteration %d", i)
		}
	}
}

// TestAllow_BucketThrottlesPastBurst — once the bucket runs out,
// subsequent calls in the same instant are denied. We deliberately
// pick a Rate slow enough that no refill happens during the test.
func TestAllow_BucketThrottlesPastBurst(t *testing.T) {
	l := New(Options{
		Rate:   rate.Every(time.Hour),
		Burst:  3,
		Logger: log.Quiet(),
	})
	ctx := context.Background()
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow(ctx, "1.2.3.4") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed = %d, want 3 (burst capacity)", allowed)
	}
}

// TestAllow_StrikesEscalateToBan — once a key racks up StrikesToBan
// over-limit hits inside StrikeWindow, the in-memory blocked set
// should reject every subsequent call regardless of burst refill.
func TestAllow_StrikesEscalateToBan(t *testing.T) {
	l := New(Options{
		Rate:         rate.Every(time.Hour),
		Burst:        1,
		StrikesToBan: 3,
		StrikeWindow: time.Minute,
		BanFor:       time.Hour,
		Logger:       log.Quiet(),
	})
	ctx := context.Background()
	key := "9.9.9.9"

	// Burn the burst.
	if !l.Allow(ctx, key) {
		t.Fatal("first request must be allowed")
	}
	// Now every subsequent call is over-limit; after StrikesToBan
	// of them, the key should be on the persisted-equivalent block
	// list (in-memory in this test, no BlockStore).
	for i := 0; i < 3; i++ {
		if l.Allow(ctx, key) {
			t.Fatalf("over-limit request %d should have been denied", i)
		}
	}
	// One more — this should now hit the blocked path, not the
	// bucket path. Same return value, but it's the test's only way
	// to assert the ban actually landed.
	if !l.isBlocked(key) {
		t.Fatal("key should be in blocked set after StrikesToBan strikes")
	}
}

// TestApplyRemoteBlock_BlocksImmediately — the NATS hook path: a
// peer-pod or ops-tooling event adds a key to our in-memory cache
// without waiting for the next refresh tick.
func TestApplyRemoteBlock_BlocksImmediately(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	key := "remote.key"

	if l.isBlocked(key) {
		t.Fatal("key blocked before ApplyRemoteBlock — wrong state")
	}
	l.ApplyRemoteBlock(key, time.Now().Add(time.Hour))
	if !l.isBlocked(key) {
		t.Fatal("ApplyRemoteBlock did not register the ban")
	}
}

// TestApplyRemoteBlock_StaleEventDropped — events with an
// already-passed expiry are no-ops. Otherwise an out-of-order
// delivery could resurrect a ban a peer just rolled off.
func TestApplyRemoteBlock_StaleEventDropped(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	l.ApplyRemoteBlock("k", time.Now().Add(-time.Minute))
	if l.isBlocked("k") {
		t.Fatal("stale ApplyRemoteBlock leaked into blocked set")
	}
}

// TestApplyRemoteBlock_DoesNotShortenExistingBan — a remote event
// that comes in with an earlier expiry shouldn't shrink a ban that
// the local strike path just persisted for longer.
func TestApplyRemoteBlock_DoesNotShortenExistingBan(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	long := time.Now().Add(time.Hour)
	l.ApplyRemoteBlock("k", long)
	l.ApplyRemoteBlock("k", time.Now().Add(time.Minute))

	v, ok := l.blocked.Load("k")
	if !ok {
		t.Fatal("key vanished from blocked set")
	}
	got := v.(time.Time)
	if !got.Equal(long) {
		t.Errorf("expiry = %v, want longer %v", got, long)
	}
}

// TestMiddleware_429WithRetryAfter — denied requests get the right
// status code and a Retry-After header (consumed by polite clients
// and by Cloudflare's own retry budgeting).
func TestMiddleware_429WithRetryAfter(t *testing.T) {
	l := New(Options{
		Rate:   rate.Every(time.Hour),
		Burst:  1,
		Logger: log.Quiet(),
	})
	keyFn := func(r *http.Request) string { return "k" }
	mw := l.Middleware(keyFn)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First passes — burns the burst.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}

	// Second is throttled.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After missing on 429")
	}
}

// TestOnBan_FiresOnceWithDetails — the NATS publish hook fires
// exactly when a ban escalates, with the configured reason and a
// future expiry. We use a sync.WaitGroup since OnBan runs in a
// goroutine on purpose (don't block the request path).
func TestOnBan_FiresOnceWithDetails(t *testing.T) {
	var (
		mu       sync.Mutex
		seenKey  string
		seenWhy  string
		seenExp  time.Time
		callsWG  sync.WaitGroup
		numCalls int
	)
	callsWG.Add(1)

	l := New(Options{
		Rate:         rate.Every(time.Hour),
		Burst:        1,
		StrikesToBan: 2,
		StrikeWindow: time.Minute,
		BanFor:       time.Hour,
		BlockReason:  "test_reason",
		Logger:       log.Quiet(),
		OnBan: func(_ context.Context, key, reason string, expiresAt time.Time) {
			mu.Lock()
			seenKey, seenWhy, seenExp = key, reason, expiresAt
			numCalls++
			mu.Unlock()
			callsWG.Done()
		},
	})
	ctx := context.Background()
	l.Allow(ctx, "k") // burns burst
	l.Allow(ctx, "k") // strike 1
	l.Allow(ctx, "k") // strike 2 → ban

	// OnBan runs in a goroutine; wait for it.
	done := make(chan struct{})
	go func() { callsWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnBan did not fire within 1s")
	}

	mu.Lock()
	defer mu.Unlock()
	if numCalls != 1 {
		t.Errorf("OnBan calls = %d, want 1", numCalls)
	}
	if seenKey != "k" || seenWhy != "test_reason" {
		t.Errorf("OnBan got (%q, %q), want (k, test_reason)", seenKey, seenWhy)
	}
	if !seenExp.After(time.Now()) {
		t.Errorf("OnBan expiry %v is not in the future", seenExp)
	}
}

// TestRefreshOnce_AddsAndDropsKeys — Snapshot is the source of
// truth: keys it returns enter the blocked set, keys that disappear
// leave it. This is how a ban roll-off propagates from one pod to
// another via the durable BlockStore path.
func TestRefreshOnce_AddsAndDropsKeys(t *testing.T) {
	store := &fakeStore{snap: map[string]time.Time{
		"a": time.Now().Add(time.Hour),
		"b": time.Now().Add(time.Hour),
	}}
	l := New(Options{BlockStore: store, Logger: log.Quiet()})
	l.refreshOnce(context.Background())
	if !l.isBlocked("a") || !l.isBlocked("b") {
		t.Fatal("refresh did not import snapshot keys")
	}

	store.mu.Lock()
	store.snap = map[string]time.Time{"a": time.Now().Add(time.Hour)}
	store.mu.Unlock()
	l.refreshOnce(context.Background())

	if !l.isBlocked("a") {
		t.Error("refresh dropped a key that was still in snapshot")
	}
	if l.isBlocked("b") {
		t.Error("refresh did not drop a key that left the snapshot")
	}
}

// TestNew_NilLoggerFallsBackToDefault — leaving Options.Logger nil
// must NOT panic later when ratelimit logs (e.g. on ban). The
// fallback to log.Default() is documented; this pins it.
func TestNew_NilLoggerFallsBackToDefault(t *testing.T) {
	l := New(Options{
		Rate:         rate.Every(time.Hour),
		Burst:        1,
		StrikesToBan: 1,
		StrikeWindow: time.Minute,
		BanFor:       time.Minute,
		// Logger intentionally nil.
	})
	ctx := context.Background()
	l.Allow(ctx, "k") // burns burst
	l.Allow(ctx, "k") // strike → ban → log.Warn through default logger
	if !l.isBlocked("k") {
		t.Error("ban did not register through default-logger path")
	}
}

// TestStartStop_AreIdempotent — Start uses sync.Once so a duplicate
// call is a no-op; Stop tolerates being called twice without
// panicking on a double channel close. Both behaviours are part of
// the package's "safe to defer" promise.
func TestStartStop_AreIdempotent(t *testing.T) {
	store := &fakeStore{snap: map[string]time.Time{}}
	l := New(Options{
		BlockStore:      store,
		RefreshInterval: 10 * time.Millisecond,
		Logger:          log.Quiet(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l.Start(ctx)
	l.Start(ctx) // second call must be a no-op via sync.Once
	// Let refreshLoop tick at least once.
	time.Sleep(50 * time.Millisecond)

	l.Stop()
	l.Stop() // second call must take the "already closed" branch
}

// TestStart_WithoutStoreSkipsRefresh — when BlockStore is nil there
// is nothing to refresh from; Start must still spin up sweepLoop but
// skip both refreshOnce and refreshLoop. Locks the nil-store branch.
func TestStart_WithoutStoreSkipsRefresh(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	// No goroutine references the store; cancelling the context
	// must release the sweep goroutine cleanly.
	cancel()
	// Stop is also safe — even if sweepLoop already exited.
	l.Stop()
}

// TestRefreshLoop_ExitsOnContextCancel — refreshLoop must terminate
// when ctx is cancelled, releasing its ticker. We use a tight
// RefreshInterval so the goroutine actually parks on the select
// before we cancel.
func TestRefreshLoop_ExitsOnContextCancel(t *testing.T) {
	store := &fakeStore{snap: map[string]time.Time{}}
	l := New(Options{
		BlockStore:      store,
		RefreshInterval: 10 * time.Millisecond,
		Logger:          log.Quiet(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)
	time.Sleep(30 * time.Millisecond) // let refreshLoop tick
	cancel()
	// No reliable way to observe goroutine exit; the test passes
	// when -race is clean and the test process doesn't hang.
	time.Sleep(50 * time.Millisecond)
}

// TestRefreshLoop_ExitsOnStop — Stop closes stopCh; refreshLoop's
// select must exit through that branch too (covers the "stopCh
// closed" case independently from ctx cancel).
func TestRefreshLoop_ExitsOnStop(t *testing.T) {
	store := &fakeStore{snap: map[string]time.Time{}}
	l := New(Options{
		BlockStore:      store,
		RefreshInterval: 10 * time.Millisecond,
		Logger:          log.Quiet(),
	})
	l.Start(context.Background())
	time.Sleep(30 * time.Millisecond)
	l.Stop()
	time.Sleep(20 * time.Millisecond)
}

// TestSweepLoop_ExitsOnStop — same shape as the refresh-loop case
// but for the sweep goroutine, which always runs (even without a
// BlockStore). Without coverage here a Stop regression could leak
// the sweep goroutine across reloads.
func TestSweepLoop_ExitsOnStop(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	l.Start(context.Background())
	l.Stop()
	time.Sleep(20 * time.Millisecond)
}

// TestSweepOnce_EvictsIdleBucketsAndStaleStrikes — sweepOnce
// pruning runs against two structures: bucket entries (idle for
// >30m) and strike counters (windowStart older than StrikeWindow).
// Direct test, since the production cadence is 5 min — too slow
// to drive through sweepLoop's timer in a unit test.
func TestSweepOnce_EvictsIdleBucketsAndStaleStrikes(t *testing.T) {
	l := New(Options{
		Rate:         rate.Every(time.Hour),
		Burst:        1,
		StrikeWindow: 1 * time.Millisecond, // tiny so the test trips it
		Logger:       log.Quiet(),
	})
	// Seed a bucket and a strike counter.
	_ = l.bucket("idle-key")
	l.recordStrike("strike-key")
	// Force the lastSeen back in time so eviction triggers.
	if v, ok := l.buckets.Load("idle-key"); ok {
		v.(*bucketEntry).lastSeen.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	time.Sleep(5 * time.Millisecond) // let the strike windowStart age

	l.sweepOnce(time.Now())

	if _, ok := l.buckets.Load("idle-key"); ok {
		t.Error("idle bucket survived sweep")
	}
	if _, ok := l.striker.Load("strike-key"); ok {
		t.Error("stale strike counter survived sweep")
	}
}

// TestIsBlocked_ExpiredEntryCleanedUp — when a previously stored
// expiry has already passed, isBlocked must DELETE it from the
// blocked set (not just return false). Otherwise the entry would
// linger forever, defeating the in-memory cache invalidation that
// matches Mongo's TTL daemon.
func TestIsBlocked_ExpiredEntryCleanedUp(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	l.blocked.Store("k", time.Now().Add(-time.Minute))
	if l.isBlocked("k") {
		t.Error("isBlocked returned true for already-expired entry")
	}
	if _, ok := l.blocked.Load("k"); ok {
		t.Error("expired entry was not removed from blocked set")
	}
}

// TestRecordHit_WithStoreInvokesHit — when a BlockStore is wired,
// Allow on a blocked key must fan out a Hit to the store
// asynchronously. We assert via the fakeStore counter.
func TestRecordHit_WithStoreInvokesHit(t *testing.T) {
	store := &fakeStore{snap: map[string]time.Time{
		"banned": time.Now().Add(time.Hour),
	}}
	l := New(Options{
		BlockStore: store,
		Logger:     log.Quiet(),
	})
	l.refreshOnce(context.Background()) // import the ban
	if !l.isBlocked("banned") {
		t.Fatal("setup: ban did not take")
	}
	l.Allow(context.Background(), "banned")
	// Hit fires in a goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		hits := store.hitCalls
		store.mu.Unlock()
		if hits >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("BlockStore.Hit was not called")
}

// TestRecordHit_LogsErrorOnHitFailure — a flapping BlockStore must
// not affect the request path; the error is logged at debug level
// and Allow still returns false. This locks the "non-fatal" promise.
func TestRecordHit_LogsErrorOnHitFailure(t *testing.T) {
	store := &fakeStore{
		snap:   map[string]time.Time{"k": time.Now().Add(time.Hour)},
		hitErr: errors.New("mongo down"),
	}
	l := New(Options{BlockStore: store, Logger: log.Quiet()})
	l.refreshOnce(context.Background())
	if l.Allow(context.Background(), "k") {
		t.Fatal("blocked key should not be allowed")
	}
	// Wait briefly for the goroutine to attempt the call.
	time.Sleep(100 * time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.hitCalls == 0 {
		t.Error("Hit was never attempted despite blocked key + store wired")
	}
}

// TestRecordStrike_BlockStoreErrorIsNonFatal — Add failures during
// ban persistence must NOT prevent the in-memory ban from taking
// effect. Otherwise a Mongo blip would let an attacker keep going.
func TestRecordStrike_BlockStoreErrorIsNonFatal(t *testing.T) {
	store := &fakeStore{addErr: errors.New("mongo down")}
	l := New(Options{
		Rate:         rate.Every(time.Hour),
		Burst:        1,
		StrikesToBan: 2,
		StrikeWindow: time.Minute,
		BanFor:       time.Hour,
		BlockStore:   store,
		Logger:       log.Quiet(),
	})
	ctx := context.Background()
	l.Allow(ctx, "k") // burst
	l.Allow(ctx, "k") // strike → ban
	l.Allow(ctx, "k") // ban triggers Add (which will error)

	if !l.isBlocked("k") {
		t.Error("in-memory ban should land even when BlockStore.Add fails")
	}
	// Let the goroutine make its (failing) Add attempt.
	time.Sleep(50 * time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.addCalls == 0 {
		t.Error("Add was never attempted")
	}
}

// TestRetryAfter_BlockedSecondsBelow1 — a near-future expiry rounds
// to <1s; the function must clamp to 1 so Retry-After never says "0"
// (which clients interpret as "retry immediately" and would worsen
// the abuse pattern this code is here to dampen).
func TestRetryAfter_BlockedSecondsBelow1(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	l.blocked.Store("k", time.Now().Add(50*time.Millisecond))
	if got := l.retryAfterSeconds("k"); got != 1 {
		t.Errorf("retryAfter = %d, want 1 (clamped)", got)
	}
}

// TestRetryAfter_BlockedSecondsAboveOne — for a blocked key with a
// comfortably future expiry the seconds-until-expiry path must
// return the actual remaining time (the un-clamped branch). Pairs
// with the sub-second clamp test to lock in both halves of the
// blocked-path arithmetic.
func TestRetryAfter_BlockedSecondsAboveOne(t *testing.T) {
	l := New(Options{Logger: log.Quiet()})
	l.blocked.Store("k", time.Now().Add(30*time.Second))
	got := l.retryAfterSeconds("k")
	if got < 25 || got > 30 {
		t.Errorf("retryAfter = %d, want ~30", got)
	}
}

// TestRetryAfter_RateInfFallsBackToSixty — when Rate is rate.Inf the
// derived bucket-refill duration computes to zero and the if-branch
// is skipped; the function must fall through to the documented
// 60-second floor instead of returning 0. Locks the "expose nothing
// internal" upper bound on a degenerate Rate.
func TestRetryAfter_RateInfFallsBackToSixty(t *testing.T) {
	l := New(Options{
		Rate:   rate.Inf,
		Burst:  1,
		Logger: log.Quiet(),
	})
	if got := l.retryAfterSeconds("unblocked"); got != 60 {
		t.Errorf("retryAfter = %d, want 60 (rate.Inf fallback)", got)
	}
}

// TestSweepLoop_TickInvokesSweepOnce — drives the timer-tick branch
// of sweepLoop by overriding the per-Limiter sweepInterval to a
// millisecond cadence. Without this the case-now branch never fires
// in unit tests (production cadence is 5 min) and a regression that
// broke the tick path would slip through.
func TestSweepLoop_TickInvokesSweepOnce(t *testing.T) {
	l := New(Options{
		Rate:   rate.Every(time.Hour),
		Burst:  1,
		Logger: log.Quiet(),
	})
	l.sweepInterval = 5 * time.Millisecond
	// Seed an idle bucket whose lastSeen is far in the past so the
	// sweep that fires on the timer tick will evict it — that gives
	// us an observable signal the tick branch ran.
	_ = l.bucket("idle")
	if v, ok := l.buckets.Load("idle"); ok {
		v.(*bucketEntry).lastSeen.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	defer l.Stop()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := l.buckets.Load("idle"); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sweepLoop tick branch never evicted the idle bucket")
}

// TestRetryAfter_BucketFallback — for keys NOT on the blocked list
// (typical bucket-throttled requests), retryAfter falls back to a
// rate-derived estimate. Locks in the "expose nothing internal"
// upper bound so /headers/Retry-After remain a stable contract.
func TestRetryAfter_BucketFallback(t *testing.T) {
	l := New(Options{
		Rate:   rate.Every(time.Minute),
		Burst:  1,
		Logger: log.Quiet(),
	})
	got := l.retryAfterSeconds("unblocked")
	if got < 1 {
		t.Errorf("retryAfter = %d, want >= 1", got)
	}
}

// TestRetryAfter_BucketFallbackClampsLowerBound — when the rate is
// fast enough that the natural retry duration is sub-second, the
// fallback path must still clamp to 1. Same rationale as the
// blocked-path clamp.
func TestRetryAfter_BucketFallbackClampsLowerBound(t *testing.T) {
	l := New(Options{
		Rate:   rate.Every(100 * time.Microsecond), // sub-second refill
		Burst:  1,
		Logger: log.Quiet(),
	})
	if got := l.retryAfterSeconds("unblocked"); got != 1 {
		t.Errorf("retryAfter = %d, want 1 (clamped lower bound)", got)
	}
}

// TestRefreshOnce_SnapshotErrorLogged — when the BlockStore can't
// produce a snapshot (transient network blip), refreshOnce must log
// and bail without mutating the in-memory blocked set; otherwise a
// flap would wipe legitimate bans every refresh.
func TestRefreshOnce_SnapshotErrorLogged(t *testing.T) {
	store := &fakeStore{snapErr: errors.New("mongo timeout")}
	l := New(Options{BlockStore: store, Logger: log.Quiet()})
	// Pre-seed an in-memory ban — must survive the failing refresh.
	l.blocked.Store("survivor", time.Now().Add(time.Hour))

	l.refreshOnce(context.Background())

	if !l.isBlocked("survivor") {
		t.Error("snapshot error wiped the in-memory blocked set")
	}
}

// fakeStore is a minimal in-memory BlockStore for tests — Add and
// Hit just record the call, Snapshot returns whatever was set.
// addErr / hitErr / snapErr are optional knobs that let tests drive
// the BlockStore-failure branches without standing up a real Mongo.
type fakeStore struct {
	mu       sync.Mutex
	snap     map[string]time.Time
	addCalls int
	hitCalls int
	addErr   error
	hitErr   error
	snapErr  error
}

func (f *fakeStore) Add(_ context.Context, key, _ string, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	if f.addErr != nil {
		return f.addErr
	}
	if f.snap == nil {
		f.snap = map[string]time.Time{}
	}
	f.snap[key] = exp
	return nil
}

func (f *fakeStore) Hit(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hitCalls++
	return f.hitErr
}

func (f *fakeStore) Snapshot(_ context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	out := make(map[string]time.Time, len(f.snap))
	for k, v := range f.snap {
		out[k] = v
	}
	return out, nil
}
