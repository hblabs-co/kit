package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

type decodeTarget struct {
	Name string `json:"name"`
}

// TestDecodeJSON_HappyPath decodes a valid body and returns true.
func TestDecodeJSON_HappyPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada"}`))
	rec := httptest.NewRecorder()

	var dst decodeTarget
	if !DecodeJSON(rec, req, &dst, 1<<20) {
		t.Fatalf("DecodeJSON returned false; body = %q", rec.Body.String())
	}
	if dst.Name != "ada" {
		t.Errorf("Name = %q", dst.Name)
	}
}

// TestDecodeJSON_InvalidJSON writes the canonical 400 and returns false.
func TestDecodeJSON_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()

	var dst decodeTarget
	if DecodeJSON(rec, req, &dst, 1<<20) {
		t.Fatal("DecodeJSON returned true for invalid JSON")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestDecodeJSON_UnknownField rejects an extra key rather than dropping
// it silently.
func TestDecodeJSON_UnknownField(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ada","extra":1}`))
	rec := httptest.NewRecorder()

	var dst decodeTarget
	if DecodeJSON(rec, req, &dst, 1<<20) {
		t.Fatal("DecodeJSON returned true for unknown field")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestDecodeJSON_ExceedsMaxBytes trips MaxBytesReader and surfaces as a
// 400.
func TestDecodeJSON_ExceedsMaxBytes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"aaaaaaaa"}`))
	rec := httptest.NewRecorder()

	var dst decodeTarget
	if DecodeJSON(rec, req, &dst, 4) {
		t.Fatal("DecodeJSON returned true past the byte cap")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestWriteJSON_HappyPath confirms the canonical wire shape: the
// status is honoured, the Content-Type is the JSON literal, and
// the body decodes back to the value we wrote.
func TestWriteJSON_HappyPath(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusAccepted, map[string]string{"msg": "hello"})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(constants.HeaderContentType); got != contentTypeJSON {
		t.Errorf("Content-Type = %q", got)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["msg"] != "hello" {
		t.Errorf("msg = %q", got["msg"])
	}
}

// TestWriteJSON_MarshalFailureFallsBackTo500 covers the
// otherwise-unreachable marshal error branch by passing a channel,
// which json.Marshal refuses. The fallback is plain-text via
// http.Error, so Content-Type is NOT the JSON literal.
func TestWriteJSON_MarshalFailureFallsBackTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(constants.HeaderContentType); got == contentTypeJSON {
		t.Errorf("expected plain-text fallback, got Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestWriteError_EnvelopeShape verifies WriteError produces the
// {"error": "..."} envelope at the right status with the right
// Content-Type. This is the contract iOS / web clients decode
// against, so the round-trip through ErrorResponse is the assertion
// that matters.
func TestWriteError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "bad request")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(constants.HeaderContentType); got != contentTypeJSON {
		t.Errorf("Content-Type = %q", got)
	}
	var got ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != "bad request" {
		t.Errorf("Error = %q", got.Error)
	}
}

// TestWriteError_PreservesPresetHeader is the 405-style scenario:
// the handler sets `Allow` before calling WriteError. Because
// WriteJSON only writes Content-Type before WriteHeader flushes,
// any pre-set header rides along. Regression guard against a
// future refactor that calls Header().Reset() or otherwise loses
// the caller's metadata.
func TestWriteError_PreservesPresetHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Allow", http.MethodPost)
	WriteError(rec, http.StatusMethodNotAllowed, "method not allowed")

	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q", got)
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}
