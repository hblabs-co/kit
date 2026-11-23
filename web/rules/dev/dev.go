// Package dev wires hot-reload onto a rules engine for local
// development. It watches a YAML directory on disk and rebuilds the
// engine whenever a pipeline file changes, swapping the active
// instance atomically so in-flight requests see a consistent view.
//
// Wire it from main when HB_ENV is "local":
//
//	devSrv, err := rulesdev.New(rulesdev.Options{
//	    EngineFn:  buildEngineFromDisk,    // fresh engine per call
//	    WatchRoot: "services/service-foo/pipelines",
//	})
//	if err != nil { /* fail boot */ }
//
//	mux.Handle("/__rules/visitor", devSrv.HTTPHandler("visitor_flags"))
//	// devSrv satisfies system.Module — drop into RunForever
//	// alongside the HTTP server module.
//	system.RunForever(ctx, server, devSrv)
//
// Never run this in production:
//
//   - re-parses YAML on every file change with no rate limit;
//   - swaps a process-wide pointer with no auditing;
//   - logs the parse error and keeps serving the stale engine so a
//     malformed save doesn't take the page offline — a contract that
//     is fine for laptops, not for live traffic.
package dev

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hblabs-co/kit/core/fswatch"
	"github.com/hblabs-co/kit/core/log"
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// defaultDebounceWindow collapses bursty editor saves into a single
// rebuild. 100 ms is the same value web/render/dev uses — good
// for VS Code, JetBrains and vim's 3–5 writes per ⌘S.
const defaultDebounceWindow = 100 * time.Millisecond

// Options configures a dev Server.
type Options struct {
	// EngineFn returns a fresh, already-Built engine. The dev
	// wrapper calls it once at New (so a misconfigured YAML or
	// missing rule fails at boot) and again on every YAML change.
	// Typical implementation:
	//
	//	func() (*rulesengine.Engine, error) {
	//	    eng := rulesengine.NewEngine().
	//	        Register(rules.All()...).
	//	        Load(os.DirFS("services/service-foo/pipelines"), "*.yaml")
	//	    return eng, eng.Build()
	//	}
	//
	// Required.
	EngineFn func() (*rulesengine.Engine, error)

	// WatchRoot is the directory whose subtree the watcher monitors
	// for YAML pipeline changes. A change anywhere underneath fires
	// EngineFn after the debounce window. Typically the service's
	// pipelines/ directory. Required.
	WatchRoot string

	// CodeWatchRoots are additional directories whose *.go files
	// trigger a warning log when they change. Useful for laptop
	// development: Go binaries can't hot-reload code, so when you
	// edit a rule implementation the watcher tells you to restart
	// the process — without spamming the log with no-op engine
	// rebuilds that wouldn't pick up the change anyway.
	//
	// Roots that don't exist on disk are skipped with a single boot
	// log line, never a hard error — keeping a missing `packages/
	// rules` (e.g. when running from outside the repo root) from
	// taking the service offline. Optional.
	CodeWatchRoots []string

	// DebounceWindow collapses bursty editor saves. Zero → 100 ms.
	DebounceWindow time.Duration
}

// Server is the dev wrapper. Build with New, drop into
// system.RunForever via Register/Shutdown, expose endpoints via
// HTTPHandler.
type Server struct {
	opts Options

	// eng holds the current engine. atomic.Pointer so HTTPHandler
	// can read it on every request without a lock; the watch loop
	// swaps it after a successful rebuild.
	eng atomic.Pointer[rulesengine.Engine]

	watcher *fswatch.Watcher
}

// New validates opts, calls EngineFn once so boot fails fast on a
// bad config, and stages the watcher. Register starts the loop.
func New(opts Options) (*Server, error) {
	if opts.EngineFn == nil {
		return nil, errors.New("rulesdev: Options.EngineFn is required")
	}
	if opts.WatchRoot == "" {
		return nil, errors.New("rulesdev: Options.WatchRoot is required")
	}

	initial, err := opts.EngineFn()
	if err != nil {
		return nil, fmt.Errorf("rulesdev: initial EngineFn: %w", err)
	}

	debounce := opts.DebounceWindow
	if debounce <= 0 {
		debounce = defaultDebounceWindow
	}
	w, err := newWatcherFn(debounce)
	if err != nil {
		return nil, fmt.Errorf("rulesdev: watcher: %w", err)
	}
	if err := w.AddTree(opts.WatchRoot); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("rulesdev: watch %s: %w", opts.WatchRoot, err)
	}
	for _, root := range opts.CodeWatchRoots {
		if !codeRootExists(root) {
			log.Warn("rulesdev: code watch root not found; skipping", "root", root)
			continue
		}
		if err := w.AddTree(root); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("rulesdev: watch code root %s: %w", root, err)
		}
	}

	s := &Server{opts: opts, watcher: w}
	s.eng.Store(initial)
	return s, nil
}

// codeRootExists is the variable seam tests use to bypass the
// filesystem check. Production stats the path; tests can override
// to skip-or-allow specific roots deterministically.
var codeRootExists = func(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// newWatcherFn is the variable seam tests use to inject a stub
// watcher without spinning up real fsnotify. Production always
// returns fswatch.New.
var newWatcherFn = func(d time.Duration) (*fswatch.Watcher, error) {
	return fswatch.New(d)
}

// Register starts the file-watching goroutine and returns
// immediately, satisfying system.Module so a *Server drops into
// system.RunForever alongside the HTTP server.
//
// Always returns nil — fsnotify failures surfaced at New time.
func (s *Server) Register(ctx context.Context) error {
	go s.watcher.Run(ctx, s.onChange)
	return nil
}

// Shutdown releases the underlying fsnotify watcher. Satisfies the
// shutdown half of system.Module so RunForever's reverse-order
// drain frees the OS handle before exit.
func (s *Server) Shutdown(_ context.Context) error {
	return s.watcher.Close()
}

// Engine returns the currently active rules engine. Reads are
// lock-free; the value may change between calls if a rebuild lands
// in between.
func (s *Server) Engine() *rulesengine.Engine {
	return s.eng.Load()
}

// HTTPHandler returns an http.HandlerFunc that always dispatches
// through the current engine — even after a hot-reload swaps it.
// Bind it once at boot and forget; the closure resolves the engine
// on every request.
func (s *Server) HTTPHandler(pipeline string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Engine().HTTPHandler(pipeline)(w, r)
	}
}

// onChange is the debounced callback fswatch.Run invokes after a
// watched-file edit settles.
//
// Two cases:
//
//   - *.go files (only fired when CodeWatchRoots is set): log a
//     warning so the developer knows to restart. We can't hot-swap
//     compiled Go without re-execing the binary, and silently
//     rebuilding the engine would log "pipelines reloaded" without
//     actually picking up the edit — confusing.
//   - everything else (YAMLs under WatchRoot): rebuild the engine,
//     diff the previous and new snapshots so the operator sees
//     *which* pipelines and *which* rules changed (added /
//     removed / reconfigured), then swap the atomic pointer. A
//     failed rebuild keeps the previous engine serving so a bad
//     save doesn't 500 every request.
func (s *Server) onChange(path string) {
	if strings.HasSuffix(path, ".go") {
		log.Warn("rulesdev: rule code changed — restart to pick it up",
			"path", filepath.ToSlash(path))
		return
	}
	prev := s.eng.Load().Snapshot()
	next, err := s.opts.EngineFn()
	if err != nil {
		log.Error("rulesdev: rebuild failed; keeping previous engine",
			"err", err, "path", path)
		return
	}
	s.eng.Store(next)
	logSnapshotDiff(prev, next.Snapshot(), path)
}

// logSnapshotDiff emits one structured log line per pipeline that
// changed between the previous and current engine. Pulled out of
// onChange so the callback stays readable and the formatting is
// unit-testable. A rebuild that produced no observable changes
// gets one summary line so the dev knows the save was processed.
func logSnapshotDiff(prev, next rulesengine.Snapshot, path string) {
	diffs := next.Diff(prev)
	if len(diffs) == 0 {
		log.Info("rulesdev: rebuild OK; no observable pipeline changes",
			"path", path)
		return
	}
	for _, d := range diffs {
		switch d.Kind {
		case "added":
			log.Info("rulesdev: pipeline added",
				"pipeline", d.Pipeline, "path", path)
		case "removed":
			log.Info("rulesdev: pipeline removed",
				"pipeline", d.Pipeline, "path", path)
		case "modified":
			log.Info("rulesdev: pipeline updated",
				"pipeline", d.Pipeline,
				"added", joinOrDash(d.AddedRules),
				"removed", joinOrDash(d.RemovedRules),
				"changed", joinOrDash(d.ChangedRules),
				"path", path)
		}
	}
}

// joinOrDash returns "-" for an empty slice and a comma-joined
// string otherwise. The dash keeps log lines aligned and makes
// "nothing here" visually distinct from a missing field.
func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}
