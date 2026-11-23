package uikit

import (
	"encoding/xml"
	"html/template"
	"strings"
	"testing"
)

// TestTechIcon_Known — a known glyph renders as a sized, solid
// (fill=currentColor) 24-viewBox SVG.
func TestTechIcon_Known(t *testing.T) {
	out := string(TechIcon("websocket", 32))
	for _, want := range []string{`width="32"`, `height="32"`, `viewBox="0 0 24 24"`, `fill="currentColor"`} {
		if !strings.Contains(out, want) {
			t.Errorf("TechIcon(websocket,32) missing %q: %s", want, out)
		}
	}
}

// TestTechIcon_Unknown — unknown name is empty (a template typo
// fails loudly enough in dev; we don't render a blank <svg>).
func TestTechIcon_Unknown(t *testing.T) {
	if out := TechIcon("definitely-not-a-tech", 24); out != "" {
		t.Errorf("want empty for unknown, got %q", out)
	}
}

// TestMono_FontSizeBranches — mono() bumps the size for a single
// glyph vs a two-char monogram. Covers both arms of the size pick.
func TestMono_FontSizeBranches(t *testing.T) {
	if one := mono("X"); !strings.Contains(one, `font-size="11"`) {
		t.Errorf("1-char monogram should use font-size 11: %s", one)
	}
	if two := mono("Go"); !strings.Contains(two, `font-size="9"`) {
		t.Errorf("2-char monogram should use font-size 9: %s", two)
	}
}

// TestTechIconSet_AllValid — every entry is non-empty AND, once
// wrapped, parses as well-formed XML. The XML parse is the real
// guard: it catches a hand-authored path/markup typo (an unclosed
// tag, a stray quote) at CI instead of rendering as garbage in the
// strip. We can't eyeball ~30 glyphs, so this is the safety net.
func TestTechIconSet_AllValid(t *testing.T) {
	for name, body := range TechIconSet {
		if strings.TrimSpace(body) == "" {
			t.Errorf("TechIconSet[%q] is empty", name)
			continue
		}
		svg := string(TechIcon(name, 24))
		if err := xml.Unmarshal([]byte(svg), new(struct {
			XMLName xml.Name `xml:"svg"`
		})); err != nil {
			t.Errorf("TechIconSet[%q] renders invalid XML: %v\n%s", name, err, svg)
		}
	}
}

// TestFuncMap_ExposesTechIcon — the strip template can call
// {{ techIcon ... }}.
func TestFuncMap_ExposesTechIcon(t *testing.T) {
	fn, ok := FuncMap()["techIcon"]
	if !ok {
		t.Fatal("FuncMap missing 'techIcon'")
	}
	if _, ok := fn.(func(string, int) template.HTML); !ok {
		t.Fatalf("techIcon wrong signature: %T", fn)
	}
}
