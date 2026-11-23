// Package siteinfo is the shared catalog of HB Labs site constants
// that are neither translatable copy (those live in each service's
// locales/*.json) nor derived from routing (those come from pageURL /
// canonical). Think: footer social links and the legal entity name —
// values that would otherwise be hardcoded in templates and
// grep-and-replaced on every change.
//
// It is shared because more than one service surfaces the same footer: every HB Labs site links
// the same socials and shows the same company name. The wiring mirrors
// web/uikit exactly — services merge siteinfo.FuncMap() into
// their site.Options.FuncMap, so a developer who knows the uikit
// pattern knows this one.
//
// Structural data in siteinfo.yaml (go:embedded at build time), strict
// YAML (KnownFields(true)), errors at boot not at render. The YAML
// lives inside this package dir because //go:embed cannot reference
// parent directories — a literal repo-root file could not be baked
// into the self-contained service binaries.
//
// Editing flow:
//
//   - Change a value / add a social link → edit siteinfo.yaml.
//   - Verify → go test ./web/siteinfo/
//   - Templates call {{ legalEntity }} and {{ range socialLinks }}.
package siteinfo

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"

	"gopkg.in/yaml.v3"

	"github.com/hblabs-co/kit/core/envcfg"
)

// infoFS embeds the on-disk YAML. Kept package-private so callers
// reach the data via the exported Info var / FuncMap helpers.
//
//go:embed siteinfo.yaml
var infoFS embed.FS

// infoPath is the embedded path for the YAML. Pulled out as a const
// so the loader and tests name it once.
const infoPath = "siteinfo.yaml"

// SocialLink is one entry in a service footer's "Social" column.
type SocialLink struct {
	// Label is the visible link text. Not translated — account /
	// brand names are language-neutral.
	Label string `yaml:"label"`

	// URL is the destination. The layout renders it with
	// target="_blank" rel="noopener".
	URL string `yaml:"url"`
}

// SiteInfo is the whole siteinfo.yaml document: the constants the
// footer needs that are neither locale copy nor routed URLs.
type SiteInfo struct {
	// LegalEntity is the company name in the footer copyright line
	// (e.g. "HB Labs SAS"). One edit renames it everywhere.
	LegalEntity string `yaml:"legal_entity"`

	// LegalAddress is the registered address of the legal entity,
	// surfaced on legal pages (privacy, imprint) via {{ legalAddress }}.
	LegalAddress string `yaml:"legal_address"`

	// ContactEmail is the public address surfaced in mailto: links
	// (plans-page CTAs, "write to us"). One edit, every service.
	ContactEmail string `yaml:"contact_email"`

	// PrivacyEmail is the dedicated mailbox for data-protection
	// requests (GDPR / RGPD), surfaced on the privacy page via
	// {{ privacyEmail }}. Kept distinct from ContactEmail so DPO
	// traffic doesn't drown in general inquiries.
	PrivacyEmail string `yaml:"privacy_email"`

	// Social is the footer social column, in declared (render) order.
	Social []SocialLink `yaml:"social"`
}

// loadFn is the variable seam tests swap to drive mustLoad's panic
// branch without breaking the real (always-valid) embedded file.
// Same pattern as projects.loadFn and render's seam vars.
var loadFn = load

// mustLoad reads the embedded YAML and panics on error. The file is
// baked into every consuming service binary, so a malformed
// siteinfo.yaml is a build-time bug we want surfaced at boot (and on
// every test that imports the package), never at the first request.
func mustLoad() SiteInfo {
	info, err := loadFn(infoFS, infoPath)
	if err != nil {
		panic(fmt.Errorf("siteinfo: load %s: %w", infoPath, err))
	}
	return info
}

// Info is populated from siteinfo.yaml at package init. Source of
// truth for the footer's social links + legal entity across every
// HB Labs service.
var Info = mustLoad()

// load parses siteinfo.yaml from the given filesystem. Pulled out of
// mustLoad so tests exercise the read/parse error paths against a
// controlled fs.FS. Mirrors projects.load: ${VAR} expansion first,
// then strict decode.
func load(fsys fs.FS, path string) (SiteInfo, error) {
	raw, err := fs.ReadFile(fsys, path)
	if err != nil {
		return SiteInfo{}, fmt.Errorf("read %s: %w", path, err)
	}
	// Expand "${VAR}" / "${VAR:-default}" before the YAML parser sees
	// the bytes so a value like `url: ${HB_SOCIAL_GITHUB:-https://...}`
	// resolves from the environment at boot. Same brace-only grammar
	// render's config loader and projects.yaml use.
	expanded := envcfg.Expand(string(raw))
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true) // strict: a misspelled key fails the boot
	var info SiteInfo
	if err := dec.Decode(&info); err != nil {
		return SiteInfo{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return info, nil
}

// FuncMap exposes the helpers every service merges into its
// site.Options.FuncMap, exactly like uikit.FuncMap(). Centralising
// the helper names here means a future addition lands once and
// propagates on next deploy, without per-service wiring drift.
//
// Helper signatures:
//
//	{{ legalEntity }}        → string, footer copyright line
//	{{ legalAddress }}       → string, registered address (legal pages)
//	{{ contactEmail }}       → string, public contact (mailto:)
//	{{ privacyEmail }}       → string, GDPR / RGPD contact (mailto:)
//	{{ range socialLinks }}  → []SocialLink, footer social column
//
// socialLinks returns a fresh copy so a template (or a buggy helper
// downstream) can't mutate the shared Info.Social backing array.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"legalEntity":  func() string { return Info.LegalEntity },
		"legalAddress": func() string { return Info.LegalAddress },
		"contactEmail": func() string { return Info.ContactEmail },
		"privacyEmail": func() string { return Info.PrivacyEmail },
		"socialLinks": func() []SocialLink {
			out := make([]SocialLink, len(Info.Social))
			copy(out, Info.Social)
			return out
		},
	}
}
