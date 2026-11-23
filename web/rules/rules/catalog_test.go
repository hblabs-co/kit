package rules

import (
	"testing"

	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

func TestAllReturnsRegisteredRules(t *testing.T) {
	t.Parallel()
	got := All()
	names := make(map[string]bool, len(got))
	for _, r := range got {
		names[r.Name()] = true
	}
	for _, want := range []string{"detect_locale", "detect_time_of_day", "compute_banner_flags"} {
		if !names[want] {
			t.Fatalf("All() missing %q; got %v", want, names)
		}
	}
}

func TestAllReturnsCopyNotReceiver(t *testing.T) {
	t.Parallel()
	a := All()
	n := len(a)
	if n == 0 {
		t.Fatal("catalog must not be empty")
	}
	// Replace the first element in the returned slice — the catalog
	// must not be affected.
	a[0] = stubRule{}
	if All()[0].Name() == "stub" {
		t.Fatal("All() must return a copy, not the underlying slice")
	}
}

func TestRegisterAppends(t *testing.T) {
	// Don't t.Parallel — we mutate the package catalog and restore.
	orig := catalog
	t.Cleanup(func() { catalog = orig })
	before := len(All())
	Register(stubRule{})
	if len(All()) != before+1 {
		t.Fatalf("Register did not append, len = %d, want %d", len(All()), before+1)
	}
}

type stubRule struct{}

func (stubRule) Name() string                                           { return "stub" }
func (stubRule) Writes() []string                                       { return nil }
func (stubRule) Run(*rulesengine.Context, rulesengine.StepConfig) error { return nil }
