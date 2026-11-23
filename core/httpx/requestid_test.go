package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// TestRequestID_GeneratesAndEchoesIDs — the core happy path. With
// neither header nor cookie present the middleware must mint both
// IDs, echo them back to the client, and stash them on the context
// so downstream code (loggers, access log) sees the same values.
func TestRequestID_GeneratesAndEchoesIDs(t *testing.T) {
	var seenRID, seenDID string
	mw := RequestID(log.Quiet())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRID = RequestIDFromContext(r.Context())
		seenDID = DeviceIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seenRID == "" {
		t.Error("request_id not stored on context")
	}
	if seenDID == "" {
		t.Error("device_id not stored on context")
	}
	if got := rec.Header().Get(HeaderRequestID); got != seenRID {
		t.Errorf("response %s = %q, want %q", HeaderRequestID, got, seenRID)
	}
	if got := rec.Header().Get(HeaderDeviceID); got != seenDID {
		t.Errorf("response %s = %q, want %q", HeaderDeviceID, got, seenDID)
	}
	// A Set-Cookie must accompany the freshly minted device-id so
	// the next request from this browser brings it back.
	cookies := rec.Result().Cookies()
	var cookie *http.Cookie
	for _, c := range cookies {
		if c.Name == CookieDeviceID {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("no Set-Cookie for device-id")
	}
	if cookie.Value != seenDID {
		t.Errorf("cookie = %q, want %q", cookie.Value, seenDID)
	}
	if cookie.MaxAge != deviceIDCookieMaxAge {
		t.Errorf("cookie MaxAge = %d, want %d", cookie.MaxAge, deviceIDCookieMaxAge)
	}
	if !cookie.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	// Default: HB_COOKIE_DOMAIN unset → host-only cookie. The
	// shared-subdomain variant is pinned in
	// TestRequestID_CookieDomainFromEnv.
	if cookie.Domain != "" {
		t.Errorf("cookie Domain = %q, want \"\" (host-only when HB_COOKIE_DOMAIN unset)", cookie.Domain)
	}
}

// TestRequestID_PrefersIncomingHeader — when an upstream proxy or
// client sends X-Request-Id, we must preserve it so they can
// correlate their own logs with ours. Generating a fresh value would
// break the contract.
func TestRequestID_PrefersIncomingHeader(t *testing.T) {
	var seen string
	mw := RequestID(log.Quiet())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		// Also assert the value is back on the *request* header for
		// any downstream middleware that reads headers (rather than
		// context).
		if got := r.Header.Get(HeaderRequestID); got != "incoming-rid" {
			t.Errorf("r.Header[%s] = %q, want \"incoming-rid\"", HeaderRequestID, got)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "incoming-rid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "incoming-rid" {
		t.Errorf("seen = %q, want \"incoming-rid\"", seen)
	}
}

// TestRequestID_PrefersDeviceIDHeader — native apps send
// X-Device-Id directly (IDFV / ANDROID_ID). The middleware must
// honour it without generating a competing cookie value.
func TestRequestID_PrefersDeviceIDHeader(t *testing.T) {
	var seen string
	mw := RequestID(nil)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = DeviceIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderDeviceID, "device-from-header")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "device-from-header" {
		t.Errorf("seen = %q, want \"device-from-header\"", seen)
	}
	// No Set-Cookie when the header is the source of truth — we
	// don't want to overwrite native-app-side state.
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieDeviceID {
			t.Errorf("unexpected Set-Cookie %s when header was provided", CookieDeviceID)
		}
	}
}

// TestRequestID_PrefersDeviceIDCookie — returning browsers carry
// the hbl_did cookie; the middleware must honour it without
// generating a fresh value or re-Setting the cookie (the browser
// already stores it).
func TestRequestID_PrefersDeviceIDCookie(t *testing.T) {
	var seen string
	mw := RequestID(nil)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = DeviceIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieDeviceID, Value: "device-from-cookie"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "device-from-cookie" {
		t.Errorf("seen = %q, want \"device-from-cookie\"", seen)
	}
	// Don't re-Set the cookie when it's already there — wastes
	// bytes on every response and triggers extra browser writes.
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieDeviceID {
			t.Errorf("unexpected re-Set of %s cookie", CookieDeviceID)
		}
	}
}

// TestRequestID_AttachesLoggerWithIDs — the per-request logger
// pulled from context (log.LoggerFromContext) must be pre-bound
// with request_id and device_id so handler logs carry the same
// correlation fields as the access log without re-attaching them.
func TestRequestID_AttachesLoggerWithIDs(t *testing.T) {
	buf, base := log.Buffered()
	mw := RequestID(base)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.LoggerFromContext(r.Context()).Info("inside")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "rid-pinned")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, "request_id=rid-pinned") {
		t.Errorf("log missing request_id\n--- got ---\n%s", out)
	}
	if !strings.Contains(out, "device_id=") {
		t.Errorf("log missing device_id\n--- got ---\n%s", out)
	}
}

// TestRequestID_NilBaseUsesDefault — passing nil must fall back to
// log.Default(). Pinning the branch so a refactor doesn't drop the
// nil guard and leave services with a nil-deref on boot when they
// forget to pass a logger.
func TestRequestID_NilBaseUsesDefault(t *testing.T) {
	buf := log.Capture(t, "requestid")

	mw := RequestID(nil)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.LoggerFromContext(r.Context()).Info("hello")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("expected log via default, got:\n%s", buf.String())
	}
}

// TestRequestID_EmptyCookieFallsThrough — a cookie header with an
// empty value must NOT be treated as a hit; the middleware must
// continue to the generate-and-set branch. Otherwise an attacker
// (or buggy client) clearing the cookie value would prevent us from
// ever assigning a real device-id.
func TestRequestID_EmptyCookieFallsThrough(t *testing.T) {
	var seen string
	mw := RequestID(nil)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = DeviceIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieDeviceID, Value: ""})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Error("device_id empty; should have been generated")
	}
	// And a fresh Set-Cookie must be on the response.
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieDeviceID && c.Value == seen {
			found = true
		}
	}
	if !found {
		t.Error("expected fresh Set-Cookie with the generated value")
	}
}

// TestRequestID_CookieDomainFromEnv — with HB_COOKIE_DOMAIN set
// (the prod profile) the freshly minted hbl_did cookie must carry
// that Domain so every *.hblabs.co subdomain shares one device-id.
// The unset default (host-only) is pinned in
// TestRequestID_GeneratesAndEchoesIDs.
func TestRequestID_CookieDomainFromEnv(t *testing.T) {
	t.Setenv(constants.EnvCookieDomain, ".hblabs.co")

	mw := RequestID(nil)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	// Assert on the raw Set-Cookie line so we see exactly what the
	// browser receives. net/http strips the leading dot from
	// Cookie.Domain on serialization (RFC 6265 §4.1.2.3: the dot is
	// ignored, "hblabs.co" and ".hblabs.co" are equivalent), so the
	// input ".hblabs.co" lands on the wire as "Domain=hblabs.co".
	// Either spelling produces the same subdomain-sharing behavior.
	var raw string
	for _, line := range rec.Result().Header["Set-Cookie"] {
		if strings.HasPrefix(line, CookieDeviceID+"=") {
			raw = line
			break
		}
	}
	if raw == "" {
		t.Fatal("no Set-Cookie for device-id")
	}
	if !strings.Contains(raw, "Domain=hblabs.co") {
		t.Errorf("Set-Cookie = %q, want it to carry Domain=hblabs.co", raw)
	}
}
