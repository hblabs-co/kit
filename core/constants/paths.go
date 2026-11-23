package constants

// Well-known HTTP paths the platform's services register and
// reference in more than one place. Centralised so the spelling is
// identical across the mux registration, the access-log quiet-path
// list, k8s probe specs, and any dashboard that filters by path — a
// drift between "where the handler is mounted" and "what the probe
// hits" is a silent outage (the kubelet calls a 404 and flaps the
// pod), so the literal lives in exactly one place.
//
// The `Path` prefix mirrors the convention of the other constant
// files (Env*, Header*, Service*) so call sites read
// `constants.PathHealthz` and the kind of value is obvious from the
// identifier alone.
const (
	// PathHealthz is the liveness probe endpoint: 200 while the
	// process is up and answering HTTP, regardless of dependency
	// health. Wired with observability.Liveness(). The kubelet's
	// livenessProbe hits this — a non-200 restarts the pod, so it
	// must NOT depend on Mongo / NATS / MinIO being reachable.
	PathHealthz = "/healthz"

	// PathReadyz is the readiness probe endpoint: 200 only when
	// every declared dependency check passes (Mongo Ping, etc.).
	// Wired with observability.Healthz(). The kubelet's
	// readinessProbe hits this — a non-200 pulls the pod out of
	// the Service endpoints (stops routing traffic) without
	// restarting it, so a transient Mongo blip drains rather than
	// crash-loops.
	PathReadyz = "/readyz"
)
