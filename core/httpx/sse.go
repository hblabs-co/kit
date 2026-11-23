package httpx

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// defaultKeepAlive is how often each connection emits a comment line
// to keep proxies and intermediaries from reaping idle SSE streams.
// 25s is below the typical 30s / 60s cutoff of nginx, ELBs, and most
// browser-side keepalive expectations.
const defaultKeepAlive = 25 * time.Second

// subscriberChanBuffer sizes each subscriber's wake-up channel.
// Buffered so a Broadcast() that fires while we're between Flush()
// and the next select doesn't block the broadcaster on a single slow
// reader. Only the latest event is preserved per subscriber — this
// hub doesn't promise delivery, only liveness.
const subscriberChanBuffer = 4

// SSEHub is a tiny in-process broadcaster: HTTP subscribers receive a
// Server-Sent Event whenever Broadcast() is called. No auth, no
// buffering of missed events, no per-subscriber state — designed to
// fan a single signal to a handful of connected browsers / agents
// without any back-pressure on the broadcaster.
//
// Typical consumers: dev hot-reload (render/dev), admin live tailing
// of in-memory state, demo dashboards. Anything that needs guaranteed
// delivery, replay, or fan-in across pods should use NATS instead;
// SSEHub is intentionally minimal.
type SSEHub struct {
	// keepAlive is the cadence used by this hub — see defaultKeepAlive
	// for the rationale.
	keepAlive time.Duration

	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

// NewSSEHub returns an SSEHub with the default keep-alive — fine for
// any reasonable setup. Override the cadence with NewSSEHubWithKeepAlive
// when running behind a proxy with a shorter idle timeout, or pass 0
// to disable keep-alive entirely.
func NewSSEHub() *SSEHub {
	return NewSSEHubWithKeepAlive(defaultKeepAlive)
}

// NewSSEHubWithKeepAlive lets the caller override the keep-alive
// cadence. Pass 0 to disable keep-alive entirely (useful when the
// hub sits behind a proxy that already sends its own keepalives).
func NewSSEHubWithKeepAlive(keepAlive time.Duration) *SSEHub {
	return &SSEHub{
		keepAlive:   keepAlive,
		subscribers: map[chan struct{}]struct{}{},
	}
}

// Broadcast wakes every connected subscriber. Non-blocking — a slow
// subscriber whose channel is already full simply misses this event,
// which is fine for the use cases this hub targets: the next event
// catches up. SSEHub doesn't promise delivery, only liveness.
func (h *SSEHub) Broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ServeSSE returns an http.HandlerFunc that streams Broadcast events
// to subscribers as Server-Sent Events. eventName is the value
// written in `event:` lines (the browser ignores it for plain
// `onmessage`, but it's useful when filtering on event types).
func (h *SSEHub) ServeSSE(eventName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set(constants.HeaderContentType, "text/event-stream")
		w.Header().Set(constants.HeaderCacheControl, "no-cache, no-store, must-revalidate")
		w.Header().Set(constants.HeaderConnection, "keep-alive")
		w.Header().Set(constants.HeaderXAccelBuffering, "no") // disable nginx response buffering

		ch := make(chan struct{}, subscriberChanBuffer)
		h.mu.Lock()
		h.subscribers[ch] = struct{}{}
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			h.mu.Unlock()
		}()

		// Send an initial comment so the connection establishes
		// without waiting for the first real event — useful for
		// debugging "is the SSE wired up at all".
		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		var keepAliveC <-chan time.Time
		if h.keepAlive > 0 {
			t := time.NewTicker(h.keepAlive)
			defer t.Stop()
			keepAliveC = t.C
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				fmt.Fprintf(w, "event: %s\ndata: reload\n\n", eventName)
				flusher.Flush()
			case <-keepAliveC:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}
