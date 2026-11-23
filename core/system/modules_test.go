package system

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// registerOnly is a Module without a Shutdown half. RunForever should
// register it, then skip it during the shutdown loop.
type registerOnly struct {
	name        string
	registerErr error
	registered  bool
}

func (r *registerOnly) Register(ctx context.Context) error {
	r.registered = true
	return r.registerErr
}

// withShutdown is a ShutdownModule whose Shutdown records its name in
// a shared slice so tests can assert teardown order across modules.
type withShutdown struct {
	name        string
	registerErr error
	shutdownErr error
	registered  bool

	mu    *sync.Mutex
	order *[]string
}

func (w *withShutdown) Register(ctx context.Context) error {
	w.registered = true
	return w.registerErr
}

func (w *withShutdown) Shutdown(ctx context.Context) error {
	if w.mu != nil {
		w.mu.Lock()
		*w.order = append(*w.order, w.name)
		w.mu.Unlock()
	}
	return w.shutdownErr
}

// TestRunForever_RegisterErrorAborts — if any module's Register
// fails, RunForever returns the wrapped error WITHOUT running any
// Shutdown. Nothing was started, so there's nothing to drain — and
// running shutdowns on half-registered state is the kind of subtle
// hazard this test is here to lock down.
func TestRunForever_RegisterErrorAborts(t *testing.T) {
	mu := &sync.Mutex{}
	order := []string{}
	m1 := &withShutdown{name: "m1", mu: mu, order: &order}
	m2 := &registerOnly{name: "m2", registerErr: errors.New("boom")}
	m3 := &withShutdown{name: "m3", mu: mu, order: &order}

	err := RunForever(context.Background(), m1, m2, m3)
	if err == nil {
		t.Fatal("expected register error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to wrap \"boom\"", err)
	}
	// m1 registered; m2 errored; m3 never reached. Nothing got Shutdown.
	if !m1.registered {
		t.Error("m1.Register should have been called before m2 failed")
	}
	if m3.registered {
		t.Error("m3.Register should NOT have run after m2 failed")
	}
	if len(order) != 0 {
		t.Errorf("Shutdown ran (%v) despite a Register failure", order)
	}
}

// TestRunForever_ShutdownInReverseOrder — last-registered tears down
// first. Reverse order matters because module N+1 is allowed to
// depend on module N's still-running state at registration time;
// matching that on shutdown keeps the dependency graph valid all the
// way through teardown.
func TestRunForever_ShutdownInReverseOrder(t *testing.T) {
	mu := &sync.Mutex{}
	order := []string{}
	m1 := &withShutdown{name: "m1", mu: mu, order: &order}
	m2 := &withShutdown{name: "m2", mu: mu, order: &order}
	m3 := &registerOnly{name: "m3"} // no Shutdown method
	m4 := &withShutdown{name: "m4", mu: mu, order: &order}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunForever(ctx, m1, m2, m3, m4) }()

	// Let Register on all four finish before triggering shutdown.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunForever returned err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunForever did not return after ctx cancel")
	}

	want := []string{"m4", "m2", "m1"} // m3 has no Shutdown; reverse of m1,m2,m4
	if !reflect.DeepEqual(order, want) {
		t.Errorf("shutdown order = %v, want %v", order, want)
	}
}

// TestRunForever_FirstShutdownErrorReturned — when multiple modules
// fail to shut down, RunForever must keep going (so every module gets
// its drain attempt) but surface only the FIRST error encountered.
// Reverse order means the first-tried module is the last-registered.
func TestRunForever_FirstShutdownErrorReturned(t *testing.T) {
	mu := &sync.Mutex{}
	order := []string{}
	m1 := &withShutdown{name: "m1", shutdownErr: errors.New("m1-err"),
		mu: mu, order: &order}
	m2 := &withShutdown{name: "m2", shutdownErr: errors.New("m2-err"),
		mu: mu, order: &order}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunForever(ctx, m1, m2) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-done
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Reverse order: m2 first → m2-err is the "first" error.
	if !strings.Contains(err.Error(), "m2-err") {
		t.Errorf("err = %v, want it to be m2-err (first in reverse order)", err)
	}
	// Both modules' Shutdown still ran — RunForever must not bail
	// on the first failure or m1 would leak.
	want := []string{"m2", "m1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("shutdown order = %v, want %v (both should run)", order, want)
	}
}

// TestRunForever_NoShutdownModulesIsClean — modules that don't
// implement Shutdown are a normal case (e.g. periodic.Run-launched
// jobs that exit when ctx cancels). RunForever must not spin or
// error on them.
func TestRunForever_NoShutdownModulesIsClean(t *testing.T) {
	m1 := &registerOnly{name: "m1"}
	m2 := &registerOnly{name: "m2"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunForever(ctx, m1, m2) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunForever did not return")
	}
}

// TestRunForever_SkipsTypedNilModules — the call-site shape this
// helper really exists for: a `*MyModule` variable that's nil
// (e.g. dev-only server in prod) passed as a Module argument.
// At the interface level the value is NOT == nil because it
// carries a type pointer, so the plain `m == nil` check misses
// it; reflect catches the underlying nil pointer.
func TestRunForever_SkipsTypedNilModules(t *testing.T) {
	mu := &sync.Mutex{}
	order := []string{}
	m1 := &withShutdown{name: "m1", mu: mu, order: &order}
	var devServer *withShutdown // typed nil; at interface level NOT == nil

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunForever(ctx, m1, devServer) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunForever returned err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunForever did not return — typed-nil filter likely failed")
	}
	if !m1.registered {
		t.Error("real module was not registered")
	}
}

// TestRunForever_SkipsNilModules — nil entries must be filtered
// out before Register runs, so callers can hand RunForever a
// conditionally-built module (e.g. dev-only server, nil in prod)
// without pre-flight boilerplate. Without the filter, Register
// would NPE on the nil entry.
func TestRunForever_SkipsNilModules(t *testing.T) {
	mu := &sync.Mutex{}
	order := []string{}
	m1 := &withShutdown{name: "m1", mu: mu, order: &order}
	m2 := &withShutdown{name: "m2", mu: mu, order: &order}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Sandwich a nil between two real modules; both must register
	// + shut down without the nil tripping anything.
	go func() { done <- RunForever(ctx, m1, nil, m2) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunForever returned err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunForever did not return")
	}
	if !m1.registered || !m2.registered {
		t.Error("real modules around a nil were not registered")
	}
	want := []string{"m2", "m1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("shutdown order = %v, want %v", order, want)
	}
}

// TestShutdownTimeout_Default — unset EnvShutdownTimeout must yield
// 25s (the documented value, sized to fit under k8s' default 30s
// terminationGracePeriodSeconds with headroom for the runtime).
func TestShutdownTimeout_Default(t *testing.T) {
	t.Setenv(constants.EnvShutdownTimeout, "")
	if got := shutdownTimeout(); got != 25*time.Second {
		t.Errorf("shutdownTimeout() = %v, want 25s", got)
	}
}

// TestShutdownTimeout_Override — a valid Go duration string must be
// honoured. Services that genuinely need longer (heavy DB drain) are
// expected to bump both this var and the k8s grace period together.
func TestShutdownTimeout_Override(t *testing.T) {
	t.Setenv(constants.EnvShutdownTimeout, "1m")
	if got := shutdownTimeout(); got != time.Minute {
		t.Errorf("shutdownTimeout() = %v, want 1m", got)
	}
}

// TestShutdownTimeout_BadValueFallback — an unparseable value must
// fall back to the default rather than panicking. Misconfiguration
// should never block a SIGTERM drain in production.
func TestShutdownTimeout_BadValueFallback(t *testing.T) {
	t.Setenv(constants.EnvShutdownTimeout, "not-a-duration")
	if got := shutdownTimeout(); got != 25*time.Second {
		t.Errorf("shutdownTimeout() = %v, want 25s fallback", got)
	}
}

// TestShutdownTimeout_NonPositiveRejected — a "0s" or negative value
// must NOT be used as the deadline. Otherwise context.WithTimeout
// would fire immediately and Shutdown would never get a chance.
func TestShutdownTimeout_NonPositiveRejected(t *testing.T) {
	t.Setenv(constants.EnvShutdownTimeout, "0s")
	if got := shutdownTimeout(); got != 25*time.Second {
		t.Errorf("shutdownTimeout() = %v, want 25s (zero rejected)", got)
	}
}
