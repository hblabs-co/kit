package site

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
)

// wrapStyledNotFound replaces the render framework's default
// plaintext "404 page not found" body with a service-styled
// pages/not-found page, served at the original URL with status
// 404. Opt-in via site.Options.StyledNotFound — services that
// don't ship a not-found template stay on the default plaintext.
//
// Strategy: response-recording shim. Pass the request to the
// inner handler, buffer its output, and inspect the status. If
// it's a 404, issue an internal sub-request to /<lang>/not-found
// against the same handler, capture that body and write it back
// to the original ResponseWriter with status 404 plus
// Content-Type: text/html.
//
// Lang detection mirrors the site router: first URL segment
// when it's in supportedLangs, else the `hb-lang` cookie, else
// defaultLang. Both values come from the loaded config.yaml so
// the wrapper stays in sync with whatever supported_langs /
// default_lang the service declares — no per-service hard-coded
// list.
//
// Recursion safety: the sub-request goes through the SAME inner
// handler, not through the wrapper, so it cannot loop. If the
// not-found page itself comes back as a non-2xx (template
// missing, render error, …) we fall back to the original
// plaintext 404 so the visitor at least sees something.
func wrapStyledNotFound(inner http.Handler, supportedLangs []string, defaultLang string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &captureWriter{ResponseWriter: w, header: http.Header{}}
		inner.ServeHTTP(rec, r)
		if rec.status != http.StatusNotFound {
			rec.flush(w)
			return
		}

		lang := pickNotFoundLang(r, supportedLangs, defaultLang)
		sub := &captureWriter{ResponseWriter: w, header: http.Header{}}
		r2 := r.Clone(r.Context())
		u, err := url.Parse("/" + lang + "/not-found")
		if err == nil {
			r2.URL = u
			r2.RequestURI = u.RequestURI()
			r2.Method = http.MethodGet
			inner.ServeHTTP(sub, r2)
		}

		if sub.status >= 200 && sub.status < 300 && sub.body.Len() > 0 {
			// Copy the rendered not-found body back, but keep the
			// 404 status so analytics + crawlers see the truth.
			ct := sub.header.Get("Content-Type")
			if ct == "" {
				ct = "text/html; charset=utf-8"
			}
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(sub.body.Bytes())
			return
		}
		// Sub-request didn't work — let the original 404 through.
		rec.flush(w)
	})
}

// pickNotFoundLang chooses the language used to resolve the
// not-found page. Priority: explicit first URL segment >
// hb-lang cookie > default. Matches the site router's own
// resolution order so the visitor sees the message in the
// language they were navigating in.
func pickNotFoundLang(r *http.Request, supported []string, def string) string {
	path := strings.Trim(r.URL.Path, "/")
	if i := strings.IndexByte(path, '/'); i > 0 {
		path = path[:i]
	}
	if path != "" && langInList(supported, path) {
		return path
	}
	if c, err := r.Cookie("hb-lang"); err == nil && langInList(supported, c.Value) {
		return c.Value
	}
	return def
}

func langInList(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// captureWriter buffers the inner handler's response so
// wrapStyledNotFound can decide what to forward. Headers go into
// a side map; the body into a bytes.Buffer. Flushing replays
// them on the real ResponseWriter when the wrapper passes
// through.
type captureWriter struct {
	http.ResponseWriter
	header  http.Header
	status  int
	body    bytes.Buffer
	wroteHd bool
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(s int) {
	if c.wroteHd {
		return
	}
	c.wroteHd = true
	c.status = s
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wroteHd {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(b)
}

// flush replays the captured headers, status and body onto the
// underlying writer. Called when the response was NOT a 404 (or
// when the not-found fallback failed).
func (c *captureWriter) flush(real http.ResponseWriter) {
	for k, vs := range c.header {
		for _, v := range vs {
			real.Header().Add(k, v)
		}
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	real.WriteHeader(status)
	_, _ = real.Write(c.body.Bytes())
}
