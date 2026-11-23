package system

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// TestMain disables the drain delay for the package so the existing
// lifecycle tests stay fast; drain tests set their own value.
func TestMain(m *testing.M) {
	_ = os.Setenv(constants.EnvShutdownDelay, "0")
	os.Exit(m.Run())
}

// drainProbe records whether Draining was already true when its
// Shutdown ran, i.e. readiness flipped before modules stopped.
type drainProbe struct {
	mu              sync.Mutex
	sawDraining     bool
	shutdownAt      time.Time
	drainingAtStart bool
}

func (p *drainProbe) Register(context.Context) error {
	p.drainingAtStart = Draining()
	return nil
}

func (p *drainProbe) Shutdown(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sawDraining = Draining()
	p.shutdownAt = time.Now()
	return nil
}

func TestRunForever_DrainsBeforeShutdown(t *testing.T) {
	t.Cleanup(func() { draining.Store(false) })
	t.Setenv(constants.EnvShutdownDelay, "50ms")
	draining.Store(false)

	probe := &drainProbe{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunForever(ctx, probe) }()
	time.Sleep(10 * time.Millisecond)
	if Draining() {
		t.Fatal("draining before the signal")
	}

	signalled := time.Now()
	cancel()
	// Readiness flips at once, well before the delay elapses.
	deadline := time.Now().Add(40 * time.Millisecond)
	for !Draining() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !Draining() {
		t.Fatal("Draining not set promptly after the signal")
	}
	probe.mu.Lock()
	stoppedEarly := !probe.shutdownAt.IsZero()
	probe.mu.Unlock()
	if stoppedEarly {
		t.Fatal("module stopped before the drain delay")
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if probe.drainingAtStart || !probe.sawDraining {
		t.Fatalf("draining at start=%v, at shutdown=%v", probe.drainingAtStart, probe.sawDraining)
	}
	if d := probe.shutdownAt.Sub(signalled); d < 50*time.Millisecond {
		t.Fatalf("modules stopped %v after the signal, want >= drain delay", d)
	}
}

// TestRunForever_DelayBoundedByTimeout: the delay never outlives the
// shutdown budget.
func TestRunForever_DelayBoundedByTimeout(t *testing.T) {
	t.Cleanup(func() { draining.Store(false) })
	t.Setenv(constants.EnvShutdownDelay, "1h")
	t.Setenv(constants.EnvShutdownTimeout, "30ms")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := RunForever(ctx, &drainProbe{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("RunForever took %v, want ~30ms", d)
	}
}

func TestShutdownDelay(t *testing.T) {
	cases := map[string]time.Duration{
		"":     defaultShutdownDelay,
		"0":    0,
		"2s":   2 * time.Second,
		" 1s ": time.Second,
		"-1s":  defaultShutdownDelay,
		"oops": defaultShutdownDelay,
	}
	for in, want := range cases {
		t.Setenv(constants.EnvShutdownDelay, in)
		if got := shutdownDelay(); got != want {
			t.Errorf("shutdownDelay(%q) = %v, want %v", in, got, want)
		}
	}
}
