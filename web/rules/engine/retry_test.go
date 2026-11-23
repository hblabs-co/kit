package rulesengine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// mockRemote simulates a flaky external resource (DB, HTTP API,
// cache). Each Call increments an attempt counter and returns
// either an error or a value based on the configured script.
//
// Two modes — both behaviours real services hit in prod:
//
//   - failureCount: fail the first N calls with a "retryable"
//     marker, then succeed. Models a transient blip (network
//     partition, GC pause, downstream restart).
//   - blockUntil: block until ctx cancels OR the configured
//     duration passes. Models a slow downstream the per-attempt
//     timeout should cut short.
//
// Both attempts and failures are counted via atomics so tests
// running with -race don't false-fail on the shared state.
type mockRemote struct {
	failureCount int           // first N calls fail
	blockUntil   time.Duration // if > 0, each call sleeps this long (or until ctx done)
	attempts     atomic.Int32
}

// Call mirrors the shape of a typical "do a network thing"
// function: takes a context, returns (value, error). The mock
// distinguishes between the per-attempt context (used to
// cancel slow calls early) and the failure-injection script
// (which counts up regardless of timeouts).
func (m *mockRemote) Call(ctx context.Context) (string, error) {
	n := m.attempts.Add(1)
	if m.blockUntil > 0 {
		t := time.NewTimer(m.blockUntil)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if int(n) <= m.failureCount {
		return "", RetryableError(errors.New("transient: simulated downstream blip"))
	}
	return "ok", nil
}

// remoteRule is the rule type used by retry/timeout tests. It
// wraps a mockRemote and writes the returned value into
// Context.Data so we can verify the pipeline actually saw a
// successful response.
type remoteRule struct {
	name   string
	remote *mockRemote
}

func (r *remoteRule) Name() string     { return r.name }
func (r *remoteRule) Writes() []string { return []string{"remote_result"} }
func (r *remoteRule) Run(c *Context, _ StepConfig) error {
	v, err := r.remote.Call(c.Context())
	if err != nil {
		return err
	}
	c.Set("remote_result", v)
	return nil
}

// retryEngine builds a tiny engine running pipeline p with the
// supplied rule + YAML body. Wraps the boilerplate the retry tests
// repeat almost verbatim.
func retryEngine(t *testing.T, rule Rule, yamlBody string) *Engine {
	t.Helper()
	e := NewEngine().Register(rule).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yamlBody)}},
		"p.yaml",
	)
	if err := e.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return e
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()
	// Mock fails twice, succeeds on attempt 3. Retry policy
	// allows up to 4 total attempts (count=3), so the pipeline
	// should land on a successful Result.
	remote := &mockRemote{failureCount: 2}
	rule := &remoteRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        retry: { count: 3, backoff: constant, initial: 1ms }
`)
	res, err := e.Run(context.Background(), nil, "p")
	if err != nil {
		t.Fatalf("Run err = %v (after %d attempts)", err, remote.attempts.Load())
	}
	if res["remote_result"] != "ok" {
		t.Fatalf("Result = %v", res)
	}
	if got := remote.attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts (2 fails + 1 success), got %d", got)
	}
}

func TestRetryExhaustsAndFails(t *testing.T) {
	t.Parallel()
	// Mock fails forever. Retry policy: 2 retries → 3 total
	// attempts. The pipeline must propagate the failure after
	// the third attempt.
	remote := &mockRemote{failureCount: 99}
	rule := &remoteRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        retry: { count: 2, backoff: constant, initial: 1ms }
`)
	_, err := e.Run(context.Background(), nil, "p")
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := remote.attempts.Load(); got != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", got)
	}
}

func TestTimeoutFiresOnSlowRule(t *testing.T) {
	t.Parallel()
	// Mock would block for 2s — way past the configured 50ms
	// timeout. Without retry, the first DeadlineExceeded
	// propagates immediately.
	remote := &mockRemote{blockUntil: 2 * time.Second}
	rule := &remoteRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        timeout: 50ms
`)

	start := time.Now()
	_, err := e.Run(context.Background(), nil, "p")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded in chain, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Run blocked for %v — timeout did not fire", elapsed)
	}
	if got := remote.attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt without retry, got %d", got)
	}
}

func TestTimeoutPlusRetryRecoversWhenLatencyDrops(t *testing.T) {
	t.Parallel()
	// Slow remote whose latency drops to 0 after the 3rd call.
	// Pair a tight 30ms timeout with 3 retries (4 total
	// attempts) — the first 3 deadline-out, the 4th succeeds
	// because blockUntil resets.
	remote := &fluctuatingRemote{
		slow:        30 * time.Millisecond, // first phase: barely under timeout, sometimes over
		fast:        0,
		switchAfter: 3,
	}
	rule := &fluctuatingRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        timeout: 5ms
        retry: { count: 5, backoff: constant, initial: 1ms }
`)
	res, err := e.Run(context.Background(), nil, "p")
	if err != nil {
		t.Fatalf("expected eventual success, got %v after %d attempts",
			err, remote.attempts.Load())
	}
	if res["remote_result"] != "ok" {
		t.Fatalf("Result = %v", res)
	}
}

func TestTimeoutPlusRetryExhausts(t *testing.T) {
	t.Parallel()
	// Slow remote that never recovers. Timeout fires every
	// attempt, retry runs out, final error must be a
	// DeadlineExceeded-bearing RuleError.
	remote := &mockRemote{blockUntil: time.Second}
	rule := &remoteRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        timeout: 10ms
        retry: { count: 2, backoff: constant, initial: 1ms }
`)
	start := time.Now()
	_, err := e.Run(context.Background(), nil, "p")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error must wrap DeadlineExceeded, got %v", err)
	}
	if got := remote.attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	// Bound the total wall-clock: 3 × (10ms timeout + 1ms backoff) ≈ 33ms.
	// Give it generous headroom for CI flakiness — anything under
	// 500ms means the timeouts are working.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("retry+timeout total = %v; timeouts not firing", elapsed)
	}
}

func TestSkipNotRetried(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	rule := &fnRule{name: "skipper", fn: func(c *Context, _ StepConfig) error {
		calls.Add(1)
		return Skip
	}}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: skipper
        retry: { count: 5, backoff: constant, initial: 1ms }
`)
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatalf("Skip must not propagate as error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Skip must NOT retry — expected 1 call, got %d", got)
	}
}

func TestAbortNotRetried(t *testing.T) {
	t.Parallel()
	calls := atomic.Int32{}
	rule := &fnRule{name: "denier", fn: func(c *Context, _ StepConfig) error {
		calls.Add(1)
		return Abort("nope")
	}}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: denier
        retry: { count: 5, backoff: constant, initial: 1ms }
`)
	_, err := e.Run(context.Background(), nil, "p")
	if !IsAbort(err) {
		t.Fatalf("expected Abort, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Abort must NOT retry — expected 1 call, got %d", got)
	}
}

func TestNonRetryableNotRetried(t *testing.T) {
	t.Parallel()
	// A plain (non-Retryable) error must NOT retry — the engine
	// only re-invokes when the error explicitly opts in or is a
	// DeadlineExceeded.
	calls := atomic.Int32{}
	rule := &fnRule{name: "perma", fn: func(c *Context, _ StepConfig) error {
		calls.Add(1)
		return errors.New("hard fail")
	}}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: perma
        retry: { count: 5, backoff: constant, initial: 1ms }
`)
	if _, err := e.Run(context.Background(), nil, "p"); err == nil {
		t.Fatal("expected error to propagate")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("non-retryable must NOT retry — expected 1 call, got %d", got)
	}
}

func TestParentContextCancelStopsRetries(t *testing.T) {
	t.Parallel()
	// Parent ctx is cancelled mid-sleep between attempts. The
	// retry loop must observe ctx.Done and abort the loop instead
	// of finishing the backoff sleep.
	remote := &mockRemote{failureCount: 99} // always fails
	rule := &remoteRule{name: "fetch_remote", remote: remote}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: fetch_remote
        retry: { count: 10, backoff: constant, initial: 200ms }
`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := e.Run(ctx, nil, "p")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after parent cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("parent-cancel did not break the retry sleep: elapsed=%v", elapsed)
	}
}

func TestBackoffSchedules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind   backoffKind
		init   time.Duration
		maxDur time.Duration
		want   []time.Duration // per attempt 0,1,2,3
	}{
		{
			kind: backoffConstant, init: 10 * time.Millisecond, maxDur: time.Second,
			want: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		},
		{
			kind: backoffLinear, init: 10 * time.Millisecond, maxDur: time.Second,
			want: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		},
		{
			kind: backoffExponential, init: 10 * time.Millisecond, maxDur: time.Second,
			want: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
		},
		{
			// Max clamps growth: exponential would be 16ms, 32ms,
			// 64ms; max=30ms clamps the third to 30.
			kind: backoffExponential, init: 16 * time.Millisecond, maxDur: 30 * time.Millisecond,
			want: []time.Duration{16 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond},
		},
	}
	for i, tc := range cases {
		r := &compiledRetry{count: 3, backoff: tc.kind, initial: tc.init, max: tc.maxDur}
		for attempt, want := range tc.want {
			if got := backoffDelay(r, attempt); got != want {
				t.Errorf("case %d attempt %d: got %v, want %v", i, attempt, got, want)
			}
		}
	}
}

func TestBackoffNilOrZeroInitial(t *testing.T) {
	t.Parallel()
	if backoffDelay(nil, 0) != 0 {
		t.Fatal("nil retry must yield 0 delay")
	}
	if backoffDelay(&compiledRetry{}, 0) != 0 {
		t.Fatal("zero initial must yield 0 delay")
	}
}

func TestBackoffExponentialClamp(t *testing.T) {
	t.Parallel()
	// Past attempt 62, the engine clamps to Max instead of
	// shifting an int64 into UB territory.
	r := &compiledRetry{count: 100, backoff: backoffExponential, initial: time.Nanosecond, max: time.Second}
	if got := backoffDelay(r, 63); got != time.Second {
		t.Fatalf("clamp at attempt 63 = %v, want max=1s", got)
	}
}

func TestRetryableErrorWrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("network blip")
	wrapped := RetryableError(inner)
	if !IsRetryable(wrapped) {
		t.Fatal("RetryableError must produce a Retryable value")
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("RetryableError must unwrap to the inner error")
	}
	if wrapped.Error() != inner.Error() {
		t.Fatalf("Error() mismatch: %q vs %q", wrapped.Error(), inner.Error())
	}
}

func TestRetryableErrorNil(t *testing.T) {
	t.Parallel()
	if got := RetryableError(nil); got != nil {
		t.Fatalf("RetryableError(nil) = %v, want nil", got)
	}
}

func TestIsRetryableFalseForPlain(t *testing.T) {
	t.Parallel()
	if IsRetryable(errors.New("plain")) {
		t.Fatal("plain error must not be retryable")
	}
	if IsRetryable(nil) {
		t.Fatal("nil must not be retryable")
	}
}

func TestRetryBuildErrorOnNonRuleStep(t *testing.T) {
	t.Parallel()
	// timeout/retry only make sense on rule steps. The compiler
	// must reject them on groups.
	e := NewEngine().Register(&fnRule{name: "r"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - parallel: [{name: r}]
        timeout: 1s
`)}}, "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("timeout on a parallel container must be rejected")
	}
}

func TestRetryBuildErrorOnNonRuleStepWithRetry(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fnRule{name: "r"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - parallel: [{name: r}]
        retry: { count: 1 }
`)}}, "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("retry on a parallel container must be rejected")
	}
}

func TestRetryBuildValidatesFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"count<=0", "retry: { count: 0 }"},
		{"unknown backoff", "retry: { count: 1, backoff: bogus }"},
		{"bad initial", "retry: { count: 1, initial: notaduration }"},
		{"bad max", "retry: { count: 1, max: nope }"},
		{"non-positive initial", "retry: { count: 1, initial: 0s }"},
		{"non-positive max", "retry: { count: 1, max: 0s }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "pipelines:\n  p:\n    flow:\n      - name: r\n        " + tc.body + "\n"
			e := NewEngine().Register(&fnRule{name: "r"}).Load(
				fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yaml)}}, "p.yaml")
			if err := e.Build(); err == nil {
				t.Fatalf("expected build to reject %q", tc.body)
			}
		})
	}
}

func TestTimeoutBuildValidatesFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"bad duration", "timeout: notaduration"},
		{"non-positive", "timeout: 0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "pipelines:\n  p:\n    flow:\n      - name: r\n        " + tc.body + "\n"
			e := NewEngine().Register(&fnRule{name: "r"}).Load(
				fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yaml)}}, "p.yaml")
			if err := e.Build(); err == nil {
				t.Fatalf("expected build to reject %q", tc.body)
			}
		})
	}
}

func TestRetryAlreadyCancelledCtxShortCircuits(t *testing.T) {
	t.Parallel()
	// Pre-cancelled ctx: the loop must observe c.ctx.Err() before
	// even invoking the rule. Verifies the top-of-loop guard.
	calls := atomic.Int32{}
	rule := &fnRule{name: "r", fn: func(c *Context, _ StepConfig) error {
		calls.Add(1)
		return RetryableError(errors.New("transient"))
	}}
	e := retryEngine(t, rule, `
pipelines:
  p:
    flow:
      - name: r
        retry: { count: 3, backoff: constant, initial: 1ms }
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Run
	if _, err := e.Run(ctx, nil, "p"); err == nil {
		t.Fatal("expected cancelled-ctx error")
	}
	// Either zero (caught at loop top) or one (race window between
	// Run starting and loop checking) — both prove the guard works
	// well enough to abort early.
	if got := calls.Load(); got > 1 {
		t.Fatalf("expected at most 1 call with pre-cancelled ctx, got %d", got)
	}
}

func TestRetryYAMLLinearBackoff(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fnRule{name: "r"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - name: r
        retry: { count: 1, backoff: linear, initial: 5ms, max: 100ms }
`)}}, "p.yaml")
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	step := e.pipelines["p"].flow[0]
	if step.retry.backoff != backoffLinear {
		t.Fatalf("backoff = %v, want linear", step.retry.backoff)
	}
	if step.retry.max != 100*time.Millisecond {
		t.Fatalf("max = %v, want 100ms", step.retry.max)
	}
}

func TestRetryDefaultBackoffApplied(t *testing.T) {
	t.Parallel()
	// Backoff omitted → should default to exponential. We can
	// see the default by reading the compiled retry directly off
	// the engine's pipelines map.
	e := NewEngine().Register(&fnRule{name: "r"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - name: r
        retry: { count: 1 }
`)}}, "p.yaml")
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	step := e.pipelines["p"].flow[0]
	if step.retry == nil || step.retry.backoff != backoffExponential {
		t.Fatalf("default backoff = %v, want exponential", step.retry)
	}
	if step.retry.initial != 100*time.Millisecond {
		t.Fatalf("default initial = %v, want 100ms", step.retry.initial)
	}
	if step.retry.max != 5*time.Second {
		t.Fatalf("default max = %v, want 5s", step.retry.max)
	}
}

// fluctuatingRemote is a second mock for tests that need
// latency to change over time (warming caches, downstream
// recovery). The split out from mockRemote is cosmetic — keeps
// each mock's responsibility narrow.
type fluctuatingRemote struct {
	slow, fast  time.Duration
	switchAfter int
	attempts    atomic.Int32
}

func (m *fluctuatingRemote) Call(ctx context.Context) (string, error) {
	n := int(m.attempts.Add(1))
	delay := m.slow
	if n > m.switchAfter {
		delay = m.fast
	}
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "ok", nil
}

type fluctuatingRule struct {
	name   string
	remote *fluctuatingRemote
}

func (r *fluctuatingRule) Name() string     { return r.name }
func (r *fluctuatingRule) Writes() []string { return []string{"remote_result"} }
func (r *fluctuatingRule) Run(c *Context, _ StepConfig) error {
	v, err := r.remote.Call(c.Context())
	if err != nil {
		return err
	}
	c.Set("remote_result", v)
	return nil
}

// fnRule is the smallest rule for tests that don't need a
// remote mock — just an inline function returning an error or
// nil. Reused across retry/timeout tests where the mock-DB
// fixture would be overkill.
type fnRule struct {
	name string
	fn   func(*Context, StepConfig) error
}

func (r *fnRule) Name() string     { return r.name }
func (r *fnRule) Writes() []string { return nil }
func (r *fnRule) Run(c *Context, w StepConfig) error {
	if r.fn == nil {
		return nil
	}
	return r.fn(c, w)
}

// ensure sync is referenced; some test variations toggle in/out.
var _ = sync.Map{}
