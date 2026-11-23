package uikit

import (
	"encoding/xml"
	"html/template"
	"strings"
	"testing"
)

// TestTechBrand_Known — a known brand renders as a sized SVG that
// carries the source viewBox unchanged (each brand keeps its native
// aspect ratio) and fill="currentColor" so it inherits the parent's
// ink color in light + dark.
func TestTechBrand_Known(t *testing.T) {
	out := string(TechBrand("docker", 28))
	want := []string{`width="28"`, `height="28"`, `fill="currentColor"`}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("TechBrand(docker,28) missing %q: %s", w, out)
		}
	}
	// viewBox must be the entry's native value, not hard-coded to
	// 24x24 like TechIcon. Re-read from the map so the assertion
	// doesn't drift if the source SVG is replaced.
	wantVB := `viewBox="` + TechBrandSet["docker"].ViewBox + `"`
	if !strings.Contains(out, wantVB) {
		t.Errorf("TechBrand(docker,28) missing %q: %s", wantVB, out)
	}
}

// TestTechBrand_Unknown — unknown name is empty (a template typo
// fails loudly enough in dev; we don't render a blank <svg>).
func TestTechBrand_Unknown(t *testing.T) {
	if out := TechBrand("definitely-not-a-brand", 28); out != "" {
		t.Errorf("want empty for unknown, got %q", out)
	}
}

// TestTechBrandSet_AllValid — every entry has a non-empty body, a
// non-empty viewBox, and once wrapped renders as well-formed XML.
// The XML parse is the real guard: the generator (techbrand_gen.py)
// strips fills/classes/styles with regex, and a malformed source
// SVG could leave behind an unbalanced tag. This test catches that
// at CI rather than in the marquee.
func TestTechBrandSet_AllValid(t *testing.T) {
	if len(TechBrandSet) == 0 {
		t.Fatal("TechBrandSet is empty — generator didn't run?")
	}
	for name, g := range TechBrandSet {
		if strings.TrimSpace(g.Body) == "" {
			t.Errorf("TechBrandSet[%q].Body is empty", name)
			continue
		}
		if strings.TrimSpace(g.ViewBox) == "" {
			t.Errorf("TechBrandSet[%q].ViewBox is empty", name)
			continue
		}
		svg := string(TechBrand(name, 24))
		if err := xml.Unmarshal([]byte(svg), new(struct {
			XMLName xml.Name `xml:"svg"`
		})); err != nil {
			t.Errorf("TechBrandSet[%q] renders invalid XML: %v\n%s", name, err, svg)
		}
	}
}

// TestFuncMap_ExposesTechBrand — the strip template can call
// {{ techBrand ... }}.
func TestFuncMap_ExposesTechBrand(t *testing.T) {
	fn, ok := FuncMap()["techBrand"]
	if !ok {
		t.Fatal("FuncMap missing 'techBrand'")
	}
	if _, ok := fn.(func(string, int) template.HTML); !ok {
		t.Fatalf("techBrand wrong signature: %T", fn)
	}
}
