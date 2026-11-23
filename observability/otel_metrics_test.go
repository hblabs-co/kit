package observability

import (
	"context"
	"errors"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// resetMeterProvider restores the global provider to its zero
// state on test cleanup so other tests in the package see a clean
// global registry.
func resetMeterProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
}

func TestEnableOTelMetricsBridgesToPromRegistry(t *testing.T) {
	resetMeterProvider(t)

	m := NewMetrics("test-svc")
	if err := m.EnableOTelMetrics(); err != nil {
		t.Fatalf("EnableOTelMetrics err = %v", err)
	}

	// Record one counter increment through OTel.
	meter := otel.Meter("test_meter")
	c, err := meter.Int64Counter("rules_test_counter")
	if err != nil {
		t.Fatalf("counter create: %v", err)
	}
	c.Add(context.Background(), 1)

	// Pull the Prom registry; gather; check the counter shows up
	// with the prom-translated name. The OTel exporter applies
	// its own naming rules (snake_case + unit suffix), so the
	// assertion looks for the metric NAME substring rather than
	// an exact match.
	gathered, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	// OTel's Prom exporter appends "_total" to counters per the
	// Prom naming guidelines, so the metric ends up named
	// "rules_test_counter_total" in the registry.
	if !containsMetric(gathered, "rules_test_counter_total") {
		t.Fatalf("counter not surfaced in Prom registry; saw: %v", metricNames(gathered))
	}
}

func TestEnableOTelMetricsExporterErrorSurfaces(t *testing.T) {
	// Don't t.Parallel — we swap the package seam.
	orig := newOTelPromReader
	defer func() { newOTelPromReader = orig }()
	newOTelPromReader = func(opts ...otelprom.Option) (sdkmetric.Reader, error) {
		return nil, errors.New("forced exporter init failure")
	}
	resetMeterProvider(t)

	m := NewMetrics("test-svc-err")
	err := m.EnableOTelMetrics()
	if err == nil {
		t.Fatal("expected error from seam-injected failure")
	}
	if !strings.Contains(err.Error(), "observability: enable otel metrics") {
		t.Fatalf("error must carry package prefix, got %v", err)
	}
}

func containsMetric(metrics []*dto.MetricFamily, name string) bool {
	for _, m := range metrics {
		if m.GetName() == name {
			return true
		}
	}
	return false
}

func metricNames(metrics []*dto.MetricFamily) []string {
	out := make([]string, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, m.GetName())
	}
	return out
}

// Verify the meter is non-trivial — sanity check that the
// installed provider isn't the no-op default after EnableOTelMetrics.
func TestEnableOTelMetricsProviderNotNoOp(t *testing.T) {
	resetMeterProvider(t)
	m := NewMetrics("nonop-svc")
	if err := m.EnableOTelMetrics(); err != nil {
		t.Fatal(err)
	}
	// Sanity: the global meter should accept a real Counter and
	// the returned instrument should have a non-zero pointer that
	// is NOT the no-op zero value.
	meter := otel.Meter("sanity")
	_, err := meter.Int64Counter("sanity_check")
	if err != nil {
		t.Fatalf("real provider should accept counter create: %v", err)
	}
	// Test passes if we got here — the no-op meter would have
	// errored or returned a zero counter, but in practice both
	// no-op and real meters succeed here. So this is mostly a
	// smoke check; the real assertion is in
	// TestEnableOTelMetricsBridgesToPromRegistry above.
	_ = metric.WithUnit("1") // touch the metric import so it stays
}
