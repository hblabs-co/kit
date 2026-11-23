package render

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// TestDataHelper_HappyPath — `{{ data "myconfig.subconfig.name" }}`
// walks the map and emits the leaf as template.HTML so SVG bodies
// and other inline markup pass through unescaped. Pins the dot-path
// resolution contract end-to-end through a template execute.
func TestDataHelper_HappyPath(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		Data: map[string]any{
			"myconfig": map[string]any{
				"subconfig": map[string]any{
					"name": "Helmer",
				},
			},
		},
	})
	got, err := execHelper(s, `{{ data "myconfig.subconfig.name" }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "Helmer" {
		t.Errorf("data lookup = %q, want %q", got, "Helmer")
	}
}

// TestDataHelper_LeafIsHTMLPassthrough — leaves marked as inline
// SVG must reach the response body verbatim, not HTML-escaped. The
// helper's template.HTML return type is what guarantees that; this
// test pins the contract so a future refactor returning string
// silently broke icon rendering.
func TestDataHelper_LeafIsHTMLPassthrough(t *testing.T) {
	body := `<path d="M1 2"/>`
	s := withSiteOpts(t, multiLangFS(), Options{
		Data: map[string]any{
			"icons": map[string]any{"love": body},
		},
	})
	got, err := execHelper(s, `{{ data "icons.love" }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != body {
		t.Errorf("data lookup = %q, want raw %q (no HTML escaping)", got, body)
	}
}

// TestDataHelper_NonStringLeafStringifies — the helper accepts any
// scalar leaf and formats it via fmt.Sprint. Lets services declare
// `data.flags.foo: 42` and reference it from templates without
// type-juggling at the YAML side.
func TestDataHelper_NonStringLeafStringifies(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		Data: map[string]any{"count": 42},
	})
	got, err := execHelper(s, `{{ data "count" }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "42" {
		t.Errorf("data lookup = %q, want %q", got, "42")
	}
}

// TestDataHelper_MissingKeyErrors — a typo in the dot-path fails
// the template render with the offending path. The hard-fail is
// what makes WithDataSchema optional: even in libre mode, missing
// keys surface loudly instead of rendering an empty string the
// operator wouldn't notice.
func TestDataHelper_MissingKeyErrors(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		Data: map[string]any{
			"myconfig": map[string]any{"name": "x"},
		},
	})
	_, err := execHelper(s, `{{ data "myconfig.missing" }}`)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), `"myconfig.missing"`) {
		t.Errorf("err = %v, want offending full path quoted", err)
	}
}

// TestDataHelper_PathThroughNonMapErrors — walking past a scalar
// (the path mid-segment lands on a string instead of a sub-map) is
// a misuse that must fail with the prefix that's wrong. Otherwise
// authors would chase a misleading "key not found" when the actual
// problem is the YAML shape.
func TestDataHelper_PathThroughNonMapErrors(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		Data: map[string]any{"name": "Helmer"},
	})
	_, err := execHelper(s, `{{ data "name.first" }}`)
	if err == nil {
		t.Fatal("expected error for path through scalar")
	}
	if !strings.Contains(err.Error(), "is not a map") {
		t.Errorf("err = %v, want 'is not a map'", err)
	}
}

// TestDataHelper_NoDataTreeConfiguredErrors — when Options.Data is
// nil the helper errors instead of silently returning "". Avoids
// templates that drift between "data is missing" and "the YAML
// key happens to be empty".
func TestDataHelper_NoDataTreeConfiguredErrors(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{})
	_, err := execHelper(s, `{{ data "anything" }}`)
	if err == nil {
		t.Fatal("expected error when Data is nil")
	}
	if !strings.Contains(err.Error(), "no data tree configured") {
		t.Errorf("err = %v, want 'no data tree configured'", err)
	}
}

// TestIconHelpers_NotRegisteredWithoutCatalog — leaving IconCatalog
// nil keeps `{{ icon }}` and `{{ iconStroke }}` out of the funcmap
// so a stray reference fails at parse with the helpful "function
// not defined" message rather than rendering an empty SVG. Pins
// the opt-in contract.
func TestIconHelpers_NotRegisteredWithoutCatalog(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{})
	if _, ok := s.funcMap["icon"]; ok {
		t.Error("icon helper registered without IconCatalog")
	}
	if _, ok := s.funcMap["iconStroke"]; ok {
		t.Error("iconStroke helper registered without IconCatalog")
	}
}

// TestIconHelpers_RegisteredWhenCatalogProvided — passing a catalog
// flips both helpers on. Landing wiring depends on this:
// pass uikit.IconSet and the templates get `{{ icon "external-link"
// 16 }}` for free without merging uikit.FuncMap manually.
func TestIconHelpers_RegisteredWhenCatalogProvided(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"x": `<line/>`},
	})
	if _, ok := s.funcMap["icon"]; !ok {
		t.Error("icon helper missing despite IconCatalog")
	}
	if _, ok := s.funcMap["iconStroke"]; !ok {
		t.Error("iconStroke helper missing despite IconCatalog")
	}
}

// TestIconHelper_DataOverridesCatalog — when both data.icons and
// IconCatalog define the same name, the data entry wins. That's
// what lets a service customise the shared catalog from
// config.yaml without forking uikit.IconSet.
func TestIconHelper_DataOverridesCatalog(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"love": `<rect from="catalog"/>`},
		Data: map[string]any{
			"icons": map[string]any{"love": `<rect from="data"/>`},
		},
	})
	got, err := execHelper(s, `{{ icon "love" 16 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, `from="data"`) {
		t.Errorf("icon body = %q, want data entry to win", got)
	}
	if strings.Contains(got, `from="catalog"`) {
		t.Errorf("icon body = %q, catalog body should not appear", got)
	}
}

// TestIconHelper_FallsBackToCatalog — names absent from data.icons
// resolve through IconCatalog, which is the common path: most
// services just plug uikit.IconSet and never declare data.icons.
func TestIconHelper_FallsBackToCatalog(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"only-in-catalog": `<line/>`},
	})
	got, err := execHelper(s, `{{ icon "only-in-catalog" 16 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, `<line/>`) {
		t.Errorf("icon body = %q, want catalog body", got)
	}
	if !strings.Contains(got, `width="16"`) || !strings.Contains(got, `stroke-width="2"`) {
		t.Errorf("icon body = %q, want 24×24 wrapper with default stroke", got)
	}
}

// TestIconHelper_MissingNameReturnsEmpty — unknown name in both
// trees returns "" rather than erroring. Matches uikit.Icon's
// existing "silent on miss" contract so the two paths stay
// interchangeable when a service swaps catalogs.
func TestIconHelper_MissingNameReturnsEmpty(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"a": "<a/>"},
	})
	got, err := execHelper(s, `{{ icon "missing" 16 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "" {
		t.Errorf("icon body = %q, want empty for missing name", got)
	}
}

// TestIconStrokeHelper_CustomStroke — iconStroke honours a caller
// stroke-width so a design that mixes line weights inside one
// component (a thin info circle next to a heavier warning) renders
// correctly. Tests the second helper directly because the wrapper
// emits the stroke-width as part of the SVG attributes.
func TestIconStrokeHelper_CustomStroke(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"warn": "<tri/>"},
	})
	got, err := execHelper(s, `{{ iconStroke "warn" 20 1.5 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, `stroke-width="1.5"`) || !strings.Contains(got, `width="20"`) {
		t.Errorf("iconStroke body = %q, want custom stroke + size", got)
	}
}

// TestIconHelper_DataIconsWrongShapeFallsBack — when data.icons
// exists but the named entry isn't a string (operator typo, e.g.
// declared a nested map), the helper ignores it and falls through
// to IconCatalog. Avoids spurious "" for icons that DO exist in
// the canonical catalog.
func TestIconHelper_DataIconsWrongShapeFallsBack(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"love": "<heart/>"},
		Data: map[string]any{
			"icons": map[string]any{"love": 42}, // wrong type → ignored
		},
	})
	got, err := execHelper(s, `{{ icon "love" 16 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, "<heart/>") {
		t.Errorf("icon body = %q, want catalog fallback when data entry is wrong type", got)
	}
}

// TestIconHelper_DataIconsRootWrongShapeIgnored — when data.icons
// itself isn't a map (operator declared `icons: "oops"` at the
// top level), the helper skips the override path and uses the
// catalog. Same rationale as the per-entry shape mismatch.
func TestIconHelper_DataIconsRootWrongShapeIgnored(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		IconCatalog: map[string]string{"love": "<heart/>"},
		Data:        map[string]any{"icons": "not-a-map"},
	})
	got, err := execHelper(s, `{{ icon "love" 16 }}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, "<heart/>") {
		t.Errorf("icon body = %q, want catalog body when data.icons is not a map", got)
	}
}

// execHelper parses + executes a tiny template string against s's
// funcmap and returns the rendered body. Used by every helper test
// so we exercise the helpers through the template engine (the way
// real pages reach them) rather than calling the methods directly.
func execHelper(s *Site, body string) (string, error) {
	t, err := template.New("t").Funcs(s.funcMap).Parse(body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", err
	}
	return buf.String(), nil
}
