package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/log"
)

// TestClientIP_CFConnectingIPWins pins the documented preference
// chain: behind Cloudflare, CF-Connecting-IP is the only fully
// trustworthy header, so it must beat X-Real-IP and XFF when all
// three are present.
func TestClientIP_CFConnectingIPWins(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("CF-Connecting-IP", "1.1.1.1")
	r.Header.Set("X-Real-IP", "2.2.2.2")
	r.Header.Set("X-Forwarded-For", "3.3.3.3")
	if got := ClientIP(r); got != "1.1.1.1" {
		t.Errorf("ClientIP = %q, want \"1.1.1.1\"", got)
	}
}

// TestClientIP_XRealIPNext — without CF-Connecting-IP, X-Real-IP is
// next in the chain. Covers the second branch.
func TestClientIP_XRealIPNext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Real-IP", "2.2.2.2")
	r.Header.Set("X-Forwarded-For", "3.3.3.3")
	if got := ClientIP(r); got != "2.2.2.2" {
		t.Errorf("ClientIP = %q, want \"2.2.2.2\"", got)
	}
}

// TestClientIP_XFFFirstHop — XFF is a comma-separated list; only
// the first hop (the original client) belongs in `ip`.
func TestClientIP_XFFFirstHop(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Forwarded-For", " 3.3.3.3 , 10.0.0.2 , 10.0.0.3 ")
	if got := ClientIP(r); got != "3.3.3.3" {
		t.Errorf("ClientIP = %q, want \"3.3.3.3\"", got)
	}
}

// TestClientIP_XFFSingle — XFF with a single value (no comma) must
// still be trimmed and returned. Different branch from the multi-
// value case.
func TestClientIP_XFFSingle(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "  3.3.3.3  ")
	if got := ClientIP(r); got != "3.3.3.3" {
		t.Errorf("ClientIP = %q, want \"3.3.3.3\"", got)
	}
}

// TestClientIP_RemoteAddrFallback — no proxy headers, normal
// host:port RemoteAddr. The host portion is what we want.
func TestClientIP_RemoteAddrFallback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if got := ClientIP(r); got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want \"10.0.0.1\"", got)
	}
}

// TestClientIP_RemoteAddrMalformed — RemoteAddr with no port (rare
// but legal in tests / unix sockets) trips SplitHostPort; the
// fallback returns the raw value so the function stays total.
func TestClientIP_RemoteAddrMalformed(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "weird-no-port"
	if got := ClientIP(r); got != "weird-no-port" {
		t.Errorf("ClientIP = %q, want \"weird-no-port\"", got)
	}
}

// TestQuietPaths_ExactMatch demotes the request to Debug. Without
// this branch the kubelet probe spam would re-flood production logs
// the next time someone bumps LOG_LEVEL.
func TestQuietPaths_ExactMatch(t *testing.T) {
	fn := QuietPaths("/healthz")
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if got := fn(r); got != log.LevelDebug {
		t.Errorf("level = %v, want Debug", got)
	}
}

// TestQuietPaths_PrefixMatch — trailing "*" means prefix; covers
// the second branch of the path resolver. The "/static/" prefix is
// the canonical asset bucket.
func TestQuietPaths_PrefixMatch(t *testing.T) {
	fn := QuietPaths("/static/*")
	r := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	if got := fn(r); got != log.LevelDebug {
		t.Errorf("level = %v, want Debug", got)
	}
}

// TestQuietPaths_NoMatchInfo — an unrelated path keeps the default
// Info level. Without this guard QuietPaths could regress to silently
// downgrading every request.
func TestQuietPaths_NoMatchInfo(t *testing.T) {
	fn := QuietPaths("/healthz", "/static/*")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/things", nil)
	if got := fn(r); got != log.LevelInfo {
		t.Errorf("level = %v, want Info", got)
	}
}

// TestQuietPaths_SkipsBlanks — splitting an unset env var on "," can
// produce empty/whitespace entries; the helper must treat them as
// no-ops so the input is safe even when LOG_QUIET_PATHS is unset.
func TestQuietPaths_SkipsBlanks(t *testing.T) {
	fn := QuietPaths("", "  ", "/healthz")
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if got := fn(r); got != log.LevelDebug {
		t.Errorf("level = %v, want Debug", got)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/api", nil)
	if got := fn(r2); got != log.LevelInfo {
		t.Errorf("level = %v, want Info (no match)", got)
	}
}

// TestDetectBot_Empty — empty UAs are common (some HTTP clients,
// healthchecks); we return "-" so a `grep bot=` always finds a
// value to match against.
func TestDetectBot_Empty(t *testing.T) {
	if got := DetectBot(""); got != "-" {
		t.Errorf("DetectBot(\"\") = %q, want \"-\"", got)
	}
}

// TestDetectBot_KnownPatterns covers a handful of representative
// branches across the table — search engines, social unfurlers and
// LLM crawlers — so a single test pinpoints regressions in any
// region of the pattern list.
func TestDetectBot_KnownPatterns(t *testing.T) {
	cases := []struct {
		ua, want string
	}{
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "googlebot"},
		{"facebookexternalhit/1.1", "facebook"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ChatGPT-User/1.0; +https://openai.com/bot)", "openai-chatgpt"},
		{"Mozilla/5.0 (compatible; GPTBot/1.0; +https://openai.com/gptbot)", "openai-gptbot"},
		{"Mozilla/5.0 (compatible; ClaudeBot/1.0)", "anthropic-claude"},
		{"Mozilla/5.0 (compatible; AhrefsBot/7.0)", "ahrefs"},
	}
	for _, tc := range cases {
		if got := DetectBot(tc.ua); got != tc.want {
			t.Errorf("DetectBot(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

// TestDetectBot_BrowserUnknown — a real browser UA must NOT be
// labelled. Returning "-" is the documented "no idea" answer; tag-
// happy regressions would inflate bot metrics with humans.
func TestDetectBot_BrowserUnknown(t *testing.T) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	if got := DetectBot(ua); got != "-" {
		t.Errorf("DetectBot(browser) = %q, want \"-\"", got)
	}
}

// TestAccessLog_HappyPath — the middleware emits exactly one record
// per request with the canonical fields. This pins the access log
// shape ops grep against, so renaming a field is a deliberate change
// not a silent regression.
func TestAccessLog_HappyPath(t *testing.T) {
	buf := log.CaptureAt(t, "accesslog", log.LevelDebug)

	mw := AccessLog(AccessLogOptions{})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brewed"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1", nil)
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("CF-IPCountry", "NL")
	req.Header.Set("User-Agent", "Mozilla/5.0 Test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{
		"msg=http",
		"method=GET",
		"path=/api/v1",
		"status=418",
		"cf_country=NL",
		"cf_ray=-",
		"bot=-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestAccessLog_LevelBelowThreshold — when the configured level is
// Debug but slog.Default() is at Info, the middleware must skip
// the attribute build entirely (the `Enabled` short-circuit). We
// detect the skip indirectly by asserting no log line is emitted.
func TestAccessLog_LevelBelowThreshold(t *testing.T) {
	buf := log.CaptureAt(t, "accesslog", log.LevelInfo) // ignore Debug

	mw := AccessLog(AccessLogOptions{
		Level: func(*http.Request) log.Level { return log.LevelDebug },
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if buf.Len() != 0 {
		t.Errorf("expected silent skip, got log:\n%s", buf.String())
	}
}

// TestAccessLog_ContextAttrs — when ContextAttrs is wired, its
// returned slog.Attrs must surface in the access record alongside
// the canonical fields. ContextLogAttrs is the typical wiring;
// using it here also covers the integration the doc comment
// recommends.
func TestAccessLog_ContextAttrs(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{ContextAttrs: ContextLogAttrs})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ctx := WithRequestID(context.Background(), "rid-123")
	ctx = WithDeviceID(ctx, "did-456")
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "request_id=rid-123") {
		t.Errorf("missing request_id\n%s", out)
	}
	if !strings.Contains(out, "device_id=did-456") {
		t.Errorf("missing device_id\n%s", out)
	}
}

// TestAccessLog_ExtraAttrs — when ExtraAttrs is wired, request-
// derived fields (headers the standard set doesn't cover) must
// surface in the access record. Mirrors the service-ingest wiring
// that logs the iOS client's X-App-Version + Accept-Language.
func TestAccessLog_ExtraAttrs(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{
		ExtraAttrs: func(r *http.Request) []log.Attr {
			return []log.Attr{
				log.String("app_version", r.Header.Get("X-App-Version")),
			}
		},
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-App-Version", "1.4.2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if out := buf.String(); !strings.Contains(out, "app_version=1.4.2") {
		t.Errorf("missing app_version from ExtraAttrs\n%s", out)
	}
}

// TestAccessLog_ExtraAttrsNilSkipped — a nil ExtraAttrs must be a
// no-op (the common case for services that don't opt in). Guards
// against a nil-deref regression in the ServeHTTP append.
func TestAccessLog_ExtraAttrsNilSkipped(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{}) // ExtraAttrs nil
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if out := buf.String(); !strings.Contains(out, "msg=http") {
		t.Errorf("access record missing with nil ExtraAttrs\n%s", out)
	}
}

// TestAccessLog_ContextAttrsDefault — a nil ContextAttrs must
// default to ContextLogAttrs, so request_id / device_id surface
// without the caller wiring them. Pins the "standard correlation
// fields are built-in" contract: service-ingest (and anyone using
// AccessLog directly) gets them for free.
func TestAccessLog_ContextAttrsDefault(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{}) // ContextAttrs nil → defaults
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ctx := WithRequestID(context.Background(), "rid-default")
	ctx = WithDeviceID(ctx, "did-default")
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "request_id=rid-default") {
		t.Errorf("default ContextAttrs missing request_id\n%s", out)
	}
	if !strings.Contains(out, "device_id=did-default") {
		t.Errorf("default ContextAttrs missing device_id\n%s", out)
	}
}

// TestAccessLog_ContextAttrsSuppressed — passing a func that
// returns nil opts OUT of the default correlation fields. Guards
// the documented escape hatch so a privacy-sensitive service can
// keep IDs out of its access log.
func TestAccessLog_ContextAttrsSuppressed(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{
		ContextAttrs: func(context.Context) []log.Attr { return nil },
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ctx := WithRequestID(context.Background(), "rid-hidden")
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if out := buf.String(); strings.Contains(out, "rid-hidden") {
		t.Errorf("suppressed ContextAttrs leaked request_id\n%s", out)
	}
}

// TestAccessLog_Query — when Query is true, the raw query string
// surfaces as a `query` field. Pins the filter/sort/pagination
// visibility service-ingest wants on its list endpoint.
func TestAccessLog_Query(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{Query: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/list?q=trainer&sort=bytes_desc&page=2", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if out := buf.String(); !strings.Contains(out, "query=") || !strings.Contains(out, "sort=bytes_desc") {
		t.Errorf("missing query string in record\n%s", out)
	}
}

// TestAccessLog_QueryEmptySkipped — a path with no query string
// must NOT emit an empty `query` field. Keeps plain-path lines
// compact.
func TestAccessLog_QueryEmptySkipped(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{Query: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/list", nil))

	if out := buf.String(); strings.Contains(out, "query=") {
		t.Errorf("emitted empty query field\n%s", out)
	}
}

// TestAccessLog_QueryOffByDefault — with Query unset, the query
// string is NOT logged even when present. Guards the opt-in
// default so a service that never enables it can't leak a
// token-bearing URL into the access log.
func TestAccessLog_QueryOffByDefault(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{}) // Query false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x?token=secret", nil))

	if out := buf.String(); strings.Contains(out, "token=secret") {
		t.Errorf("query logged despite Query being off\n%s", out)
	}
}

// TestAccessLog_ClientMetaGroup — when ClientMeta is true, the full
// clientmeta fingerprint surfaces as a nested `client` group. The
// slog text handler flattens groups with dot notation
// (client.app_version=…), so we assert on that shape. client.ip
// reuses the access log's resolved IP, so it must match the
// top-level ip field.
func TestAccessLog_ClientMetaGroup(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{ClientMeta: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	req.Header.Set("User-Agent", "HBLabs-iOS/1.4.2")
	req.Header.Set("Accept-Language", "de-DE")
	req.Header.Set("X-App-Version", "1.4.2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, want := range []string{
		"client.user_agent=",
		"client.accept_language=de-DE",
		"client.app_version=1.4.2",
		"client.ip=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in client group\n%s", want, out)
		}
	}
}

// TestAccessLog_ClientMetaEmptySkipped covers the ok=false branch:
// a request with no resolvable IP and no metadata headers produces
// an empty clientmeta capture, so no `client` group is appended.
// Force it by clearing RemoteAddr (and sending no headers) so the
// IP resolves empty and LogFields returns nothing.
func TestAccessLog_ClientMetaEmptySkipped(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	mw := AccessLog(AccessLogOptions{ClientMeta: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "" // no peer → resolveIP empty → clientmeta empty
	req.Header = http.Header{}
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "client.") {
		t.Errorf("expected no client group for empty capture\n%s", out)
	}
	// The record itself must still fire (the group is the only thing
	// suppressed, not the whole line).
	if !strings.Contains(out, "msg=http") {
		t.Errorf("access record missing\n%s", out)
	}
}

// TestAccessLog_TrustedProxiesPath — when TrustedProxies is set, the
// middleware uses ClientIPTrusted instead of the legacy permissive
// ClientIP. Pin it by making the TCP peer NOT in the set: the `ip`
// field must equal the raw peer host, ignoring the spoofed CF
// header.
func TestAccessLog_TrustedProxiesPath(t *testing.T) {
	buf := log.Capture(t, "accesslog")

	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	mw := AccessLog(AccessLogOptions{TrustedProxies: set})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"           // outside trusted set
	req.Header.Set("CF-Connecting-IP", "1.2.3.4") // spoofed
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "ip=203.0.113.5") {
		t.Errorf("expected ip=203.0.113.5 (raw peer), got:\n%s", out)
	}
	// The spoofed header is still recorded as cf_ip for forensic
	// purposes — operators correlate cf_ip vs ip to spot spoofing.
	if !strings.Contains(out, "cf_ip=1.2.3.4") {
		t.Errorf("expected cf_ip=1.2.3.4 in log, got:\n%s", out)
	}
}

// TestStatusRecorder_FlushFlusher — when the underlying writer
// implements http.Flusher (e.g. for SSE), the wrapper must forward
// the call. Without this SSE handlers wrapped by AccessLog would
// buffer until response close — fatal for streaming.
func TestStatusRecorder_FlushFlusher(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: rr, status: http.StatusOK}
	rec.Flush()
	if !rr.Flushed {
		t.Error("Flush did not propagate to underlying ResponseWriter")
	}
}

// TestStatusRecorder_FlushNoFlusher — when the underlying writer
// does NOT implement http.Flusher, the wrapper must silently no-op
// (rather than panic on a nil Flusher cast).
func TestStatusRecorder_FlushNoFlusher(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: nonFlusher{}, status: http.StatusOK}
	rec.Flush() // must not panic
}

// nonFlusher implements just enough of http.ResponseWriter to be
// embedded in a statusRecorder; it intentionally lacks Flush so the
// type assertion in statusRecorder.Flush takes the !ok branch.
type nonFlusher struct{}

func (nonFlusher) Header() http.Header         { return http.Header{} }
func (nonFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (nonFlusher) WriteHeader(int)             {}
