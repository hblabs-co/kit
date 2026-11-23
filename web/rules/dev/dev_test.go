package dev

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/fswatch"
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// stubFakeRule is the smallest rule that compiles — used to feed
// EngineFn results in tests. We don't import web/rules/rules
// here to avoid a circular dependency in test scaffolding.
type stubFakeRule struct {
	name   string
	writes []string
}

func (r *stubFakeRule) Name() string                                           { return r.name }
func (r *stubFakeRule) Writes() []string                                       { return r.writes }
func (r *stubFakeRule) Run(*rulesengine.Context, rulesengine.StepConfig) error { return nil }

// engineWithPipeline builds a tiny engine that registers `rule` and
// exposes one pipeline named `pipelineName`. Used so a Server can
// be constructed without writing real YAML to disk for the engine
// step (the watcher's WatchRoot still has to exist as a directory,
// but we don't need a parseable YAML there to test the dev wrapper
// itself — that's what web/rules/engine tests cover).
func engineWithPipeline(t *testing.T, rule rulesengine.Rule, pipelineName, yamlBody string) *rulesengine.Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := rulesengine.NewEngine().
		Register(rule).
		Load(os.DirFS(dir), "*.yaml")
	if err := eng.Build(); err != nil {
		t.Fatalf("build engine: %v", err)
	}
	return eng
}

func TestNewRequiresEngineFn(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{WatchRoot: "/tmp"}); err == nil {
		t.Fatal("expected EngineFn required")
	}
}

func TestNewRequiresWatchRoot(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{EngineFn: func() (*rulesengine.Engine, error) { return nil, nil }}); err == nil {
		t.Fatal("expected WatchRoot required")
	}
}

func TestNewInitialEngineFnFailurePropagates(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return nil, errors.New("boot fail") },
		WatchRoot: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected initial engine build failure to surface")
	}
}

func TestNewWatcherFnFailurePropagates(t *testing.T) {
	orig := newWatcherFn
	defer func() { newWatcherFn = orig }()
	newWatcherFn = func(time.Duration) (*fswatch.Watcher, error) {
		return nil, errors.New("forced watcher fail")
	}
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	_, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected watcher init failure")
	}
}

func TestNewAddTreeFailurePropagates(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	_, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot: "/this/path/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected AddTree to fail on a missing path")
	}
}

func TestNewSuccessAndEngineAccessor(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	want := engineWithPipeline(t, r, "p", yaml)
	s, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return want, nil },
		WatchRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.Engine(); got != want {
		t.Fatal("Engine() must return the initial *Engine")
	}
}

func TestRegisterShutdownLifecycle(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	eng := engineWithPipeline(t, r, "p", yaml)
	dir := t.TempDir()
	s, err := New(Options{
		EngineFn:       func() (*rulesengine.Engine, error) { return eng, nil },
		WatchRoot:      dir,
		DebounceWindow: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestHTTPHandlerDispatchesThroughCurrentEngine(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r", writes: []string{"hello"}}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	eng := engineWithPipeline(t, r, "p", yaml)

	dir := t.TempDir()
	s, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return eng, nil },
		WatchRoot: dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	s.HTTPHandler("p")(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestOnChangeSwapsEngineOnSuccess(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	first := engineWithPipeline(t, r, "p", yaml)
	second := engineWithPipeline(t, r, "p", yaml)

	var calls atomic.Int32
	engineFn := func() (*rulesengine.Engine, error) {
		// First call returns `first` (boot probe), second returns `second`.
		if calls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	s, err := New(Options{EngineFn: engineFn, WatchRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Engine() != first {
		t.Fatal("before onChange: must hold first engine")
	}
	s.onChange("foo.yaml")
	if s.Engine() != second {
		t.Fatal("after onChange: must hold second engine")
	}
}

func TestOnChangeKeepsPreviousEngineOnFailure(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	first := engineWithPipeline(t, r, "p", yaml)

	var calls atomic.Int32
	engineFn := func() (*rulesengine.Engine, error) {
		if calls.Add(1) == 1 {
			return first, nil
		}
		return nil, errors.New("bad yaml")
	}
	s, err := New(Options{EngineFn: engineFn, WatchRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.onChange("bad.yaml")
	if s.Engine() != first {
		t.Fatal("failed rebuild must keep the previous engine")
	}
}

func TestLogSnapshotDiffEmpty(t *testing.T) {
	t.Parallel()
	// Identical (default) snapshots → no diff. Just exercises
	// the no-changes branch; nothing observable to assert beyond
	// "doesn't panic".
	logSnapshotDiff(rulesengine.Snapshot{}, rulesengine.Snapshot{}, "/p")
}

func TestLogSnapshotDiffEveryKind(t *testing.T) {
	t.Parallel()
	// Build two real engines whose snapshots will exercise all
	// three diff kinds: a pipeline removed, one modified, and one
	// added. The Snapshot type is opaque from outside the engine
	// package, so we go through Engine.Snapshot here instead of
	// constructing literals.
	prevEng := engineWithPipelineMulti(t,
		[]rulesengine.Rule{
			&stubFakeRule{name: "old", writes: []string{"k"}},
			&stubFakeRule{name: "shared", writes: []string{"s"}},
		},
		`pipelines:
  to_remove:
    flow: [{name: old}]
  to_modify:
    flow: [{name: shared, with: {v: 1}}]
`)
	nextEng := engineWithPipelineMulti(t,
		[]rulesengine.Rule{
			&stubFakeRule{name: "new", writes: []string{"n"}},
			&stubFakeRule{name: "shared", writes: []string{"s"}},
		},
		`pipelines:
  to_add:
    flow: [{name: new}]
  to_modify:
    flow: [{name: shared, with: {v: 2}}]
`)
	logSnapshotDiff(prevEng.Snapshot(), nextEng.Snapshot(), "/p")
}

func TestJoinOrDash(t *testing.T) {
	t.Parallel()
	if got := joinOrDash(nil); got != "-" {
		t.Fatalf("joinOrDash(nil) = %q, want -", got)
	}
	if got := joinOrDash([]string{}); got != "-" {
		t.Fatalf("joinOrDash(empty) = %q, want -", got)
	}
	if got := joinOrDash([]string{"a", "b"}); got != "a,b" {
		t.Fatalf("joinOrDash = %q, want a,b", got)
	}
}

func TestOnChangeYAMLLogsDiff(t *testing.T) {
	t.Parallel()
	// Two engines that differ by one rule; onChange should swap
	// and the structured diff log code path should run without
	// errors.
	r1 := &stubFakeRule{name: "r1"}
	r2 := &stubFakeRule{name: "r2"}
	yamlOne := "pipelines:\n  p:\n    flow: [{name: r1}]\n"
	yamlTwo := "pipelines:\n  p:\n    flow: [{name: r2}]\n"
	first := engineWithPipeline(t, r1, "p", yamlOne)
	second := engineWithPipelineMulti(t, []rulesengine.Rule{r2}, yamlTwo)

	var phase atomic.Int32
	engineFn := func() (*rulesengine.Engine, error) {
		if phase.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	s, err := New(Options{EngineFn: engineFn, WatchRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.onChange("p.yaml")
	if s.Engine() != second {
		t.Fatal("onChange must swap to the rebuilt engine")
	}
}

// engineWithPipelineMulti is the variant of engineWithPipeline that
// accepts an arbitrary list of rules so test fixtures can change
// the registered set across calls (e.g. r1 → r2).
func engineWithPipelineMulti(t *testing.T, rules []rulesengine.Rule, yamlBody string) *rulesengine.Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := rulesengine.NewEngine().Register(rules...).Load(os.DirFS(dir), "*.yaml")
	if err := eng.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return eng
}

func TestOnChangeGoFileLogsWarnNoRebuild(t *testing.T) {
	t.Parallel()
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	first := engineWithPipeline(t, r, "p", yaml)

	var calls atomic.Int32
	engineFn := func() (*rulesengine.Engine, error) {
		calls.Add(1)
		return first, nil
	}
	s, err := New(Options{EngineFn: engineFn, WatchRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	bootCalls := calls.Load()
	s.onChange("web/rules/rules/detect_locale.go")
	if calls.Load() != bootCalls {
		t.Fatal(".go change must NOT trigger EngineFn (no rebuild)")
	}
	if s.Engine() != first {
		t.Fatal(".go change must keep the existing engine")
	}
}

func TestNewAddsCodeWatchRoots(t *testing.T) {
	// Don't t.Parallel — we swap the package-level codeRootExists.
	orig := codeRootExists
	defer func() { codeRootExists = orig }()

	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	codeDir := t.TempDir() // real existing dir; codeRootExists allows it explicitly below
	codeRootExists = func(path string) bool { return path == codeDir }

	_, err := New(Options{
		EngineFn:       func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot:      t.TempDir(),
		CodeWatchRoots: []string{codeDir},
	})
	if err != nil {
		t.Fatalf("CodeWatchRoots that exists must not error, got %v", err)
	}
}

func TestNewSkipsMissingCodeWatchRoots(t *testing.T) {
	// Don't t.Parallel — package-level seam swap.
	orig := codeRootExists
	defer func() { codeRootExists = orig }()
	codeRootExists = func(string) bool { return false }

	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	_, err := New(Options{
		EngineFn:       func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot:      t.TempDir(),
		CodeWatchRoots: []string{"/does/not/exist"},
	})
	if err != nil {
		t.Fatalf("missing CodeWatchRoot must be skipped, got %v", err)
	}
}

func TestNewCodeWatchAddTreeFailurePropagates(t *testing.T) {
	// Don't t.Parallel — package-level seam swap.
	orig := codeRootExists
	defer func() { codeRootExists = orig }()
	codeRootExists = func(string) bool { return true } // claim it exists

	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	_, err := New(Options{
		EngineFn:       func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot:      t.TempDir(),
		CodeWatchRoots: []string{"/does/not/exist"},
	})
	if err == nil {
		t.Fatal("AddTree failure on a code root must surface — codeRootExists lied about its presence")
	}
}

func TestCodeRootExistsRealStat(t *testing.T) {
	t.Parallel()
	// Exercise the real codeRootExists (not the test stub) on a
	// known-good dir and a known-bad path so the production
	// implementation has coverage.
	if !codeRootExists(t.TempDir()) {
		t.Fatal("real codeRootExists must accept an existing dir")
	}
	if codeRootExists("/this/path/definitely/does/not/exist/123") {
		t.Fatal("real codeRootExists must reject a missing path")
	}
	// Also reject files (not dirs).
	f, err := os.CreateTemp("", "rulesdev-file-not-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	_ = f.Close()
	if codeRootExists(f.Name()) {
		t.Fatal("codeRootExists must reject a regular file (not a dir)")
	}
}

func TestDefaultDebounceApplied(t *testing.T) {
	t.Parallel()
	// We can't observe debounceWindow on Watcher externally, but the
	// branch that picks defaultDebounceWindow is covered when
	// DebounceWindow is zero.
	r := &stubFakeRule{name: "r"}
	yaml := "pipelines:\n  p:\n    flow: [{name: r}]\n"
	_, err := New(Options{
		EngineFn:  func() (*rulesengine.Engine, error) { return engineWithPipeline(t, r, "p", yaml), nil },
		WatchRoot: t.TempDir(),
		// DebounceWindow intentionally zero
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
}
