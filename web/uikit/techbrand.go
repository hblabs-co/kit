package uikit

import (
	"fmt"
	"html/template"
	"strings"
)

// TechBrandGlyph carries the inner SVG markup of a brand logo
// alongside its native viewBox. Brand SVGs ship in many native
// aspect ratios (Simple Icons uses 24×24, the Go gopher uses
// 254.5×225, npm uses 128×128, …) so we keep the original viewBox
// per icon and just resize via width/height on the wrapper at
// render time.
//
// Body is the inner markup as it appears between <svg>…</svg> in
// the source file with one normalisation pass: color attributes
// (fill, stroke, class refs, inline style, <style> blocks, <title>)
// are stripped so the wrapper's fill="currentColor" propagates and
// the whole logo reads as one theme-adaptive ink. The pass
// preserves fill="none" (used for outline-only shapes) and any
// stroke="none" (rare). Pre-stripped content lives in
// techbrand_data.go; do not paste new entries here by hand —
// regenerate that file from the SVG sources instead (see its
// header).
type TechBrandGlyph struct {
	ViewBox string
	Body    string
}

// TechBrand renders a brand logo from TechBrandSet at the given
// pixel size, monochrome via currentColor so the same SVG reads
// correctly against light and dark backgrounds when a parent
// element sets `color: var(--hb-ink-X)`. Empty HTML if the name is
// unknown — a template typo fails loudly enough in dev.
//
//	{{ techBrand "go" 28 }}
//
// Brand logos are the OFFICIAL artwork imported verbatim from each
// brand's press kit / Simple Icons. The package never transcribes
// or approximates a brand mark by hand; if a logo is missing from
// TechBrandSet, the right fix is to add the source SVG to the seed
// directory and regenerate techbrand_data.go, not to draw a
// look-alike here.
func TechBrand(name string, size int) template.HTML {
	g, ok := TechBrandSet[name]
	if !ok {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb,
		`<svg width="%d" height="%d" viewBox="%s" fill="currentColor" aria-hidden="true">%s</svg>`,
		size, size, g.ViewBox, g.Body,
	)
	return template.HTML(sb.String())
}
