package siteassets

import (
	"context"
	"testing"
	"time"
)

// fixedLoader returns a manifest with the given assets map for every
// (orgID, siteID) — simpler than wiring per-key fixtures when the
// tests only exercise one scope.
func fixedLoader(assets map[string]string) Loader {
	return func(_ context.Context, _, _ string) ([]byte, error) {
		raw := `{"_v":1,"assets":{`
		first := true
		for k, v := range assets {
			if !first {
				raw += ","
			}
			raw += `"` + k + `":"` + v + `"`
			first = false
		}
		raw += `}}`
		return []byte(raw), nil
	}
}

func TestMakeCDNURL_matchingPath_resolvesViaManifest(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{
		"logo.png": "logo-abc12345.png",
	}), time.Minute)

	cdnURL := MakeCDNURL(cache)
	got := cdnURL("cloud/assets/hb_org_x/hb_site_y/logo.png")
	want := "cloud/assets/hb_org_x/hb_site_y/logo-abc12345.png"
	if got != want {
		t.Fatalf("rewrite: want %q, got %q", want, got)
	}
}

func TestMakeCDNURL_matchingPath_subdir(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{
		"img/hero.jpg": "img/hero-def67890.jpg",
	}), time.Minute)

	cdnURL := MakeCDNURL(cache)
	got := cdnURL("cloud/assets/hb_org_x/hb_site_y/img/hero.jpg")
	want := "cloud/assets/hb_org_x/hb_site_y/img/hero-def67890.jpg"
	if got != want {
		t.Fatalf("subdir rewrite: want %q, got %q", want, got)
	}
}

func TestMakeCDNURL_matchingPath_assetMissingFromManifest(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{}), time.Minute)

	cdnURL := MakeCDNURL(cache)
	in := "cloud/assets/hb_org_x/hb_site_y/logo.png"
	if got := cdnURL(in); got != in {
		t.Fatalf("missing asset: want passthrough %q, got %q", in, got)
	}
}

func TestMakeCDNURL_matchingPath_manifestAbsent(t *testing.T) {
	t.Parallel()
	// Loader returns (nil, nil) — no manifest exists for the scope.
	cache := NewManifestCache(func(context.Context, string, string) ([]byte, error) {
		return nil, nil
	}, time.Minute)

	cdnURL := MakeCDNURL(cache)
	in := "cloud/assets/hb_org_x/hb_site_y/logo.png"
	if got := cdnURL(in); got != in {
		t.Fatalf("absent manifest: want passthrough %q, got %q", in, got)
	}
}

func TestMakeCDNURL_nonMatchingPath_passesThrough(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{}), time.Minute)

	cdnURL := MakeCDNURL(cache)
	cases := []string{
		"http://example.com/foo.png",        // absolute URL
		"uikit/css/tokens.css",              // ExtraStatic mount
		"img/hblabs.png",                    // single-segment relative
		"cloud/assets/hb_org_x/no_site_yet", // wrong second segment
		"cloud/assets/no_org/hb_site_y/x",   // wrong first segment
	}
	for _, in := range cases {
		if got := cdnURL(in); got != in {
			t.Fatalf("non-matching %q: want passthrough, got %q", in, got)
		}
	}
}

func TestMakeCDNURL_nilCache_fallsThroughToBuildCDNURL(t *testing.T) {
	t.Parallel()
	cdnURL := MakeCDNURL(nil)
	in := "cloud/assets/hb_org_x/hb_site_y/logo.png"
	if got := cdnURL(in); got != in {
		t.Fatalf("nil cache: want passthrough %q, got %q", in, got)
	}
}

func TestFuncMap_exposesHelpers(t *testing.T) {
	t.Parallel()
	fm := FuncMap(NewManifestCache(fixedLoader(map[string]string{}), time.Minute))
	for _, name := range []string{"cdnURL", "siteAsset"} {
		if _, ok := fm[name]; !ok {
			t.Fatalf("FuncMap missing helper %q", name)
		}
	}
}

func TestFuncMap_siteAsset_resolved(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{
		"logo.png": "logo-abc12345.png",
	}), time.Minute)
	fm := FuncMap(cache)
	fn := fm["siteAsset"].(func(string, string, string) string)
	got := fn("hb_org_x", "hb_site_y", "logo.png")
	want := "cloud/assets/hb_org_x/hb_site_y/logo-abc12345.png"
	if got != want {
		t.Fatalf("siteAsset resolved: want %q, got %q", want, got)
	}
}

func TestFuncMap_siteAsset_fallback(t *testing.T) {
	t.Parallel()
	cache := NewManifestCache(fixedLoader(map[string]string{}), time.Minute)
	fm := FuncMap(cache)
	fn := fm["siteAsset"].(func(string, string, string) string)
	got := fn("hb_org_x", "hb_site_y", "missing.png")
	want := "cloud/assets/hb_org_x/hb_site_y/missing.png"
	if got != want {
		t.Fatalf("siteAsset fallback: want %q, got %q", want, got)
	}
}
