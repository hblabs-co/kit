package secheaders

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPick locks the three-way contract this whole package leans on:
// disabled wins, then explicit, then default. A regression here
// silently flips every header's resolution rule.
func TestPick(t *testing.T) {
	cases := []struct {
		name          string
		explicit, def string
		disabled      bool
		want          string
	}{
		{name: "default when explicit empty", explicit: "", def: "D", want: "D"},
		{name: "explicit wins", explicit: "X", def: "D", want: "X"},
		{name: "disabled beats explicit", explicit: "X", def: "D", disabled: true, want: ""},
		{name: "disabled beats default", explicit: "", def: "D", disabled: true, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pick(c.explicit, c.def, c.disabled)
			if got != c.want {
				t.Errorf("pick(%q, %q, %v) = %q, want %q", c.explicit, c.def, c.disabled, got, c.want)
			}
		})
	}
}

// TestMiddleware_Defaults verifies the zero-value Options applies
// every Default* constant. This is THE recommended config, so the
// happy path needs an explicit assertion that no header silently
// drifts away from the documented baseline.
func TestMiddleware_Defaults(t *testing.T) {
	rec := serve(t, Options{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	want := map[string]string{
		"Strict-Transport-Security": DefaultHSTS,
		"X-Content-Type-Options":    DefaultContentTypeOptions,
		"X-Frame-Options":           DefaultFrameOptions,
		"Referrer-Policy":           DefaultReferrerPolicy,
		"Permissions-Policy":        DefaultPermissionsPolicy,
		"Content-Security-Policy":   DefaultCSP,
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	// The report-only variant must be absent when ReportOnly=false.
	if got := rec.Header().Get("Content-Security-Policy-Report-Only"); got != "" {
		t.Errorf("Content-Security-Policy-Report-Only leaked: %q", got)
	}
}

// TestMiddleware_Overrides confirms each Options string field, when
// non-empty, replaces its corresponding default. Iterates field-by-
// field rather than setting all at once so a future regression
// pinpoints which field broke.
func TestMiddleware_Overrides(t *testing.T) {
	cases := []struct {
		name   string
		opts   Options
		header string
		want   string
	}{
		{"hsts", Options{HSTS: "max-age=10"}, "Strict-Transport-Security", "max-age=10"},
		{"cto", Options{ContentTypeOptions: "nosniff-x"}, "X-Content-Type-Options", "nosniff-x"},
		{"frame", Options{FrameOptions: "SAMEORIGIN"}, "X-Frame-Options", "SAMEORIGIN"},
		{"ref", Options{ReferrerPolicy: "no-referrer"}, "Referrer-Policy", "no-referrer"},
		{"perms", Options{PermissionsPolicy: "geolocation=(self)"}, "Permissions-Policy", "geolocation=(self)"},
		{"csp", Options{ContentSecurityPolicy: "default-src 'none'"}, "Content-Security-Policy", "default-src 'none'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := serve(t, c.opts, ok)
			if got := rec.Header().Get(c.header); got != c.want {
				t.Errorf("%s = %q, want %q", c.header, got, c.want)
			}
		})
	}
}

// TestMiddleware_Disable verifies each Disable* flag drops the
// header entirely — including when an explicit value is also set
// (the documented "Disable* takes precedence" contract: a future code
// change can't accidentally re-introduce a header that policy wants
// stripped because the edge already injects it).
func TestMiddleware_Disable(t *testing.T) {
	cases := []struct {
		name   string
		opts   Options
		header string
	}{
		{"hsts", Options{HSTS: "still-set", DisableHSTS: true}, "Strict-Transport-Security"},
		{"cto", Options{DisableContentTypeOptions: true}, "X-Content-Type-Options"},
		{"frame", Options{DisableFrameOptions: true}, "X-Frame-Options"},
		{"ref", Options{DisableReferrerPolicy: true}, "Referrer-Policy"},
		{"perms", Options{DisablePermissionsPolicy: true}, "Permissions-Policy"},
		{"csp", Options{ContentSecurityPolicy: "default-src 'none'", DisableContentSecurityPolicy: true}, "Content-Security-Policy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := serve(t, c.opts, ok)
			if got := rec.Header().Get(c.header); got != "" {
				t.Errorf("%s should be absent, got %q", c.header, got)
			}
		})
	}
}

// TestMiddleware_ReportOnly switches CSP to the report-only header.
// The enforcement header must NOT appear simultaneously — otherwise
// browsers enforce AND log, defeating the point of a rollout mode.
func TestMiddleware_ReportOnly(t *testing.T) {
	rec := serve(t, Options{ReportOnly: true}, ok)
	if got := rec.Header().Get("Content-Security-Policy-Report-Only"); got != DefaultCSP {
		t.Errorf("Content-Security-Policy-Report-Only = %q, want default CSP", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("enforcement header leaked alongside report-only: %q", got)
	}
}

// TestMiddleware_DownstreamOverrides locks the documented behavior
// that headers are set BEFORE next.ServeHTTP runs, so a handler that
// Set()s the same key wins. Endpoints rely on this to opt out per-
// request (e.g. a legacy embed page that needs frame-ancestors).
func TestMiddleware_DownstreamOverrides(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.WriteHeader(http.StatusOK)
	})
	rec := serve(t, Options{}, handler)
	if got := rec.Header().Get("X-Frame-Options"); got != "ALLOWALL" {
		t.Errorf("downstream Set() didn't win: X-Frame-Options = %q", got)
	}
}

// TestMiddleware_NextCalled is a minimal liveness check: the
// middleware must always invoke next, otherwise hardening a service
// would silently break it. Asserts via a sentinel status code
// (StatusTeapot) that only the inner handler can produce.
func TestMiddleware_NextCalled(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rec := serve(t, Options{}, handler)
	if rec.Code != http.StatusTeapot {
		t.Errorf("next handler not invoked: status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// TestAppendImgSrc covers every branch of the splicer: a CSP that
// has an `img-src` directive (the default), a CSP that doesn't, and
// the formatting around adjacent whitespace + the directive
// terminator.
func TestAppendImgSrc(t *testing.T) {
	cases := []struct {
		name  string
		csp   string
		hosts []string
		want  string
	}{
		{
			name:  "splices into existing directive",
			csp:   "default-src 'self'; img-src 'self' data: https:; font-src 'self'",
			hosts: []string{"http://localhost:9000"},
			want:  "default-src 'self'; img-src 'self' data: https: http://localhost:9000; font-src 'self'",
		},
		{
			name:  "no img-src directive prepends",
			csp:   "default-src 'self'; font-src 'self'",
			hosts: []string{"http://localhost:9000"},
			want:  "img-src 'self' http://localhost:9000; default-src 'self'; font-src 'self'",
		},
		{
			name:  "multiple hosts",
			csp:   "img-src 'self' https:",
			hosts: []string{"http://localhost:9000", "http://localhost:8080"},
			want:  "img-src 'self' https: http://localhost:9000 http://localhost:8080",
		},
		{
			name:  "matches bare img-src directive",
			csp:   "default-src 'self'; img-src",
			hosts: []string{"https://cdn.example.com"},
			want:  "default-src 'self'; img-src https://cdn.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendImgSrc(tc.csp, tc.hosts)
			if got != tc.want {
				t.Errorf("appendImgSrc:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

// TestMiddleware_AdditionalImgSrc — wiring test: the option flows
// through the middleware and the served header actually contains
// the spliced host. Distinct from TestAppendImgSrc which only covers
// the helper.
func TestMiddleware_AdditionalImgSrc(t *testing.T) {
	rec := serve(t, Options{AdditionalImgSrc: []string{"http://localhost:9000"}}, ok)
	got := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(got, "img-src 'self' data: https: http://localhost:9000") {
		t.Errorf("CSP missing spliced host: %s", got)
	}
}

// TestMiddleware_AdditionalImgSrc_DisabledCSP — when CSP is disabled
// the AdditionalImgSrc is a no-op. Locks the precedence: turning the
// header off wins over splicing extras.
func TestMiddleware_AdditionalImgSrc_DisabledCSP(t *testing.T) {
	rec := serve(t, Options{
		DisableContentSecurityPolicy: true,
		AdditionalImgSrc:             []string{"http://localhost:9000"},
	}, ok)
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP should be absent, got: %s", got)
	}
}

// serve wires the middleware around handler and runs a single
// GET request, returning the recorded response. Keeps the per-test
// boilerplate to one line.
func serve(t *testing.T, opts Options, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	mw := Middleware(opts)(handler)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	mw.ServeHTTP(rec, r)
	return rec
}

// ok is a no-op handler that returns 200. Reused by tests that don't
// care about response body, only headers.
var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})
