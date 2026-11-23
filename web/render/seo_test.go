package render

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// updateSEOGolden writes the actual template output to testdata/seo/*
// instead of comparing. Run via:
//
//	go test ./web/render/ -update-seo -run TestSEO
//
// Inspect the diff (`git diff web/render/testdata`), then commit
// when the new shape is what you intended.
var updateSEOGolden = flag.Bool("update-seo", false, "write SEO golden files to testdata/seo")

// fixedSEONow pins time.Now in buildSEO so Expires stays stable across
// runs and across calendars. Picked the project's MongoDB pivot date
// as a marker — irrelevant to the assertion, just memorable.
var fixedSEONow = time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

// seoFreezeTime installs the fixed clock for the test's lifetime and
// restores the real one via t.Cleanup. Use in every SEO test so a
// failing assertion never blames the calendar.
func seoFreezeTime(t *testing.T) {
	t.Helper()
	orig := seoNow
	seoNow = func() time.Time { return fixedSEONow }
	t.Cleanup(func() { seoNow = orig })
}

// seoTestOpts is the canonical SEO Options shape: two pages
// (multiLangFS), two langs, default = es, brand pinned. Per-test
// tweaks pass an SEOOptions override + canonical host.
func seoTestOpts(seo SEOOptions, host string) Options {
	return Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		DefaultLang:    "es",
		CanonicalHost:  host,
		Brand:          "TestBrand",
		SEO:            seo,
	}
}

// seoServe routes one GET request through s.mountSEO and returns the
// response + body. Existing tests in render_test.go work at the
// Mount() level; we go through mountSEO directly here so a typo in
// path registration surfaces as a clear "got 404" instead of being
// masked by the catch-all router.
func seoServe(t *testing.T, s *Site, path string) (*http.Response, []byte) {
	t.Helper()
	mux := http.NewServeMux()
	s.mountSEO(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// goldenPath returns the path under testdata/seo. testdata/ is the
// canonical Go convention for fixture files — the toolchain skips it
// during build/vet.
func goldenPath(name string) string {
	return filepath.Join("testdata", "seo", name)
}

// compareGolden diffs `got` against testdata/seo/<name>. With
// -update-seo, it writes `got` to the file instead and skips the
// compare — useful after a deliberate template edit.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := goldenPath(name)
	if *updateSEOGolden {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", p, err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v (re-run with -update-seo to create)", p, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch for %s\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

// swapSEOExecute installs a fake template executor that returns the
// given error for the rest of the test. Used to drive the 500 branch
// in each serve* handler without contriving a template that fails on
// real data (which the static embedded templates can't).
func swapSEOExecute(t *testing.T, err error) {
	t.Helper()
	orig := seoExecute
	seoExecute = func(_ *template.Template, _ io.Writer, _ any) error { return err }
	t.Cleanup(func() { seoExecute = orig })
}

// swapJSONMarshalIndent does the same for the JSON-backed AASA /
// assetlinks endpoints. The real `json.MarshalIndent` of our doc
// shapes never fails, so this seam is the only path to the 500 branch.
func swapJSONMarshalIndent(t *testing.T, err error) {
	t.Helper()
	orig := jsonMarshalIndent
	jsonMarshalIndent = func(_ any, _, _ string) ([]byte, error) { return nil, err }
	t.Cleanup(func() { jsonMarshalIndent = orig })
}

// ---- robots.txt ----------------------------------------------------

func TestSEO_Robots_Golden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, body := seoServe(t, s, "/robots.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "86400") {
		t.Fatalf("cache-control = %q (want 24h max-age)", cc)
	}
	compareGolden(t, "robots.txt", body)
}

func TestSEO_Robots_NoCanonicalHost_Golden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, ""))
	_, body := seoServe(t, s, "/robots.txt")
	compareGolden(t, "robots-no-canonical.txt", body)
}

func TestSEO_Robots_TemplateExecuteError(t *testing.T) {
	seoFreezeTime(t)
	swapSEOExecute(t, errors.New("boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, _ := seoServe(t, s, "/robots.txt")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- sitemap.xml ---------------------------------------------------

func TestSEO_Sitemap_Golden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, body := seoServe(t, s, "/sitemap.xml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "3600") {
		t.Fatalf("cache-control = %q (want 1h max-age)", cc)
	}
	compareGolden(t, "sitemap.xml", body)
}

func TestSEO_Sitemap_TemplateExecuteError(t *testing.T) {
	seoFreezeTime(t)
	swapSEOExecute(t, errors.New("boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, _ := seoServe(t, s, "/sitemap.xml")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- llms.txt ------------------------------------------------------

func TestSEO_LLMs_StubGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, body := seoServe(t, s, "/llms.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type = %q", ct)
	}
	compareGolden(t, "llms.txt", body)
}

// TestSEO_LLMs_CustomBody pins the behaviour that a non-empty
// SEOOptions.LLMsTxt is served verbatim — the user wrote markdown,
// we shouldn't second-guess it with the stub template.
func TestSEO_LLMs_CustomBody(t *testing.T) {
	seoFreezeTime(t)
	custom := "# My Custom\n\n> Hand-written.\n"
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{LLMsTxt: custom}, "https://example.com"))
	_, body := seoServe(t, s, "/llms.txt")
	if string(body) != custom {
		t.Fatalf("body = %q, want %q", body, custom)
	}
}

func TestSEO_LLMs_TemplateExecuteError(t *testing.T) {
	seoFreezeTime(t)
	swapSEOExecute(t, errors.New("boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	resp, _ := seoServe(t, s, "/llms.txt")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- security.txt --------------------------------------------------

func TestSEO_SecurityTxt_WithContact_Golden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		SecurityContact:    "sec@example.com",
		PreferredLanguages: []string{"es", "en"},
	}, "https://example.com"))
	resp, body := seoServe(t, s, "/.well-known/security.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	compareGolden(t, "security.txt", body)
}

func TestSEO_SecurityTxt_LegacyPath(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		SecurityContact: "sec@example.com",
	}, "https://example.com"))
	resp, _ := seoServe(t, s, "/security.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy /security.txt status = %d, want 200", resp.StatusCode)
	}
}

func TestSEO_SecurityTxt_PlaceholderGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, "https://example.com"))
	_, body := seoServe(t, s, "/.well-known/security.txt")
	if !bytes.Contains(body, []byte("WARNING")) {
		t.Fatalf("body missing WARNING placeholder:\n%s", body)
	}
	compareGolden(t, "security-placeholder.txt", body)
}

func TestSEO_SecurityTxt_NoCanonicalHost_Golden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		SecurityContact: "sec@example.com",
	}, ""))
	_, body := seoServe(t, s, "/.well-known/security.txt")
	if bytes.Contains(body, []byte("Canonical:")) {
		t.Fatalf("body has Canonical: line without CanonicalHost set:\n%s", body)
	}
	compareGolden(t, "security-no-canonical.txt", body)
}

func TestSEO_SecurityTxt_TemplateExecuteError(t *testing.T) {
	seoFreezeTime(t)
	swapSEOExecute(t, errors.New("boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		SecurityContact: "sec@example.com",
	}, "https://example.com"))
	resp, _ := seoServe(t, s, "/.well-known/security.txt")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- AASA ----------------------------------------------------------

func TestSEO_AASA_DefaultsGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, ""))
	resp, body := seoServe(t, s, "/.well-known/apple-app-site-association")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	compareGolden(t, "aasa-defaults.json", body)
}

func TestSEO_AASA_WithRealConfigGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		AppleTeamID:    "ABCD123456",
		IOSBundleID:    "co.hblabs.app",
		AASAComponents: []string{"/auth/*", "/share/*"},
	}, ""))
	_, body := seoServe(t, s, "/.well-known/apple-app-site-association")
	compareGolden(t, "aasa-configured.json", body)
}

func TestSEO_AASA_MarshalError(t *testing.T) {
	seoFreezeTime(t)
	swapJSONMarshalIndent(t, errors.New("marshal boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, ""))
	resp, _ := seoServe(t, s, "/.well-known/apple-app-site-association")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- assetlinks.json -----------------------------------------------

func TestSEO_AssetLinks_EmptyGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, ""))
	resp, body := seoServe(t, s, "/.well-known/assetlinks.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	compareGolden(t, "assetlinks-empty.json", body)
}

func TestSEO_AssetLinks_WithEntriesGolden(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		AndroidAssetLinks: []AssetLink{{
			Relation: []string{"delegate_permission/common.handle_all_urls"},
			Target: AssetLinkTarget{
				Namespace:              "android_app",
				PackageName:            "co.hblabs.app",
				Sha256CertFingerprints: []string{"AB:CD:EF"},
			},
		}},
	}, ""))
	_, body := seoServe(t, s, "/.well-known/assetlinks.json")
	compareGolden(t, "assetlinks-configured.json", body)
}

func TestSEO_AssetLinks_MarshalError(t *testing.T) {
	seoFreezeTime(t)
	swapJSONMarshalIndent(t, errors.New("marshal boom"))
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{}, ""))
	resp, _ := seoServe(t, s, "/.well-known/assetlinks.json")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// ---- mount disable flags -------------------------------------------

func TestSEO_Mount_AllDisabled(t *testing.T) {
	seoFreezeTime(t)
	s := withSiteOpts(t, nil, seoTestOpts(SEOOptions{
		DisableRobots:      true,
		DisableSitemap:     true,
		DisableLLMsTxt:     true,
		DisableSecurityTxt: true,
		DisableAASA:        true,
		DisableAssetLinks:  true,
	}, ""))
	mux := http.NewServeMux()
	s.mountSEO(mux)
	for _, p := range []string{
		"/robots.txt", "/sitemap.xml", "/llms.txt",
		"/.well-known/security.txt", "/security.txt",
		"/.well-known/apple-app-site-association",
		"/.well-known/assetlinks.json",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("disabled %s → status %d, want 404", p, rec.Code)
		}
	}
}

// ---- warnSEO -------------------------------------------------------

func TestSEO_WarnSEO_WarnsOnMissingProdConfig(t *testing.T) {
	seoFreezeTime(t)
	buf := log.Capture(t, "render")
	withSiteOpts(t, nil, Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.com",
	})
	got := buf.String()
	if !strings.Contains(got, "SecurityContact is unset") {
		t.Errorf("expected SecurityContact warning, got:\n%s", got)
	}
	if !strings.Contains(got, "AppleTeamID is unset") {
		t.Errorf("expected AppleTeamID warning, got:\n%s", got)
	}
}

func TestSEO_WarnSEO_SkipsInDev(t *testing.T) {
	seoFreezeTime(t)
	buf := log.Capture(t, "render")
	withSiteOpts(t, nil, Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.com",
		Dev:            true,
	})
	log.AssertContains(t, buf, "")
}

func TestSEO_WarnSEO_QuietWhenConfigured(t *testing.T) {
	seoFreezeTime(t)
	buf := log.Capture(t, "render")
	withSiteOpts(t, nil, Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.com",
		SEO: SEOOptions{
			SecurityContact: "sec@example.com",
			AppleTeamID:     "ABCD123456",
		},
	})
	got := buf.String()
	if strings.Contains(got, "SecurityContact is unset") {
		t.Errorf("unexpected SecurityContact warning when set:\n%s", got)
	}
	if strings.Contains(got, "AppleTeamID is unset") {
		t.Errorf("unexpected AppleTeamID warning when set:\n%s", got)
	}
}

// ---- mustParseSEOTemplate ------------------------------------------

// TestSEO_LoadTemplate_ReadError exercises the read-failure branch
// inside the default seoLoadTemplate closure. Real //go:embed reads
// can't fail, so the seoReadEmbedded inner seam is the only path.
func TestSEO_LoadTemplate_ReadError(t *testing.T) {
	orig := seoReadEmbedded
	seoReadEmbedded = func(_ string) ([]byte, error) {
		return nil, errors.New("synthetic read failure")
	}
	t.Cleanup(func() { seoReadEmbedded = orig })

	_, err := seoLoadTemplate("anything")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic read failure") {
		t.Errorf("err = %v, want it to wrap underlying", err)
	}
}

// TestSEO_MustParse_PanicsOnLoadError covers the only error branch of
// mustParseSEOTemplate. The real embedded templates never fail to
// load (they ship with the binary, parse is statically verified by
// the package init), so the seoLoadTemplate seam is the only path
// into the panic.
func TestSEO_MustParse_PanicsOnLoadError(t *testing.T) {
	orig := seoLoadTemplate
	seoLoadTemplate = func(_ string) (*template.Template, error) {
		return nil, errors.New("synthetic load failure")
	}
	t.Cleanup(func() { seoLoadTemplate = orig })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "synthetic load failure") {
			t.Errorf("panic message = %q, want to contain underlying err", msg)
		}
	}()
	mustParseSEOTemplate("ignored")
}

func TestSEO_WarnSEO_QuietWhenDisabled(t *testing.T) {
	seoFreezeTime(t)
	buf := log.Capture(t, "render")
	withSiteOpts(t, nil, Options{
		FS:             multiLangFS(),
		SupportedLangs: []string{"es", "en"},
		CanonicalHost:  "https://example.com",
		SEO: SEOOptions{
			DisableSecurityTxt: true,
			DisableAASA:        true,
		},
	})
	log.AssertContains(t, buf, "")
}
