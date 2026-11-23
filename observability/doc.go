// Package observability bundles the three observability surfaces every
// HB Labs service exposes: Prometheus metrics, a composite
// readiness endpoint, and OpenTelemetry tracing.
//
// The split is intentional — services rarely want all three on the
// same wire-up boilerplate, but always want the same shape:
//
//	m := observability.NewMetrics("service-foo")
//	mux.Handle("/metrics", m.Handler())
//	mux.Handle("/readyz", observability.Healthz(observability.HealthzOptions{
//	    Logger: logger,
//	}, dbCheck, redisCheck))
//	shutdownTracing, _ := observability.InitTracing(ctx, observability.TracingOptions{
//	    Service: "service-foo",
//	})
//	defer shutdownTracing(context.Background())
//
//	handler = m.Middleware()(observability.TracingMiddleware("service-foo")(handler))
//
// Conventions across the platform (architecture.md §6):
//
//   - Metrics: RED (Rate, Errors, Duration) per endpoint, exposed at
//     `/metrics`. Cardinality stays bounded — labels are method,
//     status, and an optional route pattern the caller provides.
//   - Healthz: liveness (`/healthz`) is a 200 OK ping; readiness
//     (`/readyz`, this package) walks dependency checks and returns
//     503 if any fail. k8s wires `readinessProbe → /readyz`.
//   - Tracing: OTLP-HTTP exporter to the cluster collector when
//     OTEL_EXPORTER_OTLP_ENDPOINT is set, no-op otherwise. The
//     request_id from httpx.RequestID rides as a span attribute AND
//     as W3C baggage so downstream services and logs correlate.
package observability
