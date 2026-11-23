package rulesengine

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// Engine is the compiled, ready-to-run pipeline registry. Built
// once at boot via NewEngine().Register(...).Load(...).Build(),
// then reused for every request. Goroutine-safe after Build — the
// internal maps are not mutated.
//
// Lifecycle:
//
//  1. NewEngine() — empty builder.
//  2. Register(rules...) — add Rule instances. Each rule's Name()
//     becomes the lookup key; duplicates are caught at Build.
//  3. Load(fsys, glob) — schedule YAML files for parsing. Multiple
//     calls accumulate; duplicate pipeline names across files are
//     caught at Build.
//  4. Build() — actually parse the YAML, resolve names against
//     the registry, compile every `when:` expression, and check
//     parallel groups for overlapping Writes(). Every error you
//     can catch statically surfaces here.
//
// After Build, only Run / HTTPHandler are valid calls.
type Engine struct {
	rules     map[string]Rule
	sources   []pipelineSource
	pipelines map[string]*compiledPipeline
	// allSpecs is the raw YAML for every loaded pipeline. Held
	// across Build because sub-pipeline references (`pipeline:
	// <name>`) need the source for arbitrary names — possibly
	// forward references the first compile pass hasn't reached
	// yet. Populated once at the top of Build, then read-only.
	allSpecs map[string]*pipelineSpec
	built    bool
	buildErr error
}

// compileColor is the DFS marker used to detect sub-pipeline
// cycles. White (zero value) = not visited; gray = currently in
// the DFS path (revisiting = cycle); black = fully compiled
// (revisiting is fine, it's a diamond, not a cycle).
type compileColor int

const (
	colorWhite compileColor = iota
	colorGray
	colorBlack
)

// pipelineSource is one queued Load call. Held until Build because
// the parser needs Engine.rules to resolve names — which the caller
// might still be registering when Load runs.
type pipelineSource struct {
	fsys fs.FS
	glob string
}

// NewEngine returns an empty Engine builder. Chain Register, Load
// and Build to make it usable.
func NewEngine() *Engine {
	return &Engine{
		rules:     make(map[string]Rule),
		pipelines: make(map[string]*compiledPipeline),
	}
}

// Register adds rules to the engine's lookup table. Duplicate
// Name() values are tolerated here (last-write-wins) and only
// flagged when there's a legitimate ambiguity at Build — for now
// it just overwrites, since the typical service registers exactly
// one instance per Rule type.
//
// Returns the receiver so calls chain naturally:
//
//	eng := rules.NewEngine().
//	    Register(&FetchOrg{DS: ds}, &LoadPlan{DS: ds}).
//	    Load(fs, "pipelines/*.yaml")
func (e *Engine) Register(rules ...Rule) *Engine {
	for _, r := range rules {
		e.rules[r.Name()] = r
	}
	return e
}

// Load schedules YAML pipeline files at fsys/glob for parsing
// during Build. Call it once per (fsys, glob) tuple; multiple calls
// across different filesystems are merged. Resolution happens at
// Build, not here — Load itself never fails.
func (e *Engine) Load(fsys fs.FS, glob string) *Engine {
	e.sources = append(e.sources, pipelineSource{fsys: fsys, glob: glob})
	return e
}

// Build resolves every staged pipeline against the registered
// rules, compiles `when:` expressions, and runs the parallel
// overlap check. Idempotent: calling Build twice returns the cached
// result (success or error) — so callers can re-trigger Build in
// a dev hot-reload loop without paying the parse cost twice.
//
// Errors at this stage are configuration bugs the operator should
// fix in YAML or in the rules table: missing rule name, ambiguous
// step shape, conflicting writes in a parallel group, malformed
// `when:` expression. The Engine refuses to Run until Build
// succeeds.
func (e *Engine) Build() error {
	if e.built {
		return e.buildErr
	}
	e.built = true

	// Step 1: load every YAML into a single name → spec map. Sub-
	// pipeline references can target any loaded pipeline regardless
	// of which file declared it, so we need the full registry
	// before we start compiling anything.
	e.allSpecs = make(map[string]*pipelineSpec)
	for _, src := range e.sources {
		specs, err := loadPipelinesFn(src.fsys, src.glob)
		if err != nil {
			e.buildErr = err
			return err
		}
		for name, spec := range specs {
			if _, dup := e.allSpecs[name]; dup {
				e.buildErr = fmt.Errorf("rules: duplicate pipeline %q", name)
				return e.buildErr
			}
			e.allSpecs[name] = spec
		}
	}

	// Step 2: compile each pipeline. compilePipelineByName runs
	// DFS-with-coloring so any sub-pipeline references encountered
	// inside compileStep get compiled (and memoised) on demand,
	// and cycles surface as a boot error instead of an infinite
	// recursion.
	state := make(map[string]compileColor, len(e.allSpecs))
	for name := range e.allSpecs {
		if _, err := e.compilePipelineByName(name, state); err != nil {
			e.buildErr = err
			return err
		}
	}
	return nil
}

// compilePipelineByName is the entry point both Build and
// compileStep call to resolve a pipeline name to its compiled
// form. Memoises results in e.pipelines so the second visit (and
// every sub-pipeline reference) reuses the first compile.
//
// state tracks the DFS path: gray means "this pipeline is on the
// current ancestor chain", so re-entering it would close a cycle.
// black means "already compiled successfully" — safe to reuse.
func (e *Engine) compilePipelineByName(name string, state map[string]compileColor) (*compiledPipeline, error) {
	switch state[name] {
	case colorBlack:
		return e.pipelines[name], nil
	case colorGray:
		return nil, fmt.Errorf("rules: pipeline cycle involving %q", name)
	}
	spec, ok := e.allSpecs[name]
	if !ok {
		return nil, fmt.Errorf("rules: unknown pipeline %q", name)
	}
	state[name] = colorGray
	flow, err := e.compileSteps(name, spec.Flow, state)
	if err != nil {
		return nil, err
	}
	cp := &compiledPipeline{name: name, pick: spec.Pick, flow: flow}
	e.pipelines[name] = cp
	state[name] = colorBlack
	return cp, nil
}

// compileSteps compiles a slice of stepSpecs. Used for top-level
// flow, sub-flows (sequence:, parallel:), and the on_success /
// on_failure branches.
func (e *Engine) compileSteps(pipeline string, specs []*stepSpec, state map[string]compileColor) ([]*compiledStep, error) {
	out := make([]*compiledStep, 0, len(specs))
	for _, s := range specs {
		cs, err := e.compileStep(pipeline, s, state)
		if err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, nil
}

// compileStep handles one stepSpec — resolves the rule, compiles
// the `when:`, recurses into branches and groups, runs the parallel
// overlap check.
func (e *Engine) compileStep(pipeline string, s *stepSpec, state map[string]compileColor) (*compiledStep, error) {
	kind, err := kindOf(s)
	if err != nil {
		return nil, fmt.Errorf("rules: pipeline %q: %w", pipeline, err)
	}

	when, err := compileWhen(s.When)
	if err != nil {
		return nil, fmt.Errorf("rules: pipeline %q: %w", pipeline, err)
	}

	cs := &compiledStep{kind: kind, when: when, with: s.With}

	if s.Timeout != "" {
		if kind != stepKindRule {
			return nil, fmt.Errorf("rules: pipeline %q: timeout: applies only to rule steps", pipeline)
		}
		d, err := time.ParseDuration(s.Timeout)
		if err != nil {
			return nil, fmt.Errorf("rules: pipeline %q: timeout %q: %w", pipeline, s.Timeout, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("rules: pipeline %q: timeout %q: must be > 0", pipeline, s.Timeout)
		}
		cs.timeout = d
	}
	if s.Retry != nil {
		if kind != stepKindRule {
			return nil, fmt.Errorf("rules: pipeline %q: retry: applies only to rule steps", pipeline)
		}
		retry, err := compileRetry(pipeline, s.Retry)
		if err != nil {
			return nil, err
		}
		cs.retry = retry
	}

	switch kind {
	case stepKindRule:
		rule, ok := e.rules[s.Name]
		if !ok {
			return nil, fmt.Errorf("rules: pipeline %q: unknown rule %q", pipeline, s.Name)
		}
		cs.rule = rule
	case stepKindParallel:
		group, err := e.compileSteps(pipeline, s.Parallel, state)
		if err != nil {
			return nil, err
		}
		if err := checkParallelWrites(pipeline, group); err != nil {
			return nil, err
		}
		cs.group = group
	case stepKindSequence:
		group, err := e.compileSteps(pipeline, s.Sequence, state)
		if err != nil {
			return nil, err
		}
		cs.group = group
	case stepKindPipeline:
		sub, err := e.compilePipelineByName(s.Pipeline, state)
		if err != nil {
			return nil, fmt.Errorf("rules: pipeline %q: %w", pipeline, err)
		}
		// Share the sub-pipeline's flow by reference. The
		// executor treats stepKindPipeline like a sequence: walk
		// its children. The sub's own `pick:` is intentionally
		// ignored — it only shapes top-level responses.
		cs.group = sub.flow
	}

	if cs.onSuccess, err = e.compileSteps(pipeline, s.OnSuccess, state); err != nil {
		return nil, err
	}
	if cs.onFailure, err = e.compileSteps(pipeline, s.OnFailure, state); err != nil {
		return nil, err
	}
	return cs, nil
}

// compileRetry parses a YAML retrySpec into the executor's typed
// representation. Defaults are applied here (Initial → 100ms,
// Max → 5s, Backoff → exponential) so the runtime never has to
// branch on missing fields. Validation surfaces invalid duration
// strings, count <= 0, and unknown backoff modes as boot errors.
func compileRetry(pipeline string, s *retrySpec) (*compiledRetry, error) {
	if s.Count <= 0 {
		return nil, fmt.Errorf("rules: pipeline %q: retry.count must be > 0", pipeline)
	}
	out := &compiledRetry{count: s.Count}

	switch s.Backoff {
	case "", "exponential":
		out.backoff = backoffExponential
	case "linear":
		out.backoff = backoffLinear
	case "constant":
		out.backoff = backoffConstant
	default:
		return nil, fmt.Errorf("rules: pipeline %q: retry.backoff %q: must be constant|linear|exponential", pipeline, s.Backoff)
	}

	initial := 100 * time.Millisecond
	if s.Initial != "" {
		d, err := time.ParseDuration(s.Initial)
		if err != nil {
			return nil, fmt.Errorf("rules: pipeline %q: retry.initial %q: %w", pipeline, s.Initial, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("rules: pipeline %q: retry.initial %q: must be > 0", pipeline, s.Initial)
		}
		initial = d
	}
	out.initial = initial

	maxDur := 5 * time.Second
	if s.Max != "" {
		d, err := time.ParseDuration(s.Max)
		if err != nil {
			return nil, fmt.Errorf("rules: pipeline %q: retry.max %q: %w", pipeline, s.Max, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("rules: pipeline %q: retry.max %q: must be > 0", pipeline, s.Max)
		}
		maxDur = d
	}
	out.max = maxDur

	return out, nil
}

// checkParallelWrites is the static race-prevention check: walks
// every rule-kind step inside a parallel group and verifies that
// no two declare the same key in Writes(). This is what lets the
// runtime use sync.Map without locks — by the time Run is called,
// we know the only legitimate writers to a key are sequential.
//
// Non-rule kinds inside parallel (nested sequence:, nested
// parallel:) are walked recursively so the check holds across
// arbitrary nesting.
func checkParallelWrites(pipeline string, group []*compiledStep) error {
	seen := make(map[string]string) // key → rule that already claimed it
	for _, s := range group {
		if err := collectWrites(pipeline, s, seen); err != nil {
			return err
		}
	}
	return nil
}

// collectWrites populates seen with this step's writes (and its
// children's, for nested groups). Returns an error the moment it
// finds an overlap so the operator sees the *first* conflict, not
// a noisy list.
//
// Sub-pipelines are walked too — a parallel block that includes
// a `pipeline:` reference must satisfy the no-overlap invariant
// across the sub's transitive writes, otherwise the runtime
// race-free guarantee breaks down at the inlined boundary.
func collectWrites(pipeline string, s *compiledStep, seen map[string]string) error {
	switch s.kind {
	case stepKindRule:
		for _, k := range s.rule.Writes() {
			if owner, dup := seen[k]; dup {
				return fmt.Errorf("rules: pipeline %q: parallel writes overlap on key %q (rules %q and %q)",
					pipeline, k, owner, s.rule.Name())
			}
			seen[k] = s.rule.Name()
		}
	case stepKindParallel, stepKindSequence, stepKindPipeline:
		for _, child := range s.group {
			if err := collectWrites(pipeline, child, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run executes the named pipeline against req and returns the
// Result (Context.Data filtered through pipeline.pick if declared).
//
// Errors:
//   - rules.IsAbort(err) → business logic returned "no" cleanly;
//     callers usually map this to a 4xx with AbortReason(err).
//   - *RuleError       → a rule's Run returned a non-sentinel
//     error; wrap in 500 by default.
//   - everything else  → engine-level failure (unknown pipeline,
//     `when:` evaluation error, etc.). Almost always 500.
//
// req may be nil; rules that read c.Request must nil-check, but
// most don't (the pipeline is usually driven by HTTPHandler).
func (e *Engine) Run(ctx context.Context, req *http.Request, pipelineName string) (Result, error) {
	if !e.built {
		if err := e.Build(); err != nil {
			return nil, err
		}
	}
	if e.buildErr != nil {
		return nil, e.buildErr
	}

	p, ok := e.pipelines[pipelineName]
	if !ok {
		return nil, fmt.Errorf("rules: unknown pipeline %q", pipelineName)
	}

	spanCtx, span := startPipelineSpan(ctx, pipelineName)
	c := NewContext(spanCtx, req)
	c.Logger = pipelineLogger(ctx, pipelineName)

	start := time.Now()
	err := p.run(c)
	dur := time.Since(start)
	finishSpan(span, err)
	recordPipelineRun(spanCtx, pipelineName, err, dur)
	snap := c.snapshot()
	if len(p.pick) > 0 {
		snap = snap.Pick(p.pick...)
	}
	if err != nil && !IsAbort(err) {
		return snap, err
	}
	if IsAbort(err) {
		// Surface the abort reason in the snapshot so HTTPHandler
		// (and inline callers) can render it without re-deriving.
		// "_abort" prefix avoids colliding with rule-written keys.
		snap["_abort"] = AbortReason(err)
		return snap, err
	}
	return snap, nil
}

// pipelineLogger returns the per-run logger pre-bound with the
// pipeline name. Reuses any correlation IDs (request_id, device_id,
// organization_id) the request context carries via core/log.
func pipelineLogger(ctx context.Context, name string) *log.Logger {
	return log.WithContext(ctx).With(log.String("pipeline", name))
}
