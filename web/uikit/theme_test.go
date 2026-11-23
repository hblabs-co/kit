package uikit

import (
	"html/template"
	"strings"
	"testing"
)

// TestThemeBoot_WrappedInlineScript — ThemeBoot must return the
// embedded boot JS wrapped in a <script> so it can be dropped inline
// in <head>. A regression where it returns a bare src reference (or
// empty) would reintroduce the theme flash this snippet exists to
// prevent.
func TestThemeBoot_WrappedInlineScript(t *testing.T) {
	out := string(ThemeBoot())
	if !strings.HasPrefix(out, "<script>") || !strings.HasSuffix(out, "</script>") {
		t.Fatalf("ThemeBoot not wrapped in an inline <script>: %q", out)
	}
	for _, want := range []string{"hb-theme", "prefers-color-scheme", "data-theme"} {
		if !strings.Contains(out, want) {
			t.Errorf("ThemeBoot missing %q — boot contract broken:\n%s", want, out)
		}
	}
	if strings.Contains(out, "src=") {
		t.Errorf("ThemeBoot must be inline, not an external script: %s", out)
	}
}

// TestFuncMap_ExposesThemeBoot — the template wiring exposes
// themeBoot so a service layout can call {{ themeBoot }}. Failing
// here means dark mode silently loses its no-FOUC guard.
func TestFuncMap_ExposesThemeBoot(t *testing.T) {
	fm := FuncMap()
	tb, ok := fm["themeBoot"].(func() template.HTML)
	if !ok {
		t.Fatal("FuncMap missing 'themeBoot' helper (or wrong signature)")
	}
	if tb() != ThemeBoot() {
		t.Error("FuncMap themeBoot does not match ThemeBoot()")
	}
}
