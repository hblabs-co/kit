package ids

import (
	"strings"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

// PublicIDPrefix is the global namespace every public-facing
// identifier carries. The "hb_" prefix keeps platform IDs greppable
// in logs and lets secret scanners pick out HB Labs values from
// generic strings.
const PublicIDPrefix = "hb_"

// publicIDSeparator sits between the kind and the nanoid suffix.
// Named so the format string in NewPublicID / ValidatePublicID
// reads as intent rather than as a stray underscore.
const publicIDSeparator = "_"

// PublicKind values are the 2-4 char model identifiers that sit
// between the "hb_" prefix and the nanoid suffix. They are part of
// the wire format — renaming a constant rotates every ID of that
// kind and is a breaking change for clients holding old values.
//
// Keep this catalogue in sync with notes/models_consolidation_plan.md
// §A.11.
const (
	PublicKindOrganization = "org"  // tenant org
	PublicKindUser         = "usr"  // platform user
	PublicKindUserEmail    = "em"   // user email address
	PublicKindMembership   = "mem"  // user-org membership
	PublicKindSite         = "site" // hosted site
	PublicKindSiteHost     = "host" // site hostname
	PublicKindPaymentState = "pay"  // payment state
	PublicKindMagicLink    = "ml"   // magic-link token
	PublicKindSession      = "ses"  // auth session
	PublicKindAuthIntent   = "ai"   // auth intent
	PublicKindAuthBlock    = "blk"  // auth block
	PublicKindAuthOptOut   = "oo"   // auth opt-out
	PublicKindAPIKey       = "ak"   // API key
	PublicKindInvitation   = "inv"  // org invitation
	PublicKindRequest      = "req"  // request log row
	// Ingest-site kinds. Values use the generic "is*" prefixes matching
	// the generalised "ingest site" model (the vertical lives in the
	// row's `type` field, not the ID). These replaced the legacy "dts*"
	// ("dog trainer site") values during the 2026-05 rename, done while
	// the data plane was empty so no public_id or foreign key needed
	// rewriting. Like every prefix here they are now part of the wire
	// format: add, never mutate.
	PublicKindIngestSite          = "is"   // ingest prospect site
	PublicKindIngestSiteFetch     = "isf"  // per-site crawl summary
	PublicKindIngestSiteFetchPage = "isfp" // per-URL fetch audit row
	PublicKindIngestSiteAnalysis  = "isa"  // per-site Stage 1b analysis row
	PublicKindIngestSiteResults   = "isr"  // per-site Stage 2 LLM results row
	PublicKindAppleAttestKey      = "aak"  // apple attest key (DCAppAttestService)
	PublicKindDevice              = "dev"  // DeviceSig Secure-Enclave device (macOS)
	// Mail signatures (service-signatures).
	PublicKindSignature    = "sig"     // user mail signature record
	PublicKindCompat       = "compat"  // macOS+Mail support matrix row
	PublicKindCompatReport = "creport" // compat-check telemetry row
	// Apps & feature flags (service-flags).
	PublicKindApp         = "app" // tenant-owned native application
	PublicKindFeatureFlag = "ff"  // scoped boolean feature flag
)

// NewPublicID returns a typed public-facing ID of the form
// "hb_<kind>_<nanoid>" using DefaultLength for the nanoid suffix
// (~126 bits of entropy). Callers should use the PublicKind*
// constants instead of literal strings — typed prefixes catch
// "wrong kind" mistakes at code-review time, not just at runtime.
func NewPublicID(kind string) string {
	return PublicIDPrefix + kind + publicIDSeparator + gonanoid.Must(DefaultLength)
}

// ValidatePublicID reports whether id matches the
// "hb_<kind>_<suffix>" shape. It is intentionally cheap: prefix +
// kind + separator must match, and the suffix must be non-empty.
// It does NOT enforce the suffix alphabet — that would couple the
// validator to a specific gonanoid release.
//
// Use at API boundaries to reject mismatched kinds (eg a user_id
// where a site_id was expected) before the resolved row even hits
// the database.
func ValidatePublicID(kind, id string) bool {
	want := PublicIDPrefix + kind + publicIDSeparator
	if !strings.HasPrefix(id, want) {
		return false
	}
	return len(id) > len(want)
}
