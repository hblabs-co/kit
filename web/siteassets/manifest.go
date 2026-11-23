package siteassets

import "encoding/json"

// ManifestCurrentSchemaVersion is the wire-shape version writers
// stamp into every new manifest. Bump on any breaking field change
// and add the older shape to an UpgradeFromV path when that ships —
// same per-document `_v` convention as Mongo models.
const ManifestCurrentSchemaVersion = 1

// ManifestBasename is the conventional filename for the per-site
// asset manifest emitted by storage-sync and consumed by every
// Loader. Exported so the writer (storage-sync) and the readers
// (Loaders) reference one constant — drift here means readers fetch
// a path the writer never produced.
const ManifestBasename = "_manifest.json"

// Manifest maps logical asset names (e.g. "logo.png") to their
// content-addressed hashed counterparts (e.g. "logo-abc12345.png")
// within one `<organization_id>/<site_id>/` scope. storage-sync emits
// one Manifest per site directory; ManifestCache reads them on
// demand to rewrite template URLs to their hashed form.
//
// The map is intentionally flat — subdirectories under a site
// (e.g. `img/hero.jpg`) are encoded as forward-slash keys, so a
// single hash lookup serves any depth. Keeping logical→hashed in one
// JSON keeps the rewrite a memory map lookup instead of a per-key
// HEAD round trip.
type Manifest struct {
	SchemaV int               `json:"_v"`
	Assets  map[string]string `json:"assets"`
}

// Resolve returns the hashed name for `logical` if the manifest
// carries it, or "" otherwise. A nil receiver is acceptable and also
// returns "" — callers pass a ManifestCache.Get result directly
// without nil-checking and rely on the "" sentinel to fall back to
// the logical name.
func (m *Manifest) Resolve(logical string) string {
	if m == nil {
		return ""
	}
	return m.Assets[logical]
}

// ParseManifest decodes raw JSON bytes into a Manifest. Empty input
// returns (nil, nil) so callers can treat "manifest absent" and
// "manifest is the empty object" as equivalently "no rewriting
// possible" without surfacing a parse error to the template hot path.
func ParseManifest(raw []byte) (*Manifest, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Assets == nil {
		m.Assets = map[string]string{}
	}
	return &m, nil
}
