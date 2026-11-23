// Package httpx is the HTTP middleware bundle shared by every HB Labs
// service. Provides the standard outermost-middleware chain
// (request-id propagation, panic recovery, context-bound logger) that
// every service is expected to mount.
//
// Wire order from outermost to innermost:
//
//	secheaders → httpx.RequestID → httplog → httpx.Recover → handler
//
// secheaders sets policy headers on EVERY response (including the
// 500 from Recover). RequestID resolves request-id and device-id
// before any logging middleware runs so log lines carry them.
// httplog records exactly one access entry per request — placed
// outside Recover so the entry still fires for panicked requests
// (Recover converts the panic into a normal 500 before httplog sees
// the response). Recover is innermost so any handler panic is caught
// inside the chain and never tears down the goroutine.
package httpx

import (
	"context"

	"github.com/hblabs-co/kit/core/log"
)

// ctxKey is the unexported type all this package's context keys use.
// Unexported so that no other package can collide with our keys, and
// untyped strings stay out of context.Context keys (which avoid the
// well-known "string keys can collide across packages" footgun).
type ctxKey int

const (
	// ctxKeyRequestID stores the resolved X-Request-Id value.
	ctxKeyRequestID ctxKey = iota + 1
	// ctxKeyDeviceID stores the resolved X-Device-Id value.
	ctxKeyDeviceID
)

// Naming convention: the public middleware constructors at the top
// of the package (RequestID, Recover) are the bare verbs. Context
// helpers carry a `FromContext` suffix to avoid colliding with those
// constructors and to follow the stdlib pattern (e.g. otel's
// SpanFromContext). Setters use `With*` to read like
// `ctx = WithRequestID(ctx, id)`.

// WithRequestID attaches id to ctx. Normally only the RequestID
// middleware calls this; tests and tools can call it directly when
// constructing a context without going through HTTP.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFromContext returns the request-id stored in ctx by the
// RequestID middleware, or "" if none is present. "" rather than
// panicking so background workers (cron jobs, NATS consumers) that
// lack a request-id don't crash when reusing handler code paths.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// WithDeviceID attaches id to ctx.
func WithDeviceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyDeviceID, id)
}

// DeviceIDFromContext returns the device-id stored in ctx, or "" if
// none is present. Callers that need a value either way should
// prefer "-" over "" for log lines — see ContextLogAttrs.
func DeviceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyDeviceID).(string)
	return v
}

// ContextLogAttrs returns the log Attrs derived from values the
// RequestID middleware put into ctx. Use as the value for
// AccessLogOptions.ContextAttrs so access log lines carry the same
// correlation fields as the per-request logger:
//
//	httpx.AccessLog(httpx.AccessLogOptions{
//	    ContextAttrs: httpx.ContextLogAttrs,
//	})
//
// Empty slice (not nil) when nothing is attached, so the caller can
// safely append to it without nil checks.
//
// Note: WithLogger / LoggerFromContext live in core/log, not here,
// so that package owns every logger-related helper. httpx imports
// core/log to attach the per-request logger to ctx.
func ContextLogAttrs(ctx context.Context) []log.Attr {
	attrs := make([]log.Attr, 0, 2)
	if rid := RequestIDFromContext(ctx); rid != "" {
		attrs = append(attrs, log.String("request_id", rid))
	}
	if did := DeviceIDFromContext(ctx); did != "" {
		attrs = append(attrs, log.String("device_id", did))
	}
	return attrs
}
