package httpx

import (
	"net/http"
	"runtime/debug"

	"github.com/hblabs-co/kit/core/log"
)

// Recover catches panics from any downstream handler, logs them
// with a full stack trace using the per-request logger from the
// context (so request_id / device_id are attached automatically),
// and returns 500 with a generic body.
//
// Without this middleware a panic in any handler would unwind the
// goroutine and leave the client with a closed connection or a
// torn HTTP/2 stream — symptomatically identical to a TCP reset
// from the operator's side and miserable to debug.
//
// Place this innermost in the chain (just outside the application
// handler) so the access log middleware above sees a normal 500
// response and still records the entry. If Recover were outside
// httplog, a panicked request would never reach LogAttrs and we'd
// lose the access log line for the worst kind of bug.
//
// The response body is generic ("Internal Server Error") on
// purpose: leaking the panic message or the stack to the client
// helps attackers fingerprint internal layout. Detail is in the
// log already — that's where it belongs.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented sentinel for
			// "abort this handler without logging" (used by net/http
			// itself for client disconnects). Re-panic so the server
			// suppresses the log just like any other handler that
			// raised it directly.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.LoggerFromContext(r.Context()).Error("panic in handler",
				"panic", rec,
				"stack", string(debug.Stack()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
