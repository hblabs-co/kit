package dev

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/web/render"
)

// fakeHub records Broadcast calls so tests can assert the reload
// fan-out without a real SSE connection.
type fakeHub struct {
	broadcasts atomic.Int32
	notify     chan struct{}
}

func newFakeHub() *fakeHub { return &fakeHub{notify: make(chan struct{}, 16)} }

func (f *fakeHub) Broadcast() {
	f.broadcasts.Add(1)
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeHub) ServeSSE(eventName string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("event: " + eventName + "\n\n"))
	}
}

// writeSite lays out the smallest renderable site under dir.
func writeSite(t *testing.T, dir, body string) {
	t.Helper()
	files := map[string]string{
		"pages/index.html.tmpl":      `{{ define "content" }}<p>` + body + `</p>{{ end }}`,
		"partials/_layout.html.tmpl": `{{ define "_layout" }}<html><body>{{ template "content" . }}</body></html>{{ end }}`,
		"locales/es.json":            `{"site.title":"T","site.description":"D"}`,
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func renderOpts(dir string) func() (render.Options, error) {
	return func() (render.Options, error) {
		return render.Options{FS: os.DirFS(dir), SupportedLangs: []string{"es"}, DefaultLang: "es"}, nil
	}
}

func TestNew_Validation(t *testing.T) {
	log.Capture(t, "dev")
	dir := t.TempDir()

	if _, err := New(Options{RenderOpts: renderOpts(dir)}); err == nil || !strings.Contains(err.Error(), "WatchRoot") {
		t.Errorf("missing WatchRoot: err = %v", err)
	}
	if _, err := New(Options{WatchRoot: dir}); err == nil || !strings.Contains(err.Error(), "RenderOpts") {
		t.Errorf("missing RenderOpts: err = %v", err)
	}
	boom := errors.New("boom")
	if _, err := New(Options{WatchRoot: dir, RenderOpts: func() (render.Options, error) { return render.Options{}, boom }}); !errors.Is(err, boom) {
		t.Errorf("failing RenderOpts: err = %v, want it to wrap boom", err)
	}
	if _, err := New(Options{WatchRoot: dir, RenderOpts: func() (render.Options, error) { return render.Options{}, nil }}); err == nil || !strings.Contains(err.Error(), "FS") {
		t.Errorf("nil FS: err = %v", err)
	}
	if _, err := New(Options{WatchRoot: filepath.Join(dir, "does-not-exist"), RenderOpts: renderOpts(dir)}); err == nil {
		t.Error("unwatchable WatchRoot: want an error")
	}
}

func TestServer_RebuildsOnEveryRequest(t *testing.T) {
	log.Capture(t, "dev")
	dir := t.TempDir()
	writeSite(t, dir, "first")

	s, err := New(Options{WatchRoot: dir, RenderOpts: renderOpts(dir), DebounceWindow: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	h := s.Handler()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/es/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<p>first</p>") {
		t.Fatalf("first render = %d %q", rec.Code, rec.Body.String())
	}

	// No restart, no rebuild call: the next request sees the edit.
	writeSite(t, dir, "second")
	if rec := get("/es/"); !strings.Contains(rec.Body.String(), "<p>second</p>") {
		t.Fatalf("edit not picked up: %q", rec.Body.String())
	}

	// A broken template is reported in the response instead of
	// killing the dev server; fixing it recovers.
	if err := os.WriteFile(filepath.Join(dir, "pages", "index.html.tmpl"), []byte(`{{ define "content" }}{{ nope }}{{ end }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := get("/es/"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("broken template: status = %d, want 500", rec.Code)
	}
	writeSite(t, dir, "third")
	if rec := get("/es/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<p>third</p>") {
		t.Fatalf("did not recover: %d %q", rec.Code, rec.Body.String())
	}

	// The reload endpoints are mounted next to the site.
	if rec := get("/__reload.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "EventSource('/__reload')") {
		t.Errorf("/__reload.js = %d %q", rec.Code, rec.Body.String())
	}
}

func TestServer_RenderOptsErrorIsReported(t *testing.T) {
	log.Capture(t, "dev")
	dir := t.TempDir()
	writeSite(t, dir, "x")

	var fail atomic.Bool
	s, err := New(Options{WatchRoot: dir, RenderOpts: func() (render.Options, error) {
		if fail.Load() {
			return render.Options{}, errors.New("config went away")
		}
		return renderOpts(dir)()
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	fail.Store(true)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "config went away") {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestHotReload_FileChangeBroadcasts(t *testing.T) {
	log.Capture(t, "dev")
	dir := t.TempDir()
	hub := newFakeHub()
	hr, err := NewHotReloader(10*time.Millisecond, hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hr.Close() })
	if err := hr.WatchTree(dir); err != nil {
		t.Fatal(err)
	}

	var order []string
	done := make(chan struct{}, 4)
	hr.OnReload(func() { order = append(order, "first") })
	hr.OnReload(func() { order = append(order, "second"); done <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hr.Run(ctx)

	// fsnotify needs the watch to be live; retry the write until the
	// broadcast arrives rather than sleeping a fixed amount.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		if err := os.WriteFile(filepath.Join(dir, "page.tmpl"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case <-hub.notify:
			<-done
			if len(order) < 2 || order[0] != "first" || order[1] != "second" {
				t.Errorf("OnReload order = %v", order)
			}
			return
		case <-tick.C:
		case <-deadline:
			t.Fatal("no broadcast after a file change")
		}
	}
}

func TestHotReload_BroadcastMountAndInject(t *testing.T) {
	hub := newFakeHub()
	hr, err := NewHotReloader(time.Millisecond, hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hr.Close() })

	hr.Broadcast()
	if hub.broadcasts.Load() != 1 {
		t.Errorf("Broadcast not forwarded to the hub")
	}

	mux := http.NewServeMux()
	hr.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__reload", nil))
	if !strings.Contains(rec.Body.String(), "event: reload") {
		t.Errorf("/__reload = %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__reload.js", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	withBody := string(hr.InjectScript([]byte("<html><body>x</body></html>")))
	if withBody != "<html><body>x"+ScriptTag+"\n</body></html>" {
		t.Errorf("InjectScript before </body> = %q", withBody)
	}
	noBody := string(hr.InjectScript([]byte("fragment")))
	if noBody != "fragment"+ScriptTag {
		t.Errorf("InjectScript without </body> = %q", noBody)
	}
}
