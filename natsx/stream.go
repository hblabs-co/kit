package natsx

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Default stream tunings applied by NewStreamConfig when the caller
// doesn't override them. Tuned for the "auth & signal" workload
// (small, short-lived events): 24h retention is enough to replay
// a missed magic-link delivery into a freshly-rolled service-signal
// pod, and the 2h Duplicates window lines up with the upper bound
// of any retry curve we run.
const (
	defaultStreamMaxAge     = 24 * time.Hour
	defaultStreamDuplicates = 2 * time.Hour
)

// NewStreamConfig is the declarative constructor every owner-service
// uses to describe a JetStream stream. Bus.EnsureStream consumes it
// via CreateOrUpdateStream, so re-running a service after editing
// the subject list is safe — the stream config converges on the
// new shape without dropping existing messages.
//
// The defaults assume a small, recoverable workload; services with
// bigger budgets (analytics, request_log) should build their own
// jetstream.StreamConfig directly and skip this helper.
func NewStreamConfig(name string, subjects ...string) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:       name,
		Subjects:   subjects,
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		MaxAge:     defaultStreamMaxAge,
		Duplicates: defaultStreamDuplicates,
	}
}

// EnsureStreams ensures multiple streams in declaration order.
// Stops at the first error and returns it — the canonical path is
// "fail boot and let the supervisor restart" rather than try to
// recover from a partially-applied set.
func (b *Bus) EnsureStreams(ctx context.Context, cfgs ...jetstream.StreamConfig) error {
	for _, cfg := range cfgs {
		if err := b.EnsureStream(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}
