package rulesengine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// compiledStep is the executor-ready form of a YAML stepSpec.
// Engine.Build walks every spec, resolves the rule by name (or
// recurses into the group), compiles the `when:` expression, and
// produces this tree. The executor never re-resolves or re-compiles
// anything — that's the whole point of the Build phase.
type compiledStep struct {
	kind  stepKind
	when  *compiledWhen
	with  StepConfig
	rule  Rule            // set when kind == stepKindRule
	group []*compiledStep // set when kind == stepKindParallel or stepKindSequence

	onSuccess []*compiledStep
	onFailure []*compiledStep

	// timeout caps the per-attempt duration when > 0. Set from
	// stepSpec.Timeout at compile time. Zero means "no per-attempt
	// deadline beyond the parent ctx".
	timeout time.Duration

	// retry is the parsed retry policy or nil if the step never
	// retries. Engine.compileStep validates and populates this.
	retry *compiledRetry
}

// compiledRetry is the executor-ready retry policy — same shape
// as retrySpec but with durations and backoff resolved into typed
// values so the runtime never reparses strings.
type compiledRetry struct {
	count   int
	backoff backoffKind
	initial time.Duration
	max     time.Duration
}

// backoffKind enumerates the supported retry delay schedules. The
// type is unexported because callers only ever pick one via the
// YAML string — there's no Go API to set it directly.
type backoffKind int

const (
	backoffExponential backoffKind = iota // default; doubles each attempt
	backoffLinear                         // Initial * attempt
	backoffConstant                       // always Initial
)

// compiledPipeline is the executor-ready form of a YAML pipelineSpec.
// Engine.Run looks up the pipeline by name and hands this to the
// executor.
type compiledPipeline struct {
	name string
	pick []string
	flow []*compiledStep
}

// run executes the pipeline against c. The Engine has already set
// c.Logger and c.Request; this function only worries about the
// step-by-step traversal. Returns the abort error untouched (so
// the caller can distinguish "denied" from "failed") and wraps
// any rule failure in a RuleError if it isn't one already.
func (p *compiledPipeline) run(c *Context) error {
	return runSequence(c, p.flow)
}

// runSequence runs steps in order, stopping at the first failure
// or abort. Used by both the top-level flow and explicit
// `sequence:` groups.
func runSequence(c *Context, steps []*compiledStep) error {
	for _, s := range steps {
		if err := runStep(c, s); err != nil {
			return err
		}
	}
	return nil
}

// runStep is the per-step dispatch. It evaluates `when:`, then
// hands off to the kind-specific runner. Branches (on_success /
// on_failure) are also handled here so every step shape gets the
// same treatment without duplicating logic in each runner.
func runStep(c *Context, s *compiledStep) error {
	if s.when != nil {
		ok, err := s.when.evaluate(c)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	var err error
	switch s.kind {
	case stepKindRule:
		err = runRule(c, s)
	case stepKindParallel:
		err = runParallel(c, s.group)
	case stepKindSequence, stepKindPipeline:
		// Sub-pipelines run inline as a sequence — the parent's
		// Context.Data flows straight in, the executor doesn't
		// recursively call Engine.Run.
		err = runSequence(c, s.group)
	default:
		return fmt.Errorf("rules: unknown step kind %d", s.kind)
	}

	return handleBranches(c, s, err)
}

// handleBranches runs on_success or on_failure after the step's
// main body has returned. Skip is treated as "no failure" so a
// skipped rule still triggers on_success — same way the dataplan
// engine does it. Abort short-circuits everything: branches don't
// run, the abort propagates.
func handleBranches(c *Context, s *compiledStep, err error) error {
	if IsAbort(err) {
		return err
	}
	if err == nil || errors.Is(err, Skip) {
		if len(s.onSuccess) > 0 {
			if e := runSequence(c, s.onSuccess); e != nil {
				return e
			}
		}
		// Skip never propagates past its step.
		return nil
	}
	// Real failure path.
	if len(s.onFailure) > 0 {
		if e := runSequence(c, s.onFailure); e != nil {
			// on_failure's own error wins — it's more specific
			// than the original (typically an Abort).
			return e
		}
		// on_failure handled the error → continue the outer flow.
		return nil
	}
	return err
}

// runRule dispatches the rule with its per-step `with:` config.
// One span wraps the entire call regardless of how many retry
// attempts happen underneath — the per-rule timing in the trace
// is the user's first-order interest, retry mechanics show up as
// span duration + log lines.
//
// Wraps any non-sentinel error in a RuleError so callers always
// see a tagged failure without rules having to format their own.
// No mutation of the caller's Context happens here: per-attempt
// deadlines derive a fresh Context via withCtx so parallel
// goroutines don't share a mutating ctx.
//
// Post-Run, the executor verifies the rule didn't write outside
// what it declared in Writes(). The Build-time parallel-overlap
// check uses that same declaration to prove race-freedom; without
// the runtime check, a lying rule would invisibly invalidate the
// proof. Caught here, the misbehaviour surfaces as a normal rule
// error the first time it happens.
func runRule(c *Context, s *compiledStep) error {
	if c.Logger != nil {
		c.Logger.LogAttrs(c.ctx, log.LevelDebug, "rule start", log.String("rule", s.rule.Name()))
	}
	_, span := startRuleSpan(c.ctx, s.rule.Name())
	start := time.Now()
	err := callRuleWithRetry(c, s)
	recordRuleRun(c.ctx, s.rule.Name(), err, time.Since(start))
	finishSpan(span, err)
	if err == nil || errors.Is(err, Skip) {
		return err
	}
	if IsAbort(err) {
		if internal := AbortInternal(err); internal != "" && c.Logger != nil {
			c.Logger.LogAttrs(c.ctx, log.LevelInfo,
				"rule aborted",
				log.String("rule", s.rule.Name()),
				log.String("reason", AbortReason(err)),
				log.String("internal", internal),
			)
		}
		return err
	}
	return &RuleError{Rule: s.rule.Name(), Err: err}
}

// callRuleWithRetry runs the rule once (fast path) or in a
// retry+timeout loop (when the YAML configured either). Skip and
// Abort short-circuit the loop on the first occurrence. Other
// errors retry up to retry.count additional times if the error
// reports itself as retryable (or is a deadline-exceeded from a
// per-attempt timeout).
func callRuleWithRetry(c *Context, s *compiledStep) error {
	if s.retry == nil && s.timeout == 0 {
		return callRuleOnce(c, s)
	}
	attempts := 1
	if s.retry != nil {
		attempts = s.retry.count + 1
	}
	// for {} (with internal returns) instead of for-attempt<attempts
	// so the compiler doesn't require a terminating statement we'd
	// have to mark unreachable. Every legitimate exit happens via
	// a return below.
	for attempt := 0; ; attempt++ {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 && c.Logger != nil {
			c.Logger.LogAttrs(c.ctx, log.LevelDebug, "rule retry",
				log.String("rule", s.rule.Name()),
				log.Int("attempt", attempt),
			)
		}
		err := callRuleOnce(perAttemptContext(c, s), s)
		if err == nil || errors.Is(err, Skip) || IsAbort(err) {
			return err
		}
		if !isRetryable(err) || attempt+1 >= attempts {
			return err
		}
		if waitErr := sleepBackoff(c.ctx, s.retry, attempt); waitErr != nil {
			return waitErr
		}
	}
}

// perAttemptContext returns the *Context the next attempt should
// run against. If a timeout is configured, ctx is wrapped with
// context.WithTimeout; otherwise the parent ctx is reused
// unchanged. Cancellation is bound to the lifetime of the
// returned Context via a deferred cancel in callRuleOnce.
func perAttemptContext(c *Context, s *compiledStep) *Context {
	if s.timeout <= 0 {
		return c
	}
	ctx, cancel := context.WithTimeout(c.ctx, s.timeout)
	derived := c.withCtx(ctx)
	derived.cancel = cancel
	return derived
}

// callRuleOnce performs exactly one attempt: attach a fresh write
// observer so this rule's Set calls are tracked in isolation,
// invoke Rule.Run, verify the recorded writes against the rule's
// declared Writes(), release any per-attempt cancel func.
//
// Per-rule observers (vs. a single Context-wide snapshot diff)
// are why this works for parallel groups: concurrent peers each
// have their own tracker, so rule A's writes don't show up as
// "undeclared" in rule B's verification.
func callRuleOnce(c *Context, s *compiledStep) error {
	if c.cancel != nil {
		defer c.cancel()
	}
	obs := &writeObserver{}
	tracked := c.withObserver(obs)
	err := s.rule.Run(tracked, s.with)
	if vErr := verifyWrites(s.rule, obs); vErr != nil {
		// Verification failure wins: a misbehaving rule that also
		// returns an error must surface the contract breach first
		// (we don't want the bad write to hide behind a downstream
		// symptom).
		return vErr
	}
	return err
}

// sleepBackoff blocks until the next attempt should start, or
// returns the parent context's error if it cancels first. Pulled
// out so the loop in callRuleWithRetry stays linear and the math
// is unit-testable on its own.
func sleepBackoff(ctx context.Context, r *compiledRetry, attempt int) error {
	delay := backoffDelay(r, attempt)
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoffDelay returns the wait time before attempt+1 given the
// retry policy. attempt is zero-based — the delay between the
// first and second attempt is at attempt=0. Capped at r.max so
// long-running outages don't push the total wait time out of
// bounds.
func backoffDelay(r *compiledRetry, attempt int) time.Duration {
	if r == nil || r.initial <= 0 {
		return 0
	}
	var d time.Duration
	switch r.backoff {
	case backoffConstant:
		d = r.initial
	case backoffLinear:
		d = r.initial * time.Duration(attempt+1)
	case backoffExponential:
		// 1<<attempt grows quickly; cap before the duration math
		// can overflow the int64 nanosecond range.
		if attempt >= 62 {
			d = r.max
		} else {
			d = r.initial << attempt
		}
	}
	if r.max > 0 && d > r.max {
		d = r.max
	}
	return d
}

// isRetryable reports whether err should trigger another attempt
// in the retry loop. The engine retries:
//
//   - context.DeadlineExceeded — almost always the per-attempt
//     timeout firing. Retrying is the whole point of pairing a
//     timeout with a retry.
//   - Any error in the chain that satisfies the Retryable
//     interface (or wraps one). Lets rule authors expose
//     transient/permanent distinctions on their custom errors.
//
// Anything else (plain errors, business-logic failures) is
// considered permanent — retrying would only delay the inevitable.
func isRetryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return IsRetryable(err)
}

// verifyWrites enforces the rule's declared Writes() contract
// using the per-call observer. Any key the rule wrote that isn't
// in rule.Writes() is a contract breach; the rule lied about its
// surface area and the Build-time parallel-overlap check can no
// longer guarantee race-freedom.
//
// Observer-based (vs. diffing Context.data before/after) so
// parallel peers don't false-positive each other's writes — each
// rule call has its own observer.
func verifyWrites(rule Rule, obs *writeObserver) error {
	if len(obs.keys) == 0 {
		return nil
	}
	declared := map[string]struct{}{}
	for _, k := range rule.Writes() {
		declared[k] = struct{}{}
	}
	var offending []string
	for _, k := range obs.keys {
		if _, allowed := declared[k]; !allowed {
			offending = append(offending, k)
		}
	}
	if len(offending) == 0 {
		return nil
	}
	// sort.Strings + offending[0] gives a deterministic message
	// regardless of the order writes happened in. Typical case is
	// one key; listing more would be noise so we name only the
	// first.
	sort.Strings(offending)
	return fmt.Errorf("wrote undeclared key %q (add it to %s.Writes())", offending[0], rule.Name())
}

// runParallel fans out steps onto their own goroutines. Build-time
// validation guarantees that no two rules in this group share a
// Writes() key, so the only legitimate concurrent access to
// Context.Data is on disjoint keys (safe via sync.Map).
//
// Error semantics: the first non-nil error wins; remaining
// goroutines still finish (we don't cancel mid-flight — Run calls
// are usually fast and the upstream context already provides the
// real cancellation signal). Abort wins over plain errors so a
// deliberate "no" surfaces ahead of an incidental failure.
func runParallel(c *Context, steps []*compiledStep) error {
	if len(steps) == 0 {
		return nil
	}
	_, span := startParallelSpan(c.ctx)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, s := range steps {
		wg.Add(1)
		go runParallelStep(c, s, &wg, &mu, &firstErr)
	}
	wg.Wait()
	finishSpan(span, firstErr)
	return firstErr
}

// runParallelStep is the goroutine body. Pulled out of runParallel
// per the CLAUDE.md "promote non-trivial closures" rule so the
// concurrency control reads cleanly at the call site.
func runParallelStep(c *Context, s *compiledStep, wg *sync.WaitGroup, mu *sync.Mutex, firstErr *error) {
	defer wg.Done()
	err := runStep(c, s)
	if err == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if *firstErr == nil || (IsAbort(err) && !IsAbort(*firstErr)) {
		*firstErr = err
	}
}
