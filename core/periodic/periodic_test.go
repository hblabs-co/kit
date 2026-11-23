package periodic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLogger captures Info/Error calls so tests can assert what
// periodic logs without depending on the real slog plumbing.
type fakeLogger struct {
	mu     sync.Mutex
	infos  []logEntry
	errors []logEntry
}

type logEntry struct {
	msg  string
	args []any
}

func (f *fakeLogger) Info(msg string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos = append(f.infos, logEntry{msg: msg, args: args})
}

func (f *fakeLogger) Error(msg string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, logEntry{msg: msg, args: args})
}

func (f *fakeLogger) infoCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.infos)
}

func (f *fakeLogger) errorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errors)
}

// TestNextWait_NoJitterReturnsBase — jitter=0 must produce a
// deterministic schedule. This is the documented escape hatch when
// callers want predictable timing (single-replica deployments, tests).
func TestNextWait_NoJitterReturnsBase(t *testing.T) {
	base := 30 * time.Second
	for i := 0; i < 100; i++ {
		got := nextWait(base, 0)
		if got != base {
			t.Fatalf("nextWait(%v, 0) = %v, want %v", base, got, base)
		}
	}
}

// TestNextWait_StaysInBounds — with jitter > 0, the result must fall
// in [base-jitter, base+jitter). We sample a lot of times because the
// distribution is uniform; one shot would tell us nothing.
func TestNextWait_StaysInBounds(t *testing.T) {
	base := 1 * time.Second
	jitter := 200 * time.Millisecond
	min, max := base-jitter, base+jitter
	for i := 0; i < 1000; i++ {
		got := nextWait(base, jitter)
		if got < min || got >= max {
			t.Fatalf("nextWait out of bounds: got=%v, want in [%v, %v)", got, min, max)
		}
	}
}

// TestNextWait_ClampsNegative — when jitter is larger than base the
// raw computation could produce a negative duration. Clamp returns
// base in that case so callers never get a negative timer reset.
func TestNextWait_ClampsNegative(t *testing.T) {
	base := 1 * time.Millisecond
	jitter := 1 * time.Hour
	// Trial enough times that we'd almost certainly hit a negative
	// branch if it weren't clamped. base is the only safe fallback;
	// any non-negative result is acceptable.
	for i := 0; i < 1000; i++ {
		got := nextWait(base, jitter)
		if got < 0 {
			t.Fatalf("nextWait returned negative: %v", got)
		}
	}
}

// TestRun_RunOnStartFiresImmediately — the documented contract is
// that RunOnStart=true calls Fn before the first Interval-long wait,
// so cache-priming jobs don't sit idle for 24h on boot.
func TestRun_RunOnStartFiresImmediately(t *testing.T) {
	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, Job{
		Name:       "test",
		Interval:   1 * time.Hour, // long: only RunOnStart should fire
		RunOnStart: true,
		Fn: func(context.Context) error {
			fired <- struct{}{}
			return nil
		},
		Logger: &fakeLogger{},
	})

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Fn did not fire on start")
	}
}

// TestRun_NoRunOnStartWaitsForInterval — RunOnStart defaults to
// false. With a long Interval the job must NOT fire during the test
// window; if it does we've regressed the "wait first" guarantee that
// keeps boot from stampeding upstreams.
func TestRun_NoRunOnStartWaitsForInterval(t *testing.T) {
	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, Job{
		Name:     "test",
		Interval: 1 * time.Hour,
		Fn: func(context.Context) error {
			fired <- struct{}{}
			return nil
		},
		Logger: &fakeLogger{},
	})

	select {
	case <-fired:
		t.Fatal("Fn fired before Interval elapsed")
	case <-time.After(50 * time.Millisecond):
		// ok — still waiting, as expected
	}
}

// TestRun_ErrorUsesRetryInterval — after Fn returns an error the next
// wakeup must use RetryInterval, not Interval. We exaggerate the gap
// (Interval=1h vs RetryInterval=10ms) so a regression — using Interval
// after a failure — would block the test until the deadline.
func TestRun_ErrorUsesRetryInterval(t *testing.T) {
	fired := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, Job{
		Name:          "test",
		Interval:      1 * time.Hour,         // would never fire again
		RetryInterval: 10 * time.Millisecond, // fast retry path
		RunOnStart:    true,
		Fn: func(context.Context) error {
			fired <- struct{}{}
			return errors.New("boom")
		},
		Logger: &fakeLogger{},
	})

	// First fire is RunOnStart.
	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first fire (RunOnStart) did not happen")
	}
	// Second fire should follow RetryInterval (~10ms), not Interval (1h).
	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second fire did not happen within RetryInterval window — Interval probably used after error")
	}
}

// TestRun_ContextCancelStops — cancelling ctx must terminate the
// goroutine. We let it tick once, cancel, then verify no further
// invocations happen during a quiet window. Lifecycle correctness here
// is the whole reason we don't expose a deregister API.
func TestRun_ContextCancelStops(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	bump := func() {
		mu.Lock()
		calls++
		mu.Unlock()
	}
	read := func() int32 {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	ctx, cancel := context.WithCancel(context.Background())
	Run(ctx, Job{
		Name:       "test",
		Interval:   10 * time.Millisecond,
		RunOnStart: true,
		Fn: func(context.Context) error {
			bump()
			return nil
		},
		Logger: &fakeLogger{},
	})

	// Let it run a few times.
	time.Sleep(50 * time.Millisecond)
	cancel()
	// Snapshot, wait for any in-flight tick to drain, snapshot again.
	before := read()
	time.Sleep(80 * time.Millisecond)
	after := read()
	// One extra invocation can race in if Fn was already running
	// when cancel landed, but it must not keep ticking.
	if after-before > 1 {
		t.Fatalf("Fn kept firing after cancel: before=%d after=%d", before, after)
	}
}

// TestRun_LoggerReceivesSuccessAndError — runOnce emits Info on
// success and Error on failure. This is the contract every caller
// relies on for uniform job observability, so flip Fn between the
// two outcomes and assert both branches were hit.
func TestRun_LoggerReceivesSuccessAndError(t *testing.T) {
	fl := &fakeLogger{}
	var n int
	var mu sync.Mutex
	done := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, Job{
		Name:          "test",
		Interval:      10 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
		RunOnStart:    true,
		Fn: func(context.Context) error {
			mu.Lock()
			n++
			i := n
			mu.Unlock()
			switch i {
			case 1:
				return errors.New("boom") // first call: error path
			case 2:
				select {
				case done <- struct{}{}:
				default:
				}
				return nil // second call: success path
			default:
				return nil
			}
		},
		Logger: fl,
	})

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("did not observe both error and success fires in time")
	}
	// Cancel and let the in-flight tick (if any) settle so counts
	// don't race with the assertion below.
	cancel()
	time.Sleep(30 * time.Millisecond)

	if got := fl.errorCount(); got < 1 {
		t.Errorf("errorCount = %d, want >= 1", got)
	}
	if got := fl.infoCount(); got < 1 {
		t.Errorf("infoCount = %d, want >= 1", got)
	}
}

// TestRun_NilLoggerDoesNotPanic — Job.Logger is documented as
// optional ("nil = log.Default"). The default fallback path must not
// panic when Fn fires; this guards the nil-check in Run that wires up
// the package logger when the caller didn't bring their own.
func TestRun_NilLoggerDoesNotPanic(t *testing.T) {
	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx, Job{
		Name:       "test",
		Interval:   1 * time.Hour,
		RunOnStart: true,
		Fn: func(context.Context) error {
			fired <- struct{}{}
			return nil
		},
		// Logger intentionally nil.
	})

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Fn did not fire with nil Logger")
	}
}
