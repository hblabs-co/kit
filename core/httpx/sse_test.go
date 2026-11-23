package httpx

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// flushWriter is the minimal http.ResponseWriter+http.Flusher we
// need to drive ServeSSE in tests. httptest.ResponseRecorder does
// implement Flush() (it just toggles a bool), but it neither counts
// flushes nor lets us hook a callback on each one — both of which
// the SSE tests need to coordinate cancellation deterministically
// without sleeping. flushWriter adds exactly that: a counter and
// an optional fire-after-flush callback, all behind a mutex so the
// race detector stays clean.
type flushWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	header  http.Header
	flushes int
	status  int
	flushFn func()
}

func newFlushWriter() *flushWriter {
	return &flushWriter{header: http.Header{}, status: http.StatusOK}
}

func (f *flushWriter) Header() http.Header { return f.header }

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *flushWriter) WriteHeader(code int) { f.status = code }

func (f *flushWriter) Flush() {
	f.mu.Lock()
	f.flushes++
	cb := f.flushFn
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (f *flushWriter) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

func (f *flushWriter) Flushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// TestNewSSEHub_Defaults — the no-arg constructor must produce a
// hub with the documented 25s keep-alive (the value tuned for
// nginx / ELB idle cutoffs). Nailing this in a test means a
// well-meaning refactor can't silently lower it to a value that
// would let connections die behind common proxies.
func TestNewSSEHub_Defaults(t *testing.T) {
	h := NewSSEHub()
	if h.keepAlive != 25*time.Second {
		t.Errorf("keepAlive = %v, want 25s", h.keepAlive)
	}
	if h.subscribers == nil {
		t.Error("subscribers map not initialised")
	}
}

// TestNewSSEHubWithKeepAlive_ZeroDisables — passing 0 disables
// keep-alive entirely. Used by deployments where the proxy already
// emits its own keepalives.
func TestNewSSEHubWithKeepAlive_ZeroDisables(t *testing.T) {
	h := NewSSEHubWithKeepAlive(0)
	if h.keepAlive != 0 {
		t.Errorf("keepAlive = %v, want 0", h.keepAlive)
	}
}

// TestSSEHub_Broadcast_NoSubscribers — broadcasting against an
// empty hub must be a clean no-op. The hub doesn't enqueue
// anything, doesn't panic, and returns immediately. Important for
// services that keep the hub idle most of the time.
func TestSSEHub_Broadcast_NoSubscribers(t *testing.T) {
	h := NewSSEHub()
	h.Broadcast() // must not panic / block
}

// TestSSEHub_ServeSSE_StreamingNotSupported — an http.ResponseWriter
// that doesn't implement http.Flusher must produce a 500 with the
// documented body, NOT a panic from a bad type assertion. Note we
// CAN'T use httptest.ResponseRecorder here: it implements Flush()
// itself, so the type assertion would succeed. The plain wrapper
// drops Flush() so the "ok" branch fails as intended.
func TestSSEHub_ServeSSE_StreamingNotSupported(t *testing.T) {
	h := NewSSEHub()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.ServeSSE("reload")(noFlusher{rec}, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "streaming not supported") {
		t.Errorf("body = %q, want \"streaming not supported\"", rec.Body.String())
	}
}

// noFlusher wraps a ResponseWriter and intentionally does NOT
// expose Flush — promoted methods from the embedded type would,
// so we wrap by composition (not embedding) and re-export only
// Header/Write/WriteHeader.
type noFlusher struct{ inner http.ResponseWriter }

func (n noFlusher) Header() http.Header         { return n.inner.Header() }
func (n noFlusher) Write(p []byte) (int, error) { return n.inner.Write(p) }
func (n noFlusher) WriteHeader(c int)           { n.inner.WriteHeader(c) }

// TestSSEHub_ServeSSE_DeliversBroadcast — the main happy path: a
// subscriber connects, gets the initial connect comment, receives
// a Broadcast event, and exits cleanly when its context is
// cancelled. The data line shape (`event: …\ndata: reload\n\n`) is
// the wire contract clients depend on.
func TestSSEHub_ServeSSE_DeliversBroadcast(t *testing.T) {
	h := NewSSEHubWithKeepAlive(0) // disable timer noise
	fw := newFlushWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(ctx)

	// Cancel as soon as we've seen the broadcast flush. The handler
	// flushes once on connect (1) and once after every event (2).
	fw.flushFn = func() {
		if fw.Flushes() >= 2 {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		h.ServeSSE("reload")(fw, req)
		close(done)
	}()

	// Wait for the initial connect flush so the subscriber is
	// registered before we Broadcast — otherwise the broadcast
	// races with subscribers map insertion.
	deadline := time.Now().Add(2 * time.Second)
	for fw.Flushes() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.Broadcast()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE never returned after ctx cancel")
	}

	got := fw.String()
	if !strings.Contains(got, ": connected") {
		t.Errorf("missing connect comment\n%s", got)
	}
	if !strings.Contains(got, "event: reload\ndata: reload\n\n") {
		t.Errorf("missing event line\n%s", got)
	}
	// SSE wire headers — clients (and proxies) rely on these.
	if got := fw.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := fw.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\"", got)
	}
}

// TestSSEHub_ServeSSE_KeepAliveTicks — with a short keep-alive the
// handler must emit `: keepalive` comment lines on the configured
// cadence. Pin both the wire format and the fact that the ticker
// branch is reachable.
func TestSSEHub_ServeSSE_KeepAliveTicks(t *testing.T) {
	h := NewSSEHubWithKeepAlive(20 * time.Millisecond)
	fw := newFlushWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(ctx)

	// After the connect flush + at least one keepalive flush, exit.
	fw.flushFn = func() {
		if fw.Flushes() >= 2 && strings.Contains(fw.String(), ": keepalive") {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		h.ServeSSE("reload")(fw, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE never returned; keepalive branch likely not reached")
	}

	if !strings.Contains(fw.String(), ": keepalive") {
		t.Errorf("missing keepalive line\n%s", fw.String())
	}
}

// TestSSEHub_ServeSSE_ContextDoneExits — the ctx-cancelled branch
// of the select must terminate the handler promptly. Without it,
// closing the client connection would leak a goroutine per
// previously-connected browser.
func TestSSEHub_ServeSSE_ContextDoneExits(t *testing.T) {
	h := NewSSEHubWithKeepAlive(0)
	fw := newFlushWriter()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		h.ServeSSE("reload")(fw, req)
		close(done)
	}()

	// Wait for the initial connect flush so the goroutine is past
	// the select-setup and inside the for/select; cancelling
	// before then could exit on the wrong branch.
	deadline := time.Now().Add(time.Second)
	for fw.Flushes() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeSSE did not exit on ctx.Done")
	}
}

// TestSSEHub_Broadcast_DropsOnFullChannel — the documented "no
// back-pressure" guarantee: a subscriber whose channel is full
// must NOT block the broadcaster. We saturate a subscriber's
// channel by inserting it into the hub's map directly, then
// confirm that Broadcast returns promptly without blocking.
//
// Touching the unexported map and mutex is acceptable because the
// test lives in the same package; the alternative (driving it
// through ServeSSE) couldn't reliably reproduce a full channel
// without races.
func TestSSEHub_Broadcast_DropsOnFullChannel(t *testing.T) {
	h := NewSSEHubWithKeepAlive(0)
	full := make(chan struct{}, 1)
	full <- struct{}{} // saturate

	h.mu.Lock()
	h.subscribers[full] = struct{}{}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.Broadcast()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast blocked on a full subscriber channel; the non-blocking select branch regressed")
	}
}
