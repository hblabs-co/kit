package observability

import "net/http"

// livenessBody is the literal payload Liveness writes. Plain text
// "ok" — kubelet only reads the status code, the body is for the
// human running `curl /healthz` during incident response.
const livenessBody = "ok"

// Liveness returns the http.Handler every service mounts at
// `/healthz`. It writes 200 OK plus the literal body "ok" and
// touches nothing else: no dependency probes, no allocations, no
// context — the only question the handler answers is "is this
// process running its event loop?". A dep-aware probe (Mongo, NATS)
// belongs on `/readyz` via Healthz(...); mixing the two means a
// transient dep blip restarts the pod instead of just unschedule-ing
// traffic.
//
// Wire it as:
//
//	mux.Handle("/healthz", observability.Liveness())
//
// Pair with:
//
//	mux.Handle("/readyz", observability.Healthz(opts, deps...))
//
// Returns http.Handler (not http.HandlerFunc) so the signature
// matches Healthz — a service can swap one for the other when its
// readiness story grows from "trivial" to "depends on X".
func Liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(livenessBody))
	})
}
