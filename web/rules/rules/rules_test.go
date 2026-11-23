package rules

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

func newRuleCtx(req *http.Request) *rulesengine.Context {
	return rulesengine.NewContext(context.Background(), req)
}

// fixedTime pins NowFn for the duration of t so DetectTimeOfDay's
// output is deterministic. Tests that don't pin time accept any
// bucket but verify the shape.
func fixedTime(t *testing.T, when time.Time) {
	t.Helper()
	orig := NowFn
	NowFn = func() time.Time { return when }
	t.Cleanup(func() { NowFn = orig })
}

func TestPickLocaleHonoursAcceptLanguageOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		accept string
		want   string
	}{
		{"", "de"},
		{"fr", "de"},
		{"en;q=1", "en"},
		{"en-US", "en"},
		{"  nl-BE  ,  fr  ", "nl"},
		{"x", "de"}, // too short → fall to default
	}
	for _, tc := range cases {
		got := pickLocale(tc.accept, []string{"es", "de", "en", "nl"}, "de")
		if got != tc.want {
			t.Errorf("pickLocale(%q) = %q, want %q", tc.accept, got, tc.want)
		}
	}
}

func TestDetectLocaleAbortOnEmptySupported(t *testing.T) {
	t.Parallel()
	r := DetectLocale{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	if err := r.Run(c, nil); err == nil {
		t.Fatal("empty supported must abort")
	}
}

func TestDetectLocaleHonoursDefaultFromConfig(t *testing.T) {
	t.Parallel()
	r := DetectLocale{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	with := map[string]any{
		"supported": []any{"es", "de"},
		"default":   "es",
	}
	if err := r.Run(c, with); err != nil {
		t.Fatalf("err = %v", err)
	}
	if v, _ := c.Get("locale"); v != "es" {
		t.Fatalf("locale = %v, want es (default)", v)
	}
}

func TestDetectLocaleFallsBackToFirstSupportedWhenDefaultMissing(t *testing.T) {
	t.Parallel()
	r := DetectLocale{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	with := map[string]any{"supported": []any{"nl", "es"}}
	if err := r.Run(c, with); err != nil {
		t.Fatalf("err = %v", err)
	}
	if v, _ := c.Get("locale"); v != "nl" {
		t.Fatalf("locale = %v, want nl", v)
	}
}

func TestDetectTimeOfDayBucketing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		hour int
		want string
	}{
		{6, "morning"}, {11, "morning"},
		{12, "afternoon"}, {17, "afternoon"},
		{18, "evening"}, {21, "evening"},
		{22, "night"}, {3, "night"},
	}
	for _, tc := range cases {
		if got := bucketTimeOfDay(tc.hour); got != tc.want {
			t.Errorf("bucketTimeOfDay(%d) = %q, want %q", tc.hour, got, tc.want)
		}
	}
}

func TestDetectTimeOfDayUnknownTimezoneFallsBackToUTC(t *testing.T) {
	fixedTime(t, time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC)) // 03:00 UTC → night
	r := DetectTimeOfDay{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	with := map[string]any{"timezone": "Not/A_Real_TZ"}
	if err := r.Run(c, with); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("time_of_day"); v != "night" {
		t.Fatalf("time_of_day = %v, want night", v)
	}
}

func TestComputeBannerFlagsUnknownLocale(t *testing.T) {
	t.Parallel()
	r := ComputeBannerFlags{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	c.Set("locale", "fr") // not in greetings table
	c.Set("time_of_day", "afternoon")
	if err := r.Run(c, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get("banner")
	banner := b.(map[string]any)
	if banner["greeting"] != "Good afternoon" {
		t.Fatalf("unknown locale must fall back to en, got %v", banner["greeting"])
	}
}

func TestComputeBannerFlagsMorningToneWarm(t *testing.T) {
	t.Parallel()
	r := ComputeBannerFlags{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	c.Set("locale", "es")
	c.Set("time_of_day", "morning")
	if err := r.Run(c, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get("banner")
	banner := b.(map[string]any)
	if banner["tone"] != "warm" {
		t.Fatalf("morning tone = %v, want warm", banner["tone"])
	}
	if banner["greeting"] != "Buenos días" {
		t.Fatalf("morning greeting = %v", banner["greeting"])
	}
}

func TestComputeBannerFlagsEveningToneCool(t *testing.T) {
	t.Parallel()
	r := ComputeBannerFlags{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	c.Set("locale", "en")
	c.Set("time_of_day", "evening")
	if err := r.Run(c, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get("banner")
	if b.(map[string]any)["tone"] != "cool" {
		t.Fatalf("evening tone = %v, want cool", b.(map[string]any)["tone"])
	}
}

func TestRuleWritesDeclarations(t *testing.T) {
	t.Parallel()
	if got := (ComputeBannerFlags{}).Writes(); len(got) != 1 || got[0] != "banner" {
		t.Fatalf("ComputeBannerFlags.Writes = %v", got)
	}
	if got := (DetectLocale{}).Writes(); len(got) != 1 || got[0] != "locale" {
		t.Fatalf("DetectLocale.Writes = %v", got)
	}
	if got := (DetectTimeOfDay{}).Writes(); len(got) != 1 || got[0] != "time_of_day" {
		t.Fatalf("DetectTimeOfDay.Writes = %v", got)
	}
	for _, n := range []string{
		(DetectLocale{}).Name(),
		(DetectTimeOfDay{}).Name(),
		(ComputeBannerFlags{}).Name(),
	} {
		if n == "" {
			t.Fatal("rule must have a Name()")
		}
	}
}

func TestComputeBannerFlagsNightHidesCTA(t *testing.T) {
	t.Parallel()
	r := ComputeBannerFlags{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	c.Set("locale", "es")
	c.Set("time_of_day", "night")
	if err := r.Run(c, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get("banner")
	banner := b.(map[string]any)
	if banner["show_cta"] != false {
		t.Fatalf("show_cta at night must be false")
	}
	if banner["tone"] != "dark" {
		t.Fatalf("tone at night must be dark, got %v", banner["tone"])
	}
}

func TestComputeBannerFlagsUnknownTimeOfDayDefaultsToMorningGreeting(t *testing.T) {
	t.Parallel()
	r := ComputeBannerFlags{}
	c := newRuleCtx(httptest.NewRequest("GET", "/", nil))
	c.Set("locale", "es")
	c.Set("time_of_day", "weirdness") // not in the greetings sub-table
	if err := r.Run(c, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get("banner")
	banner := b.(map[string]any)
	if banner["greeting"] != "Buenos días" {
		t.Fatalf("unknown tod must fall back to morning greeting, got %v", banner["greeting"])
	}
}

func TestReadStringSliceShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   map[string]any
		want []string
	}{
		{"absent", map[string]any{}, nil},
		{"non-slice", map[string]any{"k": "x"}, nil},
		{"mixed types skipped", map[string]any{"k": []any{"a", 1, "b"}}, []string{"a", "b"}},
		{"all strings", map[string]any{"k": []any{"a", "b"}}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readStringSlice(tc.in, "k")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
