package render

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hblabs-co/kit/web/render/config"
	"github.com/hblabs-co/kit/web/render/og"
)

// Builder is the fluent counterpart to New: read declarative bits
// from YAML, layer in code-only bits (FS, og.Storage, og.Themes,
// dev mode), and assemble a *Site at the end. Use it from main when
// most of the site config lives in config/config.yaml:
//
//	//go:embed pages partials locales static config seo
//	var staticFS embed.FS
//
//	site, err := render.Init(staticFS).
//	    LoadConfig("config/config.yaml").
//	    Build()
//	if err != nil { log.Fatal(err) }
//
// LoadConfig stashes any read/parse error; Build surfaces it. The
// builder returns *Builder from every chain step so the call site
// reads top-to-bottom even when an intermediate step failed.
//
// For the all-in-code path, keep using render.New(Options{...}) —
// the builder is sugar over New, not a replacement. Both paths
// converge on the same defaulting logic inside New so behaviour
// stays in one place.
type Builder struct {
	fs         fs.FS
	opts       Options
	cfg        *config.Config
	dataSchema any
	err        error
}

// Init starts a Builder pinned to fsys — typically the //go:embed
// fs.FS rooted at the service directory. fsys is required; nil is
// treated as a configuration bug and surfaces from Build.
func Init(fsys fs.FS) *Builder {
	b := &Builder{fs: fsys, opts: Options{FS: fsys}}
	if fsys == nil {
		b.err = errors.New("render: Init requires a non-nil fs.FS")
	}
	return b
}

// LoadConfig reads + parses the YAML at path inside the builder's
// fs.FS and merges its values into the in-flight Options. Strict
// mode: unknown keys fail at parse, not at runtime. The merge
// preserves any earlier With* call (code wins over disk so main can
// override, e.g. flipping Dev based on env).
//
// Errors are stashed on the builder and returned by Build, so the
// chain still reads cleanly:
//
//	site, err := render.Init(fsys).LoadConfig("config/config.yaml").Build()
func (b *Builder) LoadConfig(path string) *Builder {
	if b.err != nil {
		return b
	}
	cfg, err := config.Load(b.fs, path)
	if err != nil {
		b.err = err
		return b
	}
	b.cfg = cfg
	b.applyConfig(cfg)
	return b
}

// Config returns the parsed *config.Config from the most recent
// LoadConfig call, or nil if LoadConfig wasn't called or errored.
// Useful for callers (e.g. main, render/dev.MountSite) that need to
// read non-render fields like QuietPaths without re-parsing the YAML.
//
// The pointer is the loader's own — treat as read-only; mutations
// would diverge from the Options the builder already merged.
func (b *Builder) Config() *config.Config {
	return b.cfg
}

// WithDev flips the Dev flag — typically driven from env (e.g.
// `HBL_ENV=local`) rather than from config.yaml so the same YAML
// can ship to every environment. Overrides whatever LoadConfig set
// (config.yaml has no `dev` field anyway, but keep the override
// shape consistent).
func (b *Builder) WithDev(dev bool) *Builder {
	b.opts.Dev = dev
	return b
}

// WithOGStorage attaches the OG image storage backend. Required in
// production (MinIO/S3-backed); optional in dev where the in-process
// MemoryStorage default is fine. Has no effect when og.enabled is
// false in config.yaml.
func (b *Builder) WithOGStorage(storage og.Storage) *Builder {
	b.opts.OG.Storage = storage
	return b
}

// WithOGThemes registers extra themes alongside og's built-in
// "default". Themes carry font.Face values that don't serialise, so
// they only ever come from code. nil = just the default theme.
func (b *Builder) WithOGThemes(themes map[string]og.Theme) *Builder {
	b.opts.OG.Themes = themes
	return b
}

// WithFuncMap attaches service-specific template helpers to the
// builder. Pass through from a service's main when its page templates
// need helpers the framework doesn't ship (e.g. a project catalogue). Framework helpers always win on name
// collisions — see Options.FuncMap.
func (b *Builder) WithFuncMap(fm template.FuncMap) *Builder {
	b.opts.FuncMap = fm
	return b
}

// WithExtraStatic attaches additional static-asset subtrees to the
// builder. Each entry is mounted at /static/<prefix>/ with the same
// cache-immutable + content-fingerprinting policy as the service's
// own static. Use it to plug in shared brand/UI packages — see
// Options.ExtraStatic for the contract.
func (b *Builder) WithExtraStatic(extra map[string]fs.FS) *Builder {
	b.opts.ExtraStatic = extra
	return b
}

// WithIconCatalog plugs in the `name → inner-SVG` catalog the
// framework's `{{ icon }}` / `{{ iconStroke }}` helpers fall back
// to when a name isn't overridden under config.yaml's data.icons.
// Pass web/uikit.IconSet here for the shared HB Labs
// catalog; clients shipping their own catalog plug that map in
// instead. nil keeps both helpers unregistered — services use
// `{{ data "icons.<name>" }}` to read the raw markup directly in
// that case.
func (b *Builder) WithIconCatalog(catalog map[string]string) *Builder {
	b.opts.IconCatalog = catalog
	return b
}

// WithReady attaches the readiness probe backing the auto-mounted
// GET /readyz endpoint. Code-only, like WithOGStorage: a readiness
// check is a live dependency probe, not YAML config. nil (or never
// calling this) keeps /readyz on healthz semantics — see
// Options.Ready for the contract.
func (b *Builder) WithReady(ready func(ctx context.Context) error) *Builder {
	b.opts.Ready = ready
	return b
}

// WithDataSchema opts the loaded config.yaml `data:` section into
// strict-shape validation. Pass a pointer to a Go struct whose
// `yaml:"…"` tags describe the keys the service expects; at Build,
// the parsed Data map is re-marshalled and decoded into the schema
// with KnownFields(true). Missing required keys, unknown keys, and
// type mismatches surface as a boot error with the offending path,
// so a typo in config.yaml fails before the binary serves a single
// request.
//
// Use it for any data tree that backs templates — typos in a
// `{{ data "myconfig.subconfig.name" }}` lookup would otherwise
// surface as a 500 on a single page rather than at boot. Leave
// unset when the data tree is truly free-form (prototype, ad-hoc
// content); the `data` helper still works, it just fails per-request
// instead of per-deploy when a key is missing.
//
// The schema is only validated, never installed in place of Data —
// the helper continues to walk the map[string]any tree so template
// authors don't have to track which fields became typed.
func (b *Builder) WithDataSchema(schema any) *Builder {
	b.dataSchema = schema
	return b
}

// Build assembles the merged Options and calls New. Returns the
// first stashed error (Init / LoadConfig) before attempting to
// build, so a failure surfaces with the original cause, not a
// confusing follow-on like "missing pages directory".
func (b *Builder) Build() (*Site, error) {
	if b.err != nil {
		return nil, b.err
	}
	if err := b.validateDataSchema(); err != nil {
		return nil, err
	}
	return New(b.opts)
}

// BuildOptions returns the resolved Options without calling New.
// Useful when a wrapper (e.g. render/dev) builds its own *Site per
// request and just needs the config plumbing — same YAML + With*
// merge as Build, no construction. Same error-stash semantics:
// LoadConfig / read failures surface here.
func (b *Builder) BuildOptions() (Options, error) {
	if b.err != nil {
		return Options{}, b.err
	}
	if err := b.validateDataSchema(); err != nil {
		return Options{}, err
	}
	return b.opts, nil
}

// marshalDataFn is a variable seam over yaml.Marshal so tests can
// force the failure path that map[string]any from the YAML decoder
// never produces in practice (it only carries serialisable scalars,
// maps and slices). Swapped via the test-only var on the file scope;
// see init_test.go's TestBuilder_WithDataSchemaMarshalErrorWraps.
var marshalDataFn = yaml.Marshal

// validateDataSchema re-decodes the loaded data tree into the
// service-supplied schema with strict mode. No-op when WithDataSchema
// wasn't called, or when Data is empty (an absent data: section
// against a schema with no required fields would otherwise force
// services to declare an empty data: block in every config).
func (b *Builder) validateDataSchema() error {
	if b.dataSchema == nil {
		return nil
	}
	if len(b.opts.Data) == 0 {
		return nil
	}
	raw, err := marshalDataFn(b.opts.Data)
	if err != nil {
		return fmt.Errorf("render: marshal data for schema check: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(b.dataSchema); err != nil {
		return fmt.Errorf("render: data schema mismatch: %w", err)
	}
	return nil
}

// applyConfig merges every field a config.Config knows about into
// the builder's Options. Fields the YAML can't carry (FS, Dev,
// OG.Storage, OG.Themes) stay untouched — those come from With*
// calls or from Init.
func (b *Builder) applyConfig(cfg *config.Config) {
	b.opts.CanonicalHost = cfg.CanonicalHost
	b.opts.DefaultLang = cfg.DefaultLang
	b.opts.SupportedLangs = cfg.SupportedLangs
	b.opts.Brand = cfg.Brand

	b.opts.SEO = SEOOptions{
		DisableRobots:      cfg.SEO.DisableRobots,
		DisableSitemap:     cfg.SEO.DisableSitemap,
		DisableLLMsTxt:     cfg.SEO.DisableLLMsTxt,
		DisableSecurityTxt: cfg.SEO.DisableSecurityTxt,
		DisableAASA:        cfg.SEO.DisableAASA,
		DisableAssetLinks:  cfg.SEO.DisableAssetLinks,
		SecurityContact:    cfg.SEO.SecurityContact,
		PreferredLanguages: cfg.SEO.PreferredLanguages,
		AppleTeamID:        cfg.SEO.AppleTeamID,
		IOSBundleID:        cfg.SEO.IOSBundleID,
		AASAComponents:     cfg.SEO.AASAComponents,
		AndroidAssetLinks:  toRenderAssetLinks(cfg.SEO.AndroidAssetLinks),
	}

	if cfg.SEO.LLMsTxtPath != "" {
		body, err := fs.ReadFile(b.fs, cfg.SEO.LLMsTxtPath)
		if err != nil {
			b.err = fmt.Errorf("render: read llms_txt_path %q: %w", cfg.SEO.LLMsTxtPath, err)
			return
		}
		b.opts.SEO.LLMsTxt = string(body)
	}

	// OG: preserve Storage/Themes set via With* calls; only the
	// YAML-carried fields (Enabled / Brand / URLPrefix) merge in.
	// Brand falls back to the top-level cfg.Brand so services have
	// one source of truth for the visible product name — empty
	// og.brand inherits, explicit og.brand overrides.
	b.opts.OG.Enabled = cfg.OG.Enabled
	b.opts.OG.Brand = cfg.OG.Brand
	if b.opts.OG.Brand == "" {
		b.opts.OG.Brand = cfg.Brand
	}
	b.opts.OG.URLPrefix = cfg.OG.URLPrefix

	// Data flows from YAML straight into Options — the `data` helper
	// walks the map directly, and WithDataSchema (if configured) is
	// what enforces the shape at Build.
	b.opts.Data = cfg.Data
}

// toRenderAssetLinks converts the YAML-tagged config types to the
// JSON-tagged render types. The two structs hold the same data —
// the duplication is purely so each side keeps its own native tag
// shape (yaml in config, json in render).
func toRenderAssetLinks(in []config.AssetLink) []AssetLink {
	if len(in) == 0 {
		return nil
	}
	out := make([]AssetLink, len(in))
	for i, link := range in {
		out[i] = AssetLink{
			Relation: link.Relation,
			Target: AssetLinkTarget{
				Namespace:              link.Target.Namespace,
				PackageName:            link.Target.PackageName,
				Sha256CertFingerprints: link.Target.Sha256CertFingerprints,
			},
		}
	}
	return out
}
