package rulesengine

import (
	"encoding/json"
	"fmt"
	"maps"
)

// Result is the value Engine.Run returns: the snapshot of
// Context.Data after the pipeline finishes, optionally filtered
// through the pipeline's `pick:` list. Plain map[string]any so the
// HTTP handler can json.Marshal it without ceremony, and callers
// inside the app can Bind it to a typed struct when they want
// strong typing.
type Result map[string]any

// Pick returns a Result containing only the named keys. Missing
// keys are silently omitted — the contract is "give me at most
// these fields", not "fail if any is absent". Use it to whittle
// an internal Context.Data down to the response payload (the
// Engine applies the pipeline's `pick:` list this way).
//
// Empty keys = return Result unchanged (a copy, not the receiver,
// so the caller can't mutate the original through it).
func (r Result) Pick(keys ...string) Result {
	if len(keys) == 0 {
		out := make(Result, len(r))
		maps.Copy(out, r)
		return out
	}
	out := make(Result, len(keys))
	for _, k := range keys {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Bind decodes the Result into v via a JSON round-trip. Convenient
// for turning the dict the pipeline produced into a typed model the
// rest of the code already understands:
//
//	var d AccessDecision
//	if err := res.Bind(&d); err != nil { ... }
//
// v must be a pointer to a struct (or any json-unmarshalable target).
// The round-trip is the price of keeping Result a plain map without
// committing to a reflection-based mapper — JSON tags on the
// destination struct are honoured exactly as everywhere else in the
// repo.
func (r Result) Bind(v any) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("rules: marshal result: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("rules: bind result: %w", err)
	}
	return nil
}
