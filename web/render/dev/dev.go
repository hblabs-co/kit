// Package dev wires hot reload onto a render-based service for local
// development. It does two things:
//
//  1. On each request it builds a fresh render.Site from disk so
//     edits to pages/, partials/, locales/, and static/ show up
//     without restarting the process. Template parsing happens once
//     per request, which is fine for one developer hitting the page
//     a few times per minute and is the simplest way to avoid an
//     atomic-pointer reload mechanism inside render itself.
//  2. It mounts /__reload (SSE) and /__reload.js so the browser
//     auto-reloads when fswatch fires. The {{ devScript }} helper in
//     web/render emits the matching <script> tag from layouts.
//
// Never run this in production:
//   - cost of per-request site build is fine for one user, ruinous
//     for real traffic;
//   - SSE endpoint has no auth and no rate limit;
//   - the reload script forces a full page refresh on every save.
//
// Typical usage from main():
//
//	if env == "local" {
//	    opts, _ := render.Init(os.DirFS(watchRoot)).
//	        LoadConfig("config/config.yaml").
//	        WithDev(true).
//	        BuildOptions()
//	    devSrv, _ := dev.New(dev.Options{
//	        RenderOpts: opts,
//	        WatchRoot:  watchRoot,
//	    })
//	    handler = devSrv.Handler()
//	    // devSrv satisfies system.Module — drop it into RunForever
//	    // alongside the HTTP server module.
//	    system.RunForever(ctx,
//	        system.NewServer(serverOpts),
//	        devSrv,
//	    )
//	}
package dev

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/httpx"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/web/render"
)

// defaultDebounceWindow collapses bursty editor saves into a single
// reload. 100 ms is good for VS Code, JetBrains and vim — it covers
// the 3–5 writes most editors emit per ⌘S without making a real edit
// feel laggy.
const defaultDebounceWindow = 100 * time.Millisecond

// Options configures a dev Server. Required fields fail at New() if
// missing; everything else gets a default appropriate for laptop dev.
type Options struct {
	// RenderOpts is invoked once per HTTP request to produce a
	// fresh render.Options. Returning the result of an in-line
	// `render.Init(...).LoadConfig(...).BuildOptions()` is what
	// makes config.yaml hot-reload work — every request re-parses
	// the YAML from disk so edits take effect on the next refresh.
	// For static dev setups, the closure can return the same value
	// every time. Required.
	RenderOpts func() (render.Options, error)

	// WatchRoot is the directory whose subtree the dev watcher
	// monitors. A change anywhere under here triggers an SSE broadcast
	// and the browser reloads. Typically the service's source directory.
	WatchRoot string

	// DebounceWindow collapses bursty editor saves into a single
	// reload. nil → 100 ms (good for VS Code, JetBrains, vim).
	DebounceWindow time.Duration
}

// Server is the dev wrapper. Build with New, attach with Handler(),
// drive lifecycle with Register / Shutdown via system.RunForever.
type Server struct {
	opts Options
	hr   *HotReload
}

// New validates the options and stages the watcher (no goroutines
// spawned yet — Register starts the loop). Calls RenderOpts once
// up front so a misconfigured FS or a malformed config.yaml fails
// at boot, not on the first request.
func New(opts Options) (*Server, error) {
	if opts.WatchRoot == "" {
		return nil, errors.New("dev: Options.WatchRoot is required")
	}
	if opts.RenderOpts == nil {
		return nil, errors.New("dev: Options.RenderOpts is required")
	}
	// Force Dev=true on the resolved options so the layout always
	// emits the hot-reload script — caller doesn't have to
	// remember to set it inside their closure. Wrap once here so
	// the rest of the request path stays straight-line.
	userFn := opts.RenderOpts
	opts.RenderOpts = func() (render.Options, error) {
		o, err := userFn()
		if err != nil {
			return render.Options{}, err
		}
		o.Dev = true
		return o, nil
	}
	// Probe once at boot so a missing FS or bad YAML surfaces in
	// main's error log instead of as a 500 on the first browser
	// hit.
	initial, err := opts.RenderOpts()
	if err != nil {
		return nil, fmt.Errorf("dev: initial RenderOpts: %w", err)
	}
	if initial.FS == nil {
		return nil, errors.New("dev: RenderOpts must set FS (use os.DirFS in dev)")
	}

	debounce := opts.DebounceWindow
	if debounce <= 0 {
		debounce = defaultDebounceWindow
	}

	hub := httpx.NewSSEHub()
	hr, err := NewHotReloader(debounce, hub)
	if err != nil {
		return nil, err
	}
	if err := hr.WatchTree(opts.WatchRoot); err != nil {
		_ = hr.Close()
		return nil, err
	}

	return &Server{opts: opts, hr: hr}, nil
}

// Register starts the file-watching goroutine and returns
// immediately, satisfying the system.Module contract so a
// dev.Server drops into system.RunForever alongside the HTTP
// server module:
//
//	system.RunForever(ctx,
//	    system.NewServer(serverOpts),
//	    devServer, // last → first to Shutdown
//	)
//
// ctx is the long-lived service context; the watcher exits when
// ctx is cancelled. Always returns nil — fsnotify's underlying
// handle was created in New() and any failure surfaced there.
func (s *Server) Register(ctx context.Context) error {
	go s.hr.Run(ctx)
	return nil
}

// Shutdown releases the underlying fsnotify watcher. Satisfies
// system.ShutdownModule so RunForever's reverse-order drain frees
// the inotify/kqueue handle before the process exits. shutdownCtx
// is honoured implicitly: fsnotify.Close is fast (no I/O), so the
// deadline is effectively unused. Safe to call multiple times.
func (s *Server) Shutdown(shutdownCtx context.Context) error {
	return s.hr.Close()
}

// Handler returns the HTTP handler that:
//
//   - serves /__reload (SSE) and /__reload.js (the JS client) directly
//     from the embedded HotReload mux;
//   - rebuilds a fresh render.Site for every other request and
//     dispatches into it.
//
// Compose secheaders / httplog around the result; this handler does
// not touch security headers or access logs by design — those are
// concerns of the wrapping middleware, not the dev shim.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.hr.Mount(mux) // /__reload, /__reload.js

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RenderOpts is re-invoked per request so config.yaml edits
		// take effect without restarting (templates / locales /
		// static reload via fsnotify on every edit anyway). A
		// malformed YAML surfaces in the browser as plain text so
		// the dev sees the parse error without alt-tabbing.
		rOpts, err := s.opts.RenderOpts()
		if err != nil {
			log.ErrorContext(r.Context(), "dev: render opts refresh failed", "err", err, "path", r.URL.Path)
			w.Header().Set(constants.HeaderContentType, "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("dev: render opts refresh failed:\n\n" + err.Error()))
			return
		}

		site, err := render.New(rOpts)
		if err != nil {
			log.ErrorContext(r.Context(), "dev: render.New failed", "err", err, "path", r.URL.Path)
			w.Header().Set(constants.HeaderContentType, "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("dev: render.New failed:\n\n" + err.Error()))
			return
		}

		siteMux := http.NewServeMux()
		site.Mount(siteMux)
		siteMux.ServeHTTP(w, r)
	}))

	return mux
}
