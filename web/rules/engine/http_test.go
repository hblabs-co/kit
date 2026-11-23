package rulesengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

func newServingEngine(t *testing.T, rules []Rule, pipelineYAML string) *Engine {
	t.Helper()
	e := NewEngine().Register(rules...).Load(loadFS(pipelineYAML), "p.yaml")
	if err := e.Build(); err != nil {
		t.Fatalf("Build err = %v", err)
	}
	return e
}

func bodyJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func TestHTTPHandlerSuccess200(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "ok", writes: []string{"hello"}, run: func(c *Context, _ StepConfig) error {
		c.Set("hello", "world")
		return nil
	}}
	e := newServingEngine(t, []Rule{r}, `
pipelines:
  p:
    flow: [{name: ok}]
`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/p", nil)
	e.HTTPHandler("p")(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get(constants.HeaderContentType); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if body := bodyJSON(t, rec.Body); body["hello"] != "world" {
		t.Fatalf("body = %v", body)
	}
}

func TestHTTPHandlerAbort403(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "deny", writes: []string{"plan"}, run: func(c *Context, _ StepConfig) error {
		c.Set("plan", "free")
		return Abort("plan-too-low")
	}}
	e := newServingEngine(t, []Rule{r}, `
pipelines:
  p:
    pick: [plan]
    flow: [{name: deny}]
`)
	rec := httptest.NewRecorder()
	e.HTTPHandler("p")(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	body := bodyJSON(t, rec.Body)
	if body["reason"] != "plan-too-low" {
		t.Fatalf("reason = %v", body["reason"])
	}
	if body["plan"] != "free" {
		t.Fatalf("pick-filtered keys must echo in abort body, got %v", body)
	}
}

func TestHTTPHandlerAbortDoesNotOverwriteReason(t *testing.T) {
	t.Parallel()
	// Rule sets a key called "reason" — the handler's abort body
	// must not let the rule overwrite the abort's reason.
	r := &fakeRule{name: "deny", writes: []string{"reason"}, run: func(c *Context, _ StepConfig) error {
		c.Set("reason", "from-rule")
		return Abort("from-abort")
	}}
	e := newServingEngine(t, []Rule{r}, `
pipelines:
  p:
    flow: [{name: deny}]
`)
	rec := httptest.NewRecorder()
	e.HTTPHandler("p")(rec, httptest.NewRequest("GET", "/", nil))
	body := bodyJSON(t, rec.Body)
	if body["reason"] != "from-abort" {
		t.Fatalf("reason must be the abort's, got %v", body["reason"])
	}
}

func TestHTTPHandlerRuleError500(t *testing.T) {
	t.Parallel()
	r := &fakeRule{name: "boom", run: func(c *Context, _ StepConfig) error { return errors.New("db down") }}
	e := newServingEngine(t, []Rule{r}, `
pipelines:
  p:
    flow: [{name: boom}]
`)
	rec := httptest.NewRecorder()
	e.HTTPHandler("p")(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := bodyJSON(t, rec.Body)
	if body["error"] != "rule boom failed" {
		t.Fatalf("error message = %v", body["error"])
	}
}

func TestHTTPHandlerUnknownPipeline500(t *testing.T) {
	t.Parallel()
	e := newServingEngine(t, nil, `pipelines: {}`)
	rec := httptest.NewRecorder()
	e.HTTPHandler("nope")(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := bodyJSON(t, rec.Body)
	if body["error"] != "pipeline failed" {
		t.Fatalf("generic error expected, got %v", body["error"])
	}
}

func TestEngineErrorMessageGenericForNonRuleError(t *testing.T) {
	t.Parallel()
	if got := engineErrorMessage(errors.New("anything")); got != "pipeline failed" {
		t.Fatalf("generic = %q", got)
	}
}

func TestWriteBodyEncodeError(t *testing.T) {
	orig := writeJSONFn
	defer func() { writeJSONFn = orig }()
	called := false
	writeJSONFn = func(w http.ResponseWriter, body any) error {
		called = true
		return errors.New("forced encode failure")
	}
	// Swap default logger to a discard one so the t.Log doesn't pollute output.
	defer log.SetDefault(log.Default())
	log.SetDefault(log.Quiet())

	rec := httptest.NewRecorder()
	writeBody(rec, http.StatusOK, map[string]string{"k": "v"})
	if !called {
		t.Fatal("writeJSONFn must be invoked")
	}
}

func TestHTTPHandlerLogsOnFailure(t *testing.T) {
	t.Parallel()
	buf, lg := log.Buffered()
	r := &fakeRule{name: "fail", run: func(c *Context, _ StepConfig) error { return errors.New("boom") }}
	e := newServingEngine(t, []Rule{r}, `
pipelines:
  p:
    flow: [{name: fail}]
`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil).
		WithContext(log.WithLogger(context.Background(), lg))
	e.HTTPHandler("p")(rec, req)
	if buf.Len() == 0 {
		t.Fatal("expected log output for failed pipeline")
	}
}
