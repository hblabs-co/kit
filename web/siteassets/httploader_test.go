package siteassets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		endpoint string
		useSSL   bool
		want     string
	}{
		{"bare host, no ssl", "localhost:9000", false, "http://localhost:9000"},
		{"bare host, ssl", "localhost:9000", true, "https://localhost:9000"},
		{"scheme verbatim", "https://s3.hblabs.co", false, "https://s3.hblabs.co"},
		{"trailing slash trimmed", "https://s3.hblabs.co/", false, "https://s3.hblabs.co"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeBaseURL(tc.endpoint, tc.useSSL); got != tc.want {
				t.Fatalf("normalizeBaseURL(%q,%v) = %q, want %q",
					tc.endpoint, tc.useSSL, got, tc.want)
			}
		})
	}
}

const sampleManifest = `{"_v":1,"assets":{"logo.svg":"logo-fc05a347.svg"}}`

// manifestServer serves the manifest path with the given status/body.
func manifestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/media/assets/org/site/"+ManifestBasename {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPLoader_OK(t *testing.T) {
	t.Parallel()
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	raw, err := httpLoader(srv.URL, "media")(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(raw) != sampleManifest {
		t.Fatalf("body = %q, want %q", raw, sampleManifest)
	}
}

func TestHTTPLoader_NotFound(t *testing.T) {
	t.Parallel()
	srv := manifestServer(t, http.StatusNotFound, "nope")
	raw, err := httpLoader(srv.URL, "media")(context.Background(), "org", "site")
	if err != nil || raw != nil {
		t.Fatalf("404: want (nil, nil), got (%v, %v)", raw, err)
	}
}

func TestHTTPLoader_OtherStatusIsError(t *testing.T) {
	t.Parallel()
	srv := manifestServer(t, http.StatusForbidden, "denied")
	_, err := httpLoader(srv.URL, "media")(context.Background(), "org", "site")
	if err == nil {
		t.Fatal("403: want an error, got nil")
	}
}

func TestHTTPLoader_BadURL(t *testing.T) {
	t.Parallel()
	// A control char makes http.NewRequestWithContext fail at url.Parse.
	_, err := httpLoader("http://\x7f", "media")(context.Background(), "org", "site")
	if err == nil {
		t.Fatal("bad URL: want an error, got nil")
	}
}

func TestHTTPLoader_DoError(t *testing.T) {
	t.Parallel()
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the request → client.Do returns an error
	_, err := httpLoader(srv.URL, "media")(ctx, "org", "site")
	if err == nil {
		t.Fatal("cancelled ctx: want an error, got nil")
	}
}

func TestHTTPLoader_ReadAllError(t *testing.T) {
	// NOT t.Parallel(): swaps the package-global readAll seam. Runs in
	// the sequential phase (parallel tests are paused) so no concurrent
	// reader observes the swap; cleanup restores before they resume.
	orig := readAll
	readAll = func(io.Reader) ([]byte, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { readAll = orig })

	srv := manifestServer(t, http.StatusOK, sampleManifest)
	_, err := httpLoader(srv.URL, "media")(context.Background(), "org", "site")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("readAll error: want boom, got %v", err)
	}
}

func TestNewHTTPManifestCache_Disabled(t *testing.T) {
	t.Parallel()
	if NewHTTPManifestCache("", "media", false, time.Minute) != nil {
		t.Fatal("empty endpoint: want nil cache")
	}
	if NewHTTPManifestCache("https://s3.hblabs.co", "", false, time.Minute) != nil {
		t.Fatal("empty bucket: want nil cache")
	}
}

func TestNewHTTPManifestCache_Resolves(t *testing.T) {
	t.Parallel()
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	cache := NewHTTPManifestCache(srv.URL, "media", false, time.Minute)
	if cache == nil {
		t.Fatal("want non-nil cache")
	}
	m, err := cache.Get(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Resolve("logo.svg"); got != "logo-fc05a347.svg" {
		t.Fatalf("Resolve(logo.svg) = %q, want logo-fc05a347.svg", got)
	}
}

// resolvesVia asserts NewAssetsCache built a working cache (i.e. it
// picked the endpoint that points at the test manifest server).
func resolvesVia(t *testing.T) {
	t.Helper()
	c := NewAssetsCache(time.Minute)
	if c == nil {
		t.Fatal("want non-nil cache")
	}
	m, err := c.Get(context.Background(), "org", "site")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Resolve("logo.svg"); got != "logo-fc05a347.svg" {
		t.Fatalf("Resolve(logo.svg) = %q, want logo-fc05a347.svg", got)
	}
}

// NewAssetsCache must resolve the manifest from the PUBLIC endpoint,
// in precedence HB_MINIO_PUBLIC_ENDPOINT > HB_CDN_BASE >
// HB_MINIO_ENDPOINT — the bug was a public consumer using the
// private endpoint and silently 404-ing.
func TestNewAssetsCache_PublicEndpointWins(t *testing.T) {
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	t.Setenv(constants.EnvMinIOBucket, "media")
	t.Setenv(constants.EnvMinIOPublicEndpoint, srv.URL)
	t.Setenv(constants.EnvCDNBase, "http://127.0.0.1:1")       // bogus
	t.Setenv(constants.EnvMinIOEndpoint, "http://127.0.0.1:1") // bogus
	resolvesVia(t)                                             // success proves it used the public endpoint
}

func TestNewAssetsCache_FallsBackToCDNBase(t *testing.T) {
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	t.Setenv(constants.EnvMinIOBucket, "media")
	t.Setenv(constants.EnvMinIOPublicEndpoint, "")
	t.Setenv(constants.EnvCDNBase, srv.URL)
	t.Setenv(constants.EnvMinIOEndpoint, "http://127.0.0.1:1") // bogus
	resolvesVia(t)
}

func TestNewAssetsCache_FallsBackToMinIOEndpoint(t *testing.T) {
	srv := manifestServer(t, http.StatusOK, sampleManifest)
	t.Setenv(constants.EnvMinIOBucket, "media")
	t.Setenv(constants.EnvMinIOPublicEndpoint, "")
	t.Setenv(constants.EnvCDNBase, "")
	t.Setenv(constants.EnvMinIOEndpoint, srv.URL)
	resolvesVia(t)
}

func TestNewAssetsCache_DisabledWhenNoPublicBase(t *testing.T) {
	t.Setenv(constants.EnvMinIOBucket, "media")
	t.Setenv(constants.EnvMinIOPublicEndpoint, "")
	t.Setenv(constants.EnvCDNBase, "")
	t.Setenv(constants.EnvMinIOEndpoint, "")
	if NewAssetsCache(time.Minute) != nil {
		t.Fatal("no public base configured: want nil cache")
	}
}
