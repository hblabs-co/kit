package uikit

import (
	"fmt"
	"html/template"
	"strings"
)

// TechIconSet is the catalog of monochrome glyphs for the
// "technologies we use" strip — limited to GENERIC concepts
// (protocols, web standards, auth, tooling). Brand logos (Go,
// Docker, Kubernetes, GitHub, …) live in TechBrandSet
// (techbrand.go / techbrand_data.go) so the official brand artwork
// is never transcribed by hand. Templates compose the strip from
// both sets: {{ techIcon "rest" 28 }} for concepts,
// {{ techBrand "docker" 28 }} for logos.
//
// Values here are inner SVG markup on a 24×24 viewBox, rendered
// solid with fill="currentColor" so the whole strip reads as one
// theme-aware ink. Keep them simple and original. The test parses
// every entry as XML — a hand-authored typo fails at CI, not in
// production.
var TechIconSet = map[string]string{
	// Web standards / shell
	// "html":     `<path d="M4 3h16l-1.4 16L12 21l-6.6-2L4 3zm3.2 3 .4 4.2H16l-.2 2.2-3.8 1-3.8-1-.15-1.7H6.1l.35 3.6L12 16.6l5.55-1.6.8-9H7.2z"/>`,
	// "css":      `<path d="M4 3h16l-1.4 16L12 21l-6.6-2L4 3zm3 3 .5 6h8.3l-.3 3.3-3.5 1-3.5-1-.2-2H6l.4 3.6L12 18.6l5.6-1.6.9-9.9H7z"/>`,
	"bash":     mono("sh"),
	"terminal": `<path d="M3 4h18a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zm2 4 4 3-4 3v2l6-5-6-5v2zm6 6h6v2h-6v-2z"/>`,

	// Protocols / APIs
	"rest":      `<path d="M3 7h12V4l6 5-6 5v-3H3V7zm18 10H9v3l-6-5 6-5v3h12v4z"/>`,
	"api":       `<path d="M9 4 4 9l5 5-1.6 1.6L1 9 7.4 2.4 9 4zm6 16 5-5-5-5 1.6-1.6L23 15l-6.4 6.6L15 20zM13.4 4.2l2 .7-4.8 14.9-2-.7 4.8-14.9z"/>`,
	"graphql":   `<path d="M12 2 3 7v10l9 5 9-5V7l-9-5zm0 2.3 6.5 3.6L12 11 5.5 7.9 12 4.3zM5 9.6l6 3.3v6.8l-6-3.3V9.6zm8 10.1v-6.8l6-3.3v6.8l-6 3.3z"/>`,
	"websocket": `<path d="M5 8 1 12l4 4v-3h14v3l4-4-4-4v3H5V8z"/>`,
	"webrtc":    `<path d="M12 8a4 4 0 0 1 4 4h-2a2 2 0 0 0-4 0H8a4 4 0 0 1 4-4zm0-4a8 8 0 0 1 8 8h-2a6 6 0 0 0-12 0H4a8 8 0 0 1 8-8zm0 9.5a1.5 1.5 0 1 1-1.5 1.5A1.5 1.5 0 0 1 12 13.5z"/>`,
	"realtime":  `<path d="M13 2 4 14h6l-1 8 9-12h-6l1-8z"/>`,

	// Auth
	"oauth":  `<path d="M15.5 3a5.5 5.5 0 0 0-5.2 7.3L3 17.6V21h3.4l1.2-1.2V18h1.8v-1.8H11l1.7-1.7A5.5 5.5 0 1 0 15.5 3zm1.5 4.5a1.5 1.5 0 1 1-1.5-1.5 1.5 1.5 0 0 1 1.5 1.5z"/>`,
	"openid": `<path d="M13 3l-2 1v15.2l-4-1.3v-2.2c-2.4.6-4 2-4 3.6 0 2.2 3.1 4 7 4l1-.1V3zm2 4.3v2.1c2.4.6 4 2 4 3.6 0 1-.7 2-1.9 2.6L19 17l4 .7-.9-2c1.2-.9 1.9-2 1.9-3.3 0-2.6-3.9-4.8-9-5.1z"/>`,
	"jwt":    `<path d="M11 2h2v5l-1 1-1-1V2zm0 15 1-1 1 1v5h-2v-5zM4.2 6.3l1.6-1.2 3 4.1-.4 1.3-1.4.3-2.8-4.5zm12 9.1 1.4-.3.4 1.3 3 4.1-1.6 1.2-2.8-4.5-.4-1.8zM2 11h5l1 1-1 1H2v-2zm15 0h5v2h-5l-1-1 1-1zM7.8 14.7l.4 1.3-3 4.1-1.6-1.2 2.8-4.5 1.4.3zm8.4-5.4-.4-1.3 3-4.1 1.6 1.2-2.8 4.5-1.4-.3z"/>`,

	// Delivery
	"ci-cd": `<path d="M5 6a3 3 0 1 1 4 2.8V11h6V8.8a3 3 0 1 1 2 0V12a1 1 0 0 1-1 1H9v2.2a3 3 0 1 1-2 0V8.8A3 3 0 0 1 5 6z"/>`,
}

// mono builds a monospace monogram glyph (1–2 chars), filled with
// currentColor. Used by the few standards drawn as a short label
// (e.g. shell → "sh"); a presentation attribute can't read a CSS
// var, so font-family is a literal monospace stack.
func mono(s string) string {
	return fmt.Sprintf(
		`<text x="12" y="13" text-anchor="middle" dominant-baseline="central" `+
			`font-family="ui-monospace,SFMono-Regular,Menlo,Consolas,monospace" `+
			`font-size="%d" font-weight="700">%s</text>`,
		map[bool]int{true: 11, false: 9}[len([]rune(s)) == 1], s)
}

// TechIcon renders a concept glyph at the given pixel size as a
// solid monochrome SVG (fill="currentColor"). Empty HTML if the
// name is unknown — a template typo fails loudly enough in dev.
// Brand logos go through TechBrand (techbrand.go) instead so each
// preserves its native viewBox without growing this set.
//
//	{{ techIcon "websocket" 28 }}
func TechIcon(name string, size int) template.HTML {
	body, ok := TechIconSet[name]
	if !ok {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb,
		`<svg width="%d" height="%d" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">%s</svg>`,
		size, size, body,
	)
	return template.HTML(sb.String())
}
