// Package render is HB Labs' own opinionated framework for
// serving static, multi-language sites with Go html/template. The
// goal is the convenience of Next.js (file-based routing, i18n,
// asset versioning, SEO files, OG images) without leaving stdlib +
// a hand-rolled CSS pipeline.
//
// # Project layout
//
// Every site that uses this framework keeps the same on-disk shape;
// the loader walks subtrees by name, so renaming a directory means
// rewriting code in render. Don't.
//
//	myservice/
//	├── pages/                     html/template files (file-based routing)
//	│   ├── index.html.tmpl        → /{lang}/
//	│   └── about.html.tmpl        → /{lang}/about
//	├── partials/
//	│   └── _layout.html.tmpl      template named "_layout" — wraps every page
//	├── locales/                   one JSON per supported language
//	│   ├── es.json
//	│   ├── de.json
//	│   ├── en.json
//	│   └── nl.json
//	├── static/                    served at /static/, cached immutable
//	├── config/
//	│   └── config.yaml            framework config (see render/config)
//	├── seo/                       optional, e.g. seo/llms.txt
//	└── main.go
//
// Conventions:
//
//   - Pages with a leading "_" in the filename are skipped (treated as
//     partials/layouts). The layout file therefore lives at
//     partials/_layout.html.tmpl.
//   - Page templates define a "content" block; the layout renders it
//     via {{ template "content" . }}.
//   - Every locales/<code>.json must be present for the codes listed
//     in supported_langs — a missing file is a boot-time error.
//
// # Wiring (recommended)
//
// Use the builder to load config from disk, layer in the code-only
// bits (FS, dev-mode toggle, og.Storage), and assemble the *Site:
//
//	//go:embed pages all:partials locales static config
//	var siteFS embed.FS
//
//	site, err := render.Init(siteFS).
//	    LoadConfig("config/config.yaml").
//	    Build()
//	if err != nil { log.Fatal(err) }
//
//	mux := http.NewServeMux()
//	site.Mount(mux)
//
// The `all:partials` pattern is required because embed.FS skips
// underscore-prefixed entries by default; the framework relies on
// that underscore convention for partials, so we override the embed
// default and keep the convention.
//
// Builder steps:
//
//   - render.Init(fs.FS) — pin the source FS (embed.FS in prod,
//     os.DirFS in dev).
//   - .LoadConfig(path) — strict-mode YAML parse via
//     web/render/config. Unknown keys / malformed files fail at
//     boot, not at runtime.
//   - .WithDev(bool) — flip the {{ devScript }} hot-reload helper.
//     Typically `env == "local"`.
//   - .WithOGStorage(og.Storage) — inject prod storage (MinIO/S3).
//     Omit in dev to fall back to the in-process MemoryStorage.
//   - .WithOGThemes(map[string]og.Theme) — register extra themes.
//   - .Build() (*Site, error) — final step.
//   - .BuildOptions() (Options, error) — same merge, no construction.
//     Used by render/dev which builds a fresh *Site per request.
//
// # config.yaml
//
// The on-disk surface is documented in web/render/config.
// Defaults live in render.New (one source of truth) — leaving a key
// out keeps the Go zero value, which the framework then defaults the
// same way it would for an all-in-code main.
//
// # Wiring from code (no YAML)
//
// render.New is the lower-level API the builder calls into. Use it
// directly when every field is computed in code (typical for tests,
// migration jobs, embedded use cases that don't ship a config file):
//
//	site, _ := render.New(render.Options{
//	    FS:             siteFS,
//	    CanonicalHost:  "https://hblabs.co",
//	    DefaultLang:    "es",
//	    SupportedLangs: []string{"es", "de", "en", "nl"},
//	})
//
// # Sub-packages
//
//   - render/config: YAML schema + Load() helper.
//   - render/og: Open Graph image generator + storage backends.
//   - render/dev: hot-reload wrapper for local development.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
	"github.com/hblabs-co/kit/web/render/og"
)

// Cache-Control max-age values (in seconds) used across the rendered
// surface. Pulled out so each Set() reads as intent and tweaking the
// policy is one place to grep.
const (
	// pageMaxAgeSeconds is the cache lifetime for HTML pages. Short
	// enough that translation / template tweaks roll out within a
	// minute; long enough that a refresh hammer doesn't re-execute
	// every request.
	pageMaxAgeSeconds = 60

	// immutableMaxAgeSeconds = 1 year, the upper bound recommended
	// for `immutable` static assets. Safe because every URL we emit
	// for a static asset ends in ?v=<assetVer>; a redeploy flips the
	// version and clients refetch.
	immutableMaxAgeSeconds = 365 * 24 * 60 * 60
)

// Options configures a Site at boot. Most fields have sensible defaults
// for local dev so a service can spin up with just FS + SupportedLangs.
type Options struct {
	// FS is the embed.FS (or any fs.FS) rooted at the service directory
	// and containing pages/, partials/, locales/, static/.
	FS fs.FS

	// CanonicalHost is the absolute origin used in links, sitemap and
	// OG metadata. Example: "https://launch.hblabs.co".
	CanonicalHost string

	// DefaultLang is what we serve when the visitor has no cookie and no
	// recognisable Accept-Language header. Must appear in SupportedLangs.
	DefaultLang string

	// SupportedLangs lists the BCP-47-ish two-letter codes we serve. Each
	// must have a matching locales/<code>.json file.
	SupportedLangs []string

	// Brand is the site's identity name passed to layouts as
	// {{ .Brand }}. Also used as the default OG card brand when
	// OG.Brand is empty — one source of truth for the visible
	// product name across nav, footer, social previews.
	Brand string

	// Dev opts the site into development affordances. The only effect
	// today is the {{ devScript }} template helper emitting the
	// hot-reload <script> tag — everything else (file watching, SSE)
	// lives in web/render/dev. Layouts call {{ devScript }} in
	// every environment; in prod it's an empty string, in dev it's
	// the tag the dev wrapper expects.
	Dev bool

	// SEO controls the auto-mounted /robots.txt, /sitemap.xml,
	// /llms.txt, /security.txt, /.well-known/security.txt,
	// /.well-known/apple-app-site-association and
	// /.well-known/assetlinks.json endpoints. Zero value = every
	// endpoint enabled with sensible defaults; tweak per-field for
	// per-site customisation. See SEOOptions.
	SEO SEOOptions

	// OG configures the auto-mounted Open Graph image pipeline. Zero
	// value (Enabled=false) disables it; set Enabled=true to register
	// the /og handler and unlock the {{ ogImage }} template helper.
	// See OGOptions.
	OG OGOptions

	// FuncMap is an optional set of template helpers a service wants
	// exposed inside its .html.tmpl files alongside the framework's
	// own (T, asset, pageURL, canonical, ogImage, devScript). Use it
	// for service-specific catalogues — a list of projects, a menu
	// of features, anything where the alternative is repeating large
	// HTML blocks in the template or shoving structured data through
	// the flat locales map.
	//
	// Precedence: framework helpers always win. If a caller registers
	// a key that collides with a built-in (e.g. "T"), the built-in is
	// kept and the override is silently dropped — templates rely on
	// the framework helpers' signatures and letting a service shadow
	// them would surface as obscure runtime errors. Add new helpers
	// under names that won't collide; if you need to replace a
	// built-in, that's a framework-level change, not a service one.
	FuncMap template.FuncMap

	// Data is a free-form key/value tree exposed to templates via
	// the `{{ data "dot.path" }}` helper. Loaded from config.yaml's
	// `data:` section (see render/config), or populated directly in
	// code for tests / non-YAML callers. Leaves can be any YAML
	// scalar (string, int, bool, …); the helper stringifies them
	// before returning template.HTML so SVG bodies and other inline
	// markup pass through unescaped.
	//
	// Values come from operator-controlled YAML, NOT user input —
	// treat the leaves as trusted bytes; do not put visitor-supplied
	// content into this tree.
	Data map[string]any

	// IconCatalog optionally registers a `name → inner-SVG` catalog
	// that the framework's `{{ icon name size }}` / `{{ iconStroke
	// name size stroke }}` helpers resolve against. When non-nil,
	// New registers both helpers; the resolver looks up Data["icons"]
	// first (per-service extensions / overrides from config.yaml)
	// then IconCatalog (the shared catalog the service plugged in).
	// When nil, neither helper is registered — templates that need
	// icons either receive their catalog from a service-supplied
	// FuncMap or use `{{ data "icons.<name>" }}` to read the raw
	// markup.
	//
	// Generic map keeps render independent of any particular icon
	// package; services typically pass web/uikit.IconSet,
	// but a client that ships its own catalog just plugs that map
	// in instead.
	IconCatalog map[string]string

	// Ready optionally backs the auto-mounted GET /readyz endpoint
	// (the orchestrator's readiness probe, as opposed to /healthz
	// liveness). Return nil when the service can take traffic; any
	// error answers 503 with the error text. Code-only — a readiness
	// check is a live dependency probe (upstream API, database), not
	// YAML config. nil = /readyz mirrors /healthz semantics (always
	// 200 once the binary serves), which is correct for services
	// without a hard runtime dependency.
	//
	// The probe runs with a short timeout derived from the request
	// context, so implementations should honour ctx cancellation.
	Ready func(ctx context.Context) error

	// ExtraStatic mounts additional fs.FS subtrees alongside the
	// service's own static/ directory. Each entry's key becomes a
	// URL-path segment under /static/, and the fs.FS is served with
	// the same cache-immutable policy and content-hash fingerprinting
	// the framework already applies to static/.
	//
	// Use it to plug in cross-service shared assets — a UI kit, a
	// brand pack, fonts published from a single Go package — without
	// copying bytes into each service's embed:
	//
	//	import "github.com/hblabs-co/kit/web/uikit"
	//
	//	render.Options{
	//	    ExtraStatic: map[string]fs.FS{"uikit": uikit.FS},
	//	}
	//
	// Templates pull files via the standard asset helper, prefixed
	// with the same key — fingerprinting + cache-immutable headers
	// are identical to local static:
	//
	//	{{ asset "uikit/css/tokens.css" }}
	//
	// Prefix rules: non-empty, no slashes ("/" splits it into
	// directory segments which would break the mount). Collisions
	// with file names under the service's own static/ are not
	// detected; pick prefixes that won't clash (the empty prefix
	// would shadow everything, hence the validation).
	ExtraStatic map[string]fs.FS
}

// Site is a configured render instance. Build with New, then Mount.
// One Site corresponds to one service binary's HTML surface.
type Site struct {
	fs             fs.FS
	canonicalHost  string
	defaultLang    string
	supportedLangs []string
	brand          string
	dev            bool
	seo            SEOOptions

	pages        map[string]*template.Template // route key ("" for index, "about", ...)
	dynamicPages []dynamicRoute                // templates whose key contains [param] segments — see dynamic.go
	locales      map[string]map[string]string  // lang → key → translated string
	assetVer     string                        // cache-buster appended to URLs that aren't in fingerprints
	fingerprints map[string]string             // logical static path → 8-hex-char content hash
	funcMap      template.FuncMap
	extraStatic  map[string]fs.FS  // URL-segment prefix → fs.FS served under /static/<prefix>/
	data         map[string]any    // operator-supplied tree exposed via the `data` helper
	iconCatalog  map[string]string // optional name→inner-SVG catalog backing `{{ icon }}`

	ready func(ctx context.Context) error // optional readiness probe behind /readyz; nil = healthz semantics

	seoCtx *seoContext // built at boot by buildSEO; data passed to SEO templates

	og        *og.Generator // nil when OGOptions.Enabled is false
	ogStorage og.Storage    // nil when OGOptions.Enabled is false
	ogPrefix  string        // route prefix the handler mounts under, e.g. "/og"
	ogBrand   string        // brand line baked into every card
	ogEnabled bool          // shortcut so helpers can early-return
}

// New parses templates and locales. Errors are wrapped so the caller
// can fail-fast at boot — a parse error in production means the
// binary should not start serving partial content.
func New(opts Options) (*Site, error) {
	if opts.FS == nil {
		return nil, errors.New("render: Options.FS is required")
	}
	if len(opts.SupportedLangs) == 0 {
		return nil, errors.New("render: Options.SupportedLangs cannot be empty")
	}
	if opts.DefaultLang == "" {
		opts.DefaultLang = opts.SupportedLangs[0]
	}
	if !contains(opts.SupportedLangs, opts.DefaultLang) {
		return nil, fmt.Errorf("render: DefaultLang %q not in SupportedLangs", opts.DefaultLang)
	}

	for prefix := range opts.ExtraStatic {
		if prefix == "" || strings.ContainsRune(prefix, '/') {
			return nil, fmt.Errorf("render: ExtraStatic prefix %q invalid (non-empty, no slashes)", prefix)
		}
	}

	s := &Site{
		fs:             opts.FS,
		canonicalHost:  opts.CanonicalHost,
		defaultLang:    opts.DefaultLang,
		supportedLangs: opts.SupportedLangs,
		brand:          opts.Brand,
		dev:            opts.Dev,
		seo:            opts.SEO,
		pages:          map[string]*template.Template{},
		assetVer:       strconv.FormatInt(time.Now().UnixNano(), 10),
		extraStatic:    opts.ExtraStatic,
		data:           opts.Data,
		iconCatalog:    opts.IconCatalog,
		ready:          opts.Ready,
	}
	s.funcMap = s.buildFuncMap()
	for name, fn := range opts.FuncMap {
		// Framework helpers win over service overrides — see Options.FuncMap.
		if _, taken := s.funcMap[name]; taken {
			continue
		}
		s.funcMap[name] = fn
	}

	if err := s.loadLocales(); err != nil {
		return nil, fmt.Errorf("render: load locales: %w", err)
	}
	if err := s.loadPages(); err != nil {
		return nil, fmt.Errorf("render: load pages: %w", err)
	}
	if err := s.loadFingerprints(); err != nil {
		return nil, fmt.Errorf("render: fingerprint static/: %w", err)
	}
	if err := s.buildOG(opts.OG); err != nil {
		return nil, fmt.Errorf("render: build OG pipeline: %w", err)
	}
	s.buildSEO()
	s.warnSEO()
	return s, nil
}

// Mount wires the site's routes onto the given mux. Caller is free to
// register additional routes before or after this call (e.g. an API
// endpoint at /api/...). Order:
//
//	GET /healthz                → 200 ok (liveness)
//	GET /readyz                 → 200 ok, or 503 when draining or Options.Ready errors (readiness)
//	GET /static/...             → embedded assets, cached immutable
//	GET /                       → pickLang and 302 to /<lang>/
//	GET /<lang>/[<page>]        → render the matching page template
//	anything else               → 404
func (s *Site) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(constants.HeaderContentType, "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", s.handleReadyz)

	staticSub, err := fsSub(s.fs, "static")
	if err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", cacheImmutable(http.FileServerFS(staticSub))))
	}

	// ExtraStatic mounts after the main /static/ handler — ServeMux's
	// longest-match wins, so /static/<prefix>/ requests route here
	// while everything else still falls through to the service's own
	// static FS above.
	for prefix, fsys := range s.extraStatic {
		mountPath := "/static/" + prefix + "/"
		mux.Handle(mountPath, http.StripPrefix(mountPath, cacheImmutable(http.FileServerFS(fsys))))
	}

	s.mountSEO(mux)
	s.mountOG(mux)
	mux.HandleFunc("/", s.handleRoot)
}

// readyProbeTimeout bounds a single /readyz dependency probe so a hung
// upstream turns into a fast 503 instead of a stuck kubelet probe.
const readyProbeTimeout = 2 * time.Second

// handleReadyz answers the readiness probe. While the process drains
// after its shutdown signal (system.Draining) it answers 503
// "draining" without probing anything. Otherwise, without a configured
// Options.Ready it mirrors /healthz (200 "ok" once the binary serves —
// right for sites with no hard runtime dependency); with one, the probe
// decides, and its error text is surfaced in the 503 body so a failing
// rollout is diagnosable from the probe log alone.
func (s *Site) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(constants.HeaderContentType, "text/plain; charset=utf-8")
	if system.Draining() {
		// Shutdown signal received: take this instance out of the
		// load balancer while in-flight requests finish.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining"))
		return
	}
	if s.ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readyProbeTimeout)
		defer cancel()
		if err := s.ready(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "not ready: %v", err)
			return
		}
	}
	_, _ = w.Write([]byte("ok"))
}

// loadPages walks pages/ in the FS, parsing each *.html.tmpl that
// doesn't start with "_". Each page is parsed together with the
// _layout partial so it can rely on `{{ template "_layout" . }}` from
// inside its `content` block — and so a parse error in either file
// surfaces at boot, not at first request.
func (s *Site) loadPages() error {
	const layoutPath = "partials/_layout.html.tmpl"
	// Read the layout once up front. Used to be ParseFS'd alongside
	// each page, but ParseFS goes through fs.Glob, which treats
	// "[" and "]" in the pattern as character-class metachars —
	// so a dynamic-route filename like reviews/[slug].html.tmpl
	// would silently match zero files and the route would never
	// load. Reading raw bytes + Parse sidesteps the glob and
	// works for any path the filesystem can list.
	layoutBody, err := fs.ReadFile(s.fs, layoutPath)
	if err != nil {
		return fmt.Errorf("missing required %s: %w", layoutPath, err)
	}

	return fs.WalkDir(s.fs, "pages", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".html.tmpl") {
			return nil
		}
		if strings.HasPrefix(name, "_") {
			return nil
		}

		body, err := fs.ReadFile(s.fs, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		t, err := template.New(name).Funcs(s.funcMap).Parse(string(layoutBody))
		if err != nil {
			return fmt.Errorf("parse %s: %w", layoutPath, err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		// Page keys are the full path relative to pages/, minus
		// the .html.tmpl suffix. Nested directories become slashed
		// URL segments: pages/team/julia.html.tmpl serves at
		// /<lang>/team/julia. Bare `pages/index.html.tmpl` is the
		// landing (empty key); `pages/<dir>/index.html.tmpl` maps
		// to the directory itself (e.g. pages/team/index.html.tmpl
		// → /<lang>/team) so sub-trees can have a default page
		// without a parallel pages/<dir>.html.tmpl twin.
		key := strings.TrimSuffix(strings.TrimPrefix(path, "pages/"), ".html.tmpl")
		switch {
		case key == "index":
			key = ""
		case strings.HasSuffix(key, "/index"):
			key = strings.TrimSuffix(key, "/index")
		}
		// [param] segments → dynamic route. parseDynamicRoute
		// rejects malformed brackets at boot so a typo doesn't
		// silently become an unmatchable route.
		if isDynamicKey(key) {
			dr, err := parseDynamicRoute(key, t)
			if err != nil {
				return err
			}
			s.dynamicPages = append(s.dynamicPages, dr)
			return nil
		}
		s.pages[key] = t
		return nil
	})
}

// resolvePage looks up the template for a URL page key. Tries
// the exact-match map first (the fast path for the common case),
// falls back to scanning dynamic routes (pages/<dir>/[slug]) in
// load order. The returned params map is always non-nil so the
// template can range over it without a nil-check; exact matches
// just get an empty map. Returns (_, _, false) when neither
// path produces a hit, letting the caller decide whether to
// 404 or redirect.
func (s *Site) resolvePage(pageKey string) (*template.Template, map[string]string, bool) {
	if t, ok := s.pages[pageKey]; ok {
		return t, map[string]string{}, true
	}
	for _, dr := range s.dynamicPages {
		if params, ok := dr.match(pageKey); ok {
			return dr.tmpl, params, true
		}
	}
	return nil, nil, false
}

// renderPage executes the page's _layout into a buffer first so that
// any template error returns a clean 500 instead of a half-flushed
// body the browser tries to interpret. The detail is logged; the
// response body stays generic to avoid leaking template names or
// directory layout to whoever caused the error.
func (s *Site) renderPage(w http.ResponseWriter, r *http.Request, tmpl *template.Template, key, lang string, params map[string]string) {
	w.Header().Set(constants.HeaderContentType, "text/html; charset=utf-8")
	w.Header().Set(constants.HeaderVary, "Cookie, Accept-Language")
	w.Header().Set(constants.HeaderCacheControl, fmt.Sprintf("public, max-age=%d", pageMaxAgeSeconds))

	data := map[string]any{
		"Lang":          lang,
		"Langs":         s.supportedLangs,
		"DefaultLang":   s.defaultLang,
		"CanonicalHost": s.canonicalHost,
		"Brand":         s.brand,
		"Path":          r.URL.Path,
		"PageKey":       key,
		"Params":        params,
		"Year":          time.Now().Year(),
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "_layout", data); err != nil {
		log.ErrorContext(r.Context(), "render: template execute failed",
			"page", key, "lang", lang, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// cacheImmutable wraps a static-file handler so browsers cache for a
// year and treat content as immutable. Safe because every URL we emit
// for a static asset ends in ?v=<assetVer>; redeploys flip assetVer
// and all clients refetch.
func cacheImmutable(next http.Handler) http.Handler {
	header := fmt.Sprintf("public, max-age=%d, immutable", immutableMaxAgeSeconds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderCacheControl, header)
		next.ServeHTTP(w, r)
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
