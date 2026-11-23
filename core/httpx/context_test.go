package httpx

import (
	"context"
	"testing"

	"github.com/hblabs-co/kit/core/log"
)

// TestRequestIDContextRoundTrip pins the contract every middleware
// downstream of RequestID relies on: WithRequestID and
// RequestIDFromContext are inverses. A regression here would silently
// erase request_id from every log line and access record.
func TestRequestIDContextRoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "rid-abc")
	if got := RequestIDFromContext(ctx); got != "rid-abc" {
		t.Errorf("RequestIDFromContext = %q, want %q", got, "rid-abc")
	}
}

// TestRequestIDFromContextEmpty — background workers (cron, NATS
// consumers) reuse handler code paths and don't have a request-id.
// The contract is "" (not panic), so they keep running.
func TestRequestIDFromContextEmpty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext on bare ctx = %q, want \"\"", got)
	}
}

// TestDeviceIDContextRoundTrip — same shape as request-id; mirrored
// because ContextLogAttrs and the access log both depend on this
// helper returning the value WithDeviceID stored.
func TestDeviceIDContextRoundTrip(t *testing.T) {
	ctx := WithDeviceID(context.Background(), "did-xyz")
	if got := DeviceIDFromContext(ctx); got != "did-xyz" {
		t.Errorf("DeviceIDFromContext = %q, want %q", got, "did-xyz")
	}
}

// TestDeviceIDFromContextEmpty — see TestRequestIDFromContextEmpty;
// keeping this branch covered means we won't accidentally flip the
// helper to panic on bare contexts.
func TestDeviceIDFromContextEmpty(t *testing.T) {
	if got := DeviceIDFromContext(context.Background()); got != "" {
		t.Errorf("DeviceIDFromContext on bare ctx = %q, want \"\"", got)
	}
}

// TestContextLogAttrs_Both — when both IDs are attached, the order
// is request_id then device_id (matches the ordering in the access
// log so a side-by-side eyeball comparison stays predictable).
func TestContextLogAttrs_Both(t *testing.T) {
	ctx := WithRequestID(context.Background(), "rid")
	ctx = WithDeviceID(ctx, "did")
	attrs := ContextLogAttrs(ctx)
	if len(attrs) != 2 {
		t.Fatalf("len(attrs) = %d, want 2", len(attrs))
	}
	if attrs[0].Key != "request_id" || attrs[0].Value.String() != "rid" {
		t.Errorf("attrs[0] = %+v, want request_id=rid", attrs[0])
	}
	if attrs[1].Key != "device_id" || attrs[1].Value.String() != "did" {
		t.Errorf("attrs[1] = %+v, want device_id=did", attrs[1])
	}
}

// TestContextLogAttrs_OnlyRequestID — when device-id is missing the
// helper must omit it entirely, not emit an empty-string field. The
// access log keeps the line lean for platform-level traffic.
func TestContextLogAttrs_OnlyRequestID(t *testing.T) {
	ctx := WithRequestID(context.Background(), "rid")
	attrs := ContextLogAttrs(ctx)
	if len(attrs) != 1 {
		t.Fatalf("len(attrs) = %d, want 1", len(attrs))
	}
	if attrs[0].Key != "request_id" {
		t.Errorf("attrs[0].Key = %q, want request_id", attrs[0].Key)
	}
}

// TestContextLogAttrs_OnlyDeviceID — symmetric to the request-id
// case; covers the second `if` branch alone.
func TestContextLogAttrs_OnlyDeviceID(t *testing.T) {
	ctx := WithDeviceID(context.Background(), "did")
	attrs := ContextLogAttrs(ctx)
	if len(attrs) != 1 {
		t.Fatalf("len(attrs) = %d, want 1", len(attrs))
	}
	if attrs[0].Key != "device_id" {
		t.Errorf("attrs[0].Key = %q, want device_id", attrs[0].Key)
	}
}

// TestContextLogAttrs_None — empty (not nil) result so callers can
// safely append without nil checks. The pre-allocation in the helper
// is a micro-optimisation; this test pins the visible contract.
func TestContextLogAttrs_None(t *testing.T) {
	attrs := ContextLogAttrs(context.Background())
	if attrs == nil {
		t.Fatal("attrs = nil, want non-nil empty slice")
	}
	if len(attrs) != 0 {
		t.Errorf("len(attrs) = %d, want 0", len(attrs))
	}
	// Trivial sanity that the slice is appendable — pinning the
	// "safe to append" contract from the doc comment.
	attrs = append(attrs, log.String("k", "v"))
	if len(attrs) != 1 {
		t.Errorf("after append len = %d, want 1", len(attrs))
	}
}
