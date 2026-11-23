package render

import (
	"fmt"
	"html/template"
	"strings"
)

// iconDefaultStroke is the Feather-default stroke width applied by
// the `icon` helper. iconStroke takes a caller-controlled value;
// keeping the constant here means render's icon wrapper agrees
// with web/uikit.DefaultStroke without depending on it.
const iconDefaultStroke = 2.0

// pageURL builds the public path for a (lang, pageKey) pair. Empty
// pageKey is the index page (/<lang>/). Used by templates via the
// `pageURL` helper, and by sitemap.xml generation in seo.go — keeping
// the logic in one place is what guarantees they agree.
func (s *Site) pageURL(lang, pageKey string) string {
	if pageKey == "" {
		return "/" + lang + "/"
	}
	return "/" + lang + "/" + strings.TrimPrefix(pageKey, "/")
}

// buildFuncMap returns the template helpers exposed inside .html.tmpl
// files. Every helper closes over `s` so it can read locales and the
// boot-stable assetVer without globals.
//
// Conventions for new helpers:
//   - Pure (no I/O, no side effects). Templates run on the hot path.
//   - Stable signatures across versions — helpers are called from
//     locale strings and tweaking them silently breaks every page.
//   - Return strings or template.HTML. Don't expose maps to authors.
func (s *Site) buildFuncMap() template.FuncMap {
	fm := template.FuncMap{
		// T translates `key` for `lang`, falling back to defaultLang
		// then to the raw key. Use as: {{ T .Lang "hero.title" }}.
		"T": func(lang, key string) string {
			return s.translate(lang, key)
		},

		// asset returns the URL to a file in static/ with a content
		// hash as the cache-buster. Pairs with `Cache-Control: immutable`
		// for year-long browser caching that auto-invalidates only
		// when the file's bytes actually change. Files not present
		// in the fingerprint map (added at runtime, missing at boot,
		// dev-mode races) fall back to the boot-time assetVer so
		// they still bust on redeploy — never on a stable URL though.
		"asset": func(path string) string {
			cleaned := strings.TrimPrefix(path, "/")
			if h, ok := s.fingerprints[cleaned]; ok {
				return "/static/" + cleaned + "?v=" + h
			}
			return "/static/" + cleaned + "?v=" + s.assetVer
		},

		// pageURL builds a /<lang>/<page> path. pageKey "" → /<lang>/.
		// Use it for nav and language-switcher links so we never
		// hardcode paths in templates. Delegates to s.pageURL so
		// templates and the sitemap generator agree on the format.
		"pageURL": s.pageURL,

		// canonical returns the absolute URL of the current page on
		// the configured CanonicalHost. Used for og:url, canonical
		// link tags, etc.
		"canonical": func(path string) string {
			return s.canonicalHost + path
		},

		// ogImage returns the URL of the OG card for (pageKey, lang),
		// derived from the path scheme owned by the auto-mounted OG
		// handler. Returns "" when OG is disabled (Options.OG.Enabled
		// = false), so layouts can conditionally emit the meta tag
		// with `{{ with ogImage .PageKey .Lang }}{{ . }}{{ end }}`.
		"ogImage": s.ogImageURL,

		// devScript returns the hot-reload <script> tag in dev mode
		// (Options.Dev = true), empty string otherwise. Layouts call
		// {{ devScript }} just before </body> in every environment;
		// the helper itself decides whether anything renders. This
		// keeps the layout free of dev-vs-prod conditionals.
		"devScript": func() template.HTML {
			if s.dev {
				return template.HTML(`<script src="/__reload.js"></script>`)
			}
			return ""
		},

		// data resolves a dotted path inside Options.Data and returns
		// the leaf as template.HTML. Intermediate path segments must
		// be maps; the final value is stringified via fmt.Sprint and
		// emitted unescaped so SVG bodies, inline markup, and
		// already-safe HTML pass through.
		//
		// Returning template.HTML is safe because Options.Data is
		// operator-supplied via config.yaml (or in-code Options) —
		// never put user-supplied content into the data tree.
		//
		// Missing keys are an error, not a silent empty string:
		// templates that reference data fail loudly, surfacing the
		// path that the operator forgot to declare. Pairs with
		// Builder.WithDataSchema so most typos are caught at boot
		// instead of render time.
		"data": s.dataValue,

		// icon / iconStroke render the named icon from Options.Data's
		// `icons` map (per-service additions or overrides) with a
		// fallback to Options.IconCatalog (the shared catalog the
		// service plugged in). Only registered when IconCatalog is
		// non-nil — see Options.IconCatalog for the rationale. Both
		// helpers wrap the inner SVG in a 24×24 viewBox with
		// stroke="currentColor" so a single `color:` property tints
		// the icon from the surrounding text style.
		//
		// iconFilled is the sibling for brand glyphs and other
		// fill-painted shapes (WhatsApp / Spotify / Instagram
		// lockups, filled paw prints, …) whose inner paths expect
		// a fill on the parent <svg> rather than a stroke.
		// Same lookup path as icon — services drop these into the
		// same data.icons / IconCatalog and pick the wrapper at
		// the call site (`{{ icon "leaf" 18 }}` vs
		// `{{ iconFilled "whatsapp" 18 }}`).
	}
	if s.iconCatalog != nil {
		fm["icon"] = s.iconHelper
		fm["iconStroke"] = s.iconStrokeHelper
		fm["iconFilled"] = s.iconFilledHelper
	}
	return fm
}

// dataValue resolves a dotted path inside s.data and returns the
// leaf as template.HTML. Walks map[string]any nodes; anything else
// at an intermediate position is a misuse and surfaces as an error
// that fails the template render with the offending path.
func (s *Site) dataValue(path string) (template.HTML, error) {
	if s.data == nil {
		return "", fmt.Errorf("render: data %q: no data tree configured", path)
	}
	parts := strings.Split(path, ".")
	var cur any = map[string]any(s.data)
	for i, key := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("render: data %q: %q is not a map", path, strings.Join(parts[:i], "."))
		}
		v, ok := m[key]
		if !ok {
			return "", fmt.Errorf("render: data %q: key %q not found", path, strings.Join(parts[:i+1], "."))
		}
		cur = v
	}
	return template.HTML(fmt.Sprint(cur)), nil
}

// iconBody resolves the inner SVG markup for `name`, preferring an
// entry under data.icons (per-service override / extension) and
// falling back to the IconCatalog the service plugged in. Returns
// "" + ok=false when neither has the name so callers can decide
// between erroring or rendering an empty placeholder.
func (s *Site) iconBody(name string) (string, bool) {
	if icons, ok := s.data["icons"].(map[string]any); ok {
		if body, ok := icons[name].(string); ok {
			return body, true
		}
	}
	if body, ok := s.iconCatalog[name]; ok {
		return body, true
	}
	return "", false
}

// renderIcon wraps `inner` in the standard 24×24 stroke-only SVG
// wrapper used across HB Labs services. Same shape as
// web/uikit.IconStroke; duplicated here so render's helpers
// stay self-contained when uikit isn't part of the build.
func renderIcon(inner string, size int, stroke float64) template.HTML {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		`<svg width="%d" height="%d" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">%s</svg>`,
		size, size, stroke, inner,
	)
	return template.HTML(sb.String())
}

// iconHelper / iconStrokeHelper back the `icon` and `iconStroke`
// FuncMap entries when IconCatalog is configured. Missing names
// render an empty string rather than erroring — templates fail
// loud enough in dev that a typo is caught before deploy, and
// quietly omitting the SVG matches the contract uikit.Icon
// already establishes elsewhere.
func (s *Site) iconHelper(name string, size int) template.HTML {
	return s.iconStrokeHelper(name, size, iconDefaultStroke)
}

func (s *Site) iconStrokeHelper(name string, size int, stroke float64) template.HTML {
	body, ok := s.iconBody(name)
	if !ok {
		return ""
	}
	return renderIcon(body, size, stroke)
}

// iconFilledHelper backs the `iconFilled` FuncMap entry. Same
// lookup as iconHelper but the wrapper paints with
// fill="currentColor" instead of stroke — the correct shape for
// brand glyphs (WhatsApp, Spotify, …) and filled silhouettes
// (paw prints) whose inner paths leave fill unspecified and
// expect the parent <svg> to colour them. Missing names render
// an empty string, matching the icon/iconStroke contract.
func (s *Site) iconFilledHelper(name string, size int) template.HTML {
	body, ok := s.iconBody(name)
	if !ok {
		return ""
	}
	return renderIconFilled(body, size)
}

// renderIconFilled is the filled-shape sibling of renderIcon.
// Same 24×24 viewBox, but fill="currentColor" and no stroke so
// brand glyphs and silhouettes inherit the surrounding text
// colour without baking a value into the SVG.
func renderIconFilled(inner string, size int) template.HTML {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		`<svg width="%d" height="%d" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">%s</svg>`,
		size, size, inner,
	)
	return template.HTML(sb.String())
}
