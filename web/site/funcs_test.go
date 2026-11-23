package site

import (
	"html/template"
	"io/fs"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

func TestUIKitFS(t *testing.T) {
	for _, env := range []string{"production", "local"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(constants.EnvHBEnv, env)
			// Point the dev lookup at a directory without a kit
			// checkout so the "local" case exercises DevFS's
			// fallback and both cases must still serve the tokens.
			t.Setenv(constants.EnvKitDir, t.TempDir())
			if _, err := fs.Stat(UIKitFS(), "css/tokens.css"); err != nil {
				t.Fatalf("UIKitFS() cannot stat css/tokens.css: %v", err)
			}
		})
	}
}

func TestMergeFuncMaps(t *testing.T) {
	a := template.FuncMap{"one": func() int { return 1 }, "shared": func() string { return "a" }}
	b := template.FuncMap{"two": func() int { return 2 }, "shared": func() string { return "b" }}

	got := MergeFuncMaps(a, nil, b)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if v := got["shared"].(func() string)(); v != "b" {
		t.Errorf("later map must win: shared() = %q, want b", v)
	}
	if len(a) != 2 || a["shared"].(func() string)() != "a" {
		t.Error("input map was mutated")
	}

	empty := MergeFuncMaps()
	if empty == nil || len(empty) != 0 {
		t.Errorf("MergeFuncMaps() = %v, want empty non-nil map", empty)
	}
	// The result is usable by html/template as-is.
	tmpl, err := template.New("t").Funcs(got).Parse(`{{ one }}{{ two }}{{ shared }}`)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err != nil || sb.String() != "12b" {
		t.Fatalf("render = %q, %v", sb.String(), err)
	}
}

func TestBuildInfoFuncMap(t *testing.T) {
	t.Setenv(constants.EnvBuildVersion, "v9.9.9")
	t.Setenv(constants.EnvBuildTime, "2026-05-25T18:23:00Z")
	fm := BuildInfoFuncMap()
	for _, name := range []string{"buildVersion", "buildTime", "buildTimePretty"} {
		if _, ok := fm[name].(func() string); !ok {
			t.Fatalf("%s missing or wrong type: %T", name, fm[name])
		}
	}
	if v := fm["buildVersion"].(func() string)(); v != "v9.9.9" {
		t.Errorf("buildVersion() = %q", v)
	}
	if v := fm["buildTime"].(func() string)(); v != "2026-05-25T18:23:00Z" {
		t.Errorf("buildTime() = %q", v)
	}
	if v := fm["buildTimePretty"].(func() string)(); v == "" {
		t.Error("buildTimePretty() is empty for a valid stamp")
	}
	// Read once: a later env change must not leak into the map.
	t.Setenv(constants.EnvBuildVersion, "v0.0.0")
	if v := fm["buildVersion"].(func() string)(); v != "v9.9.9" {
		t.Errorf("buildVersion() re-read the environment: %q", v)
	}
}
