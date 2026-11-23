package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// happyYAML is the canonical config — every field a service is
// likely to set, mirroring what the package doc shows. Used as the
// "did the loader round-trip every key" baseline for the rest of
// the tests.
const happyYAML = `
canonical_host: https://hblabs.co
default_lang: es
supported_langs: [es, en, de, nl]

seo:
  disable_robots: false
  disable_sitemap: false
  disable_llms_txt: false
  disable_security_txt: false
  disable_aasa: false
  disable_assetlinks: false
  security_contact: security@hblabs.co
  preferred_languages: [es, en]
  apple_team_id: ABC1234567
  ios_bundle_id: co.hblabs.app
  aasa_components: ["/es/*", "/en/*"]
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

// TestLoad_HappyPath round-trips every YAML field into the Go struct
// and asserts the values land where they're expected. Pins the snake_case
// → CamelCase mapping so a typo in any yaml tag fails this test.
func TestLoad_HappyPath(t *testing.T) {
	fsys := fstest.MapFS{
		"config/config.yaml": &fstest.MapFile{Data: []byte(happyYAML)},
	}
	cfg, err := Load(fsys, "config/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CanonicalHost != "https://hblabs.co" {
		t.Errorf("CanonicalHost = %q", cfg.CanonicalHost)
	}
	if cfg.DefaultLang != "es" {
		t.Errorf("DefaultLang = %q", cfg.DefaultLang)
	}
	if got, want := cfg.SupportedLangs, []string{"es", "en", "de", "nl"}; !equalSlice(got, want) {
		t.Errorf("SupportedLangs = %v, want %v", got, want)
	}
	if cfg.SEO.SecurityContact != "security@hblabs.co" {
		t.Errorf("SEO.SecurityContact = %q", cfg.SEO.SecurityContact)
	}
	if cfg.SEO.AppleTeamID != "ABC1234567" {
		t.Errorf("SEO.AppleTeamID = %q", cfg.SEO.AppleTeamID)
	}
	if cfg.SEO.IOSBundleID != "co.hblabs.app" {
		t.Errorf("SEO.IOSBundleID = %q", cfg.SEO.IOSBundleID)
	}
	if got, want := cfg.SEO.AASAComponents, []string{"/es/*", "/en/*"}; !equalSlice(got, want) {
		t.Errorf("SEO.AASAComponents = %v, want %v", got, want)
	}
	if cfg.SEO.LLMsTxtPath != "seo/llms.txt" {
		t.Errorf("SEO.LLMsTxtPath = %q", cfg.SEO.LLMsTxtPath)
	}
	if len(cfg.SEO.AndroidAssetLinks) != 1 {
		t.Fatalf("AndroidAssetLinks len = %d, want 1", len(cfg.SEO.AndroidAssetLinks))
	}
	link := cfg.SEO.AndroidAssetLinks[0]
	if link.Target.PackageName != "co.hblabs.app" {
		t.Errorf("link.Target.PackageName = %q", link.Target.PackageName)
	}
	if !cfg.OG.Enabled {
		t.Error("OG.Enabled should be true")
	}
	if cfg.OG.Brand != "Example" {
		t.Errorf("OG.Brand = %q", cfg.OG.Brand)
	}
	if cfg.OG.URLPrefix != "/og" {
		t.Errorf("OG.URLPrefix = %q", cfg.OG.URLPrefix)
	}
}

// TestLoad_ZeroValuesForOmittedFields — fields not present in YAML
// must stay at their Go zero value. The contract is that defaults
// live in render.New, NOT in this loader; if the loader started
// inventing values here the two paths would drift.
func TestLoad_ZeroValuesForOmittedFields(t *testing.T) {
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte("canonical_host: https://x\n")},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultLang != "" {
		t.Errorf("DefaultLang = %q, want \"\" (zero value)", cfg.DefaultLang)
	}
	if cfg.SupportedLangs != nil {
		t.Errorf("SupportedLangs = %v, want nil (zero value)", cfg.SupportedLangs)
	}
	if cfg.OG.Enabled {
		t.Error("OG.Enabled = true, want false (zero value)")
	}
	if cfg.SEO.DisableRobots {
		t.Error("SEO.DisableRobots = true, want false (zero value)")
	}
}

// TestLoad_MissingFileWraps — operators see the file path in the
// error message, not just "no such file or directory". Wrapping
// also keeps errors.Is(err, fs.ErrNotExist) working for callers
// that want to treat "no config" as "use code defaults".
func TestLoad_MissingFileWraps(t *testing.T) {
	fsys := fstest.MapFS{}
	_, err := Load(fsys, "config/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "config/config.yaml") {
		t.Errorf("err = %v, want path in message", err)
	}
}

// TestLoad_UnknownKeyIsRejected — strict mode is the whole reason
// to use a custom loader; a typo like security_contac (missing t)
// must fail at boot rather than silently fall through to "" and
// confuse operators when security.txt keeps serving a placeholder.
func TestLoad_UnknownKeyIsRejected(t *testing.T) {
	bad := "seo:\n  security_contac: oops@example.com\n"
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(bad)},
	}
	_, err := Load(fsys, "c.yaml")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want \"decode\" wrapper", err)
	}
}

// TestLoad_QuietPathsDefaultsOnlyWhenYAMLEmpty — YAML omits
// quiet_paths → resolved slice is exactly DefaultQuietPaths in
// declaration order. Pins the framework baseline.
func TestLoad_QuietPathsDefaultsOnlyWhenYAMLEmpty(t *testing.T) {
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte("canonical_host: https://x\n")},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !equalSlice(cfg.QuietPaths, DefaultQuietPaths) {
		t.Errorf("QuietPaths = %v, want DefaultQuietPaths %v", cfg.QuietPaths, DefaultQuietPaths)
	}
}

// TestLoad_QuietPathsMergedWithDefaults — extras in YAML are
// appended after DefaultQuietPaths, preserving each side's order.
// Services declare only their own service-specific noisy paths;
// the framework defaults stay on without restating them.
func TestLoad_QuietPathsMergedWithDefaults(t *testing.T) {
	body := "quiet_paths:\n  - /heartbeat\n  - /assets/v2/*\n"
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := append(append([]string(nil), DefaultQuietPaths...), "/heartbeat", "/assets/v2/*")
	if !equalSlice(cfg.QuietPaths, want) {
		t.Errorf("QuietPaths = %v, want %v (defaults ++ extras)", cfg.QuietPaths, want)
	}
}

// TestMergeQuietPaths_DedupesWithinDefaults — defensive: if a
// future edit accidentally introduces a duplicate inside
// DefaultQuietPaths, the dedupe pass still produces a clean
// resolved slice. Tested directly because production
// DefaultQuietPaths has no dups today.
func TestMergeQuietPaths_DedupesWithinDefaults(t *testing.T) {
	got := mergeQuietPaths([]string{"/a", "/a", "/b"}, nil)
	want := []string{"/a", "/b"}
	if !equalSlice(got, want) {
		t.Errorf("mergeQuietPaths = %v, want %v", got, want)
	}
}

// TestLoad_QuietPathsDeduped — a YAML entry that duplicates a
// default doesn't appear twice in the resolved slice. Locks the
// "second occurrence dropped" contract so a service that copy-
// pastes "/healthz" alongside their extras doesn't get a noisy
// duplicated entry.
func TestLoad_QuietPathsDeduped(t *testing.T) {
	body := "quiet_paths:\n  - /healthz\n  - /heartbeat\n  - /heartbeat\n"
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := append(append([]string(nil), DefaultQuietPaths...), "/heartbeat")
	if !equalSlice(cfg.QuietPaths, want) {
		t.Errorf("QuietPaths = %v, want %v (no duplicates)", cfg.QuietPaths, want)
	}
}

// TestLoad_EnvVarOverridesLiteralFallback — when the env var is
// set + non-empty, the value supersedes whatever followed ":-" in
// the YAML. This is the documented "env wins" priority that
// services rely on to inject per-environment secrets without
// editing the source-controlled YAML.
func TestLoad_EnvVarOverridesLiteralFallback(t *testing.T) {
	t.Setenv("HB_APPLE_TEAM_ID", "FROM_ENV_999")
	body := `seo:
  apple_team_id: "${HB_APPLE_TEAM_ID:-FALLBACK_FROM_YAML}"
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SEO.AppleTeamID != "FROM_ENV_999" {
		t.Errorf("AppleTeamID = %q, want env value to win", cfg.SEO.AppleTeamID)
	}
}

// TestLoad_EnvVarFallbackUsedWhenUnset — without the env var the
// scalar resolves to whatever followed ":-". Locks in the
// "fallback for local dev" path so config.yaml stays a self-
// contained working file even before any env wiring exists.
func TestLoad_EnvVarFallbackUsedWhenUnset(t *testing.T) {
	t.Setenv("HB_APPLE_TEAM_ID", "")
	body := `seo:
  apple_team_id: "${HB_APPLE_TEAM_ID:-FALLBACK}"
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SEO.AppleTeamID != "FALLBACK" {
		t.Errorf("AppleTeamID = %q, want \"FALLBACK\" (env unset)", cfg.SEO.AppleTeamID)
	}
}

// TestLoad_EnvVarBareReferenceResolvesToEmpty — without the ":-"
// fallback shorthand, an unset env var resolves to the empty
// string. Documents the "explicit silence" pattern: services that
// truly want the field to be empty when no env wiring exists write
// `${HB_FOO}` (no fallback) so the YAML reads cleanly.
func TestLoad_EnvVarBareReferenceResolvesToEmpty(t *testing.T) {
	t.Setenv("HB_SECURITY_CONTACT", "")
	body := `seo:
  security_contact: "${HB_SECURITY_CONTACT}"
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SEO.SecurityContact != "" {
		t.Errorf("SecurityContact = %q, want \"\"", cfg.SEO.SecurityContact)
	}
}

// TestLoad_BareDollarLeftAlone — values that contain a literal "$"
// but NO braces (jQuery/CSS-style snippets) must pass through
// unchanged. The brace form is opt-in for substitution; the bare
// "$NAME" form would silently swallow legitimate content.
func TestLoad_BareDollarLeftAlone(t *testing.T) {
	body := `og:
  brand: "$money$"
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OG.Brand != "$money$" {
		t.Errorf("OG.Brand = %q, want \"$money$\" (bare $ should survive)", cfg.OG.Brand)
	}
}

// TestLoad_UnterminatedBraceLeftAlone — a stray "${" without a
// closing "}" is passed through unchanged so a typo doesn't take
// down the boot with a confusing parser error. The unmatched
// fragment ends up in the field verbatim, which is loud enough to
// notice on first run.
func TestLoad_UnterminatedBraceLeftAlone(t *testing.T) {
	body := `og:
  brand: "${HB_FOO"
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OG.Brand != "${HB_FOO" {
		t.Errorf("OG.Brand = %q, want \"${HB_FOO\" (unterminated brace passes through)", cfg.OG.Brand)
	}
}

// TestLoad_ReadErrorWraps — when the file opens but ReadAll fails
// (truncated read, network FS hiccup, mocked failure), Load must
// surface the error with the file path so operators see WHICH
// config blew up. The errFS scaffolding gives us a deterministic
// way to drive that branch without contorting fstest.MapFS.
func TestLoad_ReadErrorWraps(t *testing.T) {
	want := errors.New("simulated read fault")
	fsys := readErrFS{path: "c.yaml", err: want}
	_, err := Load(fsys, "c.yaml")
	if err == nil {
		t.Fatal("expected error from ReadAll fault")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want it to wrap %v", err, want)
	}
}

// readErrFS opens c.yaml as a file whose Read returns err. Used by
// TestLoad_ReadErrorWraps to drive the io.ReadAll branch — fstest.MapFS
// can't error on Read, so we layer a thin fs.FS / fs.File pair
// dedicated to that one branch.
type readErrFS struct {
	path string
	err  error
}

func (f readErrFS) Open(name string) (fs.File, error) {
	if name != f.path {
		return nil, fs.ErrNotExist
	}
	return readErrFile{err: f.err}, nil
}

type readErrFile struct{ err error }

func (f readErrFile) Stat() (fs.FileInfo, error) { return nil, f.err }
func (f readErrFile) Read(_ []byte) (int, error) { return 0, f.err }
func (f readErrFile) Close() error               { return nil }

// TestLoad_DataRoundTrip — the free-form data: section keeps its
// nested shape so render's `data` helper can walk it via dotted
// paths. Pins the loader's parse of nested maps + scalar leaves and
// the icons-extension convention (kebab-case key → inner SVG).
func TestLoad_DataRoundTrip(t *testing.T) {
	body := `
data:
  icons:
    love: '<path d="M1 2"/>'
  myconfig:
    subconfig:
      name: Helmer
      count: 42
`
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	icons, ok := cfg.Data["icons"].(map[string]any)
	if !ok {
		t.Fatalf("data.icons type = %T, want map[string]any", cfg.Data["icons"])
	}
	if icons["love"] != `<path d="M1 2"/>` {
		t.Errorf("data.icons.love = %v, want raw SVG body", icons["love"])
	}
	my, ok := cfg.Data["myconfig"].(map[string]any)
	if !ok {
		t.Fatalf("data.myconfig type = %T", cfg.Data["myconfig"])
	}
	sub, ok := my["subconfig"].(map[string]any)
	if !ok {
		t.Fatalf("data.myconfig.subconfig type = %T", my["subconfig"])
	}
	if sub["name"] != "Helmer" {
		t.Errorf("data.myconfig.subconfig.name = %v", sub["name"])
	}
	if sub["count"] != 42 {
		t.Errorf("data.myconfig.subconfig.count = %v (%T)", sub["count"], sub["count"])
	}
}

// TestLoad_DataOmittedStaysNil — leaving the data: section out of
// the YAML keeps the field at its zero value (nil map) so services
// that don't use it pay nothing. Documents that the loader doesn't
// invent an empty map.
func TestLoad_DataOmittedStaysNil(t *testing.T) {
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte("canonical_host: https://x\n")},
	}
	cfg, err := Load(fsys, "c.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Data != nil {
		t.Errorf("Data = %v, want nil when omitted", cfg.Data)
	}
}

// TestLoad_MalformedYAMLIsRejected — when the YAML itself is
// invalid (not just unknown keys), Load wraps the parser error so
// operators see both the file path and the parse position.
func TestLoad_MalformedYAMLIsRejected(t *testing.T) {
	bad := "seo:\n  security_contact: [unclosed\n"
	fsys := fstest.MapFS{
		"c.yaml": &fstest.MapFile{Data: []byte(bad)},
	}
	_, err := Load(fsys, "c.yaml")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if b[i] != v {
			return false
		}
	}
	return true
}
