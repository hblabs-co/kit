// Package siteassets resolves per-site MinIO/S3 assets to their
// content-addressed counterparts via a manifest emitted by storage-sync.
//
// # Why a manifest
//
// The platform serves per-site logos, demo fixtures, and customer
// uploads from MinIO at `cloud/assets/<organization_id>/<site_id>/<key>`.
// Without versioning, replacing `logo.png` under the same key keeps
// browsers and CDNs serving the old bytes until heuristic TTLs expire.
// The fix is the standard content-addressable pattern: upload the
// file as `logo-<hash>.png` and let the URL change when the content
// changes. Templates keep referring to the logical name (`logo.png`)
// and a sidecar manifest carries the logical → hashed mapping so the
// runtime can rewrite at render time.
//
// # Flow
//
//	storage-sync sync --apply:
//	    1. SHA-256 of each file → 8 hex chars.
//	    2. PUT as <basename>-<hash>.<ext> with
//	       Cache-Control: public, max-age=31536000, immutable.
//	    3. Write `_manifest.json` per <orgID>/<siteID>/ dir.
//
//	service render path:
//	    1. ManifestCache.Get(orgID, siteID) loads _manifest.json
//	       lazily, TTL-cached in memory.
//	    2. cdnURL template helper rewrites
//	       "cloud/assets/<org>/<site>/logo.png" →
//	       "cloud/assets/<org>/<site>/logo-abc12345.png" via the
//	       manifest, then composes the final URL via siteurl.BuildCDNURL.
//
// # Decoupling from a storage backend
//
// The Loader function in ManifestCache is the only seam between this
// package and the actual storage. A service using minio-go directly
// supplies a closure; a future consumer of `service-storage` supplies
// a different one; tests inject in-memory fixtures. The package
// itself never imports a storage SDK.
//
// # Graceful degradation
//
// Every public helper falls through to the unhashed name when the
// manifest is absent, the asset isn't in it, or the loader errors.
// This keeps services safe to wire on day one, before storage-sync
// has ever run for that site, and survives a transiently broken
// bucket.
package siteassets
