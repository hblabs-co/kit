package rulesengine

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the OTel instrumentation scope for engine metrics.
// Mirrors the span tracer name (also "rules") so trace and metric
// streams can be filtered with the same selector.
const meterName = "rules"

// recordPipelineRun emits one set of pipeline-level recordings: a
// counter bump per call and a histogram observation of the total
// run duration, both labelled by pipeline name and outcome.
//
// Looks up the meter per call rather than caching at package init:
// the global MeterProvider is set after init runs, and a cached
// no-op meter would stay no-op even after the service wires a
// real provider. The per-call lookup is two atomic reads + one
// map fetch — cheap compared to anything the engine does in a Run.
//
// outcomeOf classifies err into a stable cardinality set
// (ok / skip / abort / error), keeping the metric's label space
// bounded.
func recordPipelineRun(ctx context.Context, pipeline string, err error, dur time.Duration) {
	meter := otel.Meter(meterName)
	attrs := metric.WithAttributes(
		attribute.String("pipeline", pipeline),
		attribute.String("outcome", outcomeOf(err)),
	)
	if c, e := meter.Int64Counter("rules_pipeline_runs_total",
		metric.WithDescription("Pipeline executions, by pipeline name and outcome"),
	); e == nil {
		c.Add(ctx, 1, attrs)
	}
	if h, e := meter.Float64Histogram("rules_pipeline_duration_seconds",
		metric.WithDescription("Pipeline execution duration in seconds"),
	); e == nil {
		h.Record(ctx, dur.Seconds(), attrs)
	}
}

// recordRuleRun emits the per-rule companion of recordPipelineRun.
// One counter + one histogram, labelled by rule name and outcome.
// The duration spans every retry attempt inside the rule (the
// outer span already covers them as a single bracket), so the
// histogram answers "how long does this rule typically take to
// settle, including its retries".
func recordRuleRun(ctx context.Context, rule string, err error, dur time.Duration) {
	meter := otel.Meter(meterName)
	attrs := metric.WithAttributes(
		attribute.String("rule", rule),
		attribute.String("outcome", outcomeOf(err)),
	)
	if c, e := meter.Int64Counter("rules_rule_executions_total",
		metric.WithDescription("Rule executions, by rule name and outcome"),
	); e == nil {
		c.Add(ctx, 1, attrs)
	}
	if h, e := meter.Float64Histogram("rules_rule_duration_seconds",
		metric.WithDescription("Rule execution duration in seconds"),
	); e == nil {
		h.Record(ctx, dur.Seconds(), attrs)
	}
}
