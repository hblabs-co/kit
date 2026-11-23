package rulesengine

import "errors"

// Skip is the sentinel a Rule returns from Run when it should be
// silently skipped — the pipeline continues with the next step
// instead of treating the absence as a failure. Use this when the
// rule's preconditions don't hold and there's nothing meaningful
// to do (e.g. "no org id on the request, nothing to fetch").
//
// For the YAML-driven equivalent see the `when:` field in pipeline
// schemas — that short-circuits before Run is even called.
var Skip = errors.New("rules: skip")

// abortError is the concrete type behind Abort. Exported only via
// IsAbort + AbortReason + AbortInternal so callers can't construct
// one without going through the Abort helpers (which keep the
// public reason non-empty).
type abortError struct {
	// reason is the client-facing string. Goes verbatim into the
	// 403 response body. Treat as untrusted from the operator's
	// POV — anything written here ends up on the wire.
	reason string

	// internal is the log-only string. Stays inside the process:
	// HTTPHandler reads only `reason`; the executor logs `internal`
	// when non-empty. Use it for operator-only context (user IDs,
	// rule names, debug detail) you wouldn't want a client to see.
	internal string
}

func (e *abortError) Error() string { return "rules: abort: " + e.reason }

// Abort halts the pipeline cleanly with a client-facing reason.
// No further steps run, the error propagates up to Engine.Run, and
// HTTPHandler maps it to a 4xx with the reason in the body. Use
// it when the input is well-formed but the business logic decided
// "no" — unauthorized, plan too low, quota exceeded — not for
// unexpected failures (return a plain error for those).
//
// The string goes ON THE WIRE: don't put user IDs, internal rule
// names, stack traces, or anything else operator-only here. For
// log-only context, use AbortWithInternal.
//
// Empty reason is normalised to "aborted" so the response body
// never has a missing field.
func Abort(reason string) error {
	if reason == "" {
		reason = "aborted"
	}
	return &abortError{reason: reason}
}

// AbortWithInternal is Abort + an operator-only log message.
// `public` is the client-facing reason — same semantics as Abort.
// `internal` lands in the structured log line written by the
// executor and never crosses the HTTP boundary, so it's safe to
// include identifiers, IDs, "user X lacks role Y in org Z" detail.
//
// Use it when the public reason should stay generic ("forbidden")
// but the on-call needs the specifics to debug from logs.
func AbortWithInternal(public, internal string) error {
	if public == "" {
		public = "aborted"
	}
	return &abortError{reason: public, internal: internal}
}

// IsAbort reports whether err is the result of an Abort call (or
// wraps one). Use it when you need to branch on abort-vs-real-error
// outside the HTTP handler.
func IsAbort(err error) bool {
	_, ok := errors.AsType[*abortError](err)
	return ok
}

// AbortReason returns the CLIENT-FACING reason an Abort-error was
// created with, or "" if err is not an abort. Safe to render to the
// wire; HTTPHandler uses this for the response body.
func AbortReason(err error) string {
	if a, ok := errors.AsType[*abortError](err); ok {
		return a.reason
	}
	return ""
}

// AbortInternal returns the operator-only message attached via
// AbortWithInternal, or "" if err isn't an abort or the abort
// carried no internal context. Intended for log enrichment in the
// executor; do not render it on the wire — it may contain detail
// you wouldn't expose to a client.
func AbortInternal(err error) string {
	if a, ok := errors.AsType[*abortError](err); ok {
		return a.internal
	}
	return ""
}

// Retryable is the opt-in interface a rule's error type can
// implement to signal "this is transient — try again". The
// executor checks it via errors.As inside the retry loop, so any
// error in the chain can carry the marker (typically the bottom
// of a wrap stack).
//
// Without a Retryable in the chain (and without a
// context.DeadlineExceeded from a per-attempt timeout) the engine
// treats the failure as permanent and propagates immediately.
type Retryable interface {
	Retryable() bool
}

// retryableError is the canonical wrapper most rules use: wrap an
// inner error with RetryableError(err) to flag it as transient.
// Exposed only via the helper to keep the package surface narrow.
type retryableError struct{ err error }

func (r *retryableError) Error() string   { return r.err.Error() }
func (r *retryableError) Unwrap() error   { return r.err }
func (r *retryableError) Retryable() bool { return true }

// RetryableError wraps err in a Retryable marker so the engine's
// retry loop knows it can re-invoke the rule. Use it inside
// rules' Run methods when a transient failure (network blip,
// timeout, "service unavailable") should trigger a backoff retry
// rather than a hard 500:
//
//	if err := r.DS.Get(ctx, id); err != nil {
//	    if errors.Is(err, context.DeadlineExceeded) {
//	        return rulesengine.RetryableError(err)
//	    }
//	    return err
//	}
//
// A nil err returns nil — convenient for one-liner returns.
func RetryableError(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{err: err}
}

// IsRetryable reports whether err is or wraps a Retryable marker.
// Companion to RetryableError for callers that want to inspect an
// error without rebuilding the chain. The engine uses this
// internally too — keeping the predicate in one place.
func IsRetryable(err error) bool {
	var r Retryable
	return errors.As(err, &r) && r.Retryable()
}

// RuleError wraps an error returned from Run with the rule name
// that produced it. The Engine wraps every non-Skip, non-Abort
// error this way so log lines and HTTP responses can identify which
// rule failed without the rule itself having to format that into
// its message.
type RuleError struct {
	Rule string
	Err  error
}

func (e *RuleError) Error() string { return "rules: " + e.Rule + ": " + e.Err.Error() }
func (e *RuleError) Unwrap() error { return e.Err }
