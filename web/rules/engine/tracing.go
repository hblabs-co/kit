package rulesengine

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the package-level identifier the OTel provider
// uses to namespace these spans. Looked up via otel.Tracer at each
// Start call — the global registry caches by name, so the cost is
// one map read per span and no shared mutable state lives in this
// package (which let tests swap the global TracerProvider safely).
const tracerName = "rules"

// startPipelineSpan opens a top-level span for an Engine.Run call.
// Returns the new context (which child spans should be parented to)
// and the span itself for the caller to end + annotate after the
// pipeline finishes.
//
// Attributes are kept minimal — pipeline name only. The trace
// already carries request-level attrs from the HTTP middleware
// (request_id, route, method), so duplicating them here would just
// inflate span size without helping searches.
func startPipelineSpan(ctx context.Context, pipeline string) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "rules.pipeline",
		trace.WithAttributes(attribute.String("rules.pipeline", pipeline)),
	)
}

// startRuleSpan opens a span for one rule's Run call. The caller
// sets the outcome attribute via finishSpan before End — that way
// the span carries success / skip / abort / error in a single
// cardinality-friendly attribute.
func startRuleSpan(ctx context.Context, rule string) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "rules.rule",
		trace.WithAttributes(attribute.String("rules.rule", rule)),
	)
}

// startParallelSpan opens a span for a parallel group of N child
// rules. Useful so the trace tree visually fans out — without this
// the parallel children would all appear as siblings under the
// pipeline span without indication of concurrency.
func startParallelSpan(ctx context.Context) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "rules.parallel")
}

// finishSpan tags a span with the run outcome and ends it. err
// nil = "ok"; Skip = "skip"; Abort = "abort"; everything else =
// "error" with err recorded. Pulled into a helper so every Start
// site has a one-line counterpart.
func finishSpan(span trace.Span, err error) {
	outcome := outcomeOf(err)
	span.SetAttributes(attribute.String("rules.outcome", outcome))
	switch outcome {
	case "error":
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	case "abort":
		// Abort is intentional, not a fault — annotate but don't
		// flag the span Error. The reason is logged separately by
		// the executor when an internal context exists.
		span.SetAttributes(attribute.String("rules.abort_reason", AbortReason(err)))
	}
	span.End()
}

// outcomeOf classifies a Run/Step error into one of four labels
// used as the span's rules.outcome attribute. Stable cardinality —
// the only legitimate values are these four — so trace backends
// can aggregate without explosion.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case isSkip(err):
		return "skip"
	case IsAbort(err):
		return "abort"
	default:
		return "error"
	}
}

// isSkip avoids pulling errors.Is into this file's import list for
// the one-shot sentinel check. Skip is package-level so identity
// comparison is fine; wrapping isn't supported for Skip today.
func isSkip(err error) bool {
	return err == Skip
}
