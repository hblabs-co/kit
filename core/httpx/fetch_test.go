package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchURL_Success exercises the happy path: 2xx with a body
// returns Body + correct sha256 + ContentType. The hash is verified
// against an independent sha256 calculation so a regression in the
// streaming hasher would be caught immediately.
func TestFetchURL_Success(t *testing.T) {
	payload := []byte("<html>hello</html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the platform headers reached the upstream — a
		// missing User-Agent or Accept means a regression in
		// FetchURL's request shape.
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("missing User-Agent header")
		}
		if r.Header.Get("Accept") == "" {
			t.Errorf("missing Accept header")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second}),
		FetchOptions{Timeout: 2 * time.Second}, srv.URL)
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(res.Body, payload) {
		t.Errorf("Body = %q, want %q", res.Body, payload)
	}
	sum := sha256.Sum256(payload)
	if res.SHA256Hex != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256Hex = %q, want %q", res.SHA256Hex, hex.EncodeToString(sum[:]))
	}
	if !strings.HasPrefix(res.ContentType, "text/html") {
		t.Errorf("ContentType = %q, want text/html…", res.ContentType)
	}
}

// TestFetchURL_NonSuccessReturnsStatusAndErr covers the documented
// "non-2xx exposes via Err" contract. The caller distinguishes 4xx
// from 5xx in classifyHTTPStatus (downstream); httpx itself just
// signals "not success" with both StatusCode and Err populated so
// the caller can never mistake a 404 for a successful empty body.
func TestFetchURL_NonSuccessReturnsStatusAndErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second}),
		FetchOptions{Timeout: 2 * time.Second}, srv.URL)
	if res.Err == nil {
		t.Fatal("Err = nil, want non-nil for 404")
	}
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", res.StatusCode)
	}
	if res.Body != nil {
		t.Errorf("Body = %v, want nil", res.Body)
	}
}

// TestFetchURL_BodyTooLarge verifies the size cap: a response
// larger than MaxBodySize returns BodyTooLarge = true and DISCARDS
// the partial bytes. A truncated body stored as if it were complete
// would silently break downstream re-parses — the flag-based
// contract is intentionally strict.
func TestFetchURL_BodyTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second}),
		FetchOptions{Timeout: 2 * time.Second, MaxBodySize: 512}, srv.URL)
	if !res.BodyTooLarge {
		t.Errorf("BodyTooLarge = false, want true")
	}
	if res.Body != nil {
		t.Errorf("Body = %v, want nil (must NOT be truncated)", res.Body)
	}
	if res.SHA256Hex != "" {
		t.Errorf("SHA256Hex = %q, want empty when body is discarded", res.SHA256Hex)
	}
}

// TestFetchURL_TransportError covers the unreachable-upstream
// branch: a malformed URL bypasses the round-trip, surfacing only
// Err with StatusCode = 0 so the caller's classifier can map it
// to a transport-level error code.
func TestFetchURL_TransportError(t *testing.T) {
	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 200 * time.Millisecond}),
		FetchOptions{Timeout: 200 * time.Millisecond}, "http://127.0.0.1:1/never-listening")
	if res.Err == nil {
		t.Fatal("Err = nil, want non-nil for unreachable")
	}
	if res.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0", res.StatusCode)
	}
}

// TestFetchURL_BadRequestSurfacesErr exercises the "request build
// failed" branch (an invalid URL the http package refuses to
// construct). Err is populated; nothing else.
func TestFetchURL_BadRequestSurfacesErr(t *testing.T) {
	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{}),
		FetchOptions{}, "://bad")
	if res.Err == nil {
		t.Fatal("Err = nil, want non-nil for bad URL")
	}
}

// TestFetchURL_HeaderOverrides verifies that explicit UserAgent /
// Accept in FetchOptions wins over the package defaults. Stable
// contract: a service can identify itself per-call without
// re-creating the client.
func TestFetchURL_HeaderOverrides(t *testing.T) {
	const wantUA = "TestSuite/1.0"
	const wantAccept = "application/xml"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != wantUA {
			t.Errorf("UA = %q, want %q", got, wantUA)
		}
		if got := r.Header.Get("Accept"); got != wantAccept {
			t.Errorf("Accept = %q, want %q", got, wantAccept)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second}),
		FetchOptions{Timeout: 2 * time.Second, UserAgent: wantUA, Accept: wantAccept}, srv.URL)
}

// TestNewFetchClient_RedirectCap confirms the redirect-loop guard.
// A server that always 302s to itself must produce an error rather
// than spinning forever — the default cap (or a caller-supplied
// one) terminates the chain.
func TestNewFetchClient_RedirectCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second, MaxRedirects: 2}),
		FetchOptions{Timeout: 2 * time.Second}, srv.URL)
	if res.Err == nil {
		t.Fatal("Err = nil, want non-nil for redirect loop")
	}
	if !strings.Contains(res.Err.Error(), "stopped after") {
		t.Errorf("Err = %v, want the redirect-cap message", res.Err)
	}
}

// TestNewFetchClient_NegativeRedirectsDisablesCap verifies the
// documented "negative disables the cap" branch. We don't loop
// forever here — the server only redirects twice and then 200s —
// but with MaxRedirects = -1 the chain completes successfully.
func TestNewFetchClient_NegativeRedirectsDisablesCap(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			http.Redirect(w, r, r.URL.Path+"a", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := FetchURL(context.Background(), NewFetchClient(FetchOptions{Timeout: 2 * time.Second, MaxRedirects: -1}),
		FetchOptions{Timeout: 2 * time.Second}, srv.URL)
	if res.Err != nil {
		t.Errorf("Err = %v, want nil", res.Err)
	}
}

// errReader is a fake io.Reader that errors mid-stream. Used to
// exercise readCappedBody's transport-error branch — a 2xx whose
// body breaks half-way through.
type errReader struct {
	n int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.n > 0 {
		e.n--
		p[0] = 'x'
		return 1, nil
	}
	return 0, errors.New("read failed")
}

// TestReadCappedBody covers each branch of the cap helper
// directly: in-budget body returns hash, exactly-at-cap is fine,
// over-cap signals tooLarge, mid-stream error surfaces as err.
// Keeping the helper unexported but tested via this file lets
// the public FetchURL test stay focused on the request shape.
func TestReadCappedBody(t *testing.T) {
	t.Run("in budget", func(t *testing.T) {
		body, sha, tooLarge, err := readCappedBody(strings.NewReader("hello"), 32)
		if err != nil || tooLarge {
			t.Fatalf("err=%v tooLarge=%v", err, tooLarge)
		}
		want := sha256.Sum256([]byte("hello"))
		if hex.EncodeToString(want[:]) != sha {
			t.Errorf("sha = %q, want %q", sha, hex.EncodeToString(want[:]))
		}
		if string(body) != "hello" {
			t.Errorf("body = %q, want hello", body)
		}
	})
	t.Run("exactly at cap", func(t *testing.T) {
		body, _, tooLarge, err := readCappedBody(strings.NewReader("12345"), 5)
		if err != nil || tooLarge {
			t.Fatalf("err=%v tooLarge=%v", err, tooLarge)
		}
		if string(body) != "12345" {
			t.Errorf("body = %q, want 12345", body)
		}
	})
	t.Run("over cap", func(t *testing.T) {
		_, sha, tooLarge, err := readCappedBody(strings.NewReader("123456"), 5)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !tooLarge {
			t.Errorf("tooLarge = false, want true")
		}
		if sha != "" {
			t.Errorf("sha = %q, want empty", sha)
		}
	})
	t.Run("transport error", func(t *testing.T) {
		_, _, tooLarge, err := readCappedBody(&errReader{n: 3}, 100)
		if err == nil {
			t.Fatal("err = nil, want non-nil")
		}
		if tooLarge {
			t.Errorf("tooLarge = true, want false")
		}
	})
	t.Run("eof is not an error", func(t *testing.T) {
		// io.Copy converts a clean EOF to nil — confirm the
		// helper preserves that contract.
		_, _, tooLarge, err := readCappedBody(io.MultiReader(strings.NewReader("ab"), eofReader{}), 32)
		if err != nil || tooLarge {
			t.Fatalf("err=%v tooLarge=%v", err, tooLarge)
		}
	})
}

// eofReader returns EOF immediately. Composed with strings.NewReader
// in the test above to simulate "body ended cleanly mid-stream"
// without involving a network round-trip.
type eofReader struct{}

func (eofReader) Read(_ []byte) (int, error) { return 0, io.EOF }
