# kit

[![ci](https://github.com/hblabs-co/kit/actions/workflows/ci.yml/badge.svg)](https://github.com/hblabs-co/kit/actions/workflows/ci.yml)
![core coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-core.json)
![web coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-web.json)
![natsx coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-natsx.json)
![mongo coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-mongo.json)
![objectstore coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-objectstore.json)
![observability coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-observability.json)
![mail coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/hblabs-co/kit/badges/coverage-mail.json)

The shared Go base of HB Labs: one repository, one Go module per area, each
versioned on its own. Apps import tagged versions instead of copying the code.

## Modules

| Module (import path prefix) | What it is | External dependencies |
|---|---|---|
| `github.com/hblabs-co/kit/core` | The foundation every service uses: `log` (slog wrapper), `system` (boot, lifecycle, HTTP server, build info), `envcfg` (typed env readers), `httpx` (access log, request id, client IP, SSE, JSON responses, service token), `secheaders`, `clientmeta`, `ids` (public ids, ULID, slugs), `jwt`, `periodic` (tickers, batcher), `ratelimit` (per-key token bucket with bans and a pluggable ban store), `fswatch`, `banner`, `datasources` (key/value interface + memory backend), `constants` (generic env keys, header/cookie names, probe paths) | godotenv, go-nanoid, ulid, fsnotify, golang-jwt, x/time |
| `github.com/hblabs-co/kit/web` | The landing framework: `site` (composition root + landing helpers), `render` (file-based pages, i18n, SEO files, OG images, asset fingerprinting, dev hot reload), `uikit` (shared design system assets + icons), `siteurl`, `siteassets`, `siteinfo`, `langpick` (Accept-Language negotiation incl. Chinese scripts, endonyms), `ghrelease` (cached GitHub latest-release resolver + download redirect), `csrf` (double-submit cookie), `contactform` (mail-forwarding contact form with CSRF, honeypot, rate limit), and the request rules engine under `rules/{engine,rules,dev}` | expr, otel, x/image, yaml |
| `github.com/hblabs-co/kit/natsx` | NATS / JetStream bus: connect, ensure streams, durable subscribe with backoff, publish with message id, core-NATS fan-out (`PublishCore`, `SubscribeCore`) | nats.go |
| `github.com/hblabs-co/kit/mongo` | MongoDB wrapper (`Open`, `Module`, key/value datasource), duplicate-key helpers, declarative index manifests (`IndexSpec`, `Reconcile`, `FindSpec`, `ValidateManifest`) | mongo-driver v2 |
| `github.com/hblabs-co/kit/objectstore` | MinIO / S3 client wrapper | minio-go |
| `github.com/hblabs-co/kit/observability` | Prometheus metrics, OTLP tracing, `Healthz` / `Liveness` handlers | prometheus, otel |
| `github.com/hblabs-co/kit/mail` | `scaleway`: Scaleway Transactional Email client (send, typed API errors, retryability, extra headers) and `Notifier` (fixed sender → fixed recipient, plain text, Reply-To). The client reads no environment; `NotifierFromEnv` reads `HB_SCALEWAY_SECRET_KEY`, `HB_SCALEWAY_PROJECT_ID`, `HB_SCALEWAY_REGION`, `HB_MAIL_FROM`, `HB_MAIL_FROM_NAME` | none |

Every module except `mail` depends on `core`; nothing else depends on anything
else, so an app that only needs NATS does not compile Mongo or the web
framework.

What is deliberately **not** here: anything that names one product. Service
names, Mongo collection names, a product's env keys, data models, auth flows
tied to a data model. Those stay in the app.

## Using a module

```sh
go get github.com/hblabs-co/kit/core@v0.3.0
go get github.com/hblabs-co/kit/web@v0.3.0
go mod tidy
```

```go
import (
	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
	"github.com/hblabs-co/kit/web/site"
)
```

The build-info ldflags target is `github.com/hblabs-co/kit/core/system`:

```sh
go build -ldflags "-X github.com/hblabs-co/kit/core/system.buildVersion=v1.2.3 \
                   -X github.com/hblabs-co/kit/core/system.buildTime=2026-11-21T10:00:00Z" .
```

The modules are public: `go get`, CI and Docker builds need no credentials.

### Working on the kit and an app at the same time

Do not commit a `replace` or a `go.work` that points at a sibling checkout.
Use an uncommitted workspace instead:

```sh
cd ~/code/my-app
go work init . ../kit/core ../kit/web      # go.work stays out of git
export HB_KIT_DIR=../kit                   # uikit assets + rules catalog hot-reload from the checkout
go run .
```

`HB_KIT_DIR` (`constants.EnvKitDir`) is dev-only: with it, `uikit.DevFS` and
the rules code-watch read the kit's files from disk; without it they use the
embedded copies.

## Versions and tags

- Each module has its own tags, prefixed with its directory:
  `core/v0.2.0`, `web/v0.2.0`, `natsx/v0.3.0`, … (Go requires that form for a
  module in a subdirectory).
- Semantic versioning. While the major version is 0 the API may still change:
  a **minor** bump (`v0.2.x` → `v0.3.0`) may break, a **patch** bump does not.
  From `v1` on, breaking changes need a `/v2` module path.
- A tag is never moved or reused. A bad release is fixed by a new patch
  version (and a `retract` line if it must not be used).

Current versions:

| Module | Version |
|---|---|
| `core` | `v0.3.0` |
| `web` | `v0.3.0` |
| `natsx` | `v0.3.0` |
| `mongo` | `v0.2.0` |
| `objectstore` | `v0.2.0` |
| `observability` | `v0.3.0` |
| `mail` | `v0.3.0` |

### Releasing

```sh
scripts/check.sh                       # gofmt, go mod tidy -diff, vet, race tests, every module
git tag -a web/v0.2.1 -m "web v0.2.1: <what changed>"
git push origin main web/v0.2.1
```

If a release of `core` is needed by another module, tag `core` first, then
raise `require github.com/hblabs-co/kit/core vX` in that module's `go.mod`,
run `go mod tidy` there and tag it too. Inside this repo the modules always
build against each other's working tree (the `replace ../core` lines, which Go
ignores when the module is consumed as a dependency).

## Development

```sh
scripts/check.sh            # everything CI runs
scripts/check.sh web        # one module
cd core && go test ./log/   # the usual
```

- `go.work` at the root lists all modules for editor / gopls support.
- CI (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy -diff`, `go vet`
  and `go test -race` for each module.
- Add tests with the code. New or moved code is tested with the race detector.

## License

MIT, see [LICENSE](LICENSE).
