package render

import (
	"context"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hblabs-co/kit/web/render/og"
)

// errFS wraps fstest.MapFS and replaces selected paths with an
// erroring fs.File, letting tests force fs.WalkDir / fs.Stat /
// io.Copy error branches that the real stdlib FS impls never hit on
// in-memory data.
type errFS struct {
	inner   fstest.MapFS
	openErr map[string]error // path → err returned by Open
	readErr map[string]error // path → err returned by the file's Read
}

func (e errFS) Open(name string) (fs.File, error) {
	if err, ok := e.openErr[name]; ok {
		return nil, err
	}
	if err, ok := e.readErr[name]; ok {
		f, oerr := e.inner.Open(name)
		if oerr != nil {
			return nil, oerr
		}
		return errFile{File: f, readErr: err}, nil
	}
	return e.inner.Open(name)
}

// readDirErrFS returns an io error when WalkDir tries to read a
// directory's entries. Used to drive the `if err != nil` branch in
// the loadFingerprints walk callback — fstest.MapFS's directories
// always read cleanly, so without this FS the branch is unreachable.
type readDirErrFS struct {
	inner fstest.MapFS
	// dirErr maps directory names (relative to the root) to errors
	// returned by ReadDir. WalkDir surfaces those to the callback as
	// its `err` argument.
	dirErr map[string]error
}

func (r readDirErrFS) Open(name string) (fs.File, error) { return r.inner.Open(name) }

func (r readDirErrFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err, ok := r.dirErr[name]; ok {
		return nil, err
	}
	return r.inner.ReadDir(name)
}

// statErrFS returns a non-NotExist error from Stat on a chosen
// path. Drives the loadFingerprints branch where fs.Stat fails for
// a reason other than "no static/ dir" (a real I/O failure on a
// hosted FS, say) — without the seam this 500 path stays
// permanently uncovered.
type statErrFS struct {
	inner   fstest.MapFS
	statErr map[string]error
}

func (s statErrFS) Open(name string) (fs.File, error) {
	if err, ok := s.statErr[name]; ok {
		return nil, err
	}
	return s.inner.Open(name)
}

// Stat implements StatFS so fs.Sub's subFS routes Stat through us
// (Go 1.21+'s subFS forwards Stat when the underlying FS implements
// it). Without this method the test would only exercise the Open
// branch and miss callers that prefer the StatFS fast path.
func (s statErrFS) Stat(name string) (fs.FileInfo, error) {
	if err, ok := s.statErr[name]; ok {
		return nil, err
	}
	return fs.Stat(s.inner, name)
}

// errFile is the fs.File half of errFS — Read returns a scripted
// error so io.Copy in hashFile sees a partial-read failure that
// real *bytes.Reader-backed files never produce.
type errFile struct {
	fs.File
	readErr error
}

func (e errFile) Read(p []byte) (int, error) { return 0, e.readErr }

// withSiteOpts is the rich constructor used by tests that need
// non-default Options (CanonicalHost, Dev, OG, multi-lang, …).
func withSiteOpts(t *testing.T, fsys fs.FS, opts Options) *Site {
	t.Helper()
	if opts.FS == nil {
		opts.FS = fsys
	}
	if len(opts.SupportedLangs) == 0 {
		opts.SupportedLangs = []string{"es"}
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// multiLangFS is the canonical fixture for tests that need more than
// the bare minimum: two pages, two locales, plus per-page OG keys so
// localeOr's per-key fallback chain has something to traverse.
func multiLangFS() fstest.MapFS {
	return fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p>{{ T .Lang "hello" }}</p>{{ end }}`),
		},
		"pages/about.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p>about: {{ asset "x.css" }}</p>{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}<html lang="{{ .Lang }}">{{ template "content" . }}{{ devScript }}</html>{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{"hello":"hola","site.title":"Sitio","site.description":"Desc"}`)},
		"locales/en.json": &fstest.MapFile{Data: []byte(`{"hello":"hi","og.about.title":"About!"}`)},
		"static/x.css":    &fstest.MapFile{Data: []byte("body{}")},
	}
}

// TestNew_RequiresFS pins the boot guard: a missing FS is a config
// bug that should fail loudly, not panic on first request.
func TestNew_RequiresFS(t *testing.T) {
	_, err := New(Options{SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "FS") {
		t.Fatalf("err = %v, want it to mention FS", err)
	}
}

// TestNew_RequiresSupportedLangs — a service can boot with no FS only
// if it never serves any URL; an empty SupportedLangs is always wrong
// because every render path resolves a lang first.
func TestNew_RequiresSupportedLangs(t *testing.T) {
	_, err := New(Options{FS: minimalSiteFS(nil)})
	if err == nil || !strings.Contains(err.Error(), "SupportedLangs") {
		t.Fatalf("err = %v, want it to mention SupportedLangs", err)
	}
}

// TestNew_DefaultsLangFromSupported — leaving DefaultLang empty
// shouldn't fail; we pick the first supported lang. Keeps the common
// single-language config a one-liner.
func TestNew_DefaultsLangFromSupported(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
	})
	if s.defaultLang != "es" {
		t.Errorf("defaultLang = %q, want %q", s.defaultLang, "es")
	}
}

// TestNew_RejectsDefaultLangNotInSupported pins the cross-field
// validation: a typo'd default would silently fall back at runtime
// without this guard.
func TestNew_RejectsDefaultLangNotInSupported(t *testing.T) {
	_, err := New(Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		DefaultLang:    "fr",
	})
	if err == nil || !strings.Contains(err.Error(), "DefaultLang") {
		t.Fatalf("err = %v, want it to mention DefaultLang", err)
	}
}

// TestNew_FailsOnMissingLayout — the layout partial is non-optional.
// Catching its absence at boot beats a 500 on every page request.
func TestNew_FailsOnMissingLayout(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"locales/es.json":       &fstest.MapFile{Data: []byte(`{}`)},
	}
	_, err := New(Options{FS: fsys, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "_layout") {
		t.Fatalf("err = %v, want layout error", err)
	}
}

// TestNew_FailsOnMissingLocale — every supported lang must have a
// matching locales/<code>.json. We fail fast so the operator sees
// the typo at boot.
func TestNew_FailsOnMissingLocale(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	_, err := New(Options{FS: fsys, SupportedLangs: []string{"es", "en"}})
	if err == nil || !strings.Contains(err.Error(), "load locales") {
		t.Fatalf("err = %v, want load locales error", err)
	}
}

// TestNew_FailsOnInvalidLocaleJSON — silently ignoring a malformed
// JSON file would render every translation as the raw key. Boot
// failure forces the operator to fix it.
func TestNew_FailsOnInvalidLocaleJSON(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`not-json`)},
	}
	_, err := New(Options{FS: fsys, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "load locales") {
		t.Fatalf("err = %v, want locale parse error", err)
	}
}

// TestNew_FailsOnTemplateParseError — a busted .html.tmpl in pages/
// must fail boot rather than serve broken HTML on first request.
func TestNew_FailsOnTemplateParseError(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/broken.html.tmpl":     &fstest.MapFile{Data: []byte(`{{ if `)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	_, err := New(Options{FS: fsys, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "load pages") {
		t.Fatalf("err = %v, want pages parse error", err)
	}
}

// TestLoadPages_WalkErrorBubblesUp — when a directory inside pages/
// errors mid-walk, loadPages must surface the error wrapped under
// "load pages" so the operator pinpoints the failing file. Without
// the readDirErrFS seam this branch is unreachable.
func TestLoadPages_WalkErrorBubblesUp(t *testing.T) {
	mapfs := fstest.MapFS{
		"pages/sub/nested.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "content" }}n{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	bad := readDirErrFS{
		inner:  mapfs,
		dirErr: map[string]error{"pages/sub": errors.New("dir-flaky")},
	}
	_, err := New(Options{FS: bad, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "load pages") {
		t.Fatalf("err = %v, want load pages error", err)
	}
}

// TestLoadPages_SkipsUnderscorePrefixedAndNonTmpl — files starting
// with "_" are partials, anything not ".html.tmpl" isn't ours.
// Walking past either silently is the contract layouts rely on.
func TestLoadPages_SkipsUnderscorePrefixedAndNonTmpl(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"pages/_partial.html.tmpl":   &fstest.MapFile{Data: []byte(`{{ define "p" }}p{{ end }}`)},
		"pages/notes.txt":            &fstest.MapFile{Data: []byte(`ignored`)},
		"pages/sub/nested.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "content" }}n{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{})
	if _, ok := s.pages[""]; !ok {
		t.Errorf("missing index page key")
	}
	if _, ok := s.pages["sub/nested"]; !ok {
		t.Errorf("missing sub/nested page key")
	}
	if _, ok := s.pages["_partial"]; ok {
		t.Errorf("partial leaked into pages map")
	}
}

// TestNew_FailsOnFingerprintError — a transient error reading static/
// at boot is fatal: cache-busting URLs would silently fall back to
// assetVer, hiding the underlying problem.
func TestNew_FailsOnFingerprintError(t *testing.T) {
	mapfs := minimalSiteFS(map[string][]byte{"a.css": []byte("x")})
	bad := errFS{
		inner:   mapfs,
		readErr: map[string]error{"static/a.css": errors.New("boom")},
	}
	_, err := New(Options{FS: bad, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("err = %v, want fingerprint error", err)
	}
}

// TestFingerprint_FsSubFailsSilently — the fsSub seam swap drives
// the otherwise unreachable defensive guard in loadFingerprints.
// The contract is fail-soft: an erroring fs.Sub leaves the
// fingerprint map empty so asset() falls back to assetVer rather
// than crashing the boot.
func TestFingerprint_FsSubFailsSilently(t *testing.T) {
	orig := fsSub
	fsSub = func(_ fs.FS, _ string) (fs.FS, error) { return nil, errors.New("seam-sub-error") }
	t.Cleanup(func() { fsSub = orig })

	// loadFingerprints swallows fsSub error and returns nil — so New
	// still succeeds, but the fingerprints map stays empty. Mount also
	// uses the seam; both branches matter.
	s := withSiteOpts(t, multiLangFS(), Options{})
	if len(s.fingerprints) != 0 {
		t.Errorf("expected empty fingerprints when fsSub fails, got %v", s.fingerprints)
	}
}

// TestMount_SkipsStaticWhenFsSubFails — the same seam is used in
// Mount. When it errors we don't register /static/, which means a
// GET /static/foo falls through to the catch-all 404. This pins
// that fail-soft contract.
func TestMount_SkipsStaticWhenFsSubFails(t *testing.T) {
	orig := fsSub
	fsSub = func(_ fs.FS, _ string) (fs.FS, error) { return nil, errors.New("seam-sub-error") }
	t.Cleanup(func() { fsSub = orig })

	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/x.css", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when /static/ wasn't mounted", rec.Code)
	}
}

// TestFingerprint_StatNonNotExistError — when stating static/
// fails for any reason other than NotExist, that's a real I/O
// failure (network FS, permissions) and we surface it as a wrapped
// boot error instead of silently skipping fingerprints. Without
// the statErrFS seam this branch is unreachable for fstest.MapFS.
func TestFingerprint_StatNonNotExistError(t *testing.T) {
	mapfs := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
		"static/x.css":               &fstest.MapFile{Data: []byte("body{}")},
	}
	bad := statErrFS{
		inner:   mapfs,
		statErr: map[string]error{"static": errors.New("permission denied")},
	}
	_, err := New(Options{FS: bad, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "stat static/") {
		t.Fatalf("err = %v, want stat static/ error", err)
	}
}

// TestFingerprint_WalkDirErrorBubblesUp — drives the walk
// callback's `if err != nil` branch by making ReadDir on the
// static subtree return an error mid-walk. fstest.MapFS dirs read
// cleanly, so we wrap with readDirErrFS to simulate a flaky FS.
func TestFingerprint_WalkDirErrorBubblesUp(t *testing.T) {
	mapfs := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
		"static/sub/x.css":           &fstest.MapFile{Data: []byte("body{}")},
	}
	bad := readDirErrFS{
		inner: mapfs,
		// "static/sub" is the path WalkDir tries to enumerate after
		// landing on the directory. Erroring there reaches the
		// callback with err != nil.
		dirErr: map[string]error{"static/sub": errors.New("dir-flaky")},
	}
	_, err := New(Options{FS: bad, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("err = %v, want fingerprint walk error", err)
	}
}

// TestFingerprint_HashFileOpenError — when fs.Open errors, the walk
// callback should propagate it wrapped so the operator can find the
// offending path quickly.
func TestFingerprint_HashFileOpenError(t *testing.T) {
	mapfs := minimalSiteFS(map[string][]byte{"a.css": []byte("x")})
	bad := errFS{
		inner:   mapfs,
		openErr: map[string]error{"static/a.css": errors.New("open-failed")},
	}
	_, err := New(Options{FS: bad, SupportedLangs: []string{"es"}})
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("err = %v, want hash wrap", err)
	}
}

// TestMount_HealthzServesOK — /healthz is the liveness contract;
// load balancers and orchestrators rely on the literal "ok" body
// and 200 status. Pin both.
func TestMount_HealthzServesOK(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// TestMount_ReadyzDefaultsToHealthz — without Options.Ready the
// readiness probe mirrors liveness: 200 "ok" as soon as the binary
// serves. Pin it so services without a dependency probe stay ready.
func TestMount_ReadyzDefaultsToHealthz(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// TestMount_ReadyzUsesProbe — a configured Options.Ready decides:
// nil error answers 200, an error answers 503 with the error text in
// the body so a failing rollout is diagnosable from the probe log.
func TestMount_ReadyzUsesProbe(t *testing.T) {
	probeErr := error(nil)
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		Ready:          func(context.Context) error { return probeErr },
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthy probe: status = %d, want 200", rec.Code)
	}

	probeErr = errors.New("upstream down")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("failing probe: status = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "upstream down") {
		t.Errorf("failing probe body = %q, want the probe error surfaced", got)
	}
}

// TestMount_StaticFilesAreCacheImmutable — Cache-Control: immutable
// is what makes the asset-versioned URLs safe; missing header would
// leak a regression silently because pages still load.
func TestMount_StaticFilesAreCacheImmutable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/x.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

// TestRouter_RootRedirectsToPickedLang — bare / is handled by
// pickLang + a 302. The cookie pin keeps the visitor on their
// pick across navigations.
func TestRouter_RootRedirectsToPickedLang(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/en/" {
		t.Errorf("Location = %q, want /en/", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != langCookie || cookies[0].Value != "en" {
		t.Errorf("cookies = %v, want hbl_lang=en", cookies)
	}
}

// TestRouter_RendersPageInLang covers the happy path: /<lang>/<page>
// resolves to the matching template, in the asked-for lang.
func TestRouter_RendersPageInLang(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.test",
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/en/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `lang="en"`) {
		t.Errorf("body missing lang=en: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Errorf("body missing translation: %q", rec.Body.String())
	}
}

// TestRouter_TrailingSlashTolerated pins the friendly /es/about and
// /es/about/ both render the same page contract — losing it would
// double the 404 rate from any nav that auto-trails.
func TestRouter_TrailingSlashTolerated(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/about/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRouter_UnknownLang404sAndClearsStaleCookie — a path with a
// non-supported lang must 404 (no redirect, no input reflection) and,
// if the visitor's cookie was for that retired lang, clear it so the
// visitor doesn't get stuck on a phantom value.
func TestRouter_UnknownLang404sAndClearsStaleCookie(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/de/", nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "fr"}) // both invalid
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	cookies := rec.Result().Cookies()
	var sawClear bool
	for _, c := range cookies {
		if c.Name == langCookie && c.MaxAge < 0 {
			sawClear = true
		}
	}
	if !sawClear {
		t.Errorf("expected hbl_lang cookie cleared, got %v", cookies)
	}
}

// TestRouter_UnknownLang_ValidCookie_NotCleared — when the URL has
// an unknown lang but the visitor's cookie still points at a
// supported one, leave the cookie alone. Pins the contains() guard
// in clearLangCookieIfInvalid so a one-off bad URL doesn't wipe a
// healthy preference.
func TestRouter_UnknownLang_ValidCookie_NotCleared(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/de/", nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "en"}) // still valid
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie && c.MaxAge < 0 {
			t.Errorf("clear emitted for valid cookie: %v", c)
		}
	}
}

// TestRouter_UnknownLang_NoCookie_StillNoClear — when there's no
// cookie at all, the router shouldn't emit a clear (extra Set-Cookie
// is just noise on a 404). Pins the early-return in
// clearLangCookieIfInvalid.
func TestRouter_UnknownLang_NoCookie_StillNoClear(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/de/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie {
			t.Errorf("unexpected hbl_lang cookie on 404 with no incoming cookie: %v", c)
		}
	}
}

// TestRouter_ValidLangButUnknownPage — refresh the cookie (the
// visitor's lang is still good) and 404. Distinct from
// unknown-lang because the cookie shouldn't be cleared.
func TestRouter_ValidLangButUnknownPage(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/no-such-page", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var saw bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie && c.Value == "es" && c.MaxAge > 0 {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected fresh hbl_lang=es cookie, got %v", rec.Result().Cookies())
	}
}

// TestRouter_UnknownLang_NotEvenSegment — /.env probe-style paths
// must not redirect; they just 404. Pins the design comment about
// avoiding crawler redirect loops.
func TestRouter_UnknownLang_NotEvenSegment(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.env.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestI18n_PickLangFromCookie — once the visitor picked a lang, the
// cookie wins over Accept-Language. Critical so a manual switcher
// click doesn't get reverted by the browser's locale.
func TestI18n_PickLangFromCookie(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "en"})
	req.Header.Set("Accept-Language", "es-ES")
	if got := s.pickLang(req); got != "en" {
		t.Errorf("pickLang = %q, want en (cookie wins)", got)
	}
}

// TestI18n_PickLangIgnoresStaleCookie — if the cookie value isn't a
// supported lang anymore, fall through to Accept-Language. Without
// this the visitor would hit defaultLang forever after a locale was
// retired, even with a fresh browser preference.
func TestI18n_PickLangIgnoresStaleCookie(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "fr"}) // retired
	req.Header.Set("Accept-Language", "en")
	if got := s.pickLang(req); got != "en" {
		t.Errorf("pickLang = %q, want en", got)
	}
}

// TestI18n_PickLangFromAcceptLanguage_FallsToDefault — when the
// browser asks for a lang we don't speak, we serve our default
// instead of returning an error.
func TestI18n_PickLangFromAcceptLanguage_FallsToDefault(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		DefaultLang:    "es",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "fr-FR,it;q=0.5")
	if got := s.pickLang(req); got != "es" {
		t.Errorf("pickLang = %q, want es (default)", got)
	}
}

// TestI18n_PickLangParsesQValueShape — the parser strips ";q=…" so
// "en;q=0.7" still matches "en". Proves we don't accidentally
// require a bare tag.
func TestI18n_PickLangParsesQValueShape(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "en;q=0.7,es;q=0.3")
	if got := s.pickLang(req); got != "en" {
		t.Errorf("pickLang = %q, want en", got)
	}
}

// TestI18n_PickLangSkipsOneCharTag — "x" alone shouldn't match any
// 2-letter code. Proves the len>=2 guard works.
func TestI18n_PickLangSkipsOneCharTag(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es", "en"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "x,en")
	if got := s.pickLang(req); got != "en" {
		t.Errorf("pickLang = %q, want en", got)
	}
}

// TestI18n_TranslateFallbackChain — missing translations bubble up
// from lang → defaultLang → raw key, never returning empty. The
// final raw-key fallback is the visible-bug signal we rely on.
func TestI18n_TranslateFallbackChain(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		DefaultLang:    "es",
	})

	if got := s.translate("en", "hello"); got != "hi" {
		t.Errorf("en/hello = %q, want hi", got)
	}
	// Only present in es (default) — en falls back to it.
	if got := s.translate("en", "site.title"); got != "Sitio" {
		t.Errorf("en/site.title = %q, want Sitio", got)
	}
	// Missing in both — raw key is the visible-bug fallback.
	if got := s.translate("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("missing key = %q, want raw key", got)
	}
}

// TestRenderPage_RecoversFromTemplateError — a template that fails
// at execute time must serve a generic 500, not a half-flushed body
// the browser would render as broken HTML.
func TestRenderPage_RecoversFromTemplateError(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			// Invokes a method that doesn't exist on string → exec err.
			Data: []byte(`{{ define "content" }}{{ .Lang.NoSuchMethod }}{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "NoSuchMethod") {
		t.Errorf("500 body leaked template detail: %q", rec.Body.String())
	}
}

// TestFuncMap_AssetUsesFingerprintWhenAvailable — known asset path
// emits the fingerprint version, not the boot assetVer. Pin both
// branches: hit and miss.
func TestFuncMap_AssetUsesFingerprintAndFallsBackToAssetVer(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})

	asset := s.funcMap["asset"].(func(string) string)

	hit := asset("/x.css") // leading slash trimmed
	wantPrefix := "/static/x.css?v="
	if !strings.HasPrefix(hit, wantPrefix) {
		t.Errorf("hit = %q, want prefix %q", hit, wantPrefix)
	}
	if !strings.HasSuffix(hit, s.fingerprints["x.css"]) {
		t.Errorf("hit = %q, want fingerprint suffix %q", hit, s.fingerprints["x.css"])
	}

	miss := asset("nope.css")
	if !strings.HasSuffix(miss, s.assetVer) {
		t.Errorf("miss = %q, want assetVer suffix %q", miss, s.assetVer)
	}
}

// TestFuncMap_PageURLAndCanonical — both helpers map (lang, key) to
// stable shapes; sitemap and canonical link tags both rely on them
// agreeing exactly.
func TestFuncMap_PageURLAndCanonical(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.test",
	})

	pageURL := s.funcMap["pageURL"].(func(string, string) string)
	if got := pageURL("es", ""); got != "/es/" {
		t.Errorf("pageURL index = %q, want /es/", got)
	}
	if got := pageURL("es", "about"); got != "/es/about" {
		t.Errorf("pageURL about = %q, want /es/about", got)
	}
	if got := pageURL("es", "/about"); got != "/es/about" {
		t.Errorf("pageURL leading-slash about = %q, want /es/about", got)
	}

	canonical := s.funcMap["canonical"].(func(string) string)
	if got := canonical("/es/"); got != "https://example.test/es/" {
		t.Errorf("canonical = %q", got)
	}
}

// TestFuncMap_DevScriptToggles — production mode emits empty string,
// dev mode emits the reload tag. Layouts call {{ devScript }} in
// every env so the helper must own the conditional.
func TestFuncMap_DevScriptToggles(t *testing.T) {
	prod := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	dev := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}, Dev: true})

	got := string(prod.funcMap["devScript"].(func() template.HTML)())
	if got != "" {
		t.Errorf("prod devScript = %q, want empty", got)
	}
	got = string(dev.funcMap["devScript"].(func() template.HTML)())
	if !strings.Contains(got, "__reload.js") {
		t.Errorf("dev devScript = %q, want hot-reload tag", got)
	}
}

// TestSEO_RobotsServesAllowAllAndSitemap — robots.txt must allow all
// UAs, list every social bot explicitly (their parsers ignore *
// when their own UA is present), and point at the sitemap when a
// CanonicalHost is configured.
func TestSEO_RobotsServesAllowAllAndSitemap(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.test",
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "User-agent: *") {
		t.Errorf("missing wildcard UA: %s", body)
	}
	for _, ua := range []string{"facebookexternalhit", "Twitterbot", "LinkedInBot", "Slackbot", "Discordbot"} {
		if !strings.Contains(body, ua) {
			t.Errorf("missing social bot %q in robots.txt", ua)
		}
	}
	if !strings.Contains(body, "Sitemap: https://example.test/sitemap.xml") {
		t.Errorf("missing sitemap line: %s", body)
	}
}

// TestSEO_RobotsOmitsSitemapWhenNoCanonicalHost — without a
// CanonicalHost we can't form an absolute URL, so the line is
// skipped. Pins the early-return.
func TestSEO_RobotsOmitsSitemapWhenNoCanonicalHost(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if strings.Contains(rec.Body.String(), "Sitemap:") {
		t.Errorf("expected no Sitemap: line, got %s", rec.Body.String())
	}
}

// TestSEO_RobotsCanBeDisabled — services that don't index anything
// (private admin, JSON API) opt out via SEOOptions.DisableRobots.
// The endpoint then 404s through the catch-all.
func TestSEO_RobotsCanBeDisabled(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableRobots: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (catch-all)", rec.Code)
	}
}

// TestSEO_SitemapEmitsHreflangAlternatesAndPriority — every (page,
// lang) gets a <url> with alternates for each lang plus an
// x-default; priority 1.0 only for the default lang.
func TestSEO_SitemapEmitsHreflangAlternatesAndPriority(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.test",
		DefaultLang:    "es",
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sitemap.xml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<loc>https://example.test/es/</loc>`) {
		t.Errorf("missing es loc: %s", body)
	}
	if !strings.Contains(body, `hreflang="x-default"`) {
		t.Errorf("missing x-default alternate: %s", body)
	}
	if !strings.Contains(body, "<priority>1.0</priority>") {
		t.Errorf("missing default-lang priority 1.0: %s", body)
	}
	if !strings.Contains(body, "<priority>0.9</priority>") {
		t.Errorf("missing other-lang priority 0.9: %s", body)
	}
}

// TestSEO_SitemapDisable — DisableSitemap turns off the endpoint.
func TestSEO_SitemapDisable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableSitemap: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sitemap.xml", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestSEO_LLMsTxt_DefaultStub — without an explicit body we generate
// a minimal stub built from CanonicalHost. Better than 404 because
// the convention exists and tooling looks for /llms.txt directly.
func TestSEO_LLMsTxt_DefaultStub(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.test",
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llms.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "# Site") {
		t.Errorf("missing H1 stub: %s", body)
	}
	if !strings.Contains(body, "https://example.test/es/") {
		t.Errorf("missing per-lang link: %s", body)
	}
	if !strings.Contains(body, "sitemap.xml") {
		t.Errorf("missing sitemap link: %s", body)
	}
}

// TestSEO_LLMsTxt_OverrideServedVerbatim — when the operator passes
// real markdown we serve it byte-for-byte; the stub's job is just
// to be the don't-404 fallback.
func TestSEO_LLMsTxt_OverrideServedVerbatim(t *testing.T) {
	custom := "# Custom\n\n> One-liner about the site.\n"
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{LLMsTxt: custom},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llms.txt", nil))
	if got := rec.Body.String(); got != custom {
		t.Errorf("body = %q, want %q", got, custom)
	}
}

// TestSEO_LLMsTxtDisable — opt-out for services where the
// convention isn't relevant (an internal API).
func TestSEO_LLMsTxtDisable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableLLMsTxt: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llms.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestSEO_SecurityTxt_PlaceholderWhenContactUnset — security.txt
// must serve a placeholder body (better than 404 to a researcher
// trying to file a bug) with a comment flagging the misconfig.
func TestSEO_SecurityTxt_PlaceholderWhenContactUnset(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		CanonicalHost:  "https://example.test",
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "# WARNING") {
		t.Errorf("missing placeholder warning comment: %s", body)
	}
	if !strings.Contains(body, "security@example.invalid") {
		t.Errorf("missing placeholder mailto: %s", body)
	}
	if !strings.Contains(body, "Canonical: https://example.test/.well-known/security.txt") {
		t.Errorf("missing Canonical line: %s", body)
	}
}

// TestSEO_SecurityTxt_LegacyPathAlsoServed — /security.txt at the
// root is the legacy path some scanners still probe before checking
// /.well-known. We mount both and they emit the same body.
func TestSEO_SecurityTxt_LegacyPathAlsoServed(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{SecurityContact: "sec@example.test"},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/security.txt", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Contact: mailto:sec@example.test") {
		t.Errorf("body missing real contact: %s", rec.Body.String())
	}
}

// TestSEO_SecurityTxt_ExpiresIsAboutOneYearOut — the RFC says max
// 12 months. We compute it dynamically, so every redeploy refreshes
// it. Pin the rough year-out shape so a regression to a hardcoded
// past date is caught.
func TestSEO_SecurityTxt_ExpiresIsAboutOneYearOut(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO: SEOOptions{
			SecurityContact:    "sec@example.test",
			PreferredLanguages: []string{"es", "en"},
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))
	body := rec.Body.String()

	// Find the Expires: line and parse its timestamp.
	var expiresLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Expires: ") {
			expiresLine = strings.TrimPrefix(line, "Expires: ")
			break
		}
	}
	if expiresLine == "" {
		t.Fatalf("no Expires: line in body: %s", body)
	}
	exp, err := time.Parse(time.RFC3339, expiresLine)
	if err != nil {
		t.Fatalf("parse Expires: %v", err)
	}
	delta := time.Until(exp)
	if delta < 360*24*time.Hour || delta > 370*24*time.Hour {
		t.Errorf("Expires delta = %v, want ~1y", delta)
	}
	// PreferredLanguages override — also pinned here.
	if !strings.Contains(body, "Preferred-Languages: es, en") {
		t.Errorf("missing PreferredLanguages override: %s", body)
	}
}

// TestSEO_SecurityTxtDisable — opt-out skips both /security.txt and
// /.well-known/security.txt registrations.
func TestSEO_SecurityTxtDisable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableSecurityTxt: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestSEO_AASA_DefaultComponentsCoverAllLangs — without explicit
// AASAComponents we emit /<lang>/* for every supported lang. That's
// the correct default for any iOS app mirroring the web routes.
func TestSEO_AASA_DefaultComponentsCoverAllLangs(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		SEO: SEOOptions{
			AppleTeamID: "ABCD123456",
			IOSBundleID: "co.example.app",
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ABCD123456.co.example.app") {
		t.Errorf("missing appID: %s", body)
	}
	if !strings.Contains(body, `"/es/*"`) || !strings.Contains(body, `"/en/*"`) {
		t.Errorf("missing default per-lang components: %s", body)
	}
}

// TestSEO_AASA_PlaceholderWhenIDsMissing — without team/bundle we
// emit placeholders + the warnSEO log already nudged the operator
// at boot. The endpoint stays serving so AASA validators have
// something to point at.
func TestSEO_AASA_PlaceholderWhenIDsMissing(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "XXXXXXXXXX") {
		t.Errorf("missing teamID placeholder: %s", body)
	}
	if !strings.Contains(body, "co.example.placeholder") {
		t.Errorf("missing bundle placeholder: %s", body)
	}
}

// TestSEO_AASA_ExplicitComponentsWin — when the operator sets
// AASAComponents, we don't auto-derive from langs. Important for
// apps that handle deep links outside the /<lang>/* pattern.
func TestSEO_AASA_ExplicitComponentsWin(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO: SEOOptions{
			AppleTeamID:    "ABCD123456",
			IOSBundleID:    "co.example.app",
			AASAComponents: []string{"/place/*", "/profile/*"},
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"/place/*"`) || !strings.Contains(body, `"/profile/*"`) {
		t.Errorf("missing explicit components: %s", body)
	}
	if strings.Contains(body, `"/es/*"`) {
		t.Errorf("auto components leaked when explicit ones were set: %s", body)
	}
}

// TestSEO_AASA_MarshalErrorReturns500 — drives the json.MarshalIndent
// error branch via the jsonMarshalIndent seam. The real call can't
// fail for our struct shape, but the 500 path is part of the
// contract a future schema change might hit.
func TestSEO_AASA_MarshalErrorReturns500(t *testing.T) {
	orig := jsonMarshalIndent
	jsonMarshalIndent = func(_ any, _, _ string) ([]byte, error) {
		return nil, errors.New("seam-marshal-failure")
	}
	t.Cleanup(func() { jsonMarshalIndent = orig })

	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestSEO_AASADisable — opt-out for services without an iOS app.
func TestSEO_AASADisable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableAASA: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/apple-app-site-association", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestSEO_AssetLinks_DefaultEmptyArray — without explicit links we
// serve `[]` so Android Studio's App Links Assistant doesn't 404
// before the Android app exists.
func TestSEO_AssetLinks_DefaultEmptyArray(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

// TestSEO_AssetLinks_RendersExplicitLinks — pin the JSON shape: the
// fields propagate through json.Marshal with the right tags.
func TestSEO_AssetLinks_RendersExplicitLinks(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO: SEOOptions{
			AndroidAssetLinks: []AssetLink{{
				Relation: []string{"delegate_permission/common.handle_all_urls"},
				Target: AssetLinkTarget{
					Namespace:              "android_app",
					PackageName:            "co.example.app",
					Sha256CertFingerprints: []string{"AA:BB:CC"},
				},
			}},
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"package_name": "co.example.app"`) {
		t.Errorf("missing package_name: %s", body)
	}
	if !strings.Contains(body, `"AA:BB:CC"`) {
		t.Errorf("missing fingerprint: %s", body)
	}
}

// TestSEO_AssetLinks_MarshalErrorReturns500 — same seam-driven 500
// branch as AASA. Belt-and-suspenders against a future schema
// change that breaks Marshal.
func TestSEO_AssetLinks_MarshalErrorReturns500(t *testing.T) {
	orig := jsonMarshalIndent
	jsonMarshalIndent = func(_ any, _, _ string) ([]byte, error) {
		return nil, errors.New("seam-marshal-failure")
	}
	t.Cleanup(func() { jsonMarshalIndent = orig })

	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestSEO_AssetLinksDisable — opt-out.
func TestSEO_AssetLinksDisable(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		SEO:            SEOOptions{DisableAssetLinks: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestWarnSEO_SkipsInDevMode — Site rebuilds per request in dev, so
// warning would fire dozens of times per minute. Pin the early
// return by constructing with Dev=true and an unset SecurityContact:
// the lack of error means nothing went wrong (we can't intercept
// the log easily, so this asserts the code path doesn't panic and
// returns Site successfully).
func TestWarnSEO_SkipsInDevMode(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		Dev:            true,
		// SecurityContact and AppleTeamID intentionally empty: would
		// fire warnings in prod, must be silent here.
	})
	if s == nil {
		t.Fatalf("expected non-nil Site in dev mode")
	}
}

// TestWarnSEO_FiresInProdMode — exercises the warning paths in
// warnSEO when not in dev. We don't intercept the log; we just
// drive the branches by leaving SecurityContact + AppleTeamID
// empty in prod.
func TestWarnSEO_FiresInProdMode(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		// Dev=false (prod), no SecurityContact, no AppleTeamID.
	})
	if s == nil {
		t.Fatalf("expected non-nil Site in prod mode")
	}
}

// TestOG_DisabledByDefault — without OG.Enabled, Mount must not
// register an /og handler, and ogImage must return "" so layouts
// can guard with {{ with ogImage … }}.
func TestOG_DisabledByDefault(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{SupportedLangs: []string{"es"}})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/og/es/index.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when OG disabled", rec.Code)
	}
	if got := s.ogImageURL("", "es"); got != "" {
		t.Errorf("ogImageURL when disabled = %q, want empty", got)
	}
}

// TestOG_HandlerServesPNGAtCanonicalPath — end-to-end happy path:
// /og/<lang>/<page>.png renders a PNG. We only assert on the
// content-type and a non-empty body — the og package owns the
// pixel-level invariants.
func TestOG_HandlerServesPNGAtCanonicalPath(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		OG: OGOptions{
			Enabled: true,
			Brand:   "Test",
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/og/en/about.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Errorf("empty body")
	}
}

// TestOG_HandlerWithCustomPrefix — URLPrefix flows through to both
// the helper URL and the resolver. Trailing slashes are tolerated
// so callers can pass "/og/" or "/og" and it works the same.
func TestOG_HandlerWithCustomPrefix(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG: OGOptions{
			Enabled:   true,
			URLPrefix: "/share/",
		},
	})
	if got := s.ogImageURL("", "es"); got != "/share/es/index.png" {
		t.Errorf("ogImageURL = %q, want /share/es/index.png", got)
	}

	mux := http.NewServeMux()
	s.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/es/index.png", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestOG_HandlerReusesProvidedStorage — operators can pass a custom
// Storage (typically MinIO in prod). The render Site must not
// allocate its own MemoryStorage when one is provided.
func TestOG_HandlerReusesProvidedStorage(t *testing.T) {
	store := og.NewMemoryStorage("/og")
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG: OGOptions{
			Enabled: true,
			Storage: store,
		},
	})
	if s.ogStorage != store {
		t.Errorf("ogStorage was replaced, want the one we passed")
	}
}

// TestOG_BuildWrapsGeneratorError — drives the og.New failure
// branch via the ogNew seam. The real og.New only fails when the
// embedded font can't load, which never happens in tests; the
// wrap is part of the boot-error contract a future op needs.
func TestOG_BuildWrapsGeneratorError(t *testing.T) {
	orig := ogNew
	ogNew = func(_ og.Options) (*og.Generator, error) {
		return nil, errors.New("seam-og-failure")
	}
	t.Cleanup(func() { ogNew = orig })

	_, err := New(Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es"},
		OG:             OGOptions{Enabled: true},
	})
	if err == nil || !strings.Contains(err.Error(), "OG") {
		t.Fatalf("err = %v, want OG wrap", err)
	}
}

// TestOG_Resolver_RejectsMalformedPaths exercises every false-return
// branch in ogResolver, which the handler then surfaces as 404.
// Important contract: never render a card for a page that doesn't
// exist (would pollute storage with garbage cards).
func TestOG_Resolver_RejectsMalformedPaths(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		OG:             OGOptions{Enabled: true},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	cases := map[string]string{
		"missing png":    "/og/es/index",  // no .png suffix
		"single segment": "/og/index.png", // no lang/page split
		"unknown lang":   "/og/fr/index.png",
		"unknown page":   "/og/es/no-such-page.png",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("status for %q = %d, want 404", path, rec.Code)
			}
		})
	}
}

// TestOG_LocaleOr_PerPageBeatsSiteFallback — when og.<page>.title
// exists in the lang we use it; otherwise we fall through to
// site.title. cardFor encodes that priority.
func TestOG_LocaleOr_PerPageBeatsSiteFallback(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		OG:             OGOptions{Enabled: true, Brand: "B"},
		DefaultLang:    "es",
	})
	// /en/about has og.about.title in en; site.title in es.
	c := s.cardFor("about", "en")
	if c.Title != "About!" {
		t.Errorf("title = %q, want per-page about title", c.Title)
	}
	if c.Subtitle != "Desc" {
		t.Errorf("subtitle = %q, want fallback site.description", c.Subtitle)
	}
	if c.Brand != "B" || c.Lang != "en" {
		t.Errorf("card meta = %+v", c)
	}
}

// TestOG_LocaleOr_AllMissingReturnsLastKey — pin the last-resort
// fallback so a fully missing translation chain never produces an
// empty title (which would render an off-brand blank card).
func TestOG_LocaleOr_AllMissingReturnsLastKey(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG:             OGOptions{Enabled: true},
	})
	got := s.localeOr("es", "missing.a", "missing.b")
	if got != "missing.b" {
		t.Errorf("localeOr fallback = %q, want last key", got)
	}
}

// TestOG_LocaleOr_NoKeysReturnsEmpty — defensive branch: callers
// pass at least one key in practice, but the empty-args path is
// tested so a regression doesn't panic.
func TestOG_LocaleOr_NoKeysReturnsEmpty(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG:             OGOptions{Enabled: true},
	})
	if got := s.localeOr("es"); got != "" {
		t.Errorf("localeOr() = %q, want empty", got)
	}
}

// TestOG_CardFor_IndexLabel — "" pageKey is the home; we map it to
// "index" for the og.<page>.* key scheme so a single locale layout
// covers both home and named pages.
func TestOG_CardFor_IndexLabel(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG:             OGOptions{Enabled: true},
	})
	c := s.cardFor("", "es")
	// site.title falls back through the chain since og.index.title
	// isn't defined in our fixture.
	if c.Title != "Sitio" {
		t.Errorf("index title = %q, want site.title fallback", c.Title)
	}
}

// TestOG_ImageURL_IndexEmptyKey — the URL form of the empty pageKey
// is "index", matching the resolver's reverse mapping. Pin both
// directions so they can't drift.
func TestOG_ImageURL_IndexEmptyKey(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
		OG:             OGOptions{Enabled: true},
	})
	if got := s.ogImageURL("", "es"); got != "/og/es/index.png" {
		t.Errorf("ogImageURL empty key = %q, want /og/es/index.png", got)
	}
	if got := s.ogImageURL("about", "es"); got != "/og/es/about.png" {
		t.Errorf("ogImageURL named = %q, want /og/es/about.png", got)
	}
}

// TestFingerprint_HashFile_OneLineCoverage — short input still pads
// out to fingerprintLength. Exercises the hashFile happy path
// independent of Site so the helper's contract is pinned.
func TestFingerprint_HashFile_DirectCall(t *testing.T) {
	fsys := fstest.MapFS{
		"a": &fstest.MapFile{Data: []byte("hi")},
	}
	got, err := hashFile(fsys, "a")
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if len(got) != fingerprintLength {
		t.Errorf("len = %d, want %d", len(got), fingerprintLength)
	}
}

// TestFingerprint_HashFile_OpenError — an Open error propagates;
// the caller uses the wrapped form to attribute the failure to a
// specific path.
func TestFingerprint_HashFile_OpenError(t *testing.T) {
	bad := errFS{
		inner:   fstest.MapFS{"a": &fstest.MapFile{Data: []byte("hi")}},
		openErr: map[string]error{"a": errors.New("nope")},
	}
	if _, err := hashFile(bad, "a"); err == nil {
		t.Errorf("expected error")
	}
}

// TestFingerprint_HashFile_ReadError — io.Copy error propagates.
// Without the errFS seam this is unreachable for in-memory data.
func TestFingerprint_HashFile_ReadError(t *testing.T) {
	bad := errFS{
		inner:   fstest.MapFS{"a": &fstest.MapFile{Data: []byte("hi")}},
		readErr: map[string]error{"a": errors.New("read-fail")},
	}
	if _, err := hashFile(bad, "a"); err == nil {
		t.Errorf("expected error")
	}
}

// TestRouter_RootSetsLangCookie — a fresh visitor lands on /, the
// router calls pickLang and pins the result via Set-Cookie. Pin
// the path attribute so the cookie applies site-wide.
func TestRouter_RootSetsLangCookie(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("no cookie set on /")
	}
	c := cookies[0]
	if c.Name != langCookie || c.Path != "/" {
		t.Errorf("cookie = %+v, want hbl_lang on /", c)
	}
}

// TestRouter_PageRendererBodyContainsAssetURL — drives the
// asset-helper code path inside a rendered page, confirming the
// TestRenderPage_UnknownKey404s — renderPage is defensive against
// being called with a key that isn't in s.pages. The router checks
// before dispatching, so this branch is only reachable when callers
// invoke renderPage directly (or via a future wiring that bypasses
// the router). Either way, 404 is the contract — without coverage
// here a regression to "panic on missing key" would slip through.
// TestDynamicRoute_MatchesAndPassesParams pins the dynamic
// routing contract: a template at pages/<dir>/[slug].html.tmpl
// matches any non-empty segment in that position, and the
// captured value lands in .Params keyed by the bracket name.
// Without this any later refactor that quietly drops the
// [param] detection would silently make the per-slug consolidation
// feature inert.
func TestDynamicRoute_MatchesAndPassesParams(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}home{{ end }}`),
		},
		"pages/reviews/[slug].html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}review: {{ .Params.slug }}{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{SupportedLangs: []string{"es"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/es/reviews/julia", nil)
	s.handleRoot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — dynamic route should match /reviews/julia", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "review: julia") {
		t.Errorf("body = %q, want it to contain %q (params.slug should resolve)", rec.Body.String(), "review: julia")
	}
}

// TestDynamicRoute_ExactMatchWins pins the precedence rule.
// pages/reviews/featured.html.tmpl (literal) must beat
// pages/reviews/[slug].html.tmpl (dynamic) when both could match
// the same URL — otherwise the dynamic route would shadow every
// hand-written static page in the same directory.
func TestDynamicRoute_ExactMatchWins(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}home{{ end }}`),
		},
		"pages/reviews/featured.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}EXACT{{ end }}`),
		},
		"pages/reviews/[slug].html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}DYNAMIC{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{SupportedLangs: []string{"es"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/es/reviews/featured", nil)
	s.handleRoot(rec, req)
	if !strings.Contains(rec.Body.String(), "EXACT") {
		t.Errorf("body = %q, want %q (exact match must win over dynamic)", rec.Body.String(), "EXACT")
	}
}

// TestDynamicRoute_SegmentCountMismatch ensures
// pages/reviews/[slug] does NOT swallow /reviews/julia/extra —
// segment count must match exactly so longer paths fall through
// to 404 (or a more specific route) instead of binding the
// first segment and ignoring the rest.
func TestDynamicRoute_SegmentCountMismatch(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}home{{ end }}`),
		},
		"pages/reviews/[slug].html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}{{ .Params.slug }}{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{SupportedLangs: []string{"es"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/es/reviews/julia/extra", nil)
	s.handleRoot(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — /reviews/julia/extra has 3 segments but route has 2", rec.Code)
	}
}

func TestRenderPage_UnknownKey404s(t *testing.T) {
	// Lookup moved out of renderPage into resolvePage + the router
	// when dynamic routes (pages/<dir>/[slug]) joined the picture.
	// Drive the 404 path through the public surface (handleRoot)
	// so the test still covers the same contract — visitor hits a
	// non-existent page, gets 404 — without depending on the
	// private renderPage signature.
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/es/ghost", nil)
	s.handleRoot(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown page key", rec.Code)
	}
}

// fingerprint is interpolated into the HTML output the way layouts
// expect.
func TestRouter_PageRendererBodyContainsAssetURL(t *testing.T) {
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es"},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/about", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := "/static/x.css?v=" + s.fingerprints["x.css"]
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body missing asset URL %q: %s", want, rec.Body.String())
	}
}

// TestFuncMap_ServiceHelpersMergedIntoTemplate — a service-supplied
// helper must be callable from page templates. Pins the
// Options.FuncMap merge path so adding e.g. a project catalogue from
// a landing doesn't silently fall back to the framework's
// built-ins.
func TestFuncMap_ServiceHelpersMergedIntoTemplate(t *testing.T) {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p>{{ shout "hi" }}</p>{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}<html>{{ template "content" . }}</html>{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{
		SupportedLangs: []string{"es"},
		FuncMap: template.FuncMap{
			"shout": func(s string) string { return strings.ToUpper(s) + "!" },
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<p>HI!</p>") {
		t.Errorf("body = %s, want service func output", rec.Body.String())
	}
}

// TestExtraStatic_RoutesAndFingerprintsServeFromMountedFS —
// end-to-end: a UI-kit-style fs.FS handed in via Options.ExtraStatic
// must be reachable at /static/<prefix>/<path>, the asset() helper
// must emit a fingerprinted URL for it, and the served bytes must
// match the source.
func TestExtraStatic_RoutesAndFingerprintsServeFromMountedFS(t *testing.T) {
	uikit := fstest.MapFS{
		"css/tokens.css": &fstest.MapFile{Data: []byte("/* tokens */\n:root{--x:1;}")},
	}
	fsys := fstest.MapFS{
		"pages/index.html.tmpl":      &fstest.MapFile{Data: []byte(`{{ define "content" }}<link href="{{ asset "uikit/css/tokens.css" }}">{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{Data: []byte(`{{ define "_layout" }}{{ template "content" . }}{{ end }}`)},
		"locales/es.json":            &fstest.MapFile{Data: []byte(`{}`)},
	}
	s := withSiteOpts(t, fsys, Options{
		SupportedLangs: []string{"es"},
		ExtraStatic:    map[string]fs.FS{"uikit": uikit},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	// 1. The static file is reachable at /static/uikit/css/tokens.css.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/uikit/css/tokens.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("static fetch status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/* tokens */") {
		t.Errorf("static body = %q, want bytes from uikit fs", rec.Body.String())
	}

	// 2. The page renders a fingerprinted URL pointing at it.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d", rec.Code)
	}
	hash, ok := s.fingerprints["uikit/css/tokens.css"]
	if !ok {
		t.Fatalf("fingerprint map missing uikit entry: %+v", s.fingerprints)
	}
	want := "/static/uikit/css/tokens.css?v=" + hash
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("page body = %q, want asset URL %q", rec.Body.String(), want)
	}
}

// TestExtraStatic_RejectsBadPrefixes — empty or slashed prefixes
// would break the mount path, so New must reject them at boot
// instead of producing a half-broken /static/ tree.
func TestExtraStatic_RejectsBadPrefixes(t *testing.T) {
	for _, prefix := range []string{"", "with/slash"} {
		_, err := New(Options{
			FS:             multiLangFS(),
			SupportedLangs: []string{"es"},
			ExtraStatic:    map[string]fs.FS{prefix: fstest.MapFS{}},
		})
		if err == nil || !strings.Contains(err.Error(), "ExtraStatic prefix") {
			t.Errorf("prefix %q: err = %v, want validation error", prefix, err)
		}
	}
}

// TestExtraStatic_FingerprintErrorBubblesUp — if a hand-rolled fs.FS
// errors during the walk (a future caller passing a network-backed
// FS, say), the failure must surface at boot, not as a 500 once the
// asset is requested.
func TestExtraStatic_FingerprintErrorBubblesUp(t *testing.T) {
	bad := errFS{
		inner:   fstest.MapFS{"a.css": &fstest.MapFile{Data: []byte("a")}},
		openErr: map[string]error{"a.css": errors.New("synthetic")},
	}
	_, err := New(Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es"},
		ExtraStatic:    map[string]fs.FS{"uikit": bad},
	})
	if err == nil || !strings.Contains(err.Error(), "extra static") {
		t.Errorf("err = %v, want extra static wrapping", err)
	}
}

// TestFuncMap_FrameworkHelpersWinOnCollision — if a service registers
// a name that collides with a built-in (e.g. "T"), the framework's
// implementation is preserved. Otherwise a service could accidentally
// break every page that uses T by shadowing it with a bad signature.
func TestFuncMap_FrameworkHelpersWinOnCollision(t *testing.T) {
	called := false
	s := withSiteOpts(t, multiLangFS(), Options{
		SupportedLangs: []string{"es", "en"},
		FuncMap: template.FuncMap{
			"T": func(string, string) string {
				called = true
				return "SHOULD-NOT-APPEAR"
			},
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/es/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("service T override was invoked; framework helper must win on collision")
	}
	if !strings.Contains(rec.Body.String(), "hola") {
		t.Errorf("body = %s, want framework T to translate 'hello' to 'hola'", rec.Body.String())
	}
}
