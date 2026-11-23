package rulesengine

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// snapshotEngine builds a tiny engine from inline YAML for snapshot
// tests. Pulled into a helper because every test here needs a
// pre-Built engine with one or more pipelines.
func snapshotEngine(t *testing.T, yamlBody string, rules ...Rule) *Engine {
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

// snapRule implements Rule for snapshot tests without dragging in
// the catalog (which would create an import cycle).
type snapRule struct {
	name   string
	writes []string
}

func (r *snapRule) Name() string                   { return r.name }
func (r *snapRule) Writes() []string               { return r.writes }
func (r *snapRule) Run(*Context, StepConfig) error { return nil }

func TestSnapshotEmptyEngine(t *testing.T) {
	t.Parallel()
	e := NewEngine()
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot(); len(got.pipelines) != 0 {
		t.Fatalf("empty engine snapshot must have no pipelines, got %v", got.pipelines)
	}
}

func TestSnapshotCapturesFlowAndConfig(t *testing.T) {
	t.Parallel()
	e := snapshotEngine(t, `
pipelines:
  greet:
    flow:
      - name: a
        with: { msg: hello }
      - name: b
`, &snapRule{name: "a"}, &snapRule{name: "b"})

	pipeline, ok := e.Snapshot().pipelines["greet"]
	if !ok {
		t.Fatal("pipeline missing")
	}
	if len(pipeline.steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(pipeline.steps))
	}
	if pipeline.steps[0].rule != "a" || pipeline.steps[1].rule != "b" {
		t.Fatalf("unexpected steps: %+v", pipeline.steps)
	}
	if pipeline.steps[0].configHash == "" {
		t.Fatal("configHash must be non-empty")
	}
	if pipeline.steps[0].path != "flow[0]" {
		t.Fatalf("path[0] = %q", pipeline.steps[0].path)
	}
	if pipeline.steps[1].path != "flow[1]" {
		t.Fatalf("path[1] = %q", pipeline.steps[1].path)
	}
}

func TestSnapshotDescendsIntoParallelAndSequence(t *testing.T) {
	t.Parallel()
	e := snapshotEngine(t, `
pipelines:
  fan:
    flow:
      - parallel:
          - name: a
          - sequence:
              - name: b
              - name: c
`, &snapRule{name: "a", writes: []string{"x"}}, &snapRule{name: "b", writes: []string{"y"}}, &snapRule{name: "c", writes: []string{"z"}})

	steps := e.Snapshot().pipelines["fan"].steps
	got := make([]string, len(steps))
	for i, s := range steps {
		got[i] = s.rule
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flattened order = %v, want %v", got, want)
	}
}

func TestSnapshotDescendsIntoBranches(t *testing.T) {
	t.Parallel()
	e := snapshotEngine(t, `
pipelines:
  branchy:
    flow:
      - name: a
        on_success:
          - name: b
        on_failure:
          - name: c
`, &snapRule{name: "a"}, &snapRule{name: "b"}, &snapRule{name: "c"})

	steps := e.Snapshot().pipelines["branchy"].steps
	rules := make(map[string]bool)
	for _, s := range steps {
		rules[s.rule] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !rules[want] {
			t.Fatalf("step %q missing from snapshot", want)
		}
	}
}

func TestSnapshotConfigHashChangesWithWith(t *testing.T) {
	t.Parallel()
	e1 := snapshotEngine(t, "pipelines:\n  p:\n    flow:\n      - name: r\n        with: { k: 1 }\n",
		&snapRule{name: "r"})
	e2 := snapshotEngine(t, "pipelines:\n  p:\n    flow:\n      - name: r\n        with: { k: 2 }\n",
		&snapRule{name: "r"})
	h1 := e1.Snapshot().pipelines["p"].steps[0].configHash
	h2 := e2.Snapshot().pipelines["p"].steps[0].configHash
	if h1 == h2 {
		t.Fatal("changing `with:` must change configHash")
	}
}

func TestSnapshotConfigHashChangesWithWhen(t *testing.T) {
	t.Parallel()
	e1 := snapshotEngine(t, "pipelines:\n  p:\n    flow:\n      - name: r\n        when: \"1 == 1\"\n",
		&snapRule{name: "r"})
	e2 := snapshotEngine(t, "pipelines:\n  p:\n    flow:\n      - name: r\n        when: \"1 == 2\"\n",
		&snapRule{name: "r"})
	h1 := e1.Snapshot().pipelines["p"].steps[0].configHash
	h2 := e2.Snapshot().pipelines["p"].steps[0].configHash
	if h1 == h2 {
		t.Fatal("changing `when:` must change configHash")
	}
}

func TestDiffAdded(t *testing.T) {
	t.Parallel()
	prev := Snapshot{pipelines: map[string]pipelineDigest{}}
	next := Snapshot{pipelines: map[string]pipelineDigest{"new_one": {}}}
	changes := next.Diff(prev)
	if len(changes) != 1 || changes[0].Kind != "added" || changes[0].Pipeline != "new_one" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestDiffRemoved(t *testing.T) {
	t.Parallel()
	prev := Snapshot{pipelines: map[string]pipelineDigest{"gone": {}}}
	next := Snapshot{pipelines: map[string]pipelineDigest{}}
	changes := next.Diff(prev)
	if len(changes) != 1 || changes[0].Kind != "removed" || changes[0].Pipeline != "gone" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestDiffUnchanged(t *testing.T) {
	t.Parallel()
	e := snapshotEngine(t, "pipelines:\n  p:\n    flow:\n      - name: r\n",
		&snapRule{name: "r"})
	snap := e.Snapshot()
	if got := snap.Diff(snap); len(got) != 0 {
		t.Fatalf("identical snapshots must produce no diff, got %+v", got)
	}
}

func TestDiffModifiedAddedRemoved(t *testing.T) {
	t.Parallel()
	e1 := snapshotEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: a\n      - name: b\n",
		&snapRule{name: "a", writes: []string{"x"}},
		&snapRule{name: "b", writes: []string{"y"}})
	e2 := snapshotEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: a\n      - name: c\n",
		&snapRule{name: "a", writes: []string{"x"}},
		&snapRule{name: "c", writes: []string{"z"}})
	diffs := e2.Snapshot().Diff(e1.Snapshot())
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	d := diffs[0]
	if d.Kind != "modified" {
		t.Fatalf("Kind = %q", d.Kind)
	}
	if !reflect.DeepEqual(d.AddedRules, []string{"c"}) {
		t.Fatalf("Added = %v", d.AddedRules)
	}
	if !reflect.DeepEqual(d.RemovedRules, []string{"b"}) {
		t.Fatalf("Removed = %v", d.RemovedRules)
	}
	if d.ChangedRules != nil {
		t.Fatalf("ChangedRules = %v, want nil", d.ChangedRules)
	}
}

func TestDiffModifiedConfigChanged(t *testing.T) {
	t.Parallel()
	e1 := snapshotEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: r\n        with: { k: 1 }\n",
		&snapRule{name: "r"})
	e2 := snapshotEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: r\n        with: { k: 2 }\n",
		&snapRule{name: "r"})
	diffs := e2.Snapshot().Diff(e1.Snapshot())
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	d := diffs[0]
	if d.Kind != "modified" || !reflect.DeepEqual(d.ChangedRules, []string{"r"}) {
		t.Fatalf("unexpected diff = %+v", d)
	}
	if len(d.AddedRules) != 0 || len(d.RemovedRules) != 0 {
		t.Fatal("config-only change must not populate Added/Removed")
	}
}

func TestDiffStableOrder(t *testing.T) {
	t.Parallel()
	prev := Snapshot{pipelines: map[string]pipelineDigest{"zeta": {}}}
	next := Snapshot{pipelines: map[string]pipelineDigest{"alpha": {}, "mid": {}}}
	changes := next.Diff(prev)
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes (alpha+mid added, zeta removed), got %d", len(changes))
	}
	names := []string{changes[0].Pipeline, changes[1].Pipeline, changes[2].Pipeline}
	if !reflect.DeepEqual(names, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("change order = %v, want alphabetical", names)
	}
}

func TestCanonicaliseNestedShapes(t *testing.T) {
	t.Parallel()
	in := StepConfig{
		"slice": []any{
			StepConfig{"x": 1},
			map[string]any{"y": 2},
			"raw",
		},
		"nested": map[string]any{"inner": 1},
	}
	out := canonicalise(in).(map[string]any)
	if _, ok := out["slice"].([]any); !ok {
		t.Fatalf("slice not preserved: %T", out["slice"])
	}
	if _, ok := out["nested"].(map[string]any); !ok {
		t.Fatalf("nested not preserved: %T", out["nested"])
	}
}

func TestItoaCoversEdgeCases(t *testing.T) {
	t.Parallel()
	cases := map[int]string{0: "0", 1: "1", 12: "12", 999: "999"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHashStepConfigStable(t *testing.T) {
	t.Parallel()
	s1 := &compiledStep{kind: stepKindRule, with: StepConfig{"a": 1, "b": "x"}}
	s2 := &compiledStep{kind: stepKindRule, with: StepConfig{"b": "x", "a": 1}}
	if hashStepConfig(s1) != hashStepConfig(s2) {
		t.Fatal("hash must be stable across key ordering")
	}
}

func TestSameHashSetSizeMismatch(t *testing.T) {
	t.Parallel()
	a := map[string]struct{}{"x": {}, "y": {}}
	b := map[string]struct{}{"x": {}}
	if sameHashSet(a, b) {
		t.Fatal("sets of different sizes must not be equal")
	}
}

func TestSameHashSetSameSizeDifferentMembers(t *testing.T) {
	t.Parallel()
	a := map[string]struct{}{"x": {}, "y": {}}
	b := map[string]struct{}{"x": {}, "z": {}}
	if sameHashSet(a, b) {
		t.Fatal("same-sized sets with different members must not be equal")
	}
}
