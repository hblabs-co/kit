package log

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Quiet returns a *Logger that drops every record. Use in tests
// where the code under test logs something on the path being
// exercised (warnings, errors) and you want the test output to stay
// clean — e.g. `New(Options{Logger: log.Quiet()})`. Centralised here
// so the io.Discard / NewTextHandler boilerplate doesn't get
// re-typed in every package's test file.
func Quiet() *Logger {
	return NewTo(io.Discard, "quiet")
}

// Buffered returns a buffer + *Logger pair: every record the logger
// emits is captured in the buffer so tests can assert on log shape
// after the code under test has run. Hermetic w.r.t. LOG_LEVEL —
// always captures debug+ so the assertion isn't silently dropped
// when an env var is set in CI:
//
//	buf, lg := log.Buffered()
//	mw := RequestID(lg)
//	mw(handler).ServeHTTP(rec, req)
//	if !strings.Contains(buf.String(), "request_id=...") { ... }
//
// For tests that need the process-wide default swapped (i.e. code
// reads via slog.Default rather than an injected logger), use
// Capture instead.
func Buffered() (*bytes.Buffer, *Logger) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return &buf, slog.New(h)
}

// Capture swaps slog.Default() for a buffer-backed logger tagged
// service+"-test" and restores the previous default via t.Cleanup.
// The returned buffer accumulates every log line emitted while the
// test runs, so callers can assert on the contents after the code
// under test has executed.
//
// Lives in package log (rather than a logtest subpackage) so test
// code reads `log.Capture(t, "envcfg")` — the import of "testing"
// here is harmless: the Go linker doesn't pull testing into a binary
// that never references it.
//
// Captures at LevelInfo regardless of LOG_LEVEL — keeps tests
// hermetic; use CaptureAt to pin a different threshold.
func Capture(t *testing.T, service string) *bytes.Buffer {
	return CaptureAt(t, service, LevelInfo)
}

// CaptureAt is the explicit-level form of Capture. Useful when the
// code under test relies on a specific level threshold (e.g. an
// access log middleware that should skip its attribute build when
// the configured level is below the global default — that branch
// can only be exercised by pinning the default at LevelInfo while
// the middleware asks for LevelDebug).
func CaptureAt(t *testing.T, service string, level Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})
	lg := slog.New(h).With(slog.String("service", service+"-test"))
	prev := slog.Default()
	slog.SetDefault(lg)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// AssertContains is the standard assertion against a buffer returned
// by Capture: when match is non-empty the buffer must contain it
// (typically the warn substring the code under test should emit);
// when match is empty the buffer must be empty too — a stray log
// line during a "happy path" case usually means the code is logging
// when it shouldn't.
func AssertContains(t *testing.T, buf *bytes.Buffer, match string) {
	t.Helper()
	got := buf.String()
	if match == "" {
		if got != "" {
			t.Errorf("expected no log output, got:\n%s", got)
		}
		return
	}
	if !strings.Contains(got, match) {
		t.Errorf("log output missing %q\n--- got ---\n%s", match, got)
	}
}
