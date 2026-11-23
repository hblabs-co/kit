package uikit

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// filepathAbsImpl wraps filepath.Abs so the test file uses one
// import site for path-handling. Tiny indirection but keeps the
// rest of the file free of stdlib clutter.
var filepathAbsImpl = filepath.Abs

// TestFS_ExposesTokensCSS — pinning the file path the layout
// templates depend on. If a refactor relocates tokens.css inside the
// uikit package, this test fails before any service notices a
// missing stylesheet.
func TestFS_ExposesTokensCSS(t *testing.T) {
	body, err := fs.ReadFile(FS, "css/tokens.css")
	if err != nil {
		t.Fatalf("read css/tokens.css: %v", err)
	}
	// Sanity: we expect HB-prefixed custom properties as the most
	// stable signal that this is the design-system tokens file and
	// not, say, an empty stub.
	if !strings.Contains(string(body), "--hb-accent") {
		t.Errorf("tokens.css missing --hb-accent custom property")
	}
}

// TestFS_ExposesComponentsCSS — same pinning for components.css. A
// non-empty body is enough; the contents are reviewed visually
// elsewhere.
func TestFS_ExposesComponentsCSS(t *testing.T) {
	info, err := fs.Stat(FS, "css/components.css")
	if err != nil {
		t.Fatalf("stat css/components.css: %v", err)
	}
	if info.Size() == 0 {
		t.Error("components.css is empty")
	}
}

// TestMustSub_PanicsOnBadInput — drives the error branch of
// mustSub deterministically. fs.Sub returns an error when the
// target dir doesn't exist in the source FS, so we hand it an
// empty fstest.MapFS and assert the panic message is informative.
func TestMustSub_PanicsOnBadInput(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustSub did not panic on missing dir")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("recovered value not a string: %T", r)
		}
		if !strings.Contains(msg, "uikit: fs.Sub") {
			t.Errorf("panic msg = %q, want to mention uikit: fs.Sub", msg)
		}
	}()
	// Sub on a path containing "/" forces fs.Sub's input validator
	// to error out — the documented "invalid argument" path. Empty
	// MapFS just gives us a concrete fs.FS to hand in.
	_ = mustSub(fstest.MapFS{}, "../bad")
}

// TestMustSub_HappyPath — also exercise the success branch directly
// (independently of FS init) so refactors that change the panic
// message don't accidentally break the success path silently.
func TestMustSub_HappyPath(t *testing.T) {
	in := fstest.MapFS{"sub/x.txt": &fstest.MapFile{Data: []byte("hi")}}
	got := mustSub(in, "sub")
	body, err := fs.ReadFile(got, "x.txt")
	if err != nil {
		t.Fatalf("ReadFile after Sub: %v", err)
	}
	if string(body) != "hi" {
		t.Errorf("body = %q, want hi", body)
	}
}

// TestDevFS_FallsBackToEmbedFromUnknownCwd — when the test runs
// from web/uikit/ (the package's own dir), devSourcePath
// "web/uikit/assets" doesn't resolve to a real directory, so
// DevFS must transparently fall back to the embedded FS rather
// than serve 404s. This is the safety net for developers running
// from a sub-cwd.
func TestDevFS_FallsBackToEmbedFromUnknownCwd(t *testing.T) {
	// `go test` cwd is the package directory, where
	// web/uikit/assets does not exist as a relative path —
	// good fixture for the fallback branch.
	got := DevFS()
	if _, err := fs.Stat(got, "css/tokens.css"); err != nil {
		t.Fatalf("DevFS fallback could not stat css/tokens.css: %v", err)
	}
}

// TestDevFS_PrefersOnDiskWhenAvailable — when the on-disk path
// resolves (running from repo root), DevFS returns the live
// os.DirFS so edits propagate. We chdir into the repo root for
// the duration of the test to drive that branch deterministically.
func TestDevFS_PrefersOnDiskWhenAvailable(t *testing.T) {
	// Find the repo root by walking up until web/uikit/assets
	// resolves. Avoids hard-coding a relative path that would
	// break if test layout shifts.
	wd, _ := os.Getwd()
	dir := wd
	for {
		if _, err := os.Stat(dir + "/web/uikit/assets/css/tokens.css"); err == nil {
			break
		}
		parent := dir + "/.."
		abs, _ := filepathAbs(parent)
		if abs == dir {
			t.Skip("could not locate repo root from test cwd")
		}
		dir = abs
	}
	t.Chdir(dir)
	got := DevFS()
	body, err := fs.ReadFile(got, "css/tokens.css")
	if err != nil {
		t.Fatalf("ReadFile via on-disk DevFS: %v", err)
	}
	if !strings.Contains(string(body), "--hb-accent") {
		t.Errorf("on-disk DevFS missing --hb-accent — wrong file?")
	}
}

// filepathAbs is a tiny wrapper around filepath.Abs to keep the
// test file's import surface minimal (one stdlib path-handling call
// site, used only here).
func filepathAbs(p string) (string, error) {
	return filepathAbsImpl(p)
}
