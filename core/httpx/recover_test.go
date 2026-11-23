package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/log"
)

// TestRecover_CatchesPanicAndReturns500 verifies the core invariant
// of the middleware: a panic anywhere downstream becomes a clean 500
// instead of a torn goroutine.
func TestRecover_CatchesPanicAndReturns500(t *testing.T) {
	// Discard the panic log so test output stays clean. We're not
	// asserting on the log line shape here — just on HTTP behaviour.
	ctx := log.WithLogger(context.Background(), log.Quiet())

	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Internal Server Error") {
		t.Errorf("body = %q, want generic 500 message", body)
	}
}

// TestRecover_PassesThroughWhenNoPanic verifies the middleware is
// transparent on the happy path — no double-WriteHeader, no body
// rewriting, status code preserved.
func TestRecover_PassesThroughWhenNoPanic(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brewing"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Code; got != http.StatusTeapot {
		t.Errorf("status = %d, want 418", got)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "brewing" {
		t.Errorf("body = %q, want %q", body, "brewing")
	}
}

// TestRecover_RepanicsAbortHandler confirms we honour the stdlib
// contract: http.ErrAbortHandler must not be swallowed, so net/http
// itself can do its no-log abort dance.
func TestRecover_RepanicsAbortHandler(t *testing.T) {
	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Errorf("recovered = %v, want http.ErrAbortHandler", rec)
		}
	}()

	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
}
