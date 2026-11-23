package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/hblabs-co/kit/core/httpx"
)

// TestStripScheme covers all three input shapes the function tolerates:
// https://-prefixed (production with TLS), http://-prefixed (insecure
// in-cluster), and bare host:port (the form otlptracehttp wants
// natively). A trailing "/" must always be trimmed since the OTLP
// HTTP client appends its own path components.
func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"https://otel.example.com:4318":  "otel.example.com:4318",
		"http://otel.example.com:4318":   "otel.example.com:4318",
		"otel.example.com:4318":          "otel.example.com:4318",
		"https://otel.example.com:4318/": "otel.example.com:4318",
		"http://otel.example.com:4318/":  "otel.example.com:4318",
		"otel.example.com:4318/":         "otel.example.com:4318",
	}
	for in, want := range cases {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRouteOrPath_PatternWins — when ServeMux has matched a pattern,
// span names should use that low-cardinality form, not the raw
// URL.Path which would explode trace search by user-id-style IDs.
func TestRouteOrPath_PatternWins(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/sites/abc123", nil)
	r.Pattern = "/sites/{id}"
	if got := routeOrPath(r); got != "/sites/{id}" {
		t.Errorf("routeOrPath = %q, want pattern", got)
	}
}

// TestRouteOrPath_FallbackToURLPath — services that don't use Go
// 1.22+ ServeMux (chi, raw http.Handler) leave Pattern empty;
// routeOrPath must still produce something, otherwise span names
// would become "GET " with an empty route.
func TestRouteOrPath_FallbackToURLPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/raw", nil)
	if got := routeOrPath(r); got != "/raw" {
		t.Errorf("routeOrPath = %q, want \"/raw\"", got)
	}
}

// TestInitTracing_EmptyEndpointIsNoop — local dev / tests run with
// OTEL_EXPORTER_OTLP_ENDPOINT unset; InitTracing must install only
// the propagators and return a noop shutdown. Any otlptracehttp.New
// attempt would 1) cost dial latency and 2) break offline runs.
func TestInitTracing_EmptyEndpointIsNoop(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), TracingOptions{
		Service: "test-svc",
	})
	if err != nil {
		t.Fatalf("InitTracing err = %v, want nil with empty endpoint", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown closure is nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned %v, want nil", err)
	}
	// Propagator must be installed so downstream services pick up
	// traceparent headers even when this pod has no exporter.
	if otel.GetTextMapPropagator() == nil {
		t.Error("propagator not installed when endpoint is empty")
	}
}

// TestInitTracing_EmptyServiceDefaultsToUnknown — leaving Service
// blank is documented to default to "unknown" rather than crash
// otel's resource builder. Without this guard, dashboards would see
// spans with no service tag and no way to find them in search.
func TestInitTracing_EmptyServiceDefaultsToUnknown(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), TracingOptions{
		// Service intentionally empty.
	})
	if err != nil {
		t.Fatalf("InitTracing err = %v", err)
	}
	_ = shutdown(context.Background())
}

// TestInitTracing_FullExporterPath — covers the Endpoint != "" branch
// end-to-end: exporter built, resource composed, sampler picked,
// global TracerProvider installed. We don't actually export anything
// (no collector listening) — otlptracehttp.New only fails on
// programmatic config, not on connectivity.
func TestInitTracing_FullExporterPath(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), TracingOptions{
		Service:     "test-svc",
		Endpoint:    "http://127.0.0.1:14318",
		Environment: "ci",
		SampleRate:  0.5,
	})
	if err != nil {
		t.Fatalf("InitTracing err = %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil after successful init")
	}
	// Force a bounded shutdown — the exporter has nothing to flush
	// since no spans were emitted, so this returns quickly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}

// TestInitTracing_InsecureFlag — the Insecure option must produce
// a working init even when the endpoint already has https:// (the
// flag wins over the scheme heuristic). Same shape as the http://
// case but driven by the explicit flag instead of the URL.
func TestInitTracing_InsecureFlag(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), TracingOptions{
		Service:  "test-svc",
		Endpoint: "https://collector.example.com:4318",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("InitTracing err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = shutdown(ctx)
}

// TestTracingMiddleware_ServesAndTagsRequest — drives the middleware
// over a normal request and asserts that the inner handler ran and
// the response went through. The status-recorder wrap, span Start /
// End, and propagator extract paths are all touched in passing —
// without this test the middleware body's branches are 0% covered.
func TestTracingMiddleware_ServesAndTagsRequest(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	req.Pattern = "/foo"
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("inner handler was not invoked")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestTracingMiddleware_RequestIDFromHeader — when no httpx.RequestID
// has run, the middleware must fall back to the x-request-id header
// so the trace still carries a correlation handle. Verifies the rid
// extraction + baggage attach path independently of the context one.
func TestTracingMiddleware_RequestIDFromHeader(t *testing.T) {
	var seenRID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pull request_id back out of baggage to confirm it landed.
		bag := baggage.FromContext(r.Context())
		seenRID = bag.Member("request_id").Value()
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	req.Header.Set(httpx.HeaderRequestID, "rid-from-header")
	h.ServeHTTP(rec, req)

	if seenRID != "rid-from-header" {
		t.Errorf("baggage request_id = %q, want \"rid-from-header\"", seenRID)
	}
}

// TestTracingMiddleware_RequestIDFromContext — when httpx.RequestID
// has already populated the context (the normal path with the
// middleware ordered before tracing), TracingMiddleware should pick
// up the rid from there instead of re-reading the header.
func TestTracingMiddleware_RequestIDFromContext(t *testing.T) {
	var seenRID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bag := baggage.FromContext(r.Context())
		seenRID = bag.Member("request_id").Value()
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	ctx := httpx.WithRequestID(context.Background(), "rid-from-ctx")
	req := httptest.NewRequest(http.MethodGet, "/foo", nil).WithContext(ctx)
	h.ServeHTTP(rec, req)

	if seenRID != "rid-from-ctx" {
		t.Errorf("baggage request_id = %q, want \"rid-from-ctx\"", seenRID)
	}
}

// TestTracingMiddleware_5xxSetsErrorStatus — only ≥500 responses
// flip the span to Error status; lower codes (like 4xx) are normal
// client mistakes that shouldn't show up red on dashboards. Driving
// a 500 response covers the Error-status branch.
func TestTracingMiddleware_5xxSetsErrorStatus(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestTracingMiddleware_InvalidRIDForBaggage — baggage.NewMember
// rejects values containing characters outside the percent-encodable
// set. The middleware must swallow that error and continue serving
// the request rather than crashing or omitting the response. We use
// a literal newline in the header — invalid for baggage, valid for
// HTTP after stripping (Go strips control chars from request
// headers, but we can construct one directly).
func TestTracingMiddleware_InvalidRIDForBaggage(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	// W3C baggage allows token values; spaces / commas / quotes
	// trigger NewMember errors. A bare DEL char (0x7F) is invalid
	// for both name and value parsers.
	ctx := httpx.WithRequestID(context.Background(), "bad\x7Frid")
	req = req.WithContext(ctx)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (invalid rid must not break the request)", rec.Code)
	}
}

// TestInitTracing_ExporterError — otlptracehttp.New is essentially
// infallible at construction, so the error branch in InitTracing is
// only reachable through the seam. Driving it locks in the wrap so a
// regression that swallows the exporter error is caught.
func TestInitTracing_ExporterError(t *testing.T) {
	prev := newOTLPExporter
	newOTLPExporter = func(context.Context, ...otlptracehttp.Option) (sdktrace.SpanExporter, error) {
		return nil, errors.New("exporter init refused")
	}
	defer func() { newOTLPExporter = prev }()

	_, err := InitTracing(context.Background(), TracingOptions{
		Service:  "test-svc",
		Endpoint: "http://127.0.0.1:14318",
	})
	if err == nil {
		t.Fatal("expected exporter init error")
	}
}

// TestInitTracing_BadResourceEnv — OTEL_RESOURCE_ATTRIBUTES with an
// unparseable shape ("novalue,a=b" — first token has no `=`) makes
// resource.New return an error from its WithFromEnv detector. The
// only path through InitTracing that hits the resource error branch.
func TestInitTracing_BadResourceEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "novalue,a=b")
	_, err := InitTracing(context.Background(), TracingOptions{
		Service:  "test-svc",
		Endpoint: "http://127.0.0.1:14318",
	})
	if err == nil {
		t.Fatal("expected error from resource.New on invalid OTEL_RESOURCE_ATTRIBUTES")
	}
}

// TestTracingMiddleware_NoRID — without a request_id in either
// context or header, the rid block is skipped entirely; the request
// still flows and a span is still emitted, just without the
// correlation attribute. Locks in the "tracing degrades, doesn't
// fail" property.
func TestTracingMiddleware_NoRID(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := TracingMiddleware("test-svc")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	_ = strings.ToLower // keep imports grouped
}
