package natsx

// Environment variable keys read by this package. They used to live
// in the shared constants package as EnvNATSURL / EnvNATSDeliverPolicy;
// the keys belong to the package that reads them.
const (
	// EnvURL is the connection string Connect uses when Options.URL
	// is empty. Standard nats:// (or tls:// when terminating at the
	// broker) URL pointing at the cluster's NATS endpoint. Local dev:
	// "nats://localhost:4222"; inside compose: "nats://nats:4222".
	EnvURL = "HB_NATS_URL"

	// EnvDeliverPolicy overrides the default JetStream deliver policy
	// applied to durable consumers built by this package. Accepted
	// values: "new" (default — only messages published after the
	// consumer was created), "all" (replay everything still in the
	// stream — useful after a consumer reset), "last" (only the most
	// recent message per subject). Unknown → "new".
	EnvDeliverPolicy = "HB_NATS_DELIVER_POLICY"
)
