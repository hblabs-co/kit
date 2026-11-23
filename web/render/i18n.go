package render

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/hblabs-co/kit/core/constants"
)

// langCookie is the cookie name we use to remember the visitor's
// language pick. Keep it stable — renaming evicts everyone's cookie.
const langCookie = "hbl_lang"

// langCookieMaxAge keeps the language preference for one year — long
// enough that returning visitors stay on their pick, short enough
// that a permanently-abandoned cookie eventually rotates without
// server-side state. http.Cookie.MaxAge expects seconds.
const langCookieMaxAge = 365 * 24 * 60 * 60

// loadLocales reads locales/<code>.json for every entry in
// SupportedLangs. A missing file is fatal (we'd silently fall back to
// the default lang at runtime, leaving translations as raw keys —
// way better to fail at boot).
func (s *Site) loadLocales() error {
	s.locales = map[string]map[string]string{}
	for _, lang := range s.supportedLangs {
		path := fmt.Sprintf("locales/%s.json", lang)
		raw, err := fs.ReadFile(s.fs, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		s.locales[lang] = m
	}
	return nil
}

// translate looks up `key` in `lang`, falling back to defaultLang and
// finally to the raw key so a missing translation is visible (rather
// than silently rendering an empty string). Templates call it via
// the `T` helper in funcmap.go.
func (s *Site) translate(lang, key string) string {
	if v := s.locales[lang][key]; v != "" {
		return v
	}
	if v := s.locales[s.defaultLang][key]; v != "" {
		return v
	}
	return key
}

// pickLang resolves the visitor's preferred language using, in order:
//
//  1. The hbl_lang cookie if it points to a still-supported lang.
//  2. The first 2-letter prefix in Accept-Language we support.
//  3. The site's DefaultLang.
//
// We deliberately don't parse quality values (q=...) — the order
// browsers send is good enough and worth the simpler code.
func (s *Site) pickLang(r *http.Request) string {
	if c, _ := r.Cookie(langCookie); c != nil && contains(s.supportedLangs, c.Value) {
		return c.Value
	}

	for _, tag := range strings.Split(r.Header.Get(constants.HeaderAcceptLanguage), ",") {
		tag = strings.TrimSpace(tag)
		// strip ";q=..."
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		if len(tag) >= 2 && contains(s.supportedLangs, strings.ToLower(tag[:2])) {
			return strings.ToLower(tag[:2])
		}
	}

	return s.defaultLang
}

// setLangCookie pins the visitor's lang choice for a year. Lax SameSite
// is fine — the cookie only affects which translation we render and
// never carries any auth signal.
func (s *Site) setLangCookie(w http.ResponseWriter, lang string) {
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    lang,
		Path:     "/",
		MaxAge:   langCookieMaxAge,
		HttpOnly: false, // not a security cookie; readable by JS for client-side toggles
		SameSite: http.SameSiteLaxMode,
	})
}

// clearLangCookieIfInvalid wipes a stale cookie pointing at an
// unsupported lang. Called from the 404 path so a visitor with a
// retired lang cookie doesn't get stuck on a phantom value forever.
func (s *Site) clearLangCookieIfInvalid(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(langCookie)
	if err != nil || c == nil {
		return
	}
	if contains(s.supportedLangs, c.Value) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
}
