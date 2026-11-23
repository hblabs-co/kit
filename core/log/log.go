// Package log centralises slog setup for every HB Labs service.
// One call from main() — log.New("service-foo") — returns the canonical
// per-service logger: text handler in local, JSON handler in prod,
// level from LOG_LEVEL, and `service` attached as a base attribute so
// every line carries the originating service without being threaded
// through call sites.
//
// The package also wires the standard correlation chain. Once a logger
// is set as default via slog.SetDefault(log.New(...)), the per-request
// logger that httpx.RequestID puts into context inherits the service
// attribute; calling log.WithContext(ctx) inside a handler returns a
// logger pre-bound with request_id, device_id and organization_id.
//
// Required fields per architecture.md: ts, level, service, msg,
// request_id, device_id, organization_id. The first four are emitted
// by every slog handler we build; the last three flow in via context.
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// Logger is the canonical structured-logger type. Aliased from
// *slog.Logger so callers can write `*log.Logger` in signatures and
// receive the same value core/log builds, without importing
// log/slog. Methods (.With, .Info, .LogAttrs, .Enabled, …) come
// from slog and are called as usual on the value.
type Logger = slog.Logger

// Level is the level type used when classifying log records. Aliased
// from slog so callers across the repo can write `log.LevelInfo` and
// signatures like `func(...) log.Level` without importing log/slog
// directly — keeping core/log as the single dependency that
// touches the underlying logger library.
type Level = slog.Level

// Level constants. Re-exported so callers don't need to import
// log/slog just to name a level.
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Attr is the canonical key/value attribute pair carried by log
// records. Aliased from slog so callers can build typed-attribute
// slices for LogAttrs without importing log/slog. Pair with the
// String/Int/Duration constructors below.
type Attr = slog.Attr

// String builds an Attr with a string value. Thin re-export of
// slog.String so call sites read `log.String("path", r.URL.Path)`
// without dragging log/slog into business code.
func String(k, v string) Attr { return slog.String(k, v) }

// Int builds an Attr with an int value. Thin re-export of slog.Int.
func Int(k string, v int) Attr { return slog.Int(k, v) }

// Duration builds an Attr with a time.Duration value. Thin re-export
// of slog.Duration.
func Duration(k string, v time.Duration) Attr { return slog.Duration(k, v) }

// Any builds an Attr from a value of any type, deferring to slog's
// type inference. Re-export of slog.Any for the cases where the
// value isn't a string/int/duration (a bool, a float, a nested
// struct) and a typed constructor would be overkill.
func Any(k string, v any) Attr { return slog.Any(k, v) }

// Group builds a nested Attr whose value is a sub-group of the
// given args (alternating key/value, same grammar as the variadic
// logging calls). Re-export of slog.Group so a call site can emit
// a structured sub-document — e.g. log.Group("client", "ip", ip,
// "app_version", ver) renders as client.ip / client.app_version in
// JSON — without importing log/slog directly.
func Group(key string, args ...any) Attr { return slog.Group(key, args...) }

// New builds the canonical *slog.Logger for a service. Format and
// level are read from the environment:
//
//   - HBL_ENV: "local" defaults to text format, anything else (staging,
//     prod, ci) defaults to JSON. Loki/Promtail ingests JSON typed
//     without regex; we only ever drop to text on a developer laptop.
//   - LOG_FORMAT: explicit override ("text" or "json"). Handy when a
//     prod debug session wants text in `kubectl logs`.
//   - LOG_LEVEL: "debug" | "info" | "warn" | "error" (case-insensitive;
//     unknown → info).
//
// The returned logger has `service=<service>` as a base attribute.
// Set it as the process default so context-bound loggers (e.g. the
// one httpx.RequestID puts into ctx) inherit the service tag too:
//
//	l := log.New("service-foo")
//	slog.SetDefault(l)
func New(service string) *slog.Logger {
	return NewTo(os.Stderr, service)
}

// NewTo is the explicit-writer variant of New. Tests use it to capture
// log output; production has no reason to redirect away from stderr.
func NewTo(w io.Writer, service string) *slog.Logger {
	env := getenv(constants.EnvHBEnv, "local")
	format := getenv(constants.EnvLogFormat, defaultFormat(env))
	level := ParseLevel(getenv(constants.EnvLogLevel, "info"))
	h := NewHandlerTo(w, format, &slog.HandlerOptions{Level: level})
	return slog.New(h).With(slog.String("service", service))
}

// getenv reads key, falls back to def when unset or whitespace.
// Mirrored from envcfg.String to keep core/log free of a
// dependency on envcfg — and so envcfg itself can import this
// package for its own warnings without creating a cycle.
func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// NewHandler returns a slog handler ready to feed slog.SetDefault.
// Most services should call New / NewTo instead — this exists for
// callers that need the handler standalone (custom slog wrappers,
// io.MultiWriter fan-out, fixtures).
//
// format selects the wire format:
//
//   - "json"           → slog.JSONHandler, one JSON object per line.
//     Loki/Promtail/Vector ingest each field typed without parsing.
//   - "text" (default) → slog.TextHandler, key=value, terminal-friendly.
//
// Anything else (including empty string) falls back to "text" so a
// missing env var never crashes the boot. opts is forwarded as-is to
// whichever handler is built; pass it to set Level, AddSource, etc.
//
// Output goes to os.Stderr by default; use NewHandlerTo to redirect.
// Container runtimes expect logs on stderr — staying on the default
// is almost always the right call.
func NewHandler(format string, opts *slog.HandlerOptions) slog.Handler {
	return NewHandlerTo(os.Stderr, format, opts)
}

// NewHandlerTo is the explicit-writer form of NewHandler.
//
// Always lowercases the level value (`info` instead of `INFO`) so log
// lines blend into the rest of the key=value text without shouting;
// if the caller passes its own ReplaceAttr we chain after the
// lowercase step so user transforms still apply.
func NewHandlerTo(w io.Writer, format string, opts *slog.HandlerOptions) slog.Handler {
	o := slog.HandlerOptions{}
	if opts != nil {
		o = *opts
	}
	userReplace := o.ReplaceAttr
	o.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			if lvl, ok := a.Value.Any().(slog.Level); ok {
				a.Value = slog.StringValue(strings.ToLower(lvl.String()))
			}
		}
		if userReplace != nil {
			return userReplace(groups, a)
		}
		return a
	}
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.NewJSONHandler(w, &o)
	}
	return slog.NewTextHandler(w, &o)
}

// Default returns the canonical service-level *slog.Logger. After
// the bootstrap call slog.SetDefault(log.New("svc")) it carries the
// service tag and the configured handler. Exposed here so packages
// across the repo can fall back to "the configured app logger"
// without importing log/slog directly — keeping core/log as the
// single import that touches the underlying logger library.
func Default() *slog.Logger {
	return slog.Default()
}

// SetDefault makes l the process-wide default *slog.Logger that
// Default() (and slog.Default()) return. Thin wrapper so service
// bootstraps install the canonical logger via core/log without
// importing log/slog just for this one call. Typical use:
//
//	l := log.New("service-foo")
//	log.SetDefault(l)
func SetDefault(l *slog.Logger) {
	slog.SetDefault(l)
}

// ParseLevel converts the strings "debug", "info", "warn", "error"
// (case-insensitive) into the matching slog.Level. Unknown values
// fall back to Info. Useful for `LOG_LEVEL=debug`-style env wiring.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithContext returns a logger pre-bound with the correlation IDs
// stored in ctx by upstream middleware: request_id and device_id
// (httpx.RequestID), and organization_id (log.WithOrganizationID,
// typically called by auth after the request's org is resolved).
//
// Resolution:
//
//   - Base = the *slog.Logger httpx.RequestID attached to ctx, falling
//     back to slog.Default() — which is the service-tagged logger from
//     New if slog.SetDefault was called at boot.
//   - organization_id is appended only when set, so platform-level
//     traffic (no org) doesn't carry an empty field.
//
// Use it at the top of any handler / NATS consumer / cron job that
// wants its log lines correlated with the request that triggered them:
//
//	l := log.WithContext(ctx)
//	l.Info("processing", "items", n)
func WithContext(ctx context.Context) *slog.Logger {
	base := LoggerFromContext(ctx)
	if orgID := OrganizationIDFromContext(ctx); orgID != "" {
		base = base.With(slog.String("organization_id", orgID))
	}
	return base
}

// ctxKey is the unexported type used for the context keys defined in
// this package, so other packages can't collide with our keys (the
// well-known footgun of using untyped strings as context keys).
type ctxKey int

const (
	ctxKeyOrganizationID ctxKey = iota + 1
	// ctxKeyLogger stores a *slog.Logger pre-bound with correlation
	// fields (request_id, device_id, …) so handlers don't have to
	// re-attach them on every log call. The httpx.RequestID middleware
	// is the canonical writer; tests and tools can use WithLogger
	// directly when constructing a context outside the HTTP chain.
	ctxKeyLogger
)

// WithLogger attaches l to ctx for retrieval by LoggerFromContext.
// Lives in this package (not httpx) so core/log owns every
// logger-related context helper — and so httpx can import log
// without creating a cycle.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// LoggerFromContext returns the logger attached by WithLogger,
// falling back to slog.Default() so callers can write
// LoggerFromContext(ctx).Info(...) without a nil check even when
// running outside an HTTP request (cron jobs, NATS consumers, tests).
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if v, _ := ctx.Value(ctxKeyLogger).(*slog.Logger); v != nil {
		return v
	}
	return slog.Default()
}

// WithOrganizationID attaches orgID to ctx so subsequent WithContext
// loggers carry organization_id automatically. Auth middleware calls
// this once it resolves the session's org; the value then flows
// through the rest of the request (and any goroutines derived from
// it) without being threaded as a function argument.
//
// Empty orgID returns ctx unchanged so callers can pipe potentially-
// missing values through without a nil check.
func WithOrganizationID(ctx context.Context, orgID string) context.Context {
	if orgID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyOrganizationID, orgID)
}

// OrganizationIDFromContext returns the organization-id attached to
// ctx, or "" if none is set. "" rather than panicking so background
// workers without an org context (cron jobs, platform-level NATS
// consumers) don't crash when reusing handler code paths.
func OrganizationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrganizationID).(string)
	return v
}

// defaultFormat picks the wire format slog uses when LOG_FORMAT is
// unset. Local dev gets human-readable text; everything else (prod,
// staging, ci) emits JSON so Loki / Promtail can index without a
// parser.
func defaultFormat(env string) string {
	if env == "local" {
		return "text"
	}
	return "json"
}

// Package-level convenience wrappers. They delegate to slog.Default(),
// which after slog.SetDefault(log.New(...)) at boot is the canonical
// service-tagged logger. The point of having them here (rather than
// calling slog directly) is that business code only ever imports
// "github.com/hblabs-co/kit/core/log" — if we ever change the underlying
// logging library, this file is the only place that touches it.
//
// For ctx-aware variants (handlers want their slog.Handler to see
// the request context — e.g. for trace IDs picked up from baggage),
// use the *Context forms. For loggers pre-bound with correlation
// IDs from middleware, use WithContext(ctx) and call methods on the
// returned *slog.Logger.

// Debug logs at debug level against slog.Default().
func Debug(msg string, args ...any) {
	slog.Default().Debug(msg, args...)
}

// DebugContext is the ctx-aware form of Debug.
func DebugContext(ctx context.Context, msg string, args ...any) {
	slog.Default().DebugContext(ctx, msg, args...)
}

// Info logs at info level against slog.Default().
func Info(msg string, args ...any) {
	slog.Default().Info(msg, args...)
}

// InfoContext is the ctx-aware form of Info.
func InfoContext(ctx context.Context, msg string, args ...any) {
	slog.Default().InfoContext(ctx, msg, args...)
}

// Warn logs at warn level against slog.Default().
func Warn(msg string, args ...any) {
	slog.Default().Warn(msg, args...)
}

// WarnContext is the ctx-aware form of Warn.
func WarnContext(ctx context.Context, msg string, args ...any) {
	slog.Default().WarnContext(ctx, msg, args...)
}

// Error logs at error level against slog.Default().
func Error(msg string, args ...any) {
	slog.Default().Error(msg, args...)
}

// ErrorContext is the ctx-aware form of Error.
func ErrorContext(ctx context.Context, msg string, args ...any) {
	slog.Default().ErrorContext(ctx, msg, args...)
}
