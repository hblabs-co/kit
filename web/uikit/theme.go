package uikit

import (
	_ "embed"
	"html/template"
)

// themeBootJS is the no-FOUC theme bootstrap, kept as a real,
// lintable .js file under assets/js/embedded/ and embedded at build
// time rather than living as a Go raw-string. It is the read side of
// the dark-mode switch (sets <html data-theme> from localStorage
// 'hb-theme' > prefers-color-scheme > light); the write side is its
// sibling assets/js/public/theme.js.
//
// It MUST run in <head> before any stylesheet, so ThemeBoot emits it
// INLINE — an external/deferred <script src> would flash the light
// theme first. The assets/js/ split encodes intent: public/ is
// fetched by URL, embedded/ is compiled into HTML. The render
// ExtraStatic mount still exposes the whole tree, so this file is
// *also* reachable at /static/uikit/js/embedded/theme-boot.js — an
// incidental, unreferenced copy (the directory name says "don't fetch
// me"). Harmless: the boot logic is already fully public inside every
// page's <head>, so an extra unlinked URL leaks nothing.
//
//go:embed assets/js/embedded/theme-boot.js
var themeBootJS string

// themeBootHTML wraps the embedded JS in a <script> once at init so
// every ThemeBoot call is a cheap typed conversion, not string
// concatenation per request.
var themeBootHTML = template.HTML("<script>\n" + themeBootJS + "</script>")

// ThemeBoot returns the inline no-FOUC theme bootstrap as trusted
// HTML for a service layout's <head>. Drop it in before the
// stylesheets, then pair it with the uikit/js/public/theme.js handler
// and the .hb-theme-toggle button so a service gets the full
// dark-mode switch with no per-service logic:
//
//	<head>
//	  {{ themeBoot }}
//	  <link rel="stylesheet" href="{{ asset "uikit/css/tokens.css" }}">
//	  ...
//	  <script src="{{ asset "uikit/js/public/theme.js" }}" defer></script>
//	</head>
//
// The content is a build-time-embedded constant the package author
// controls (no user input), so template.HTML is safe here.
func ThemeBoot() template.HTML { return themeBootHTML }
