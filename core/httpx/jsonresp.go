package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/hblabs-co/kit/core/constants"
)

// contentTypeJSON is the wire value services advertise for JSON
// bodies. No charset parameter — JSON is UTF-8 by definition (RFC
// 8259 §8.1), and omitting it matches the established convention
// in this repo (service-api, service-ingest).
const contentTypeJSON = "application/json"

// ErrorResponse is the canonical JSON error envelope every
// service returns when a request fails. One field, one
// human-readable message, no nested codes or stack traces. The
// shape is part of the API contract for iOS / web clients — a
// client decoder hard-codes `{"error": string}` and doesn't need
// to branch per endpoint.
//
// Why a struct rather than a free-form map: the field name is
// guaranteed at compile time, and any logging or middleware that
// wants to enrich the payload (eg add a request_id) can extend
// the struct in one place rather than chasing literals across
// every handler.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteJSON serialises v at the given status with the canonical
// JSON Content-Type. On marshal failure the response degrades to
// a plain-text 500 via http.Error — that branch is meant to be
// unreachable in practice (every caller marshals a struct
// declared in this repo), but the fallback is here so a future
// addition doesn't silently truncate a response body.
//
// Status MUST be set through this function (not via a manual
// w.WriteHeader before the call); the order Content-Type →
// WriteHeader → Write matters for proxies and httptest recorders
// alike.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(constants.HeaderContentType, contentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// WriteError writes the canonical ErrorResponse with the given
// status and message. The convenience wrapper exists so handler
// branches read as one-liners — `httpx.WriteError(w, 400, "bad
// JSON")` instead of an inline ErrorResponse literal.
//
// Pre-set headers (eg the `Allow` header on a 405) survive the
// call because WriteJSON only writes Content-Type before flushing
// — set them before calling WriteError and they land on the wire.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Error: msg})
}

// DecodeJSON reads the request body into dst, capping it at maxBytes
// and rejecting unknown fields (so a typo'd or extra key is a 400, not a
// silently-dropped value). On any failure it writes the canonical 400
// ErrorResponse and returns false; the caller's branch reads as one
// line:
//
//	if !httpx.DecodeJSON(w, r, &req, maxBody) { return }
//
// It owns the MaxBytesReader + DisallowUnknownFields + decode dance that
// every JSON-accepting handler in this repo would otherwise repeat.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
