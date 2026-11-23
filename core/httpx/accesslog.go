package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/clientmeta"
	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// AccessLog is the access-log middleware shared by every HTTP
// service. It emits one structured slog record per request with the
// data we actually look at when something goes wrong behind
// Cloudflare:
//
//	level=info msg=http method=GET path=/de/ status=200 duration=1.4ms
//	    ip=… cf_country=… cf_ray=… cf_ip=… xri=… xff=… ua="…"
//
// `ip` is the resolved client IP (see ClientIP). The raw
// CF-Connecting-IP / X-Real-IP / X-Forwarded-For values stay
// alongside so we can spot header spoofing or a misconfigured proxy
// — if cf_ip and ip disagree, something is wrong upstream. Missing
// headers print as `-` so a `grep cf_country=` always matches.
//
// Each service decides per-route verbosity with AccessLogOptions.Level:
// noisy paths (kubelet probes, embedded asset fetches) drop to Debug
// so they only surface when LOG_LEVEL=debug is set; the rest stays at
// Info. This keeps production logs clean without losing the ability
// to inspect probes when troubleshooting.
//
//	return httpx.AccessLog(httpx.AccessLogOptions{
//	    Level: httpx.QuietPaths("/healthz", "/static/*", "/favicon.*"),
//	})(mux)
//
// The middleware writes to log.Default() (slog.Default() under the
// hood), which after the standard log.New + slog.SetDefault wiring
// at boot is the service-tagged logger. No Logger override field by
// design — services that want access logs in a separate sink can
// wrap or tee at the slog handler layer.
func AccessLog(opts AccessLogOptions) func(http.Handler) http.Handler {
	cfg := accessLogConfig{
		levelFn:      opts.Level,
		trusted:      opts.TrustedProxies,
		contextAttrs: opts.ContextAttrs,
		extraAttrs:   opts.ExtraAttrs,
		clientMeta:   opts.ClientMeta,
		query:        opts.Query,
	}
	if cfg.levelFn == nil {
		cfg.levelFn = defaultLevel
	}
	// Default to the standard correlation attrs (request_id /
	// device_id from context). They're platform-standard — every
	// service behind RequestID wants them — so a nil ContextAttrs
	// surfaces them automatically rather than forcing each caller
	// to wire ContextLogAttrs by hand. ContextLogAttrs is a no-op
	// when the context carries no IDs, so this is safe even when
	// RequestID didn't run upstream. To suppress them, pass a func
	// that returns nil.
	if cfg.contextAttrs == nil {
		cfg.contextAttrs = ContextLogAttrs
	}
	return cfg.wrap
}

// defaultLevel is the fallback Level fn used when AccessLogOptions
// leaves the field nil. Hard-coded to LevelInfo — every request
// logs at Info, the global logger's threshold decides what reaches
// stderr.
func defaultLevel(*http.Request) log.Level { return log.LevelInfo }

// accessLogConfig is the resolved per-AccessLog state captured once
// and re-used across every wrapped handler. Pulled out of a closure
// so the middleware reads top-down without anonymous-fn nesting.
type accessLogConfig struct {
	levelFn      func(*http.Request) log.Level
	trusted      *CIDRSet
	contextAttrs func(ctx context.Context) []log.Attr
	extraAttrs   func(r *http.Request) []log.Attr
	clientMeta   bool
	query        bool
}

// wrap returns the http.Handler the middleware composes around next.
// Method value, not a closure literal — the binding lives on the
// returned http.Handler value (accessLogHandler), keeping the
// AccessLog body straight-line.
func (c accessLogConfig) wrap(next http.Handler) http.Handler {
	return accessLogHandler{cfg: c, next: next}
}

// accessLogHandler is the per-wrap http.Handler implementation.
// Holds the resolved config + the wrapped handler so ServeHTTP
// reads the same data every request without going through a
// closure environment.
type accessLogHandler struct {
	cfg  accessLogConfig
	next http.Handler
}

func (h accessLogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.next.ServeHTTP(rw, r)
	logger := log.Default()
	level := h.cfg.levelFn(r)
	// Skip the attribute build entirely when the level is below the
	// logger's threshold — saves header lookups and time math on
	// every silent request.
	if !logger.Enabled(r.Context(), level) {
		return
	}
	ua := r.UserAgent()
	ip := resolveIP(r, h.cfg.trusted)
	attrs := []log.Attr{
		log.Int("status", rw.status),
		log.String("method", r.Method),
		log.String("path", r.URL.Path),
		log.Duration("duration", time.Since(start)),

		log.String("ip", ip),

		log.String("cf_ip", headerOrDash(r, constants.HeaderCFConnectingIP)),
		log.String("cf_ray", headerOrDash(r, constants.HeaderCFRay)),
		log.String("cf_ipcontinent", headerOrDash(r, constants.HeaderCFIPContinent)),
		log.String("cf_country", headerOrDash(r, constants.HeaderCFIPCountry)),
		log.String("cf_ipcity", headerOrDash(r, constants.HeaderCFIPCity)),
		log.String("cf_iplatitude", headerOrDash(r, constants.HeaderCFIPLatitude)),
		log.String("cf_iplongitude", headerOrDash(r, constants.HeaderCFIPLongitude)),

		// cf_verified_bot is `true` when Cloudflare confirmed
		// the request via reverse-DNS against the bot's
		// official ranges (Googlebot, Bingbot, Applebot,
		// FacebookExternalHit, …). Requires the Verified Bot
		// feature to be enabled in the Cloudflare dashboard.
		// Use it to tell a real Googlebot from a scraper that
		// just put "Googlebot" in its UA string.
		log.String("cf_verified_bot", headerOrDash(r, constants.HeaderCFVerifiedBot)),

		// bot is a short identifier derived from the UA so we
		// can group / count crawlers without parsing the full
		// string downstream. `-` for browsers, curl, or any
		// UA we don't recognise.
		log.String("bot", DetectBot(ua)),
		log.String("ua", ua),

		log.String("xri", headerOrDash(r, constants.HeaderXRealIP)),
		log.String("xff", headerOrDash(r, constants.HeaderXForwardedFor)),
	}

	if h.cfg.contextAttrs != nil {
		attrs = append(attrs, h.cfg.contextAttrs(r.Context())...)
	}
	if h.cfg.extraAttrs != nil {
		attrs = append(attrs, h.cfg.extraAttrs(r)...)
	}
	if h.cfg.clientMeta {
		if g, ok := clientMetaGroup(r, ip); ok {
			attrs = append(attrs, g)
		}
	}
	// query is opt-in (AccessLogOptions.Query) because a raw query
	// string can carry secrets on some endpoints (tokens in the
	// URL) — services enable it only where the params are safe to
	// log. Skipped when empty so paths without a query stay
	// compact.
	if h.cfg.query {
		if q := r.URL.RawQuery; q != "" {
			attrs = append(attrs, log.String("query", q))
		}
	}

	logger.LogAttrs(r.Context(), level, "http", attrs...)
}

// clientMetaGroup builds the nested `client` group from a full
// clientmeta capture. It reuses the access log's already-resolved
// client IP (the same value the top-level `ip` field uses, so it
// honours TrustedProxies) instead of re-resolving — keeping
// client.ip and the top-level ip consistent on every line.
//
// Returns ok=false when the capture has no non-empty fields
// (clientmeta.LogFields drops empties), so the caller skips
// appending an empty group. Nesting under `client.*` keeps the
// keys distinct from the top-level request fields, so emitting
// both views produces no duplicate JSON keys.
func clientMetaGroup(r *http.Request, ip string) (log.Attr, bool) {
	fields := clientmeta.FromRequest(r, ip).LogFields()
	if len(fields) == 0 {
		return log.Attr{}, false
	}
	kv := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		kv = append(kv, k, v)
	}
	return log.Group("client", kv...), true
}

// resolveIP picks the IP attribute value: the legacy permissive
// ClientIP when no trusted CIDR set is configured, ClientIPTrusted
// otherwise. Pulled out so the middleware stays a sequence of
// straight-line statements.
func resolveIP(r *http.Request, trusted *CIDRSet) string {
	if trusted == nil {
		return ClientIP(r) // legacy permissive
	}
	return ClientIPTrusted(r, trusted)
}

// AccessLogOptions configures AccessLog. Zero value = log every
// request at Info, with proxy headers trusted blindly. Set
// TrustedProxies in any production deployment behind a proxy.
type AccessLogOptions struct {
	// Level returns the log level for a request. nil = Info for all.
	// Use this to demote noisy probes to Debug; the global slog
	// logger's level threshold then decides what gets emitted.
	Level func(r *http.Request) log.Level

	// TrustedProxies is the dynamic CIDR set that may legitimately
	// set CF-Connecting-IP / X-Real-IP / X-Forwarded-For. When
	// non-nil, the access log's `ip` field falls back to RemoteAddr
	// for any request whose TCP peer is NOT in the current snapshot
	// — so an attacker who finds the origin can't spoof their IP in
	// our logs or in any downstream component (rate limit, audit)
	// keyed off ip.
	//
	// Pass an httpx.NewCloudflareCIDRSet() that has been Refresh()ed
	// at boot and is being kept up to date by a periodic ticker.
	// nil = trust every proxy header blindly (legacy behaviour, only
	// fine when the origin is firewalled to the proxy at the network
	// layer).
	TrustedProxies *CIDRSet

	// ContextAttrs returns Attrs appended to each access log line,
	// derived from the request context. nil defaults to
	// httpx.ContextLogAttrs (the request-id / device-id the
	// RequestID middleware puts into context) — the canonical
	// correlation fields every service wants, surfaced without
	// per-call wiring. Pass an explicit func to add MORE (compose
	// ContextLogAttrs with your own), or a func returning nil to
	// suppress the defaults entirely.
	ContextAttrs func(ctx context.Context) []log.Attr

	// ExtraAttrs returns Attrs derived from the REQUEST itself
	// (headers, method, path) rather than from context. Use it for
	// genuinely service-specific fields the standard set doesn't
	// cover. For the platform-standard caller fingerprint, prefer
	// the ClientMeta toggle below — it's the same data every
	// service would otherwise re-wire here. Runs after
	// ContextAttrs, so its keys win on a collision. nil = none.
	ExtraAttrs func(r *http.Request) []log.Attr

	// ClientMeta, when true, appends the full core/clientmeta
	// fingerprint as a nested `client` group (client.ip,
	// client.user_agent, client.accept_language, client.app_version,
	// client.sec_ch_ua*, the raw proxy chain, the Cloudflare geo
	// headers). These are platform-standard request headers, so a
	// service that wants the caller fingerprint flips this bool
	// instead of re-wiring an ExtraAttrs hook with the same
	// clientmeta plumbing.
	//
	// The group reuses the same resolved IP as the top-level `ip`
	// field (honouring TrustedProxies), and clientmeta drops empty
	// headers so a native-app call stays compact. Nesting under
	// `client.*` keeps the keys distinct from the top-level request
	// fields — no duplicate JSON keys.
	ClientMeta bool

	// Query, when true, appends the raw URL query string as a
	// `query` field (e.g. query="q=trainer&sort=bytes_desc&page=2").
	// Empty query strings are skipped so plain paths stay compact.
	//
	// OPT-IN on purpose: a query string can carry secrets on some
	// endpoints (auth tokens, signed URLs in the URL itself), and
	// access logs ship to long-lived sinks (Loki). Enable it only
	// where the parameters are safe to log — filter/sort/pagination
	// params on an internal API, not token-bearing public routes.
	// If a service needs query logging on a route that MIGHT carry
	// a secret, redact at the handler instead of flipping this on.
	Query bool
}

// ClientIP returns the best-guess origin IP using the preference
// chain CF-Connecting-IP → X-Real-IP → X-Forwarded-For[0] →
// RemoteAddr. Behind Cloudflare the only fully trustworthy value is
// CF-Connecting-IP (Cloudflare strips and re-sets it on every
// request); the others are kept as fallbacks for non-Cloudflare
// deployments. For X-Forwarded-For we take the first comma-separated
// value (the original client; the rest are intermediate proxies).
//
// Exported because rate limiting, audit logs and geo lookups all need
// the same answer — keeping the logic here means every service agrees
// on what "the client IP" means.
func ClientIP(r *http.Request) string {
	if v := r.Header.Get(constants.HeaderCFConnectingIP); v != "" {
		return v
	}
	if v := r.Header.Get(constants.HeaderXRealIP); v != "" {
		return v
	}
	if v := r.Header.Get(constants.HeaderXForwardedFor); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// QuietPaths returns an AccessLogOptions.Level function that demotes
// the listed paths to Debug and leaves everything else at Info.
// Patterns follow a tiny convention so a single env var can express
// both shapes:
//
//   - trailing "*" → prefix match (e.g. "/static/*", "/favicon.*")
//   - anything else → exact match (e.g. "/healthz")
//
// Empty / whitespace-only patterns are ignored, so splitting a CSV
// env var on "," and passing the result is safe even if the var is
// unset.
//
//	patterns := strings.Split(envcfg.String("LOG_QUIET_PATHS",
//	    "/healthz,/static/*,/favicon.*"), ",")
//	httpx.AccessLog(httpx.AccessLogOptions{Level: httpx.QuietPaths(patterns...)})
func QuietPaths(patterns ...string) func(r *http.Request) log.Level {
	m := &quietPathMatcher{exact: map[string]struct{}{}}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if pref, ok := strings.CutSuffix(p, "*"); ok {
			m.prefixes = append(m.prefixes, pref)
		} else {
			m.exact[p] = struct{}{}
		}
	}
	return m.level
}

// quietPathMatcher resolves a request to a Level by matching the URL
// path against the exact-match set first, then the prefix list.
// Constructed once by QuietPaths and queried per-request.
type quietPathMatcher struct {
	exact    map[string]struct{}
	prefixes []string
}

// level returns LevelDebug for any URL path that matches an exact
// entry or a prefix entry, LevelInfo otherwise. Method value (not
// closure literal) so QuietPaths can return m.level without an
// anonymous-fn wrapper.
func (m *quietPathMatcher) level(r *http.Request) log.Level {
	p := r.URL.Path
	if _, ok := m.exact[p]; ok {
		return log.LevelDebug
	}
	for _, pref := range m.prefixes {
		if strings.HasPrefix(p, pref) {
			return log.LevelDebug
		}
	}
	return log.LevelInfo
}

// DetectBot returns a short identifier for the crawler / bot behind a
// given User-Agent string. Match is by case-insensitive substring;
// the table is ordered so more specific identifiers win over the
// generic family (e.g. ChatGPT-User vs GPTBot are kept separate).
// Unknown / browser / empty UAs return "-" so the field is always
// safe to grep for an exact bot name.
//
// Coverage focuses on bots that actually show up in real access logs:
// search-engine crawlers, social-media link unfurlers, LLM training
// & retrieval crawlers, and a handful of SEO scrapers. Easy to extend
// — just add a {needle, name} pair, mindful of order.
func DetectBot(ua string) string {
	if ua == "" {
		return "-"
	}
	lower := strings.ToLower(ua)
	for _, p := range botPatterns {
		if strings.Contains(lower, p.needle) {
			return p.name
		}
	}
	return "-"
}

// botPatterns is consulted top-to-bottom; the first substring hit
// wins. List specific identifiers (chatgpt-user) above their family
// (gptbot) and family above generic markers, so DetectBot returns the
// most useful label.
var botPatterns = []struct{ needle, name string }{
	// Search engines
	{"googlebot", "googlebot"},
	{"bingbot", "bingbot"},
	{"applebot", "applebot"},
	{"duckduckbot", "duckduckbot"},
	{"yandex", "yandex"},
	{"baiduspider", "baidu"},
	{"seznambot", "seznam"},
	{"petalbot", "petal"},
	{"coccoc", "coccoc"},

	// Social / link unfurlers
	{"facebookexternalhit", "facebook"},
	{"facebot", "facebook"},
	{"twitterbot", "twitter"},
	{"linkedinbot", "linkedin"},
	{"slackbot", "slack"},
	{"discordbot", "discord"},
	{"whatsapp", "whatsapp"},
	{"telegrambot", "telegram"},
	{"pinterestbot", "pinterest"},

	// LLM crawlers — list user-facing ones above training crawlers,
	// and specific names above generic vendor strings.
	{"chatgpt-user", "openai-chatgpt"},
	{"oai-searchbot", "openai-search"},
	{"gptbot", "openai-gptbot"},
	{"claudebot", "anthropic-claude"},
	{"claude-web", "anthropic-claude"},
	{"anthropic-ai", "anthropic"},
	{"perplexitybot", "perplexity"},
	{"perplexity-user", "perplexity"},
	{"cohere-ai", "cohere"},
	{"google-extended", "google-extended"},

	// SEO / market intelligence — frequent and worth identifying.
	{"ahrefsbot", "ahrefs"},
	{"semrushbot", "semrush"},
	{"mj12bot", "mj12"},
	{"dotbot", "moz"},

	// Other commercial crawlers
	{"amazonbot", "amazon"},
	{"bytespider", "bytedance"},
}

// statusRecorder is the minimum wrapping needed to learn the status
// code after the handler runs. No body buffering — that would defeat
// streaming responses (favicon, static assets).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter when it implements
// http.Flusher — important for SSE handlers that need to push events
// before the response is complete.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func headerOrDash(r *http.Request, name string) string {
	if v := r.Header.Get(name); v != "" {
		return v
	}
	return "-"
}
