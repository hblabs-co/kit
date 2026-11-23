package site

import (
	"html/template"
	"io/fs"

	"github.com/hblabs-co/kit/core/system"
	"github.com/hblabs-co/kit/web/siteurl"
	"github.com/hblabs-co/kit/web/uikit"
)

// The helpers in this file are the glue every landing service used to
// copy into its own funcmap.go: pick the uikit asset tree, merge the
// shared FuncMaps, expose the build stamp to templates.

// UIKitFS returns the shared design-system asset tree to mount under
// Options.ExtraStatic["uikit"]: the on-disk tree in dev (so an edit to
// a local kit checkout shows up on the next request, see
// uikit.DevFS), the embedded immutable copy otherwise.
func UIKitFS() fs.FS {
	if siteurl.IsDev() {
		return uikit.DevFS()
	}
	return uikit.FS
}

// MergeFuncMaps returns a new FuncMap holding every entry of maps.
// Later maps win on a name clash, so a service passes the shared maps
// first and its own overrides last. Nil maps are skipped; the inputs
// are never mutated.
func MergeFuncMaps(maps ...template.FuncMap) template.FuncMap {
	n := 0
	for _, m := range maps {
		n += len(m)
	}
	out := make(template.FuncMap, n)
	for _, m := range maps {
		for name, fn := range m {
			out[name] = fn
		}
	}
	return out
}

// BuildInfoFuncMap exposes the binary's build stamp
// (system.ReadBuildInfo) to templates, typically for a footer:
//
//	buildVersion    → "v1.0.487"
//	buildTime       → "2026-05-25T18:23:00Z"
//	buildTimePretty → the same instant formatted for humans
//
// The stamp is read once, when the map is built; all three functions
// return "" for an unstamped (go run) binary.
func BuildInfoFuncMap() template.FuncMap {
	bi := system.ReadBuildInfo()
	return template.FuncMap{
		"buildVersion":    func() string { return bi.Version },
		"buildTime":       func() string { return bi.Time },
		"buildTimePretty": func() string { return bi.TimePretty },
	}
}
