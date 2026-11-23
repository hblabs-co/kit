package rulesengine

import (
	"context"
	"net/http"
	"sync"

	"github.com/hblabs-co/kit/core/log"
)

// Context is the per-pipeline-run state shared across every Rule
// in a flow. It's a value the Engine creates once at the top of
// Run and threads through every step; rules read from it, write
// into Data, and signal control via the error they return.
//
// Concurrency: parallel groups run rules on multiple goroutines
// simultaneously. Data is therefore behind a sync.Map encapsulated
// in this type — callers go through Get/Set, not the map directly.
// The Engine validates at Build time that no two parallel rules
// declare overlapping Writes(), so the only legitimate races
// (different keys at the same time) are safe.
type Context struct {
	// Request is the HTTP request that triggered the pipeline. Always
	// non-nil for HTTP entry points; the inline form (Engine.Run with
	// req=nil) gets a stub *http.Request the engine constructs so
	// rules don't have to nil-check.
	Request *http.Request

	// Logger is the per-run logger. Pre-bound with pipeline name and
	// any correlation IDs the parent http.Request context carries
	// (request_id, device_id, organization_id from core/log).
	// Rules should use this instead of log.Default() so their lines
	// show up under the right pipeline in aggregated logs.
	Logger *log.Logger

	// data holds the running state: a key set by an earlier rule,
	// read by a later one. Pointer so withCtx-derived Contexts can
	// share the same underlying map without copying sync.Map's
	// internal state (which the stdlib forbids). Unexported so
	// external code goes through Get/Set; the engine controls the
	// storage type. sync.Map pays a small read overhead vs map +
	// RWMutex but keeps the API lock-free on writes when rules
	// touch disjoint keys, which is the common case after the
	// Build-time overlap check.
	data *sync.Map

	// ctx is the deadlines/cancellation source. Mirrors what
	// Request.Context() would return; carried separately so Engine.Run
	// can install timeouts without mutating the caller's request.
	// Per-step timeout / retry attempts derive a new Context via
	// withCtx with a fresh ctx, sharing the same data pointer.
	ctx context.Context

	// cancel is the cancel func paired with a per-attempt timeout
	// ctx. Only the executor's per-attempt Context carries one;
	// callRuleOnce defers cancel() so the timeout's resources are
	// released regardless of whether the rule returned cleanly.
	// nil on the top-level pipeline Context (whose lifetime is
	// bounded by the caller).
	cancel context.CancelFunc

	// observer captures the keys a single rule's Run call writes,
	// so the executor can verify them against the rule's declared
	// Writes() without comparing the shared Context.data — which
	// would race with peer parallel rules. Each rule invocation
	// runs against a Context with its own observer; nil means
	// "not tracking" (top-level Context, when:-eval Context, etc).
	observer *writeObserver
}

// writeObserver records every key the current rule wrote, so
// verifyWrites can check the recorded set against rule.Writes()
// instead of diffing the shared Context.data. Per-call isolation
// fixes the parallel false-positive where rule A's writes would
// appear in rule B's diff.
type writeObserver struct {
	mu   sync.Mutex
	keys []string
}

// record appends key to the observer's write log. Called by
// Context.Set when the Context has an observer attached. Locked
// because a single rule can in principle Set concurrently from
// its own goroutines — uncommon but technically allowed.
func (o *writeObserver) record(key string) {
	o.mu.Lock()
	o.keys = append(o.keys, key)
	o.mu.Unlock()
}

// NewContext builds a *Context outside of Engine.Run — useful for
// tests that exercise a single rule, or for callers that want to
// drive a rule manually without composing a pipeline. The Engine
// uses this internally; the constructor is exported so external
// callers don't have to reimplement the sync.Map plumbing.
//
// ctx must be non-nil (use context.Background() if you have no
// real context). req may be nil; rules that read it must nil-check.
func NewContext(ctx context.Context, req *http.Request) *Context {
	return &Context{Request: req, data: &sync.Map{}, ctx: ctx}
}

// withCtx returns a *Context that shares Request, Logger, and the
// underlying data map with c but carries a different cancellation
// context. Used by the executor's per-attempt timeout wiring: each
// retry / timeout layer wants its own deadline without mutating
// the parent's ctx (which would race in parallel groups).
//
// Internal — exposed only inside the package because the use case
// is bounded to the executor.
func (c *Context) withCtx(ctx context.Context) *Context {
	return &Context{
		Request:  c.Request,
		Logger:   c.Logger,
		data:     c.data,
		ctx:      ctx,
		observer: c.observer,
	}
}

// withObserver returns a *Context that shares everything with c
// but installs obs as the write tracker. Used by callRuleOnce to
// scope per-rule write observation: each rule invocation gets its
// own observer so concurrent peers in a parallel group don't
// pollute each other's write logs.
func (c *Context) withObserver(obs *writeObserver) *Context {
	return &Context{
		Request:  c.Request,
		Logger:   c.Logger,
		data:     c.data,
		ctx:      c.ctx,
		cancel:   c.cancel,
		observer: obs,
	}
}

// Context returns the cancellation/deadline-bearing context.Context
// for the running pipeline. Use it for outgoing network calls,
// database queries, etc., so rules cancel cleanly when the request
// is aborted upstream.
func (c *Context) Context() context.Context { return c.ctx }

// Get returns the value stored under key by a previous rule. The
// boolean disambiguates "key was set to nil" from "key was never
// set" — most callers ignore it.
func (c *Context) Get(key string) (any, bool) {
	return c.data.Load(key)
}

// Set stores v under key. The Engine's Build-time validation
// guarantees that within a parallel group no two rules write the
// same key, so Set never has to coordinate beyond what sync.Map
// already provides.
//
// If this Context was derived for a tracked rule call, Set also
// records the key in the observer so the executor's
// Writes()-contract verification can compare the recorded set to
// the rule's declaration after Run returns.
func (c *Context) Set(key string, v any) {
	if c.observer != nil {
		c.observer.record(key)
	}
	c.data.Store(key, v)
}

// snapshot copies the current data into a plain Result map. Called
// by Engine.Run once the pipeline finishes (or aborts), then passed
// through Result.Pick if the pipeline declared `pick:`. Internal —
// rules never need a snapshot; they read individual keys.
func (c *Context) snapshot() Result {
	out := make(Result)
	c.data.Range(func(k, v any) bool {
		if ks, ok := k.(string); ok {
			out[ks] = v
		}
		return true
	})
	return out
}

// StepConfig is a typed view over the `with:` map declared on a
// YAML step. Stored as map[string]any (yaml.v3's default for
// untyped maps) and accessed via the typed helpers below so rule
// code reads `with.String("key")` instead of casting any.
type StepConfig map[string]any

// String returns the value at key as a string, or "" if missing or
// not a string. Use it for header names, source identifiers, etc.
// — the common YAML scalar.
func (s StepConfig) String(key string) string {
	if v, ok := s[key].(string); ok {
		return v
	}
	return ""
}

// Bool returns the value at key as a bool, or false if missing or
// not a bool. Mirrors String for the boolean YAML scalar.
func (s StepConfig) Bool(key string) bool {
	if v, ok := s[key].(bool); ok {
		return v
	}
	return false
}

// Int returns the value at key as an int. YAML.v3 decodes integer
// scalars as int (not int64) into map[string]any, so this is the
// natural fetch path. Missing or non-int → 0.
func (s StepConfig) Int(key string) int {
	if v, ok := s[key].(int); ok {
		return v
	}
	return 0
}

// Has reports whether key is present in the config map, regardless
// of its value type. Lets rules distinguish "key set to false" from
// "key absent" when the difference matters.
func (s StepConfig) Has(key string) bool {
	_, ok := s[key]
	return ok
}
