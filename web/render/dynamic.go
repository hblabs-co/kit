package render

import (
	"fmt"
	"html/template"
	"strings"
)

// dynamicRoute is a single template whose URL key contains one
// or more [param] path segments. The classic use case is a
// per-slug detail page: pages/reviews/[slug].html.tmpl serves
// every /<lang>/reviews/<anything> URL with .Params.slug bound
// to the captured segment, so 1 file replaces N near-identical
// per-slug files.
//
// Naming convention mirrors Next.js / Astro / SvelteKit so
// engineers crossing from those stacks recognise it without a
// readme:
//
//	pages/team/[slug].html.tmpl              → /<lang>/team/<slug>
//	pages/blog/[year]/[slug].html.tmpl       → /<lang>/blog/<year>/<slug>
//
// Multiple params per template are fine. Exact-match templates
// still win — if both pages/reviews/[slug].html.tmpl AND
// pages/reviews/featured.html.tmpl exist, /reviews/featured
// renders the static one. The router checks s.pages (exact)
// before scanning s.dynamicPages.
//
// Validation of which slugs are "valid" is the template's job:
// a pages/reviews/[slug].html.tmpl can use a funcmap
// helper to look up the item by slug and renders a friendly
// not-found body if the lookup fails. The framework matches
// any non-empty segment to keep the routing layer
// service-agnostic.
type dynamicRoute struct {
	// segments holds each path segment of the route, either as a
	// literal string (must match exactly) or as a captured param
	// (matches any non-empty segment, value flows into .Params).
	segments []routeSegment
	tmpl     *template.Template
	// key is the original pages/-relative key for diagnostics —
	// e.g. "reviews/[slug]". Lets boot logs and error messages
	// reference the source template by the path the author
	// recognises.
	key string
}

// routeSegment is one piece of a dynamicRoute. Exactly one of
// literal or param is non-empty.
type routeSegment struct {
	literal string
	param   string
}

// isDynamicKey reports whether a pages-relative key contains
// any [param] segment. Cheap pre-check so loadPages routes
// non-dynamic keys through the fast s.pages map.
func isDynamicKey(key string) bool {
	return strings.Contains(key, "[") && strings.Contains(key, "]")
}

// parseDynamicRoute turns a pages-relative key like
// "reviews/[slug]" into a dynamicRoute. Validates that each
// [param] segment is well-formed ([ and ] balance, non-empty
// param name, valid identifier characters) so a typo surfaces
// at boot rather than as a silently-unmatched route.
func parseDynamicRoute(key string, tmpl *template.Template) (dynamicRoute, error) {
	parts := strings.Split(key, "/")
	segs := make([]routeSegment, 0, len(parts))
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "[") && strings.HasSuffix(p, "]"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "["), "]")
			if name == "" {
				return dynamicRoute{}, fmt.Errorf("render: dynamic route %q: empty [] param", key)
			}
			if !validIdentifier(name) {
				return dynamicRoute{}, fmt.Errorf("render: dynamic route %q: param %q is not a valid identifier (a-z, 0-9, _)", key, name)
			}
			segs = append(segs, routeSegment{param: name})
		case strings.ContainsAny(p, "[]"):
			return dynamicRoute{}, fmt.Errorf("render: dynamic route %q: segment %q has unbalanced [ ] — wrap the whole segment as [name]", key, p)
		default:
			segs = append(segs, routeSegment{literal: p})
		}
	}
	return dynamicRoute{segments: segs, tmpl: tmpl, key: key}, nil
}

// match tries to bind a request path to this route. Returns the
// captured params and true on success; nil + false on a miss.
// Segment counts must match exactly — pages/reviews/[slug] does
// NOT match /reviews/julia/extra because the count differs.
func (dr dynamicRoute) match(pageKey string) (map[string]string, bool) {
	parts := strings.Split(pageKey, "/")
	if len(parts) != len(dr.segments) {
		return nil, false
	}
	params := make(map[string]string, len(dr.segments))
	for i, seg := range dr.segments {
		if seg.literal != "" {
			if parts[i] != seg.literal {
				return nil, false
			}
			continue
		}
		if parts[i] == "" {
			return nil, false
		}
		params[seg.param] = parts[i]
	}
	return params, true
}

// validIdentifier reports whether s is a syntactically safe
// param name: lowercase ASCII letters, digits, underscores,
// at least one char, first char not a digit. Lets templates
// reach the value via {{ .Params.slug }} without needing
// `index` for the simple case.
func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
