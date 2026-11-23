package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChain_NoMiddlewareReturnsHandler — the empty case is the
// identity composition. Pinning it means a feature flag that
// strips every middleware doesn't accidentally drop requests on
// the floor — the inner handler still runs.
func TestChain_NoMiddlewareReturnsHandler(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("inner"))
	})
	got := Chain().Then(inner)
	rec := httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Body.String() != "inner" {
		t.Errorf("body = %q, want \"inner\" (empty chain must be identity)", rec.Body.String())
	}
}

// TestChain_OrderOuterWrapsInner — the FIRST middleware listed is
// the outermost: it logs/records a marker BEFORE the second one
// sees the request, and AFTER on the way out. We assemble the
// trace by appending markers as each layer enters and exits.
func TestChain_OrderOuterWrapsInner(t *testing.T) {
	var trace []string
	mark := func(label string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace = append(trace, label+":enter")
				next.ServeHTTP(w, r)
				trace = append(trace, label+":exit")
			})
		}
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
	})
	h := Chain(mark("A"), mark("B"), mark("C")).Then(inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "A:enter,B:enter,C:enter,handler,C:exit,B:exit,A:exit"
	if got := strings.Join(trace, ","); got != want {
		t.Errorf("trace = %s\nwant   %s", got, want)
	}
}

// TestChain_SingleMiddleware — degenerate case: one middleware
// wraps the handler exactly the same way calling it directly
// would. Locks the API so callers can build the chain
// programmatically (slice → Chain(slice...)) without a special
// case for one element.
func TestChain_SingleMiddleware(t *testing.T) {
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Wrapped", "yes")
			next.ServeHTTP(w, r)
		})
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Chain(mw).Then(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Wrapped"); got != "yes" {
		t.Errorf("X-Wrapped = %q, want \"yes\"", got)
	}
}
