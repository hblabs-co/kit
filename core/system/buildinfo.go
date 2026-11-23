package system

import (
	"os"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// buildVersion and buildTime are package-level ldflags injection
// targets. Services that want their binary stamped at build time
// hand the linker:
//
//	go build -ldflags="\
//	  -X github.com/hblabs-co/kit/core/system.buildVersion=v1.0.487 \
//	  -X github.com/hblabs-co/kit/core/system.buildTime=2026-05-25T18:23:00Z" .
//
// Boot promotes whichever of these is non-empty into the
// HB_BUILD_VERSION / HB_BUILD_TIME env vars before resolving the
// build info, so the rest of the framework (banner log line,
// ReadBuildInfo helper, template helpers reading via env) sees a
// single source. Empty defaults are intentional — when ldflags
// didn't inject AND env vars aren't set, the boot banner stays
// quiet and the footer helper returns "" instead of a stale
// "dev" stamp.
//
// Services that prefer Boot options or env vars don't need to
// touch these — system.Build(version, time) and the HB_BUILD_*
// env vars work the same way they always did.
var (
	buildVersion = ""
	buildTime    = ""
)

// BuildInfoData carries the three forms of the build stamp a
// typical service needs at render time:
//
//   - Version    — raw HB_BUILD_VERSION ("v1.0.487").
//   - Time       — raw HB_BUILD_TIME in RFC 3339
//     ("2026-05-25T16:57:32Z"). Suitable for the
//     `datetime` attribute of an HTML <time> tag
//     so client-side JS can localise the stamp
//     with `new Date(datetime).toLocaleString()`.
//   - TimePretty — "YYYY-MM-DD HH:MM UTC" — server-rendered
//     fallback shown to no-JS visitors and to
//     crawlers. A parse failure falls back to the
//     raw env string so a hand-edited HB_BUILD_TIME
//     still surfaces instead of vanishing silently.
//
// Use Version + Time directly in HTML templates that wrap the
// time in <time datetime="...">; use BuildInfo() for the combined
// "version · pretty-UTC" string in boot logs and non-HTML contexts.
type BuildInfoData struct {
	Version    string
	Time       string
	TimePretty string
}

// ReadBuildInfo snapshots the build stamp from the HB_BUILD_*
// env vars (which system.Boot promotes from the BuildVersion /
// BuildTime options or any pre-existing env). Cheap enough to
// call per request, but typically captured once at startup and
// reused via closure so the env reads + RFC-3339 parse don't
// re-run on every render.
func ReadBuildInfo() BuildInfoData {
	v := strings.TrimSpace(os.Getenv(constants.EnvBuildVersion))
	t := strings.TrimSpace(os.Getenv(constants.EnvBuildTime))
	pretty := t
	if t != "" {
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			pretty = ts.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	return BuildInfoData{Version: v, Time: t, TimePretty: pretty}
}

// BuildInfo returns the combined "<version> · <pretty time>"
// string for boot logs and any non-HTML context that wants both
// pieces at once. HTML templates should call ReadBuildInfo()
// instead so the time can be wrapped in <time> and localised by
// JS. Returns "" when both halves are empty so callers can
// branch without painting a lonely separator.
func BuildInfo() string {
	bi := ReadBuildInfo()
	switch {
	case bi.Version != "" && bi.TimePretty != "":
		return bi.Version + " · " + bi.TimePretty
	case bi.Version != "":
		return bi.Version
	default:
		return bi.TimePretty
	}
}
