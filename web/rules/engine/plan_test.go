package rulesengine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// planEngine builds a tiny pre-built engine from inline YAML and
// the rules the test names.
func planEngine(t *testing.T, yamlBody string, rules ...Rule) *Engine {
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

func TestPlanUnknownPipeline(t *testing.T) {
	t.Parallel()
	e := NewEngine()
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Plan(context.Background(), nil, "missing", nil); err == nil {
		t.Fatal("unknown pipeline must error")
	}
}

func TestPlanFlatFlow(t *testing.T) {
	t.Parallel()
	e := planEngine(t, "pipelines:\n  p:\n    flow: [{name: a}, {name: b}]\n",
		&snapRule{name: "a", writes: []string{"x"}},
		&snapRule{name: "b"})
	plan, err := e.Plan(context.Background(), nil, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("plan len = %d, want 2", len(plan))
	}
	if plan[0].Kind != "rule" || plan[0].Name != "a" || plan[0].Depth != 0 {
		t.Fatalf("plan[0] = %+v", plan[0])
	}
	if !equalStrings(plan[0].Writes, []string{"x"}) {
		t.Fatalf("plan[0].Writes = %v", plan[0].Writes)
	}
}

func TestPlanGroupsAndBranches(t *testing.T) {
	t.Parallel()
	e := planEngine(t, `
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - name: b
      - name: c
        on_success:
          - name: d
        on_failure:
          - name: e
`,
		&snapRule{name: "a", writes: []string{"x"}},
		&snapRule{name: "b", writes: []string{"y"}},
		&snapRule{name: "c"},
		&snapRule{name: "d"},
		&snapRule{name: "e"})

	plan, err := e.Plan(context.Background(), nil, "p", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Expected order (pre-order DFS):
	//   parallel (depth 0)
	//     a (depth 1)
	//     b (depth 1)
	//   c (depth 0)
	//     on_success (depth 1)
	//       d (depth 2)
	//     on_failure (depth 1)
	//       e (depth 2)
	wantKinds := []string{"parallel", "rule", "rule", "rule", "branch", "rule", "branch", "rule"}
	if len(plan) != len(wantKinds) {
		t.Fatalf("plan len = %d, want %d (%+v)", len(plan), len(wantKinds), plan)
	}
	for i, w := range wantKinds {
		if plan[i].Kind != w {
			t.Errorf("plan[%d].Kind = %q, want %q", i, plan[i].Kind, w)
		}
	}
	if plan[4].Name != "on_success" || plan[6].Name != "on_failure" {
		t.Fatalf("branch markers wrong: %+v", []PlanStep{plan[4], plan[6]})
	}
	if plan[5].Depth != 2 || plan[7].Depth != 2 {
		t.Fatal("branch children must sit at depth 2")
	}
}

func TestPlanWhenResolvedAgainstData(t *testing.T) {
	t.Parallel()
	e := planEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: r\n        when: \"data.flag == true\"\n",
		&snapRule{name: "r"})

	// Falsy data → false
	plan, _ := e.Plan(context.Background(), nil, "p", map[string]any{"flag": false})
	if plan[0].WhenResult == nil || *plan[0].WhenResult {
		t.Fatalf("falsy when must resolve to false, got %+v", plan[0])
	}

	// Truthy data → true
	plan, _ = e.Plan(context.Background(), nil, "p", map[string]any{"flag": true})
	if plan[0].WhenResult == nil || !*plan[0].WhenResult {
		t.Fatalf("truthy when must resolve to true, got %+v", plan[0])
	}
}

func TestPlanWhenErrorCaptured(t *testing.T) {
	t.Parallel()
	// data.org is set to nil so the deref fails at evaluation
	// time — Plan should record WhenError without aborting the
	// whole walk.
	e := planEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: r\n        when: \"data.org.id == 1\"\n",
		&snapRule{name: "r"})
	plan, err := e.Plan(context.Background(), nil, "p", map[string]any{"org": nil})
	if err != nil {
		t.Fatalf("Plan must not error on a runtime-failing when, got %v", err)
	}
	if plan[0].WhenError == "" {
		t.Fatal("WhenError must be populated")
	}
	if plan[0].WhenResult != nil {
		t.Fatal("WhenResult must stay nil on evaluation failure")
	}
}

func TestPlanLazyBuild(t *testing.T) {
	t.Parallel()
	e := NewEngine().Register(&snapRule{name: "r"}).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte("pipelines:\n  p:\n    flow: [{name: r}]\n")}},
		"p.yaml",
	)
	// Don't call Build explicitly — Plan should trigger it.
	plan, err := e.Plan(context.Background(), nil, "p", nil)
	if err != nil {
		t.Fatalf("lazy build via Plan must succeed, got %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan len = %d", len(plan))
	}
}

func TestPlanLazyBuildError(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte("pipelines:\n  p:\n    flow: [{name: missing_rule}]\n")}},
		"p.yaml",
	)
	if _, err := e.Plan(context.Background(), nil, "p", nil); err == nil {
		t.Fatal("Plan must surface build errors")
	}
}

func TestPlanCachedBuildError(t *testing.T) {
	t.Parallel()
	e := NewEngine().Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte("pipelines:\n  p:\n    flow: [{name: missing_rule}]\n")}},
		"p.yaml",
	)
	_ = e.Build()
	if _, err := e.Plan(context.Background(), nil, "p", nil); err == nil {
		t.Fatal("Plan must propagate cached build error")
	}
}

func TestPlanReadsRequestInWhen(t *testing.T) {
	t.Parallel()
	e := planEngine(t,
		"pipelines:\n  p:\n    flow:\n      - name: r\n        when: \"request.headers['X-Plan'] == 'pro'\"\n",
		&snapRule{name: "r"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Plan", "pro")
	plan, _ := e.Plan(context.Background(), req, "p", nil)
	if plan[0].WhenResult == nil || !*plan[0].WhenResult {
		t.Fatalf("header-based when must resolve true, got %+v", plan[0])
	}
}

func TestPlanGroupWhenStored(t *testing.T) {
	t.Parallel()
	// Currently the YAML doesn't allow `when:` on group containers
	// directly (the parser puts when on rule steps), but the
	// PlanStep field exists for future extension. Just verify the
	// "no when" case sets WhenResult to nil and When to "".
	e := planEngine(t, `
pipelines:
  p:
    flow:
      - parallel: [{name: a}]
`, &snapRule{name: "a"})
	plan, _ := e.Plan(context.Background(), nil, "p", nil)
	if plan[0].When != "" || plan[0].WhenResult != nil {
		t.Fatalf("group without when must have empty When + nil WhenResult, got %+v", plan[0])
	}
}

func TestPlanSequenceGroup(t *testing.T) {
	t.Parallel()
	e := planEngine(t, `
pipelines:
  p:
    flow:
      - sequence:
          - name: a
          - name: b
`, &snapRule{name: "a"}, &snapRule{name: "b"})
	plan, _ := e.Plan(context.Background(), nil, "p", nil)
	if plan[0].Kind != "sequence" {
		t.Fatalf("first step Kind = %q, want sequence", plan[0].Kind)
	}
	if plan[1].Depth != 1 || plan[2].Depth != 1 {
		t.Fatal("sequence children must sit at depth 1")
	}
}

func TestWhenSourceNil(t *testing.T) {
	t.Parallel()
	if got := whenSource(nil); got != "" {
		t.Fatalf("whenSource(nil) = %q", got)
	}
}

// equalStrings compares two []string slices in order. Tiny helper
// to keep the test asserts terse.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Smoke test for the rendering use case: PlanStep is intended to
// drive an "indented tree" CLI in the future. Verify the Depth +
// Kind data is sufficient to reconstruct one.
func TestPlanRendersAsTree(t *testing.T) {
	t.Parallel()
	e := planEngine(t, `
pipelines:
  p:
    flow:
      - parallel:
          - name: a
          - name: b
      - name: c
`, &snapRule{name: "a"}, &snapRule{name: "b"}, &snapRule{name: "c"})
	plan, _ := e.Plan(context.Background(), nil, "p", nil)
	var b strings.Builder
	for _, s := range plan {
		b.WriteString(strings.Repeat("  ", s.Depth))
		if s.Kind == "rule" {
			b.WriteString(s.Name)
		} else {
			b.WriteString("[" + s.Kind + "]")
		}
		b.WriteByte('\n')
	}
	got := b.String()
	want := "[parallel]\n  a\n  b\nc\n"
	if got != want {
		t.Fatalf("rendered tree = %q, want %q", got, want)
	}
}
