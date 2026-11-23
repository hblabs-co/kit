package siteassets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/envcfg"
	"github.com/hblabs-co/kit/core/log"
)

// manifestHTTPClient is the client httpLoader uses. A modest timeout
// keeps a hung asset endpoint from stalling a template render — the
// ManifestCache TTL means this only fires on cold paths anyway.
var manifestHTTPClient = &http.Client{Timeout: 5 * time.Second}

// readAll is a var seam (repo convention for stdlib error paths that
// are virtually unreachable in tests — see CLAUDE.md / render/og).
// Tests swap it to exercise the body-read failure branch.
var readAll = io.ReadAll

// normalizeBaseURL turns an S3/MinIO endpoint into a scheme-qualified
// base URL. An endpoint that already carries a scheme
// (e.g. https://s3.hblabs.co) is used verbatim; a bare host:port
// (e.g. localhost:9000) gets http:// or https:// from useSSL. Any
// trailing slash is trimmed so callers can always join with "/".
func normalizeBaseURL(endpoint string, useSSL bool) string {
	if !strings.Contains(endpoint, "://") {
		scheme := "http"
		if useSSL {
			scheme = "https"
		}
		endpoint = scheme + "://" + endpoint
	}
	return strings.TrimRight(endpoint, "/")
}

// httpLoader returns a Loader that fetches each per-site
// `_manifest.json` over plain HTTP — no credentials. The manifest
// lives under the anonymous-read `assets/` prefix, so a consumer that
// only renders public asset URLs never needs MinIO keys. Putting the secret nowhere
// is strictly better than scoping it.
//
// Status handling mirrors the old minio-go path: 404 means "no
// manifest yet" → (nil, nil) so the cache treats the site as absent
// and template helpers fall back to logical names; any other non-200
// is a real error worth surfacing in logs (e.g. a 403 means the
// prefix lost its public policy — you want to know, not silently
// serve broken images).
func httpLoader(baseURL, bucket string) Loader {
	return func(ctx context.Context, orgID, siteID string) ([]byte, error) {
		u := baseURL + "/" + bucket + "/assets/" + orgID + "/" + siteID + "/" + ManifestBasename
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := manifestHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusOK:
			return readAll(resp.Body)
		case http.StatusNotFound:
			return nil, nil
		default:
			return nil, fmt.Errorf("manifest %s: unexpected status %d", u, resp.StatusCode)
		}
	}
}

// NewHTTPManifestCache builds a ManifestCache backed by an anonymous
// HTTP loader against a public S3/MinIO endpoint. This is the
// drop-in a service wires when it only needs to resolve hashed asset
// URLs (no credentials, read-only, public objects).
//
// Returns nil when endpoint or bucket is empty so the caller can wire
// it unconditionally and let the nil-safe ManifestCache degrade to
// logical names (a dev running without an asset host still gets
// working, just-not-immutably-cached, URLs).
func NewHTTPManifestCache(endpoint, bucket string, useSSL bool, ttl time.Duration) *ManifestCache {
	if endpoint == "" || bucket == "" {
		return nil
	}
	return NewManifestCache(httpLoader(normalizeBaseURL(endpoint, useSSL), bucket), ttl)
}

// publicAssetsEndpoint resolves the base a public, anonymous-read
// consumer fetches `_manifest.json` from. Precedence:
//
//  1. HB_MINIO_PUBLIC_ENDPOINT — explicit public read origin.
//  2. HB_CDN_BASE — the base the browser already builds asset URLs
//     from; using it makes the server resolve the manifest from the
//     SAME origin the emitted <img src> points at (this alone fixes
//     the prod 404 — CDN base was correct, the loader was wrongly
//     using the private MinIO endpoint).
//  3. HB_MINIO_ENDPOINT — legacy/dev fallback (the private endpoint;
//     only right when it is also publicly reachable, e.g. a
//     single-host dev box).
func publicAssetsEndpoint() string {
	if e := envcfg.String(constants.EnvMinIOPublicEndpoint, ""); e != "" {
		return e
	}
	if e := envcfg.String(constants.EnvCDNBase, ""); e != "" {
		return e
	}
	return envcfg.String(constants.EnvMinIOEndpoint, "")
}

// NewAssetsCache builds the ManifestCache a public asset consumer
// wires. It resolves the
// per-site manifest over anonymous HTTP from the PUBLIC endpoint
// (see publicAssetsEndpoint) — never the private/credentialed MinIO
// endpoint, which in prod is usually cluster-internal and
// unreachable for a public read. Nil (and a log line) when nothing
// resolves a base/bucket, so the caller can wire it unconditionally
// and degrade to logical names.
func NewAssetsCache(ttl time.Duration) *ManifestCache {
	cache := NewHTTPManifestCache(
		publicAssetsEndpoint(),
		envcfg.String(constants.EnvMinIOBucket, ""),
		envcfg.Bool(constants.EnvMinIOUseSSL, false),
		ttl,
	)
	if cache == nil {
		log.Info("asset manifest cache: disabled (no public endpoint / bucket configured)")
	}

	return cache
}
