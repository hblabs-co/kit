// Package constants holds the names shared by more than one kit
// package or by every app built on the kit: generic environment
// variable keys (environment.go), HTTP header and cookie names
// (headers.go) and well-known HTTP paths (paths.go).
//
// What does NOT belong here:
//
//   - keys read by a single kit package: they live in that package
//     (mongo.EnvURL, natsx.EnvURL, ...);
//   - anything that names one product: service names, Mongo
//     collection names and product-specific env keys live in the app
//     that owns them.
//
// Keeping the package this small is deliberate: almost every kit
// package imports it, so everything in it is a dependency of
// everything.
package constants

// Generic environment variable keys.
//
// All Go identifiers carry the `Env` prefix so they're immediately
// recognisable as environment-variable keys at the call site, and all
// values carry the `HB_` prefix, which namespaces our variables apart
// from the ones the runtime or cloud provider injects.
//
// The EnvMinIO* keys stay here (instead of in objectstore) because
// two modules read them: objectstore and web (siteurl, siteassets).
const (
	// EnvHBEnv selects the runtime profile. "local" means a developer
	// laptop (text logs, hot reload, dev wrappers); anything else
	// (staging, prod, ci) flips defaults toward production behavior.
	EnvHBEnv = "HB_ENV"

	// EnvLogFormat overrides the slog wire format independently of
	// EnvHBEnv. "text" or "json"; unknown values fall back to text.
	// Useful when a prod debug session wants human-readable output
	// in `kubectl logs`.
	EnvLogFormat = "HB_LOG_FORMAT"

	// EnvLogLevel sets the slog level threshold: "debug" | "info" |
	// "warn" | "error" (case-insensitive; unknown → info).
	EnvLogLevel = "HB_LOG_LEVEL"

	// EnvLogQuietPaths is a comma-separated list of URL patterns that
	// the access-log middleware demotes to debug level (kubelet
	// probes, embedded asset fetches). Trailing "*" = prefix match.
	EnvLogQuietPaths = "HB_LOG_QUIET_PATHS"

	// EnvPort is the listen port of the service's HTTP server, as
	// a bare integer ("8080", not ":8080"). The full listen address
	// is built from this — services prepend ":" via fmt.Sprintf to
	// match net.Listen syntax. Aligned with the standard PORT env
	// convention (k8s, Heroku, fly.io) so platform tooling can
	// inject a port without service-specific knowledge.
	EnvPort = "HB_PORT"

	// EnvCanonicalHost is the public origin used by render in
	// canonical links, sitemaps, OG cards, etc. Include scheme, no
	// trailing slash. Example: "https://hblabs.co".
	EnvCanonicalHost = "HB_CANONICAL_HOST"

	// EnvCookieDomain scopes the platform's first-party cookies
	// (device-id "hbl_did", and the session cookie when wired) to a
	// registrable domain so every subdomain shares one value. Set to
	// ".example.com" in prod → app./console./analytics. all see the
	// same hbl_did. Leave EMPTY in dev/localhost and for deployments
	// on a customer's own domain: an empty Domain makes the cookie
	// host-only, which is the correct (and only browser-accepted)
	// behavior there. Leading dot optional in modern browsers; we
	// keep it for readability.
	EnvCookieDomain = "HB_COOKIE_DOMAIN"

	// EnvCDNBase is the public CDN URL the platform prepends to
	// relative asset paths via siteurl.BuildCDNURL. Typical values:
	//   - dev:  "http://localhost:9000/<bucket>/assets"
	//   - prod: "https://cdn.hblabs.co"
	// Include scheme, no trailing slash. Empty → BuildCDNURL falls
	// back to EnvMinIOPublicEndpoint and then EnvMinIOEndpoint (same
	// cascade as ExtraImgSrc and siteassets.publicAssetsEndpoint),
	// so a single-knob deploy that only sets MINIO_PUBLIC still
	// emits absolute URLs in the HTML — no silent 404s from relative
	// paths. All three empty → path verbatim (same-origin).
	//
	// The value also works inside YAML via "${HB_CDN_BASE}/..."
	// env-substitution at load time, but render-time composition
	// (`{{ cdnURL .Logo }}`) is the canonical path because it picks
	// up the dev host-rewrite for LAN browsing.
	EnvCDNBase = "HB_CDN_BASE"

	// EnvDefaultLang is the language slug picked when the visitor has
	// no cookie or Accept-Language hint. Must be in the service's
	// SupportedLangs list.
	EnvDefaultLang = "HB_DEFAULT_LANG"

	// EnvSecurityContact is the email surfaced in
	// /.well-known/security.txt (RFC 9116). Empty → render warns at
	// boot and serves a placeholder.
	EnvSecurityContact = "HB_SECURITY_CONTACT"

	// EnvBuildTime is the build-stamp the binary logs once at boot.
	// Invaluable when telling whether a rollout "took" or k8s is
	// still serving the old image. Set by the build pipeline (e.g.
	// `-ldflags "-X main.buildTime=…"` or as a real env var).
	EnvBuildTime = "HB_BUILD_TIME"

	// EnvBuildVersion is the human-readable version string the binary
	// logs once at boot and that services may surface in their UI
	// footer. Convention is MAJOR.MINOR.BUILD where MAJOR.MINOR live
	// in a hand-edited VERSION file and BUILD is the monotonic git
	// commit count (`git rev-list --count HEAD`) — so the number
	// climbs on every commit without any pipeline involvement.
	// Same injection pattern as EnvBuildTime: ldflags into a Go
	// var, then the binary copies it into this env var before
	// calling system.Boot so the banner picks it up.
	EnvBuildVersion = "HB_BUILD_VERSION"

	// EnvShutdownTimeout overrides system.RunForever's default 25s
	// shutdown budget on SIGTERM (drain delay + module shutdown).
	// Accepts a Go duration string ("25s", "1m").
	// Stays under k8s' default 30s terminationGracePeriodSeconds —
	// bump both together when a service genuinely needs more.
	EnvShutdownTimeout = "HB_SHUTDOWN_TIMEOUT"

	// EnvShutdownDelay is how long system.RunForever keeps serving
	// after SIGTERM, with readiness already answering 503, before it
	// stops any module. Go duration ("5s"); default 5s, "0" disables.
	// It is spent inside the EnvShutdownTimeout budget, so module
	// shutdown gets the remainder (25s - 5s = 20s by default).
	EnvShutdownDelay = "HB_SHUTDOWN_DELAY"

	// EnvMinIOEndpoint is the PRIVATE / internal endpoint the
	// credentialed S3 client connects to for reads AND writes
	// (storage-sync uploads, future server-side object IO). In
	// prod this is typically the in-cluster address
	// (e.g. "minio.minio.svc:9000") reachable only from inside.
	// Local dev points at "localhost:9000" — the docker-compose
	// stack exposes 9000 on all interfaces. Scheme is selected
	// separately via EnvMinIOUseSSL.
	//
	// This is NOT what anonymous public asset/manifest reads use —
	// that is EnvMinIOPublicEndpoint (a service that only renders
	// public asset URLs must never touch the private endpoint;
	// it is often unreachable from where the browser is anyway).
	EnvMinIOEndpoint = "HB_MINIO_ENDPOINT"

	// EnvMinIOPublicEndpoint is the PUBLIC, anonymous-read base for
	// the assets/ prefix — the same origin the browser fetches asset
	// URLs from (e.g. "s3.hblabs.co"). siteassets.NewAssetsCache
	// resolves the per-site `_manifest.json` from here (preferring
	// it over EnvCDNBase to skip the CDN hop on a server-internal
	// fetch); siteurl.BuildCDNURL falls back to it when EnvCDNBase is
	// empty (so it can serve as the single asset knob in deployments
	// without a separate CDN). Cascade: server-side
	// MINIO_PUBLIC → CDN_BASE → MINIO_ENDPOINT; browser-side
	// CDN_BASE → MINIO_PUBLIC → MINIO_ENDPOINT — orderings differ on
	// purpose (see siteurl.resolvedCDNBase and
	// siteassets.publicAssetsEndpoint). Splitting this out from
	// EnvMinIOEndpoint also fixes the prod footgun where a public
	// consumer pointed at the private endpoint and silently degraded
	// to logical (un-hashed) names → 404.
	EnvMinIOPublicEndpoint = "HB_MINIO_PUBLIC_ENDPOINT"

	// EnvMinIOAccessKey is the S3 access-key-id the storage client
	// authenticates with. Locally this matches MINIO_ROOT_USER from
	// docker-compose; in prod it's an issued IAM credential scoped to
	// the bucket(s) the service actually needs.
	EnvMinIOAccessKey = "HB_MINIO_ACCESS_KEY"

	// EnvMinIOSecretKey is the S3 secret-access-key paired with
	// EnvMinIOAccessKey. Same provisioning rules as EnvMinIOAccessKey.
	EnvMinIOSecretKey = "HB_MINIO_SECRET_KEY"

	// EnvMinIOBucket is the default bucket the storage tooling targets
	// when no --bucket flag is supplied. The platform's convention is
	// one bucket per environment; per-org scoping
	// happens via key prefix, not via bucket-per-tenant.
	EnvMinIOBucket = "HB_MINIO_BUCKET"

	// EnvMinIOUseSSL selects the URL scheme used to talk to MinIO / S3.
	// "true" → https://, anything else → http://. Local docker-compose
	// runs plain http; remote endpoints almost always require TLS.
	EnvMinIOUseSSL = "HB_MINIO_USE_SSL"

	// EnvMinIORegion is the S3 region label sent in signed requests.
	// MinIO ignores it but still requires a value; AWS-style backends
	// pick the routing target from it. Empty → "us-east-1" (the MinIO
	// default), which works for both flavors.
	EnvMinIORegion = "HB_MINIO_REGION"

	// EnvInternalServiceToken is the shared service-to-service token that
	// gates internal (non-public) HTTP routes across HB Labs services:
	// a caller presents it in the X-HB-Service-Token header and the
	// receiving service compares it constant-time against this value (see
	// httpx.RequireServiceToken). One shared secret for all internal
	// service auth in v1, a placeholder until real service identity
	// (mTLS/SPIFFE).
	// SECRET: inject it from a secret store, never a literal in .env
	// or YAML. Empty closes every gated
	// route (rejects all requests), so it must be set for any internal
	// caller or tooling to get through; public routes need no token.
	EnvInternalServiceToken = "HB_INTERNAL_SERVICE_TOKEN"

	// EnvKitDir points at a local checkout of github.com/hblabs-co/kit.
	// Dev-only: web/uikit.DevFS and web/site's rules code-watch resolve
	// the kit's on-disk sources against it, so editing the shared
	// design-system assets hot-reloads in an app that consumes the kit
	// as a module. Unset in production (the embedded copies are used).
	EnvKitDir = "HB_KIT_DIR"
)
