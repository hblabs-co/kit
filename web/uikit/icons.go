package uikit

import (
	"fmt"
	"html/template"
	"strings"
)

// IconSet is the platform's catalog of inline SVG icons, shared by
// every HB Labs service. Keys are stable, kebab-case names
// used by templates; values are the inner SVG markup (no <svg>
// wrapper) so the renderer can decorate them with caller-provided
// size + stroke without re-parsing the body.
//
// Convention: 24×24 viewBox, no fill, stroke="currentColor", round
// linecaps + joins — matches Feather Icons and lets a single
// `color:` property tint the icon from the surrounding text style.
// Paste the design's path/rect/line markup keyed by the same name
// the design system uses; the wrapper takes care of width/height/
// stroke-width.
//
// Adding an icon: drop the new entry below, alphabetically by key.
// Tests fail if a key has no inner markup, so a malformed copy-paste
// is caught at CI rather than rendering as an empty SVG in
// production.
var IconSet = map[string]string{
	// External link — used by project cards to open a public URL.
	"external-link": `<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/>`,

	// Code chevrons "< >" — used as the source-repository icon.
	"code": `<polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/>`,

	// Smartphone — used as the App Store icon on iOS / Android
	// project cards. Apple's own glyph is trademarked, so we use a
	// generic mobile device.
	"smartphone": `<rect x="5" y="2" width="14" height="20" rx="2" ry="2"/><line x1="12" y1="18" x2="12.01" y2="18"/>`,

	// Globe — multilingual / internationalisation.
	"globe": `<circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/>`,

	// Pen tool — bespoke / hands-on design work.
	"pen-tool": `<path d="M12 19l7-7 3 3-7 7-3-3z"/><path d="M18 13l-1.5-7.5L2 2l3.5 14.5L13 18l5-5z"/><path d="M2 2l7.586 7.586"/><circle cx="11" cy="11" r="2"/>`,

	// Search (magnifier) — discoverability / SEO.
	"search": `<circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>`,

	// Server — owned hosting infrastructure.
	"server": `<rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/>`,

	// Shield — security / privacy / regulatory compliance.
	"shield": `<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/>`,

	// Sparkles — AI-assisted work. (Lucide glyph; same 24×24 stroke
	// grid as the Feather set.)
	"sparkles": `<path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .962 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.962 0z"/><path d="M20 3v4"/><path d="M22 5h-4"/><path d="M4 17v2"/><path d="M5 18H3"/>`,

	// Upload-cloud — upload / migration.
	"upload-cloud": `<polyline points="16 16 12 12 8 16"/><line x1="12" y1="12" x2="12" y2="21"/><path d="M20.39 18.39A5 5 0 0 0 18 9h-1.26A8 8 0 1 0 3 16.3"/><polyline points="16 16 12 12 8 16"/>`,

	// Zap (lightning bolt) — speed / performance.
	"zap": `<polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/>`,

	// --- Design-system sprite icons, ported from
	// the design-system icon sprite (Lucide, 24×24,
	// stroke-only). Wrapper attrs stripped — IconStroke adds
	// fill/stroke/stroke-width. Names are the DS sprite ids
	// without the `i-` prefix. globe/search/sparkles/zap omitted
	// here: already curated above.
	"alert-circle":   `<circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/>`,
	"alert-triangle": `<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/>`,
	"arrow-down":     `<path d="M12 5v14M19 12l-7 7-7-7"/>`,
	"arrow-up":       `<path d="M12 19V5M5 12l7-7 7 7"/>`,
	"arrow-up-right": `<path d="M7 17 17 7M7 7h10v10"/>`,
	"bell":           `<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>`,
	"blocks":         `<rect x="14" y="3" width="7" height="7" rx="1"/><path d="M10 21V8a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-5a1 1 0 0 0-1-1Z"/>`,
	"bot":            `<path d="M12 8V4H8"/><rect x="4" y="8" width="16" height="12" rx="2"/><path d="M2 14h2M20 14h2M15 13v2M9 13v2"/>`,
	"calendar":       `<rect x="3" y="4" width="18" height="18" rx="2"/><path d="M16 2v4M8 2v4M3 10h18"/>`,
	"chart-line":     `<path d="M3 3v18h18"/><path d="m7 14 4-4 4 4 5-6"/>`,
	"check":          `<path d="M20 6 9 17l-5-5"/>`,
	"check-circle":   `<circle cx="12" cy="12" r="9"/><path d="m9 12 2 2 4-4"/>`,
	"chevron-down":   `<path d="m6 9 6 6 6-6"/>`,
	"chevron-right":  `<path d="m9 6 6 6-6 6"/>`,
	"clock":          `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
	"credit-card":    `<rect x="2" y="5" width="20" height="14" rx="2"/><path d="M2 10h20"/>`,
	"eye":            `<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7z"/><circle cx="12" cy="12" r="3"/>`,
	"filter":         `<path d="M3 4h18l-7 9v6l-4 2v-8L3 4z"/>`,
	"image":          `<rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-5-5L5 21"/>`,
	"info":           `<circle cx="12" cy="12" r="9"/><path d="M12 16v-4M12 8h.01"/>`,
	"layout-grid":    `<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>`,
	"life-buoy":      `<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="4"/><path d="m4.93 4.93 4.24 4.24M14.83 14.83l4.24 4.24M14.83 9.17l4.24-4.24M9.17 14.83l-4.24 4.24"/>`,
	"link":           `<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>`,
	"mail":           `<rect x="2" y="4" width="20" height="16" rx="2"/><path d="m22 7-10 7L2 7"/>`,
	"more":           `<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>`,
	"package":        `<path d="M16.5 9.4 7.5 4.21M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/><path d="m3.27 6.96 8.73 5.05 8.73-5.05M12 22.08V12"/>`,
	"phone":          `<path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72c.13.96.37 1.9.72 2.81a2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45c.91.35 1.85.59 2.81.72A2 2 0 0 1 22 16.92z"/>`,
	"plus":           `<path d="M12 5v14M5 12h14"/>`,
	"settings":       `<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>`,
	"trending-up":    `<path d="m3 17 6-6 4 4 8-8"/><path d="M17 7h4v4"/>`,
	"users":          `<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>`,
	"utensils":       `<path d="M3 2v7c0 1.1.9 2 2 2h0a2 2 0 0 0 2-2V2M7 2v20M21 15V2a5 5 0 0 0-3 4.5V13c0 1.1.9 2 2 2h1zM21 15v7"/>`,
	"x":              `<path d="M18 6 6 18M6 6l12 12"/>`,

	// --- Theme toggle. Feather sun/moon, mirrored verbatim from the
	// design system's inline theme switch
	// (design/project/design-system/index.html). Not in the DS
	// icons.svg sprite — they live in the catalog's own toggle markup
	// — so they sit in their own block rather than under the sprite
	// header above. Powers the light/dark switch in every service.
	"moon": `<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79Z"/>`,
	"sun":  `<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"/>`,
}

// DefaultStroke is the Feather-default stroke width applied by
// Icon. Services that need a different weight pass it explicitly
// via IconStroke.
const DefaultStroke = 2.0

// Icon renders the named inline SVG at the given pixel size with
// the default stroke. Returns empty HTML if the name is not in
// IconSet — templates fail loud enough during dev that a typo is
// caught before deploy, so we avoid logging.
//
// Example template usage (after wiring uikit.FuncMap):
//
//	{{ icon "external-link" 16 }}
//
// Renders a 16×16 SVG inheriting color from its parent's `color:`.
func Icon(name string, size int) template.HTML {
	return IconStroke(name, size, DefaultStroke)
}

// IconStroke is Icon with a caller-controlled stroke width. Useful
// when a design mixes line weights inside one component (a thin
// "info" circle next to a heavier "warning" triangle).
func IconStroke(name string, size int, stroke float64) template.HTML {
	body, ok := IconSet[name]
	if !ok {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb,
		`<svg width="%d" height="%d" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">%s</svg>`,
		size, size, stroke, body,
	)
	return template.HTML(sb.String())
}

// FuncMap exposes Icon and IconStroke as template helpers. Services
// merge it into their site.Options.FuncMap so every template across
// every service spells icons the same way:
//
//	{{ icon "external-link" 16 }}
//	{{ iconStroke "warning" 20 1.5 }}
//
// Centralising the helper names here means a future addition (e.g.
// `iconColor name size hex`) lands once and propagates on next
// deploy, without per-service wiring drift.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"icon":       Icon,
		"iconStroke": IconStroke,
		"techIcon":   TechIcon,
		"techBrand":  TechBrand,
		"themeBoot":  ThemeBoot,
	}
}
