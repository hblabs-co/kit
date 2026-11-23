package render

import (
	"errors"
	"html/template"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hblabs-co/kit/web/render/og"
)

// configYAML is a fixture for the builder tests — every field that
// the YAML loader carries is set so the merge into Options is fully
// exercised. Tests that only need a subset can use trimmed YAML
// directly inline.
const configYAML = `
canonical_host: https://hblabs.co
default_lang: es
supported_langs: [es]

seo:
  security_contact: security@hblabs.co
  apple_team_id: ABC1234567
  ios_bundle_id: co.hblabs.app
  preferred_languages: [es, en]
  aasa_components: ["/es/*"]
  android_asset_links:
    - relation: [delegate_permission/common.handle_all_urls]
      target:
        namespace: android_app
        package_name: co.hblabs.app
        sha256_cert_fingerprints: ["AB:CD"]
  llms_txt_path: seo/llms.txt

og:
  enabled: true
  brand: Example
  url_prefix: /og
`

// builderFS extends multiLangFS with a config/config.yaml + a
// seo/llms.txt the tests use as the LLMsTxtPath target. Returning a
// fresh map per call keeps tests independent — one test mutating
// the FS doesn't bleed into another.
func builderFS() fstest.MapFS {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(configYAML)}
	fs["seo/llms.txt"] = &fstest.MapFile{Data: []byte("# llms.txt body\n")}
	return fs
}

// TestInit_NilFSStashesError — passing nil fs.FS is a misuse that
// would otherwise NPE deep inside loadPages. Stashing the error on
// the builder lets the chain stay readable; Build surfaces it.
func TestInit_NilFSStashesError(t *testing.T) {
	site, err := Init(nil).Build()
	if site != nil {
		t.Errorf("site = %v, want nil on Init(nil)", site)
	}
	if err == nil || !strings.Contains(err.Error(), "non-nil fs.FS") {
		t.Errorf("err = %v, want \"non-nil fs.FS\" message", err)
	}
}

// TestBuilder_LoadConfigHappyPath — the canonical wiring: Init →
// LoadConfig → Build → working *Site. Asserts the YAML values made
// it across the boundary by inspecting the fields render copies into
// the Site struct.
func TestBuilder_LoadConfigHappyPath(t *testing.T) {
	fs := builderFS()
	site, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if site.canonicalHost != "https://hblabs.co" {
		t.Errorf("canonicalHost = %q", site.canonicalHost)
	}
	if site.defaultLang != "es" {
		t.Errorf("defaultLang = %q", site.defaultLang)
	}
	if site.seo.SecurityContact != "security@hblabs.co" {
		t.Errorf("seo.SecurityContact = %q", site.seo.SecurityContact)
	}
	if site.seo.LLMsTxt != "# llms.txt body\n" {
		t.Errorf("seo.LLMsTxt = %q, want body read from seo/llms.txt", site.seo.LLMsTxt)
	}
	if !site.ogEnabled {
		t.Error("ogEnabled = false, want true")
	}
	if site.ogBrand != "Example" {
		t.Errorf("ogBrand = %q", site.ogBrand)
	}
	if len(site.seo.AndroidAssetLinks) != 1 {
		t.Fatalf("AndroidAssetLinks len = %d, want 1", len(site.seo.AndroidAssetLinks))
	}
	if site.seo.AndroidAssetLinks[0].Target.PackageName != "co.hblabs.app" {
		t.Errorf("AndroidAssetLinks mapping lost package_name")
	}
}

// TestBuilder_LoadConfigParseErrorSurfacesAtBuild — a typo in the
// YAML doesn't NPE the chain or return a half-built Site. The error
// surfaces at Build with the original cause attached.
func TestBuilder_LoadConfigParseErrorSurfacesAtBuild(t *testing.T) {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{
		Data: []byte("canonical_host: https://x\nseo:\n  security_contac: typo\n"),
	}
	site, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if site != nil {
		t.Errorf("site != nil despite parse error")
	}
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// TestBuilder_LoadConfigShortCircuitsAfterEarlierError — once the
// builder has an error stashed (Init(nil) here), subsequent chain
// steps must NOT do extra IO. Pinning this avoids surprising
// "missing-file" errors masking the original Init misuse.
func TestBuilder_LoadConfigShortCircuitsAfterEarlierError(t *testing.T) {
	site, err := Init(nil).LoadConfig("would-not-exist").Build()
	if site != nil || err == nil {
		t.Fatalf("Build = (%v, %v), want (nil, error)", site, err)
	}
	if !strings.Contains(err.Error(), "non-nil fs.FS") {
		t.Errorf("err = %v, want the original Init error to win", err)
	}
}

// TestBuilder_LoadConfigBadLLMsTxtPathFails — when the YAML points
// at a missing file for LLMsTxt, Build must surface the read error
// instead of silently serving the built-in stub. Ops then know the
// rename / missing embed is the real problem, not the renderer.
func TestBuilder_LoadConfigBadLLMsTxtPathFails(t *testing.T) {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{
		Data: []byte("canonical_host: https://x\nseo:\n  llms_txt_path: missing/llms.txt\n"),
	}
	_, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if err == nil {
		t.Fatal("expected error from missing llms_txt_path")
	}
	if !strings.Contains(err.Error(), "llms_txt_path") {
		t.Errorf("err = %v, want \"llms_txt_path\" in message", err)
	}
}

// TestBuilder_WithDevOverridesYAML — env-driven Dev flag must beat
// whatever YAML carried. Right now the YAML can't set Dev, but the
// override pattern is what services will use to flip dev on locally
// (HBL_ENV=local) without editing config.yaml.
func TestBuilder_WithDevOverridesYAML(t *testing.T) {
	fs := builderFS()
	site, err := Init(fs).LoadConfig("config/config.yaml").WithDev(true).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !site.dev {
		t.Error("WithDev(true) did not propagate")
	}
}

// TestBuilder_WithOGStorageInjectsBackend — production OG needs a
// real Storage (MinIO/S3); the YAML can't carry it. The builder
// method is the seam.
func TestBuilder_WithOGStorageInjectsBackend(t *testing.T) {
	fs := builderFS()
	store := og.NewMemoryStorage("/og")
	site, err := Init(fs).LoadConfig("config/config.yaml").WithOGStorage(store).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if site.ogStorage != store {
		t.Error("WithOGStorage did not inject the provided backend")
	}
}

// TestBuilder_WithOGThemesIsApplied — extra og.Theme registrations
// flow through to og.New so Cards can reference them by name. nil
// themes (the omitted case) is the default and tested implicitly by
// every other Build call.
func TestBuilder_WithOGThemesIsApplied(t *testing.T) {
	def, err := og.DefaultTheme()
	if err != nil {
		t.Fatal(err)
	}
	fs := builderFS()
	site, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithOGThemes(map[string]og.Theme{"alt": def}).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// og.Generator carries themes internally; render's only proof
	// the map landed is that Render with the named theme works.
	if _, err := site.og.Render(og.Card{Title: "x", Theme: "alt"}); err != nil {
		t.Errorf("Render with custom theme = %v, want nil", err)
	}
}

// TestBuilder_BuildWithoutLoadConfigDelegatesToNew — Init + Build
// without LoadConfig must behave identically to render.New(Options{FS:
// fs}). Pinning the equivalence (here: same "SupportedLangs cannot
// be empty" error from New) means defaults stay in one place — if
// the builder ever silently filled in a language list it would mask
// the misuse the framework wants to surface at boot.
func TestBuilder_BuildWithoutLoadConfigDelegatesToNew(t *testing.T) {
	fs := multiLangFS()
	_, err := Init(fs).WithDev(false).Build()
	if err == nil || !strings.Contains(err.Error(), "SupportedLangs") {
		t.Fatalf("Build err = %v, want New's SupportedLangs error", err)
	}
}

// TestBuilder_TopLevelBrandFlowsToLayoutAndOG — cfg.Brand at the
// YAML top level lands both in render.Options.Brand (so layouts
// see {{ .Brand }}) and as the OG card's default brand line when
// og.brand is empty. Pins the "one source of truth" contract for
// the visible product name.
func TestBuilder_TopLevelBrandFlowsToLayoutAndOG(t *testing.T) {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
brand: Acme Co
og:
  enabled: true
`)}
	site, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if site.brand != "Acme Co" {
		t.Errorf("site.brand = %q, want \"Acme Co\"", site.brand)
	}
	if site.ogBrand != "Acme Co" {
		t.Errorf("site.ogBrand = %q, want it to inherit \"Acme Co\"", site.ogBrand)
	}
}

// TestBuilder_OGBrandOverridesTopLevel — when og.brand is set
// explicitly, it wins over the top-level brand. Use case:
// promotional OG card with a different label than the in-page
// chrome. Locks the override priority.
func TestBuilder_OGBrandOverridesTopLevel(t *testing.T) {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
brand: Acme Co
og:
  enabled: true
  brand: Acme Promo
`)}
	site, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if site.brand != "Acme Co" {
		t.Errorf("site.brand = %q, want \"Acme Co\" (top-level untouched)", site.brand)
	}
	if site.ogBrand != "Acme Promo" {
		t.Errorf("site.ogBrand = %q, want \"Acme Promo\" (explicit override)", site.ogBrand)
	}
}

// TestBuilder_BuildOptionsReturnsResolvedOptions — BuildOptions is
// the same merge path as Build, minus calling New. Wrappers (the
// dev server builds a fresh *Site per request) rely on it to share
// the YAML+overrides plumbing without paying for construction.
func TestBuilder_BuildOptionsReturnsResolvedOptions(t *testing.T) {
	fs := builderFS()
	opts, err := Init(fs).LoadConfig("config/config.yaml").WithDev(true).BuildOptions()
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if opts.CanonicalHost != "https://hblabs.co" {
		t.Errorf("CanonicalHost = %q", opts.CanonicalHost)
	}
	if !opts.Dev {
		t.Error("Dev override lost in BuildOptions")
	}
	if opts.SEO.LLMsTxt != "# llms.txt body\n" {
		t.Errorf("SEO.LLMsTxt = %q, want body read from seo/llms.txt", opts.SEO.LLMsTxt)
	}
	if opts.FS == nil {
		t.Error("FS not propagated into resolved Options")
	}
}

// TestBuilder_BuildOptionsSurfacesStashedError — if LoadConfig
// failed earlier, BuildOptions returns that error instead of
// hand-back a half-merged Options struct.
func TestBuilder_BuildOptionsSurfacesStashedError(t *testing.T) {
	_, err := Init(nil).BuildOptions()
	if err == nil {
		t.Fatal("expected stashed error from Init(nil)")
	}
}

// TestBuilder_ConfigExposesParsedYAML — Config() lets callers
// (notably render/dev.MountSite, main) read non-render fields
// like QuietPaths without re-parsing the YAML. Returns nil before
// LoadConfig runs and the parsed pointer afterwards.
func TestBuilder_ConfigExposesParsedYAML(t *testing.T) {
	b := Init(builderFS())
	if b.Config() != nil {
		t.Error("Config() before LoadConfig should be nil")
	}
	b = b.LoadConfig("config/config.yaml")
	cfg := b.Config()
	if cfg == nil {
		t.Fatal("Config() after LoadConfig returned nil")
	}
	if cfg.CanonicalHost != "https://hblabs.co" {
		t.Errorf("Config().CanonicalHost = %q, want canonical from YAML", cfg.CanonicalHost)
	}
}

// TestBuilder_WithExtraStaticAttachesSubtrees — pins the
// pass-through from the builder so services can plug shared assets
// (e.g. uikit.FS) without crafting an Options literal by hand.
func TestBuilder_WithExtraStaticAttachesSubtrees(t *testing.T) {
	extra := map[string]fs.FS{"uikit": fstest.MapFS{}}
	opts, err := Init(builderFS()).
		LoadConfig("config/config.yaml").
		WithExtraStatic(extra).
		BuildOptions()
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if _, ok := opts.ExtraStatic["uikit"]; !ok {
		t.Errorf("extra static missing uikit entry: %+v", opts.ExtraStatic)
	}
}

// TestBuilder_WithFuncMapAttachesServiceHelpers — pins the
// pass-through from the builder so a service can plug its own
// template helpers (project catalog, menu data) without crafting an
// Options literal by hand.
func TestBuilder_WithFuncMapAttachesServiceHelpers(t *testing.T) {
	fm := template.FuncMap{
		"greet": func() string { return "hola" },
	}
	opts, err := Init(builderFS()).
		LoadConfig("config/config.yaml").
		WithFuncMap(fm).
		BuildOptions()
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	if _, ok := opts.FuncMap["greet"]; !ok {
		t.Errorf("greet helper missing from resolved Options.FuncMap = %v", opts.FuncMap)
	}
}

// TestBuilder_DataFromYAMLReachesSite — config.yaml's `data:`
// section flows through the builder into Site.data so the helper
// sees what the operator wrote. Pins the loader → builder → Site
// path that backs `{{ data "..." }}`.
func TestBuilder_DataFromYAMLReachesSite(t *testing.T) {
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
data:
  myconfig:
    name: Helmer
`)}
	site, err := Init(fs).LoadConfig("config/config.yaml").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	my, ok := site.data["myconfig"].(map[string]any)
	if !ok {
		t.Fatalf("site.data[myconfig] type = %T, want map[string]any", site.data["myconfig"])
	}
	if my["name"] != "Helmer" {
		t.Errorf("site.data.myconfig.name = %v, want Helmer", my["name"])
	}
}

// TestBuilder_WithIconCatalogReachesSite — pins the builder
// pass-through for IconCatalog. Without this test a future refactor
// that drops the field from the merge step would silently lose
// `{{ icon }}` for every service.
func TestBuilder_WithIconCatalogReachesSite(t *testing.T) {
	catalog := map[string]string{"x": "<line/>"}
	fs := builderFS()
	site, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithIconCatalog(catalog).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if site.iconCatalog["x"] != "<line/>" {
		t.Errorf("iconCatalog = %v, want passthrough", site.iconCatalog)
	}
}

// TestBuilder_WithDataSchemaHappyPath — a struct that matches the
// YAML's data section validates clean. Documents the typical wire-
// up where the service declares a typed shape and the builder
// confirms config.yaml fits it.
func TestBuilder_WithDataSchemaHappyPath(t *testing.T) {
	type schema struct {
		MyConfig struct {
			Name string `yaml:"name"`
		} `yaml:"myconfig"`
	}
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
data:
  myconfig:
    name: Helmer
`)}
	var s schema
	_, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithDataSchema(&s).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if s.MyConfig.Name != "Helmer" {
		t.Errorf("schema not populated; got %+v", s)
	}
}

// TestBuilder_WithDataSchemaUnknownKeyFails — strict mode rejects
// keys the schema doesn't declare. This is the whole point of the
// opt-in: a typo in config.yaml's data: section (e.g. `nme`
// instead of `name`) fails at boot, not on a single 500-ing page
// at render time.
func TestBuilder_WithDataSchemaUnknownKeyFails(t *testing.T) {
	type schema struct {
		MyConfig struct {
			Name string `yaml:"name"`
		} `yaml:"myconfig"`
	}
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
data:
  myconfig:
    nme: typo
`)}
	var s schema
	_, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithDataSchema(&s).
		Build()
	if err == nil {
		t.Fatal("expected schema mismatch")
	}
	if !strings.Contains(err.Error(), "data schema mismatch") {
		t.Errorf("err = %v, want 'data schema mismatch' wrapper", err)
	}
}

// TestBuilder_WithDataSchemaEmptyDataSkipped — a service can ship a
// schema that's only validated when data: is present. An absent
// section keeps Build clean so services aren't forced to declare an
// empty data: block in every config.
func TestBuilder_WithDataSchemaEmptyDataSkipped(t *testing.T) {
	type schema struct {
		MyConfig struct {
			Name string `yaml:"name"`
		} `yaml:"myconfig"`
	}
	fs := builderFS() // config has no data: section
	var s schema
	_, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithDataSchema(&s).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestBuilder_BuildOptionsRunsSchemaValidation — wrappers that go
// through BuildOptions instead of Build (the dev server is the
// canonical example) must still see schema errors. Otherwise a typo
// would slip past in dev mode and only surface in prod.
func TestBuilder_BuildOptionsRunsSchemaValidation(t *testing.T) {
	type schema struct {
		Foo string `yaml:"foo"`
	}
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
data:
  unknown: bad
`)}
	var s schema
	_, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithDataSchema(&s).
		BuildOptions()
	if err == nil {
		t.Fatal("expected schema mismatch via BuildOptions")
	}
}

// TestBuilder_WithDataSchemaMarshalErrorWraps — covers the
// virtually unreachable yaml.Marshal failure path via the
// marshalDataFn seam (see the var's doc). Without this seam the
// branch stays uncovered because every map[string]any produced by
// the YAML decoder is serialisable by design.
func TestBuilder_WithDataSchemaMarshalErrorWraps(t *testing.T) {
	defer func(orig func(any) ([]byte, error)) { marshalDataFn = orig }(marshalDataFn)
	marshalDataFn = func(any) ([]byte, error) {
		return nil, errMarshalFault
	}

	type schema struct {
		Foo string `yaml:"foo"`
	}
	fs := multiLangFS()
	fs["config/config.yaml"] = &fstest.MapFile{Data: []byte(`
canonical_host: https://x
supported_langs: [es]
data:
  foo: bar
`)}
	var s schema
	_, err := Init(fs).
		LoadConfig("config/config.yaml").
		WithDataSchema(&s).
		Build()
	if err == nil {
		t.Fatal("expected marshal failure to surface")
	}
	if !strings.Contains(err.Error(), "marshal data for schema check") {
		t.Errorf("err = %v, want marshal-failure wrapper", err)
	}
}

var errMarshalFault = errors.New("simulated marshal fault")

// TestLoadPages_DirIndexCollapsesToDirKey — pages/<dir>/index.html.tmpl
// maps to /<lang>/<dir>, not /<lang>/<dir>/index. Sub-trees can
// therefore declare a default page without a parallel
// pages/<dir>.html.tmpl twin. Pins the switch branch in loadPages
// that does the trim.
func TestLoadPages_DirIndexCollapsesToDirKey(t *testing.T) {
	fs := fstest.MapFS{
		"pages/team/index.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "content" }}team{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fs, Options{})
	if _, ok := s.pages["team"]; !ok {
		t.Errorf("missing team page key — pages/team/index.html.tmpl should map to 'team'")
	}
	if _, ok := s.pages["team/index"]; ok {
		t.Errorf("team/index leaked into pages map; expected the /index suffix to collapse")
	}
}

// TestToRenderAssetLinks_EmptyReturnsNil — the helper short-
// circuits empty input so the resulting Options.SEO.AndroidAssetLinks
// stays nil (matching the documented zero-value contract: nil →
// "[]" served at /.well-known/assetlinks.json). Tested directly to
// avoid plumbing a YAML round-trip just for the boundary.
func TestToRenderAssetLinks_EmptyReturnsNil(t *testing.T) {
	if got := toRenderAssetLinks(nil); got != nil {
		t.Errorf("toRenderAssetLinks(nil) = %v, want nil", got)
	}
}
