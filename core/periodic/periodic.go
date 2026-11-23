// Package periodic is a tiny in-process scheduler. It runs a function
// on a recurring interval, with two niceties that a raw time.Ticker
// can't do:
//
//   - RetryInterval: when the function returns an error, the next
//     wakeup uses this shorter cadence so a transient failure (DNS
//     blip, upstream 503) recovers fast without spamming. After the
//     next success the schedule snaps back to Interval.
//   - Jitter: ±N added to every wakeup so N service replicas don't
//     all hit the same upstream at the exact same second.
//
// Internally it uses time.Timer with Reset(), not time.Ticker —
// Ticker has a fixed cadence by design and can't accommodate the
// retry-on-error path. From the caller's POV the behaviour is
// ticker-like: register a job, it runs forever in a goroutine until
// ctx is cancelled.
//
// One job per Run call. To register many, call it many times — each
// spawns its own goroutine. There's no central registry by design:
// bookkeeping should live with the consumer.
//
//	periodic.Run(ctx, periodic.Job{
//	    Name:          "cf_cidrs_refresh",
//	    Interval:      24 * time.Hour,
//	    RetryInterval: 1 * time.Hour,
//	    Jitter:        30 * time.Minute,
//	    Fn:            cidrSet.Refresh,
//	})
package periodic

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// logger is the minimal logging surface periodic needs. *slog.Logger
// satisfies it directly, as does the canonical logger returned by
// log.Default(); keeping the interface unexported avoids leaking an
// extra type into this package's public API — Job.Logger accepts any
// value that fits, which in practice is always *slog.Logger.
type logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// Job describes one recurring task. The zero value is incomplete
// (no Run = nothing to do); set Name + Interval + Run at minimum.
type Job struct {
	// Name labels the job in log records (`name=…`). Required for
	// readable logs when a service registers more than one ticker.
	Name string

	// Interval is the happy-path cadence. Every successful Run
	// schedules the next wakeup `Interval ± Jitter` from now.
	Interval time.Duration

	// RetryInterval is the cadence used after Run returns a non-nil
	// error. Zero falls back to Interval. Use a value smaller than
	// Interval (e.g. 1h vs 24h) so a recovery happens quickly
	// without spamming the upstream during a sustained outage.
	RetryInterval time.Duration

	// Jitter is the maximum +/- variation applied to every wakeup.
	// Zero disables jitter (predictable but synchronizes replicas).
	// 30m on a 24h interval is plenty for cross-pod desynchronisation.
	Jitter time.Duration

	// RunOnStart fires Run once immediately when the goroutine
	// starts, before the first Interval-long sleep. Use when "fresh
	// data right away" matters (cache priming) and skip when the
	// upstream is expensive enough that a startup-storm hurts.
	RunOnStart bool

	// Fn is the job body. Returning a non-nil error switches the
	// next schedule to RetryInterval. A panic terminates the
	// goroutine — wrap with defer/recover if survival matters.
	Fn func(ctx context.Context) error

	// Logger records start / OK / error events. nil = log.Default.
	Logger logger
}

// Run starts the goroutine and returns immediately. The goroutine
// runs until ctx is cancelled. No way to deregister a single job —
// cancel ctx (which the caller probably bound to SIGTERM) to stop
// everything together.
func Run(ctx context.Context, j Job) {
	lg := j.Logger
	if lg == nil {
		lg = log.Default()
	}
	retry := j.RetryInterval
	if retry <= 0 {
		retry = j.Interval
	}

	go func() {
		// time.Timer (not Ticker) so we can vary the next wait
		// based on whether the previous Fn succeeded. Initial
		// wait honours RunOnStart and its return value: a failed
		// first-fire schedules the next attempt at RetryInterval,
		// not Interval — important when RunOnStart is being used
		// to recover from a failed boot-time fetch.
		first := j.Interval
		if j.RunOnStart {
			if err := runOnce(ctx, j, lg); err != nil {
				first = retry
			}
		}
		timer := time.NewTimer(nextWait(first, j.Jitter))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				err := runOnce(ctx, j, lg)
				next := j.Interval
				if err != nil {
					next = retry
				}
				timer.Reset(nextWait(next, j.Jitter))
			}
		}
	}()
}

// runOnce wraps Fn with timing + structured log. Errors are logged
// here so each periodic job has a uniform observability footprint.
//
// Duration is emitted as two human-readable numeric fields rather
// than a raw time.Duration: slog serialises a Duration as its
// int64 nanosecond count ("duration":5864207), which is precise
// but unreadable and awkward to graph. duration_ms (fractional
// milliseconds) is the natural axis for the sub-second jobs this
// scheduler runs; duration_s (seconds) reads naturally for the
// occasional slow sweep. Both are float64 so a Loki / Grafana
// query can plot either without parsing.
func runOnce(ctx context.Context, j Job, lg logger) error {
	start := time.Now()
	err := j.Fn(ctx)
	dur := time.Since(start)
	durMS := float64(dur.Microseconds()) / 1000.0
	durS := dur.Seconds()
	if err != nil {
		lg.Error("periodic job failed",
			"name", j.Name, "duration_ms", durMS, "duration_s", durS, "error", err)
		return err
	}
	lg.Info("periodic job ran",
		"name", j.Name, "duration_ms", durMS, "duration_s", durS)
	return nil
}

// nextWait returns base ± rand([0, jitter*2)) − jitter, i.e. a
// uniform distribution in [base-jitter, base+jitter). When jitter
// is zero the result is just base. math/rand/v2 is concurrency-safe
// and seeded automatically (no manual seeding needed since Go 1.22).
func nextWait(base, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return base
	}
	delta := time.Duration(rand.Int64N(int64(jitter*2))) - jitter
	if base+delta < 0 {
		return base
	}
	return base + delta
}
