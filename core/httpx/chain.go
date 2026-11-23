package httpx

import "net/http"

// Middleware is the canonical http middleware shape: a function
// that takes an http.Handler and returns an http.Handler. Aliased
// here so call sites read `[]httpx.Middleware{...}` instead of the
// raw `func(http.Handler) http.Handler` mouthful.
type Middleware = func(http.Handler) http.Handler

// Chain composes middleware in execution order — the FIRST listed
// middleware wraps the rest, so it sees the request first and the
// response last. Terminate the chain with .Then(handler):
//
//	handler := httpx.Chain(
//	    secheaders.Middleware(secheaders.Options{}),
//	    httpx.RequestID(nil),
//	    httpx.AccessLog(opts),
//	    httpx.Recover,
//	).Then(siteHandler)
//
// Reads top-to-bottom matching execution order. Compare to the
// equivalent nested-call form, which reads inside-out and reverses
// the visual flow:
//
//	handler := secheaders.Middleware(...)(httpx.RequestID(nil)(
//	    httpx.AccessLog(opts)(httpx.Recover(siteHandler))))
//
// The returned value's only public surface is Then; the chain is
// applied at that point and a fresh http.Handler comes out.
func Chain(mws ...Middleware) chain {
	return chain{mws: mws}
}

// chain is the in-progress composition returned by Chain. The type
// is unexported because callers never need to spell it — they
// always go Chain(...).Then(h), and the result of Then is a plain
// http.Handler.
type chain struct {
	mws []Middleware
}

// Then binds h as the innermost handler — the one invoked after
// every middleware in the chain has run. Applies the chain in
// reverse so the FIRST middleware passed to Chain becomes the
// OUTERMOST wrap, matching the documented "first listed sees the
// request first" semantics.
func (c chain) Then(h http.Handler) http.Handler {
	for i := len(c.mws) - 1; i >= 0; i-- {
		h = c.mws[i](h)
	}
	return h
}
