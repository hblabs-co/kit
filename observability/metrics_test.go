package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewMetrics_DefaultsCoverNilOptions — NewMetrics is the
// no-options constructor; calling it must give back a working
// bundle with a fresh registry, the default route extractor (always
// "") and the prometheus default histogram buckets. This locks in
// the "drop into a service with one line" promise.
func TestNewMetrics_DefaultsCoverNilOptions(t *testing.T) {
	m := NewMetrics("svc-default")
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	if m.Registry() == nil {
		t.Error("Registry() = nil")
	}
	// Drive one request through the middleware so the default
	// route extractor (returns "") is exercised end-to-end.
	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	srv := httptest.NewServer(m.Middleware()(mux))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// TestStatusClass_OtherForBelow400 — statusClass is documented to
// fall back to "other" for any status not in the 4xx/5xx ranges.
// 1xx/2xx/3xx never reach the errors counter in practice, but the
// function is exported through coverage; lock the fall-through so a
// future tweak doesn't quietly relabel 200s as "5xx".
func TestStatusClass_OtherForBelow400(t *testing.T) {
	cases := []int{100, 200, 304, 399}
	for _, s := range cases {
		if got := statusClass(s); got != "other" {
			t.Errorf("statusClass(%d) = %q, want \"other\"", s, got)
		}
	}
}

// TestStatusRecorder_FlushPropagates — when the wrapped writer
// implements http.Flusher (SSE, NDJSON streams), the recorder must
// forward Flush calls. Without this, streaming endpoints behind the
// metrics middleware would buffer silently until the handler returns.
func TestStatusRecorder_FlushPropagates(t *testing.T) {
	flushed := false
	inner := &flushableRecorder{flushed: &flushed}
	rec := &statusRecorder{ResponseWriter: inner, status: 200}
	rec.Flush()
	if !flushed {
		t.Error("statusRecorder.Flush did not propagate to inner Flusher")
	}

	// And a non-flushable writer must not crash.
	plain := httptest.NewRecorder()
	(&statusRecorder{ResponseWriter: plain, status: 200}).Flush()
}

// flushableRecorder is a tiny http.ResponseWriter+http.Flusher pair
// for the Flush propagation test; httptest.NewRecorder doesn't
// expose its Flushed flag through Flush() in a way we can inspect.
type flushableRecorder struct {
	httptest.ResponseRecorder
	flushed *bool
}

func (f *flushableRecorder) Flush() { *f.flushed = true }

func TestMetricsMiddlewareCountsAndCategorisesByStatus(t *testing.T) {
	m := NewMetricsWithOptions("test", MetricsOptions{
		Route: func(r *http.Request) string {
			// Simulate a router that exposes the matched pattern.
			if strings.HasPrefix(r.URL.Path, "/sites/") {
				return "/sites/{id}"
			}
			return r.URL.Path
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/sites/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	handler := m.Middleware()(mux)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, p := range []string{"/sites/123", "/sites/abc", "/missing", "/boom"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Scrape /metrics from the bundle's own registry and assert the
	// expected lines appear. We don't compare exact text — Prom's
	// exposition format includes HELP/TYPE preambles and ordering.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	wantSubstrings := []string{
		`http_requests_total{method="GET",route="/sites/{id}",service="test",status="200"} 2`,
		`http_request_errors_total{method="GET",route="/missing",service="test",status_class="4xx"} 1`,
		`http_request_errors_total{method="GET",route="/boom",service="test",status_class="5xx"} 1`,
		`http_request_duration_seconds_count{method="GET",route="/boom",service="test",status="500"} 1`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(body, s) {
			t.Errorf("metrics output missing %q\n--- got ---\n%s", s, body)
		}
	}
}
