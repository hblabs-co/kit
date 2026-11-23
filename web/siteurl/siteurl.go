// Package siteurl holds the environment-aware URL and host helpers
// every HB Labs site needs at render time: the dev/prod mode
// check, the localhost→LAN rewrite, the CDN URL composer, and the
// derived CSP img-src origin.
//
// Why its own leaf package (and not part of web/site): these
// are pure string/env helpers with a tiny dependency surface
// (constants, envcfg, banner). web/site is the composition
// root — it drags in the rules engine, system, httpx and
// secheaders. Consumers that only need a CDN URL (web/siteassets,
// and through it the storage-sync CLI) were importing the whole
// composition root for four small functions, which made a file-sync
// binary compile the HTTP rules engine. Keeping these here lets
// siteassets depend on a leaf, not on the assembler, and keeps the
// package DAG flat.
//
// web/site re-uses these directly (it imports siteurl), so the
// dev/prod toggle and CDN composition stay defined exactly once.
package siteurl

import (
	"net/url"
	"strings"

	"github.com/hblabs-co/kit/core/banner"
	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/envcfg"
)

// lanFn is the variable seam tests swap to drive DevHostRewrite's
// LAN-dependent branches deterministically — banner.LAN() returns
// whatever the host machine happens to expose, which is neither
// stable across CI runners nor controllable from a test. Same
// pattern as render/og's encodePNG and fswatch's newFsnotifier.
var lanFn = banner.LAN

// IsDev reports whether the current process env triggers the dev
// wiring (hot-reload, on-disk FS, per-request *Site rebuild). Read
// from HB_ENV, "local" → true. Useful when main needs to decide
// something BEFORE site.New runs — typically banner extras or
// log-level overrides — so the timing matches when system.Boot
// prints the banner.
func IsDev() bool {
	return envcfg.String(constants.EnvHBEnv, "local") == "local"
}

// DevHostRewrite returns rawURL with a "localhost" or "127.0.0.1"
// host replaced by the machine's first non-loopback IPv4 address —
// only when the process is running in dev mode (EnvHBEnv == "local").
// Used so asset URLs that work for the laptop's own browser
// (http://localhost:9000/...) also resolve from a phone on the
// same wifi (http://192.168.x.y:9000/...). Scheme, port and path
// are preserved; only the host is swapped.
//
// Returns rawURL unchanged when:
//   - the process is not in dev mode
//   - rawURL is empty or unparseable
//   - the host is not localhost / 127.0.0.1
//   - no LAN address is available (offline laptop)
//
// Typical wiring from a service main():
//
//	cdn := siteurl.DevHostRewrite(envcfg.String(constants.EnvCDNBase, ""))
//
// Plus a pass over any catalog of media URLs loaded from YAML (the
// projects landing rewrites each Project.Logo the same way before
// passing the catalog to the template FuncMap).
func DevHostRewrite(rawURL string) string {
	if rawURL == "" || !IsDev() {
		return rawURL
	}
	lan := lanFn()
	if lan == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" {
		return rawURL
	}
	if port := u.Port(); port != "" {
		u.Host = lan + ":" + port
	} else {
		u.Host = lan
	}
	return u.String()
}

// resolvedCDNBase returns the public asset base URL that templates
// (BuildCDNURL) and CSP (ExtraImgSrc) reference, cascading through
// three env vars so a single-knob deploy ("just point ONE variable
// at the bucket") still produces working URLs instead of failing
// silently with relative paths and a 404 in the browser.
//
// Precedence:
//  1. HB_CDN_BASE — dedicated CDN URL. Preferred for browser
//     traffic: edge caches, lower latency.
//  2. HB_MINIO_PUBLIC_ENDPOINT — anonymous-read S3/MinIO origin.
//     Used when no CDN sits in front and the bucket is publicly
//     reachable (the common single-tier setup).
//  3. HB_MINIO_ENDPOINT — last-resort fallback for dev / single-host
//     boxes where the "private" endpoint IS the public one.
//
// Empty result → callers treat assets as same-origin: BuildCDNURL
// returns the path verbatim, ExtraImgSrc returns nil.
//
// Note the ordering intentionally differs from
// siteassets.publicAssetsEndpoint (MINIO_PUBLIC → CDN_BASE →
// MINIO_ENDPOINT): server-side fetches prefer the direct S3 origin
// to skip the CDN hop, browser-facing URLs prefer the CDN so the
// edge cache actually gets used.
func resolvedCDNBase() string {
	if e := envcfg.String(constants.EnvCDNBase, ""); e != "" {
		return e
	}
	if e := envcfg.String(constants.EnvMinIOPublicEndpoint, ""); e != "" {
		return e
	}
	return envcfg.String(constants.EnvMinIOEndpoint, "")
}

// BuildCDNURL composes the absolute URL for an asset path so
// templates can emit a single, environment-aware reference without
// having to know whether the bytes live in local MinIO, a remote
// CDN, or the service's own static tree.
//
// Behaviour:
//   - path is "" → returns "" (callers can branch on the result
//     with `{{ if (cdnURL X) }}`).
//   - path is already absolute (http:// or https://) → returns it
//     after DevHostRewrite so a localhost-bound URL still works
//     when accessed from another device on the LAN.
//   - no asset base resolves (HB_CDN_BASE, HB_MINIO_PUBLIC_ENDPOINT
//     and HB_MINIO_ENDPOINT all empty) → returns path verbatim
//     (treated as relative to the service origin — useful in
//     single-host setups that don't bother with a separate asset
//     host).
//   - otherwise → joins resolvedCDNBase + "/" + path with a single
//     slash, then runs the result through DevHostRewrite.
//
// Typical bindings in a service:
//
//	HB_CDN_BASE=http://localhost:9000/media/assets   	     # dev
//	HB_CDN_BASE=https://cdn.hblabs.co                        # prod
//
// And in templates:
//
//	<img src="{{ cdnURL .Logo }}">
//	<img src="{{ cdnURL "img/hblabs.png" }}">
//
// One helper, one mental model — services don't sprinkle
// scheme-aware string concat through their handlers.
func BuildCDNURL(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return DevHostRewrite(path)
	}
	base := resolvedCDNBase()
	if base == "" {
		return path
	}
	full := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
	return DevHostRewrite(full)
}

// ExtraImgSrc returns the additional CSP `img-src` origins this
// service needs to permit at boot. The asset host comes from
// resolvedCDNBase (same cascade as BuildCDNURL), routed through
// DevHostRewrite so the CSP source matches the exact URL emitted in
// templates (same localhost → LAN-IP swap in dev). No asset host
// resolved → nil, default CSP is left intact.
//
// Use case: in dev the asset host is `http://localhost:9000`
// (MinIO), which the default policy (`img-src 'self' data: https:`)
// blocks because plain `http:` is not in the source list. In prod,
// assets live behind the platform's HTTPS CDN, so `https:` already
// covers them and this returns nil.
func ExtraImgSrc() []string {
	if cdn := DevHostRewrite(resolvedCDNBase()); cdn != "" {
		return []string{cdn}
	}
	return nil
}
