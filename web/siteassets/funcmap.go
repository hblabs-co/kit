package siteassets

import (
	"context"
	"html/template"
	"regexp"

	"github.com/hblabs-co/kit/web/siteurl"
)

// assetPathPattern matches the canonical bucket-relative shape
// `cloud/assets/<organization_id>/<site_id>/<rest>` used in template
// inputs (e.g. `projects.yaml` `logo:` fields). Capture groups:
//
//	1 = organization_id (hb_org_<nanoid>)
//	2 = site_id         (hb_site_<nanoid>)
//	3 = the rest        (logical filename, possibly with subdirs
//	                     like "img/hero.jpg")
//
// Paths outside this shape — absolute URLs, ExtraStatic mounts,
// arbitrary single-segment relatives — pass through the helpers
// untouched.
var assetPathPattern = regexp.MustCompile(`^cloud/assets/(hb_org_[^/]+)/(hb_site_[^/]+)/(.+)$`)

// MakeCDNURL returns a drop-in replacement for siteurl.BuildCDNURL that
// rewrites bucket-relative paths matching assetPathPattern to their
// content-addressed counterparts before composing the final CDN URL.
//
// Behaviour:
//   - Matching path + manifest has the asset → emit hashed name.
//   - Matching path + manifest absent or missing asset → emit
//     unhashed name (graceful fallback).
//   - Non-matching path (absolute URL, ExtraStatic mount, …) →
//     delegates straight to siteurl.BuildCDNURL.
//
// Templates always end up with a working URL, even before
// storage-sync has ever run for that site.
func MakeCDNURL(cache *ManifestCache) func(string) string {
	return func(path string) string {
		if rewritten, ok := rewriteAssetPath(cache, path); ok {
			path = rewritten
		}
		return siteurl.BuildCDNURL(path)
	}
}

// rewriteAssetPath rewrites a bucket-relative asset path to its
// content-addressed form when:
//
//   - the path matches assetPathPattern, AND
//   - the cache has (or can fetch) a manifest for the org/site, AND
//   - the manifest knows the logical name.
//
// Returns (rewritten, true) on success, or (path, false) otherwise.
// Loader errors are intentionally swallowed here — a template helper
// returning "image will silently 404 in the worst case" is preferable
// to one that surfaces an error to a marketing page on the hot path.
func rewriteAssetPath(cache *ManifestCache, path string) (string, bool) {
	if cache == nil {
		return path, false
	}
	m := assetPathPattern.FindStringSubmatch(path)
	if m == nil {
		return path, false
	}
	orgID, siteID, logical := m[1], m[2], m[3]
	manifest, _ := cache.Get(context.Background(), orgID, siteID)
	hashed := manifest.Resolve(logical)
	if hashed == "" {
		return path, false
	}
	return "cloud/assets/" + orgID + "/" + siteID + "/" + hashed, true
}

// FuncMap returns the template helpers a service merges into its
// render funcmap to gain manifest-aware asset resolution. Two
// helpers, two call shapes:
//
//   - cdnURL replaces siteurl.BuildCDNURL: same signature, paths that
//     match the canonical per-site convention get rewritten to their
//     hashed names; everything else passes through.
//   - siteAsset is the explicit form for multi-tenant render contexts
//     (service-sites, service-console) where the template has the
//     tenant in scope: `{{ siteAsset .Site.OrgID .Site.ID "logo.png" }}`.
//
// Both fall back gracefully — missing manifest, missing asset, nil
// cache, paths outside the convention — by returning the unhashed
// URL. Safe to wire on day one, before storage-sync has run.
func FuncMap(cache *ManifestCache) template.FuncMap {
	cdnURL := MakeCDNURL(cache)
	return template.FuncMap{
		"cdnURL": cdnURL,
		"siteAsset": func(orgID, siteID, name string) string {
			return cdnURL("cloud/assets/" + orgID + "/" + siteID + "/" + name)
		},
	}
}
