package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
)

// defaultCheckTimeout caps each individual Ping when
// HealthzOptions.Timeout is zero. Long enough that a healthy dep with
// one slow query still passes; short enough that an unresponsive dep
// doesn't pile up requests waiting for the readiness probe to time
// out at the kubelet layer (default kubelet probe timeout is 1s — we
// run just under that for the per-check budget so the overall
// response still beats the kubelet timeout).
const defaultCheckTimeout = 2 * time.Second

// Status strings emitted in the JSON body. Pulled out so the response
// shape is one place to grep when wiring kubelet probes or alerts.
const (
	statusOK       = "ok"
	statusFail     = "fail"
	statusDegraded = "degraded"
	statusDrain    = "draining"
)

// logger is the minimal logging surface Healthz needs. *log.Logger
// satisfies it as-is, so callers keep passing log.Default() /
// log.New(...) without adapters; expressing it as an interface
// keeps this package free of any direct logging-library dependency
// — core/log stays the single point of swap.
type logger interface {
	WarnContext(ctx context.Context, msg string, args ...any)
}

// Check is one named dependency that Healthz can probe (Postgres,
// Redis, NATS, MinIO, an upstream service). Implementations must be
// cheap and fast: a Ping that takes longer than the per-check
// Timeout is reported as a failure even if the underlying dep would
// have succeeded.
type Check interface {
	Name() string
	Ping(ctx context.Context) error
}

// CheckFunc adapts a function to the Check interface so callers can
// pass an inline closure instead of declaring a type per dep:
//
//	observability.CheckFunc{Label: "postgres", Fn: pool.Ping}
type CheckFunc struct {
	Label string
	Fn    func(ctx context.Context) error
}

func (c CheckFunc) Name() string                   { return c.Label }
func (c CheckFunc) Ping(ctx context.Context) error { return c.Fn(ctx) }

// HealthzOptions tunes how Healthz runs the dependency probes.
type HealthzOptions struct {
	// Timeout caps each individual Ping. Zero falls back to
	// defaultCheckTimeout.
	Timeout time.Duration

	// Logger receives a warn line when a check fails. nil =
	// log.Default(), so a service that called log.SetDefault at boot
	// gets service-tagged failure lines for free.
	Logger logger
}

// Healthz returns an http.Handler that runs every dep concurrently
// and serves a JSON summary:
//
//	{
//	  "status": "ok" | "degraded",
//	  "checks": [
//	    {"name": "postgres", "status": "ok",   "duration_ms": 4},
//	    {"name": "redis",    "status": "fail", "duration_ms": 2003,
//	     "error": "context deadline exceeded"}
//	  ]
//	}
//
// HTTP 200 when all checks pass, 503 otherwise. Suitable for
// kubelet `readinessProbe.httpGet.path: /readyz`. Liveness probes
// (`/healthz`) should stay separate and trivial — that endpoint
// only proves the process is alive, not that it can do work.
//
// With no deps, Healthz returns 200 immediately. While the process
// drains after its shutdown signal (system.Draining) it answers 503
// {"status":"draining"} without running the checks.
func Healthz(opts HealthzOptions, deps ...Check) http.Handler {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if system.Draining() {
			// Shutdown signal received: leave the load balancer
			// without probing dependencies.
			w.Header().Set(constants.HeaderContentType, "application/json; charset=utf-8")
			w.Header().Set(constants.HeaderCacheControl, "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(healthzResponse{Status: statusDrain, Checks: []checkResult{}})
			return
		}
		results := make([]checkResult, len(deps))
		var wg sync.WaitGroup
		for i, dep := range deps {
			wg.Add(1)
			go func(i int, dep Check) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), timeout)
				defer cancel()
				start := time.Now()
				err := dep.Ping(ctx)
				results[i] = checkResult{
					Name:       dep.Name(),
					Status:     statusOK,
					DurationMS: time.Since(start).Milliseconds(),
				}
				if err != nil {
					results[i].Status = statusFail
					results[i].Error = err.Error()
					logger.WarnContext(r.Context(), "healthz check failed",
						"check", dep.Name(),
						"err", err,
						"duration_ms", results[i].DurationMS,
					)
				}
			}(i, dep)
		}
		wg.Wait()

		overall := statusOK
		status := http.StatusOK
		for _, res := range results {
			if res.Status != statusOK {
				overall = statusDegraded
				status = http.StatusServiceUnavailable
				break
			}
		}

		w.Header().Set(constants.HeaderContentType, "application/json; charset=utf-8")
		w.Header().Set(constants.HeaderCacheControl, "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(healthzResponse{
			Status: overall,
			Checks: results,
		})
	})
}

type healthzResponse struct {
	Status string        `json:"status"`
	Checks []checkResult `json:"checks"`
}

type checkResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}
