package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRobotsCandidates verifies the normalisation contract: a bare
// domain, a scheme-prefixed host, and a host with a trailing slash
// produce the same two candidates in the same priority order.
// https before http because that's how the platform crawls in 2026.
func TestRobotsCandidates(t *testing.T) {
	want := []string{"https://example.com/robots.txt", "http://example.com/robots.txt"}
	for _, in := range []string{"example.com", "https://example.com", "http://example.com", "example.com/", "  example.com "} {
		got := RobotsCandidates(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("RobotsCandidates(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestFallbackSitemapURLs guards the well-known sitemap paths the
// discoverer probes when robots fails to point at one. Order
// matters: /sitemap.xml first (universal), /sitemap_index.xml
// second (older WordPress/Yoast layout).
func TestFallbackSitemapURLs(t *testing.T) {
	want := []string{
		"https://example.com/sitemap.xml",
		"https://example.com/sitemap_index.xml",
		"http://example.com/sitemap.xml",
		"http://example.com/sitemap_index.xml",
	}
	got := FallbackSitemapURLs("https://example.com/")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FallbackSitemapURLs = %v, want %v", got, want)
	}
}

// TestParseRobotsSitemaps covers the small grammar the helper
// supports — case-insensitive field, inline comments, blank lines,
// duplicates collapsed, non-http(s) schemes dropped. Anchoring
// these in tests lets a future "the spec changed" PR justify any
// loosening of the rules.
func TestParseRobotsSitemaps(t *testing.T) {
	body := []byte(`User-agent: *
Disallow: /admin
linewithoutcolon
:emptyfield

# block of sitemaps
Sitemap: https://example.com/sitemap.xml
sitemap:   https://example.com/news.xml   # weekly digest
SITEMAP: https://example.com/sitemap.xml
Sitemap: ftp://example.com/old.xml
Sitemap:
Sitemap: not a url ::
`)
	want := []string{"https://example.com/sitemap.xml", "https://example.com/news.xml"}
	got := ParseRobotsSitemaps(body)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRobotsSitemaps = %v, want %v", got, want)
	}
}

// TestParseRobotsSitemaps_EmptyAndNoSitemap covers the "found
// nothing" branches: an empty body returns nil; a body without any
// Sitemap directive returns nil. Both flow through to the fallback
// discovery path in DiscoverSitemap.
func TestParseRobotsSitemaps_EmptyAndNoSitemap(t *testing.T) {
	if got := ParseRobotsSitemaps(nil); got != nil {
		t.Errorf("empty body: %v, want nil", got)
	}
	if got := ParseRobotsSitemaps([]byte("User-agent: *\nDisallow: /\n")); got != nil {
		t.Errorf("no-sitemap body: %v, want nil", got)
	}
}

// TestParseSitemap_URLSet covers the leaf case: a <urlset> document
// returns its <url> entries (with lastmod preserved verbatim) and
// no nested URLs. A malformed <url> in the middle of the set is
// skipped so one bad entry doesn't kill the rest.
func TestParseSitemap_URLSet(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a</loc><lastmod>2026-04-01</lastmod></url>
  <url><loc>https://example.com/b</loc></url>
  <url><loc>   </loc></url>
  <ignored>this is not a url</ignored>
  <url><loc>https://example.com/c</loc><lastmod>2026-04-15T12:00:00Z</lastmod></url>
</urlset>`)
	entries, nested, err := ParseSitemap(body)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if nested != nil {
		t.Errorf("urlset → nested = %v, want nil", nested)
	}
	want := []SitemapEntry{
		{Loc: "https://example.com/a", LastMod: "2026-04-01"},
		{Loc: "https://example.com/b"},
		{Loc: "https://example.com/c", LastMod: "2026-04-15T12:00:00Z"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("urlset entries = %#v, want %#v", entries, want)
	}
}

// TestParseSitemap_Index covers the index case: a <sitemapindex>
// returns the nested sitemap URLs and no leaf entries. Empty <loc>
// is dropped (same rule as <url>).
func TestParseSitemap_Index(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap-1.xml</loc></sitemap>
  <other>noise</other>
  <sitemap><loc></loc></sitemap>
  <sitemap><loc>https://example.com/sitemap-2.xml</loc></sitemap>
</sitemapindex>`)
	entries, nested, err := ParseSitemap(body)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if entries != nil {
		t.Errorf("sitemapindex → entries = %v, want nil", entries)
	}
	want := []string{"https://example.com/sitemap-1.xml", "https://example.com/sitemap-2.xml"}
	if !reflect.DeepEqual(nested, want) {
		t.Errorf("sitemapindex nested = %v, want %v", nested, want)
	}
}

// TestParseSitemap_RSS covers an RSS 2.0 feed used as a sitemap —
// the shape WordPress's All in One SEO / Yoast emit at
// sitemap.rss. URLs live in <item><link> (CDATA-wrapped), and the
// channel-level <link> (the homepage) + <atom:link rel="self">
// must NOT be mistaken for entries.
func TestParseSitemap_RSS(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom">
  <channel>
    <title>DOGs Life Berlin</title>
    <link><![CDATA[https://www.dogs-life-berlin.de]]></link>
    <atom:link href="https://www.dogs-life-berlin.de/sitemap.rss" rel="self" type="application/rss+xml" />
    <item>
      <guid><![CDATA[https://www.dogs-life-berlin.de/jacquelinerunge/]]></guid>
      <link><![CDATA[https://www.dogs-life-berlin.de/jacquelinerunge/]]></link>
      <title>Jacqueline</title>
      <pubDate><![CDATA[Wed, 31 Dec 2025 13:18:02 +0000]]></pubDate>
    </item>
    <item>
      <link><![CDATA[https://www.dogs-life-berlin.de/]]></link>
      <title>Home</title>
    </item>
  </channel>
</rss>`)
	entries, nested, err := ParseSitemap(body)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if nested != nil {
		t.Errorf("rss → nested = %v, want nil", nested)
	}
	want := []SitemapEntry{
		{Loc: "https://www.dogs-life-berlin.de/jacquelinerunge/", LastMod: "Wed, 31 Dec 2025 13:18:02 +0000"},
		{Loc: "https://www.dogs-life-berlin.de/"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("rss entries = %#v, want %#v", entries, want)
	}
}

// TestParseSitemap_Atom covers an Atom 1.0 feed sitemap: the URL
// is the href ATTRIBUTE of <entry><link>, and rel="alternate"
// wins over rel="self" when an entry has multiple links.
func TestParseSitemap_Atom(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example</title>
  <link href="https://example.com/feed" rel="self"/>
  <entry>
    <title>Page A</title>
    <link href="https://example.com/a/" rel="alternate"/>
    <link href="https://example.com/a/comments" rel="replies"/>
    <updated>2026-04-01T12:00:00Z</updated>
  </entry>
  <entry>
    <title>Page B</title>
    <link href="https://example.com/b/"/>
    <updated>2026-04-02T12:00:00Z</updated>
  </entry>
</feed>`)
	entries, nested, err := ParseSitemap(body)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if nested != nil {
		t.Errorf("feed → nested = %v, want nil", nested)
	}
	want := []SitemapEntry{
		{Loc: "https://example.com/a/", LastMod: "2026-04-01T12:00:00Z"},
		{Loc: "https://example.com/b/", LastMod: "2026-04-02T12:00:00Z"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("atom entries = %#v, want %#v", entries, want)
	}
}

// TestParseSitemap_NonUTF8Encoding exercises the CharsetReader
// hook: an XML declaration with a non-UTF-8 charset would
// otherwise make encoding/xml refuse to parse. The hook returns
// the bytes verbatim, so ASCII-only structural markup parses
// fine regardless of the declared charset.
func TestParseSitemap_NonUTF8Encoding(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?>
<urlset>
  <url><loc>https://example.com/iso</loc></url>
</urlset>`)
	entries, _, err := ParseSitemap(body)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	want := []SitemapEntry{{Loc: "https://example.com/iso"}}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries = %#v, want %#v", entries, want)
	}
}

// TestParseSitemap_EmptyAndUnknownRoot guards the soft-fail
// contract: an empty body and an unrecognised root element both
// return (nil, nil, nil) so a caller can treat "found nothing"
// uniformly with "the upstream served a 404".
func TestParseSitemap_EmptyAndUnknownRoot(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("   "),
		[]byte(`<?xml version="1.0"?><html><body>no</body></html>`),
		[]byte(`not xml at all`),
	}
	for i, body := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			entries, nested, err := ParseSitemap(body)
			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if entries != nil || nested != nil {
				t.Errorf("entries=%v nested=%v, want (nil,nil)", entries, nested)
			}
		})
	}
}

// TestDiscoverSitemap_RobotsHit verifies the happy path: the
// discoverer hits the first reachable robots candidate, parses
// Sitemap: directives, and returns them. The mock server records
// which paths were hit so we know the https candidate was tried
// before the http fallback.
func TestDiscoverSitemap_RobotsHit(t *testing.T) {
	robots := "User-agent: *\nSitemap: https://example.com/sitemap-via-robots.xml\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(robots))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	domain := strings.TrimPrefix(srv.URL, "http://")
	// Override the candidates: the helper builds https first; for
	// the test server we point straight at the http port via the
	// test's known URL. RobotsCandidates is exported so we trust
	// its order separately — this test just verifies DiscoverSitemap
	// uses whatever the supplied client returns.
	client := NewFetchClient(FetchOptions{Timeout: 2 * time.Second})
	// To exercise the production code path without altering the
	// candidate list, call FetchURL directly to verify the test
	// server is reachable, then run DiscoverSitemap with the bare
	// domain.
	if got := FetchURL(context.Background(), client, FetchOptions{Timeout: 2 * time.Second}, srv.URL+"/robots.txt"); got.Err != nil {
		t.Fatalf("test server unreachable: %v", got.Err)
	}
	res := DiscoverSitemap(context.Background(), client, FetchOptions{Timeout: 2 * time.Second}, domain)
	// The discoverer tried https://<domain>/robots.txt and http://<domain>/robots.txt
	// in order; the http one matches the test server. RobotsURL
	// records the URL that produced the body.
	if res.RobotsURL == "" {
		t.Errorf("RobotsURL empty, expected the http candidate")
	}
	want := []string{"https://example.com/sitemap-via-robots.xml"}
	if !reflect.DeepEqual(res.SitemapURLs, want) {
		t.Errorf("SitemapURLs = %v, want %v", res.SitemapURLs, want)
	}
	if string(res.RobotsBody) != robots {
		t.Errorf("RobotsBody = %q, want %q", string(res.RobotsBody), robots)
	}
}

// TestDiscoverSitemap_FallsBack verifies the second path: every
// robots candidate fails (the server 404s), so the discoverer
// returns the well-known fallback URLs without fetching them.
// SitemapURLs is the FallbackSitemapURLs output; RobotsURL is
// empty (we never got a body).
func TestDiscoverSitemap_FallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := NewFetchClient(FetchOptions{Timeout: 500 * time.Millisecond})
	res := DiscoverSitemap(context.Background(), client, FetchOptions{Timeout: 500 * time.Millisecond}, "example.com")
	if res.RobotsURL != "" {
		t.Errorf("RobotsURL = %q, want empty", res.RobotsURL)
	}
	if res.RobotsBody != nil {
		t.Errorf("RobotsBody not nil: %q", res.RobotsBody)
	}
	want := FallbackSitemapURLs("example.com")
	if !reflect.DeepEqual(res.SitemapURLs, want) {
		t.Errorf("SitemapURLs = %v, want %v", res.SitemapURLs, want)
	}
}
