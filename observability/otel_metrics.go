package observability

import (
	"fmt"

	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// EnableOTelMetrics installs a process-global OTel meter provider
// whose recordings export through this Metrics bundle's Prometheus
// registry. Once called, any package using
// `otel.Meter("name")` — web/rules/engine being the first —
// records counters and histograms that show up at the same
// `/metrics` endpoint as the RED HTTP metrics, no extra wiring.
//
// Call once per process, after NewMetrics, before the first request
// hits an instrumented code path:
//
//	m := observability.NewMetrics("service-foo")
//	if err := m.EnableOTelMetrics(); err != nil { /* fail boot */ }
//	mux.Handle("/metrics", m.Handler())
//
// Idempotent in spirit but not in fact — calling twice overwrites
// the global MeterProvider. Fine in main; problematic if invoked
// from background goroutines (don't).
//
// Returns an error only if the OTel Prom exporter rejects the
// registry (e.g. a metric name collision the operator must fix
// before metrics can flow). The global provider is left untouched
// in that case so the caller can decide whether to fail boot.
func (m *Metrics) EnableOTelMetrics() error {
	reader, err := newOTelPromReader(otelprom.WithRegisterer(m.registry))
	if err != nil {
		return fmt.Errorf("observability: enable otel metrics: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	return nil
}

// newOTelPromReader is the variable seam tests use to force the
// exporter-init error branch. Production always uses the real
// otelprom.New; tests override to return a synthesised failure.
var newOTelPromReader = func(opts ...otelprom.Option) (sdkmetric.Reader, error) {
	return otelprom.New(opts...)
}
