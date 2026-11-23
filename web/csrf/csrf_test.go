package csrf

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/web/uikit"
)

const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNew_Defaults(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "prod")
	g := New(Options{})
	if g.Cookie() != DefaultCookie || g.Field() != DefaultField || g.maxAge != int(DefaultMaxAge/time.Second) || !g.secure {
		t.Fatalf("defaults = %+v", g)
	}
	g = New(Options{Cookie: "c", Field: "f", Insecure: true, MaxAge: time.Hour})
	if g.Cookie() != "c" || g.Field() != "f" || g.maxAge != 3600 || g.secure {
		t.Fatalf("overrides = %+v", g)
	}
}

func TestNew_InsecureInLocalDev(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "local")
	if New(Options{}).secure {
		t.Fatal("Secure cookie on local dev (plain http)")
	}
	t.Setenv(constants.EnvHBEnv, "")
	if New(Options{}).secure {
		t.Fatal("unset HB_ENV means local; want no Secure flag")
	}
}

func TestIssue(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	t.Setenv(constants.EnvHBEnv, "prod")
	h := New(Options{}).Issue(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	c := rec.Result().Cookies()
	if rec.Code != http.StatusTeapot || len(c) != 1 || c[0].Name != DefaultCookie || len(c[0].Value) != tokenLen ||
		!c[0].Secure || c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/" {
		t.Fatalf("new cookie: code=%d cookies=%v", rec.Code, c)
	}

	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookie, Value: tok})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Value != tok {
		t.Fatalf("existing cookie not re-issued: %v", c)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookie, Value: "short"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Value == "short" || len(c[0].Value) != tokenLen {
		t.Fatalf("malformed cookie not replaced: %v", c)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("POST must not issue a cookie")
	}
}

func TestIssue_RandFailureFailsClosed(t *testing.T) {
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { randRead = orig }()
	rec := httptest.NewRecorder()
	New(Options{}).Issue(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("cookie issued despite rand failure")
	}
}

func TestVerify(t *testing.T) {
	g := New(Options{Cookie: "c", Field: "f"})
	cases := map[string]struct {
		cookie, field string
		want          error
	}{
		"ok":         {tok, tok, nil},
		"no cookie":  {"", tok, ErrCookieMissing},
		"no field":   {tok, "", ErrFieldEmpty},
		"mismatch":   {tok, "x", ErrMismatch},
		"same short": {"abc", "abc", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			form := url.Values{}
			if c.field != "" {
				form.Set("f", c.field)
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if c.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "c", Value: c.cookie})
			}
			if err := g.Verify(req); !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
		})
	}
}

// TestClientScriptDefaults pins the uikit copy script to the server
// defaults: a rename on one side only would silently break every form.
func TestClientScriptDefaults(t *testing.T) {
	body, err := fs.ReadFile(uikit.FS, "js/public/csrf.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"` + DefaultCookie + `"`, `"` + DefaultField + `"`, "data-csrf-form"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("csrf.js missing %s", want)
		}
	}
}
