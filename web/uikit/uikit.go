// Package uikit is the shared visual layer every HB Labs
// service composes from: design tokens (colors, type, spacing,
// motion), reset, and the canonical component CSS (buttons, badges,
// cards, glass surfaces, …).
//
// Why a Go package and not a checked-in folder under each service:
// services compile their HTML surface from a //go:embed FS rooted at
// their own directory, so they can't reach files outside it. Wrapping
// the assets in a Go package gives every service a single import to
// pull the same CSS — change a token here, every service rebuilds
// against the new bytes on next deploy without per-service copy.
//
// Wiring from a service:
//
//	import "github.com/hblabs-co/kit/web/uikit"
//
//	handler, modules, _, err := site.New(site.Options{
//	    SiteFS:      siteFS,
//	    ConfigPath:  "config/config.yaml",
//	    ExtraStatic: map[string]fs.FS{"uikit": uikit.FS},
//	})
//
// In templates the layout pulls the shared sheets via the existing
// asset helper — same fingerprinting, same cache-immutable headers
// as the service's own static:
//
//	<link rel="stylesheet" href="{{ asset "uikit/css/tokens.css" }}">
//	<link rel="stylesheet" href="{{ asset "uikit/css/components.css" }}">
//
// # Source of truth
//
// The CSS files under assets/css/ are ported from the design-system
// reference at design/project/design-system/{tokens,components}.css
// (the Claude Design handoff). When the design system evolves there,
// mirror the change here in the same commit so the running services
// don't drift from the reference.
//
// # Versioning
//
// uikit ships immutable bytes; consumers fingerprint them via the
// render framework's asset pipeline. Token renames are breaking
// changes — bump the comment header in the affected CSS file and
// announce in the team channel before merging, since every consuming
// service may need a template tweak.
package uikit

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hblabs-co/kit/core/constants"
)

// devSourcePath is the location of the uikit assets inside a kit
// checkout. DevFS resolves it against the directory named by
// constants.EnvKitDir (HB_KIT_DIR), or against the current cwd when
// the variable is unset, so a CSS edit in a local kit checkout
// re-renders on the next page request without a `go run` restart.
const devSourcePath = "web/uikit/assets"

// devSourceDir is devSourcePath resolved against HB_KIT_DIR.
func devSourceDir() string {
	return filepath.Join(os.Getenv(constants.EnvKitDir), filepath.FromSlash(devSourcePath))
}

//go:embed assets
var embedded embed.FS

// FS is the embedded shared-asset tree, rooted at the assets/
// directory so URL paths look like "css/tokens.css" rather than
// "assets/css/tokens.css". A consumer mounts FS under a service-
// chosen prefix (typically "uikit") via render.Options.ExtraStatic;
// the resulting URL is /static/uikit/css/tokens.css.
//
// fs.Sub strips the leading "assets/" segment. The error path is
// effectively unreachable because the embed.FS we built above always
// contains "assets" — embed-time paths are validated by the compiler
// — but we surface a panic over a silent partial-init so a future
// refactor that breaks the layout fails loudly at import time, not
// 500s on first request.
var FS = mustSub(embedded, "assets")

// mustSub is the unwrap of fs.Sub used by FS. Pulled out as a named
// function (rather than an inline IIFE in the var line) so the panic
// has a stable stack frame to grep for and the test can swap the
// inputs without invoking package init.
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("uikit: fs.Sub(assets) failed: " + err.Error())
	}
	return sub
}

// DevFS returns the on-disk fs.FS for the uikit assets — use it in
// dev mode so a CSS edit is reflected on the next request without
// rebuilding the binary:
//
//	siteOpts := site.Options{...}
//	if siteurl.IsDev() {
//	    siteOpts.ExtraStatic = map[string]fs.FS{"uikit": uikit.DevFS()}
//	} else {
//	    siteOpts.ExtraStatic = map[string]fs.FS{"uikit": uikit.FS}
//	}
//
// Resolves via os.DirFS at devSourceDir(): set HB_KIT_DIR to a local
// checkout of github.com/hblabs-co/kit to edit the shared assets
// live. Falls back to the embedded FS when the on-disk path isn't
// reachable (the normal case for an app that consumes a tagged kit
// version), so assets always work, just without hot-reload.
//
// Note: the dev wrapper's fsnotify watcher only observes the
// service's WatchRoot, so editing a uikit file does NOT trigger an
// SSE-driven browser reload — the next page request picks up the
// new bytes, but you have to refresh manually. To get auto-reload,
// extend dev.Options with extra watch roots; that's a framework
// change, not a service one.
func DevFS() fs.FS {
	disk := os.DirFS(devSourceDir())
	if _, err := fs.Stat(disk, "css/tokens.css"); err != nil {
		return FS
	}
	return disk
}
