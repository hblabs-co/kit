// Package csrf implements the double-submit cookie pattern for plain
// HTML forms on cacheable pages.
//
// Issue wraps the page handler so every GET/HEAD leaves a random token
// in a cookie (sliding expiry). The token is never embedded in the
// HTML, so pages stay cacheable; a small script copies the cookie into
// a hidden form field at submit time (uikit ships one:
// js/public/csrf.js). Verify then checks that the posted field equals
// the cookie. A cross-site attacker can make the browser send the
// cookie but cannot read it to fill the field, and the cookie is
// SameSite=Strict on top.
//
//	guard := csrf.New(csrf.Options{}) // Secure cookie unless HB_ENV=local
//	mux.Handle("/", guard.Issue(pages))
//	// in the POST handler:
//	if err := guard.Verify(r); err != nil { ... reject ... }
package csrf

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
	"github.com/hblabs-co/kit/web/siteurl"
)

// Defaults applied by New.
const (
	DefaultCookie = "hbl_csrf"
	DefaultField  = "_csrf"
	DefaultMaxAge = 30 * 24 * time.Hour
)

// tokenLen is the hex length of a token (32 random bytes).
const tokenLen = 64

// Verification errors.
var (
	ErrCookieMissing = errors.New("csrf: cookie missing")
	ErrFieldEmpty    = errors.New("csrf: form field empty")
	ErrMismatch      = errors.New("csrf: cookie and form field differ")
)

// Options configures a Guard. Zero values take the defaults above.
type Options struct {
	// Cookie is the token cookie name. It must match what the
	// client-side copy script reads.
	Cookie string
	// Field is the hidden form field name.
	Field string
	// Insecure drops the cookie's Secure flag. The flag is set by
	// default and dropped automatically in local dev
	// (siteurl.IsDev(), HB_ENV=local), where pages run on plain
	// http://localhost; set Insecure only for another non-TLS setup.
	Insecure bool
	// MaxAge is the sliding cookie lifetime.
	MaxAge time.Duration
}

// Guard issues and verifies double-submit tokens. Safe for concurrent
// use.
type Guard struct {
	cookie string
	field  string
	secure bool
	maxAge int
}

// New applies defaults to opts. Whether the cookie is Secure is
// decided here, from opts.Insecure and HB_ENV.
func New(opts Options) *Guard {
	secure := !opts.Insecure && !siteurl.IsDev()
	g := &Guard{cookie: opts.Cookie, field: opts.Field, secure: secure, maxAge: int(opts.MaxAge / time.Second)}
	if g.cookie == "" {
		g.cookie = DefaultCookie
	}
	if g.field == "" {
		g.field = DefaultField
	}
	if g.maxAge <= 0 {
		g.maxAge = int(DefaultMaxAge / time.Second)
	}
	secureSrc := system.SourceDefault
	switch {
	case opts.Insecure:
		secureSrc = "option (Insecure)"
	case !secure:
		secureSrc = "local dev (" + constants.EnvHBEnv + ")"
	}
	log.Info(system.DefaultsMsg, "area", "csrf",
		"cookie", g.cookie, "field", g.field, "secure", g.secure, "secure_source", secureSrc,
		"max_age", (time.Duration(g.maxAge) * time.Second).String())
	return g
}

// Cookie returns the cookie name.
func (g *Guard) Cookie() string { return g.cookie }

// Field returns the form field name.
func (g *Guard) Field() string { return g.field }

// Issue wraps next so every GET/HEAD response carries the token
// cookie, reusing a well-formed existing token and refreshing its
// expiry. If no random token can be made the cookie is skipped, so
// the next POST fails closed.
func (g *Guard) Issue(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			g.ensureCookie(w, r)
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) ensureCookie(w http.ResponseWriter, r *http.Request) {
	var token string
	if c, err := r.Cookie(g.cookie); err == nil && len(c.Value) == tokenLen {
		token = c.Value
	} else {
		t, err := newToken()
		if err != nil {
			return
		}
		token = t
	}
	http.SetCookie(w, &http.Cookie{
		Name:     g.cookie,
		Value:    token,
		Path:     "/",
		MaxAge:   g.maxAge,
		SameSite: http.SameSiteStrictMode,
		Secure:   g.secure,
		// Not HttpOnly: the page script reads it to fill the field.
	})
}

// Verify checks the posted field against the cookie in constant
// time. It parses the form if needed.
func (g *Guard) Verify(r *http.Request) error {
	c, err := r.Cookie(g.cookie)
	if err != nil || c.Value == "" {
		return ErrCookieMissing
	}
	field := r.PostFormValue(g.field)
	if field == "" {
		return ErrFieldEmpty
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) != 1 {
		return ErrMismatch
	}
	return nil
}

// randRead is the crypto/rand seam for the fail-closed branch.
var randRead = rand.Read

func newToken() (string, error) {
	buf := make([]byte, tokenLen/2)
	if _, err := randRead(buf); err != nil {
		return "", fmt.Errorf("csrf: rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
