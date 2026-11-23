package rulesengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hblabs-co/kit/core/log"
)

// fakeRule is the test rule used across executor + engine tests.
// It records its own invocation, optionally writes a key, and can
// be forced to return any error.
type fakeRule struct {
	name   string
	writes []string
	run    func(c *Context, with StepConfig) error
	called atomic.Int32
}

func (r *fakeRule) Name() string     { return r.name }
func (r *fakeRule) Writes() []string { return r.writes }
func (r *fakeRule) Run(c *Context, with StepConfig) error {
	r.called.Add(1)
	if r.run != nil {
		return r.run(c, with)
	}
	return nil
}

func newCtx() *Context {
	return &Context{data: &sync.Map{}, ctx: context.Background(), Logger: log.Quiet()}
}

func TestRunSequenceCallsEveryStep(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a"}
	b := &fakeRule{name: "b"}
	steps := []*compiledStep{
		{kind: stepKindRule, rule: a},
		{kind: stepKindRule, rule: b},
	}
	if err := runSequence(newCtx(), steps); err != nil {
		t.Fatalf("err = %v", err)
	}
	if a.called.Load() != 1 || b.called.Load() != 1 {
		t.Fatalf("each rule must run exactly once (a=%d b=%d)", a.called.Load(), b.called.Load())
	}
}

func TestRunSequenceStopsOnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	a := &fakeRule{name: "a", run: func(c *Context, _ StepConfig) error { return boom }}
	b := &fakeRule{name: "b"}
	steps := []*compiledStep{
		{kind: stepKindRule, rule: a},
		{kind: stepKindRule, rule: b},
	}
	err := runSequence(newCtx(), steps)
	if err == nil {
		t.Fatal("error must propagate")
	}
	var re *RuleError
	if !errors.As(err, &re) || re.Rule != "a" {
		t.Fatalf("expected RuleError for 'a', got %v", err)
	}
	if b.called.Load() != 0 {
		t.Fatal("b must not run after a fails")
	}
}

func TestRunStepWhenFalseSkips(t *testing.T) {
	t.Parallel()
	cw, _ := compileWhen("data.gate == true")
	r := &fakeRule{name: "x"}
	c := newCtx()
	c.Set("gate", false)
	err := runStep(c, &compiledStep{kind: stepKindRule, rule: r, when: cw})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.called.Load() != 0 {
		t.Fatal("when=false must skip Run")
	}
}

func TestRunStepWhenTrueRuns(t *testing.T) {
	t.Parallel()
	cw, _ := compileWhen("data.gate == true")
	r := &fakeRule{name: "x"}
	c := newCtx()
	c.Set("gate", true)
	if err := runStep(c, &compiledStep{kind: stepKindRule, rule: r, when: cw}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.called.Load() != 1 {
		t.Fatal("when=true must run the rule")
	}
}

func TestRunStepWhenEvalError(t *testing.T) {
	t.Parallel()
	cw, _ := compileWhen("data.org.id == 1")
	c := newCtx()
	c.Set("org", nil) // nil deref at runtime
	if err := runStep(c, &compiledStep{kind: stepKindRule, rule: &fakeRule{name: "x"}, when: cw}); err == nil {
		t.Fatal("when error must propagate")
	}
}

func TestRunStepUnknownKind(t *testing.T) {
	t.Parallel()
	err := runStep(newCtx(), &compiledStep{kind: stepKindUnknown})
	if err == nil {
		t.Fatal("unknown kind must error")
	}
}

func TestRunRulePassesWithConfig(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", run: func(c *Context, with StepConfig) error {
		if with.String("k") != "v" {
			t.Fatalf("with['k'] = %q, want v", with.String("k"))
		}
		return nil
	}}
	step := &compiledStep{kind: stepKindRule, rule: r, with: StepConfig{"k": "v"}}
	if err := runRule(newCtx(), step); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRuleSkipPassesThrough(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", run: func(c *Context, _ StepConfig) error { return Skip }}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	if !errors.Is(err, Skip) {
		t.Fatalf("Skip must round-trip, got %v", err)
	}
}

func TestRunRuleAbortPassesThrough(t *testing.T) {
	t.Parallel()
	abort := Abort("denied")
	r := &fakeRule{name: "x", run: func(c *Context, _ StepConfig) error { return abort }}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	if !IsAbort(err) {
		t.Fatalf("Abort must round-trip, got %v", err)
	}
}

func TestRunRuleAbortWithInternalLogs(t *testing.T) {
	t.Parallel()
	buf, lg := log.Buffered()
	r := &fakeRule{name: "deny", run: func(c *Context, _ StepConfig) error {
		return AbortWithInternal("forbidden", "user=42 over-quota")
	}}
	c := newCtx()
	c.Logger = lg
	err := runRule(c, &compiledStep{kind: stepKindRule, rule: r})
	if !IsAbort(err) {
		t.Fatalf("must propagate Abort, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "rule aborted") || !strings.Contains(out, "over-quota") {
		t.Fatalf("internal context must reach the log, got %q", out)
	}
}

func TestRunRuleAbortWithInternalNoLoggerOk(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "deny", run: func(c *Context, _ StepConfig) error {
		return AbortWithInternal("forbidden", "context")
	}}
	c := &Context{data: &sync.Map{}, ctx: context.Background()} // Logger nil
	if err := runRule(c, &compiledStep{kind: stepKindRule, rule: r}); !IsAbort(err) {
		t.Fatalf("must propagate Abort even without logger, got %v", err)
	}
}

func TestRunRuleNilLoggerOk(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x"}
	c := &Context{data: &sync.Map{}, ctx: context.Background()} // Logger nil
	if err := runRule(c, &compiledStep{kind: stepKindRule, rule: r}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRuleNonSentinelErrorWraps(t *testing.T) {
	t.Parallel()
	inner := errors.New("db down")
	r := &fakeRule{name: "x", run: func(c *Context, _ StepConfig) error { return inner }}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	var re *RuleError
	if !errors.As(err, &re) {
		t.Fatalf("error must wrap as RuleError, got %v", err)
	}
	if re.Rule != "x" || !errors.Is(re.Err, inner) {
		t.Fatalf("unexpected wrap: %+v", re)
	}
}

func TestHandleBranchesOnSuccessRuns(t *testing.T) {
	t.Parallel()
	hook := &fakeRule{name: "ok"}
	main := &compiledStep{
		kind: stepKindRule,
		rule: &fakeRule{name: "main"},
		onSuccess: []*compiledStep{
			{kind: stepKindRule, rule: hook},
		},
	}
	if err := runStep(newCtx(), main); err != nil {
		t.Fatalf("err = %v", err)
	}
	if hook.called.Load() != 1 {
		t.Fatal("on_success must run after a successful step")
	}
}

func TestHandleBranchesOnFailureRecovers(t *testing.T) {
	t.Parallel()
	main := &compiledStep{
		kind: stepKindRule,
		rule: &fakeRule{name: "main", run: func(c *Context, _ StepConfig) error { return errors.New("fail") }},
		onFailure: []*compiledStep{
			{kind: stepKindRule, rule: &fakeRule{name: "recover"}},
		},
	}
	if err := runStep(newCtx(), main); err != nil {
		t.Fatalf("on_failure must absorb the error, got %v", err)
	}
}

func TestHandleBranchesOnFailureCanFail(t *testing.T) {
	t.Parallel()
	main := &compiledStep{
		kind: stepKindRule,
		rule: &fakeRule{name: "main", run: func(c *Context, _ StepConfig) error { return errors.New("fail") }},
		onFailure: []*compiledStep{
			{kind: stepKindRule, rule: &fakeRule{name: "recover", run: func(c *Context, _ StepConfig) error { return Abort("denied") }}},
		},
	}
	err := runStep(newCtx(), main)
	if !IsAbort(err) {
		t.Fatalf("on_failure's abort must win, got %v", err)
	}
}

func TestHandleBranchesOnSuccessErrorPropagates(t *testing.T) {
	t.Parallel()
	main := &compiledStep{
		kind: stepKindRule,
		rule: &fakeRule{name: "main"},
		onSuccess: []*compiledStep{
			{kind: stepKindRule, rule: &fakeRule{name: "after", run: func(c *Context, _ StepConfig) error { return errors.New("boom") }}},
		},
	}
	if err := runStep(newCtx(), main); err == nil {
		t.Fatal("on_success error must propagate")
	}
}

func TestHandleBranchesAbortShortCircuits(t *testing.T) {
	t.Parallel()
	hook := &fakeRule{name: "hook"}
	main := &compiledStep{
		kind:      stepKindRule,
		rule:      &fakeRule{name: "main", run: func(c *Context, _ StepConfig) error { return Abort("nope") }},
		onSuccess: []*compiledStep{{kind: stepKindRule, rule: hook}},
		onFailure: []*compiledStep{{kind: stepKindRule, rule: hook}},
	}
	err := runStep(newCtx(), main)
	if !IsAbort(err) {
		t.Fatalf("abort must propagate, got %v", err)
	}
	if hook.called.Load() != 0 {
		t.Fatal("abort must skip both on_success and on_failure")
	}
}

func TestHandleBranchesSkipTreatedAsSuccess(t *testing.T) {
	t.Parallel()
	hook := &fakeRule{name: "after"}
	main := &compiledStep{
		kind:      stepKindRule,
		rule:      &fakeRule{name: "main", run: func(c *Context, _ StepConfig) error { return Skip }},
		onSuccess: []*compiledStep{{kind: stepKindRule, rule: hook}},
	}
	if err := runStep(newCtx(), main); err != nil {
		t.Fatalf("Skip must not propagate, got %v", err)
	}
	if hook.called.Load() != 1 {
		t.Fatal("on_success must run when main returned Skip")
	}
}

func TestHandleBranchesRealErrorWithoutFailureBranch(t *testing.T) {
	t.Parallel()
	main := &compiledStep{
		kind: stepKindRule,
		rule: &fakeRule{name: "main", run: func(c *Context, _ StepConfig) error { return errors.New("fail") }},
	}
	if err := runStep(newCtx(), main); err == nil {
		t.Fatal("error without on_failure must propagate")
	}
}

func TestRunParallelEmptyOk(t *testing.T) {
	t.Parallel()
	if err := runParallel(newCtx(), nil); err != nil {
		t.Fatalf("empty parallel must be a no-op, got %v", err)
	}
}

func TestRunParallelRunsConcurrently(t *testing.T) {
	t.Parallel()
	var ready sync.WaitGroup
	ready.Add(2)
	gate := make(chan struct{})
	mk := func(name string) *fakeRule {
		return &fakeRule{name: name, run: func(c *Context, _ StepConfig) error {
			ready.Done()
			<-gate
			return nil
		}}
	}
	a := mk("a")
	b := mk("b")
	steps := []*compiledStep{
		{kind: stepKindRule, rule: a},
		{kind: stepKindRule, rule: b},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- runParallel(newCtx(), steps) }()
	ready.Wait() // both rules have started — proves concurrency
	close(gate)
	if err := <-errCh; err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRunParallelFirstErrorWins(t *testing.T) {
	t.Parallel()
	a := &fakeRule{name: "a", run: func(c *Context, _ StepConfig) error { return errors.New("a-fail") }}
	b := &fakeRule{name: "b"}
	steps := []*compiledStep{
		{kind: stepKindRule, rule: a},
		{kind: stepKindRule, rule: b},
	}
	err := runParallel(newCtx(), steps)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunParallelAbortBeatsPlainError(t *testing.T) {
	t.Parallel()
	plain := &fakeRule{name: "plain", run: func(c *Context, _ StepConfig) error { return errors.New("oops") }}
	denied := &fakeRule{name: "denied", run: func(c *Context, _ StepConfig) error { return Abort("denied") }}
	// Plain first so its error lands first; abort should still win.
	steps := []*compiledStep{
		{kind: stepKindRule, rule: plain},
		{kind: stepKindRule, rule: denied},
	}
	// We retry a few times to make ordering non-flaky on fast systems.
	for i := 0; i < 20; i++ {
		err := runParallel(newCtx(), steps)
		if IsAbort(err) {
			return
		}
	}
	t.Fatal("abort never won over plain error across 20 runs")
}

func TestRunRuleVerifyWritesAccepted(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", writes: []string{"a"}, run: func(c *Context, _ StepConfig) error {
		c.Set("a", 1)
		return nil
	}}
	if err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r}); err != nil {
		t.Fatalf("declared write must pass verification, got %v", err)
	}
}

func TestRunRuleVerifyWritesRejectsUndeclared(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", writes: []string{"a"}, run: func(c *Context, _ StepConfig) error {
		c.Set("a", 1)
		c.Set("b", 2)
		return nil
	}}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	if err == nil {
		t.Fatal("undeclared write must surface as RuleError")
	}
	var re *RuleError
	if !errors.As(err, &re) || re.Rule != "x" {
		t.Fatalf("expected RuleError for 'x', got %v", err)
	}
	if !strings.Contains(re.Err.Error(), "\"b\"") {
		t.Fatalf("error must name the offending key, got %q", re.Err.Error())
	}
}

func TestRunRuleVerifyWritesPicksFirstAlphabetical(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", run: func(c *Context, _ StepConfig) error {
		c.Set("zeta", 1)
		c.Set("alpha", 1)
		return nil
	}}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	var re *RuleError
	if !errors.As(err, &re) || !strings.Contains(re.Err.Error(), "\"alpha\"") {
		t.Fatalf("error must name alpha first, got %v", err)
	}
}

func TestRunRuleVerifyIgnoresPreexistingKeys(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x", writes: nil}
	c := newCtx()
	c.Set("preexisting", 42)
	if err := runRule(c, &compiledStep{kind: stepKindRule, rule: r}); err != nil {
		t.Fatalf("pre-existing key must not trigger verification, got %v", err)
	}
}

func TestRunRuleVerifyWritesMasksRuleError(t *testing.T) {
	t.Parallel()
	boom := errors.New("downstream-fail")
	r := &fakeRule{name: "x", run: func(c *Context, _ StepConfig) error {
		c.Set("undeclared", 1)
		return boom
	}}
	err := runRule(newCtx(), &compiledStep{kind: stepKindRule, rule: r})
	if errors.Is(err, boom) {
		t.Fatal("verifyWrites error must mask the rule's own error")
	}
}

func TestVerifyWritesEmptyObserverPasses(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "x"}
	if err := verifyWrites(r, &writeObserver{}); err != nil {
		t.Fatalf("rule that wrote nothing must pass verification, got %v", err)
	}
}

func TestVerifyWritesParallelSafe(t *testing.T) {
	t.Parallel()
	// Verifies the bug fix the observer model is for: two
	// parallel rules each writing their declared key must NOT
	// trip each other's verification. Before the observer, this
	// scenario produced a false-positive "undeclared key" error
	// when one rule's Range happened to see the peer's write.
	a := &fakeRule{name: "a", writes: []string{"x"}, run: func(c *Context, _ StepConfig) error {
		c.Set("x", 1)
		return nil
	}}
	b := &fakeRule{name: "b", writes: []string{"y"}, run: func(c *Context, _ StepConfig) error {
		c.Set("y", 1)
		return nil
	}}
	steps := []*compiledStep{
		{kind: stepKindRule, rule: a},
		{kind: stepKindRule, rule: b},
	}
	if err := runParallel(newCtx(), steps); err != nil {
		t.Fatalf("parallel rules with disjoint declared writes must not falsely fail, got %v", err)
	}
}

func TestParallelKindThroughRunStep(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "p"}
	s := &compiledStep{
		kind:  stepKindParallel,
		group: []*compiledStep{{kind: stepKindRule, rule: r}},
	}
	if err := runStep(newCtx(), s); err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.called.Load() != 1 {
		t.Fatal("parallel kind via runStep must execute its group")
	}
}

func TestSequenceKindRunsGroup(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "inner"}
	s := &compiledStep{
		kind: stepKindSequence,
		group: []*compiledStep{
			{kind: stepKindRule, rule: r},
		},
	}
	if err := runStep(newCtx(), s); err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.called.Load() != 1 {
		t.Fatal("sequence group must run its child")
	}
}

func TestCompiledPipelineRun(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "r"}
	p := &compiledPipeline{
		name: "p",
		flow: []*compiledStep{{kind: stepKindRule, rule: r}},
	}
	if err := p.run(newCtx()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.called.Load() != 1 {
		t.Fatal("pipeline.run must execute its flow")
	}
}
