// Package config is the on-disk configuration surface of the render
// framework. Mirrors render.Options + render.SEOOptions + render.OGOptions
// minus the fields that can't be serialised (embed.FS, og.Storage,
// og.Themes, dev mode) — those keep coming from main via the
// render.Init builder.
//
// # Env-var substitution
//
// Before parsing the YAML, Load expands shell-style variable
// references inside the file body so per-environment secrets stay
// out of source control:
//
//   - "${HB_FOO}"           → value of HB_FOO, "" if unset
//   - "${HB_FOO:-fallback}" → value of HB_FOO if set & non-empty,
//     "fallback" otherwise
//
// Priority: when both an env var and a YAML literal could supply a
// value, the env var wins — that's the whole point of the syntax.
// To force-override per environment without changing config.yaml,
// services declare `apple_team_id: "${HB_APPLE_TEAM_ID:-}"`; an
// unset env var resolves to "" and the field stays at its zero
// value. Quote the scalar when the substituted value might contain
// YAML-special characters (":" most commonly).
//
// # Strict mode
//
// Load runs the YAML decoder with KnownFields(true) — any unknown
// key in config.yaml is a hard error, not a silent ignore. Defaults
// live in render.New, NOT here — missing fields stay at the Go
// zero value and the same defaulting path runs whether the caller
// used render.New(Options{...}) or render.Init(...).LoadConfig(...).Build().
//
// # File shape
//
//	# config/config.yaml
//	canonical_host: "${HB_CANONICAL_HOST:-https://hblabs.co}"
//	default_lang: es
//	supported_langs: [es, en, de, nl]
//	seo:
//	  security_contact: "${HB_SECURITY_CONTACT:-}"
//	  apple_team_id: "${HB_APPLE_TEAM_ID:-}"
//	  ios_bundle_id: "${HB_IOS_BUNDLE_ID:-co.hblabs.app}"
//	  llms_txt_path: seo/llms.txt
//	og:
//	  enabled: true
//	  brand: Example
//	  url_prefix: /og
package config

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hblabs-co/kit/core/envcfg"
)

// Config is the root document. Mirrors render.Options field-for-field
// for the YAML-serialisable subset, plus a small set of HTTP-layer
// knobs (QuietPaths) that services tune alongside the framework
// config so they live in the same file.
type Config struct {
	// CanonicalHost is the absolute origin used in canonical links,
	// sitemap, og:url, etc. e.g. "https://hblabs.co". No trailing /.
	CanonicalHost string `yaml:"canonical_host"`

	// DefaultLang is the language slug picked when the visitor has
	// no cookie or recognisable Accept-Language. Must appear in
	// SupportedLangs.
	DefaultLang string `yaml:"default_lang"`

	// SupportedLangs lists the BCP-47-ish two-letter codes the site
	// serves. Each must have a matching locales/<code>.json file.
	SupportedLangs []string `yaml:"supported_langs"`

	// Brand is the site's identity name surfaced in the layout
	// template (`{{ .Brand }}`) and used as the default for the OG
	// card's brand line when og.brand is left empty. One source of
	// truth: change this to update the nav, footer, and social
	// previews together.
	Brand string `yaml:"brand"`

	// QuietPaths feeds httpx.QuietPaths(...) so the access-log
	// middleware demotes these patterns to debug. Trailing "*" =
	// prefix match (e.g. "/static/*"); anything else = exact.
	//
	// Merge semantics: config.Load always seeds the resolved slice
	// with DefaultQuietPaths and appends whatever the YAML adds,
	// dropping duplicates on second occurrence. Services therefore
	// only list the *extras* their service needs; the framework
	// defaults (healthz, /static/*, dev hot-reload) are always on.
	//
	// Lives here (not in render.Options) because access logging is
	// an HTTP-layer concern, not a render concern; centralising it
	// in config.yaml just spares services a separate config file.
	QuietPaths []string `yaml:"quiet_paths"`

	// SEO controls the auto-mounted /robots.txt, /sitemap.xml,
	// /llms.txt, /security.txt, and /.well-known/* endpoints.
	SEO SEO `yaml:"seo"`

	// OG configures the auto-mounted Open Graph image pipeline.
	OG OG `yaml:"og"`

	// Data is a free-form key/value tree exposed to templates via
	// the `{{ data "dot.path" }}` helper. Use it for site-local
	// content that doesn't fit any other section: extra brand
	// strings, a few extension icons, structured copy a partial
	// iterates over.
	//
	// Strict mode without a fixed schema: the loader keeps Data as
	// map[string]any so the field stays free, but services that
	// want compile-time guarantees pass a Go struct to
	// render.Builder.WithDataSchema — Build then re-decodes Data
	// into that struct with KnownFields(true) and fails boot when
	// a key is missing, mistyped or unknown. The same data ships
	// through render.Site.Data() for non-template consumers.
	//
	// Convention for icon extension: top-level `icons:` maps stable
	// kebab-case names to the inner SVG markup (same contract as
	// web/uikit.IconSet). The `{{ icon name size }}`
	// helper falls back to this entry when a name isn't in
	// uikit.IconSet, letting services add or override icons without
	// editing the shared catalog.
	Data map[string]any `yaml:"data"`
}

// DefaultQuietPaths is the canonical noise-suppression list every
// service starts with. config.Load seeds the resolved Config.QuietPaths
// with this slice and then appends anything the YAML adds (dedup on
// duplicates). Centralised here so the framework's defaults are one
// search away — touching this slice shifts every service.
var DefaultQuietPaths = []string{
	"/healthz",
	"/readyz",
	"/static/*",
	"/favicon.*",
	"/__reload",
	"/__reload.js",
}

// SEO mirrors render.SEOOptions (minus AndroidAssetLinks rendered
// via the AssetLink type below, plus llms_txt_path for the llms.txt
// body which is read from a file in the same FS, not inlined).
type SEO struct {
	// Per-endpoint disable flags. Default (zero) keeps every
	// endpoint mounted.
	DisableRobots      bool `yaml:"disable_robots"`
	DisableSitemap     bool `yaml:"disable_sitemap"`
	DisableLLMsTxt     bool `yaml:"disable_llms_txt"`
	DisableSecurityTxt bool `yaml:"disable_security_txt"`
	DisableAASA        bool `yaml:"disable_aasa"`
	DisableAssetLinks  bool `yaml:"disable_assetlinks"`

	// SecurityContact ends up in /.well-known/security.txt. Empty →
	// the endpoint serves a placeholder and render emits a boot
	// warning.
	SecurityContact string `yaml:"security_contact"`

	// PreferredLanguages overrides the default Preferred-Languages
	// line in security.txt. Empty → render reuses SupportedLangs.
	PreferredLanguages []string `yaml:"preferred_languages"`

	// AppleTeamID + IOSBundleID drive
	// /.well-known/apple-app-site-association. Empty teamID logs a
	// boot warning but the endpoint still serves a placeholder JSON
	// so AASA validators have something to point at.
	AppleTeamID string `yaml:"apple_team_id"`
	IOSBundleID string `yaml:"ios_bundle_id"`

	// AASAComponents lists the URL paths that should open the iOS
	// app via Universal Links. Empty → render derives "/<lang>/*"
	// for every SupportedLang.
	AASAComponents []string `yaml:"aasa_components"`

	// AndroidAssetLinks is the array served at
	// /.well-known/assetlinks.json. Empty → "[]".
	AndroidAssetLinks []AssetLink `yaml:"android_asset_links"`

	// LLMsTxtPath references a file inside the same fs.FS render
	// runs against (typically the //go:embed root). render reads it
	// at boot and uses the bytes as the /llms.txt body. Empty →
	// render serves a built-in stub. Inlining the markdown in YAML
	// would bloat the config and force operators to escape every
	// special char, so the loader keeps content in its own file.
	LLMsTxtPath string `yaml:"llms_txt_path"`
}

// OG mirrors render.OGOptions for the YAML-serialisable subset.
// Storage and Themes are concrete Go values that don't serialise —
// pass them through the render.Init builder instead.
type OG struct {
	// Enabled flips the pipeline on. Required because the zero
	// value otherwise looks indistinguishable from "use defaults".
	Enabled bool `yaml:"enabled"`

	// Brand is the line shown above the title in every card.
	// Typically the product / site name. Empty disables the brand
	// line.
	Brand string `yaml:"brand"`

	// URLPrefix is the route the handler mounts under. "" → "/og".
	URLPrefix string `yaml:"url_prefix"`
}

// AssetLink mirrors render.AssetLink. Duplicated in this package
// (rather than aliased) so the YAML tags can be snake_case without
// imposing them on render's Go-level type — render's struct uses
// json tags because that's what gets served.
type AssetLink struct {
	Relation []string        `yaml:"relation"`
	Target   AssetLinkTarget `yaml:"target"`
}

// AssetLinkTarget mirrors render.AssetLinkTarget.
type AssetLinkTarget struct {
	Namespace              string   `yaml:"namespace"`
	PackageName            string   `yaml:"package_name"`
	Sha256CertFingerprints []string `yaml:"sha256_cert_fingerprints"`
}

// Load reads the YAML at path inside fsys and decodes it in strict
// mode — any unknown key is a hard error, not a silent ignore. Use
// the standard config.yaml location:
//
//	cfg, err := config.Load(staticFS, "config/config.yaml")
//
// fsys is the same fs.FS render itself runs against (typically the
// //go:embed root). Reusing it means the file ships in the binary
// and tests can swap in a fstest.MapFS to drive parse-error paths
// deterministically.
//
// Returns the parsed Config or a wrapped error. The caller is
// expected to feed the Config into render.Init(...).LoadConfig(...)
// rather than reading fields by hand — that path layers in code-
// only values (FS, og.Storage, dev mode) the YAML can't carry.
func Load(fsys fs.FS, path string) (*Config, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("render/config: open %s: %w", path, err)
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("render/config: read %s: %w", path, err)
	}
	expanded := envcfg.Expand(string(raw))

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	// Strict mode: reject typos in keys at boot rather than letting
	// them silently fall through to zero values that confuse
	// operators ("I set seo.security_contac and it's still
	// placeholder?").
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("render/config: decode %s: %w", path, err)
	}

	// Merge QuietPaths with the framework defaults so services only
	// list their own extras. Render-side defaults still live in
	// render.New (single source of truth for framework behaviour),
	// but QuietPaths feeds httpx — render never sees it — so the
	// loader is the only place that can apply this baseline without
	// forcing every caller to repeat it.
	cfg.QuietPaths = mergeQuietPaths(DefaultQuietPaths, cfg.QuietPaths)

	return &cfg, nil
}

// mergeQuietPaths returns defaults ∪ extras, preserving the order
// of defaults first and then any extras the YAML added in YAML
// order. Duplicates dropped on second occurrence so a service that
// happens to repeat "/healthz" doesn't pollute the resolved list.
func mergeQuietPaths(defaults, extras []string) []string {
	seen := make(map[string]struct{}, len(defaults)+len(extras))
	out := make([]string, 0, len(defaults)+len(extras))
	for _, p := range defaults {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range extras {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
