package ghrelease

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const goodPayload = `{"tag_name":"v0.1.1","draft":false,"assets":[
 {"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"},
 {"name":"App-0.1.1.dmg","browser_download_url":"https://example.com/App-0.1.1.dmg"}]}`

// fakeAPI serves body with status and counts hits.
func fakeAPI(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Accept") != acceptJSON || r.Header.Get("User-Agent") != DefaultUserAgent {
			t.Errorf("unexpected headers: %v", r.Header)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// clock is a controllable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

var testAsset = PrefixSuffix("App-", ".dmg")

func mustNew(t *testing.T, opts Options) *Resolver {
	t.Helper()
	if opts.Asset == nil {
		opts.Asset = testAsset
	}
	if opts.Repo == "" && (opts.APIURL == "" || opts.FallbackURL == "") {
		opts.Repo = "owner/app"
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTestResolver(t *testing.T, url string, c *clock) *Resolver {
	r := mustNew(t, Options{APIURL: url, FallbackURL: "https://fallback.example/latest", TTL: time.Minute * 10})
	r.now = c.now
	return r
}

func TestNew_Defaults(t *testing.T) {
	r := mustNew(t, Options{Repo: "owner/app"})
	if r.apiURL != "https://api.github.com/repos/owner/app/releases/latest" ||
		r.fallbackURL != "https://github.com/owner/app/releases/latest" ||
		r.ttl != DefaultTTL || r.client != http.DefaultClient ||
		r.userAgent != DefaultUserAgent || r.cacheControl != DefaultCacheControl {
		t.Fatalf("defaults not applied: %+v", r)
	}
}

func TestNew_Errors(t *testing.T) {
	if _, err := New(Options{Repo: "owner/app"}); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("no asset: %v", err)
	}
	for _, repo := range []string{"", "owner", "/app", "owner/", "a/b/c"} {
		if _, err := New(Options{Repo: repo, Asset: testAsset}); !errors.Is(err, ErrNoRepo) {
			t.Errorf("repo %q: %v", repo, err)
		}
	}
	if _, err := New(Options{Asset: testAsset, APIURL: "https://a", FallbackURL: "https://b"}); err != nil {
		t.Fatalf("explicit URLs without repo: %v", err)
	}
}

func TestNew_Overrides(t *testing.T) {
	r := mustNew(t, Options{UserAgent: "ua", CacheControl: "no-store"})
	if r.userAgent != "ua" || r.cacheControl != "no-store" {
		t.Fatalf("overrides lost: %+v", r)
	}
}

func TestPrefixSuffix(t *testing.T) {
	m := PrefixSuffix("App-", ".dmg")
	cases := map[string]string{"App-1.2.3.dmg": "1.2.3", "App-.dmg": "", "App-1.zip": "", "Other-1.dmg": "", "App": ""}
	for name, want := range cases {
		got, ok := m(name)
		if got != want || ok != (want != "") {
			t.Errorf("%q = %q, %v; want %q", name, got, ok, want)
		}
	}
}

func TestPeek_FallbackBeforeFetch(t *testing.T) {
	r := mustNew(t, Options{FallbackURL: "https://fallback.example/latest"})
	got := r.Peek()
	if got.Resolved() || got.URL != "https://fallback.example/latest" {
		t.Fatalf("Peek = %+v", got)
	}
}

func TestLatest_FetchesAndCaches(t *testing.T) {
	srv, hits := fakeAPI(t, http.StatusOK, goodPayload)
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newTestResolver(t, srv.URL, c)

	got := r.Latest(context.Background())
	if got.Version != "0.1.1" || got.URL != "https://example.com/App-0.1.1.dmg" {
		t.Fatalf("Latest = %+v", got)
	}
	if r.Peek() != got {
		t.Fatalf("Peek = %+v, want %+v", r.Peek(), got)
	}
	c.advance(5 * time.Minute)
	r.Latest(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("fresh cache refetched: hits=%d", hits.Load())
	}
	c.advance(6 * time.Minute)
	r.Latest(context.Background())
	if hits.Load() != 2 {
		t.Fatalf("stale cache not refetched: hits=%d", hits.Load())
	}
}

func TestLatest_FailureKeepsLastGoodAndBacksOff(t *testing.T) {
	var status, hits atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(goodPayload))
	}))
	defer srv.Close()
	c := &clock{t: time.Unix(1_000_000, 0)}
	r := newTestResolver(t, srv.URL, c)
	good := r.Latest(context.Background())

	status.Store(http.StatusForbidden)
	c.advance(11 * time.Minute)
	if got := r.Latest(context.Background()); got != good {
		t.Fatalf("after failure = %+v, want last good %+v", got, good)
	}
	c.advance(30 * time.Second)
	r.Latest(context.Background())
	if hits.Load() != 2 {
		t.Fatalf("backoff not honoured: hits=%d", hits.Load())
	}
	status.Store(http.StatusOK)
	c.advance(31 * time.Second)
	r.Latest(context.Background())
	if hits.Load() != 3 || !r.failedAt.IsZero() {
		t.Fatalf("retry after backoff: hits=%d failedAt=%v", hits.Load(), r.failedAt)
	}
}

func TestLatest_ErrorsFallBack(t *testing.T) {
	cases := map[string]string{
		"bad json": `{`,
		"no dmg":   `{"tag_name":"v1","assets":[{"name":"x.zip","browser_download_url":"https://e/x.zip"}]}`,
		"empty":    `{"tag_name":"v1","assets":[]}`,
		"draft":    `{"tag_name":"v1","draft":true,"assets":[{"name":"App-1.0.0.dmg","browser_download_url":"https://e/f.dmg"}]}`,
		"http url": `{"tag_name":"v1","assets":[{"name":"App-1.0.0.dmg","browser_download_url":"http://e/f.dmg"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := fakeAPI(t, http.StatusOK, body)
			r := newTestResolver(t, srv.URL, &clock{t: time.Unix(1, 0)})
			got := r.Latest(context.Background())
			if got.Resolved() || got.URL != "https://fallback.example/latest" {
				t.Fatalf("Latest = %+v", got)
			}
		})
	}
}

func TestFetch_BadURL(t *testing.T) {
	r := newTestResolver(t, "://bad", &clock{})
	if _, err := r.fetch(context.Background()); err == nil {
		t.Fatal("expected request build error")
	}
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("boom")
}

func TestFetch_TransportError(t *testing.T) {
	r := mustNew(t, Options{APIURL: "https://example.invalid", Client: &http.Client{Transport: errTransport{}}})
	if _, err := r.fetch(context.Background()); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestHandler_Redirects(t *testing.T) {
	srv, _ := fakeAPI(t, http.StatusOK, goodPayload)
	r := newTestResolver(t, srv.URL, &clock{t: time.Unix(1, 0)})
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://example.com/App-0.1.1.dmg" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"", "0.1.2", false},
		{"0.1.1", "0.1.2", false},
		{"0.1.2", "0.1.2", true},
		{"0.1.10", "0.1.2", true},
		{"0.2.0", "0.1.2", true},
		{"1.0.0", "0.1.2", true},
		{"0.0.9", "0.1.2", false},
		{"0.1.2-rc.1", "0.1.2", true},
		{"v0.1.2", "0.1.2", true},
		{"0.1", "0.1.2", false},
		{"0.1", "0.1.0", true},
		{"0.1.2.1", "0.1.2", true},
		{"0.x.2", "0.1.2", false},
		{"0.1.2", "bad", false},
		{"-1.0", "0.1", false},
	}
	for _, c := range cases {
		if got := (Release{Version: c.v}).AtLeast(c.min); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

func TestRefresher_WarmsAndRefreshes(t *testing.T) {
	srv, hits := fakeAPI(t, http.StatusOK, goodPayload)
	r := mustNew(t, Options{APIURL: srv.URL, TTL: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	m := &Refresher{Resolver: r, Interval: 5 * time.Millisecond}
	if err := m.Register(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if hits.Load() < 2 {
		t.Fatalf("refresher hits = %d, want >= 2", hits.Load())
	}
	if !r.Peek().Resolved() {
		t.Fatal("cache not warmed")
	}
}

func TestRefresher_DefaultInterval(t *testing.T) {
	srv, hits := fakeAPI(t, http.StatusOK, goodPayload)
	r := mustNew(t, Options{APIURL: srv.URL, TTL: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m := &Refresher{Resolver: r}
	if err := m.Register(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	go func() { cancel(); close(done) }()
	<-done
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}
