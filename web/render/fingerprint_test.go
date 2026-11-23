package render

import (
	"strings"
	"testing"
	"testing/fstest"
)

// minimalSiteFS returns the smallest FS that satisfies render.New —
// one page, one layout, one locale, plus whatever static files the
// caller wants to fingerprint.
func minimalSiteFS(static map[string][]byte) fstest.MapFS {
	fsys := fstest.MapFS{
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p>hi</p>{{ end }}`),
		},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}<html>{{ template "content" . }}</html>{{ end }}`),
		},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	for path, data := range static {
		fsys["static/"+path] = &fstest.MapFile{Data: data}
	}
	return fsys
}

func newSiteForTest(t *testing.T, fsys fstest.MapFS) *Site {
	t.Helper()
	s, err := New(Options{
		FS:             fsys,
		SupportedLangs: []string{"es"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestFingerprint_HashesEveryStaticFile checks that the walker
// covers every file under static/, regardless of extension or depth.
func TestFingerprint_HashesEveryStaticFile(t *testing.T) {
	s := newSiteForTest(t, minimalSiteFS(map[string][]byte{
		"css/base.css":        []byte("body{color:red}"),
		"js/app.js":           []byte("console.log('hi')"),
		"img/hero.jpg":        {0x89, 0x50, 0x4E, 0x47}, // bytes are arbitrary
		"fonts/inter.woff2":   []byte("opaque-binary-blob"),
		"data/regions.json":   []byte(`{"de":["HH","BE"]}`),
		"deep/nested/dir.svg": []byte(`<svg/>`),
	}))

	want := []string{
		"css/base.css",
		"js/app.js",
		"img/hero.jpg",
		"fonts/inter.woff2",
		"data/regions.json",
		"deep/nested/dir.svg",
	}
	for _, k := range want {
		h, ok := s.fingerprints[k]
		if !ok {
			t.Errorf("fingerprints missing %q", k)
			continue
		}
		if len(h) != fingerprintLength {
			t.Errorf("hash for %q has length %d, want %d", k, len(h), fingerprintLength)
		}
	}
}

// TestFingerprint_DifferentContentDifferentHash is the core
// invariant: two different files must end up with different hashes
// or cache busting is broken.
func TestFingerprint_DifferentContentDifferentHash(t *testing.T) {
	s := newSiteForTest(t, minimalSiteFS(map[string][]byte{
		"a.css": []byte("body{color:red}"),
		"b.css": []byte("body{color:blue}"),
	}))

	if s.fingerprints["a.css"] == s.fingerprints["b.css"] {
		t.Errorf("hashes collided: a=%s b=%s", s.fingerprints["a.css"], s.fingerprints["b.css"])
	}
}

// TestFingerprint_StableAcrossInstances verifies the property that
// makes the design work without coordination: two pods serving the
// same binary emit the same URL for the same content.
func TestFingerprint_StableAcrossInstances(t *testing.T) {
	fsys := minimalSiteFS(map[string][]byte{
		"css/base.css": []byte("body{color:red}"),
	})

	s1 := newSiteForTest(t, fsys)
	s2 := newSiteForTest(t, fsys)

	if s1.fingerprints["css/base.css"] != s2.fingerprints["css/base.css"] {
		t.Errorf("hash unstable: %s vs %s",
			s1.fingerprints["css/base.css"], s2.fingerprints["css/base.css"])
	}
}

// TestFingerprint_AssetHelperUsesFingerprint pins down the contract
// the asset() template helper exposes to layouts: known paths get
// the content hash, unknown paths fall back to assetVer so dev-mode
// races and runtime-added files still bust on redeploy.
func TestFingerprint_AssetHelperUsesFingerprint(t *testing.T) {
	s := newSiteForTest(t, minimalSiteFS(map[string][]byte{
		"css/base.css": []byte("body{color:red}"),
	}))

	asset, ok := s.funcMap["asset"].(func(string) string)
	if !ok {
		t.Fatalf("funcMap[asset] is %T, want func(string) string", s.funcMap["asset"])
	}

	got := asset("css/base.css")
	want := "/static/css/base.css?v=" + s.fingerprints["css/base.css"]
	if got != want {
		t.Errorf("known asset URL = %q, want %q", got, want)
	}

	got = asset("css/missing.css")
	prefix := "/static/css/missing.css?v="
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("missing asset URL = %q, want prefix %q", got, prefix)
	}
	if strings.TrimPrefix(got, prefix) == s.fingerprints["css/base.css"] {
		t.Errorf("missing asset reused another file's hash: %q", got)
	}
}

// TestFingerprint_EmptyStaticDirOK lets services that don't ship any
// embedded static files (a JSON API, a NATS consumer using render
// only for its few error pages) boot without errors.
func TestFingerprint_EmptyStaticDirOK(t *testing.T) {
	s := newSiteForTest(t, minimalSiteFS(nil))
	if len(s.fingerprints) != 0 {
		t.Errorf("expected empty fingerprints map, got %v", s.fingerprints)
	}
}
