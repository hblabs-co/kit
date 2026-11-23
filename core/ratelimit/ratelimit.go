// Package ratelimit is the abuse-protection layer shared by every
// HB Labs HTTP service. Two concerns wrapped behind one Limiter:
//
//   - Hot path: a token bucket per key (typically client IP) using
//     `golang.org/x/time/rate`. Cheap, in-memory, per-pod. Absorbs
//     99% of casual flooding without touching any external store.
//   - Cold path: a BlockStore (implemented by the app, e.g. on its database) that
//     persists explicit bans. When a key trips the bucket
//     StrikesToBan times within StrikeWindow, the Limiter records
//     a long-lived ban (BanFor) so the abuser stays blocked across
//     restarts and across pods. A background goroutine refreshes
//     the in-memory blocked set from the store every RefreshInterval
//     so manual bans (or bans from peer pods) propagate within a
//     bounded delay.
//
// For instant cross-pod propagation (no waiting for the next refresh
// tick) services should subscribe to the NATS subject
// `ratelimit.block.created` and call Limiter.ApplyRemoteBlock when
// an event lands. Pull-based refresh stays as the durable fallback.
//
// Wire it in like:
//
//	store := myapp.NewBlockStore(db) // any BlockStore; nil keeps bans in memory
//	rl := ratelimit.New(ratelimit.Options{
//	    Rate:        rate.Every(12 * time.Second),
//	    Burst:       3,
//	    BlockStore:  store,
//	    BlockReason: "waitlist_flood",
//	})
//	rl.Start(ctx)
//	mux.Handle("/api/waitlist", rl.Middleware(func(r *http.Request) string {
//	    return clientIP(r)
//	})(serveWaitlist))
//
// Same Limiter is safe to mount under several routes, but if you
// want different policies per endpoint (e.g. login = 1/sec, signup
// = 5/min), build one Limiter per policy.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// Defaults applied by New when the matching Options field is zero.
// Tuned for a public POST endpoint behind Cloudflare; override
// per-Options for endpoints with different abuse profiles.
const (
	// defaultRateInterval = "one token every 12s" → 5 req/min steady-state.
	defaultRateInterval = 12 * time.Second
	// defaultBurst is how many requests a key can fire back-to-back
	// before being throttled to defaultRateInterval.
	defaultBurst = 3
	// defaultStrikesToBan: this many over-limit hits within
	// defaultStrikeWindow trigger a persisted ban.
	defaultStrikesToBan = 5
	// defaultStrikeWindow is the rolling window for counting strikes.
	defaultStrikeWindow = 5 * time.Minute
	// defaultBanFor is the TTL of a persisted ban.
	defaultBanFor = 24 * time.Hour
	// defaultRefreshInterval bounds how stale a remote ban can be on
	// this pod before the next BlockStore poll picks it up.
	defaultRefreshInterval = 60 * time.Second
	// defaultBlockReason is recorded on persisted blocks when the
	// caller didn't override Options.BlockReason.
	defaultBlockReason = "rate_limit_exceeded"
)

// blockstoreOpTimeout caps every BlockStore op invoked from a request
// goroutine (Add / Hit) and from the periodic refresh loop. Detached
// from the request context so persistence outlives the request that
// triggered the ban.
const blockstoreOpTimeout = 5 * time.Second

// minRetryAfterSeconds is the floor for the Retry-After header in the
// 429 response. Honours HTTP semantics (Retry-After must be >= 1).
const minRetryAfterSeconds = 1

// fallbackRetryAfterSeconds is what retryAfterSeconds returns when
// neither the blocklist expiry nor the bucket refill rate yield a
// usable value. One minute is a safe upper bound that doesn't expose
// internal state.
const fallbackRetryAfterSeconds = 60

// idleBucketAge is how long a bucket / strike counter can go untouched
// before the sweep loop evicts it. A bucket regenerated cheaply on
// the next request is worth more than keeping a long tail of cold
// entries in memory.
const idleBucketAge = 30 * time.Minute

// logger is the minimal logging surface ratelimit needs internally.
// The canonical *slog.Logger from core/log satisfies it
// natively, as do test spies. Keeping it unexported avoids leaking
// an extra type into callers who already work with *slog.Logger.
type logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Options configures a Limiter. Zero values pick the defaults
// documented per-field — sane for a public POST endpoint behind
// Cloudflare. Override what makes sense for your endpoint.
type Options struct {
	// Rate is the steady-state refill rate of the per-key bucket.
	// Default: rate.Every(12 * time.Second) → 5 req/min.
	Rate rate.Limit
	// Burst is the bucket capacity — how many requests a key can
	// fire in quick succession before being throttled to Rate.
	// Default: 3.
	Burst int

	// StrikesToBan is how many over-limit requests within
	// StrikeWindow trigger a persisted ban. Default: 5.
	StrikesToBan int
	// StrikeWindow is the rolling window for counting strikes.
	// Default: 5 * time.Minute.
	StrikeWindow time.Duration
	// BanFor is the TTL of a persisted ban. Default: 24 * time.Hour.
	BanFor time.Duration

	// BlockStore persists bans across pod restarts and shares them
	// across replicas. nil = memory-only (bans evaporate on
	// restart, no cross-pod sync). Production: pass a persistent BlockStore.
	BlockStore BlockStore

	// BlockReason is recorded on the persisted block row so an
	// admin reviewing the table can tell which endpoint triggered
	// the ban. Default: "rate_limit_exceeded".
	BlockReason string

	// RefreshInterval controls how often the in-memory blocked set
	// is reloaded from BlockStore. Bounds how stale a remote ban
	// (manual ops, sibling pod) can be on this pod. Default: 60s.
	// For sub-second cross-pod propagation wire NATS into
	// ApplyRemoteBlock — refresh is the durable fallback.
	RefreshInterval time.Duration

	// Logger records diagnostic events (BlockStore failures,
	// escalations, etc.). nil = log.Default. *slog.Logger satisfies
	// this naturally, so existing callers don't need to change.
	Logger logger

	// OnBan is called from the request goroutine the moment a key
	// trips StrikesToBan and earns a persisted ban. Use it to
	// publish a NATS `ratelimit.block.created` event so peer pods
	// can apply the ban without waiting for the next refresh tick.
	// Receives a detached context — do NOT inherit a request ctx
	// here, the publish should outlive the request that triggered
	// the ban. nil = no-op.
	OnBan func(ctx context.Context, key, reason string, expiresAt time.Time)
}

// BlockStore persists explicit bans. Implementations are expected
// to be safe for concurrent use; the Limiter calls them from the
// request goroutine (Add/Hit) and from a background loop (Snapshot).
type BlockStore interface {
	// Add records a block for key until expiresAt with the given
	// reason. Idempotent: re-adding an existing key extends the
	// expiry and bumps the recorded reason.
	Add(ctx context.Context, key, reason string, expiresAt time.Time) error
	// Hit signals that a blocked key tried again. Best-effort —
	// useful for forensics ("this attacker hit us 4321 times after
	// the ban started"); failures are non-fatal.
	Hit(ctx context.Context, key string) error
	// Snapshot returns the currently-active blocks, key → expiry.
	// The Limiter calls this periodically to refresh its in-memory
	// view; implementations should NOT include already-expired rows.
	Snapshot(ctx context.Context) (map[string]time.Time, error)
}

// Limiter is the public type. Build it with New, then call Start
// once before serving traffic and Stop on shutdown. Allow / Middleware
// are safe for concurrent use.
type Limiter struct {
	rate    rate.Limit
	burst   int
	strikes int
	window  time.Duration
	banFor  time.Duration
	store   BlockStore
	reason  string
	refresh time.Duration
	log     logger
	onBan   func(ctx context.Context, key, reason string, expiresAt time.Time)

	buckets sync.Map // key → *bucketEntry
	striker sync.Map // key → *strikeCounter
	blocked sync.Map // key → time.Time (expiry)

	stopCh chan struct{}
	once   sync.Once

	// sweepInterval is the per-Limiter override for sweepLoop's
	// ticker cadence. Zero (the production case) falls back to
	// defaultSweepInterval. Set only from tests that need to drive
	// the timer branch deterministically without the 5-minute wait.
	sweepInterval time.Duration
}

type bucketEntry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // unix nanos; atomic so the sweep loop can read while requests update
}

type strikeCounter struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
}

// New constructs a Limiter. Does NOT start the background refresh
// loop — call Start(ctx) for that, typically from main / Init.
func New(opts Options) *Limiter {
	if opts.Rate == 0 {
		opts.Rate = rate.Every(defaultRateInterval)
	}
	if opts.Burst <= 0 {
		opts.Burst = defaultBurst
	}
	if opts.StrikesToBan <= 0 {
		opts.StrikesToBan = defaultStrikesToBan
	}
	if opts.StrikeWindow <= 0 {
		opts.StrikeWindow = defaultStrikeWindow
	}
	if opts.BanFor <= 0 {
		opts.BanFor = defaultBanFor
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = defaultRefreshInterval
	}
	if opts.BlockReason == "" {
		opts.BlockReason = defaultBlockReason
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	return &Limiter{
		rate:    opts.Rate,
		burst:   opts.Burst,
		strikes: opts.StrikesToBan,
		window:  opts.StrikeWindow,
		banFor:  opts.BanFor,
		store:   opts.BlockStore,
		reason:  opts.BlockReason,
		refresh: opts.RefreshInterval,
		log:     opts.Logger,
		onBan:   opts.OnBan,
		stopCh:  make(chan struct{}),
	}
}

// Start kicks off the background blocklist refresher and the
// idle-bucket sweeper. Safe to call multiple times — only the
// first call starts the goroutines. Pass a long-lived context;
// the goroutines exit when Stop is called or ctx is cancelled.
func (l *Limiter) Start(ctx context.Context) {
	l.once.Do(func() {
		if l.store != nil {
			// Prime the cache once synchronously so the very first
			// request after boot already sees existing bans.
			l.refreshOnce(ctx)
			go l.refreshLoop(ctx)
		}
		go l.sweepLoop(ctx)
	})
}

// Stop signals the background goroutines to exit. Idempotent.
func (l *Limiter) Stop() {
	select {
	case <-l.stopCh:
		// already closed
	default:
		close(l.stopCh)
	}
}

// Allow returns true if a request keyed by `key` should proceed.
// Decision order:
//
//  1. If key is in the blocked set (in-memory cache of BlockStore),
//     deny + record a Hit asynchronously.
//  2. If the per-key token bucket allows the request, permit.
//  3. Otherwise increment the strike counter; on the StrikesToBan-th
//     strike within StrikeWindow, persist a ban via BlockStore (if
//     configured) and add to the in-memory blocked set.
//
// Allow never blocks on I/O — BlockStore writes happen in a
// goroutine. Allow itself only touches in-memory state.
func (l *Limiter) Allow(ctx context.Context, key string) bool {
	if key == "" {
		// No key = nothing to limit. Caller should pre-validate.
		return true
	}
	if l.isBlocked(key) {
		l.recordHit(key)
		return false
	}
	if l.bucket(key).Allow() {
		return true
	}
	l.recordStrike(key)
	return false
}

// ApplyRemoteBlock applies a ban that originated outside this pod —
// typically an event from the NATS `ratelimit.block.created` stream
// published by a sibling pod or by ops tooling. The key starts being
// rejected on this pod immediately, without waiting for the next
// BlockStore refresh tick.
//
// Idempotent: a later expiresAt extends the ban, an earlier one is
// ignored. Past expiries are dropped silently (the event was stale).
func (l *Limiter) ApplyRemoteBlock(key string, expiresAt time.Time) {
	if key == "" || time.Now().After(expiresAt) {
		return
	}
	if v, ok := l.blocked.Load(key); ok {
		if existing := v.(time.Time); existing.After(expiresAt) {
			return
		}
	}
	l.blocked.Store(key, expiresAt)
}

// Middleware wraps an http.Handler with Allow. keyFn extracts the
// rate-limit key from the request (typically a closure around
// a trusted client-IP helper); when it returns "" the request bypasses
// the limiter (use that to exempt internal traffic).
//
// On deny: 429 with a Retry-After header derived from BanFor when
// the key is on the blocklist, or from the bucket's reservation
// otherwise. Body is a tiny text — APIs that need JSON should wrap
// the response writer.
func (l *Limiter) Middleware(keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFn(r)
			if l.Allow(r.Context(), key) {
				next.ServeHTTP(w, r)
				return
			}
			retry := l.retryAfterSeconds(key)
			if retry > 0 {
				w.Header().Set(constants.HeaderRetryAfter, strconv.Itoa(retry))
			}
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		})
	}
}

// ───── internals ────────────────────────────────────────────────

func (l *Limiter) bucket(key string) *rate.Limiter {
	now := time.Now().UnixNano()
	if v, ok := l.buckets.Load(key); ok {
		e := v.(*bucketEntry)
		e.lastSeen.Store(now)
		return e.limiter
	}
	e := &bucketEntry{limiter: rate.NewLimiter(l.rate, l.burst)}
	e.lastSeen.Store(now)
	actual, _ := l.buckets.LoadOrStore(key, e)
	return actual.(*bucketEntry).limiter
}

func (l *Limiter) isBlocked(key string) bool {
	v, ok := l.blocked.Load(key)
	if !ok {
		return false
	}
	expiresAt := v.(time.Time)
	if time.Now().After(expiresAt) {
		l.blocked.Delete(key)
		return false
	}
	return true
}

func (l *Limiter) recordStrike(key string) {
	v, _ := l.striker.LoadOrStore(key, &strikeCounter{})
	c := v.(*strikeCounter)
	c.mu.Lock()
	now := time.Now()
	if c.windowStart.IsZero() || now.Sub(c.windowStart) > l.window {
		c.windowStart = now
		c.count = 0
	}
	c.count++
	tripped := c.count >= l.strikes
	if tripped {
		// Reset so a subsequent burst after the ban triggers a
		// fresh escalation (instead of immediately re-banning).
		c.windowStart = time.Time{}
		c.count = 0
	}
	c.mu.Unlock()

	if !tripped {
		return
	}
	expiresAt := time.Now().Add(l.banFor)
	l.blocked.Store(key, expiresAt)
	l.log.Warn("ratelimit ban issued", "key", key, "reason", l.reason, "expires_at", expiresAt)
	if l.store != nil {
		go func() {
			// Detached context — we don't want a request cancel to
			// abort the persistence of the ban itself.
			bg, cancel := context.WithTimeout(context.Background(), blockstoreOpTimeout)
			defer cancel()
			if err := l.store.Add(bg, key, l.reason, expiresAt); err != nil {
				l.log.Error("ratelimit blockstore add failed", "key", key, "error", err)
			}
		}()
	}
	if l.onBan != nil {
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), blockstoreOpTimeout)
			defer cancel()
			l.onBan(bg, key, l.reason, expiresAt)
		}()
	}
}

func (l *Limiter) recordHit(key string) {
	if l.store == nil {
		return
	}
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), blockstoreOpTimeout)
		defer cancel()
		if err := l.store.Hit(bg, key); err != nil {
			l.log.Debug("ratelimit blockstore hit failed", "key", key, "error", err)
		}
	}()
}

func (l *Limiter) retryAfterSeconds(key string) int {
	if v, ok := l.blocked.Load(key); ok {
		secs := int(time.Until(v.(time.Time)).Seconds())
		if secs < minRetryAfterSeconds {
			return minRetryAfterSeconds
		}
		return secs
	}
	// Fall back to roughly one bucket-refill interval. Safe upper
	// bound that doesn't expose internal state.
	if d := time.Duration(float64(time.Second) / float64(l.rate)); d > 0 {
		secs := int(d.Seconds())
		if secs < minRetryAfterSeconds {
			return minRetryAfterSeconds
		}
		return secs
	}
	return fallbackRetryAfterSeconds
}

func (l *Limiter) refreshLoop(ctx context.Context) {
	t := time.NewTicker(l.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stopCh:
			return
		case <-t.C:
			l.refreshOnce(ctx)
		}
	}
}

func (l *Limiter) refreshOnce(ctx context.Context) {
	bg, cancel := context.WithTimeout(ctx, blockstoreOpTimeout)
	defer cancel()
	snap, err := l.store.Snapshot(bg)
	if err != nil {
		l.log.Error("ratelimit snapshot failed", "error", err)
		return
	}
	// Replace the in-memory set in two passes so a refresh that
	// returns N keys lands atomically from a reader's perspective:
	// add the new ones first, then drop anything that disappeared.
	seen := make(map[string]struct{}, len(snap))
	for k, exp := range snap {
		l.blocked.Store(k, exp)
		seen[k] = struct{}{}
	}
	l.blocked.Range(func(k, _ any) bool {
		if _, ok := seen[k.(string)]; !ok {
			l.blocked.Delete(k)
		}
		return true
	})
}

// sweepLoop evicts idle buckets / strike counters so memory stays
// bounded under high-cardinality traffic. A bucket with no Allow()
// in 30 min is regenerated cheaply on the next request.
// defaultSweepInterval is the production cadence at which sweepLoop
// calls sweepOnce. Tests override it per-Limiter via the unexported
// sweepInterval field so the tick branch is exercisable in
// milliseconds without waiting five production minutes.
const defaultSweepInterval = 5 * time.Minute

func (l *Limiter) sweepLoop(ctx context.Context) {
	interval := l.sweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stopCh:
			return
		case now := <-t.C:
			l.sweepOnce(now)
		}
	}
}

// sweepOnce performs one eviction pass. Pulled out of sweepLoop's
// timer case so tests can drive it deterministically without
// waiting on the 5-minute production cadence.
func (l *Limiter) sweepOnce(now time.Time) {
	cutoff := now.Add(-idleBucketAge).UnixNano()
	l.buckets.Range(func(k, v any) bool {
		if v.(*bucketEntry).lastSeen.Load() < cutoff {
			l.buckets.Delete(k)
		}
		return true
	})
	l.striker.Range(func(k, v any) bool {
		c := v.(*strikeCounter)
		c.mu.Lock()
		stale := !c.windowStart.IsZero() && now.Sub(c.windowStart) > l.window
		c.mu.Unlock()
		if stale {
			l.striker.Delete(k)
		}
		return true
	})
}
