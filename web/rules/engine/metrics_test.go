package rulesengine

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// withMeterReader installs an OTel meter provider backed by a
// ManualReader and returns the reader so tests can poll the
// recorded metrics. Like withSpanRecorder, this MUST NOT run with
// t.Parallel — it mutates the process-global MeterProvider.
func withMeterReader(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
	})
	return reader
}

// collectMetrics polls the reader and returns the recorded
// resource metrics for inspection. Convenience wrapper around
// Reader.Collect with the boilerplate ResourceMetrics struct.
func collectMetrics(t *testing.T, reader *metric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

// metricByName walks the recorded scope metrics looking for an
// instrument with the supplied name. t.Fatal if not found — every
// caller asserts presence.
func metricByName(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not recorded (saw: %v)", name, listMetricNames(rm))
	return metricdata.Metrics{}
}

func listMetricNames(rm metricdata.ResourceMetrics) []string {
	var out []string
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			out = append(out, m.Name)
		}
	}
	return out
}

// counterValue extracts the total Int64 count for a counter
// across all attribute sets (sum). For tests asserting a single
// recording's outcome label, walk the data points directly.
func counterDatapoints(t *testing.T, m metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an Int64 Sum, got %T", m.Name, m.Data)
	}
	return sum.DataPoints
}

func histogramDatapoints(t *testing.T, m metricdata.Metrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is not a Float64 Histogram, got %T", m.Name, m.Data)
	}
	return hist.DataPoints
}

// metricsEngine is a tiny engine-builder helper shared by the
// metric tests. Same shape as snapshotEngine but lives here so
// the metrics tests are self-contained.
func metricsEngine(t *testing.T, yamlBody string, rules ...Rule) *Engine {
	t.Helper()
	e := NewEngine().Register(rules...).Load(
		fstest.MapFS{"p.yaml": &fstest.MapFile{Data: []byte(yamlBody)}},
		"p.yaml",
	)
	if err := e.Build(); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPipelineRunCounterRecorded(t *testing.T) {
	reader := withMeterReader(t)
	e := metricsEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&fnRule{name: "r", fn: func(*Context, StepConfig) error { return nil }},
	)
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	rm := collectMetrics(t, reader)
	m := metricByName(t, rm, "rules_pipeline_runs_total")
	dps := counterDatapoints(t, m)
	if len(dps) != 1 {
		t.Fatalf("want 1 datapoint, got %d", len(dps))
	}
	if dps[0].Value != 1 {
		t.Fatalf("counter value = %d, want 1", dps[0].Value)
	}
	pipeline, _ := dps[0].Attributes.Value("pipeline")
	if pipeline.AsString() != "p" {
		t.Fatalf("pipeline attr = %q", pipeline.AsString())
	}
	outcome, _ := dps[0].Attributes.Value("outcome")
	if outcome.AsString() != "ok" {
		t.Fatalf("outcome attr = %q, want ok", outcome.AsString())
	}
}

func TestPipelineRunHistogramRecorded(t *testing.T) {
	reader := withMeterReader(t)
	e := metricsEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&fnRule{name: "r", fn: func(*Context, StepConfig) error { return nil }},
	)
	if _, err := e.Run(context.Background(), nil, "p"); err != nil {
		t.Fatal(err)
	}
	rm := collectMetrics(t, reader)
	m := metricByName(t, rm, "rules_pipeline_duration_seconds")
	dps := histogramDatapoints(t, m)
	if len(dps) != 1 || dps[0].Count != 1 {
		t.Fatalf("want one observation, got %+v", dps)
	}
}

func TestRuleCounterRecordedPerOutcome(t *testing.T) {
	reader := withMeterReader(t)
	// Three rules, three outcomes — ok / skip / abort. The counter
	// must aggregate one datapoint per (rule, outcome) tuple.
	e := metricsEngine(t, `
pipelines:
  p:
    flow:
      - name: ok_rule
      - name: skip_rule
      - name: abort_rule
`,
		&fnRule{name: "ok_rule", fn: func(*Context, StepConfig) error { return nil }},
		&fnRule{name: "skip_rule", fn: func(*Context, StepConfig) error { return Skip }},
		&fnRule{name: "abort_rule", fn: func(*Context, StepConfig) error { return Abort("nope") }},
	)
	_, _ = e.Run(context.Background(), nil, "p")

	rm := collectMetrics(t, reader)
	m := metricByName(t, rm, "rules_rule_executions_total")
	dps := counterDatapoints(t, m)

	want := map[string]string{
		"ok_rule":    "ok",
		"skip_rule":  "skip",
		"abort_rule": "abort",
	}
	saw := map[string]string{}
	for _, dp := range dps {
		ruleAttr, _ := dp.Attributes.Value("rule")
		outcomeAttr, _ := dp.Attributes.Value("outcome")
		saw[ruleAttr.AsString()] = outcomeAttr.AsString()
		if dp.Value != 1 {
			t.Errorf("counter for %s = %d, want 1", ruleAttr.AsString(), dp.Value)
		}
	}
	for rule, outcome := range want {
		if saw[rule] != outcome {
			t.Errorf("rule %q outcome = %q, want %q", rule, saw[rule], outcome)
		}
	}
}

func TestRuleErrorOutcomeRecorded(t *testing.T) {
	reader := withMeterReader(t)
	e := metricsEngine(t,
		"pipelines:\n  p:\n    flow: [{name: r}]\n",
		&fnRule{name: "r", fn: func(*Context, StepConfig) error { return errors.New("boom") }},
	)
	_, _ = e.Run(context.Background(), nil, "p")

	rm := collectMetrics(t, reader)
	m := metricByName(t, rm, "rules_rule_executions_total")
	dps := counterDatapoints(t, m)
	if len(dps) != 1 {
		t.Fatalf("want 1 datapoint, got %d", len(dps))
	}
	outcome, _ := dps[0].Attributes.Value("outcome")
	if outcome.AsString() != "error" {
		t.Fatalf("error rule outcome = %q, want error", outcome.AsString())
	}
}

func TestMetricsNoOpWithoutProvider(t *testing.T) {
	t.Parallel()
	// Without withMeterReader, the global provider is whatever
	// was last installed (the default no-op in a fresh process).
	// Recording must not panic and must not affect anything
	// observable to the test.
	recordPipelineRun(context.Background(), "p", nil, 0)
	recordRuleRun(context.Background(), "r", nil, 0)
}
