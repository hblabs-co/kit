package render

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"text/template"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// jsonMarshalIndent is the seam tests swap to drive the
// json.MarshalIndent error branches in serveAASA and serveAssetLinks.
// The structures we feed are simple maps/slices of strings so the
// real call is effectively infallible — without the seam those 500
// branches stay permanently uncovered.
var jsonMarshalIndent = json.MarshalIndent

// seoNow is the seam tests swap to make security.txt's Expires field
// deterministic in golden tests. Production code reads time.Now via
// this var so a fixed clock can be injected.
var seoNow = time.Now

// seoExecute is the seam tests swap to drive the template.Execute
// error branches in the four SEO text endpoints. Templates are static
// and data shape is known so the real call is effectively infallible;
// without the seam the 500 branches stay permanently uncovered.
var seoExecute = func(t *template.Template, w io.Writer, data any) error {
	return t.Execute(w, data)
}

// seoTemplatesFS holds the embedded SEO templates. Parsed once at
// package init (one *template.Template per endpoint) and shared
// across every Site in the process — a 1M-tenant binary parses
// these four templates a grand total of four times at startup, not
// 4M times. Per-request execution carries the Site-specific
// seoContext.
//
//go:embed seo_templates/*.tmpl
var seoTemplatesFS embed.FS

// seoReadEmbedded is the inner seam tests swap to drive the
// "embedded read failure" branch inside the default seoLoadTemplate
// closure. Real reads of //go:embed paths never fail, so without the
// seam that branch stays permanently uncovered.
var seoReadEmbedded = seoTemplatesFS.ReadFile

// seoLoadTemplate is the seam tests swap to drive
// mustParseSEOTemplate's panic branch. Production reads + parses from
// the embedded FS; tests substitute a stub returning an error to
// verify the panic fires (the only path that exercises that branch
// since the real embedded templates are never missing or malformed).
var seoLoadTemplate = func(name string) (*template.Template, error) {
	path := "seo_templates/" + name + ".tmpl"
	raw, err := seoReadEmbedded(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return template.New(name).Parse(string(raw))
}

// Parsed SEO templates. text/template (not html/template) because the
// outputs are XML / plain text / markdown — html/template's auto-
// escape rules would corrupt every one of them.
var (
	seoRobotsTmpl   = mustParseSEOTemplate("robots.txt")
	seoSitemapTmpl  = mustParseSEOTemplate("sitemap.xml")
	seoLLMsTmpl     = mustParseSEOTemplate("llms.txt")
	seoSecurityTmpl = mustParseSEOTemplate("security.txt")
)

// mustParseSEOTemplate is called at package init for each SEO
// template. A failure panics — the templates ship inside the binary,
// so anything that goes wrong here is a build-time bug we want
// surfaced loudly, not at the first crawler request hours after boot.
func mustParseSEOTemplate(name string) *template.Template {
	t, err := seoLoadTemplate(name)
	if err != nil {
		panic(fmt.Sprintf("render: SEO template %s: %v", name, err))
	}
	return t
}

// Cache-Control max-age values (in seconds) for the SEO endpoints.
// Long-lived files use 24h; sitemap / well-known JSON use 1h so
// edits propagate within a normal CDN refresh cycle.
const (
	seoOneDayCacheSeconds  = 24 * 60 * 60
	seoOneHourCacheSeconds = 60 * 60
)

// securityTxtExpiryYears is the lifetime baked into the Expires:
// header of /.well-known/security.txt. RFC 9116's max is one year;
// every Site build refreshes the value automatically — no annual
// reminder to bump a hardcoded date.
const securityTxtExpiryYears = 1

// sitemapPriorityDefault / sitemapPriorityAlt are the <priority>
// values emitted in /sitemap.xml. The default lang ranks slightly
// above the alternates so search engines surface the primary
// variant when they have to pick.
const (
	sitemapPriorityDefault = "1.0"
	sitemapPriorityAlt     = "0.9"
)

// SEOOptions configures the auto-mounted SEO endpoints. Zero value
// gets every endpoint enabled with sensible defaults; tweak per-field
// to customise. Disabling everything is supported for services where
// these endpoints don't make sense (a JSON-only API, a private admin
// surface).
type SEOOptions struct {
	// Disable* lets services opt out of specific endpoints. Setting
	// any of these to true skips the mux registration entirely — the
	// path returns 404 from the catch-all instead.
	DisableRobots      bool
	DisableSitemap     bool
	DisableLLMsTxt     bool
	DisableSecurityTxt bool
	DisableAASA        bool
	DisableAssetLinks  bool

	// SecurityContact is the email shown in /.well-known/security.txt.
	// Empty → endpoint serves a placeholder + the renderer logs a
	// boot warning so the operator notices the misconfig before a
	// security researcher complains.
	SecurityContact string

	// PreferredLanguages overrides the default Preferred-Languages
	// header in security.txt. Empty → SupportedLangs is used as-is
	// (the languages we serve are the ones we accept reports in).
	PreferredLanguages []string

	// AppleTeamID + IOSBundleID drive
	// /.well-known/apple-app-site-association. Empty teamID logs a
	// warning at boot but the endpoint still serves a placeholder
	// JSON so AASA validators have something to point at.
	AppleTeamID string
	IOSBundleID string

	// AASAComponents lists the URL paths that should open the iOS
	// app via Universal Links. Empty → the renderer derives a
	// "/<lang>/*" component for every SupportedLang.
	AASAComponents []string

	// AndroidAssetLinks is the array served at
	// /.well-known/assetlinks.json. Empty → "[]" so scanners and
	// Android Studio's "App Links Assistant" don't 404 even when
	// no Android app exists yet.
	AndroidAssetLinks []AssetLink

	// LLMsTxt is the body served at /llms.txt. Non-empty → served
	// verbatim, bypassing the template. Empty → the embedded stub
	// template runs against the Site's seoContext.
	LLMsTxt string
}

// AssetLink mirrors the schema Google uses for Android App Links
// (https://developers.google.com/digital-asset-links/v1/getting-started).
type AssetLink struct {
	Relation []string        `json:"relation"`
	Target   AssetLinkTarget `json:"target"`
}

// AssetLinkTarget is the `target` sub-document of an AssetLink.
type AssetLinkTarget struct {
	Namespace              string   `json:"namespace"`
	PackageName            string   `json:"package_name,omitempty"`
	Sha256CertFingerprints []string `json:"sha256_cert_fingerprints,omitempty"`
}

// socialBots is the list of UAs that get an explicit allow-block in
// robots.txt on top of the wildcard `*`. Some social-preview crawlers
// read robots.txt with a strict parser that ignores `User-agent: *`
// when their own name is also present — spelling each one out removes
// that ambiguity for og:* tag fetches.
var socialBots = []string{
	"facebookexternalhit", "facebookcatalog", "meta-externalagent",
	"Twitterbot", "LinkedInBot",
	"Slackbot", "Slackbot-LinkExpanding",
	"Discordbot", "TelegramBot",
	"WhatsApp", "Applebot",
}

// seoContext is the data every SEO template gets executed against.
// Built once per Site at New() time (cached on *Site.seoCtx) since
// every field depends only on Site config + page list — all known
// at boot, none per-request.
type seoContext struct {
	Brand                 string
	CanonicalHost         string
	SupportedLangs        []string
	DefaultLang           string
	SocialBots            []string
	SitemapURLs           []seoSitemapURL
	SecurityContact       string
	SecurityIsPlaceholder bool
	PreferredLanguages    []string
	Expires               string
}

type seoSitemapURL struct {
	Loc        string
	Priority   string
	Alternates []seoSitemapAlt
	XDefault   string
}

type seoSitemapAlt struct {
	Lang string
	Href string
}

// buildSEO precomputes the seoContext for this Site. Called from New
// after loadPages (sitemap depends on s.pages). Idempotent; safe to
// re-run if the Site is rebuilt (dev mode).
func (s *Site) buildSEO() {
	contact := s.seo.SecurityContact
	placeholder := contact == ""
	if placeholder {
		contact = "security@example.invalid"
	}
	langs := s.seo.PreferredLanguages
	if len(langs) == 0 {
		langs = s.supportedLangs
	}

	keys := make([]string, 0, len(s.pages))
	for k := range s.pages {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	urls := make([]seoSitemapURL, 0, len(keys)*len(s.supportedLangs))
	for _, key := range keys {
		for _, lang := range s.supportedLangs {
			priority := sitemapPriorityAlt
			if lang == s.defaultLang {
				priority = sitemapPriorityDefault
			}
			alts := make([]seoSitemapAlt, 0, len(s.supportedLangs))
			for _, alt := range s.supportedLangs {
				alts = append(alts, seoSitemapAlt{
					Lang: alt,
					Href: s.canonicalHost + s.pageURL(alt, key),
				})
			}
			urls = append(urls, seoSitemapURL{
				Loc:        s.canonicalHost + s.pageURL(lang, key),
				Priority:   priority,
				Alternates: alts,
				XDefault:   s.canonicalHost + s.pageURL(s.defaultLang, key),
			})
		}
	}

	s.seoCtx = &seoContext{
		Brand:                 s.brand,
		CanonicalHost:         s.canonicalHost,
		SupportedLangs:        s.supportedLangs,
		DefaultLang:           s.defaultLang,
		SocialBots:            socialBots,
		SitemapURLs:           urls,
		SecurityContact:       contact,
		SecurityIsPlaceholder: placeholder,
		PreferredLanguages:    langs,
		Expires:               seoNow().UTC().AddDate(securityTxtExpiryYears, 0, 0).Format(time.RFC3339),
	}
}

// mountSEO registers every enabled SEO endpoint on the mux. Called
// from Site.Mount before the catch-all "/" so the specific patterns
// (/robots.txt, /sitemap.xml, etc.) win the dispatch.
func (s *Site) mountSEO(mux *http.ServeMux) {
	if !s.seo.DisableRobots {
		mux.HandleFunc("/robots.txt", s.serveRobots)
	}
	if !s.seo.DisableSitemap {
		mux.HandleFunc("/sitemap.xml", s.serveSitemap)
	}
	if !s.seo.DisableLLMsTxt {
		mux.HandleFunc("/llms.txt", s.serveLLMsTxt)
	}
	if !s.seo.DisableSecurityTxt {
		// Canonical location plus the legacy /security.txt fallback
		// some old scanners still probe at the root before checking
		// /.well-known/.
		mux.HandleFunc("/.well-known/security.txt", s.serveSecurityTxt)
		mux.HandleFunc("/security.txt", s.serveSecurityTxt)
	}
	if !s.seo.DisableAASA {
		// IMPORTANT: must be served as application/json with NO `.json`
		// extension in the URL. iOS rejects redirects, so /.well-known
		// is the canonical (and only) location — never a 301.
		mux.HandleFunc("/.well-known/apple-app-site-association", s.serveAASA)
	}
	if !s.seo.DisableAssetLinks {
		mux.HandleFunc("/.well-known/assetlinks.json", s.serveAssetLinks)
	}
}

// warnSEO emits boot warnings for SEO config that's missing but
// should be set in any real deployment. Skipped in dev mode because
// the dev wrapper rebuilds Site on every request — the warning would
// fire dozens of times per minute.
func (s *Site) warnSEO() {
	if s.dev {
		return
	}
	if !s.seo.DisableSecurityTxt && s.seo.SecurityContact == "" {
		log.Warn("render: SEO.SecurityContact is unset — security.txt will serve a placeholder",
			"endpoint", "/.well-known/security.txt")
	}
	if !s.seo.DisableAASA && s.seo.AppleTeamID == "" {
		log.Warn("render: SEO.AppleTeamID is unset — Universal Links will not validate, AASA serves a placeholder",
			"endpoint", "/.well-known/apple-app-site-association")
	}
}

// serveSEOTemplate is the shared executor for the four text-format
// endpoints. The template name is read from t.Name() for logging on
// failure — no extra arg threads through every call site.
func (s *Site) serveSEOTemplate(w http.ResponseWriter, t *template.Template, contentType string, maxAge int) {
	w.Header().Set(constants.HeaderContentType, contentType)
	w.Header().Set(constants.HeaderCacheControl, fmt.Sprintf("public, max-age=%d", maxAge))

	var buf bytes.Buffer
	if err := seoExecute(t, &buf, s.seoCtx); err != nil {
		log.Error("render: SEO template execute failed", "template", t.Name(), "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// serveRobots answers crawler probes. Template lives at
// seo_templates/robots.txt.tmpl — diff that file, not Go code, to
// tweak the output.
func (s *Site) serveRobots(w http.ResponseWriter, _ *http.Request) {
	s.serveSEOTemplate(w, seoRobotsTmpl, "text/plain; charset=utf-8", seoOneDayCacheSeconds)
}

// serveSitemap emits one <url> per (page, lang) pair with hreflang
// alternates pointing at the other language variants, plus an
// x-default that resolves to DefaultLang. Page list comes from
// s.pages (file-based routing in pages/), pre-sorted in buildSEO so
// the rendered XML is deterministic.
func (s *Site) serveSitemap(w http.ResponseWriter, _ *http.Request) {
	s.serveSEOTemplate(w, seoSitemapTmpl, "application/xml; charset=utf-8", seoOneHourCacheSeconds)
}

// serveLLMsTxt answers /llms.txt — an emerging convention (2024-2025)
// that gives LLM crawlers a curated, structured summary of the site.
// When SEOOptions.LLMsTxt is non-empty we serve it verbatim (services
// write their own markdown and embed it). Empty → the stub template
// renders a minimal default built from CanonicalHost + SupportedLangs.
func (s *Site) serveLLMsTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(constants.HeaderContentType, "text/markdown; charset=utf-8")
	w.Header().Set(constants.HeaderCacheControl, fmt.Sprintf("public, max-age=%d", seoOneDayCacheSeconds))

	if s.seo.LLMsTxt != "" {
		_, _ = w.Write([]byte(s.seo.LLMsTxt))
		return
	}
	var buf bytes.Buffer
	if err := seoExecute(seoLLMsTmpl, &buf, s.seoCtx); err != nil {
		log.Error("render: SEO template execute failed", "template", seoLLMsTmpl.Name(), "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// serveSecurityTxt answers /.well-known/security.txt (RFC 9116) and
// the legacy /security.txt fallback some scanners still probe.
// Expires is computed once at Site build time (seoNow + 1 year), so
// every fresh build refreshes the value automatically; the per-
// request handler just renders the template.
func (s *Site) serveSecurityTxt(w http.ResponseWriter, _ *http.Request) {
	s.serveSEOTemplate(w, seoSecurityTmpl, "text/plain; charset=utf-8", seoOneDayCacheSeconds)
}

// aasaDoc is the JSON shape iOS expects at
// /.well-known/apple-app-site-association. Keep field tags exact —
// Apple's parser is strict.
type aasaDoc struct {
	Applinks struct {
		Details []aasaDetail `json:"details"`
	} `json:"applinks"`
}

type aasaDetail struct {
	AppIDs     []string            `json:"appIDs"`
	Components []map[string]string `json:"components"`
}

// serveAASA answers /.well-known/apple-app-site-association. Stays
// on the Go struct + json.MarshalIndent path (not a template) because
// JSON shape is well-typed and the marshal is bulletproof; emitting
// JSON via text/template would mean hand-rolling escaping and comma
// placement for negligible win.
func (s *Site) serveAASA(w http.ResponseWriter, _ *http.Request) {
	teamID := s.seo.AppleTeamID
	if teamID == "" {
		teamID = "XXXXXXXXXX"
	}
	bundleID := s.seo.IOSBundleID
	if bundleID == "" {
		bundleID = "co.example.placeholder"
	}

	components := s.seo.AASAComponents
	if len(components) == 0 {
		components = make([]string, 0, len(s.supportedLangs))
		for _, lang := range s.supportedLangs {
			components = append(components, "/"+lang+"/*")
		}
	}

	doc := aasaDoc{}
	detail := aasaDetail{
		AppIDs: []string{teamID + "." + bundleID},
	}
	for _, c := range components {
		detail.Components = append(detail.Components, map[string]string{"/": c})
	}
	doc.Applinks.Details = append(doc.Applinks.Details, detail)

	body, err := jsonMarshalIndent(doc, "", "  ")
	if err != nil {
		log.Error("render: marshal AASA failed", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set(constants.HeaderContentType, "application/json")
	w.Header().Set(constants.HeaderCacheControl, fmt.Sprintf("public, max-age=%d", seoOneHourCacheSeconds))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// serveAssetLinks answers /.well-known/assetlinks.json. Same
// rationale as serveAASA — Go struct + marshal beats template for
// JSON output.
func (s *Site) serveAssetLinks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(constants.HeaderContentType, "application/json")
	w.Header().Set(constants.HeaderCacheControl, fmt.Sprintf("public, max-age=%d", seoOneHourCacheSeconds))

	links := s.seo.AndroidAssetLinks
	if links == nil {
		links = []AssetLink{}
	}
	body, err := jsonMarshalIndent(links, "", "  ")
	if err != nil {
		log.Error("render: marshal assetlinks failed", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}
