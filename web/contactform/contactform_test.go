package contactform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/web/csrf"
)

type fakeLimiter struct{ allow bool }

func (f fakeLimiter) Allow(context.Context, string) bool { return f.allow }

type fakeSender struct {
	enabled bool
	err     error
	subject string
	body    string
	replyTo string
}

func (f *fakeSender) Enabled() bool { return f.enabled }
func (f *fakeSender) Send(_ context.Context, s, b, r string) error {
	f.subject, f.body, f.replyTo = s, b, r
	return f.err
}

const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var testFields = []Field{
	{Name: "need", Label: "What they need", Required: true, Multiline: true},
	{Name: "why", Label: "Why", Required: true, Multiline: true},
	{Name: "email", Label: "Reply to", Email: true, MaxRunes: 200},
	{Name: "version", Label: "Version", MaxRunes: 40},
}

func validForm() url.Values {
	return url.Values{
		"lang": {"de"}, csrf.DefaultField: {tok}, "need": {"Exclude folders\nsecond line"},
		"why": {"I keep VMs there"}, "email": {"user@example.com"}, "version": {"15.2"},
	}
}

func post(t *testing.T, h http.Handler, form url.Values, cookie bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/contact", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie {
		req.AddCookie(&http.Cookie{Name: csrf.DefaultCookie, Value: tok})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newHandler(s Sender, allow bool) *Handler {
	return &Handler{
		Fields: testFields, Sender: s, Limiter: fakeLimiter{allow}, CSRF: csrf.New(csrf.Options{}),
		Langs: []string{"en", "de"}, DefaultLang: "en", SubjectPrefix: "Request: ", Intro: "New request",
	}
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, loc string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != loc {
		t.Fatalf("got %d %q, want 303 %q", rec.Code, rec.Header().Get("Location"), loc)
	}
}

func TestHandler_Success(t *testing.T) {
	s := &fakeSender{enabled: true}
	wantRedirect(t, post(t, newHandler(s, true), validForm(), true), "/de/thanks")
	want := "New request\n\nWhat they need:\nExclude folders\nsecond line\n\nWhy:\nI keep VMs there\n\n" +
		"Reply to: user@example.com\nVersion: 15.2\nPage language: de\n"
	if s.replyTo != "user@example.com" || s.subject != "Request: Exclude folders" || s.body != want {
		t.Fatalf("sent %+v", s)
	}
}

func TestHandler_CustomPagesSubjectNoLimiter(t *testing.T) {
	s := &fakeSender{enabled: true}
	h := newHandler(s, false)
	h.Limiter = nil
	h.SuccessPage, h.FailurePage = "ok", "nope"
	h.Subject = func(sub Submission) string { return "custom " + sub.Get("version") }
	wantRedirect(t, post(t, h, validForm(), true), "/de/ok")
	if s.subject != "custom 15.2" {
		t.Fatalf("subject = %q", s.subject)
	}
	wantRedirect(t, post(t, h, validForm(), false), "/de/nope")
}

func TestHandler_Failures(t *testing.T) {
	cases := map[string]struct {
		mutate func(url.Values)
		cookie bool
		allow  bool
		sender Sender
		want   string
	}{
		"no cookie":     {func(url.Values) {}, false, true, &fakeSender{enabled: true}, "/de/failed"},
		"empty field":   {func(v url.Values) { v.Del(csrf.DefaultField) }, true, true, &fakeSender{enabled: true}, "/de/failed"},
		"field differs": {func(v url.Values) { v.Set(csrf.DefaultField, "x") }, true, true, &fakeSender{enabled: true}, "/de/failed"},
		"honeypot":      {func(v url.Values) { v.Set(DefaultHoneypot, "spam") }, true, true, &fakeSender{enabled: true}, "/de/thanks"},
		"rate limit":    {func(url.Values) {}, true, false, &fakeSender{enabled: true}, "/de/failed"},
		"invalid":       {func(v url.Values) { v.Set("need", "") }, true, true, &fakeSender{enabled: true}, "/de/failed"},
		"disabled":      {func(url.Values) {}, true, true, &fakeSender{}, "/de/failed"},
		"nil sender":    {func(url.Values) {}, true, true, nil, "/de/failed"},
		"send error":    {func(url.Values) {}, true, true, &fakeSender{enabled: true, err: errors.New("x")}, "/de/failed"},
		"unknown lang":  {func(v url.Values) { v.Set("lang", "xx"); v.Set("need", "") }, true, true, &fakeSender{enabled: true}, "/en/failed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := validForm()
			c.mutate(f)
			wantRedirect(t, post(t, newHandler(c.sender, c.allow), f, c.cookie), c.want)
		})
	}
}

func TestHandler_NoCSRFFailsClosed(t *testing.T) {
	s := &fakeSender{enabled: true}
	h := newHandler(s, true)
	h.CSRF = nil
	wantRedirect(t, post(t, h, validForm(), true), "/de/failed")
	if s.subject != "" {
		t.Fatal("sent without a CSRF verifier")
	}
}

func TestHandler_BodyTooLarge(t *testing.T) {
	h := newHandler(&fakeSender{enabled: true}, true)
	big := url.Values{"need": {strings.Repeat("a", DefaultMaxBody+10)}}
	wantRedirect(t, post(t, h, big, true), "/en/failed")
}

func TestParse_Validation(t *testing.T) {
	cases := map[string]struct {
		key, val string
		ok       bool
	}{
		"long need":      {"need", strings.Repeat("a", DefaultMaxRunes+1), false},
		"empty why":      {"why", "  ", false},
		"long why":       {"why", strings.Repeat("a", DefaultMaxRunes+1), false},
		"long version":   {"version", strings.Repeat("a", 41), false},
		"bad email":      {"email", "not-an-email", false},
		"named email":    {"email", "A <a@example.com>", false},
		"long email":     {"email", strings.Repeat("a", 190) + "@example.com", false},
		"no email ok":    {"email", "", true},
		"no version ok":  {"version", "", true},
		"unicode need":   {"need", strings.Repeat("ü", DefaultMaxRunes), true},
		"trimmed spaces": {"version", "  15.2  ", true},
	}
	h := newHandler(nil, true)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := validForm()
			f.Set(c.key, c.val)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(f.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if _, ok := h.Parse(req, "en"); ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
		})
	}
}

func TestValidEmail_CapsLength(t *testing.T) {
	if validEmail(strings.Repeat("a", MaxEmail) + "@example.com") {
		t.Fatal("over-long address accepted")
	}
}

func TestBody_MissingValues(t *testing.T) {
	h := &Handler{}
	s := Submission{Lang: "en", Values: []Value{
		{Field: Field{Name: "email", Label: "Reply to", Email: true}},
		{Field: Field{Name: "os"}},
	}}
	want := "Reply to: (not given, no reply possible)\nos: (not given)\nPage language: en\n"
	if got := h.Body(s); got != want {
		t.Fatalf("Body = %q", got)
	}
	if got := h.subject(Submission{}); got != "" {
		t.Fatalf("empty subject = %q", got)
	}
	if got := (Submission{}).Get("x"); got != "" {
		t.Fatalf("Get missing = %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	long := strings.Repeat("é", 90)
	cases := map[string]string{"": "", "  one \ntwo": "one", long: strings.Repeat("é", 80) + "..."}
	for in, want := range cases {
		if got := FirstLine(in, 80); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
