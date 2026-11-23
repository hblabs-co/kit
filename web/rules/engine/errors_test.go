package rulesengine

import (
	"errors"
	"testing"
)

func TestSkipIsSentinel(t *testing.T) {
	t.Parallel()
	if Skip == nil {
		t.Fatal("Skip must be a non-nil sentinel")
	}
	wrapped := errors.Join(Skip, errors.New("other"))
	if !errors.Is(wrapped, Skip) {
		t.Fatal("Skip must be discoverable via errors.Is on wrapped errors")
	}
}

func TestAbortRoundTrip(t *testing.T) {
	t.Parallel()
	err := Abort("plan-too-low")
	if !IsAbort(err) {
		t.Fatal("IsAbort should return true for Abort()'s error")
	}
	if got := AbortReason(err); got != "plan-too-low" {
		t.Fatalf("AbortReason = %q, want plan-too-low", got)
	}
	if msg := err.Error(); msg != "rules: abort: plan-too-low" {
		t.Fatalf("Error() = %q, want rules: abort: plan-too-low", msg)
	}
}

func TestAbortEmptyReasonNormalised(t *testing.T) {
	t.Parallel()
	err := Abort("")
	if AbortReason(err) != "aborted" {
		t.Fatalf("empty reason must normalise to 'aborted', got %q", AbortReason(err))
	}
}

func TestAbortReasonOnNonAbort(t *testing.T) {
	t.Parallel()
	if r := AbortReason(errors.New("plain")); r != "" {
		t.Fatalf("AbortReason on non-abort must be \"\", got %q", r)
	}
	if r := AbortReason(nil); r != "" {
		t.Fatalf("AbortReason on nil must be \"\", got %q", r)
	}
}

func TestAbortWithInternalRoundTrip(t *testing.T) {
	t.Parallel()
	err := AbortWithInternal("forbidden", "user=42 lacks role=admin")
	if !IsAbort(err) {
		t.Fatal("IsAbort must accept AbortWithInternal's value")
	}
	if got := AbortReason(err); got != "forbidden" {
		t.Fatalf("public reason = %q, want forbidden", got)
	}
	if got := AbortInternal(err); got != "user=42 lacks role=admin" {
		t.Fatalf("internal = %q", got)
	}
}

func TestAbortWithInternalEmptyPublicNormalised(t *testing.T) {
	t.Parallel()
	err := AbortWithInternal("", "log only")
	if AbortReason(err) != "aborted" {
		t.Fatal("empty public must normalise to 'aborted'")
	}
	if AbortInternal(err) != "log only" {
		t.Fatal("internal must survive empty-public normalisation")
	}
}

func TestAbortInternalEmptyForPlainAbort(t *testing.T) {
	t.Parallel()
	if AbortInternal(Abort("plain")) != "" {
		t.Fatal("Abort without internal must return empty AbortInternal")
	}
}

func TestAbortInternalOnNonAbort(t *testing.T) {
	t.Parallel()
	if AbortInternal(errors.New("plain")) != "" {
		t.Fatal("AbortInternal on non-abort must be empty")
	}
	if AbortInternal(nil) != "" {
		t.Fatal("AbortInternal on nil must be empty")
	}
}

func TestIsAbortFalseForOtherErrors(t *testing.T) {
	t.Parallel()
	if IsAbort(nil) {
		t.Fatal("IsAbort(nil) must be false")
	}
	if IsAbort(errors.New("nope")) {
		t.Fatal("IsAbort on plain error must be false")
	}
	if IsAbort(Skip) {
		t.Fatal("IsAbort on Skip must be false")
	}
}

func TestRuleErrorFormatsAndUnwraps(t *testing.T) {
	t.Parallel()
	inner := errors.New("boom")
	re := &RuleError{Rule: "fetch-org", Err: inner}
	if got := re.Error(); got != "rules: fetch-org: boom" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(re, inner) {
		t.Fatal("RuleError must unwrap to its inner error via errors.Is")
	}
}
