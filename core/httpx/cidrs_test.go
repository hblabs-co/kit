package httpx

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseCIDRs_HappyPath pins that the parser preserves order and
// the resulting *net.IPNet values do real membership checks. Without
// this the rest of the package's trust boundary collapses to no-ops.
func TestParseCIDRs_HappyPath(t *testing.T) {
	nets, err := ParseCIDRs("10.0.0.0/8", "192.168.0.0/16")
	if err != nil {
		t.Fatalf("ParseCIDRs: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("len = %d, want 2", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("10.1.2.3")) {
		t.Errorf("10.1.2.3 not contained by 10.0.0.0/8")
	}
	if !nets[1].Contains(net.ParseIP("192.168.5.5")) {
		t.Errorf("192.168.5.5 not contained by 192.168.0.0/16")
	}
}

// TestParseCIDRs_SkipsBlanks — splitting an unset env var on "," can
// produce empty strings; the parser must treat them as no-ops so
// `strings.Split(envcfg.String("X", ""), ",")` is a safe input.
func TestParseCIDRs_SkipsBlanks(t *testing.T) {
	nets, err := ParseCIDRs("", "  ", "10.0.0.0/8", "")
	if err != nil {
		t.Fatalf("ParseCIDRs: %v", err)
	}
	if len(nets) != 1 {
		t.Errorf("len = %d, want 1 (only the real CIDR)", len(nets))
	}
}

// TestParseCIDRs_BadValue — a malformed CIDR is an operator config
// bug; the function must return the original value verbatim so the
// log line points at the actual offender, not a sanitised summary.
func TestParseCIDRs_BadValue(t *testing.T) {
	_, err := ParseCIDRs("not-a-cidr/99")
	if err == nil {
		t.Fatal("ParseCIDRs(\"not-a-cidr/99\") err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not-a-cidr/99") {
		t.Errorf("err = %v, want it to mention the bad value", err)
	}
}

// TestIsTrustedProxy_NilIP — net.ParseIP returns nil for a malformed
// host; IsTrustedProxy must short-circuit to false rather than blow
// up inside Contains. ClientIPTrusted depends on this in the
// "RemoteAddr is garbage" branch.
func TestIsTrustedProxy_NilIP(t *testing.T) {
	nets, _ := ParseCIDRs("10.0.0.0/8")
	if IsTrustedProxy(nil, nets) {
		t.Error("IsTrustedProxy(nil, …) = true, want false")
	}
}

// TestIsTrustedProxy_HitAndMiss covers both matching branches in the
// loop — a trusted IP must be reported, an outsider must not.
func TestIsTrustedProxy_HitAndMiss(t *testing.T) {
	nets, _ := ParseCIDRs("10.0.0.0/8", "192.168.0.0/16")
	if !IsTrustedProxy(net.ParseIP("192.168.10.1"), nets) {
		t.Error("192.168.10.1 should be trusted (matches 192.168.0.0/16)")
	}
	if IsTrustedProxy(net.ParseIP("8.8.8.8"), nets) {
		t.Error("8.8.8.8 should NOT be trusted")
	}
}

// TestNewCIDRSet_DefaultsClient — a nil http.Client must be replaced
// with one that has a 10s timeout. Keeping the default sane means
// every service Refresh()ing CF gets the same upstream behaviour
// without re-typing the timeout in a dozen call sites.
func TestNewCIDRSet_DefaultsClient(t *testing.T) {
	s := NewCIDRSet("https://example.test/", nil)
	if s.client == nil {
		t.Fatal("client = nil, want default *http.Client")
	}
	if s.client.Timeout != 10*time.Second {
		t.Errorf("client.Timeout = %v, want 10s", s.client.Timeout)
	}
}

// TestNewCIDRSet_KeepsExplicitClient — when the caller passes a
// custom client (e.g. one wired to a tracing transport), it must
// flow through unchanged. The default-client branch must NOT clobber
// it.
func TestNewCIDRSet_KeepsExplicitClient(t *testing.T) {
	custom := &http.Client{Timeout: 1 * time.Second}
	s := NewCIDRSet("https://example.test/", custom)
	if s.client != custom {
		t.Error("custom http.Client was replaced; want it preserved")
	}
}

// TestNewCloudflareCIDRSet_WiresOfficialEndpoint — pin that the
// helper uses Cloudflare's documented IPs URL. If this constant ever
// drifts the next operator who reads "trust the proxies" will be
// silently trusting nothing.
func TestNewCloudflareCIDRSet_WiresOfficialEndpoint(t *testing.T) {
	s := NewCloudflareCIDRSet()
	if s.url != CloudflareIPsURL {
		t.Errorf("url = %q, want %q", s.url, CloudflareIPsURL)
	}
	if s.client == nil {
		t.Error("client = nil, want default")
	}
}

// TestCIDRSet_Snapshot_BeforeRefresh — Snapshot must be safe to call
// before Refresh has ever succeeded so callers can range without nil
// checks during boot. Empty result = "trust nothing yet", which is
// exactly the safe default.
func TestCIDRSet_Snapshot_BeforeRefresh(t *testing.T) {
	s := NewCIDRSet("https://example.test/", nil)
	got := s.Snapshot()
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	// Range over the result; if Snapshot ever returns a value that
	// can't be ranged this loop will panic.
	for range got {
		t.Error("snapshot before refresh should be empty")
	}
}

// TestCIDRSet_Refresh_HappyPath — full lifecycle: a healthy upstream
// produces a populated, atomically-stored snapshot. Uses
// httptest.Server so the test never touches the real network.
func TestCIDRSet_Refresh_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": {
				"ipv4_cidrs": ["10.0.0.0/8", "192.168.0.0/16"],
				"ipv6_cidrs": ["::1/128"],
				"etag": "abc123"
			}
		}`)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Errorf("snapshot len = %d, want 3", len(snap))
	}
}

// TestCIDRSet_Refresh_BadURL forces the http.NewRequestWithContext
// branch to fail — the only way Refresh's first error path is hit
// without a network call. A control character in the URL trips
// url.Parse before any I/O.
func TestCIDRSet_Refresh_BadURL(t *testing.T) {
	s := NewCIDRSet("http://example.test/\x7f", nil)
	err := s.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh on malformed URL = nil err, want error")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("err = %v, want \"build request\" wrap", err)
	}
}

// TestCIDRSet_Refresh_TransportError — the upstream is unreachable.
// Refresh must return an error containing the URL so logs point at
// the actual offender (e.g. when an outage is to a specific mirror).
func TestCIDRSet_Refresh_TransportError(t *testing.T) {
	// Bind+close a port to get a guaranteed-unused address. The
	// kernel won't reuse it instantly on a localhost test host.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	s := NewCIDRSet("http://"+addr, &http.Client{Timeout: 200 * time.Millisecond})
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh against dead port = nil err, want error")
	} else if !strings.Contains(err.Error(), "fetch") {
		t.Errorf("err = %v, want \"fetch\" wrap", err)
	}
}

// TestCIDRSet_Refresh_Non200 — upstream returned a 5xx. The previous
// snapshot must remain in place; the operator sees the bad status
// without losing trust on a transient blip.
func TestCIDRSet_Refresh_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want \"status 500\"", err)
	}
}

// TestCIDRSet_Refresh_BadJSON — a malformed body is an upstream bug.
// The middleware must surface it (rather than store junk) so the
// operator sees the actual decode error in the log.
func TestCIDRSet_Refresh_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not-json`)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want \"decode\" wrap", err)
	}
}

// TestCIDRSet_Refresh_SuccessFalse — Cloudflare returned a body with
// success=false. Even though the HTTP status is 200, treat it as a
// failure so the snapshot stays unchanged.
func TestCIDRSet_Refresh_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"success": false, "result": {}}`)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Errorf("err = %v, want \"success=false\"", err)
	}
}

// TestCIDRSet_Refresh_BadCIDRInBody — the upstream said success=true
// but shipped junk. Surface the parse error verbatim so the operator
// sees the bad value (matches the contract of ParseCIDRs).
func TestCIDRSet_Refresh_BadCIDRInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": {"ipv4_cidrs": ["nope/99"], "ipv6_cidrs": []}
		}`)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parse cidrs") {
		t.Errorf("err = %v, want \"parse cidrs\"", err)
	}
}

// TestCIDRSet_Refresh_ZeroCIDRs — an empty list from upstream is
// suspicious enough to refuse the swap; without this guard a single
// bad deployment at Cloudflare would silently flip our trust boundary
// to "trust nothing", logging every request as anonymous.
func TestCIDRSet_Refresh_ZeroCIDRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": {"ipv4_cidrs": [], "ipv6_cidrs": []}
		}`)
	}))
	defer srv.Close()

	s := NewCIDRSet(srv.URL, srv.Client())
	err := s.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "zero CIDRs") {
		t.Errorf("err = %v, want \"zero CIDRs\"", err)
	}
}

// TestClientIPTrusted_NilSet — the documented "no proxies trusted"
// shortcut: when the set is nil, always return the raw RemoteAddr
// (host portion) so an attacker who finds the origin can't lie in
// our logs.
func TestClientIPTrusted_NilSet(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	got := ClientIPTrusted(r, nil)
	if got != "203.0.113.5" {
		t.Errorf("ClientIPTrusted = %q, want \"203.0.113.5\"", got)
	}
}

// TestClientIPTrusted_RemoteAddrNoPort — RemoteAddr without a port
// is unusual but legal; SplitHostPort errors and the function falls
// back to the raw value. Pinning the branch so a refactor doesn't
// drop the fallback and panic.
func TestClientIPTrusted_RemoteAddrNoPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5" // no port
	got := ClientIPTrusted(r, nil)
	if got != "203.0.113.5" {
		t.Errorf("ClientIPTrusted = %q, want \"203.0.113.5\"", got)
	}
}

// TestClientIPTrusted_UntrustedPeerIgnoresHeaders — the security-
// critical branch: if the TCP peer isn't in the trusted set, proxy
// headers MUST be ignored. Otherwise an attacker who connected
// directly to the origin could spoof the source IP in our logs.
func TestClientIPTrusted_UntrustedPeerIgnoresHeaders(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1234" // not in 10/8
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	got := ClientIPTrusted(r, set)
	if got != "203.0.113.5" {
		t.Errorf("ClientIPTrusted = %q, want \"203.0.113.5\" (untrusted peer)", got)
	}
}

// TestClientIPTrusted_TrustedPeerCFConnectingIP — the standard
// Cloudflare path: TCP peer is in the trusted set, so honour
// CF-Connecting-IP.
func TestClientIPTrusted_TrustedPeerCFConnectingIP(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	got := ClientIPTrusted(r, set)
	if got != "1.2.3.4" {
		t.Errorf("ClientIPTrusted = %q, want \"1.2.3.4\"", got)
	}
}

// TestClientIPTrusted_TrustedPeerXRealIP — fallback when CF-
// Connecting-IP is absent. Order matters: CF-Connecting-IP wins
// over X-Real-IP if both are set; this test isolates the X-Real-IP
// branch.
func TestClientIPTrusted_TrustedPeerXRealIP(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("X-Real-IP", "5.6.7.8")
	got := ClientIPTrusted(r, set)
	if got != "5.6.7.8" {
		t.Errorf("ClientIPTrusted = %q, want \"5.6.7.8\"", got)
	}
}

// TestClientIPTrusted_TrustedPeerXFFFirst — XFF with a comma-
// separated list: take the first hop (the original client). The
// rest are intermediate proxies and don't belong in `ip`.
func TestClientIPTrusted_TrustedPeerXFFFirst(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9 , 10.0.0.1")
	got := ClientIPTrusted(r, set)
	if got != "9.9.9.9" {
		t.Errorf("ClientIPTrusted = %q, want \"9.9.9.9\"", got)
	}
}

// TestClientIPTrusted_TrustedPeerXFFSingle — a single-value XFF
// with no comma should also be trimmed and returned. Different
// branch from the multi-value case.
func TestClientIPTrusted_TrustedPeerXFFSingle(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("X-Forwarded-For", "  9.9.9.9  ")
	got := ClientIPTrusted(r, set)
	if got != "9.9.9.9" {
		t.Errorf("ClientIPTrusted = %q, want \"9.9.9.9\"", got)
	}
}

// TestClientIPTrusted_TrustedPeerNoHeaders — TCP peer is trusted but
// the request has no proxy headers (someone hit the proxy directly,
// or the headers got stripped). Fall back to the host portion of
// RemoteAddr — we still trust the peer, we just have nothing else.
func TestClientIPTrusted_TrustedPeerNoHeaders(t *testing.T) {
	set := NewCIDRSet("ignored", nil)
	nets, _ := ParseCIDRs("10.0.0.0/8")
	set.cidrs.Store(&nets)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	got := ClientIPTrusted(r, set)
	if got != "10.1.2.3" {
		t.Errorf("ClientIPTrusted = %q, want \"10.1.2.3\"", got)
	}
}
