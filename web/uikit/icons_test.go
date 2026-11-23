package uikit

import (
	"html/template"
	"strings"
	"testing"
)

// TestIcon_Known renders one of the catalog entries and verifies the
// wrapper carries the size, the default stroke, currentColor, the
// 24×24 viewBox and the inner body. Locks the public contract so a
// future refactor that drops any of those attributes is caught here.
func TestIcon_Known(t *testing.T) {
	got := string(Icon("external-link", 16))
	wantSubstrings := []string{
		`width="16"`,
		`height="16"`,
		`viewBox="0 0 24 24"`,
		`stroke="currentColor"`,
		`stroke-width="2"`,
		`aria-hidden="true"`,
		`M18 13v6`,
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("Icon missing %q in:\n%s", w, got)
		}
	}
}

// TestIcon_Unknown — a typo'd name returns empty HTML rather than a
// half-rendered <svg>. Keeps the dev-time signal loud (the icon just
// doesn't appear) without crashing the request.
func TestIcon_Unknown(t *testing.T) {
	if got := Icon("does-not-exist", 16); got != template.HTML("") {
		t.Errorf("Icon(unknown) = %q, want empty", got)
	}
}

// TestIconStroke_OverridesDefault — explicit stroke makes it into
// the markup and supersedes DefaultStroke. Pins the contract that
// IconStroke is the "I know what I'm doing" path.
func TestIconStroke_OverridesDefault(t *testing.T) {
	got := string(IconStroke("code", 20, 1.5))
	if !strings.Contains(got, `stroke-width="1.5"`) {
		t.Errorf("IconStroke missing stroke-width=1.5:\n%s", got)
	}
	if !strings.Contains(got, `width="20"`) {
		t.Errorf("IconStroke missing width=20:\n%s", got)
	}
}

// TestFuncMap_ExposesBothHelpers — the template wiring exposes both
// Icon and IconStroke under their canonical helper names, and the
// returned funcs preserve behavior. Failing here means a service
// template can't call {{ icon ... }} at all.
func TestFuncMap_ExposesBothHelpers(t *testing.T) {
	fm := FuncMap()
	icon, ok := fm["icon"].(func(string, int) template.HTML)
	if !ok {
		t.Fatal("FuncMap missing 'icon' helper (or wrong signature)")
	}
	if out := icon("smartphone", 24); !strings.Contains(string(out), `width="24"`) {
		t.Errorf("icon(smartphone, 24) wrong: %s", out)
	}
	iconStroke, ok := fm["iconStroke"].(func(string, int, float64) template.HTML)
	if !ok {
		t.Fatal("FuncMap missing 'iconStroke' helper (or wrong signature)")
	}
	if out := iconStroke("code", 18, 1.25); !strings.Contains(string(out), `stroke-width="1.25"`) {
		t.Errorf("iconStroke wrong: %s", out)
	}
}

// TestIconSet_AllEntriesNonEmpty — guard against a copy-paste that
// leaves an entry as the empty string. An empty value would still
// produce a <svg>...</svg> wrapper but with no inner markup, which
// renders as an invisible icon in production — silent and
// frustrating to debug. This sentinel catches it at CI.
func TestIconSet_AllEntriesNonEmpty(t *testing.T) {
	for name, body := range IconSet {
		if strings.TrimSpace(body) == "" {
			t.Errorf("IconSet[%q] is empty", name)
		}
	}
}
