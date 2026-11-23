package system

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// Module is implemented by every worker that wants to register itself
// into a service. Register wires up NATS consumers / starts an HTTP
// listener / launches background goroutines, and returns immediately —
// long-lived work runs in goroutines anchored to the lifecycle ctx.
type Module interface {
	Register(ctx context.Context) error
}

// ShutdownModule is the optional second half. If a module holds
// resources that need orderly teardown (open HTTP server, in-flight
// NATS handlers, websocket clients), implement Shutdown — RunForever
// detects the interface via type assertion and calls it before the
// process exits.
//
// shutdownCtx has a deadline (constants.EnvShutdownTimeout, default
// 25s — under k8s' 30s terminationGracePeriodSeconds). Honour it:
// if the deadline fires, return whatever you have rather than
// blocking forever.
type ShutdownModule interface {
	Module
	Shutdown(shutdownCtx context.Context) error
}

// RunForever is the standard service entrypoint. It:
//
//  1. Filters nil entries so callers can pass conditionally-built
//     modules (e.g. `devServer`, which is nil in prod) directly,
//     without an `if x != nil { append }` dance.
//  2. Calls Register on every remaining module in declaration order.
//     If any fails, no Shutdown is run (nothing started yet) and
//     the error is returned.
//  3. Blocks on ctx (the signal-cancellable context returned by Boot).
//  4. On SIGTERM/SIGINT, flips Draining to true at once (readiness
//     handlers answer 503 from then on), then waits the drain delay
//     (constants.EnvShutdownDelay, default 5s) while still serving,
//     so the load balancer stops routing to this instance before any
//     listener closes.
//  5. Calls Shutdown on every ShutdownModule in REVERSE order.
//     The drain delay and the module shutdowns share one
//     EnvShutdownTimeout budget (default 25s: 5s delay + up to 20s of
//     module shutdown, under k8s' 30s terminationGracePeriodSeconds).
//  6. Returns the first non-nil error from the shutdown phase, or nil
//     if everything stopped cleanly.
//
// Reverse order matters: if module B was registered after A, B's
// resources may depend on A's still being up. Tearing down in the
// reverse of construction is the safe default.
//
// Typical wiring:
//
//	ctx := system.Boot("service-edge", system.WithPort(8080))
//	if err := system.RunForever(ctx,
//	    system.NewServer(system.ServerOptions{Handler: handler}),
//	    devServer, // may be nil; RunForever skips it
//	); err != nil {
//	    log.Error("run", "err", err)
//	    os.Exit(1)
//	}
func RunForever(ctx context.Context, modules ...Module) error {
	modules = nonNilModules(modules)
	for _, m := range modules {
		if err := m.Register(ctx); err != nil {
			return fmt.Errorf("register %T: %w", m, err)
		}
	}

	<-ctx.Done()
	// Release the OS signal handler so the next Ctrl-C kills the
	// process immediately, rather than queueing behind the orderly
	// drain we're about to run.
	if appStop != nil {
		appStop()
	}
	SetDraining(true)
	timeout := shutdownTimeout()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	delay := shutdownDelay()
	log.Info("shutdown signal received — draining: readiness now 503", "delay", delay.String(), "budget", timeout.String())
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-shutdownCtx.Done():
			t.Stop()
		}
	}
	log.Info("drain delay over — stopping modules")

	var firstErr error
	// Reverse order: last-registered tears down first.
	for i := len(modules) - 1; i >= 0; i-- {
		sm, ok := modules[i].(ShutdownModule)
		if !ok {
			continue
		}
		if err := sm.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown failed",
				"module", fmt.Sprintf("%T", modules[i]), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	log.Info("service stopped")
	return firstErr
}

// nonNilModules drops nil entries so callers can pass
// conditionally-built modules (e.g. dev-only servers that are nil
// in prod) directly to RunForever — no `if m != nil { append }`
// boilerplate at the call site.
//
// Catches both forms of "nil module":
//
//   - Plain interface nil — `var m Module; m == nil` is true.
//   - Typed nil — `var s *Server; var m Module = s` makes m
//     non-nil at the interface level (it carries a type) but
//     calling m.Register would NPE. Reflect spots the underlying
//     nil pointer.
//
// Without the reflect path, `RunForever(ctx, server, devServer)`
// with `devServer *dev.Server == nil` would still panic on
// Register — exactly the call-site shape this helper exists to
// support.
func nonNilModules(in []Module) []Module {
	out := make([]Module, 0, len(in))
	for _, m := range in {
		if m == nil {
			continue
		}
		v := reflect.ValueOf(m)
		if v.Kind() == reflect.Ptr && v.IsNil() {
			continue
		}
		out = append(out, m)
	}
	return out
}

// defaultShutdownTimeout is the bound applied when
// constants.EnvShutdownTimeout is unset or unparseable. Under k8s'
// default 30s terminationGracePeriodSeconds this leaves ~5s of
// headroom for the container runtime to actually kill the process
// after Shutdown returns. Bump the env var (and the k8s grace period
// to match) when a service genuinely needs more.
const defaultShutdownTimeout = 25 * time.Second

// shutdownTimeout reads constants.EnvShutdownTimeout (a Go duration
// like "25s", "1m") and falls back to defaultShutdownTimeout.
func shutdownTimeout() time.Duration {
	s := strings.TrimSpace(os.Getenv(constants.EnvShutdownTimeout))
	if s == "" {
		return defaultShutdownTimeout
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return defaultShutdownTimeout
}

// draining is set by RunForever when the shutdown signal arrives.
var draining atomic.Bool

// Draining reports whether the process received its shutdown signal
// and is draining. Readiness handlers answer 503 while it is true so
// the load balancer stops routing new requests here; liveness must
// stay 200 (the process is healthy, just leaving).
func Draining() bool { return draining.Load() }

// SetDraining sets the flag Draining reports. RunForever sets it on
// the shutdown signal; a custom lifecycle (or a test of a readiness
// handler) may set it directly.
func SetDraining(v bool) { draining.Store(v) }

// defaultShutdownDelay is the drain delay applied when
// constants.EnvShutdownDelay is unset or unparseable: long enough for
// kube-proxy / ingress controllers to see the failed readiness probe
// or the endpoint removal and stop sending traffic.
const defaultShutdownDelay = 5 * time.Second

// shutdownDelay reads constants.EnvShutdownDelay (a Go duration;
// "0" disables the delay) and falls back to defaultShutdownDelay.
func shutdownDelay() time.Duration {
	s := strings.TrimSpace(os.Getenv(constants.EnvShutdownDelay))
	if s == "" {
		return defaultShutdownDelay
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d
	}
	return defaultShutdownDelay
}
