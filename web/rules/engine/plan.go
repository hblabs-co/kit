package rulesengine

import (
	"context"
	"fmt"
	"net/http"
)

// PlanStep is one node in the static view of a pipeline produced
// by Engine.Plan. The slice form is a DFS pre-order walk: a parent
// (parallel / sequence / a rule with branches) appears before its
// children, with Depth incremented for each level of nesting.
// That layout makes "render as an indented tree" trivial.
type PlanStep struct {
	// Pipeline is the name of the pipeline this step belongs to.
	// Always set, mirrors the argument to Engine.Plan.
	Pipeline string

	// Depth is the indentation level — 0 for top-level entries in
	// flow, +1 for each parallel / sequence / on_success /
	// on_failure level descended into. Used to render trees.
	Depth int

	// Kind is "rule" | "parallel" | "sequence" | "branch" where
	// "branch" wraps on_success / on_failure containers (their
	// Name carries which one).
	Kind string

	// Name is the rule name for Kind == "rule", "" for the group
	// containers, and "on_success" / "on_failure" for branches.
	Name string

	// When is the raw expression source from YAML, "" if none.
	When string

	// WhenResult records the static evaluation of When at plan
	// time: nil if there's no When, *true if it evaluated truthy
	// against the supplied Data, *false otherwise. A When that
	// errored stays nil with WhenError populated.
	WhenResult *bool

	// WhenError holds the evaluation error if When could not be
	// evaluated against the supplied Data (typical: a referenced
	// field on `data` is missing). "" when the expression evaluated
	// cleanly or the step has no When.
	WhenError string

	// With is the YAML `with:` config attached to a rule step. Nil
	// for group containers.
	With StepConfig

	// Writes lists the keys a rule step declared in Writes(). Nil
	// for group containers.
	Writes []string
}

// Plan returns a static view of `pipelineName` — what steps would
// execute (and which `when:` guards would pass) given the data the
// caller seeds. It does NOT invoke any Rule.Run, so the result is
// safe to call against arbitrary inputs without side effects.
//
// The static view is most useful for:
//
//   - Debugging branchy pipelines: "if I feed this Data, which
//     steps fire and which `when:` guards block?"
//   - PR review: rendering the resolved flow next to the diff so
//     reviewers can read it without simulating the YAML.
//   - Tooling (future CLI in service-tools).
//
// `data` is optional — pass nil for an "empty Data" plan, useful
// when you only want to see the static structure and don't need
// the `when:` results to reflect a particular request state.
// Passing a populated map evaluates `when:` against it so the
// returned WhenResult fields are meaningful for that input.
//
// `req` is also optional. Rules that read from `request.headers`
// in their `when:` need a non-nil req for the evaluation to match
// what would happen at runtime.
//
// Errors:
//   - "unknown pipeline" if the name isn't registered (same
//     wording Engine.Run uses).
//   - engine not built — same lazy-build behaviour as Run.
func (e *Engine) Plan(ctx context.Context, req *http.Request, pipelineName string, data map[string]any) ([]PlanStep, error) {
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

	c := NewContext(ctx, req)
	for k, v := range data {
		c.Set(k, v)
	}

	var out []PlanStep
	planSteps(c, p.flow, pipelineName, 0, &out)
	return out, nil
}

// planSteps walks a slice of compiledStep, appending PlanStep
// entries for every node in DFS pre-order. The Depth increments
// inside groups and branches so callers can indent without doing
// the math themselves.
func planSteps(c *Context, steps []*compiledStep, pipeline string, depth int, out *[]PlanStep) {
	for _, s := range steps {
		switch s.kind {
		case stepKindRule:
			*out = append(*out, makeRulePlanStep(c, s, pipeline, depth))
		case stepKindParallel:
			*out = append(*out, PlanStep{
				Pipeline: pipeline,
				Depth:    depth,
				Kind:     "parallel",
				When:     whenSource(s.when),
			})
			evalAndStorePlanWhen(c, s, &(*out)[len(*out)-1])
			planSteps(c, s.group, pipeline, depth+1, out)
		case stepKindSequence:
			*out = append(*out, PlanStep{
				Pipeline: pipeline,
				Depth:    depth,
				Kind:     "sequence",
				When:     whenSource(s.when),
			})
			evalAndStorePlanWhen(c, s, &(*out)[len(*out)-1])
			planSteps(c, s.group, pipeline, depth+1, out)
		case stepKindPipeline:
			*out = append(*out, PlanStep{
				Pipeline: pipeline,
				Depth:    depth,
				Kind:     "pipeline",
				When:     whenSource(s.when),
			})
			evalAndStorePlanWhen(c, s, &(*out)[len(*out)-1])
			planSteps(c, s.group, pipeline, depth+1, out)
		}
		appendBranch(c, s.onSuccess, pipeline, depth+1, "on_success", out)
		appendBranch(c, s.onFailure, pipeline, depth+1, "on_failure", out)
	}
}

// makeRulePlanStep builds the PlanStep for a single rule node,
// evaluating its `when:` (if any) against the planning Context so
// the caller can see whether the guard would let the rule execute.
func makeRulePlanStep(c *Context, s *compiledStep, pipeline string, depth int) PlanStep {
	ps := PlanStep{
		Pipeline: pipeline,
		Depth:    depth,
		Kind:     "rule",
		Name:     s.rule.Name(),
		When:     whenSource(s.when),
		With:     s.with,
		Writes:   s.rule.Writes(),
	}
	evalAndStorePlanWhen(c, s, &ps)
	return ps
}

// appendBranch is the helper for on_success / on_failure: skips
// empty branches, otherwise emits a "branch" marker followed by
// the recursive walk of the child steps.
func appendBranch(c *Context, steps []*compiledStep, pipeline string, depth int, name string, out *[]PlanStep) {
	if len(steps) == 0 {
		return
	}
	*out = append(*out, PlanStep{
		Pipeline: pipeline,
		Depth:    depth,
		Kind:     "branch",
		Name:     name,
	})
	planSteps(c, steps, pipeline, depth+1, out)
}

// evalAndStorePlanWhen evaluates the step's `when:` against the
// planning Context (if there is one) and stashes the result on the
// PlanStep. An evaluation error doesn't fail the whole Plan call —
// we record it in WhenError so the operator sees what went wrong
// without losing the rest of the plan.
func evalAndStorePlanWhen(c *Context, s *compiledStep, ps *PlanStep) {
	if s.when == nil {
		return
	}
	ok, err := s.when.evaluate(c)
	if err != nil {
		ps.WhenError = err.Error()
		return
	}
	ps.WhenResult = &ok
}

// whenSource pulls the raw source out of a compiledWhen, returning
// "" for a nil pointer. Inlined here so callers don't have to
// nil-check.
func whenSource(w *compiledWhen) string {
	if w == nil {
		return ""
	}
	return w.source
}
