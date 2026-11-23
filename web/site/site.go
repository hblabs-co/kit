// Package site is the composition root for an HB Labs web
// service. One call to New does everything a typical main needs to
// stand up the framework:
//
//  1. Reads config/config.yaml (env vars expanded, defaults merged).
//  2. Builds the *render.Site (prod) or wraps it in a hot-reload
//     *render/dev.Server (when EnvHBEnv is "local").
//  3. Optionally builds the rules engine (web/rules/engine)
//     from YAML pipelines embedded next to the service. In dev
//     it wraps it with rules/dev so YAML edits hot-reload, in
//     prod it builds once at boot.
//  4. Mounts everything on a top-level mux: pipeline endpoints at
//     the URLs declared in RulesOptions.Mounts, the render site at
//     "/".
//  5. Wraps the mux in the canonical middleware bundle: Secure →
//     RequestID → AccessLog (with QuietPaths) → Recover.
//  6. Returns the composed http.Handler ready to drop into
//     system.RunForever, alongside the system.Module slice for the
//     two dev hot-reloaders (nil-filtered: in prod the slice is
//     all nils that RunForever silently drops).
//
// Wiring from main:
//
//	//go:embed pages all:partials locales static config pipelines
//	var siteFS embed.FS
//
//	handler, modules, _, err := site.New(site.Options{
//	    SiteFS: siteFS, // WatchRoot ".", ConfigPath "config/config.yaml", uikit mounted by default
//	    Rules: site.RulesOptions{
//	        PipelinesDir: "pipelines",
//	        Mounts: map[string]string{
//	            "/__rules/visitor": "visitor_flags",
//	        },
//	    },
//	})
//	if err != nil { log.Error("site", "err", err); os.Exit(1) }
//
//	modules = append(modules, system.NewServer(serverOpts))
//	system.RunForever(ctx, modules...)
//
// Rules wiring is optional: leave Options.Rules zero and site
// behaves exactly like the render-only setup (no /__rules/*
// endpoints, no rules dev server).
package site

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hblabs-co/kit/core/constants"

	"github.com/hblabs-co/kit/core/httpx"
	"github.com/hblabs-co/kit/core/secheaders"
	"github.com/hblabs-co/kit/core/system"
	"github.com/hblabs-co/kit/web/render"
	"github.com/hblabs-co/kit/web/render/config"
	renderdev "github.com/hblabs-co/kit/web/render/dev"
	rulesdev "github.com/hblabs-co/kit/web/rules/dev"
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
	"github.com/hblabs-co/kit/web/rules/rules"
	"github.com/hblabs-co/kit/web/siteurl"
)

// Options configures New.
type Options struct {
	// SiteFS is the production source FS (typically the //go:embed
	// rooted at the service directory). Used as-is when EnvHBEnv
	// resolves to anything other than "local"; ignored otherwise.
	SiteFS fs.FS

	// WatchRoot is the on-disk source directory used in dev mode —
	// New wraps it with os.DirFS so file edits flow through the
	// dev wrapper without recompiling. Defaults to DefaultWatchRoot
	// (".", i.e. `go run .` from the service directory); ignored
	// outside EnvHBEnv "local".
	WatchRoot string

	// ConfigPath is the location of the framework config inside
	// whichever fs.FS render uses. Same path in both modes —
	// LoadConfig reads it from SiteFS in prod and from os.DirFS in
	// dev. Defaults to DefaultConfigPath.
	ConfigPath string

	// Rules wires the rules engine into the service. Zero value =
	// no rules engine, no /__rules/* endpoints. Set PipelinesDir +
	// Mounts to mount one or more pipelines.
	Rules RulesOptions

	// FuncMap is an optional set of template helpers the service
	// wants exposed inside its .html.tmpl files alongside the
	// framework built-ins. Threaded straight through to
	// render.Options.FuncMap, so collisions with framework helpers
	// (T, asset, pageURL, canonical, ogImage, devScript) are
	// silently dropped in favour of the built-in.
	//
	// Typical use: a service-local catalogue (e.g. {{ featuredProjects }}
	// and {{ projectsByCategory }} for a portfolio).
	// Leave nil for services that don't need extras.
	FuncMap template.FuncMap

	// ExtraStatic mounts shared asset subtrees alongside the
	// service's own static/. Each entry's key is a URL-path segment
	// (e.g. "uikit") that maps to /static/<key>/, with the same
	// cache-immutable + content-fingerprinting policy.
	//
	// Use it to consume cross-service asset packages — typically
	// github.com/hblabs-co/kit/web/uikit for the shared design-system
	// CSS — without copying files into each service's embed:
	//
	//	import "github.com/hblabs-co/kit/web/uikit"
	//
	//	site.Options{
	//	    ExtraStatic: map[string]fs.FS{"uikit": uikit.FS},
	//	}
	//
	// In templates, the standard asset helper resolves shared paths
	// the same way as local ones:
	//
	//	{{ asset "uikit/css/tokens.css" }}
	//
	// The uikit is mounted under "uikit" by default (UIKitFS); an
	// explicit "uikit" entry here wins, and WithoutUIKit opts out.
	ExtraStatic map[string]fs.FS

	// WithoutUIKit skips the default "uikit" ExtraStatic mount.
	WithoutUIKit bool

	// AdditionalImgSrc extends the framework's default CSP `img-src`
	// directive with extra origins. Use case: in dev the asset host
	// is `http://localhost:9000` (MinIO), which the default policy
	// (`img-src 'self' data: https:`) blocks because plain `http:` is
	// not in the source list. Pass the resolved origin here so
	// browser-loaded assets render without disabling CSP wholesale.
	//
	// Each entry must be a valid CSP source expression — typically
	// "scheme://host[:port]" (e.g. "http://localhost:9000") or a
	// scheme-only token like "https:" if a broader allow-list is
	// intended. In prod, leave empty: the default already permits
	// any HTTPS image origin via `https:`.
	AdditionalImgSrc []string

	// AdditionalFrameSrc extends the framework's CSP with a
	// `frame-src` directive so the service can embed third-party
	// iframes (Spotify episode embeds, YouTube, Vimeo, payment
	// widgets). Default policy has no frame-src, so default-src
	// 'self' applies — blocking every embedded origin. Pass an
	// explicit list here to opt in per service. Same grammar as
	// AdditionalImgSrc.
	AdditionalFrameSrc []string

	// DataSchema is an optional pointer to a service-defined Go
	// struct whose yaml tags lock the shape of config.yaml's
	// `data:` section. Threaded straight through to
	// render.Builder.WithDataSchema, which (a) validates the
	// loaded data against the struct's tags with KnownFields(true)
	// so a typo in YAML fails at boot, and (b) leaves the struct
	// populated as a side effect of the validation decode — so
	// services can read typed values from Go-side code (URL
	// builders, init guards) without re-parsing the YAML or
	// walking the framework's map[string]any.
	//
	// In dev mode the closure that re-loads config on every
	// request re-decodes into the same pointer, so a YAML edit
	// shows up immediately in both templates and Go-side helpers
	// without restarting the binary.
	//
	// Leave nil when the data: section is free-form (prototype,
	// content authored by non-engineers) or absent entirely.
	DataSchema any

	// IconCatalog is the shared name→inner-SVG map the framework's
	// {{ icon }} / {{ iconStroke }} / {{ iconFilled }} helpers
	// fall back to when a name is missing from the per-service
	// data.icons override. Typically `uikit.IconSet` — passing
	// it once here lets templates use {{ icon "leaf" 18 }} for
	// any catalogue glyph without the service redefining it.
	//
	// The render helpers are registered only when this is
	// non-nil. Leave empty when the service paints icons by
	// other means (raw <img>, pure CSS, etc.).
	IconCatalog map[string]string

	// Ready is the optional readiness probe behind the framework's
	// auto-mounted GET /readyz. Threaded straight through to
	// render.Builder.WithReady: return nil when the service can
	// take traffic, an error to answer 503. Leave nil for services
	// without a hard runtime dependency — /readyz then mirrors
	// /healthz. See render.Options.Ready for the contract.
	Ready func(ctx context.Context) error

	// StyledNotFound, when true, wraps the site handler so a
	// 404 from the render framework gets re-served as the
	// service's own `/<lang>/not-found` page (with status 404
	// preserved). Sub-request strategy — see notfound.go for
	// the gritty details. Default false to keep behaviour
	// unchanged for services that don't ship a not-found
	// template; flip to true when you have one at
	// pages/not-found.html.tmpl and want visitors to see it
	// instead of the framework's plaintext "404 page not
	// found".
	//
	// Lang resolution uses the loaded config.yaml's
	// supported_langs / default_lang so the wrapper stays in
	// sync with whatever the service declares — no need to
	// pass those again here.
	StyledNotFound bool
}

// RulesOptions configures the rules-engine half of site.New.
type RulesOptions struct {
	// PipelinesDir is the subdirectory inside SiteFS containing
	// the *.yaml pipeline declarations. The same path is used as
	// a relative-to-WatchRoot disk path in dev. Required when
	// Mounts is non-empty.
	PipelinesDir string

	// Mounts maps URL patterns to pipeline names. Each entry
	// becomes mux.Handle(pattern, engine.HTTPHandler(name)).
	// Empty = rules wiring is skipped entirely.
	//
	// Patterns follow http.ServeMux semantics: exact path or
	// prefix with trailing slash. The canonical middleware bundle
	// (Secure / RequestID / AccessLog / Recover) wraps the mux
	// once, so every pipeline endpoint gets correlation IDs and
	// access logs without extra wiring.
	Mounts map[string]string

	// ExtraRules registers additional Rule instances on top of
	// rules.All(). Useful for service-local rules that aren't
	// reusable enough to live in web/rules/rules. Optional.
	ExtraRules []rulesengine.Rule
}

// rulesCatalogPath is the location of the shared rules catalog
// inside a kit checkout. site.New wires it (resolved against
// constants.EnvKitDir, or the cwd when unset) as a code-watch root in
// dev so an edit to any catalog rule logs a clear "restart to pick it
// up" line — the honest signal that the running binary can't hot-swap
// the change. Stat-skipped when the directory isn't there.
const rulesCatalogPath = "web/rules/rules"

// rulesCatalogDir is rulesCatalogPath resolved against HB_KIT_DIR.
func rulesCatalogDir() string {
	return filepath.Join(os.Getenv(constants.EnvKitDir), filepath.FromSlash(rulesCatalogPath))
}

// hotReloadBannerLine is the line surfaced in the boot banner when
// site.New ends up wiring the dev hot-reloader. Lives here (rather
// than in render/dev) because it's a property of the runtime
// configuration site composes, not of the hot-reloader itself —
// the same decision that flips dev mode controls this string.
const hotReloadBannerLine = "🔥  Hot-reload: on (DEV=true)"

// BannerExtras returns the banner lines that describe site-level
// runtime state main should surface in system.Boot's startup
// banner. Empty in prod; in dev it surfaces the hot-reload
// affordance so the operator sees it before the first request
// log line.
func BannerExtras() []string {
	if !siteurl.IsDev() {
		return nil
	}
	return []string{hotReloadBannerLine}
}

// New is the one-shot composition root. See the package doc for
// the full call shape and lifecycle.
//
// Returns:
//   - http.Handler: the composed handler, middleware already
//     applied. Ready for system.NewServer.
//   - []system.Module: dev hot-reloaders (render + rules). In prod
//     all entries are nil; RunForever filters them automatically.
//   - *config.Config: the parsed YAML so callers can read fields
//     beyond what the middleware bundle consumes (CanonicalHost
//     for external integrations, SEO config, etc.).
func New(opts Options) (http.Handler, []system.Module, *config.Config, error) {
	opts = withDefaults(opts)
	siteHandler, renderDev, cfg, err := mountSite(opts)
	if err != nil {
		return nil, nil, nil, err
	}

	rulesHandlers, rulesDev, err := mountRules(opts)
	if err != nil {
		return nil, nil, nil, err
	}

	if opts.StyledNotFound {
		// Wrap BEFORE mounting under the mux so the not-found
		// shim only sees site requests, never rules-engine
		// traffic. Rules pipelines own their own error shapes
		// and shouldn't get re-rendered as a 404 HTML page.
		siteHandler = wrapStyledNotFound(siteHandler, cfg.SupportedLangs, cfg.DefaultLang)
	}

	mux := http.NewServeMux()
	for pattern, h := range rulesHandlers {
		mux.Handle(pattern, h)
	}
	mux.Handle("/", siteHandler)

	return wrapCanonical(mux, cfg, opts.AdditionalImgSrc, opts.AdditionalFrameSrc), []system.Module{renderDev, rulesDev}, cfg, nil
}

// Defaults applied by New to zero Options fields.
const (
	DefaultWatchRoot  = "."
	DefaultConfigPath = "config/config.yaml"
	// UIKitMount is the ExtraStatic key (URL segment) of the default
	// uikit mount: /static/uikit/...
	UIKitMount = "uikit"
)

// withDefaults fills WatchRoot, ConfigPath and the uikit mount. The
// caller's ExtraStatic map is copied, never mutated.
func withDefaults(opts Options) Options {
	if opts.WatchRoot == "" {
		opts.WatchRoot = DefaultWatchRoot
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = DefaultConfigPath
	}
	if !opts.WithoutUIKit {
		if _, ok := opts.ExtraStatic[UIKitMount]; !ok {
			extra := make(map[string]fs.FS, len(opts.ExtraStatic)+1)
			for k, v := range opts.ExtraStatic {
				extra[k] = v
			}
			extra[UIKitMount] = UIKitFS()
			opts.ExtraStatic = extra
		}
	}
	return opts
}

// mountSite resolves the framework config and builds the inner
// site http.Handler — either a *render.Site mux (prod) or the dev
// wrapper's mux. Pulled out of New so the orchestration in New
// stays readable.
func mountSite(opts Options) (http.Handler, *renderdev.Server, *config.Config, error) {
	devMode := siteurl.IsDev()

	sourceFS := opts.SiteFS
	if devMode {
		sourceFS = os.DirFS(opts.WatchRoot)
	}

	if !devMode {
		// Prod: build the *Site once at boot, mount on a fresh
		// ServeMux. No dev.Server needed; nil tells the caller not
		// to register a hot-reload module.
		b := render.Init(sourceFS).
			LoadConfig(opts.ConfigPath).
			WithFuncMap(opts.FuncMap).
			WithExtraStatic(opts.ExtraStatic).
			WithDataSchema(opts.DataSchema).
			WithIconCatalog(opts.IconCatalog).
			WithReady(opts.Ready)
		s, err := b.Build()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("site: build site: %w", err)
		}
		mux := http.NewServeMux()
		s.Mount(mux)
		return mux, nil, b.Config(), nil
	}

	// Dev: hand renderdev.Server a closure that re-parses the YAML
	// on every request so config.yaml edits hot-reload alongside
	// templates / locales / static.
	loadOpts := func() (render.Options, error) {
		return render.Init(sourceFS).
			LoadConfig(opts.ConfigPath).
			WithDev(true).
			WithFuncMap(opts.FuncMap).
			WithExtraStatic(opts.ExtraStatic).
			WithDataSchema(opts.DataSchema).
			WithIconCatalog(opts.IconCatalog).
			WithReady(opts.Ready).
			BuildOptions()
	}
	if _, err := loadOpts(); err != nil {
		return nil, nil, nil, fmt.Errorf("site: load config: %w", err)
	}
	cfg, err := config.Load(sourceFS, opts.ConfigPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("site: load config: %w", err)
	}

	srv, err := renderdev.New(renderdev.Options{
		RenderOpts: loadOpts,
		WatchRoot:  opts.WatchRoot,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("site: init dev server: %w", err)
	}
	return srv.Handler(), srv, cfg, nil
}

// mountRules builds the rules engine (or its hot-reload wrapper)
// and returns a pattern → handler map plus the optional dev module.
// Zero Rules.Mounts means rules are skipped entirely; the returned
// map is nil and the dev module is nil too. Callers passing the
// nil module straight to RunForever is fine: nonNilModules drops
// it.
func mountRules(opts Options) (map[string]http.Handler, *rulesdev.Server, error) {
	if len(opts.Rules.Mounts) == 0 {
		return nil, nil, nil
	}
	if opts.Rules.PipelinesDir == "" {
		return nil, nil, errors.New("site: Rules.PipelinesDir is required when Rules.Mounts is non-empty")
	}

	build := rulesEngineBuilder(opts)

	if !siteurl.IsDev() {
		eng, err := build()
		if err != nil {
			return nil, nil, fmt.Errorf("site: build rules engine: %w", err)
		}
		handlers := make(map[string]http.Handler, len(opts.Rules.Mounts))
		for pattern, pipelineName := range opts.Rules.Mounts {
			handlers[pattern] = eng.HTTPHandler(pipelineName)
		}
		return handlers, nil, nil
	}

	rd, err := rulesdev.New(rulesdev.Options{
		EngineFn:  build,
		WatchRoot: filepath.Join(opts.WatchRoot, opts.Rules.PipelinesDir),
		// CodeWatchRoots logs a warning when a rule's Go source
		// changes — Go binaries can't hot-swap code, so the dev
		// gets a clear "restart needed" line instead of a silent
		// no-op rebuild. Stat-skipped inside rulesdev.New if the
		// path isn't there (services running from a sub-cwd, for
		// instance), so this never breaks boot.
		CodeWatchRoots: []string{rulesCatalogDir()},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("site: init rules dev server: %w", err)
	}
	handlers := make(map[string]http.Handler, len(opts.Rules.Mounts))
	for pattern, pipelineName := range opts.Rules.Mounts {
		handlers[pattern] = rd.HTTPHandler(pipelineName)
	}
	return handlers, rd, nil
}

// rulesEngineBuilder closes over opts and returns the factory the
// rules engine (and its dev wrapper) call to assemble a fresh,
// already-Built engine. Source FS swaps between embed (prod) and
// os.DirFS (dev) — same dev/prod toggle as the render site uses.
func rulesEngineBuilder(opts Options) func() (*rulesengine.Engine, error) {
	return func() (*rulesengine.Engine, error) {
		var fsys fs.FS
		if siteurl.IsDev() {
			fsys = os.DirFS(filepath.Join(opts.WatchRoot, opts.Rules.PipelinesDir))
		} else {
			sub, err := fs.Sub(opts.SiteFS, opts.Rules.PipelinesDir)
			if err != nil {
				return nil, err
			}
			fsys = sub
		}
		eng := rulesengine.NewEngine().
			Register(rules.All()...).
			Register(opts.Rules.ExtraRules...).
			Load(fsys, "*.yaml")
		return eng, eng.Build()
	}
}

// wrapCanonical layers the platform's standard middleware bundle
// around the inner handler — order matches every service:
// Secure outermost so policy headers land on every response;
// RequestID before WithoutLogging so request-id/device-id flow
// into the log line; WithoutLogging outside WithRecover so a panic
// still produces an access entry (WithRecover converts the panic
// to a 500 first).
func wrapCanonical(h http.Handler, cfg *config.Config, extraImgSrc, extraFrameSrc []string) http.Handler {
	return httpx.NewHandler().
		Secure(secheaders.Options{AdditionalImgSrc: extraImgSrc, AdditionalFrameSrc: extraFrameSrc}).
		RequestID().
		WithoutLogging(cfg.QuietPaths...).
		WithRecover().
		Merge(h)
}
