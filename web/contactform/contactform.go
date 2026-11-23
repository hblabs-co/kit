// Package contactform serves a plain HTML contact / feedback form that
// forwards each submission by mail.
//
// The handler applies its defenses in a fixed order: body cap → CSRF
// (double-submit, see web/csrf) → honeypot → per-IP rate limit →
// field validation → send. Every outcome is a 303 to a rendered page
// (/<lang>/<SuccessPage> or /<lang>/<FailurePage>), so the form works
// without JavaScript beyond the CSRF copy and the browser never
// re-POSTs on reload. A honeypot hit is answered with the success
// page so bots learn nothing.
//
// Wiring:
//
//	guard := csrf.New(csrf.Options{})
//	mux.Handle("POST /contact", &contactform.Handler{
//	    Fields: []contactform.Field{
//	        {Name: "message", Label: "Message", Required: true, Multiline: true},
//	        {Name: "email", Label: "Reply to", Email: true},
//	    },
//	    SubjectPrefix: "Contact: ",
//	    Sender:        notifier, // e.g. *scaleway.Notifier
//	    Limiter:       limiter,  // e.g. *ratelimit.Limiter
//	    CSRF:          guard,
//	    Langs:         cfg.SupportedLangs,
//	    DefaultLang:   cfg.DefaultLang,
//	})
//	mux.Handle("/", guard.Issue(pages))
package contactform

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hblabs-co/kit/core/httpx"
	"github.com/hblabs-co/kit/core/log"
)

// Defaults applied when the corresponding Handler field is zero.
const (
	DefaultHoneypot    = "website"
	DefaultLangField   = "lang"
	DefaultSuccessPage = "thanks"
	DefaultFailurePage = "failed"
	DefaultMaxRunes    = 4000
	DefaultMaxBody     = 64 << 10
	DefaultSendTimeout = 15 * time.Second
	// MaxEmail caps an Email field, whatever its MaxRunes.
	MaxEmail = 254
	// subjectRunes caps the part of the subject taken from a field.
	subjectRunes = 80
)

// Field describes one form input.
type Field struct {
	// Name is the form field name.
	Name string
	// Label names the field in the mail body.
	Label string
	// Required rejects the submission when the field is blank.
	Required bool
	// MaxRunes caps the trimmed value; 0 means DefaultMaxRunes.
	MaxRunes int
	// Email requires a bare address ("a@example.com", no display
	// name). The first non-empty Email field becomes the Reply-To.
	Email bool
	// Multiline puts the label on its own line in the mail body.
	Multiline bool
}

// Sender delivers one plain-text mail; replyTo may be empty.
// *scaleway.Notifier from github.com/hblabs-co/kit/mail implements it.
type Sender interface {
	Enabled() bool
	Send(ctx context.Context, subject, text, replyTo string) error
}

// Limiter caps submissions per client IP. *ratelimit.Limiter from
// github.com/hblabs-co/kit/core implements it.
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

// Verifier checks the CSRF token of a POST. *csrf.Guard implements it.
type Verifier interface {
	Verify(r *http.Request) error
}

// Value is one submitted field.
type Value struct {
	Field Field
	Text  string
}

// Submission is one validated form post.
type Submission struct {
	Lang    string
	Values  []Value
	ReplyTo string
}

// Get returns the value of the field called name, or "".
func (s Submission) Get(name string) string {
	for _, v := range s.Values {
		if v.Field.Name == name {
			return v.Text
		}
	}
	return ""
}

// Handler serves the form's POST endpoint. Fields, Sender and CSRF are
// required: without CSRF every post is rejected (fail closed).
type Handler struct {
	Fields  []Field
	Sender  Sender
	CSRF    Verifier
	Limiter Limiter // nil disables the rate limit

	// Langs and DefaultLang validate the posted language, which
	// picks the result page.
	Langs       []string
	DefaultLang string
	LangField   string // defaults to DefaultLangField
	Honeypot    string // defaults to DefaultHoneypot

	SuccessPage string // defaults to DefaultSuccessPage
	FailurePage string // defaults to DefaultFailurePage

	// Subject builds the mail subject. Nil uses SubjectPrefix plus
	// the first line of the first field.
	Subject       func(Submission) string
	SubjectPrefix string
	// Intro is the first paragraph of the mail body (e.g. which site
	// the submission came from).
	Intro string

	MaxBody     int64         // defaults to DefaultMaxBody
	SendTimeout time.Duration // defaults to DefaultSendTimeout
}

// ServeHTTP handles one submission.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, or(h.MaxBody, DefaultMaxBody))
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, h.DefaultLang, "parse: "+err.Error())
		return
	}
	lang := h.lang(r.PostFormValue(or(h.LangField, DefaultLangField)))
	if h.CSRF == nil {
		h.fail(w, r, lang, "csrf: no verifier configured")
		return
	}
	if err := h.CSRF.Verify(r); err != nil {
		h.fail(w, r, lang, err.Error())
		return
	}
	if r.PostFormValue(or(h.Honeypot, DefaultHoneypot)) != "" {
		log.Warn("contact form: honeypot hit, dropped", "ip", httpx.ClientIP(r))
		h.redirect(w, r, lang, or(h.SuccessPage, DefaultSuccessPage))
		return
	}
	if h.Limiter != nil && !h.Limiter.Allow(r.Context(), httpx.ClientIP(r)) {
		h.fail(w, r, lang, "rate limit")
		return
	}
	sub, ok := h.Parse(r, lang)
	if !ok {
		h.fail(w, r, lang, "validation failed")
		return
	}
	if h.Sender == nil || !h.Sender.Enabled() {
		h.fail(w, r, lang, "mailer not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), or(h.SendTimeout, DefaultSendTimeout))
	defer cancel()
	if err := h.Sender.Send(ctx, h.subject(sub), h.Body(sub), sub.ReplyTo); err != nil {
		log.Error("contact form: send failed", "err", err)
		h.fail(w, r, lang, "send failed")
		return
	}
	log.Info("contact form: sent", "lang", lang, "has_reply_to", sub.ReplyTo != "")
	h.redirect(w, r, lang, or(h.SuccessPage, DefaultSuccessPage))
}

// Parse reads and validates the fields of a parsed form. It reports
// false when a required field is blank, a value is too long or an
// Email field is not a bare address.
func (h *Handler) Parse(r *http.Request, lang string) (Submission, bool) {
	s := Submission{Lang: lang, Values: make([]Value, 0, len(h.Fields))}
	for _, f := range h.Fields {
		v := strings.TrimSpace(r.PostFormValue(f.Name))
		switch {
		case v == "" && f.Required:
			return Submission{}, false
		case utf8.RuneCountInString(v) > or(f.MaxRunes, DefaultMaxRunes):
			return Submission{}, false
		case f.Email && v != "" && !validEmail(v):
			return Submission{}, false
		}
		if f.Email && v != "" && s.ReplyTo == "" {
			s.ReplyTo = v
		}
		s.Values = append(s.Values, Value{Field: f, Text: v})
	}
	return s, true
}

func validEmail(s string) bool {
	if len(s) > MaxEmail {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

func (h *Handler) subject(s Submission) string {
	if h.Subject != nil {
		return h.Subject(s)
	}
	first := ""
	if len(s.Values) > 0 {
		first = s.Values[0].Text
	}
	return h.SubjectPrefix + FirstLine(first, subjectRunes)
}

// FirstLine returns the first line of s, trimmed and cut to max runes
// with "..." appended when it was longer.
func FirstLine(s string, maxRunes int) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxRunes {
		line = string(r[:maxRunes]) + "..."
	}
	return line
}

// Body renders the plain-text mail: Intro, then each field (multiline
// fields as a labelled paragraph, others as "Label: value"), then the
// page language.
func (h *Handler) Body(s Submission) string {
	var b strings.Builder
	if h.Intro != "" {
		b.WriteString(h.Intro + "\n\n")
	}
	for _, v := range s.Values {
		label := v.Field.Label
		if label == "" {
			label = v.Field.Name
		}
		text := v.Text
		if text == "" {
			text = "(not given)"
			if v.Field.Email {
				text = "(not given, no reply possible)"
			}
		}
		if v.Field.Multiline {
			fmt.Fprintf(&b, "%s:\n%s\n\n", label, text)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", label, text)
		}
	}
	fmt.Fprintf(&b, "Page language: %s\n", s.Lang)
	return b.String()
}

func (h *Handler) lang(v string) string {
	if slices.Contains(h.Langs, v) {
		return v
	}
	return h.DefaultLang
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, lang, reason string) {
	log.Warn("contact form: rejected", "reason", reason, "ip", httpx.ClientIP(r))
	h.redirect(w, r, lang, or(h.FailurePage, DefaultFailurePage))
}

func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, lang, page string) {
	http.Redirect(w, r, "/"+lang+"/"+page, http.StatusSeeOther)
}

// or returns v, or def when v is the zero value.
func or[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
