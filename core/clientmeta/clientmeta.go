// Package clientmeta captures HTTP request metadata that's useful
// for understanding the caller of any endpoint — auth intents
// today, audit logs / abuse detection / analytics tomorrow.
//
// Design rule: capture, don't interpret. We snapshot the headers
// verbatim and let the consumer decide what to do with them. Only
// `IP` is computed (the caller passes it in, typically from
// httplog.ClientIPTrusted) — the proxy headers are still kept raw
// so a downstream investigator can see the full chain.
//
// Cloudflare headers (CF-*) are present only when CF sits in front
// of the request; Sec-CH-UA-* only on browsers that opt into Client
// Hints. Both fields end up empty for native iOS calls — that's
// fine, omitempty keeps the JSON small.
//
// The visitor-location fields (CFCity/CFContinent/CFLatitude/
// CFLongitude, alongside the existing CFCountry) come from
// Cloudflare's "Add visitor location headers" managed transform,
// which is OFF by default — enable it on the zone or they stay
// empty. They are coarse IP-level geo, captured verbatim (lat/long
// kept as strings); good for analytics and abuse heuristics, not
// for anything needing real precision.
package clientmeta

import (
	"net"
	"net/http"

	"github.com/hblabs-co/kit/core/constants"
)

// logFieldCount is the maximum number of entries LogFields can emit —
// one per non-empty ClientMeta field. Used as the initial capacity for
// the result map so the first appends never reallocate.
const logFieldCount = 15

// ClientMeta is the canonical bag of "what we know about the
// caller from this HTTP request". Embed it as a sub-document in
// any model that wants to record this metadata; reuse the Capture
// helper at the call site.
//
// Snake-case JSON tags match the usual model convention.
type ClientMeta struct {
	// IP is the best-effort real client IP. Caller resolves it
	// (typically via httplog.ClientIPTrusted) and passes it in;
	// we just normalise to RFC 5952 canonical form.
	IP string `json:"ip,omitempty"`

	// UserAgent is the raw User-Agent header. Not parsed here —
	// downstream tooling (admin UI, analytics) does that.
	UserAgent string `json:"user_agent,omitempty"`

	// AcceptLanguage is the raw Accept-Language header. Useful for
	// localising emails before the user has any explicit locale
	// preference saved.
	AcceptLanguage string `json:"accept_language,omitempty"`

	// AppVersion is the X-App-Version custom header — populated
	// once the iOS / future web client starts sending it.
	AppVersion string `json:"app_version,omitempty"`

	// Sec-CH-UA-* (Client Hints) — only browsers that opt into the
	// Client Hints standard send these. Native iOS via URLSession
	// does NOT, so expect empty for the iOS app today. Keep the
	// fields so when the web client lands they slot in.
	SecCHUA         string `json:"sec_ch_ua,omitempty"`
	SecCHUAPlatform string `json:"sec_ch_ua_platform,omitempty"`

	// Proxy headers — the raw chain. The resolved IP already picks
	// the "right" value from these, but keeping the raw values lets
	// ops see whether the request hit a proxy and which one.
	XRealIP       string `json:"x_real_ip,omitempty"`
	XForwardedFor string `json:"x_forwarded_for,omitempty"`

	// Cloudflare-specific headers — only present when CF sits in
	// front of the request. CFRay is the unique request ID (great
	// for support tickets that need to correlate with CF logs);
	// CFCountry is the ISO-3166-1 alpha-2 country code that CF
	// derives from the source IP (cheaper than running our own
	// GeoIP lookup); CFConnectingIP is CF's view of the real
	// client IP, useful as a sanity check against the resolved IP.
	CFRay          string `json:"cf_ray,omitempty"`
	CFCountry      string `json:"cf_country,omitempty"`
	CFConnectingIP string `json:"cf_connecting_ip,omitempty"`

	// Visitor location, derived by Cloudflare from the source IP
	// when the "Add visitor location headers" managed transform is
	// enabled on the zone (off by default). Coarse, IP-level — good
	// for analytics / abuse heuristics, not for anything that needs
	// real precision. Empty when the transform is off or CF isn't in
	// front. Captured verbatim; latitude/longitude stay strings
	// (the package never interprets — a consumer parses if it needs
	// floats).
	CFCity      string `json:"cf_city,omitempty"`
	CFContinent string `json:"cf_continent,omitempty"`
	CFLatitude  string `json:"cf_latitude,omitempty"`
	CFLongitude string `json:"cf_longitude,omitempty"`
}

// Capture builds a ClientMeta from a generic header lookup
// function and a pre-resolved client IP. Framework-agnostic on
// purpose — packages/ doesn't depend on any particular HTTP
// router. Pass a function (not a map or http.Header) so callers
// who normalise their headers can plug their own normaliser in.
//
// For net/http use FromRequest, which wires r.Header.Get for you.
func Capture(getHeader func(string) string, clientIP string) ClientMeta {
	cfIP, _ := NormalizeIP(getHeader(constants.HeaderCFConnectingIP))
	ip, _ := NormalizeIP(clientIP)
	return ClientMeta{
		IP:              ip,
		UserAgent:       getHeader(constants.HeaderUserAgent),
		AcceptLanguage:  getHeader(constants.HeaderAcceptLanguage),
		AppVersion:      getHeader(constants.HeaderXAppVersion),
		SecCHUA:         getHeader(constants.HeaderSecCHUA),
		SecCHUAPlatform: getHeader(constants.HeaderSecCHUAPlatform),
		XRealIP:         getHeader(constants.HeaderXRealIP),
		XForwardedFor:   getHeader(constants.HeaderXForwardedFor),
		CFRay:           getHeader(constants.HeaderCFRay),
		CFCountry:       getHeader(constants.HeaderCFIPCountry),
		CFConnectingIP:  cfIP,
		CFCity:          getHeader(constants.HeaderCFIPCity),
		CFContinent:     getHeader(constants.HeaderCFIPContinent),
		CFLatitude:      getHeader(constants.HeaderCFIPLatitude),
		CFLongitude:     getHeader(constants.HeaderCFIPLongitude),
	}
}

// FromRequest is the net/http convenience wrapper around Capture.
// clientIP should come from httplog.ClientIPTrusted in production
// (so an attacker bypassing the proxy can't lie about their IP);
// httplog.ClientIP is acceptable in tests / dev.
func FromRequest(r *http.Request, clientIP string) ClientMeta {
	return Capture(r.Header.Get, clientIP)
}

// NormalizeIP parses `ip` and returns its RFC 5952 canonical form
// plus an `ok` flag. If the input doesn't parse, the original
// string is returned unchanged with ok=false (audit captures
// shouldn't drop data; admin filters can use the flag to reject
// with 400). Same primitive on both sides of the boundary so the
// canonical form written to Mongo equals the canonical form an
// admin filter looks up — no false-empty results from `2001:DB8`
// vs `2001:db8` mismatches.
func NormalizeIP(ip string) (string, bool) {
	if ip == "" {
		return "", false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip, false
	}
	return parsed.String(), true
}

// LogFields returns the captured metadata as a map for log
// enrichment. Returns map[string]any (NOT slog.Attr) so this
// package stays free of any logging-library coupling — callers
// can pass the result to slog.Group, logrus.WithFields, zap.Any,
// or whatever. Only non-empty fields are included so logs stay
// readable.
func (m ClientMeta) LogFields() map[string]any {
	out := make(map[string]any, logFieldCount)
	add := func(key, val string) {
		if val != "" {
			out[key] = val
		}
	}
	add("ip", m.IP)
	add("user_agent", m.UserAgent)
	add("accept_language", m.AcceptLanguage)
	add("app_version", m.AppVersion)
	add("sec_ch_ua", m.SecCHUA)
	add("sec_ch_ua_platform", m.SecCHUAPlatform)
	add("x_real_ip", m.XRealIP)
	add("x_forwarded_for", m.XForwardedFor)
	add("cf_ray", m.CFRay)
	add("cf_country", m.CFCountry)
	add("cf_connecting_ip", m.CFConnectingIP)
	add("cf_city", m.CFCity)
	add("cf_continent", m.CFContinent)
	add("cf_latitude", m.CFLatitude)
	add("cf_longitude", m.CFLongitude)
	return out
}
