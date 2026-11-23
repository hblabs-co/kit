package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/secheaders"
)

// TestNewHandler_FullCanonicalChain — the canonical 4 layers
// (Secure → RequestID → WithoutLogging → WithRecover) actually
// wrap the handler in declared order: every layer's side-effect
// must be visible on the response, and the inner handler runs.
// Asserts secheaders set its CSP header, RequestID echoed
// X-Request-Id, the inner handler ran, and WithRecover passed
// through without intercepting the success path.
func TestNewHandler_FullCanonicalChain(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHandler().
		Secure().
		RequestID().
		WithoutLogging("/healthz").
		WithRecover().
		Merge(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Error("inner handler never ran")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Secure() did not install CSP header")
	}
	if rec.Header().Get(HeaderRequestID) == "" {
		t.Error("RequestID() did not echo X-Request-Id")
	}
}

// TestNewHandler_WithRecoverCatchesPanic — WithRecover must be a
// real recovery layer, not just an entry in the chain. Drive a
// panic from the inner handler and verify the response is a 500,
// not a crashed test process.
func TestNewHandler_WithRecoverCatchesPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := NewHandler().WithRecover().Merge(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (panic should be caught)", rec.Code)
	}
}

// TestNewHandler_OptionalArgsApplied — every variadic method has
// a "no args = sane default" path AND a "one arg = explicit
// override" path. Drive both for Secure and RequestID by passing
// distinguishable values and asserting they reached the wrapped
// middleware.
func TestNewHandler_OptionalArgsApplied(t *testing.T) {
	// Secure with ReportOnly flips the CSP header name. If the
	// override didn't take, the response would carry the regular
	// "Content-Security-Policy" header instead.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHandler().
		Secure(secheaders.Options{ReportOnly: true}).
		Merge(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Security-Policy-Report-Only") == "" {
		t.Error("Secure(ReportOnly) didn't reach secheaders")
	}

	// RequestID with an explicit nil base logger: confirms the
	// non-default branch (len(base) > 0) runs without panicking.
	handler2 := NewHandler().RequestID(nil).Merge(inner)
	rec = httptest.NewRecorder()
	handler2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get(HeaderRequestID) == "" {
		t.Error("RequestID(nil) did not echo header")
	}
}

// TestNewHandler_AccessLogFullOptions — AccessLog(opts) is the
// escape hatch when WithoutLogging's recipe doesn't fit (custom
// Level fn, TrustedProxies, custom ContextAttrs). Verify it
// composes the same way: opts threaded through, response served.
func TestNewHandler_AccessLogFullOptions(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHandler().
		AccessLog(AccessLogOptions{Level: QuietPaths()}).
		Merge(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestNewHandler_UseAppendsCustomMiddleware — Use is the escape
// hatch for non-canonical middleware. Drive a custom one that
// stamps a marker header so we can assert it actually ran.
func TestNewHandler_UseAppendsCustomMiddleware(t *testing.T) {
	custom := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "yes")
			next.ServeHTTP(w, r)
		})
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHandler().Use(custom).Merge(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("X-Custom") != "yes" {
		t.Error("Use(custom) did not run the supplied middleware")
	}
}

// TestNewHandler_OrderOuterToInner — locks the documented order
// guarantee: the FIRST method called wraps the rest. Drive two
// trace-collecting middlewares and assert the outer one's
// "enter" fires before the inner's, exit in reverse.
func TestNewHandler_OrderOuterToInner(t *testing.T) {
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

	handler := NewHandler().Use(mark("A")).Use(mark("B")).Merge(inner)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "A:enter,B:enter,handler,B:exit,A:exit"
	if got := strings.Join(trace, ","); got != want {
		t.Errorf("trace = %s\nwant   %s", got, want)
	}
}
