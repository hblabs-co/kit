package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, " DEBUG ": slog.LevelDebug,
		"info": slog.LevelInfo, "": slog.LevelInfo, "nonsense": slog.LevelInfo,
		"warn": slog.LevelWarn, "Warning": slog.LevelWarn,
		"error": slog.LevelError, "ERR": slog.LevelError,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewTo_FormatFollowsEnvironment(t *testing.T) {
	cases := []struct {
		name, env, format string
		wantJSON          bool
	}{
		{"local defaults to text", "local", "", false},
		{"unset env means local", "", "", false},
		{"non-local defaults to json", "production", "", true},
		{"format override wins in prod", "production", "text", false},
		{"format override wins locally", "local", "JSON", true},
		{"unknown format falls back to text", "production", "xml", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvHBEnv, tc.env)
			t.Setenv(constants.EnvLogFormat, tc.format)
			t.Setenv(constants.EnvLogLevel, "")
			var buf bytes.Buffer
			NewTo(&buf, "svc").Info("hello", "k", "v")

			line := strings.TrimSpace(buf.String())
			var obj map[string]any
			isJSON := json.Unmarshal([]byte(line), &obj) == nil
			if isJSON != tc.wantJSON {
				t.Fatalf("json=%v, want %v; line: %s", isJSON, tc.wantJSON, line)
			}
			if tc.wantJSON {
				if obj["service"] != "svc" || obj["msg"] != "hello" || obj["k"] != "v" || obj["level"] != "info" {
					t.Errorf("json record = %v", obj)
				}
				return
			}
			for _, want := range []string{"service=svc", "msg=hello", "k=v", "level=info"} {
				if !strings.Contains(line, want) {
					t.Errorf("text line missing %q: %s", want, line)
				}
			}
		})
	}
}

func TestNewTo_LevelFromEnvironment(t *testing.T) {
	t.Setenv(constants.EnvHBEnv, "local")
	t.Setenv(constants.EnvLogFormat, "")

	t.Setenv(constants.EnvLogLevel, "warn")
	var buf bytes.Buffer
	l := NewTo(&buf, "svc")
	l.Info("dropped")
	l.Warn("kept")
	if out := buf.String(); strings.Contains(out, "dropped") || !strings.Contains(out, "kept") {
		t.Errorf("level=warn output:\n%s", out)
	}

	t.Setenv(constants.EnvLogLevel, "debug")
	buf.Reset()
	NewTo(&buf, "svc").Debug("verbose")
	if !strings.Contains(buf.String(), "verbose") {
		t.Errorf("level=debug dropped a debug line:\n%s", buf.String())
	}
}

func TestNewHandlerTo(t *testing.T) {
	t.Run("nil options and lower-case level", func(t *testing.T) {
		var buf bytes.Buffer
		slog.New(NewHandlerTo(&buf, "text", nil)).Warn("m")
		if !strings.Contains(buf.String(), "level=warn") {
			t.Errorf("want lower-case level, got: %s", buf.String())
		}
	})
	t.Run("caller ReplaceAttr still runs", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandlerTo(&buf, "json", &slog.HandlerOptions{
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == "secret" {
					a.Value = slog.StringValue("***")
				}
				return a
			},
		})
		slog.New(h).Error("m", "secret", "hunter2")
		out := buf.String()
		if strings.Contains(out, "hunter2") || !strings.Contains(out, `"secret":"***"`) || !strings.Contains(out, `"level":"error"`) {
			t.Errorf("record = %s", out)
		}
	})
	t.Run("NewHandler and New build without panicking", func(t *testing.T) {
		if NewHandler("json", nil) == nil || New("svc") == nil {
			t.Fatal("nil handler or logger")
		}
	})
}

func TestDefaultAndPackageLevelHelpers(t *testing.T) {
	buf := CaptureAt(t, "helpers", LevelDebug)
	if Default() != slog.Default() {
		t.Error("Default() must be slog.Default()")
	}
	ctx := context.Background()
	Debug("d1")
	DebugContext(ctx, "d2")
	Info("i1", "n", 1)
	InfoContext(ctx, "i2")
	Warn("w1")
	WarnContext(ctx, "w2")
	Error("e1")
	ErrorContext(ctx, "e2")
	for _, msg := range []string{"d1", "d2", "i1", "i2", "w1", "w2", "e1", "e2", "service=helpers-test", "n=1"} {
		AssertContains(t, buf, msg)
	}
}

func TestSetDefault(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	b, l := Buffered()
	SetDefault(l)
	Info("through the new default")
	AssertContains(t, b, "through the new default")
}

func TestCapture_RestoresDefaultAndFiltersInfo(t *testing.T) {
	prev := slog.Default()
	t.Run("inner", func(t *testing.T) {
		buf := Capture(t, "cap")
		Debug("too verbose")
		AssertContains(t, buf, "") // Capture is info-level: nothing yet
		Info("visible")
		AssertContains(t, buf, "visible")
	})
	if slog.Default() != prev {
		t.Error("Capture did not restore the previous default logger")
	}
}

func TestQuietAndBuffered(t *testing.T) {
	Quiet().Error("goes nowhere") // must not panic
	buf, l := Buffered()
	l.Debug("debug is on")
	AssertContains(t, buf, "debug is on")
}

func TestContextLoggerAndOrganization(t *testing.T) {
	buf := Capture(t, "ctx")
	ctx := context.Background()

	if LoggerFromContext(ctx) != slog.Default() {
		t.Error("empty context must yield the default logger")
	}
	if OrganizationIDFromContext(ctx) != "" {
		t.Error("empty context must yield no organization id")
	}
	if WithOrganizationID(ctx, "") != ctx {
		t.Error("an empty organization id must not allocate a new context")
	}

	WithContext(ctx).Info("no org")
	if strings.Contains(buf.String(), "organization_id") {
		t.Errorf("organization_id leaked without being set:\n%s", buf.String())
	}

	own, l := Buffered()
	ctx = WithLogger(ctx, l)
	ctx = WithOrganizationID(ctx, "hb_org_1")
	if LoggerFromContext(ctx) != l {
		t.Error("WithLogger not honoured")
	}
	if got := OrganizationIDFromContext(ctx); got != "hb_org_1" {
		t.Errorf("OrganizationIDFromContext = %q", got)
	}
	WithContext(ctx).Info("with org")
	AssertContains(t, own, "organization_id=hb_org_1")
	AssertContains(t, own, "with org")
}

func TestAttrHelpers(t *testing.T) {
	buf, l := Buffered()
	l.Info("attrs",
		String("s", "v"), Int("i", 7), Duration("d", 1500*time.Millisecond),
		Any("a", []int{1, 2}), Group("g", "inner", "x"))
	for _, want := range []string{"s=v", "i=7", "d=1.5s", `a="[1 2]"`, "g.inner=x"} {
		AssertContains(t, buf, want)
	}
}
