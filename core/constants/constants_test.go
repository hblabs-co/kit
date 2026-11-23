package constants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// declared parses the package's own sources and returns every
// top-level string constant as name → value. Parsing the source (vs.
// listing the constants by hand) means a new constant is covered by
// the guards below without anybody remembering to register it.
func declared(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							t.Fatalf("%s is not a string literal constant", n.Name)
						}
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("%s: %v", n.Name, err)
						}
						out[n.Name] = v
					}
				}
			}
		}
	}
	return out
}

func TestNamingConventions(t *testing.T) {
	consts := declared(t)
	if len(consts) == 0 {
		t.Fatal("no constants found")
	}
	for name, value := range consts {
		switch {
		case strings.HasPrefix(name, "Env"):
			if !strings.HasPrefix(value, "HB_") || value != strings.ToUpper(value) {
				t.Errorf("%s = %q: env keys are upper-case and start with HB_", name, value)
			}
		case strings.HasPrefix(name, "Header"):
			if value == "" || strings.ContainsAny(value, " :\t") {
				t.Errorf("%s = %q is not a valid header name", name, value)
			}
		case strings.HasPrefix(name, "Cookie"):
			if value == "" || strings.ContainsAny(value, " ;=,") {
				t.Errorf("%s = %q is not a valid cookie name", name, value)
			}
		case strings.HasPrefix(name, "Path"):
			if !strings.HasPrefix(value, "/") {
				t.Errorf("%s = %q: paths start with /", name, value)
			}
		default:
			t.Errorf("%s: unknown kind of constant; this package only holds Env*, Header*, Cookie* and Path* names", name)
		}
	}
}

func TestValuesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for name, value := range declared(t) {
		key := strings.ToLower(value) // header names are case-insensitive
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s share the value %q", name, other, value)
		}
		seen[key] = name
	}
}

// TestStableValues pins the values that are part of deployed
// configuration (k8s manifests, .env files, probes). Renaming one is
// a breaking change for every app, so it must be a deliberate edit of
// this test too.
func TestStableValues(t *testing.T) {
	want := map[string]string{
		"EnvHBEnv":           "HB_ENV",
		"EnvPort":            "HB_PORT",
		"EnvLogLevel":        "HB_LOG_LEVEL",
		"EnvLogFormat":       "HB_LOG_FORMAT",
		"EnvBuildVersion":    "HB_BUILD_VERSION",
		"EnvBuildTime":       "HB_BUILD_TIME",
		"EnvShutdownTimeout": "HB_SHUTDOWN_TIMEOUT",
		"EnvShutdownDelay":   "HB_SHUTDOWN_DELAY",
		"PathHealthz":        "/healthz",
		"PathReadyz":         "/readyz",
		"HeaderRequestID":    "X-Request-Id",
	}
	got := declared(t)
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
}
