package constants

// HTTP header and cookie names used across HB Labs services.
// Centralised so the spelling is identical at every call site
// (header names are case-insensitive on the wire, but consistent
// capitalisation keeps grep / log search predictable) and so a
// rename is a single-edit change instead of a repo-wide
// find-and-replace.

// Correlation headers minted or consumed by httpx middleware.
const (
	// HeaderRequestID carries the per-request correlation ID.
	// Echoed back on the response so clients can grep their own
	// logs by the same ID.
	HeaderRequestID = "X-Request-Id"

	// HeaderDeviceID carries the per-device identifier used by
	// native apps (IDFV / equivalent). Browser clients use the
	// CookieDeviceID cookie instead.
	HeaderDeviceID = "X-Device-Id"

	// HeaderHBServiceToken carries the shared internal service token
	// gating cluster-internal routes (e.g. service-flags' /v1/flags*).
	// Backend services and tooling set it; the receiving service
	// compares it constant-time against its configured secret. A v1
	// placeholder for real service identity (mTLS / SPIFFE) later.
	HeaderHBServiceToken = "X-HB-Service-Token"
)

// Cookies set by httpx middleware.
const (
	// CookieDeviceID is the browser-side device-id cookie. Sticky
	// across visits so RequestLog and analytics tag the same visitor
	// without server-side state.
	CookieDeviceID = "hbl_did"

	// CookieSessionID is the browser-side session cookie. Value is
	// the AuthSession's public_id ("hb_ses_<nanoid>"); server-side
	// lookup against `auth_sessions` keeps state revocable. Set
	// HttpOnly + Secure + SameSite=Lax; in production scoped to
	// Domain=.hblabs.co so console and ops share the session.
	CookieSessionID = "hbl_sid"
)

// Cloudflare-injected request headers. Trustworthy only when the
// TCP peer is inside the Cloudflare CIDR set (see httpx.CIDRSet);
// raw clients can spoof these freely.
const (
	HeaderCFConnectingIP = "CF-Connecting-IP"
	HeaderCFIPCountry    = "CF-IPCountry"
	HeaderCFRay          = "CF-Ray"
	HeaderCFVerifiedBot  = "CF-Verified-Bot"

	// Visitor location headers — added by Cloudflare's "Add visitor
	// location headers" managed transform (off by default; enable it
	// on the zone). Same trust rule as the CF-* set above.
	HeaderCFIPCity      = "CF-IPCity"
	HeaderCFIPContinent = "CF-IPContinent"
	HeaderCFIPLatitude  = "CF-IPLatitude"
	HeaderCFIPLongitude = "CF-IPLongitude"
)

// Generic proxy / forwarding headers. Same trust caveat as the
// CF-* set: only honour them when the TCP peer is a known proxy.
const (
	HeaderXRealIP         = "X-Real-IP"
	HeaderXForwardedFor   = "X-Forwarded-For"
	HeaderXAccelBuffering = "X-Accel-Buffering"
)

// Inbound headers consumed by content negotiation / client-meta.
const (
	HeaderUserAgent       = "User-Agent"
	HeaderAccept          = "Accept"
	HeaderAcceptLanguage  = "Accept-Language"
	HeaderXAppVersion     = "X-App-Version"
	HeaderSecCHUA         = "Sec-CH-UA"
	HeaderSecCHUAPlatform = "Sec-CH-UA-Platform"
)

// Standard response headers that handlers and middleware set.
const (
	HeaderContentType   = "Content-Type"
	HeaderContentLength = "Content-Length"
	HeaderCacheControl  = "Cache-Control"
	HeaderVary          = "Vary"
	HeaderConnection    = "Connection"
	HeaderRetryAfter    = "Retry-After"
)

// Security response headers managed by core/secheaders.
const (
	HeaderContentSecurityPolicy           = "Content-Security-Policy"
	HeaderContentSecurityPolicyReportOnly = "Content-Security-Policy-Report-Only"
	HeaderStrictTransportSecurity         = "Strict-Transport-Security"
	HeaderReferrerPolicy                  = "Referrer-Policy"
	HeaderPermissionsPolicy               = "Permissions-Policy"
	HeaderXContentTypeOptions             = "X-Content-Type-Options"
	HeaderXFrameOptions                   = "X-Frame-Options"
)
