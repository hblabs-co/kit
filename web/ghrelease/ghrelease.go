// Package ghrelease resolves the newest downloadable asset of a GitHub
// repository from the Releases API, so a landing page's download
// button follows every new release without a redeploy.
//
// The Resolver keeps the last good answer in memory. Callers on the
// hot path (template helpers) read it with Peek, which never touches
// the network; the download handler calls Latest, which refreshes the
// cache once it is older than the TTL. When GitHub is unreachable,
// rate-limited or returns a release without a matching asset, the
// resolver keeps serving the last good release, and before the first
// successful fetch it answers with the repository's releases/latest
// page, which always lists the newest assets.
//
// Wiring:
//
//	resolver, err := ghrelease.New(ghrelease.Options{
//	    Repo:  "owner/app",
//	    Asset: ghrelease.PrefixSuffix("App-", ".dmg"),
//	})
//	mux.Handle("GET /download", resolver.Handler())
//	modules = append(modules, &ghrelease.Refresher{Resolver: resolver})
package ghrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
)

// Defaults applied by New.
const (
	// DefaultTTL is how long a fetched release is served before the
	// next Latest call refreshes it.
	DefaultTTL = 10 * time.Minute
	// DefaultUserAgent identifies the client to the GitHub API when
	// no service was booted (see Options.UserAgent).
	DefaultUserAgent = "hblabs-kit-ghrelease/1 (+https://hblabs.co)"
	// DefaultCacheControl is set on the Handler's redirect: short, so
	// CDNs and browsers do not pin an old version for long.
	DefaultCacheControl = "public, max-age=300"

	// retryAfter is how long a failed fetch suppresses the next
	// attempt, so a GitHub outage or the unauthenticated rate limit
	// (60 requests/hour per IP) is not hammered by every visitor.
	retryAfter   = time.Minute
	fetchTimeout = 5 * time.Second
	// maxBody caps the API response; a release JSON is a few KB.
	maxBody    = 1 << 20
	acceptJSON = "application/vnd.github+json"
)

// AssetMatcher reports whether an asset name is the download and, if
// so, the version it carries (shown on the page, e.g. "1.2.3").
type AssetMatcher func(name string) (version string, ok bool)

// PrefixSuffix matches assets named <prefix><version><suffix>, such
// as "App-1.2.3.dmg" for PrefixSuffix("App-", ".dmg"). The version
// part must not be empty.
func PrefixSuffix(prefix, suffix string) AssetMatcher {
	return func(name string) (string, bool) {
		if len(name) <= len(prefix)+len(suffix) ||
			!strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			return "", false
		}
		return name[len(prefix) : len(name)-len(suffix)], true
	}
}

// Release is one resolved download. Version is empty when the
// resolver has never fetched successfully; URL then points at the
// releases/latest page instead of an asset.
type Release struct {
	Version string
	URL     string
}

// Resolved reports whether r carries a concrete asset (as opposed to
// the releases-page fallback).
func (r Release) Resolved() bool { return r.Version != "" }

// AtLeast reports whether r.Version is at least minimum. Both are
// dotted numeric versions ("1.2.3"); a leading "v" and a pre-release
// suffix after '-' are ignored, missing components count as 0.
// Unresolved or unparseable versions answer false, so callers fall
// back to neutral copy.
func (r Release) AtLeast(minimum string) bool {
	v, ok := parseVersion(r.Version)
	m, okMin := parseVersion(minimum)
	if !ok || !okMin {
		return false
	}
	for i := 0; i < max(len(v), len(m)); i++ {
		a, b := at(v, i), at(m, i)
		if a != b {
			return a > b
		}
	}
	return true
}

func parseVersion(s string) ([]int, bool) {
	core, _, _ := strings.Cut(strings.TrimPrefix(s, "v"), "-")
	if core == "" {
		return nil, false
	}
	parts := strings.Split(core, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

func at(v []int, i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// Options configures a Resolver.
type Options struct {
	// Repo is "owner/name". It derives APIURL and FallbackURL when
	// those are empty.
	Repo string
	// Asset picks the download among the release's assets. Required.
	Asset AssetMatcher
	// APIURL overrides the latest-release API endpoint.
	APIURL string
	// FallbackURL overrides the page served before the first
	// successful fetch.
	FallbackURL string
	// TTL defaults to DefaultTTL.
	TTL time.Duration
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// UserAgent defaults to "HB Labs <service>/<version>
	// (+https://hblabs.co)" from system.ServiceName and the build
	// version ("dev" when unstamped), or DefaultUserAgent when
	// system.Boot has not run.
	UserAgent string
	// CacheControl for the Handler's redirect; defaults to
	// DefaultCacheControl.
	CacheControl string
}

// defaultUserAgent names the booted service and its build version.
func defaultUserAgent() string {
	svc := system.ServiceName()
	if svc == "" {
		return DefaultUserAgent
	}
	version := system.ReadBuildInfo().Version
	if version == "" {
		version = "dev"
	}
	return "HB Labs " + svc + "/" + version + " (+https://hblabs.co)"
}

// Resolver caches the latest release. Safe for concurrent use.
type Resolver struct {
	apiURL       string
	fallbackURL  string
	asset        AssetMatcher
	ttl          time.Duration
	client       *http.Client
	userAgent    string
	cacheControl string
	now          func() time.Time

	mu        sync.Mutex
	cached    Release
	fetchedAt time.Time
	failedAt  time.Time
}

// Errors returned by New.
var (
	ErrNoAsset = errors.New("ghrelease: Options.Asset is required")
	ErrNoRepo  = errors.New("ghrelease: Options.Repo is required unless APIURL and FallbackURL are set")
)

// New builds a Resolver from opts.
func New(opts Options) (*Resolver, error) {
	if opts.Asset == nil {
		return nil, ErrNoAsset
	}
	r := &Resolver{
		apiURL:       opts.APIURL,
		fallbackURL:  opts.FallbackURL,
		asset:        opts.Asset,
		ttl:          opts.TTL,
		client:       opts.Client,
		userAgent:    opts.UserAgent,
		cacheControl: opts.CacheControl,
		now:          time.Now,
	}
	if r.apiURL == "" || r.fallbackURL == "" {
		if strings.Count(opts.Repo, "/") != 1 || strings.HasPrefix(opts.Repo, "/") || strings.HasSuffix(opts.Repo, "/") {
			return nil, ErrNoRepo
		}
		if r.apiURL == "" {
			r.apiURL = "https://api.github.com/repos/" + opts.Repo + "/releases/latest"
		}
		if r.fallbackURL == "" {
			r.fallbackURL = "https://github.com/" + opts.Repo + "/releases/latest"
		}
	}
	if r.ttl <= 0 {
		r.ttl = DefaultTTL
	}
	if r.client == nil {
		r.client = http.DefaultClient
	}
	if r.userAgent == "" {
		r.userAgent = defaultUserAgent()
	}
	if r.cacheControl == "" {
		r.cacheControl = DefaultCacheControl
	}
	uaSrc := system.SourceOption
	if opts.UserAgent == "" {
		uaSrc = system.SourceDefault
	}
	log.Info(system.DefaultsMsg, "area", "ghrelease",
		"repo", opts.Repo, "api_url", r.apiURL, "fallback_url", r.fallbackURL,
		"user_agent", r.userAgent, "user_agent_source", uaSrc,
		"ttl", r.ttl.String(), "cache_control", r.cacheControl)
	return r, nil
}

// Peek returns the cached release without any network I/O, or the
// fallback when nothing has been fetched yet.
func (r *Resolver) Peek() Release {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentLocked()
}

// Latest returns the cached release, refreshing it first when the
// cache is older than the TTL and no failed attempt happened within
// the last minute. It never returns an error: failures degrade to
// the last good release, then to the fallback page.
func (r *Resolver) Latest(ctx context.Context) Release {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	fresh := r.cached.Resolved() && now.Sub(r.fetchedAt) < r.ttl
	backoff := !r.failedAt.IsZero() && now.Sub(r.failedAt) < retryAfter
	if fresh || backoff {
		return r.currentLocked()
	}
	rel, err := r.fetch(ctx)
	if err != nil {
		r.failedAt = now
		return r.currentLocked()
	}
	r.cached, r.fetchedAt, r.failedAt = rel, now, time.Time{}
	return rel
}

// currentLocked returns the cached release or the fallback. Callers
// hold r.mu.
func (r *Resolver) currentLocked() Release {
	if r.cached.Resolved() {
		return r.cached
	}
	return Release{URL: r.fallbackURL}
}

// apiRelease is the subset of the GitHub release payload we read.
type apiRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// errNoMatch is returned when the latest release carries no matching
// asset (e.g. a release whose assets are still uploading).
var errNoMatch = errors.New("ghrelease: latest release has no matching asset")

func (r *Resolver) fetch(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL, nil)
	if err != nil {
		return Release{}, fmt.Errorf("ghrelease: build request: %w", err)
	}
	req.Header.Set(constants.HeaderAccept, acceptJSON)
	req.Header.Set(constants.HeaderUserAgent, r.userAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("ghrelease: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("ghrelease: fetch: status %d", resp.StatusCode)
	}
	var payload apiRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("ghrelease: decode: %w", err)
	}
	return r.pick(payload)
}

// pick selects the first matching https asset of a published release.
func (r *Resolver) pick(p apiRelease) (Release, error) {
	if p.Draft {
		return Release{}, errNoMatch
	}
	for _, a := range p.Assets {
		if !strings.HasPrefix(a.URL, "https://") {
			continue
		}
		if version, ok := r.asset(a.Name); ok && version != "" {
			return Release{Version: version, URL: a.URL}, nil
		}
	}
	return Release{}, errNoMatch
}

// Handler answers with a 302 to the latest asset (or the releases
// page), with a short shared Cache-Control.
func (r *Resolver) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rel := r.Latest(req.Context())
		w.Header().Set(constants.HeaderCacheControl, r.cacheControl)
		http.Redirect(w, req, rel.URL, http.StatusFound)
	})
}

// Refresher is a system.Module that warms the cache at boot and
// refreshes it every interval, so Peek (used while rendering pages)
// stays current without visitors waiting on GitHub.
type Refresher struct {
	Resolver *Resolver
	// Interval overrides the refresh period (defaults to the
	// resolver's TTL).
	Interval time.Duration
}

// Register starts the refresh loop anchored to ctx and returns
// immediately.
func (m *Refresher) Register(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = m.Resolver.ttl
	}
	go m.loop(ctx, interval)
	return nil
}

func (m *Refresher) loop(ctx context.Context, interval time.Duration) {
	m.Resolver.Latest(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Resolver.Latest(ctx)
		}
	}
}
