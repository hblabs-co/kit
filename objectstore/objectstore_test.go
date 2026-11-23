package objectstore

import "testing"

// TestNormaliseEndpoint_PassThrough covers the bare host[:port]
// branch: when no scheme is present, the function is a no-op and
// the operator's HB_MINIO_USE_SSL choice survives unchanged.
func TestNormaliseEndpoint_PassThrough(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		useSSL   bool
	}{
		{"host only", "minio.storage.svc.cluster.local", false},
		{"host port", "minio.storage.svc.cluster.local:9000", false},
		{"localhost", "localhost:9000", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{Endpoint: c.endpoint, UseSSL: c.useSSL}
			if err := normaliseEndpoint(&cfg); err != nil {
				t.Fatalf("err = %v", err)
			}
			if cfg.Endpoint != c.endpoint || cfg.UseSSL != c.useSSL {
				t.Errorf("got Endpoint=%q UseSSL=%v; want unchanged",
					cfg.Endpoint, cfg.UseSSL)
			}
		})
	}
}

// TestNormaliseEndpoint_HTTPSchemeForcesUseSSLFalse anchors the
// fix to the in-cluster outage: a `http://...:9000` endpoint must
// produce UseSSL=false regardless of what HB_MINIO_USE_SSL said,
// so the next operator who sets the scheme prefix and forgets the
// flag doesn't hit the same TLS-vs-plain mismatch.
func TestNormaliseEndpoint_HTTPSchemeForcesUseSSLFalse(t *testing.T) {
	cfg := Config{
		Endpoint: "http://minio.storage.svc.cluster.local:9000",
		UseSSL:   true, // operator typo; the scheme must win.
	}
	if err := normaliseEndpoint(&cfg); err != nil {
		t.Fatalf("err = %v", err)
	}
	if cfg.Endpoint != "minio.storage.svc.cluster.local:9000" {
		t.Errorf("Endpoint = %q, want bare host:port", cfg.Endpoint)
	}
	if cfg.UseSSL {
		t.Errorf("UseSSL = true, want false (http scheme should win)")
	}
}

// TestNormaliseEndpoint_HTTPSSchemeForcesUseSSLTrue is the
// symmetric case: a https:// endpoint with HB_MINIO_USE_SSL=false
// must end up TLS-enabled — the scheme is the authoritative
// signal.
func TestNormaliseEndpoint_HTTPSSchemeForcesUseSSLTrue(t *testing.T) {
	cfg := Config{
		Endpoint: "https://s3.hblabs.co",
		UseSSL:   false,
	}
	if err := normaliseEndpoint(&cfg); err != nil {
		t.Fatalf("err = %v", err)
	}
	if cfg.Endpoint != "s3.hblabs.co" {
		t.Errorf("Endpoint = %q, want s3.hblabs.co", cfg.Endpoint)
	}
	if !cfg.UseSSL {
		t.Errorf("UseSSL = false, want true (https scheme should win)")
	}
}

// TestNormaliseEndpoint_RejectsBadInput covers the two refusal
// paths: an unsupported scheme (so a future "tcp://" / "s3://"
// drift fails loud) and a URL with no host.
func TestNormaliseEndpoint_RejectsBadInput(t *testing.T) {
	cases := []string{
		"ftp://example.com",
		"http://",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			cfg := Config{Endpoint: in}
			if err := normaliseEndpoint(&cfg); err == nil {
				t.Errorf("normaliseEndpoint(%q) = nil error", in)
			}
		})
	}
}
