package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SitemapDiscoveryResult is the typed outcome of walking the
// robots.txt → sitemap discovery chain for one domain.
//
//   - RobotsURL is the URL the discoverer resolved robots.txt
//     from. Empty when every candidate failed.
//   - RobotsBody is the robots.txt body verbatim (empty when
//     unreachable). Exposed so the caller can persist it to its
//     own audit store.
//   - SitemapURLs is the list of sitemap URLs to fetch next. Comes
//     from `Sitemap:` directives in robots.txt when available, or
//     from well-known fallback paths (`/sitemap.xml`,
//     `/sitemap_index.xml`) when robots was empty / unreachable.
type SitemapDiscoveryResult struct {
	RobotsURL   string
	RobotsBody  []byte
	SitemapURLs []string
}

// SitemapEntry is one <url> from a sitemap.xml's urlset. Carries
// only the fields downstream crawlers actually use; the rest of
// the spec (changefreq, priority) is parsed but discarded.
type SitemapEntry struct {
	// Loc is the absolute URL of the resource. Empty entries are
	// dropped by ParseSitemap.
	Loc string

	// LastMod is the upstream's last-modified hint, verbatim
	// (W3C-DTF date or ISO-8601 timestamp). The caller is free to
	// parse it or ignore it; leaving it as a string keeps the
	// parser lossless and avoids forcing a time-format choice on
	// every consumer.
	LastMod string
}

// RobotsCandidates returns the schemes-and-paths a discoverer
// should try when looking for a site's robots.txt, in priority
// order. https first because the vast majority of properties are
// TLS-only by 2026; the http fallback exists for the long tail
// that hasn't moved yet. /robots.txt at the root is the only path
// the RFC defines.
//
// Accepts a bare domain ("example.com"), a scheme-prefixed host
// ("https://example.com"), or a host with a trailing slash —
// normalises all three to the same candidate set.
func RobotsCandidates(domain string) []string {
	d := normaliseDomain(domain)
	return []string{
		"https://" + d + "/robots.txt",
		"http://" + d + "/robots.txt",
	}
}

// FallbackSitemapURLs returns the paths a discoverer probes when
// robots.txt is unreachable OR returns no Sitemap: directives.
// /sitemap.xml at the root is the universal convention;
// /sitemap_index.xml covers the (older) WordPress-Yoast layout
// that some properties still ship.
//
// https is tried first (the 2026 norm); the http variants are the
// tail fallback for the long tail of sites that never moved to
// TLS — without them an http-only site that relies on the
// conventional /sitemap.xml (no Sitemap: directive in robots)
// would never be discovered, since every https probe fails on a
// box with no 443 listener. For an https site the http probes are
// harmless: they 404 fast or 301 to the https URL already tried.
func FallbackSitemapURLs(domain string) []string {
	d := normaliseDomain(domain)
	return []string{
		"https://" + d + "/sitemap.xml",
		"https://" + d + "/sitemap_index.xml",
		"http://" + d + "/sitemap.xml",
		"http://" + d + "/sitemap_index.xml",
	}
}

// normaliseDomain strips scheme prefixes and trailing slashes so
// the candidate builders produce a consistent set regardless of
// whether the caller passed "example.com", "https://example.com",
// or "https://example.com/".
func normaliseDomain(domain string) string {
	d := strings.TrimSpace(domain)
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	d = strings.TrimSuffix(d, "/")
	return d
}

// ParseRobotsSitemaps extracts every `Sitemap: <url>` directive
// from a robots.txt body. Case-insensitive on the field name (the
// RFC is); URL preserved verbatim because Sitemap: directives are
// absolute by definition.
//
// Returns a deduplicated list in source order. A robots.txt that
// declares the same sitemap twice (some CMS plugins do) shouldn't
// produce two fetches downstream.
//
// Skips URLs that don't parse or that use a non-http(s) scheme —
// a misuse the caller doesn't want to retry-bomb on.
func ParseRobotsSitemaps(body []byte) []string {
	seen := make(map[string]struct{})
	var out []string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// robots.txt lines are usually short, but defensively bump
	// the scanner buffer so a pathological one-line file doesn't
	// crash the caller.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if hash := strings.IndexByte(line, '#'); hash >= 0 {
			line = line[:hash]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		field := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if !strings.EqualFold(field, "sitemap") || value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// DiscoverSitemap walks the robots.txt → sitemap discovery chain
// for one domain using the supplied HTTP client and options.
// Composes RobotsCandidates + ParseRobotsSitemaps +
// FallbackSitemapURLs so a caller gets the full discovery in one
// call.
//
// "Success" for robots.txt is a 2xx with a body; a 404 or any
// transport failure is treated as "site has no robots, that's
// fine, fall through to the fallback paths". A 5xx / DNS /
// timeout is treated the same way — discovery is best-effort,
// and recording "I couldn't read your robots" without giving up
// on the rest is the more useful behaviour for an audit-driven
// crawler.
//
// SitemapURLs in the result is ALWAYS non-empty (it contains the
// well-known fallbacks when nothing else worked); whether those
// URLs actually resolve to a parseable sitemap is the caller's
// problem — DiscoverSitemap doesn't fetch them.
func DiscoverSitemap(ctx context.Context, client *http.Client, opts FetchOptions, domain string) SitemapDiscoveryResult {
	out := SitemapDiscoveryResult{}
	for _, candidate := range RobotsCandidates(domain) {
		got := FetchURL(ctx, client, opts, candidate)
		if got.Err != nil || len(got.Body) == 0 {
			continue
		}
		out.RobotsURL = candidate
		out.RobotsBody = got.Body
		out.SitemapURLs = ParseRobotsSitemaps(got.Body)
		break
	}
	if len(out.SitemapURLs) > 0 {
		return out
	}
	out.SitemapURLs = FallbackSitemapURLs(domain)
	return out
}

// sitemapMaxEntries caps how many entries ParseSitemap will read
// from a single document. The sitemaps spec ceiling is 50k URLs
// per file (and 50MB uncompressed); 100k here is a generous
// safety net that still bounds the parser's memory under abuse.
// Callers that want to crawl beyond this limit split the URL set
// into more sitemap files anyway.
const sitemapMaxEntries = 100_000

// ParseSitemap parses a sitemap body and returns either the list
// of leaf entries or the list of nested sitemap URLs, dispatching
// on the root element:
//
//   - <urlset>       → leaf entries (<url><loc>)               [sitemaps.org XML]
//   - <sitemapindex> → nested sitemap URLs (<sitemap><loc>)    [sitemaps.org index]
//   - <rss>          → leaf entries (<item><link>)             [RSS 2.0 feed]
//   - <feed>         → leaf entries (<entry><link href>)       [Atom 1.0 feed]
//
// RSS 2.0 and Atom 1.0 are first-class sitemap formats per the
// sitemaps.org / Google spec — WordPress SEO plugins (All in One
// SEO, Yoast) often point robots.txt at a `sitemap.rss` feed
// instead of the XML urlset, so a crawler that only understood
// <urlset> would silently miss every WordPress site on a feed
// sitemap. The entry's link is the URL; pubDate / updated maps to
// LastMod.
//
// Exactly one of the two return slices is non-empty on success;
// both are empty on an unrecognised root. Recursion is the
// caller's job: walk `nested` with the same parser, applying a
// depth cap (the right value depends on the crawler's policy, not
// the parser).
//
// Returns an error only on hard XML parse failures — an empty
// document, or unrecognised root, returns (nil, nil, nil) so a
// caller can treat "found nothing" uniformly with "the upstream
// served a 404" without branching on error type.
func ParseSitemap(body []byte) (entries []SitemapEntry, nested []string, err error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil, nil
	}

	// xml.Decoder lets us inspect the root element name without
	// fully unmarshalling — needed because urlset and sitemapindex
	// share the same shape (<x><y><loc>...</loc></y></x>) but
	// mean different things.
	dec := xml.NewDecoder(bytes.NewReader(body))
	// Tolerate non-UTF-8 sitemaps by accepting any declared
	// charset and reading the bytes verbatim. encoding/xml refuses
	// to parse a document with a non-UTF-8 charset declaration
	// unless CharsetReader is set; returning the input reader
	// unchanged is the documented "I trust the bytes, just parse
	// them" pattern. Sitemap structural markup is ASCII, so any
	// non-ASCII in <loc> bodies passes through to the caller as
	// the raw bytes the upstream sent.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	for {
		tok, terr := dec.Token()
		if terr != nil {
			return nil, nil, nil
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "urlset":
			entries, err = decodeURLSet(dec)
			return entries, nil, err
		case "sitemapindex":
			nested, err = decodeSitemapIndex(dec)
			return nil, nested, err
		case "rss":
			entries, err = decodeRSS(dec)
			return entries, nil, err
		case "feed":
			entries, err = decodeAtom(dec)
			return entries, nil, err
		default:
			return nil, nil, nil
		}
	}
}

// decodeURLSet walks the children of an already-consumed <urlset>
// start element, returning each <url><loc>...</loc>...</url> as a
// SitemapEntry. Stops at sitemapMaxEntries to bound memory under
// abuse; entries beyond the cap are silently dropped (the caller
// already chose a max-pages policy at a higher layer).
func decodeURLSet(dec *xml.Decoder) ([]SitemapEntry, error) {
	var out []SitemapEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if strings.ToLower(t.Name.Local) != "url" {
				if err := dec.Skip(); err != nil {
					return out, fmt.Errorf("sitemap: skip stray element: %w", err)
				}
				continue
			}
			var entry struct {
				Loc     string `xml:"loc"`
				LastMod string `xml:"lastmod"`
			}
			if err := dec.DecodeElement(&entry, &t); err != nil {
				// One bad <url> shouldn't kill the rest of the
				// sitemap — keep going.
				continue
			}
			loc := strings.TrimSpace(entry.Loc)
			if loc == "" {
				continue
			}
			out = append(out, SitemapEntry{Loc: loc, LastMod: strings.TrimSpace(entry.LastMod)})
			if len(out) >= sitemapMaxEntries {
				return out, nil
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "urlset" {
				return out, nil
			}
		}
	}
}

// decodeSitemapIndex walks the children of an already-consumed
// <sitemapindex> start element, returning each <sitemap><loc>...</loc>
// </sitemap> as a string. Same bounding behaviour as decodeURLSet.
func decodeSitemapIndex(dec *xml.Decoder) ([]string, error) {
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if strings.ToLower(t.Name.Local) != "sitemap" {
				if err := dec.Skip(); err != nil {
					return out, fmt.Errorf("sitemap: skip stray element: %w", err)
				}
				continue
			}
			var entry struct {
				Loc string `xml:"loc"`
			}
			if err := dec.DecodeElement(&entry, &t); err != nil {
				continue
			}
			loc := strings.TrimSpace(entry.Loc)
			if loc == "" {
				continue
			}
			out = append(out, loc)
			if len(out) >= sitemapMaxEntries {
				return out, nil
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "sitemapindex" {
				return out, nil
			}
		}
	}
}

// decodeRSS walks an RSS 2.0 feed used as a sitemap, returning one
// SitemapEntry per <item> with its <link> as the URL and <pubDate>
// (if any) as LastMod. Only <item> elements are decoded, so the
// channel-level <link> (the site homepage) and <atom:link
// rel="self"> are correctly skipped — they're not items. <link>
// bodies are commonly wrapped in CDATA, which encoding/xml unwraps
// transparently. Same bounding behaviour as decodeURLSet.
func decodeRSS(dec *xml.Decoder) ([]SitemapEntry, error) {
	var out []SitemapEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, nil
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		// Descend through <channel> token-by-token; only <item>
		// subtrees are pulled out. Non-item elements at the channel
		// level (title, link, description, atom:link, …) fall
		// through without their bodies being mistaken for URLs.
		if strings.ToLower(start.Name.Local) != "item" {
			continue
		}
		var item struct {
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
		}
		if err := dec.DecodeElement(&item, &start); err != nil {
			continue
		}
		loc := strings.TrimSpace(item.Link)
		if loc == "" {
			continue
		}
		out = append(out, SitemapEntry{Loc: loc, LastMod: strings.TrimSpace(item.PubDate)})
		if len(out) >= sitemapMaxEntries {
			return out, nil
		}
	}
}

// decodeAtom walks an Atom 1.0 feed used as a sitemap, returning
// one SitemapEntry per <entry>. The URL is the href ATTRIBUTE of
// the entry's <link> (Atom puts the URL in an attribute, unlike
// RSS's element body); when an entry carries multiple <link>s
// (alternate / self / enclosure), the rel="alternate" one wins,
// falling back to a link with no rel, then to the first link.
// <updated> maps to LastMod. Same bounding behaviour as the rest.
func decodeAtom(dec *xml.Decoder) ([]SitemapEntry, error) {
	var out []SitemapEntry
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, nil
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if strings.ToLower(start.Name.Local) != "entry" {
			continue
		}
		var entry struct {
			Links []struct {
				Href string `xml:"href,attr"`
				Rel  string `xml:"rel,attr"`
			} `xml:"link"`
			Updated string `xml:"updated"`
		}
		if err := dec.DecodeElement(&entry, &start); err != nil {
			continue
		}
		loc := pickAtomLink(entry.Links)
		if loc == "" {
			continue
		}
		out = append(out, SitemapEntry{Loc: loc, LastMod: strings.TrimSpace(entry.Updated)})
		if len(out) >= sitemapMaxEntries {
			return out, nil
		}
	}
}

// pickAtomLink chooses the canonical URL from an Atom entry's
// <link> set: rel="alternate" first (the entry's main page),
// then a link with no rel (Atom defaults an absent rel to
// "alternate"), then the first link as a last resort. Returns ""
// when the entry has no usable href.
func pickAtomLink(links []struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}) string {
	var noRel, first string
	for i, l := range links {
		href := strings.TrimSpace(l.Href)
		if href == "" {
			continue
		}
		if i == 0 {
			first = href
		}
		switch strings.ToLower(strings.TrimSpace(l.Rel)) {
		case "alternate":
			return href
		case "":
			if noRel == "" {
				noRel = href
			}
		}
	}
	if noRel != "" {
		return noRel
	}
	return first
}
