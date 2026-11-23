package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTP status thresholds for the errors counter. Mirrors the textbook
// definitions of 4xx (client error) and 5xx (server error) so the
// status_class label stays bounded while alerts can still distinguish
// "we are broken" (5xx) from "the caller misbehaved" (4xx).
const (
	statusClientErrorMin = 400
	statusServerErrorMin = 500
)

// statusClass labels emitted by statusClass — kept as named constants
// so dashboards / alert rules grep against a single source of truth.
const (
	statusClass4xx   = "4xx"
	statusClass5xx   = "5xx"
	statusClassOther = "other"
)

// MetricsOptions configures a Metrics bundle. The zero value is fine
// for typical services (no route extraction, default histogram
// buckets, default registry).
type MetricsOptions struct {
	// Route extracts a low-cardinality route pattern from a request
	// (e.g. "/sites/:id" instead of "/sites/01HX..."). Returning ""
	// drops the route dimension for that request — useful to keep
	// catch-all 404s from blowing up cardinality. nil = always "".
	//
	// For Go 1.22+ ServeMux callers, a typical implementation is
	// `func(r *http.Request) string { return r.Pattern }`; routers
	// that store their own pattern (chi, httprouter) expose an
	// equivalent helper.
	Route func(*http.Request) string

	// DurationBuckets overrides the default histogram buckets. nil
	// keeps the Prometheus default (~5ms-10s), which is fine for
	// HTTP latencies; override for sub-millisecond services or
	// long-running endpoints.
	DurationBuckets []float64

	// Registry is the Prometheus registry to register metrics on.
	// nil = a fresh, isolated *prometheus.Registry seeded with the
	// Go runtime + process collectors. Pass prometheus.DefaultRegisterer
	// only when a service has a strong reason to share a global
	// registry (almost never — isolated registries make tests trivial
	// and side-step double-register panics across services in the
	// same binary).
	Registry *prometheus.Registry
}

// Metrics is the per-service RED bundle: requests counter, errors
// counter, request duration histogram, plus a Prometheus handler.
// One instance per service; mounted via Middleware on every HTTP
// handler that should be measured.
type Metrics struct {
	service  string
	route    func(*http.Request) string
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewMetrics builds the canonical metrics bundle for a service. Three
// vectors are registered:
//
//   - http_requests_total{service, method, route, status}
//   - http_request_errors_total{service, method, route, status_class}
//   - http_request_duration_seconds{service, method, route, status}
//
// plus an `http_in_flight_requests{service}` gauge. status_class on
// errors collapses `4xx` / `5xx` so the counter stays small while
// still distinguishing client from server failures. Use Middleware to
// feed all four with one wrap.
func NewMetrics(service string) *Metrics {
	return NewMetricsWithOptions(service, MetricsOptions{})
}

// NewMetricsWithOptions is the explicit-options form of NewMetrics.
func NewMetricsWithOptions(service string, opts MetricsOptions) *Metrics {
	reg := opts.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	route := opts.Route
	if route == nil {
		route = func(*http.Request) string { return "" }
	}
	buckets := opts.DurationBuckets
	if buckets == nil {
		buckets = prometheus.DefBuckets
	}

	constLabels := prometheus.Labels{"service": service}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "http_requests_total",
		Help:        "Total HTTP requests handled, labeled by method, route and status code.",
		ConstLabels: constLabels,
	}, []string{"method", "route", "status"})
	errors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "http_request_errors_total",
		Help:        "HTTP requests that returned a 4xx or 5xx, grouped by status class.",
		ConstLabels: constLabels,
	}, []string{"method", "route", "status_class"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "http_request_duration_seconds",
		Help:        "HTTP request latency from first byte read to handler return.",
		ConstLabels: constLabels,
		Buckets:     buckets,
	}, []string{"method", "route", "status"})
	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "http_in_flight_requests",
		Help:        "Currently in-flight HTTP requests.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(requests, errors, duration, inFlight)

	return &Metrics{
		service:  service,
		route:    route,
		registry: reg,
		requests: requests,
		errors:   errors,
		duration: duration,
		inFlight: inFlight,
	}
}

// Middleware records one request, one duration sample and (when
// status >= 400) one error per request. Place it as outer as possible
// in the middleware chain so it captures everything a request
// touched, but inside secheaders / RequestID so request_id is in
// context if a panic recovery later wants it.
func (m *Metrics) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.inFlight.Inc()
			defer m.inFlight.Dec()
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			route := m.route(r)
			status := strconv.Itoa(rw.status)
			m.requests.WithLabelValues(r.Method, route, status).Inc()
			m.duration.WithLabelValues(r.Method, route, status).
				Observe(time.Since(start).Seconds())
			if rw.status >= statusClientErrorMin {
				m.errors.WithLabelValues(r.Method, route, statusClass(rw.status)).Inc()
			}
		})
	}
}

// Handler returns the http.Handler that exposes /metrics in the
// Prometheus exposition format. Mount it on the service's mux:
//
//	mux.Handle("/metrics", m.Handler())
//
// The handler reads from the bundle's own registry; default-registry
// collisions across services in the same binary are not a concern.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry: m.registry,
	})
}

// Registry exposes the underlying *prometheus.Registry so services
// can register their own counters/gauges (e.g. business metrics like
// `magic_links_sent_total`) without leaking those registrations into
// the global default registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// statusClass collapses an HTTP status into "4xx" / "5xx" / "other"
// for the errors counter. Keeps cardinality bounded while still
// distinguishing client vs server failures in alerts.
func statusClass(status int) string {
	switch {
	case status >= statusServerErrorMin:
		return statusClass5xx
	case status >= statusClientErrorMin:
		return statusClass4xx
	default:
		return statusClassOther
	}
}

// statusRecorder is the minimum wrapping needed to learn the status
// code after the handler runs. Mirrors the pattern in httplog — kept
// local so observability has zero dep on the access-log package.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
