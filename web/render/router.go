package render

import (
	"net/http"
	"strings"
)

// handleRoot is the catch-all dispatcher for paths the framework
// owns. Behaviour:
//
//	/                       → pick a lang, set cookie, 302 to /<lang>/
//	/<lang>/                → render index page in that lang
//	/<lang>/<page>          → render that page (must exist in pages/)
//	/<lang>/<page>/         → same as above (trailing slash tolerated)
//	anything else           → 404 (no redirect, no input reflection)
//
// Returning 404 for unknown paths instead of redirecting keeps crawler
// probes (/.env.js, /wp-admin) from looping through pickLang on every
// hit. Tampered/stale lang cookies (e.g. someone retired a locale
// while a visitor still had it cached) get cleared so they don't keep
// the visitor on a phantom value forever.
func (s *Site) handleRoot(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")

	// Bare / → pick a language and redirect into it.
	if path == "" {
		lang := s.pickLang(r)
		s.setLangCookie(w, lang)
		http.Redirect(w, r, "/"+lang+"/", http.StatusFound)
		return
	}

	first, rest, _ := strings.Cut(path, "/")

	// First segment must be a supported lang; otherwise 404. This
	// also blocks attackers stuffing arbitrary strings into the path
	// hoping we'll reflect them anywhere.
	if !contains(s.supportedLangs, first) {
		s.clearLangCookieIfInvalid(w, r)
		http.NotFound(w, r)
		return
	}
	lang := first

	// `rest` is the page key. Trim trailing slash so /es/about and
	// /es/about/ both render the same page. Empty rest = index page.
	pageKey := strings.TrimSuffix(rest, "/")

	tmpl, params, ok := s.resolvePage(pageKey)
	if !ok {
		// Lang was valid but neither an exact-match page nor a
		// dynamic-route (pages/<dir>/[slug]) matches — refresh
		// the cookie (the visitor's lang preference is fine)
		// and 404.
		s.setLangCookie(w, lang)
		http.NotFound(w, r)
		return
	}

	s.setLangCookie(w, lang)
	s.renderPage(w, r, tmpl, pageKey, lang, params)
}
