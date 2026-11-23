package siteurl

import (
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

// withLAN swaps the banner.LAN seam for the duration of a test so
// the LAN-dependent branches of DevHostRewrite are deterministic
// regardless of the host machine's interfaces.
func withLAN(t *testing.T, lan string) {
	t.Helper()
	prev := lanFn
	lanFn = func() string { return lan }
	t.Cleanup(func() { lanFn = prev })
}

func TestIsDev(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "local")
	if !IsDev() {
		t.Fatal("HB_ENV=local: want dev")
	}
	t.Setenv(constants.EnvHBEnv, "prod")
	if IsDev() {
		t.Fatal("HB_ENV=prod: want not dev")
	}
	// Empty value falls back to the "local" default → dev.
	t.Setenv(constants.EnvHBEnv, "")
	if !IsDev() {
		t.Fatal("HB_ENV unset: want dev (default local)")
	}
}

func TestDevHostRewrite(t *testing.T) {
	tests := []struct {
		name string
		env  string
		lan  string
		in   string
		want string
	}{
		{"empty input", "local", "10.0.0.5", "", ""},
		{"not dev passes through", "prod", "10.0.0.5", "http://localhost:9000/x", "http://localhost:9000/x"},
		{"no LAN passes through", "local", "", "http://localhost/x", "http://localhost/x"},
		{"unparseable passes through", "local", "10.0.0.5", "http://localhost\x7f/x", "http://localhost\x7f/x"},
		{"non-local host passes through", "local", "10.0.0.5", "http://example.com/x", "http://example.com/x"},
		{"localhost with port rewritten", "local", "10.0.0.5", "http://localhost:9000/a?b=c", "http://10.0.0.5:9000/a?b=c"},
		{"127.0.0.1 no port rewritten", "local", "10.0.0.5", "http://127.0.0.1/a", "http://10.0.0.5/a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvHBEnv, tc.env)
			withLAN(t, tc.lan)
			if got := DevHostRewrite(tc.in); got != tc.want {
				t.Fatalf("DevHostRewrite(%q): want %q, got %q", tc.in, tc.want, got)
			}
		})
	}
}

// clearAssetBaseEnvs zeroes the three vars that feed resolvedCDNBase
// so each subtest starts from a known empty state and can opt in to
// exactly the cascade rung it's exercising. The parent process may
// have any of them set (Helmer's laptop, CI runner, …); without this
// reset the tests would non-deterministically resolve a real value.
func clearAssetBaseEnvs(t *testing.T) {
	t.Helper()
	t.Setenv(constants.EnvCDNBase, "")
	t.Setenv(constants.EnvMinIOPublicEndpoint, "")
	t.Setenv(constants.EnvMinIOEndpoint, "")
}

func TestBuildCDNURL(t *testing.T) {
	// HB_ENV=prod keeps DevHostRewrite a no-op so the join logic is
	// asserted without LAN-rewrite interference.
	t.Setenv(constants.EnvHBEnv, "prod")
	clearAssetBaseEnvs(t)

	t.Run("empty path", func(t *testing.T) {
		if got := BuildCDNURL(""); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("absolute http passes through", func(t *testing.T) {
		if got := BuildCDNURL("http://x/y"); got != "http://x/y" {
			t.Fatalf("want unchanged, got %q", got)
		}
	})
	t.Run("absolute https passes through", func(t *testing.T) {
		if got := BuildCDNURL("https://x/y"); got != "https://x/y" {
			t.Fatalf("want unchanged, got %q", got)
		}
	})
	t.Run("no base resolves → path verbatim", func(t *testing.T) {
		clearAssetBaseEnvs(t)
		if got := BuildCDNURL("img/a.png"); got != "img/a.png" {
			t.Fatalf("want verbatim, got %q", got)
		}
	})
	t.Run("HB_CDN_BASE wins (preferred)", func(t *testing.T) {
		t.Setenv(constants.EnvCDNBase, "https://cdn.example.com/")
		if got := BuildCDNURL("/img/a.png"); got != "https://cdn.example.com/img/a.png" {
			t.Fatalf("want joined, got %q", got)
		}
	})
	t.Run("cascades to MINIO_PUBLIC when CDN empty", func(t *testing.T) {
		clearAssetBaseEnvs(t)
		t.Setenv(constants.EnvMinIOPublicEndpoint, "https://s3.example.com/media/assets")
		if got := BuildCDNURL("img/a.png"); got != "https://s3.example.com/media/assets/img/a.png" {
			t.Fatalf("want public-endpoint join, got %q", got)
		}
	})
	t.Run("cascades to MINIO_ENDPOINT when CDN and PUBLIC empty", func(t *testing.T) {
		clearAssetBaseEnvs(t)
		t.Setenv(constants.EnvMinIOEndpoint, "http://minio.internal:9000/media/assets")
		if got := BuildCDNURL("img/a.png"); got != "http://minio.internal:9000/media/assets/img/a.png" {
			t.Fatalf("want endpoint join, got %q", got)
		}
	})
	t.Run("CDN_BASE preferred over MINIO_PUBLIC", func(t *testing.T) {
		t.Setenv(constants.EnvCDNBase, "https://cdn.example.com")
		t.Setenv(constants.EnvMinIOPublicEndpoint, "https://s3.example.com")
		if got := BuildCDNURL("img/a.png"); got != "https://cdn.example.com/img/a.png" {
			t.Fatalf("CDN_BASE must win, got %q", got)
		}
	})
}

func TestExtraImgSrc(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "prod") // DevHostRewrite no-op in prod
	clearAssetBaseEnvs(t)

	t.Run("CDN_BASE set", func(t *testing.T) {
		t.Setenv(constants.EnvCDNBase, "https://cdn.example.com")
		got := ExtraImgSrc()
		if len(got) != 1 || got[0] != "https://cdn.example.com" {
			t.Fatalf("want [https://cdn.example.com], got %v", got)
		}
	})
	t.Run("no base resolves → nil", func(t *testing.T) {
		clearAssetBaseEnvs(t)
		if got := ExtraImgSrc(); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})
	t.Run("cascades to MINIO_PUBLIC", func(t *testing.T) {
		clearAssetBaseEnvs(t)
		t.Setenv(constants.EnvMinIOPublicEndpoint, "https://s3.example.com")
		got := ExtraImgSrc()
		if len(got) != 1 || got[0] != "https://s3.example.com" {
			t.Fatalf("want [https://s3.example.com], got %v", got)
		}
	})
}
