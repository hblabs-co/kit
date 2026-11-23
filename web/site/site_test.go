package site

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// siteFixture is the smallest tree site.New accepts in prod mode:
// one page, a styled not-found page, the layout, two locales and the
// YAML config.
func siteFixture() fstest.MapFS {
	return fstest.MapFS{
		"config/config.yaml": &fstest.MapFile{Data: []byte(
			"canonical_host: https://example.test\ndefault_lang: es\nsupported_langs: [es, en]\nbrand: Fixture\n")},
		"pages/index.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p id="home">{{ T .Lang "hello" }} {{ greet }}</p>{{ end }}`)},
		"pages/not-found.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "content" }}<p id="nf">{{ T .Lang "missing" }}</p>{{ end }}`)},
		"partials/_layout.html.tmpl": &fstest.MapFile{
			Data: []byte(`{{ define "_layout" }}<html lang="{{ .Lang }}">{{ template "content" . }}</html>{{ end }}`)},
		"locales/es.json": &fstest.MapFile{Data: []byte(`{"hello":"hola","missing":"no existe","site.title":"Sitio","site.description":"Desc"}`)},
		"locales/en.json": &fstest.MapFile{Data: []byte(`{"hello":"hi","missing":"not here","site.title":"Site","site.description":"Desc"}`)},
		"static/x.css":    &fstest.MapFile{Data: []byte("body{}")},
	}
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBannerExtras(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "production")
	if got := BannerExtras(); got != nil {
		t.Errorf("prod BannerExtras() = %v, want nil", got)
	}
	t.Setenv(constants.EnvHBEnv, "local")
	got := BannerExtras()
	if len(got) != 1 || got[0] != hotReloadBannerLine {
		t.Errorf("dev BannerExtras() = %v", got)
	}
}

func TestNew_Prod(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "production")

	h, modules, cfg, err := New(Options{
		SiteFS:         siteFixture(),
		ConfigPath:     "config/config.yaml",
		FuncMap:        MergeFuncMaps(map[string]any{"greet": func() string { return "mundo" }}),
		StyledNotFound: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg == nil || cfg.DefaultLang != "es" || len(cfg.SupportedLangs) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
	// Prod has no hot reloaders. The slice holds typed-nil pointers
	// (system.RunForever filters those), hence the reflect check.
	for i, m := range modules {
		if m != nil && !reflect.ValueOf(m).IsNil() {
			t.Errorf("prod module %d = %v, want nil (no hot reload outside dev)", i, m)
		}
	}

	t.Run("page renders with service FuncMap", func(t *testing.T) {
		rec := get(t, h, "/es/")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /es/ = %d, body %q", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, `<p id="home">hola mundo</p>`) {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("security headers and request id are applied", func(t *testing.T) {
		rec := get(t, h, "/en/")
		for _, name := range []string{
			constants.HeaderContentSecurityPolicy,
			constants.HeaderXContentTypeOptions,
			constants.HeaderReferrerPolicy,
			constants.HeaderRequestID,
		} {
			if rec.Header().Get(name) == "" {
				t.Errorf("response header %s is missing", name)
			}
		}
	})

	t.Run("unknown page gets the styled 404 in the URL's language", func(t *testing.T) {
		rec := get(t, h, "/en/does-not-exist")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, `<p id="nf">not here</p>`) {
			t.Errorf("styled 404 body = %q", body)
		}
	})

	t.Run("without StyledNotFound the 404 is the plain one", func(t *testing.T) {
		plain, _, _, err := New(Options{
			SiteFS:     siteFixture(),
			ConfigPath: "config/config.yaml",
			FuncMap:    map[string]any{"greet": func() string { return "" }},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := get(t, plain, "/en/does-not-exist")
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), `id="nf"`) {
			t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
	})
}

func TestNew_Errors(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "production")

	if _, _, _, err := New(Options{SiteFS: siteFixture(), ConfigPath: "config/nope.yaml"}); err == nil {
		t.Error("missing config file: want an error")
	}

	bad := siteFixture()
	bad["config/config.yaml"] = &fstest.MapFile{Data: []byte("unknown_key: 1\n")}
	if _, _, _, err := New(Options{SiteFS: bad, ConfigPath: "config/config.yaml"}); err == nil {
		t.Error("unknown config key: want an error (strict YAML)")
	}

	// A template that calls a function nobody registered fails at
	// boot, not on the first request.
	if _, _, _, err := New(Options{SiteFS: siteFixture(), ConfigPath: "config/config.yaml"}); err == nil {
		t.Error("template using an unregistered func: want an error")
	}
}

func TestPickNotFoundLang(t *testing.T) {
	supported := []string{"de", "en"}
	cases := []struct {
		name, path, cookie, want string
	}{
		{"lang from first path segment", "/en/foo/bar", "", "en"},
		{"bare lang segment", "/en", "", "en"},
		{"unsupported segment falls to cookie", "/fr/foo", "en", "en"},
		{"unsupported cookie falls to default", "/fr/foo", "fr", "de"},
		{"root falls to default", "/", "", "de"},
		{"path wins over cookie", "/de/x", "en", "de"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "hb-lang", Value: tc.cookie})
			}
			if got := pickNotFoundLang(r, supported, "de"); got != tc.want {
				t.Errorf("pickNotFoundLang(%s, cookie=%q) = %q, want %q", tc.path, tc.cookie, got, tc.want)
			}
		})
	}
}

func TestWrapStyledNotFound_PassesThroughNon404(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("X-Inner", "1")
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("tea"))
		default:
			// Also 404s for the /<lang>/not-found sub-request, so the
			// wrapper has no styled body to substitute.
			http.NotFound(w, r)
		}
	})
	h := wrapStyledNotFound(inner, []string{"de"}, "de")

	rec := get(t, h, "/ok")
	if rec.Code != http.StatusTeapot || rec.Body.String() != "tea" || rec.Header().Get("X-Inner") != "1" {
		t.Errorf("non-404 was altered: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}

	rec = get(t, h, "/missing")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("fallback 404 = %d %q", rec.Code, rec.Body.String())
	}
}

func TestWithDefaults(t *testing.T) {
	got := withDefaults(Options{})
	if got.WatchRoot != DefaultWatchRoot || got.ConfigPath != DefaultConfigPath || got.ExtraStatic[UIKitMount] == nil {
		t.Fatalf("defaults = %+v", got)
	}

	own := fstest.MapFS{"x.css": &fstest.MapFile{Data: []byte("x")}}
	in := map[string]fs.FS{"other": own}
	got = withDefaults(Options{WatchRoot: "w", ConfigPath: "c.yaml", ExtraStatic: in})
	if got.WatchRoot != "w" || got.ConfigPath != "c.yaml" || got.ExtraStatic["other"] == nil || got.ExtraStatic[UIKitMount] == nil {
		t.Fatalf("explicit = %+v", got)
	}
	if len(in) != 1 {
		t.Fatal("caller's ExtraStatic map was mutated")
	}

	got = withDefaults(Options{ExtraStatic: map[string]fs.FS{UIKitMount: own}})
	if _, err := fs.Stat(got.ExtraStatic[UIKitMount], "x.css"); err != nil {
		t.Fatal("explicit uikit entry did not win")
	}

	got = withDefaults(Options{WithoutUIKit: true})
	if _, ok := got.ExtraStatic[UIKitMount]; ok {
		t.Fatal("WithoutUIKit still mounted the uikit")
	}
}

// TestNew_DefaultsServeUIKit: with no ConfigPath and no ExtraStatic
// the shared stylesheet is served under /static/uikit/.
func TestNew_DefaultsServeUIKit(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "production")
	h, _, _, err := New(Options{
		SiteFS:  siteFixture(),
		FuncMap: map[string]any{"greet": func() string { return "" }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, "/static/uikit/css/tokens.css"); rec.Code != http.StatusOK {
		t.Fatalf("uikit tokens.css = %d", rec.Code)
	}
}

func TestSiteDefaultsLog(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "production")
	buf := log.Capture(t, "site")
	_, _, _, err := New(Options{SiteFS: siteFixture(), FuncMap: map[string]any{"greet": func() string { return "" }}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="effective defaults"`, "area=site", "dev=false",
		"config_path=config/config.yaml", "config_path_source=default",
		"watch_root=.", "static_mounts=static,static/uikit", "uikit_source=default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("defaults log missing %q:\n%s", want, out)
		}
	}

	own := fstest.MapFS{}
	got := siteDefaults(Options{ConfigPath: "c.yaml", ExtraStatic: map[string]fs.FS{UIKitMount: own, "a": own}})
	line := fmt.Sprint(got...)
	for _, want := range []string{"config_path_sourceoption", "static_mountsstatic,static/a,static/uikit", "uikit_sourceoption"} {
		if !strings.Contains(strings.ReplaceAll(line, " ", ""), want) {
			t.Errorf("explicit options: %q missing in %v", want, got)
		}
	}
	if line := fmt.Sprint(siteDefaults(Options{WithoutUIKit: true})...); !strings.Contains(line, "disabled (WithoutUIKit)") {
		t.Errorf("WithoutUIKit not reported: %s", line)
	}
}
