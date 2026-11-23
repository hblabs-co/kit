package render

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hblabs-co/kit/web/render/og"
)

// ogNew is the seam tests swap to drive the og.New error branch in
// buildOG. og.New only fails when DefaultTheme can't load the
// embedded font — in tests with embedded gobold the real call
// always succeeds, so the wrap is otherwise unreachable.
var ogNew = og.New

// OGOptions configures the auto-mounted OG image pipeline. Zero
// value disables OG entirely — Mount won't register the handler and
// the {{ ogImage }} helper returns an empty string. Opt in with
// Enabled=true so an empty struct doesn't accidentally enable a
// bunch of defaults.
type OGOptions struct {
	// Enabled flips the pipeline on. Required because the zero
	// value of OGOptions otherwise looks indistinguishable from
	// "use defaults".
	Enabled bool

	// Brand is the line shown above the title in every card.
	// Typically the product / site name. Empty disables the brand
	// line in the rendered image.
	Brand string

	// URLPrefix is the route the handler mounts under. "" → "/og".
	// Trailing slashes are tolerated; the helper and resolver agree
	// on the trimmed form.
	URLPrefix string

	// Storage is where rendered cards live. nil → an in-process
	// MemoryStorage seeded at boot, sufficient for local dev. In
	// prod, pass the MinIO-backed Storage (packages/storageclient).
	Storage og.Storage

	// Themes registers extra og.Themes alongside the default. nil
	// means just the default theme; Cards then leave Theme="".
	Themes map[string]og.Theme
}

// buildOG initialises the og.Generator + Storage from opts and
// stashes them on the Site for Mount + the funcmap helper. Called
// from New when opts.OG.Enabled. Returns a hard error so a bad
// theme doesn't silently disable OG at runtime.
func (s *Site) buildOG(opts OGOptions) error {
	if !opts.Enabled {
		return nil
	}
	gen, err := ogNew(og.Options{
		Themes: opts.Themes,
	})
	if err != nil {
		return fmt.Errorf("og generator: %w", err)
	}
	prefix := strings.TrimSuffix(opts.URLPrefix, "/")
	if prefix == "" {
		prefix = "/og"
	}
	store := opts.Storage
	if store == nil {
		store = og.NewMemoryStorage(prefix)
	}
	s.og = gen
	s.ogStorage = store
	s.ogPrefix = prefix
	s.ogBrand = opts.Brand
	s.ogEnabled = true
	return nil
}

// mountOG registers the handler at "<prefix>/" so requests for
// "<prefix>/<lang>/<page>.png" land on the resolver. Mount is the
// single caller; this function exists to keep the wiring near the
// types it touches.
func (s *Site) mountOG(mux *http.ServeMux) {
	if !s.ogEnabled {
		return
	}
	handler := og.NewHandler(og.HandlerOptions{
		Generator: s.og,
		Storage:   s.ogStorage,
		Resolver:  s.ogResolver,
	})
	mux.Handle(s.ogPrefix+"/", handler)
}

// ogResolver parses "<prefix>/<lang>/<page>.png" into the Card the
// generator should render. Returns false on any shape mismatch
// (unknown lang, unknown page, missing .png) so the handler replies
// 404 — better than rendering a card for a page that doesn't exist.
func (s *Site) ogResolver(r *http.Request) (og.Card, bool) {
	p := strings.TrimPrefix(r.URL.Path, s.ogPrefix)
	p = strings.TrimPrefix(p, "/")
	if !strings.HasSuffix(p, ".png") {
		return og.Card{}, false
	}
	p = strings.TrimSuffix(p, ".png")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) != 2 {
		return og.Card{}, false
	}
	lang, pageURLKey := parts[0], parts[1]
	if !contains(s.supportedLangs, lang) {
		return og.Card{}, false
	}
	pageKey := pageURLKey
	if pageKey == "index" {
		pageKey = ""
	}
	if _, ok := s.pages[pageKey]; !ok {
		return og.Card{}, false
	}
	return s.cardFor(pageKey, lang), true
}

// cardFor builds the Card for (pageKey, lang) by consulting the
// locale map. Per-page keys take precedence:
//
//	og.<page>.title    → og.<page>.subtitle
//	site.title         → site.description     (fallback)
//
// `index` is the URL form of the empty pageKey so a single locale
// scheme covers both the home page and named pages.
func (s *Site) cardFor(pageKey, lang string) og.Card {
	label := pageKey
	if label == "" {
		label = "index"
	}
	title := s.localeOr(lang,
		"og."+label+".title",
		"site.title",
	)
	subtitle := s.localeOr(lang,
		"og."+label+".subtitle",
		"site.description",
	)
	return og.Card{
		Title:    title,
		Subtitle: subtitle,
		Brand:    s.ogBrand,
		Lang:     lang,
	}
}

// localeOr looks up keys in lang then defaultLang, returning the
// first non-empty hit. Values that resolve to the raw key (i.e.
// `translate` couldn't find them) are skipped so the fallback gets
// a chance to win. Returns the last key as a final fallback so the
// rendered card never carries an empty title.
func (s *Site) localeOr(lang string, keys ...string) string {
	for _, k := range keys {
		if v := s.locales[lang][k]; v != "" {
			return v
		}
		if v := s.locales[s.defaultLang][k]; v != "" {
			return v
		}
	}
	if len(keys) > 0 {
		return keys[len(keys)-1]
	}
	return ""
}

// ogImageURL is the value backing the {{ ogImage }} template helper.
// Returns "" when OG is disabled so layouts can guard with
// {{ if ogImage .PageKey .Lang }}{{ end }} without a separate flag.
func (s *Site) ogImageURL(pageKey, lang string) string {
	if !s.ogEnabled {
		return ""
	}
	p := pageKey
	if p == "" {
		p = "index"
	}
	return s.ogPrefix + "/" + lang + "/" + p + ".png"
}
