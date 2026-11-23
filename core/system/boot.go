// Package system consolidates the boilerplate prelude every
// service repeats in its main(): .env loading, logger setup, startup
// banner, build-time log line, and a signal-cancellable context. It
// also owns the module-lifecycle loop (RunForever) so a service
// entrypoint is just Boot → wire → RunForever.
//
// What it does NOT do:
//   - Open Mongo. Each service decides whether it needs it; call
//     mongo.New(ctx) yourself after Boot when it does. Boot doesn't
//     manage that lifetime so tests / CLIs / migration jobs can opt in
//     selectively.
//
// The HTTP server lives in this package too — NewServer returns a
// Module that drops into RunForever alongside any other lifecycle
// workers (NATS consumers, dev watchers, schedulers).
package system

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/hblabs-co/kit/core/banner"
	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// logger is the minimal logging surface Boot needs internally. The
// canonical *log.Logger from core/log satisfies it, as do test
// spies that want to observe Boot's "build" line without wiring a
// real logger. Keeping it unexported avoids leaking an extra type
// from system into callers who already work with *log.Logger.
type logger interface {
	Info(msg string, args ...any)
}

// Option is the unit bootOptions.With consumes. Every boot-time
// knob (port, banner extras, logger override, etc.) is exposed as
// a constructor in this package that returns an Option, and
// bootOptions.With dispatches by GetName to write the right private
// field. Adding a new knob means writing one constructor + one case
// in With — nothing else changes at the call site.
type Option interface {
	GetName() string
	GetValue() any
}

// bootOption is the concrete carrier for every option this package
// produces. Unexported because callers should always go through the
// public constructors (Port, BannerExtras, …) so the option name
// matches one of the keys With understands.
type bootOption struct {
	name  string
	value any
}

func (o bootOption) GetName() string { return o.name }
func (o bootOption) GetValue() any   { return o.value }

// Option keys. Unexported so callers can't synthesise an option
// from outside this package — every Option must come from a
// constructor below, which guarantees the name matches a case in
// With. Pulled out as consts (rather than string literals) so the
// compiler catches typos.
const (
	optPort         = "port"
	optBannerExtras = "banner_extras"
	optSkipBanner   = "skip_banner"
	optSkipDotenv   = "skip_dotenv"
	optLogger       = "logger"
	optBuildVersion = "build_version"
	optBuildTime    = "build_time"
)

// Port sets the listen port the banner prints in its Local / Network
// URL lines. Zero or unset means the banner falls back to the
// title-only Box (no URLs). The HTTP listener itself is the
// caller's job — wire NewServer into RunForever to bind it.
func Port(p int) Option { return bootOption{name: optPort, value: p} }

// BannerExtras appends arbitrary lines to the boxed banner —
// "watching: …", "🔥  Hot-reload: on", any human-readable startup
// hint. Multiple With(BannerExtras(...)) calls accumulate.
func BannerExtras(lines ...string) Option {
	return bootOption{name: optBannerExtras, value: lines}
}

// WithoutBanner skips the boxed banner. Use for one-shot CLIs and
// migration jobs whose output is the only thing that matters.
func WithoutBanner() Option {
	return bootOption{name: optSkipBanner, value: true}
}

// WithoutDotenv skips godotenv.Load. Use in tests or when the
// caller has already loaded a specific .env file.
func WithoutDotenv() Option {
	return bootOption{name: optSkipDotenv, value: true}
}

// WithLogger overrides the logger Boot uses for its own boot-time
// log lines (the "build" record, primarily). Rare — usually you
// want the canonical one core/log builds for you, which Boot
// installs as the process default automatically.
//
// Note: when WithLogger is used Boot does NOT call log.SetDefault.
// The caller is responsible for installing whatever process-wide
// default they want via log.SetDefault before or after Boot. This
// keeps the system package free of any direct logging-library
// dependency: the parameter is an interface, not *log.Logger, so
// Boot can't pass it to log.SetDefault on its own.
func WithLogger(l logger) Option {
	return bootOption{name: optLogger, value: l}
}

// BuildVersion sets the human-readable version string that Boot
// logs and that downstream code (template helpers, /healthz, …)
// can read via HB_BUILD_VERSION. Intended to be the
// ldflags-injected Go var from main:
//
//	go build -ldflags="-X main.buildVersion=v1.0.487" …
//
// Empty strings and the conventional sentinels "dev" / "unknown"
// are ignored so a `go run` invocation without ldflags falls back
// to whatever HB_BUILD_VERSION already holds (often "").
func BuildVersion(v string) Option {
	return bootOption{name: optBuildVersion, value: v}
}

// BuildTime sets the build-stamp Boot logs and that downstream
// code can read via HB_BUILD_TIME. Same injection convention as
// BuildVersion. Use RFC 3339 (`date -u +%Y-%m-%dT%H:%M:%SZ`) so
// the value is sortable in log search.
func BuildTime(t string) Option {
	return bootOption{name: optBuildTime, value: t}
}

// Build is the convenience bundle for the common "I have both
// pieces" path. Returns a slice so callers can spread it into
// With:
//
//	With(system.Build(buildVersion, buildTime)...)
//
// Equivalent to passing BuildVersion + BuildTime individually.
func Build(version, time string) []Option {
	return []Option{BuildVersion(version), BuildTime(time)}
}

// bootOptions is the chainable configuration value Boot consumes.
// Build it with CreatebootOptions (which requires the service name
// up front) and apply knobs via the single With method — every
// boot-time setting is exposed as an Option constructor in this
// package, so adding a new knob is one constructor + one case in
// With, no API churn at every call site.
//
// Defaults: dotenv on, banner on, no port (banner skips the URL
// lines), no extras, logger built from the service name via
// core/log.
type bootOptions struct {
	service      string
	port         int
	bannerExtras []string
	skipBanner   bool
	skipDotenv   bool
	logger       logger
	buildVersion string
	buildTime    string
}

// CreatebootOptions returns a bootOptions seeded with the service
// name, ready for With. Service is required at construction
// (rather than as an option) so it's impossible to call Boot with
// an empty service — every log line, banner, and metric elsewhere
// keys off this string.
//
//	opts := system.CreatebootOptions(constants.ServiceFoo).With(
//	    system.Port(8080),
//	    system.BannerExtras(site.BannerExtras()...),
//	)
//	ctx := system.Boot(opts)
func BootOptions(service string) *bootOptions {
	return &bootOptions{service: service}
}

// With applies one or more Options to the receiver and returns it
// so calls can be chained. Each Option is dispatched by its
// GetName() to the matching private field; unknown names panic to
// surface the mistake at boot rather than silently no-op (this
// package owns every Option constructor, so an unknown name means
// either a missing case here or a bad value passed across packages).
//
// Type assertions on GetValue() panic on mismatch by design —
// constructors are the only sanctioned way to build an Option, so
// a wrong type here is a programmer error worth crashing loudly.
func (o *bootOptions) With(opts ...Option) *bootOptions {
	for _, opt := range opts {
		switch opt.GetName() {
		case optPort:
			o.port = opt.GetValue().(int)
		case optBannerExtras:
			o.bannerExtras = append(o.bannerExtras, opt.GetValue().([]string)...)
		case optSkipBanner:
			o.skipBanner = opt.GetValue().(bool)
		case optSkipDotenv:
			o.skipDotenv = opt.GetValue().(bool)
		case optLogger:
			o.logger = opt.GetValue().(logger)
		case optBuildVersion:
			o.buildVersion = opt.GetValue().(string)
		case optBuildTime:
			o.buildTime = opt.GetValue().(string)
		default:
			panic(fmt.Sprintf("system: bootOptions.With received unknown option %q — use a constructor from core/system", opt.GetName()))
		}
	}
	return o
}

// Boot is the standard prelude every service runs in main().
// One call instead of five, in a fixed order, returns the
// signal-cancellable context. Service name comes from opts.service
// (set by CreatebootOptions), so the entire boot config lives in
// one value and the call site stays a single argument:
//
//	ctx := system.Boot(system.CreatebootOptions("service-tools").
//	    With(system.WithoutBanner()))
//
//	ctx := system.Boot(system.CreatebootOptions("service-edge").With(
//	    system.Port(8080),
//	    system.BannerExtras("watching: services/service-edge/..."),
//	))
//
// Sequence:
//
//  1. .env load (best-effort; production gets vars from the cluster).
//  2. Logger from core/log, installed as the process default so
//     context-bound loggers inherit the service tag.
//  3. Banner with Local / Network URLs (when Port is set).
//  4. EnvBuildTime log line — invaluable when telling whether a
//     rollout "took" or k8s is still serving the old image.
//  5. signal.NotifyContext for SIGINT / SIGTERM. Returned so callers
//     can pass it to RunForever or compose with their own workers.
//
// Passing nil opts panics — service name is required, and silently
// substituting a placeholder would scatter empty service tags across
// every downstream log/metric. Use CreatebootOptions(name) even when
// no other knobs are needed.
//
// Calling Boot more than once in the same process is safe but only
// the first call installs the signal handler. Tests should treat
// Boot as a process-singleton.
func Boot(opts *bootOptions) context.Context {
	if opts == nil {
		panic("system: Boot requires a non-nil *bootOptions — use CreatebootOptions(serviceName)")
	}

	if !opts.skipDotenv {
		// godotenv.Load is best-effort: VS Code's launch.json already
		// feeds vars via envFile, and prod gets them from the cluster.
		// This call covers `go run ./services/...` from a plain shell.
		_ = godotenv.Load()
	}

	lg := opts.logger
	if lg == nil {
		// Default path: build the canonical service-tagged logger
		// and install it as the process default so context-bound
		// loggers (httpx.RequestID, log.WithContext) inherit the
		// service tag downstream.
		sl := log.New(opts.service)
		log.SetDefault(sl)
		lg = sl
	}
	// If WithLogger was used, the caller is on the hook for
	// installing whatever process default they want — Boot can't
	// safely round-trip an arbitrary logger interface into the
	// concrete *log.Logger that log.SetDefault demands.

	if !opts.skipBanner {
		if opts.port > 0 {
			banner.Print(opts.service, opts.port, opts.bannerExtras...)
		} else {
			banner.Box(opts.service, opts.bannerExtras...)
		}
	}

	// Resolve build metadata. Three sources, first non-empty wins:
	//   1. opts.buildVersion / opts.buildTime — explicit Boot
	//      options the service passed via With(system.Build(…)).
	//   2. package-level buildVersion / buildTime — ldflags
	//      injection targets services point the linker at with
	//      -X github.com/hblabs-co/kit/core/system.buildVersion=…
	//      (zero Go boilerplate in the service — see buildinfo.go).
	//   3. HB_BUILD_VERSION / HB_BUILD_TIME env vars — set by
	//      Docker images, .env files, or any external runner.
	// The resolved values are then PROMOTED back into env so
	// downstream code (template helpers, /healthz handlers,
	// ReadBuildInfo) reads from a single source of truth without
	// having to know which path supplied them.
	bv := strings.TrimSpace(opts.buildVersion)
	if bv == "" {
		bv = strings.TrimSpace(buildVersion)
	}
	if bv == "" {
		bv = strings.TrimSpace(os.Getenv(constants.EnvBuildVersion))
	}
	bt := strings.TrimSpace(opts.buildTime)
	if bt == "" {
		bt = strings.TrimSpace(buildTime)
	}
	if bt == "" {
		bt = strings.TrimSpace(os.Getenv(constants.EnvBuildTime))
	}
	if bv != "" {
		_ = os.Setenv(constants.EnvBuildVersion, bv)
	}
	if bt != "" {
		_ = os.Setenv(constants.EnvBuildTime, bt)
	}
	if bt != "" || bv != "" {
		// Single log line so a "which build is this pod running?"
		// grep returns one record with both fields, instead of
		// version and time landing in separate entries that have
		// to be correlated by timestamp.
		args := []any{}
		if bv != "" {
			args = append(args, "version", bv)
		}
		if bt != "" {
			args = append(args, "time", bt)
		}
		lg.Info("build", args...)
	}

	bootOnce.Do(func() {
		appCtx, appStop = signal.
			NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	})
	return appCtx
}

// appCtx / appStop hold the process-wide signal-cancellable context
// installed on the first Boot call. RunForever uses appStop to
// release the signal handler after the first SIGTERM/SIGINT, so a
// second Ctrl-C kills the process immediately rather than queueing
// behind the orderly drain.
var (
	appCtx   context.Context
	appStop  context.CancelFunc
	bootOnce sync.Once
)
