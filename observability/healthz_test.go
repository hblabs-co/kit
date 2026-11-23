package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/log"
	"github.com/hblabs-co/kit/core/system"
)

func TestHealthzAllOK(t *testing.T) {
	ok := CheckFunc{Label: "postgres", Fn: func(context.Context) error { return nil }}
	srv := httptest.NewServer(Healthz(HealthzOptions{
		Logger: log.Quiet(),
	}, ok))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body healthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || len(body.Checks) != 1 || body.Checks[0].Status != "ok" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestHealthzOneFailFlipsTo503(t *testing.T) {
	deps := []Check{
		CheckFunc{Label: "postgres", Fn: func(context.Context) error { return nil }},
		CheckFunc{Label: "redis", Fn: func(context.Context) error { return errors.New("dial: refused") }},
	}
	srv := httptest.NewServer(Healthz(HealthzOptions{
		Logger: log.Quiet(),
	}, deps...))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body healthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", body.Status)
	}
	var redis *checkResult
	for i := range body.Checks {
		if body.Checks[i].Name == "redis" {
			redis = &body.Checks[i]
		}
	}
	if redis == nil || redis.Status != "fail" || redis.Error == "" {
		t.Fatalf("expected redis fail with error, got %+v", redis)
	}
}

func TestHealthzPerCheckTimeout(t *testing.T) {
	slow := CheckFunc{Label: "slow", Fn: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	}}
	srv := httptest.NewServer(Healthz(HealthzOptions{
		Timeout: 50 * time.Millisecond,
		Logger:  log.Quiet(),
	}, slow))
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on timeout", resp.StatusCode)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Healthz waited %s — timeout not enforced", elapsed)
	}
}

func TestHealthzNoDepsReturnsOK(t *testing.T) {
	srv := httptest.NewServer(Healthz(HealthzOptions{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no deps", resp.StatusCode)
	}
}

// TestHealthzDraining: after the shutdown signal readiness answers
// 503 "draining" without running any check.
func TestHealthzDraining(t *testing.T) {
	t.Cleanup(func() { system.SetDraining(false) })
	called := false
	dep := CheckFunc{Label: "db", Fn: func(context.Context) error { called = true; return nil }}
	h := Healthz(HealthzOptions{}, dep)

	system.SetDraining(true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body healthzResponse
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusServiceUnavailable || body.Status != "draining" || called {
		t.Fatalf("draining: code=%d status=%q checkCalled=%v", rec.Code, body.Status, called)
	}

	system.SetDraining(false)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("after drain reset: code=%d checkCalled=%v", rec.Code, called)
	}
}
