package observability

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/httpx"
)

// defaultExporterTimeout caps each OTLP-HTTP export round trip. Long
// enough to absorb a momentarily slow collector; short enough that a
// stuck export never piles up across batches.
const defaultExporterTimeout = 10 * time.Second

// defaultSampleRate is the head-based sampling probability used when
// TracingOptions.SampleRate is zero. 1.0 = sample every span — useful
// at this stage of the platform where volume is low and complete
// traces matter more than collector cost. Tune down once we get noisy.
const defaultSampleRate = 1.0

// unknownService is the resource attribute substituted when a caller
// passes an empty Service name. Keeps the exported spans discoverable
// instead of silently dropping into an unnamed bucket.
const unknownService = "unknown"

// TracingOptions configures the OTLP-HTTP exporter and the global
// tracer / propagator. Sensible defaults make this safe to call
// unconditionally — the package falls back to a no-op tracer when
// Endpoint is empty, so a service in `local` that never set
// OTEL_EXPORTER_OTLP_ENDPOINT pays nothing.
type TracingOptions struct {
	// Service is the resource attribute attached to every span.
	// Required; an empty value is replaced with "unknown" so the
	// caller's misuse doesn't make spans silently undiscoverable.
	Service string

	// Endpoint is the OTLP-HTTP collector endpoint, e.g.
	// "otel-collector.observability.svc:4318". When empty, the
	// returned shutdown is a no-op and TracingMiddleware returns the
	// identity middleware — services keep working with zero tracing
	// overhead.
	Endpoint string

	// Insecure switches the exporter to plain HTTP instead of HTTPS.
	// In-cluster collectors are typically behind plain HTTP because
	// the network already isolates them; set false when going
	// through an internet-facing collector.
	Insecure bool

	// Environment surfaces as the deployment.environment.name
	// resource attribute. Pass "local" / "staging" / "prod" so
	// dashboards can filter without parsing labels.
	Environment string

	// SampleRate is the head-based sampling probability between 0
	// and 1. 0 = drop everything, 1 = sample everything. Default
	// (zero value) means 1.0 — useful at this stage of the
	// platform where volume is low and complete traces matter more
	// than collector cost. Tune down once we get noisy.
	SampleRate float64
}

// newOTLPExporter is the seam tests swap to drive the
// otlptracehttp.New error branch. Production always returns a real
// exporter; otlptracehttp.New is effectively infallible at
// construction time, so without this seam its error branch would be
// permanently uncovered.
var newOTLPExporter = func(ctx context.Context, opts ...otlptracehttp.Option) (sdktrace.SpanExporter, error) {
	return otlptracehttp.New(ctx, opts...)
}

// InitTracing configures the global OpenTelemetry TracerProvider
// and the W3C trace context + baggage propagators. Returns a
// shutdown closure that flushes pending spans — call it from the
// service's graceful-shutdown path so the exporter has a chance to
// drain before the process exits.
//
// Safe to call multiple times across a binary, but only the last
// call wins (otel keeps a single global provider). When Endpoint is
// empty, a no-op provider is installed and shutdown returns nil.
func InitTracing(ctx context.Context, opts TracingOptions) (func(context.Context) error, error) {
	if opts.Service == "" {
		opts.Service = unknownService
	}
	// Always set the propagators so a service running without an
	// exporter still extracts incoming traceparent headers and can
	// forward them — keeps end-to-end traces intact when only some
	// pods have an exporter configured.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	if opts.Endpoint == "" {
		// No exporter. Leave the default no-op TracerProvider in
		// place so spans are valid but don't cost anything.
		return func(context.Context) error { return nil }, nil
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(stripScheme(opts.Endpoint)),
		otlptracehttp.WithTimeout(defaultExporterTimeout),
	}
	if opts.Insecure || strings.HasPrefix(opts.Endpoint, "http://") {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	exporter, err := newOTLPExporter(ctx, exporterOpts...)
	if err != nil {
		return nil, err
	}

	rate := opts.SampleRate
	if rate == 0 {
		rate = defaultSampleRate
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(opts.Service),
			attribute.String("deployment.environment.name", opts.Environment),
		),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// TracingMiddleware wraps an http.Handler so every request runs
// inside a server span tagged with method, route (when r.Pattern is
// set by Go 1.22+ ServeMux) and the resolved request_id. The
// request_id is also added to W3C baggage so downstream services
// pick it up and emit it on their own spans / logs — this is the
// missing link that makes "search by request_id" work end-to-end.
//
// service is used to name the tracer; pass the same string given to
// InitTracing.Service.
func TracingMiddleware(service string) func(http.Handler) http.Handler {
	tracer := otel.Tracer(service)
	propagator := otel.GetTextMapPropagator()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			rid := httpx.RequestIDFromContext(ctx)
			if rid == "" {
				// httpx.RequestID hasn't run yet, or this is a
				// non-HTTP code path. Try the header directly so
				// tracing still has a correlation handle.
				rid = r.Header.Get(constants.HeaderRequestID)
			}
			if rid != "" {
				if member, err := baggage.NewMember("request_id", rid); err == nil {
					if bag, err := baggage.FromContext(ctx).SetMember(member); err == nil {
						ctx = baggage.ContextWithBaggage(ctx, bag)
					}
				}
			}

			spanName := r.Method + " " + routeOrPath(r)
			ctx, span := tracer.Start(ctx, spanName,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(r.Method),
					semconv.URLPath(r.URL.Path),
					semconv.UserAgentOriginal(r.UserAgent()),
				),
			)
			defer span.End()

			if rid != "" {
				span.SetAttributes(attribute.String("request_id", rid))
			}

			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r.WithContext(ctx))
			span.SetAttributes(semconv.HTTPResponseStatusCode(rw.status))
			if rw.status >= statusServerErrorMin {
				span.SetStatus(codes.Error, http.StatusText(rw.status))
			}
		})
	}
}

// routeOrPath prefers Go 1.22+ ServeMux's matched pattern (low
// cardinality, "/sites/{id}") and falls back to the raw URL path
// when the request hasn't been routed by ServeMux. Span names
// inherit the same value so trace search-by-route works.
func routeOrPath(r *http.Request) string {
	if p := r.Pattern; p != "" {
		return p
	}
	return r.URL.Path
}

// stripScheme removes a leading http:// or https:// from an OTLP
// endpoint. otlptracehttp.WithEndpoint expects host:port without
// scheme; accepting either form here lets services pass whatever
// shape OTEL_EXPORTER_OTLP_ENDPOINT happens to carry.
func stripScheme(endpoint string) string {
	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		return strings.TrimSuffix(rest, "/")
	}
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		return strings.TrimSuffix(rest, "/")
	}
	return strings.TrimSuffix(endpoint, "/")
}
