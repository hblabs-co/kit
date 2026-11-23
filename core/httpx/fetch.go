package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultFetchUserAgent identifies the platform's outbound fetches
// to upstream operators inspecting their access logs. Embeds an
// https://hblabs.co URL so a curious admin can find the platform
// without guessing. Override per-call via FetchOptions.UserAgent
// when a service has a more specific identity to advertise (e.g.
// the service-ingest crawler).
const DefaultFetchUserAgent = "HB Labs Fetch/1.0 (+https://hblabs.co)"

// DefaultFetchAccept matches the Accept header a normal browser
// sends. Generic on purpose: a sitemap (application/xml), a
// robots.txt (text/plain), and a page (text/html) all flow
// through one call site, and a server that Vary's by Accept
// returning 406 for an unfamiliar value defeats the purpose.
const DefaultFetchAccept = "text/html,application/xml,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8"

// defaultFetchMaxRedirects caps the redirect chain a single GET
// will follow. Stops a "site redirects to itself with a session-
// id" loop without failing legitimate 1-2 hop redirects to the
// canonical host. Exposed via FetchOptions.MaxRedirects for
// callers that need a different ceiling.
const defaultFetchMaxRedirects = 5

// defaultFetchMaxBodySize is the fallback per-response cap in
// bytes when FetchOptions.MaxBodySize is zero. Sized for typical
// HTML pages and sitemaps; consumers that pull bigger artefacts
// override at the call site.
const defaultFetchMaxBodySize = 5 * 1024 * 1024

// FetchOptions configures NewFetchClient and FetchURL. Zero
// values pick safe defaults (DefaultFetchUserAgent, DefaultFetchAccept,
// defaultFetchMaxRedirects, defaultFetchMaxBodySize) so a caller
// can pass FetchOptions{Timeout: 15 * time.Second} and get the
// rest for free.
type FetchOptions struct {
	// Timeout caps the total time per request (DNS → headers →
	// body). Zero leaves the http.Client default (no timeout) —
	// almost never what you want for an outbound crawl, so pass
	// an explicit value at the call site.
	Timeout time.Duration

	// MaxBodySize caps the response body in bytes. A response
	// larger than the cap returns FetchResult.BodyTooLarge = true
	// with Body = nil; the partial download is discarded so a
	// later re-fetch with a higher cap remains meaningful.
	MaxBodySize int64

	// UserAgent overrides the User-Agent header. Empty falls back
	// to DefaultFetchUserAgent.
	UserAgent string

	// Accept overrides the Accept header. Empty falls back to
	// DefaultFetchAccept (a browser-shaped wildcard).
	Accept string

	// MaxRedirects overrides the redirect-chain cap. Zero falls
	// back to defaultFetchMaxRedirects. Negative disables the
	// cap (use carefully — a hostile upstream can loop forever).
	MaxRedirects int
}

// FetchResult is the typed outcome of one FetchURL call. The
// caller drives behaviour off the fields rather than re-inspecting
// the *http.Response — the response body is already consumed and
// the connection closed by the time FetchURL returns.
//
// Classification is the caller's job: this package returns
// (Err, StatusCode, BodyTooLarge) as neutral signals and lets
// the caller map them to its own closed vocabulary of error
// codes. That keeps httpx free of any one service's error
// taxonomy while still funnelling every fetch through one
// implementation.
type FetchResult struct {
	// StatusCode is the HTTP status the upstream returned. Zero
	// when the request never reached a response (transport error
	// — see Err).
	StatusCode int

	// ContentType is the value of the upstream's Content-Type
	// header, verbatim. Empty when no header was returned.
	ContentType string

	// Body is the response bytes, present only when the upstream
	// returned a 2xx and the body fit under MaxBodySize. Nil in
	// every other case (non-2xx, transport error, BodyTooLarge).
	Body []byte

	// SHA256Hex is the hex-encoded sha256 of Body. Computed
	// inline while reading the body so callers that need it for
	// content addressing don't pay a second pass. Empty when
	// Body is nil.
	SHA256Hex string

	// BodyTooLarge is true when the upstream's response exceeded
	// MaxBodySize. Surfaced as a flag (not an error) so the
	// caller's classifier can distinguish "too big" from "broke"
	// without parsing error messages.
	BodyTooLarge bool

	// Err is the raw transport-layer error when the request did
	// not produce a 2xx-with-body result. Nil on success. The
	// caller is expected to classify it (timeout, DNS, TLS, ...)
	// using its own vocabulary.
	Err error
}

// NewFetchClient builds a goroutine-safe *http.Client tuned for
// one-shot outbound GETs with a bounded redirect chain. The
// Timeout is baked into the client (cleanest cancellation
// guarantee across all sub-stages); the User-Agent / Accept
// headers are NOT — those are applied per request inside
// FetchURL so a test can override one call without rewiring the
// client.
func NewFetchClient(opts FetchOptions) *http.Client {
	maxRedirects := opts.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = defaultFetchMaxRedirects
	}
	return &http.Client{
		Timeout: opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if maxRedirects >= 0 && len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// FetchURL performs one bounded HTTP GET against url and returns
// a FetchResult. The caller-supplied client is usually built by
// NewFetchClient; reusing the same client across many calls
// amortises TLS handshake and connection-pool cost.
//
// Returns:
//
//   - On transport error (DNS, TLS, timeout, ...): FetchResult{Err: ...}
//     with StatusCode = 0 and Body = nil. The caller classifies
//     Err however it wants.
//   - On non-2xx with no body limit hit: FetchResult{StatusCode: code,
//     ContentType: ..., Body: nil, Err: an error describing the
//     status}. The caller decides whether 4xx and 5xx mean
//     different things.
//   - On 2xx with body over MaxBodySize: FetchResult{
//     BodyTooLarge: true, Body: nil}. The partial download is
//     discarded.
//   - On 2xx success: FetchResult{StatusCode: 2xx, Body: bytes,
//     SHA256Hex: hex, ContentType: ...}.
func FetchURL(ctx context.Context, client *http.Client, opts FetchOptions, url string) FetchResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchResult{Err: err}
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultFetchUserAgent
	}
	req.Header.Set("User-Agent", ua)
	accept := opts.Accept
	if accept == "" {
		accept = DefaultFetchAccept
	}
	req.Header.Set("Accept", accept)

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchResult{
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Err:         fmt.Errorf("http %d", resp.StatusCode),
		}
	}

	cap := opts.MaxBodySize
	if cap <= 0 {
		cap = defaultFetchMaxBodySize
	}
	body, sha, tooLarge, err := readCappedBody(resp.Body, cap)
	if err != nil {
		return FetchResult{
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Err:         err,
		}
	}
	if tooLarge {
		return FetchResult{
			StatusCode:   resp.StatusCode,
			ContentType:  resp.Header.Get("Content-Type"),
			BodyTooLarge: true,
		}
	}
	return FetchResult{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		SHA256Hex:   sha,
	}
}

// readCappedBody reads from r into memory up to (cap+1) bytes,
// computing the running sha256 in parallel. Returns:
//
//   - body bytes (nil if over the cap)
//   - sha256 hex (computed over the bytes read; "" if over the cap,
//     since we discard the partial)
//   - tooLarge = true when the reader still had bytes after the
//     cap was reached
//   - err = the first transport-layer read error (other than EOF)
//
// Reading cap+1 lets us tell "exactly at the cap, fine" from "the
// cap was hit and there's more". A naive io.LimitReader can't:
// it would silently truncate.
func readCappedBody(r io.Reader, cap int64) ([]byte, string, bool, error) {
	hasher := sha256.New()
	var buf bytes.Buffer
	limited := &io.LimitedReader{R: r, N: cap + 1}
	tee := io.TeeReader(limited, hasher)
	n, err := io.Copy(&buf, tee)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", false, err
	}
	if n > cap {
		return nil, "", true, nil
	}
	return buf.Bytes(), hex.EncodeToString(hasher.Sum(nil)), false, nil
}
