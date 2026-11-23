package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// CloudflareIPsURL is the official endpoint Cloudflare publishes for
// their proxy IP ranges (https://api.cloudflare.com/client/v4/ips).
// One unauthenticated GET returns both IPv4 + IPv6 CIDRs as JSON,
// edge-cached so the call is essentially free for both us and CF.
const CloudflareIPsURL = "https://api.cloudflare.com/client/v4/ips"

// defaultCIDRFetchTimeout caps the upstream fetch when NewCIDRSet is
// called without an explicit *http.Client. Sane for a small JSON
// payload from a CDN — anything slower is upstream pain we don't want
// to absorb on the request path.
const defaultCIDRFetchTimeout = 10 * time.Second

// ParseCIDRs converts a list of CIDR strings into *net.IPNet for fast
// membership checks. Whitespace and empty entries are ignored so the
// result of `strings.Split(env, ",")` can be passed verbatim even
// when the env var is unset.
//
// Returns the first parse error verbatim — operators see the bad
// value with line context instead of a silent partial parse.
func ParseCIDRs(cidrs ...string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("httpx: parse cidr %q: %w", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// IsTrustedProxy reports whether ip falls inside any of the trusted
// CIDRs. nil/empty trusted means "trust nothing" — callers should
// treat that as "no proxy headers honoured".
func IsTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// CIDRSet is a lock-free, periodically-refreshed view of a CIDR list
// — typically the Cloudflare proxy ranges. Reads (Snapshot) are an
// atomic.Pointer load, so the hot path on every request is a single
// pointer read + slice scan; writes (Refresh) replace the whole slice
// atomically so a concurrent reader sees either the old or new set,
// never a torn one.
//
// Pair with a periodic ticker for daily refreshes; if Refresh fails
// the previous good snapshot stays put, so a CF outage doesn't
// degrade the trust boundary.
type CIDRSet struct {
	url    string
	client *http.Client
	cidrs  atomic.Pointer[[]*net.IPNet]
}

// NewCIDRSet builds a generic CIDRSet for any JSON endpoint that
// returns a Cloudflare-shaped body (`{result:{ipv4_cidrs,ipv6_cidrs}}`).
// For the standard CF endpoint use NewCloudflareCIDRSet.
//
// httpClient may be nil; we build one with defaultCIDRFetchTimeout.
func NewCIDRSet(url string, httpClient *http.Client) *CIDRSet {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultCIDRFetchTimeout}
	}
	return &CIDRSet{url: url, client: httpClient}
}

// NewCloudflareCIDRSet returns a CIDRSet wired to Cloudflare's
// official IPs endpoint. Refresh hasn't been called yet — invoke it
// (synchronously, with a timeout context) at boot so the first
// request already sees a populated set.
func NewCloudflareCIDRSet() *CIDRSet {
	return NewCIDRSet(CloudflareIPsURL, nil)
}

// cfIPsResponse mirrors the shape of api.cloudflare.com/client/v4/ips.
// `etag` lets the caller detect a "no real change" refresh in logs.
type cfIPsResponse struct {
	Success bool `json:"success"`
	Result  struct {
		IPv4 []string `json:"ipv4_cidrs"`
		IPv6 []string `json:"ipv6_cidrs"`
		Etag string   `json:"etag"`
	} `json:"result"`
}

// Refresh fetches the upstream CIDR list and atomically swaps the
// in-memory snapshot. Safe to call concurrently with Snapshot.
//
// Errors leave the previous snapshot untouched — the caller (the
// ticker loop) keeps retrying on its retry cadence; meanwhile reads
// continue with whatever was last known good.
func (s *CIDRSet) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("httpx: build request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("httpx: fetch %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("httpx: fetch %s: status %d", s.url, resp.StatusCode)
	}
	var body cfIPsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("httpx: decode %s: %w", s.url, err)
	}
	if !body.Success {
		return fmt.Errorf("httpx: %s reported success=false", s.url)
	}
	cidrs, err := ParseCIDRs(append(body.Result.IPv4, body.Result.IPv6...)...)
	if err != nil {
		return fmt.Errorf("httpx: parse cidrs from %s: %w", s.url, err)
	}
	if len(cidrs) == 0 {
		return fmt.Errorf("httpx: %s returned zero CIDRs", s.url)
	}
	s.cidrs.Store(&cidrs)
	log.Info("trusted proxy cidrs refreshed",
		"source", s.url,
		"ipv4_count", len(body.Result.IPv4),
		"ipv6_count", len(body.Result.IPv6),
		"etag", body.Result.Etag,
	)
	return nil
}

// Snapshot returns the current in-memory CIDR list. Returns an empty
// slice (not nil) when Refresh has never succeeded — callers can
// range over the result without nil checks.
func (s *CIDRSet) Snapshot() []*net.IPNet {
	p := s.cidrs.Load()
	if p == nil {
		return nil
	}
	return *p
}

// ClientIPTrusted is the security-aware version of ClientIP. It only
// honours proxy headers (CF-Connecting-IP, X-Real-IP, XFF) when
// r.RemoteAddr falls inside the trusted set; otherwise it returns the
// raw RemoteAddr (the actual TCP peer) so an attacker who bypasses
// the proxy can't lie in the access log.
//
// set == nil OR set with no CIDRs loaded yet means "no proxies
// trusted" — equivalent to always returning RemoteAddr. To get the
// legacy permissive behaviour, call ClientIP instead.
func ClientIPTrusted(r *http.Request, set *CIDRSet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if set == nil {
		return host
	}
	if !IsTrustedProxy(net.ParseIP(host), set.Snapshot()) {
		return host
	}
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
	return host
}
