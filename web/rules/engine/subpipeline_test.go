package rulesengine

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

// subEngine builds an engine from inline YAML + a rules list,
// used across sub-pipeline tests where each fixture has its own
// catalog of rules and pipelines.
func subEngine(t *testing.T, yamlBody string, rules ...Rule) *Engine {
	t.Helper()
	e := NewEngine().Register(rules...).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yamlBody)}},
		"p.yaml",
	)
	if err := e.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return e
}

func TestSubPipelineInlinesFlow(t *testing.T) {
	t.Parallel()
	// auth_chain runs r1 then r2; access_decision invokes
	// auth_chain as a sub then r3. End-to-end the three rules
	// should fire in order.
	var order []string
	mk := func(name string) Rule {
		return &fnRule{name: name, fn: func(c *Context, _ StepConfig) error {
			order = append(order, name)
			return nil
		}}
	}
	e := subEngine(t, `
pipelines:
  auth_chain:
    flow:
      - name: r1
      - name: r2
  access_decision:
    flow:
      - pipeline: auth_chain
      - name: r3
`, mk("r1"), mk("r2"), mk("r3"))
	if _, err := e.Run(context.Background(), nil, "access_decision"); err != nil {
		t.Fatal(err)
	}
	want := []string{"r1", "r2", "r3"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
}

func TestSubPipelineCycleRejectedAtBuild(t *testing.T) {
	t.Parallel()
	// a → b → a is a cycle the DFS must catch at Build, not at
	// the first request.
	e := NewEngine().Register(&fnRule{name: "stub"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  a:
    flow:
      - pipeline: b
  b:
    flow:
      - pipeline: a
`)}}, "p.yaml")
	err := e.Build()
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error must mention cycle, got %v", err)
	}
}

func TestSubPipelineSelfReferenceRejected(t *testing.T) {
	t.Parallel()
	// Direct self-reference is the smallest possible cycle.
	e := NewEngine().Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  loop:
    flow:
      - pipeline: loop
`)}}, "p.yaml")
	if err := e.Build(); err == nil {
		t.Fatal("self-reference must surface as a cycle error")
	}
}

func TestSubPipelineDiamondAllowed(t *testing.T) {
	t.Parallel()
	// A → B and A → C, both depend on D. Not a cycle —
	// each branch reaches D independently. Should compile.
	calls := map[string]int{}
	mk := func(name string) Rule {
		return &fnRule{name: name, fn: func(*Context, StepConfig) error {
			calls[name]++
			return nil
		}}
	}
	e := subEngine(t, `
pipelines:
  d:
    flow:
      - name: leaf
  b:
    flow:
      - pipeline: d
  c:
    flow:
      - pipeline: d
  a:
    flow:
      - pipeline: b
      - pipeline: c
`, mk("leaf"))
	if _, err := e.Run(context.Background(), nil, "a"); err != nil {
		t.Fatalf("diamond must execute cleanly, got %v", err)
	}
	if calls["leaf"] != 2 {
		t.Fatalf("leaf should run twice via b and c, got %d", calls["leaf"])
	}
}

func TestSubPipelineUnknownReference(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  caller:
    flow:
      - pipeline: nonexistent
`)}}, "p.yaml")
	err := e.Build()
	if err == nil || !strings.Contains(err.Error(), "unknown pipeline") {
		t.Fatalf("missing reference must surface; got %v", err)
	}
}

func TestSubPipelinePickIgnoredWhenInlined(t *testing.T) {
	t.Parallel()
	// inner has pick: [keep], but when invoked as a sub it
	// contributes ALL keys to the parent's Context.Data. The
	// parent's own pick (if any) is what shapes the response.
	rule := &fnRule{name: "fill", fn: func(c *Context, _ StepConfig) error {
		c.Set("keep", 1)
		c.Set("drop", 2)
		return nil
	}}
	// Add Writes for the verification check
	wrap := &writesRule{Rule: rule, writes: []string{"keep", "drop"}}

	e := subEngine(t, `
pipelines:
  inner:
    pick: [keep]
    flow: [{name: fill}]
  outer:
    flow:
      - pipeline: inner
`, wrap)
	res, err := e.Run(context.Background(), nil, "outer")
	if err != nil {
		t.Fatal(err)
	}
	if res["keep"] != 1 || res["drop"] != 2 {
		t.Fatalf("outer must see both keys; inner's pick is ignored when sub-inlined. Got %v", res)
	}
}

func TestSubPipelineParallelWritesOverlapDetected(t *testing.T) {
	t.Parallel()
	// a and the sub-pipeline both write the same key inside a
	// parallel block. The Build-time check must follow the
	// sub-pipeline's transitive writes and reject the overlap.
	a := &writesRule{Rule: &fnRule{name: "a"}, writes: []string{"shared"}}
	subInner := &writesRule{Rule: &fnRule{name: "sub_inner"}, writes: []string{"shared"}}
	e := NewEngine().Register(a, subInner).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  side:
    flow:
      - name: sub_inner
  main:
    flow:
      - parallel:
          - name: a
          - pipeline: side
`)}}, "p.yaml")
	err := e.Build()
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("expected overlap on 'shared'; got %v", err)
	}
}

func TestSubPipelineRunStepDispatch(t *testing.T) {
	t.Parallel()
	// Cover runStep's stepKindPipeline branch directly: a step
	// whose kind is Pipeline should run its group like a
	// sequence.
	r := &fnRule{name: "inner"}
	s := &compiledStep{
		kind:  stepKindPipeline,
		group: []*compiledStep{{kind: stepKindRule, rule: r}},
	}
	if err := runStep(newCtx(), s); err != nil {
		t.Fatalf("pipeline-kind dispatch failed: %v", err)
	}
}

func TestSubPipelineKindOfRejectsMixedShape(t *testing.T) {
	t.Parallel()
	// pipeline + name together is ambiguous and the loader
	// must reject it.
	_, err := kindOf(&stepSpec{Name: "r", Pipeline: "p"})
	if err == nil {
		t.Fatal("mixed name+pipeline must be rejected")
	}
}

func TestSubPipelineKindOfRecognisesPipeline(t *testing.T) {
	t.Parallel()
	got, err := kindOf(&stepSpec{Pipeline: "p"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != stepKindPipeline {
		t.Fatalf("got %d, want stepKindPipeline", got)
	}
}

func TestSubPipelineSnapshotIncludesSubRules(t *testing.T) {
	t.Parallel()
	// The dev diff log relies on Snapshot walking sub-pipelines.
	// A pipeline whose flow inlines a sub must surface the sub's
	// rules in the digest.
	e := subEngine(t, `
pipelines:
  helper:
    flow:
      - name: leaf
  caller:
    flow:
      - pipeline: helper
`, &fnRule{name: "leaf"})
	got := e.Snapshot().pipelines["caller"]
	if len(got.steps) != 1 || got.steps[0].rule != "leaf" {
		t.Fatalf("caller digest must surface leaf via the sub-pipeline; got %+v", got)
	}
	if !strings.Contains(got.steps[0].path, "pipeline") {
		t.Fatalf("path should record the sub-pipeline boundary, got %q", got.steps[0].path)
	}
}

func TestSubPipelinePlanIncludesSubFlow(t *testing.T) {
	t.Parallel()
	e := subEngine(t, `
pipelines:
  helper:
    flow:
      - name: leaf
  caller:
    flow:
      - pipeline: helper
`, &fnRule{name: "leaf"})
	plan, err := e.Plan(context.Background(), nil, "caller", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: [pipeline] container at depth 0, then [rule leaf] at depth 1.
	if len(plan) != 2 {
		t.Fatalf("plan len = %d (%v)", len(plan), plan)
	}
	if plan[0].Kind != "pipeline" || plan[0].Depth != 0 {
		t.Fatalf("plan[0] = %+v", plan[0])
	}
	if plan[1].Kind != "rule" || plan[1].Name != "leaf" || plan[1].Depth != 1 {
		t.Fatalf("plan[1] = %+v", plan[1])
	}
}

// writesRule wraps an existing Rule to override its Writes()
// declaration without rewriting the underlying logic. Used by
// sub-pipeline tests where the rule's contract needs to be
// specific but the fixture rule (fnRule) doesn't carry Writes.
type writesRule struct {
	Rule
	writes []string
}

func (w *writesRule) Writes() []string { return w.writes }
