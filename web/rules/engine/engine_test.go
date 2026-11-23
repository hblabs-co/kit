package rulesengine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func mustBuild(t *testing.T, e *Engine) *Engine {
	t.Helper()
	if err := e.Build(); err != nil {
		t.Fatalf("Build err = %v", err)
	}
	return e
}

func loadFS(yamlBody string) fstest.MapFS {
	return fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yamlBody)}}
}

func TestEngineRegisterAndRunSimplePipeline(t *testing.T) {
	t.Parallel()
	called := false
	r := &fakeRule{name: "set-greeting", writes: []string{"greeting"}, run: func(c *Context, _ StepConfig) error {
		called = true
		c.Set("greeting", "hola")
		return nil
	}}
	e := NewEngine().
		Register(r).
		Load(loadFS(`
pipelines:
  greet:
    pick: [greeting]
    flow:
      - name: set-greeting
`), "p.yaml")
	mustBuild(t, e)
	res, err := e.Run(context.Background(), nil, "greet")
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if !called {
		t.Fatal("rule was not invoked")
	}
	if res["greeting"] != "hola" {
		t.Fatalf("Result = %v", res)
	}
}

func TestEngineUnknownPipeline(t *testing.T) {
	t.Parallel()
	e := NewEngine()
	mustBuild(t, e)
	if _, err := e.Run(context.Background(), nil, "missing"); err == nil {
		t.Fatal("unknown pipeline must error")
	}
}

func TestEngineBuildIdempotent(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow: [{name: r}]
`), "p.yaml")
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	if err := e.Build(); err != nil {
		t.Fatalf("second Build must be a no-op, got %v", err)
	}
}

func TestEngineBuildCachesError(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(loadFS(`
pipelines:
  p:
    flow: [{name: missing-rule}]
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("expected build to fail")
	}
	// Second Build must return the same error without re-parsing.
	if err := e.Build(); err == nil {
		t.Fatal("cached build error must persist")
	}
	if _, err := e.Run(context.Background(), nil, "p"); err == nil {
		t.Fatal("Run after build error must propagate")
	}
}

func TestEngineRunBuildsLazilyOnce(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow: [{name: r}]
`), "p.yaml")
	// First Run triggers Build automatically.
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatalf("Run err = %v", err)
	}
}

func TestEngineRunBuildErrorOnLazyBuild(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(loadFS(`
pipelines:
  p:
    flow: [{name: missing}]
`), "p.yaml")
	if _, err := e.Run(context.Background(), nil, "p"); err == nil {
		t.Fatal("lazy build error must surface from Run")
	}
}

func TestEngineUnknownRuleErrors(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(loadFS(`
pipelines:
  p:
    flow: [{name: not-registered}]
`), "p.yaml")
	if err := e.Build(); err == nil || !strings.Contains(err.Error(), "unknown rule") {
		t.Fatalf("expected unknown-rule error, got %v", err)
	}
}

func TestEngineAmbiguousStepShape(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(loadFS(`
pipelines:
  p:
    flow:
      - name: x
        parallel: [{name: y}]
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("ambiguous step shape must fail Build")
	}
}

func TestEngineParallelOverlapDetected(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a", writes: []string{"shared"}}
	b := &fakeRule{name: "b", writes: []string{"shared"}}
	e := NewEngine().Register(a, b).Load(loadFS(`
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - name: b
`), "p.yaml")
	err := e.Build()
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("overlap must be flagged, got %v", err)
	}
}

func TestEngineNestedParallelOverlapDetected(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a", writes: []string{"x"}}
	b := &fakeRule{name: "b", writes: []string{"x"}}
	e := NewEngine().Register(a, b).Load(loadFS(`
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - sequence:
              - name: b
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("nested overlap must be flagged")
	}
}

func TestEngineParallelNoOverlapOk(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a", writes: []string{"alpha"}}
	b := &fakeRule{name: "b", writes: []string{"beta"}}
	e := NewEngine().Register(a, b).Load(loadFS(`
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - name: b
`), "p.yaml")
	if err := e.Build(); err != nil {
		t.Fatalf("disjoint writes must build cleanly, got %v", err)
	}
}

func TestEngineInvalidWhenAtBuild(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow:
      - name: r
        when: "data."
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("invalid when must fail Build")
	}
}

func TestEngineInvalidWhenInsideGroup(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow:
      - parallel:
          - name: r
            when: "data."
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("invalid when inside parallel must fail Build")
	}
}

func TestEngineInvalidWhenInOnSuccess(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow:
      - name: r
        on_success:
          - name: r
            when: "data."
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("invalid when in on_success must fail Build")
	}
}

func TestEngineInvalidWhenInOnFailure(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow:
      - name: r
        on_failure:
          - name: r
            when: "data."
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("invalid when in on_failure must fail Build")
	}
}

func TestEngineDuplicatePipelineAcrossFiles(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "r"}
	e := NewEngine().Register(r).
		Load(loadFS(`pipelines:
  p:
    flow: [{name: r}]
`), "p.yaml").
		Load(fstest.MapFS{"q.yaml": &fstest.MapFile{Data: []byte(`pipelines:
  p:
    flow: [{name: r}]
`)}}, "q.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("duplicate pipeline across sources must fail")
	}
}

func TestEngineRunPropagatesAbort(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "deny", run: func(c *Context, _ StepConfig) error { return Abort("nope") }}
	e := NewEngine().Register(r).Load(loadFS(`
pipelines:
  p:
    flow: [{name: deny}]
`), "p.yaml")
	mustBuild(t, e)
	res, err := e.Run(context.Background(), nil, "p")
	if !IsAbort(err) {
		t.Fatalf("expected abort, got %v", err)
	}
	if res["_abort"] != "nope" {
		t.Fatalf("_abort = %v, want nope", res["_abort"])
	}
}

func TestEngineRunPropagatesRuleError(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "fail", run: func(c *Context, _ StepConfig) error { return errors.New("boom") }}
	e := NewEngine().Register(r).Load(loadFS(`
pipelines:
  p:
    flow: [{name: fail}]
`), "p.yaml")
	mustBuild(t, e)
	if _, err := e.Run(context.Background(), nil, "p"); err == nil {
		t.Fatal("rule error must propagate")
	}
}

func TestEngineWithRequest(t *testing.T) {
	t.Parallel()
	got := ""
	r := &fakeRule{name: "read-header", writes: []string{"hdr"}, run: func(c *Context, _ StepConfig) error {
		got = c.Request.Header.Get("X-Test")
		return nil
	}}
	e := NewEngine().Register(r).Load(loadFS(`
pipelines:
  p:
    flow: [{name: read-header}]
`), "p.yaml")
	mustBuild(t, e)
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("X-Test", "marker")
	if _, err := e.Run(context.Background(), req, "p"); err != nil {
		t.Fatal(err)
	}
	if got != "marker" {
		t.Fatalf("rule did not see Request, got %q", got)
	}
}

func TestEngineLoadFailurePropagates(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(errOpenFS{name: "missing.yaml", err: errors.New("forced")}, "missing.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("Load failure must surface from Build")
	}
}

func TestEngineSequenceCompilesAndRuns(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a"}
	b := &fakeRule{name: "b"}
	e := NewEngine().Register(a, b).Load(loadFS(`
pipelines:
  p:
    flow:
      - sequence:
          - name: a
          - name: b
`), "p.yaml")
	mustBuild(t, e)
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	if a.called.Load() != 1 || b.called.Load() != 1 {
		t.Fatal("both rules in sequence must run")
	}
}

func TestEnginePickFiltersResult(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "fill", writes: []string{"keep", "drop"}, run: func(c *Context, _ StepConfig) error {
		c.Set("keep", 1)
		c.Set("drop", 2)
		return nil
	}}
	e := NewEngine().Register(r).Load(loadFS(`
pipelines:
  p:
    pick: [keep]
    flow: [{name: fill}]
`), "p.yaml")
	mustBuild(t, e)
	res, err := e.Run(context.Background(), nil, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := res["drop"]; has {
		t.Fatalf("Pick must drop 'drop', got %v", res)
	}
	if res["keep"] != 1 {
		t.Fatalf("Pick must keep 'keep', got %v", res)
	}
}

func TestEngineSequenceCompilationError(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&fakeRule{name: "r"}).Load(loadFS(`
pipelines:
  p:
    flow:
      - sequence:
          - name: r
            when: "data."
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("invalid when inside sequence must fail Build")
	}
}

func TestEngineParallelCompilationError(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(loadFS(`
pipelines:
  p:
    flow:
      - parallel:
          - name: missing
`), "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("unknown rule inside parallel must fail Build")
	}
}
