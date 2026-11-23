package httpx

import (
	"net/http"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/envcfg"
	"github.com/hblabs-co/kit/core/ids"
	"github.com/hblabs-co/kit/core/log"
)

// Re-exports of the canonical names from core/constants so
// callers (and tests) can refer to them through this package without
// importing constants directly. The strings themselves live in
// core/constants — single source of truth.
const (
	HeaderRequestID = constants.HeaderRequestID
	HeaderDeviceID  = constants.HeaderDeviceID
	CookieDeviceID  = constants.CookieDeviceID
)

// secondsPerDay is the count of seconds in a calendar day, used when
// expressing http.Cookie MaxAge in human-friendly day / year terms
// instead of a hard-coded total. http.Cookie.MaxAge is an int (seconds),
// hence the integer constants rather than time.Duration math.
const secondsPerDay = 24 * 60 * 60

// deviceIDCookieMaxAgeYears is the lifetime of the device-id cookie
// in calendar years. Long enough that returning visitors keep their
// device-id; short enough that a permanently-abandoned cookie
// eventually rotates without server-side state.
const deviceIDCookieMaxAgeYears = 2

// deviceIDCookieMaxAge is the device-id cookie lifetime expressed in
// seconds, the unit http.Cookie.MaxAge expects.
const deviceIDCookieMaxAge = deviceIDCookieMaxAgeYears * 365 * secondsPerDay

// deviceIDLength is the length (in URL-safe nanoid chars) of generated
// device IDs. 32 chars at the default alphabet (~64 symbols, ~6 bits
// per char) gives ~190 bits of entropy — plenty against brute-force
// guessing of another visitor's device-id from collected log samples.
const deviceIDLength = 32

// RequestID is the standard outermost-app middleware. It resolves a
// request-id and a device-id, stores both in the request context,
// and echoes them back as response headers so callers can correlate.
//
// Resolution rules:
//
//  1. request-id: prefer the X-Request-Id sent by the client (a
//     reverse proxy, an iOS app, a script with its own correlation
//     ID), else generate a fresh one. We always put it back in the
//     request header too so downstream handlers/middleware that read
//     headers (rather than the context) see the same value.
//
//  2. device-id: prefer the X-Device-Id header (native apps sending
//     IDFV or equivalent), then the hbl_did cookie (browser clients
//     who've been here before), else generate one and Set-Cookie it.
//     The result persists across requests for browsers without any
//     server-side state — exactly what RequestLog and analytics
//     downstream want. The Set-Cookie's Domain comes from
//     HB_COOKIE_DOMAIN: empty (host-only) on a laptop / custom
//     domain, ".hblabs.co" in prod so every *.hblabs.co subdomain
//     shares one device-id.
//
// The middleware also attaches a slog.Logger pre-bound with both
// IDs to the context. Handlers retrieve it with httpx.Logger(ctx)
// so every line they emit carries request_id and device_id without
// having to re-attach them on each call.
//
// `base` is the logger to derive the per-request logger from. Pass
// the same one set with log.SetDefault(log.New("svc")) at boot so
// handler logs inherit the service tag. nil → log.Default().
func RequestID(base *log.Logger) func(http.Handler) http.Handler {
	if base == nil {
		base = log.Default()
	}
	// Resolved once at middleware-build (boot) time: the cookie
	// domain is a deploy-level constant, not a per-request value.
	// Empty → host-only cookie (dev/localhost, custom-domain
	// deploys); ".hblabs.co" in prod → shared across subdomains.
	cookieDomain := envcfg.String(constants.EnvCookieDomain, "")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(HeaderRequestID)
			if rid == "" {
				rid = ids.New()
				r.Header.Set(HeaderRequestID, rid)
			}
			did := resolveDeviceID(w, r, cookieDomain)

			// Echo both back so the caller can grep its own logs by
			// request-id and so native apps see what we settled on.
			w.Header().Set(HeaderRequestID, rid)
			if did != "" {
				w.Header().Set(HeaderDeviceID, did)
			}

			reqLogger := base.With(
				log.String("request_id", rid),
				log.String("device_id", did),
			)

			ctx := r.Context()
			ctx = WithRequestID(ctx, rid)
			ctx = WithDeviceID(ctx, did)
			ctx = log.WithLogger(ctx, reqLogger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveDeviceID picks the device-id following the
// header → cookie → generate-and-set chain. The Set-Cookie path is
// what makes browser-side device-ids "sticky" without server state:
// the first response carries the cookie, every subsequent request
// brings it back, and the middleware short-circuits on the cookie
// branch.
//
// Secure flag is intentionally NOT set: the same binary runs on
// http://localhost (where browsers reject Secure cookies) and
// behind HTTPS in prod. The reverse proxy / edge upgrades this to
// HSTS-bound HTTPS-only at the origin level, which is the right
// place to enforce it.
//
// cookieDomain is HB_COOKIE_DOMAIN, resolved once by the caller. An
// empty string leaves http.Cookie.Domain unset, so the browser
// treats the cookie as host-only — the correct behavior on a laptop
// and on customer-owned domains (where a ".hblabs.co" cookie would
// be a rejected cross-site write anyway). ".hblabs.co" in prod
// makes the one device-id span every *.hblabs.co subdomain.
func resolveDeviceID(w http.ResponseWriter, r *http.Request, cookieDomain string) string {
	if v := r.Header.Get(HeaderDeviceID); v != "" {
		return v
	}
	if c, _ := r.Cookie(CookieDeviceID); c != nil && c.Value != "" {
		return c.Value
	}
	did := ids.NewSize(deviceIDLength)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieDeviceID,
		Value:    did,
		Path:     "/",
		Domain:   cookieDomain,
		MaxAge:   deviceIDCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return did
}
