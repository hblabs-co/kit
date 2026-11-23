package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLiveness_ReturnsOK is the entire contract: 200 status, body
// "ok", any HTTP method allowed (kubelet uses GET; curl debugging
// might use HEAD). One test covers the lot because the handler is
// trivial and the assertion shape lets a future regression land on
// exactly the line that changed.
func TestLiveness_ReturnsOK(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/healthz", nil)
			rec := httptest.NewRecorder()
			Liveness().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Body.String(); got != livenessBody {
				t.Errorf("body = %q, want %q", got, livenessBody)
			}
		})
	}
}
