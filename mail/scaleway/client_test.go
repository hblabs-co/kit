package scaleway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer spins up an httptest.Server with the given handler
// and returns its URL. Caller defers shutdown via t.Cleanup.
func newTestServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newClient returns a Client wired against url with valid creds.
func newClient(url string) *Client {
	return New(Options{
		SecretKey: "scw-secret",
		ProjectID: "proj-uuid",
		Region:    "fr-par",
		BaseURL:   url,
	})
}

// sampleEmail is the canonical "magic-link mail" payload used by
// most tests.
func sampleEmail() Email {
	return Email{
		From:    Address{Email: "no-reply@example.test", Name: "Example"},
		To:      []Address{{Email: "someone@example.test"}},
		Subject: "Test",
		HTML:    "<p>Hello</p>",
		Text:    "Hello",
	}
}

// ─── New ─────────────────────────────────────────────────────────

func TestNew_AppliesDefaults(t *testing.T) {
	c := New(Options{SecretKey: "k", ProjectID: "p"})
	if c.Region != defaultRegion {
		t.Errorf("Region = %q, want %q", c.Region, defaultRegion)
	}
	if c.BaseURL != APIBase {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, APIBase)
	}
	if c.HTTP == nil || c.HTTP.Timeout != defaultHTTPTimeout {
		t.Errorf("HTTP timeout = %v, want %v", c.HTTP.Timeout, defaultHTTPTimeout)
	}
}

func TestNew_HonoursOverrides(t *testing.T) {
	hc := &http.Client{Timeout: 3 * time.Second}
	c := New(Options{SecretKey: "k", ProjectID: "p", Region: "nl-ams", BaseURL: "http://x", HTTPClient: hc})
	if c.Region != "nl-ams" || c.BaseURL != "http://x" || c.HTTP != hc {
		t.Fatalf("unexpected overrides: %+v", c)
	}
}

// ─── Send: pre-flight validation ─────────────────────────────────

func TestSend_MissingSecretKey(t *testing.T) {
	c := New(Options{ProjectID: "p"})
	_, err := c.Send(context.Background(), sampleEmail())
	if !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("expected missing-secret error, got %v", err)
	}
}

func TestSend_MissingProjectID(t *testing.T) {
	c := New(Options{SecretKey: "k"})
	_, err := c.Send(context.Background(), sampleEmail())
	if !errors.Is(err, ErrNoProjectID) {
		t.Fatalf("expected missing-project error, got %v", err)
	}
}

// ─── Send: happy path ────────────────────────────────────────────

func TestSend_RoundTrip(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fr-par/emails" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Auth-Token"); got != "scw-secret" {
			t.Errorf("X-Auth-Token = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var payload Email
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload.ProjectID != "proj-uuid" {
			t.Errorf("ProjectID injected = %q", payload.ProjectID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"emails":[{"id":"em-1","message_id":"msg-1","status":"sending"}]}`))
	})
	c := newClient(url)
	resp, err := c.Send(context.Background(), sampleEmail())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Emails) != 1 || resp.Emails[0].ID != "em-1" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestSend_TrimsBaseURLTrailingSlash(t *testing.T) {
	// Server records its own URL — we add the trailing slash and
	// expect the client to compose the path correctly without a
	// double slash.
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"emails":[]}`))
	}))
	defer srv.Close()
	c := New(Options{SecretKey: "k", ProjectID: "p", BaseURL: srv.URL + "/", Region: "fr-par"})
	if _, err := c.Send(context.Background(), sampleEmail()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if seenPath != "/fr-par/emails" {
		t.Fatalf("path = %q (expected single slash)", seenPath)
	}
}

// ─── Send: API errors ────────────────────────────────────────────

func TestSend_4xxTypedAPIError(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"invalid_argument","message":"From not verified"}`))
	})
	c := newClient(url)
	_, err := c.Send(context.Background(), sampleEmail())
	apiErr := AsAPIError(err)
	if apiErr == nil {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Type != "invalid_argument" {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
	if apiErr.Retryable() {
		t.Error("4xx must not be Retryable")
	}
	if !strings.Contains(apiErr.Error(), "invalid_argument") {
		t.Errorf("Error() lacks type: %s", apiErr.Error())
	}
}

func TestSend_4xxNonJSONBody(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden — bad token"))
	})
	c := newClient(url)
	_, err := c.Send(context.Background(), sampleEmail())
	apiErr := AsAPIError(err)
	if apiErr == nil {
		t.Fatalf("expected *APIError, got %v", err)
	}
	if apiErr.Type != "" || !strings.Contains(apiErr.Error(), "forbidden") {
		t.Errorf("unexpected APIError: %+v / msg=%q", apiErr, apiErr.Error())
	}
}

func TestSend_5xxRetryable(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"internal","message":"oops"}`))
	})
	_, err := newClient(url).Send(context.Background(), sampleEmail())
	apiErr := AsAPIError(err)
	if apiErr == nil || !apiErr.Retryable() {
		t.Fatalf("expected retryable APIError, got %+v", apiErr)
	}
}

func TestSend_429Retryable(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := newClient(url).Send(context.Background(), sampleEmail())
	apiErr := AsAPIError(err)
	if apiErr == nil || !apiErr.Retryable() {
		t.Fatalf("429 must be retryable: %+v", apiErr)
	}
}

func TestAPIError_RetryableNil(t *testing.T) {
	var apiErr *APIError
	if apiErr.Retryable() {
		t.Fatal("nil APIError must not be retryable")
	}
}

// ─── Send: transport / decode errors ─────────────────────────────

func TestSend_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // close BEFORE Send so the TCP connect fails
	c := newClient(url)
	_, err := c.Send(context.Background(), sampleEmail())
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("expected transport error, got %v", err)
	}
}

func TestSend_DecodeError(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})
	_, err := newClient(url).Send(context.Background(), sampleEmail())
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestSend_MarshalErrViaSeam(t *testing.T) {
	orig := marshalFn
	marshalFn = func(any) ([]byte, error) { return nil, errors.New("synthetic marshal failure") }
	defer func() { marshalFn = orig }()
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be reached when marshal fails")
	})
	_, err := newClient(url).Send(context.Background(), sampleEmail())
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestSend_NewRequestErrViaSeam(t *testing.T) {
	orig := newRequestFn
	newRequestFn = func(_ context.Context, _, _ string, _ io.Reader) (*http.Request, error) {
		return nil, errors.New("synthetic request failure")
	}
	defer func() { newRequestFn = orig }()
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be reached when NewRequest fails")
	})
	_, err := newClient(url).Send(context.Background(), sampleEmail())
	if err == nil || !strings.Contains(err.Error(), "request") {
		t.Fatalf("expected request error, got %v", err)
	}
}

// ─── APIError + AsAPIError ───────────────────────────────────────

func TestAPIError_ErrorNoType(t *testing.T) {
	e := &APIError{Status: 500, RawBody: "  raw bytes  "}
	if got := e.Error(); !strings.Contains(got, "500") || !strings.Contains(got, "raw bytes") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestAsAPIError_FoundInChain(t *testing.T) {
	inner := &APIError{Status: 400, Type: "x"}
	wrapped := fmt.Errorf("context: %w", inner)
	if AsAPIError(wrapped) != inner {
		t.Fatal("AsAPIError did not surface wrapped *APIError")
	}
}

func TestAsAPIError_NotPresent(t *testing.T) {
	if AsAPIError(errors.New("plain")) != nil {
		t.Fatal("AsAPIError returned non-nil for non-APIError chain")
	}
	if AsAPIError(nil) != nil {
		t.Fatal("AsAPIError(nil) should be nil")
	}
}

// ─── Additional headers / Configured ─────────────────────────────

func TestSend_AdditionalHeaders(t *testing.T) {
	var raw map[string]any
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"emails":[]}`))
	})
	c := newClient(url)

	e := sampleEmail()
	e.Headers = []Header{ReplyTo("visitor@example.test")}
	if _, err := c.Send(context.Background(), e); err != nil {
		t.Fatalf("Send: %v", err)
	}
	hs, _ := raw["additional_headers"].([]any)
	if len(hs) != 1 {
		t.Fatalf("additional_headers = %v", raw["additional_headers"])
	}
	h, _ := hs[0].(map[string]any)
	if h["key"] != "Reply-To" || h["value"] != "visitor@example.test" {
		t.Errorf("header = %v", h)
	}

	// Without headers the field is omitted entirely, so the payload
	// is byte-for-byte what it was before Headers existed.
	raw = nil
	if _, err := c.Send(context.Background(), sampleEmail()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, present := raw["additional_headers"]; present {
		t.Errorf("additional_headers must be omitted when empty: %v", raw)
	}
}

func TestConfigured(t *testing.T) {
	var nilClient *Client
	cases := []struct {
		name string
		c    *Client
		want bool
	}{
		{"nil client", nilClient, false},
		{"no credentials", New(Options{}), false},
		{"secret only", New(Options{SecretKey: "k"}), false},
		{"project only", New(Options{ProjectID: "p"}), false},
		{"both", New(Options{SecretKey: "k", ProjectID: "p"}), true},
	}
	for _, tc := range cases {
		if got := tc.c.Configured(); got != tc.want {
			t.Errorf("%s: Configured() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSend_EmptyBodyOn2xxIsSuccess(t *testing.T) {
	url := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	resp, err := newClient(url).Send(context.Background(), sampleEmail())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp == nil || len(resp.Emails) != 0 {
		t.Fatalf("response = %+v, want empty non-nil", resp)
	}
}
