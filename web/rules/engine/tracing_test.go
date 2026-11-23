package rulesengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withSpanRecorder swaps the global TracerProvider for a SDK
// provider backed by an in-memory span recorder. The engine looks
// up its tracer via otel.Tracer(tracerName) on every Start call,
// so flipping the global provider is enough — no package-level
// state to mutate.
//
// IMPORTANT: tests that install a span recorder MUST NOT call
// t.Parallel — they mutate process-global tracer state, and
// running concurrently with other engine tests would race on
// otel.SetTracerProvider's internal pointer.
func withSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	})
	return rec
}

func tracedEngine(t *testing.T, yamlBody string, rules ...Rule) *Engine {
	t.Helper()
	e := NewEngine().Register(rules...).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yamlBody)}},
		"p.yaml",
	)
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	return e
}

// findSpan returns the first span whose Name() matches; t.Fatal
// otherwise. Tests rely on this for "span named X exists with
// these attributes" assertions.
func findSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q (got %v)", name, spanNames(rec))
	return nil
}

func spanNames(rec *tracetest.SpanRecorder) []string {
	spans := rec.Ended()
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}

// attr returns the string value of attribute k on span s, or "" if
// missing / non-string. Tests on outcome / pipeline / rule name use
// this — every attribute we set is a string today.
func attr(s sdktrace.ReadOnlySpan, k string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == k {
			return a.Value.AsString()
		}
	}
	return ""
}

// emitRule is the smallest rule the tracing tests need: takes a
// run function and returns whatever it produces.
type emitRule struct {
	name string
	run  func(*Context, StepConfig) error
}

func (r *emitRule) Name() string                       { return r.name }
func (r *emitRule) Writes() []string                   { return nil }
func (r *emitRule) Run(c *Context, w StepConfig) error { return r.run(c, w) }

func TestSpansEmittedOnSuccessfulRun(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&emitRule{name: "r", run: func(*Context, StepConfig) error { return nil }})

	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}

	pipeSpan := findSpan(t, rec, "rules.pipeline")
	if attr(pipeSpan, "rules.pipeline") != "p" {
		t.Fatalf("rules.pipeline attr = %q", attr(pipeSpan, "rules.pipeline"))
	}
	if attr(pipeSpan, "rules.outcome") != "ok" {
		t.Fatalf("pipeline outcome = %q, want ok", attr(pipeSpan, "rules.outcome"))
	}
	ruleSpan := findSpan(t, rec, "rules.rule")
	if attr(ruleSpan, "rules.rule") != "r" {
		t.Fatalf("rule attr = %q", attr(ruleSpan, "rules.rule"))
	}
	if attr(ruleSpan, "rules.outcome") != "ok" {
		t.Fatalf("rule outcome = %q, want ok", attr(ruleSpan, "rules.outcome"))
	}
}

func TestRuleSpanOutcomeSkip(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&emitRule{name: "r", run: func(*Context, StepConfig) error { return Skip }})

	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	if got := attr(findSpan(t, rec, "rules.rule"), "rules.outcome"); got != "skip" {
		t.Fatalf("outcome = %q, want skip", got)
	}
}

func TestRuleSpanOutcomeAbort(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&emitRule{name: "r", run: func(*Context, StepConfig) error { return Abort("denied") }})

	_, _ = e.Run(context.Background(), nil, "p")
	rs := findSpan(t, rec, "rules.rule")
	if got := attr(rs, "rules.outcome"); got != "abort" {
		t.Fatalf("outcome = %q, want abort", got)
	}
	if got := attr(rs, "rules.abort_reason"); got != "denied" {
		t.Fatalf("abort_reason = %q", got)
	}
}

func TestRuleSpanOutcomeError(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&emitRule{name: "r", run: func(*Context, StepConfig) error { return errors.New("boom") }})

	_, _ = e.Run(context.Background(), nil, "p")
	rs := findSpan(t, rec, "rules.rule")
	if got := attr(rs, "rules.outcome"); got != "error" {
		t.Fatalf("outcome = %q, want error", got)
	}
	if rs.Status().Code.String() != "Error" {
		t.Fatalf("status = %q, want Error", rs.Status().Code.String())
	}
}

func TestParallelSpanEmitted(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t, `
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - name: b
`,
		&emitRule{name: "a", run: func(*Context, StepConfig) error { return nil }},
		&emitRule{name: "b", run: func(*Context, StepConfig) error { return nil }})

	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	if _ = findSpan(t, rec, "rules.parallel"); false {
		t.Fatal("parallel span must be emitted")
	}
}

func TestParallelSpanSkippedWhenEmpty(t *testing.T) {
	rec := withSpanRecorder(t)
	// runParallel returns early when len(steps) == 0; no span
	// should be emitted in that case. Build can't produce an empty
	// parallel group from YAML (kindOf rejects it), so we call
	// runParallel directly.
	_ = runParallel(&Context{data: &sync.Map{}, ctx: context.Background()}, nil)
	for _, n := range spanNames(rec) {
		if n == "rules.parallel" {
			t.Fatal("empty parallel must not emit a span")
		}
	}
}

func TestOutcomeOfAllKinds(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"ok":    nil,
		"skip":  Skip,
		"abort": Abort("x"),
		"error": errors.New("e"),
	}
	for want, err := range cases {
		if got := outcomeOf(err); got != want {
			t.Fatalf("outcomeOf(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestIsSkipDoesNotMatchOtherErrors(t *testing.T) {
	t.Parallel()
	if isSkip(errors.New("plain")) {
		t.Fatal("isSkip on plain error must be false")
	}
	if !isSkip(Skip) {
		t.Fatal("isSkip on Skip must be true")
	}
}

// Smoke test the span dump matches expectations — exercises the
// helper that lists spans by name (used by other tests' failure
// messages too).
func TestSpanNamesDumpListsEmitted(t *testing.T) {
	rec := withSpanRecorder(t)
	e := tracedEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&emitRule{name: "r", run: func(*Context, StepConfig) error { return nil }})
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	names := strings.Join(spanNames(rec), ",")
	if !strings.Contains(names, "rules.pipeline") || !strings.Contains(names, "rules.rule") {
		t.Fatalf("expected both spans, got %q", names)
	}
}
