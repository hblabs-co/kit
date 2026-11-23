// Package secheaders is the security-headers middleware shared by
// every HB Labs HTTP service. It sets the standard hardening
// headers before the wrapped handler runs, so the response carries
// them regardless of which downstream handler produced it (template,
// JSON API, file server, error page).
//
// All headers come with sane defaults tuned for a public site behind
// Cloudflare. Each can be overridden by setting the matching field on
// Options, or disabled by setting it to the empty string (useful when
// Cloudflare is already injecting it at the edge — defence in depth
// is good, duplicate headers are not).
//
// Wire it as the OUTERMOST middleware so security headers land on
// every response, including 404s, 5xxs, and short-circuited errors:
//
//	return secheaders.Middleware(secheaders.Options{})(
//	    httplog.Middleware(httplog.Options{...})(mux),
//	)
//
// CSP is the most opinionated default and the most likely to need
// per-app tuning. The default allows inline `<style>` and `<script>`
// blocks (the landing/launch sites have both) but blocks framing,
// plugins, and base-uri hijacking. While iterating on CSP in
// production set Options.ReportOnly = true to switch to
// Content-Security-Policy-Report-Only — same headers, no enforcement,
// browser logs violations to the console (or to a report-uri if you
// wire one).
package secheaders

import (
	"net/http"
	"strings"

	"github.com/hblabs-co/kit/core/constants"
)

// Defaults — exported so callers can compose ("base default + my
// extra source") instead of hand-writing the whole policy. Empty
// string in any field disables that header in the middleware.
const (
	DefaultHSTS               = "max-age=63072000; includeSubDomains; preload" // 2 years
	DefaultContentTypeOptions = "nosniff"
	DefaultFrameOptions       = "DENY"
	DefaultReferrerPolicy     = "strict-origin-when-cross-origin"
	DefaultPermissionsPolicy  = "geolocation=(), microphone=(), camera=(), payment=(), usb=()"

	// DefaultCSP allows inline style/script (the landing has both,
	// generated server-side from templates) and same-origin fetch /
	// form / image / font / media. Blocks framing, plugins, and
	// <base> redirection. data: is allowed for images so inline SVG
	// icons work without uploads. https: on img-src lets template
	// authors reference CDN images without per-host allow-listing.
	//
	// fonts.googleapis.com (style) + fonts.gstatic.com (font files)
	// are explicitly allow-listed because web/uikit's tokens.css
	// loads Inter + JetBrains Mono via @import from Google Fonts —
	// every service that mounts uikit needs them. If a service
	// switches to self-hosted fonts, pass an override CSP via
	// Options.ContentSecurityPolicy.
	DefaultCSP = "default-src 'self'; " +
		"img-src 'self' data: https:; " +
		"font-src 'self' https://fonts.gstatic.com; " +
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
		"script-src 'self' 'unsafe-inline'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"object-src 'none'"
)

// Options configures Middleware. The zero value applies every
// Default* constant — that's the recommended baseline for any
// public-facing service. Override per-field to tune.
type Options struct {
	HSTS                  string
	ContentTypeOptions    string
	FrameOptions          string
	ReferrerPolicy        string
	PermissionsPolicy     string
	ContentSecurityPolicy string

	// Disable* takes precedence over the corresponding string field
	// — set true to skip the header even if the string is non-empty.
	// Useful when Cloudflare injects the header at the edge: set the
	// local copy to empty AND DisableXxx=true so a future code change
	// can't accidentally undo the policy decision.
	DisableHSTS                  bool
	DisableContentTypeOptions    bool
	DisableFrameOptions          bool
	DisableReferrerPolicy        bool
	DisablePermissionsPolicy     bool
	DisableContentSecurityPolicy bool

	// ReportOnly switches CSP to Content-Security-Policy-Report-Only,
	// so the browser logs violations without blocking content. Use
	// during a CSP rollout to discover what breaks before flipping
	// to enforcement.
	ReportOnly bool

	// AdditionalImgSrc are extra origins spliced into the `img-src`
	// directive of the resolved CSP. Use case: in dev the asset host
	// is `http://localhost:9000` (MinIO), which the default CSP
	// blocks because plain `http:` isn't in the source list. Adding
	// the origin here keeps the rest of the policy intact without
	// forcing the service to hand-roll a full CSP override.
	//
	// Each entry should be a CSP source expression — typically the
	// scheme + host (e.g. "http://localhost:9000") or a single
	// "https:" wildcard if needed.
	AdditionalImgSrc []string

	// AdditionalFrameSrc are extra origins spliced into a `frame-src`
	// directive. The default CSP has no explicit frame-src, so the
	// browser falls back to default-src ('self') — too strict for
	// services that embed third-party media (Spotify, YouTube,
	// Vimeo, payment widgets). When non-empty, a `frame-src 'self'
	// <extras>` directive is emitted; default-src stays untouched.
	//
	// Same source-expression grammar as AdditionalImgSrc.
	AdditionalFrameSrc []string
}

// Middleware returns a function that wraps an http.Handler with the
// configured security headers. Headers are set BEFORE the downstream
// handler runs, so a handler that explicitly Set()s the same header
// overrides our default — intentional, lets endpoints opt out per-
// request when there's a real reason.
func Middleware(opts Options) func(http.Handler) http.Handler {
	hsts := pick(opts.HSTS, DefaultHSTS, opts.DisableHSTS)
	cto := pick(opts.ContentTypeOptions, DefaultContentTypeOptions, opts.DisableContentTypeOptions)
	frame := pick(opts.FrameOptions, DefaultFrameOptions, opts.DisableFrameOptions)
	ref := pick(opts.ReferrerPolicy, DefaultReferrerPolicy, opts.DisableReferrerPolicy)
	perms := pick(opts.PermissionsPolicy, DefaultPermissionsPolicy, opts.DisablePermissionsPolicy)
	csp := pick(opts.ContentSecurityPolicy, DefaultCSP, opts.DisableContentSecurityPolicy)
	if csp != "" && len(opts.AdditionalImgSrc) > 0 {
		csp = appendImgSrc(csp, opts.AdditionalImgSrc)
	}
	if csp != "" && len(opts.AdditionalFrameSrc) > 0 {
		csp = appendFrameSrc(csp, opts.AdditionalFrameSrc)
	}
	cspHeader := constants.HeaderContentSecurityPolicy
	if opts.ReportOnly {
		cspHeader = constants.HeaderContentSecurityPolicyReportOnly
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			if hsts != "" {
				h.Set(constants.HeaderStrictTransportSecurity, hsts)
			}
			if cto != "" {
				h.Set(constants.HeaderXContentTypeOptions, cto)
			}
			if frame != "" {
				h.Set(constants.HeaderXFrameOptions, frame)
			}
			if ref != "" {
				h.Set(constants.HeaderReferrerPolicy, ref)
			}
			if perms != "" {
				h.Set(constants.HeaderPermissionsPolicy, perms)
			}
			if csp != "" {
				h.Set(cspHeader, csp)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pick returns "" if disabled is true, otherwise the explicit value
// when non-empty, otherwise the default. Centralises the "explicit-
// empty == use default" decision so each field uses the same
// semantics.
func pick(explicit, def string, disabled bool) string {
	if disabled {
		return ""
	}
	if explicit != "" {
		return explicit
	}
	return def
}

// appendImgSrc inserts hosts into the `img-src` directive of csp.
// Preserves the rest of the policy verbatim — directive order, the
// original source list, the terminating semicolons — so the result
// is still a syntactically valid CSP. Pulled out so the splicing
// rule is unit-testable on its own (the middleware test only checks
// the integration).
//
// If csp has no `img-src` directive, prepends one with `'self'` plus
// the extras — matches the default policy's intent (img-src is
// always tighter than default-src 'self').
func appendImgSrc(csp string, hosts []string) string {
	extra := strings.Join(hosts, " ")
	parts := strings.Split(csp, ";")
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "img-src" || strings.HasPrefix(trimmed, "img-src ") {
			parts[i] = strings.TrimRight(p, " \t") + " " + extra
			return strings.Join(parts, ";")
		}
	}
	return "img-src 'self' " + extra + "; " + csp
}

// appendFrameSrc mirrors appendImgSrc for the `frame-src` directive.
// The default policy ships no frame-src, so embedded iframes inherit
// default-src ('self') — blocking every cross-origin iframe. When a
// service opts in via Options.AdditionalFrameSrc, this function
// either extends an existing frame-src list or prepends a fresh
// directive seeded with 'self'.
func appendFrameSrc(csp string, hosts []string) string {
	extra := strings.Join(hosts, " ")
	parts := strings.Split(csp, ";")
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "frame-src" || strings.HasPrefix(trimmed, "frame-src ") {
			parts[i] = strings.TrimRight(p, " \t") + " " + extra
			return strings.Join(parts, ";")
		}
	}
	return "frame-src 'self' " + extra + "; " + csp
}
