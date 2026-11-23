package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

// passThrough is the wrapped handler: it flips a flag so a test can
// assert whether the gate let the request through.
func passThrough(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
}

// TestRequireServiceToken_MatchingTokenPasses — the happy path: a
// configured token and a matching X-HB-Service-Token header let the
// request reach the wrapped handler.
func TestRequireServiceToken_MatchingTokenPasses(t *testing.T) {
	t.Setenv(constants.EnvInternalServiceToken, "s3cr3t")

	var reached bool
	h := RequireServiceToken(passThrough(&reached))

	req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
	req.Header.Set(constants.HeaderHBServiceToken, "s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached {
		t.Fatal("matching token did not reach the wrapped handler")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

// TestRequireServiceToken_WrongTokenRejected — a present but incorrect
// token must 401 and never reach the wrapped handler.
func TestRequireServiceToken_WrongTokenRejected(t *testing.T) {
	t.Setenv(constants.EnvInternalServiceToken, "s3cr3t")

	var reached bool
	h := RequireServiceToken(passThrough(&reached))

	req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
	req.Header.Set(constants.HeaderHBServiceToken, "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Fatal("wrong token reached the wrapped handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestRequireServiceToken_MissingHeaderRejected — no header at all with
// a configured token is the same as a wrong token: 401.
func TestRequireServiceToken_MissingHeaderRejected(t *testing.T) {
	t.Setenv(constants.EnvInternalServiceToken, "s3cr3t")

	var reached bool
	h := RequireServiceToken(passThrough(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/flags", nil))

	if reached {
		t.Fatal("missing header reached the wrapped handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestRequireServiceToken_FailsClosedWhenUnset — the fail-closed
// invariant: with no token configured the gate rejects EVERY request,
// even one carrying a header, so internal access stays shut until the
// secret is provisioned.
func TestRequireServiceToken_FailsClosedWhenUnset(t *testing.T) {
	t.Setenv(constants.EnvInternalServiceToken, "")

	var reached bool
	h := RequireServiceToken(passThrough(&reached))

	req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
	req.Header.Set(constants.HeaderHBServiceToken, "anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Fatal("request passed the gate while the token was unset")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
