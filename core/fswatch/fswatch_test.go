package fswatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fakeFsnotifier is a scripted stand-in for *fsnotify.Watcher. Tests
// drive its Events/Errors channels directly so the Run loop's
// non-happy-path branches (Errors-channel hit, channel close,
// Add-error) can be exercised deterministically without depending on
// real OS event delivery.
type fakeFsnotifier struct {
	events chan fsnotify.Event
	errs   chan error
	addErr error
	added  []string
	closed bool
}

func newFakeFsnotifier() *fakeFsnotifier {
	return &fakeFsnotifier{
		events: make(chan fsnotify.Event, 8),
		errs:   make(chan error, 8),
	}
}

func (f *fakeFsnotifier) Add(name string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, name)
	return nil
}
func (f *fakeFsnotifier) Close() error                  { f.closed = true; return nil }
func (f *fakeFsnotifier) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeFsnotifier) Errors() <-chan error          { return f.errs }

// stubFsnotifier swaps newFsnotifier for fn for the duration of the
// test, restoring the real fsnotify-backed factory on cleanup.
func stubFsnotifier(t *testing.T, fn func() (fsnotifier, error)) {
	t.Helper()
	prev := newFsnotifier
	newFsnotifier = fn
	t.Cleanup(func() { newFsnotifier = prev })
}

// settle is the post-Add pause that lets fsnotify's underlying
// kqueue/inotify start delivering events. Without it the early
// writes in a test sometimes race the listener startup. 20ms is
// enough on Linux + macOS in practice.
const settle = 20 * time.Millisecond

// waitFor returns the first value from ch within d, or fails the
// test. Used to assert "the callback fired" without a hard sleep.
func waitFor(t *testing.T, ch <-chan string, d time.Duration) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("no callback within %s", d)
		return ""
	}
}

// expectNoCall is the inverse: after waiting d, the channel must
// still be empty. Used to verify the basename filter rejected an
// event for an unrelated file.
func expectNoCall(t *testing.T, ch <-chan string, d time.Duration) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected callback: %q", v)
	case <-time.After(d):
	}
}

// runWatcher boots a Watcher with the given debounce, registers via
// the supplied configure func, and returns a channel that receives
// every onChange path. The watcher is closed and the goroutine
// stopped when the test ends.
func runWatcher(t *testing.T, debounce time.Duration, configure func(*Watcher) error) <-chan string {
	t.Helper()
	w, err := New(debounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := configure(w); err != nil {
		t.Fatalf("configure: %v", err)
	}

	ch := make(chan string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx, func(p string) { ch <- p })

	time.Sleep(settle)
	return ch
}

func TestAddTree_FiresOnWrite(t *testing.T) {
	root := t.TempDir()
	ch := runWatcher(t, 20*time.Millisecond, func(w *Watcher) error {
		return w.AddTree(root)
	})

	target := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(target, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	got := waitFor(t, ch, time.Second)
	if got != target {
		t.Errorf("path = %q, want %q", got, target)
	}
}

func TestAddTree_PicksUpSubdirectories(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested", "deep")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	ch := runWatcher(t, 20*time.Millisecond, func(w *Watcher) error {
		return w.AddTree(root)
	})

	target := filepath.Join(sub, "x.txt")
	if err := os.WriteFile(target, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	got := waitFor(t, ch, time.Second)
	if got != target {
		t.Errorf("path = %q, want %q (recursive walk should have added the subdir)", got, target)
	}
}

func TestAddFile_FiltersByBasename(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	other := filepath.Join(root, "other.yaml")
	if err := os.WriteFile(config, []byte("v: 1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("k: 1"), 0644); err != nil {
		t.Fatal(err)
	}

	ch := runWatcher(t, 20*time.Millisecond, func(w *Watcher) error {
		return w.AddFile(config)
	})

	// Touch the unrelated sibling first — basename filter should
	// drop it before the debounce timer ever starts.
	if err := os.WriteFile(other, []byte("k: 2"), 0644); err != nil {
		t.Fatal(err)
	}
	expectNoCall(t, ch, 200*time.Millisecond)

	// Now touch the registered file — should fire.
	if err := os.WriteFile(config, []byte("v: 2"), 0644); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, ch, time.Second)
	if got != config {
		t.Errorf("path = %q, want %q", got, config)
	}
}

func TestRun_DebouncesBurst(t *testing.T) {
	root := t.TempDir()
	ch := runWatcher(t, 100*time.Millisecond, func(w *Watcher) error {
		return w.AddTree(root)
	})

	// Five back-to-back writes — fsnotify will emit several events,
	// debounce should fold them into a single onChange call.
	for i := 0; i < 5; i++ {
		path := filepath.Join(root, "f.txt")
		if err := os.WriteFile(path, []byte{byte('a' + i)}, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Wait past the debounce window plus generous slack.
	time.Sleep(400 * time.Millisecond)

	count := 0
drain:
	for {
		select {
		case <-ch:
			count++
		default:
			break drain
		}
	}
	if count != 1 {
		t.Errorf("debounce produced %d fires, want exactly 1", count)
	}
}

func TestRun_ExitsOnContextCancel(t *testing.T) {
	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.AddTree(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		w.Run(ctx, func(string) {})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of ctx cancel")
	}
}

func TestClose_ReleasesWatcher(t *testing.T) {
	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestNew_PropagatesFsnotifyError — when fsnotify.NewWatcher fails
// (e.g. inotify FD exhaustion in production), New must surface the
// error to the caller instead of returning a half-built Watcher.
// The seam is the only way to drive this branch without exhausting
// host resources.
func TestNew_PropagatesFsnotifyError(t *testing.T) {
	want := errors.New("inotify exhausted")
	stubFsnotifier(t, func() (fsnotifier, error) { return nil, want })

	w, err := New(20 * time.Millisecond)
	if w != nil {
		t.Errorf("New returned watcher = %v, want nil on error", w)
	}
	if !errors.Is(err, want) {
		t.Errorf("New err = %v, want %v", err, want)
	}
}

// TestAddTree_PropagatesWalkError — filepath.Walk can fail on a
// nonexistent root before any Add is attempted. AddTree must return
// that error verbatim so the caller sees the underlying I/O reason
// instead of a generic "walk failed".
func TestAddTree_PropagatesWalkError(t *testing.T) {
	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	err = w.AddTree(filepath.Join(t.TempDir(), "does", "not", "exist"))
	if err == nil {
		t.Fatal("expected error walking nonexistent tree, got nil")
	}
}

// TestAddTree_PropagatesAddError — when fsnotify.Add fails on a
// directory that walk found, AddTree must abort with that error.
// Driven through the seam because real Add failures need
// platform-specific edge cases (revoked permissions mid-walk).
func TestAddTree_PropagatesAddError(t *testing.T) {
	fake := newFakeFsnotifier()
	fake.addErr = errors.New("permission denied")
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(t.TempDir()); err == nil {
		t.Fatal("expected AddTree to surface the inner Add error")
	}
}

// TestAddFile_PropagatesAddError — same rationale as the AddTree
// case but for the single-file path. Without coverage here a future
// refactor could swallow the inner Add error and silently watch
// nothing.
func TestAddFile_PropagatesAddError(t *testing.T) {
	fake := newFakeFsnotifier()
	fake.addErr = errors.New("not a directory")
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile("missing/dir/config.yaml"); err == nil {
		t.Fatal("expected AddFile to surface the inner Add error")
	}
}

// TestRun_ExitsOnEventsChannelClose — when fsnotify closes its
// Events channel (typical at shutdown after Close), the loop's
// `<-Events` case sees `!ok` and Run must return. Without this
// branch a closed-but-not-cancelled watcher would spin.
func TestRun_ExitsOnEventsChannelClose(t *testing.T) {
	fake := newFakeFsnotifier()
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		w.Run(context.Background(), func(string) {})
		close(done)
	}()

	close(fake.events)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit when Events channel closed")
	}
}

// TestRun_ExitsOnErrorsChannelClose — symmetric to the Events case:
// when the Errors channel closes (also part of fsnotify's shutdown),
// Run must terminate. Both close-detection branches need coverage so
// either teardown ordering exits cleanly.
func TestRun_ExitsOnErrorsChannelClose(t *testing.T) {
	fake := newFakeFsnotifier()
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		w.Run(context.Background(), func(string) {})
		close(done)
	}()

	close(fake.errs)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit when Errors channel closed")
	}
}

// TestRun_LogsErrorEvents — fsnotify error events must NOT terminate
// the loop (a single-error tear-down would be brittle). They're
// logged and the loop continues; we drive an error then a real
// event and assert both the error didn't crash and the subsequent
// event still flows through to onChange.
func TestRun_LogsErrorEvents(t *testing.T) {
	fake := newFakeFsnotifier()
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(10 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-register the dir we'll use for the follow-up event so
	// allow() returns true.
	w.mu.Lock()
	w.treeDirs["/tmp/fakedir"] = struct{}{}
	w.mu.Unlock()

	got := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx, func(p string) { got <- p })

	fake.errs <- errors.New("watcher overflow")
	// Give the loop a chance to handle and log the error.
	time.Sleep(20 * time.Millisecond)
	fake.events <- fsnotify.Event{Name: "/tmp/fakedir/x.txt", Op: fsnotify.Write}

	select {
	case p := <-got:
		if p != "/tmp/fakedir/x.txt" {
			t.Errorf("path = %q, want /tmp/fakedir/x.txt", p)
		}
	case <-time.After(time.Second):
		t.Fatal("loop appears stuck after handling an Errors event")
	}
}

// TestRun_DropsChmodEvents — pure Chmod events come from editors
// touching perms on save and contain no content signal; the loop
// must skip them without arming the debounce timer. Otherwise
// tools like git or chmod would falsely trigger reloads.
func TestRun_DropsChmodEvents(t *testing.T) {
	fake := newFakeFsnotifier()
	stubFsnotifier(t, func() (fsnotifier, error) { return fake, nil })

	w, err := New(10 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.treeDirs["/tmp/fakedir"] = struct{}{}
	w.mu.Unlock()

	got := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx, func(p string) { got <- p })

	fake.events <- fsnotify.Event{Name: "/tmp/fakedir/x.txt", Op: fsnotify.Chmod}

	select {
	case p := <-got:
		t.Errorf("Chmod event leaked through to callback: %q", p)
	case <-time.After(80 * time.Millisecond):
		// expected — debounce never armed
	}
}

// TestWrapFsnotify_PropagatesError — the error branch of the
// production newFsnotifier closure delegates to wrapFsnotify;
// exercising it directly closes the last gap on the constructor
// without needing to fail fsnotify.NewWatcher at the OS level.
func TestWrapFsnotify_PropagatesError(t *testing.T) {
	want := errors.New("inotify init failed")
	got, err := wrapFsnotify(nil, want)
	if got != nil {
		t.Errorf("wrapFsnotify returned %v on error, want nil", got)
	}
	if !errors.Is(err, want) {
		t.Errorf("wrapFsnotify err = %v, want %v", err, want)
	}
}

// TestAddTree_VisitsFilesWithoutAdding — Walk visits every entry,
// including files. AddTree must skip non-directories without
// erroring; otherwise a tree containing any regular file would fail
// to register at all. Pre-creating a file ensures the walk hits the
// !IsDir branch.
func TestAddTree_VisitsFilesWithoutAdding(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "preexisting.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.AddTree(root); err != nil {
		t.Errorf("AddTree on tree containing a file = %v, want nil", err)
	}
}

// TestAllow_FalseForUnknownDir — a directory not registered via
// AddTree or AddFile must reject every event. This locks the
// fall-through return at the bottom of allow(), which Run's filter
// depends on to stay quiet on shared parents (e.g. /tmp).
func TestAllow_FalseForUnknownDir(t *testing.T) {
	w, err := New(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if w.allow("/tmp/totally/unrelated/file.txt") {
		t.Error("allow returned true for an unwatched path")
	}
}
