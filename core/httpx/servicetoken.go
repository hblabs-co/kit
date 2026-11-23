package httpx

import (
	"crypto/subtle"
	"net/http"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/envcfg"
)

// RequireServiceToken gates the wrapped handler behind the shared internal
// service token: the X-HB-Service-Token header must match
// HB_INTERNAL_SERVICE_TOKEN (constant-time compare). An empty configured
// token rejects every request, so the gate fails closed: internal access
// stays shut until the secret is provisioned.
//
// This is the canonical service-to-service auth gate for internal (non
// public) routes. Any service wraps its internal subtree with it so the
// same shared secret works everywhere, e.g.
//
//	mux.Handle("/v1/admin/", httpx.RequireServiceToken(adminHandler))
//
// The token is one shared secret across services in v1, a placeholder
// until real service identity (mTLS/SPIFFE). It is read from the env on
// every request (via envcfg) so a rotated secret takes effect without a
// restart once the pod's secret env is refreshed.
func RequireServiceToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := envcfg.String(constants.EnvInternalServiceToken, "")
		got := r.Header.Get(constants.HeaderHBServiceToken)
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			WriteError(w, http.StatusUnauthorized, "invalid or missing service token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
