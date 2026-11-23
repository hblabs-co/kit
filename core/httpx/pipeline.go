package httpx

import (
	"net/http"

	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/secheaders"
)

// NewHandler starts a fluent middleware composition for HB Labs
// service stack. Add layers by chaining methods, then bind the
// inner handler with Merge:
//
//	handler := httpx.NewHandler().
//	    Secure().
//	    RequestID().
//	    WithoutLogging(cfg.QuietPaths...).
//	    WithRecover().
//	    Merge(siteHandler)
//
// Order: the FIRST method called wraps the rest. The example above
// produces a handler where Secure sees the request first, the
// inner siteHandler runs, and Recover catches any panic on the way
// out (converted to a 500). Layers compose top-to-bottom in the
// same order they appear in the call chain — the visual flow
// matches the request flow.
//
// For arbitrary middleware outside the canonical set (rate limiter,
// OTel tracer, custom auth wrap), reach for Use(mw); for full
// AccessLog control beyond the WithoutLogging shorthand, use
// AccessLog(opts).
func NewHandler() *handlerBuilder {
	return &handlerBuilder{}
}

// handlerBuilder is the in-progress composition returned by
// NewHandler. Unexported because callers go
// NewHandler().X().Y().Merge(h) and never need to spell the type.
type handlerBuilder struct {
	steps []Middleware
}

// Secure appends the secheaders middleware so policy headers
// (CSP, HSTS, X-Frame-Options, …) land on every response —
// including the 500 produced by WithRecover. Pass options to tune
// the policy; zero arg = secheaders defaults, which is what every
// public-facing service wants in production.
func (b *handlerBuilder) Secure(opts ...secheaders.Options) *handlerBuilder {
	var o secheaders.Options
	if len(opts) > 0 {
		o = opts[0]
	}
	b.steps = append(b.steps, secheaders.Middleware(o))
	return b
}

// RequestID appends the request-id / device-id middleware so every
// downstream layer (AccessLog, the inner handler) sees a context
// pre-populated with correlation IDs. Pass a base logger to derive
// per-request loggers from; zero arg = log.Default(), which is the
// canonical service-tagged logger after Boot installs it.
func (b *handlerBuilder) RequestID(base ...*log.Logger) *handlerBuilder {
	var lg *log.Logger
	if len(base) > 0 {
		lg = base[0]
	}
	b.steps = append(b.steps, RequestID(lg))
	return b
}

// AccessLog appends the access-log middleware with full options
// control. Use this when WithoutLogging's shorthand isn't enough
// — custom Level fn, TrustedProxies, non-default ContextAttrs.
// For the platform-standard recipe (quiet paths + ContextLogAttrs)
// reach for WithoutLogging instead.
func (b *handlerBuilder) AccessLog(opts AccessLogOptions) *handlerBuilder {
	b.steps = append(b.steps, AccessLog(opts))
	return b
}

// WithoutLogging adds the access-log middleware AND configures it
// to skip (demote to debug) the supplied URL patterns. Reads as
// "log every request, EXCEPT these paths" — the framework's
// canonical recipe wired in one call. Pair with cfg.QuietPaths
// from web/render/config which already merges in the
// framework's noise list (healthz, /static/*, dev hot-reload).
//
// Empty arg = log every request at Info. Use AccessLog(opts) when
// you need more than the QuietPaths/ContextLogAttrs combination.
func (b *handlerBuilder) WithoutLogging(quietPaths ...string) *handlerBuilder {
	// ContextAttrs is left nil on purpose — AccessLog defaults it to
	// ContextLogAttrs (request-id / device-id), so the canonical
	// recipe gets the correlation fields without spelling them out.
	return b.AccessLog(AccessLogOptions{
		Level: QuietPaths(quietPaths...),
	})
}

// WithRecover appends the panic-recovery middleware. Place it last
// (innermost) so a handler panic is converted to a normal 500
// before AccessLog sees the response — that way the access-log
// entry still fires for the requests we care about most.
func (b *handlerBuilder) WithRecover() *handlerBuilder {
	b.steps = append(b.steps, Recover)
	return b
}

// Use appends an arbitrary middleware. Escape hatch for steps
// outside the canonical set (rate limiter, OTel tracer, custom
// auth wrap). Order rules are the same as the named methods —
// first listed wraps the rest.
func (b *handlerBuilder) Use(mw Middleware) *handlerBuilder {
	b.steps = append(b.steps, mw)
	return b
}

// Merge binds h as the innermost handler and returns the composed
// http.Handler. Applies the chain in reverse so the FIRST method
// called on the builder becomes the OUTERMOST wrap, matching the
// "first listed sees the request first" semantics every method's
// docstring describes.
func (b *handlerBuilder) Merge(h http.Handler) http.Handler {
	for i := len(b.steps) - 1; i >= 0; i-- {
		h = b.steps[i](h)
	}
	return h
}
