package natsx

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// messagesFn is a variable seam over Consumer.Messages so tests can
// exercise the rare "iterator construction fails" path. Production
// path is the identity wrapper.
var messagesFn = func(c jetstream.Consumer) (jetstream.MessagesContext, error) {
	return c.Messages()
}

// defaultAckWait is the per-message ack budget applied by
// SubscribeDurable when SubscribeOptions.AckWait is zero. 10 minutes
// matches what falcon shipped and is well above the worst
// handler-latency we have today (Scaleway TEM POST + Mongo write).
const defaultAckWait = 10 * time.Minute

// SubscribeOptions configures one durable JetStream consumer.
// Mandatory: Stream + Consumer + FilterSubject. Everything else has
// a default tuned for "consume one subject family from one
// stream", which is the only shape we use today.
type SubscribeOptions struct {
	// Stream is the JetStream stream name the consumer attaches to.
	// Must already exist (Bus.EnsureStream).
	Stream string

	// Consumer is the durable name. Stays stable across restarts so
	// the broker remembers pending state. Convention: dot-separated
	// "<service>.<purpose>" (e.g. "service-signal.magic_link").
	Consumer string

	// FilterSubject narrows the consumer to a single subject (or
	// wildcard subtree). Required — broad-match consumers are easy
	// to mis-route and we'd rather force authors to be explicit.
	FilterSubject string

	// AckWait overrides defaultAckWait. Bump this when a handler
	// runs longer than 10 minutes (it shouldn't, but report writers
	// occasionally do).
	AckWait time.Duration

	// DeliverPolicy overrides the Bus-wide policy for this consumer
	// only. Zero → inherit Bus.policy. Set to DeliverAllPolicy on a
	// freshly recreated consumer to replay everything still in the
	// stream.
	DeliverPolicy jetstream.DeliverPolicy
}

// RetryAttempt is the contextual handle SubscribeDurableWithBackoff
// passes to the handler. Number is 1-indexed (1 = first delivery).
// IsLast is true when the broker has exhausted MaxDeliver — the
// handler MUST handle the failure itself (DLQ insert, audit row,
// etc.) and return nil, or the message will be dropped silently.
type RetryAttempt struct {
	Number uint64
	IsLast bool
}

// SubscribeDurable creates a durable consumer and dispatches
// messages to handler in a background goroutine. Returning nil from
// handler acks the message; returning an error naks it (the broker
// will redeliver up to JetStream's default MaxDeliver).
//
// The dispatch goroutine exits when ctx is cancelled. Bus.Drain
// waits for the handler to finish whatever message is in flight
// before closing the connection.
func (b *Bus) SubscribeDurable(
	ctx context.Context,
	opts SubscribeOptions,
	handler func(ctx context.Context, data []byte) error,
) error {
	cfg := b.consumerConfig(opts, 0, nil)
	return b.consume(ctx, opts, cfg, func(msg jetstream.Msg) error {
		return handler(ctx, msg.Data())
	})
}

// SubscribeDurableWithBackoff is SubscribeDurable with an explicit
// retry curve. MaxDeliver = len(backoff) + 1; the broker waits
// backoff[attempt-1] between consecutive attempts. On the final
// attempt the handler receives RetryAttempt{IsLast: true} so it can
// persist the failure and return nil (ack) — otherwise the broker
// drops the message after MaxDeliver attempts and nothing else
// remembers it.
func (b *Bus) SubscribeDurableWithBackoff(
	ctx context.Context,
	opts SubscribeOptions,
	backoff []time.Duration,
	handler func(ctx context.Context, data []byte, attempt RetryAttempt) error,
) error {
	if len(backoff) == 0 {
		return fmt.Errorf("natsx subscribe %s: backoff is empty (use SubscribeDurable for no-retry)", opts.Consumer)
	}
	maxDeliver := len(backoff) + 1
	cfg := b.consumerConfig(opts, maxDeliver, backoff)
	return b.consume(ctx, opts, cfg, func(msg jetstream.Msg) error {
		attempt := RetryAttempt{Number: 1}
		if meta, err := msg.Metadata(); err == nil {
			attempt.Number = meta.NumDelivered
		}
		attempt.IsLast = int(attempt.Number) >= maxDeliver
		return handler(ctx, msg.Data(), attempt)
	})
}

// consumerConfig assembles the jetstream.ConsumerConfig used by both
// subscribe shapes. Pulled out so the two callsites stay aligned
// when we tune defaults (e.g. ack policy, deliver subject).
func (b *Bus) consumerConfig(opts SubscribeOptions, maxDeliver int, backoff []time.Duration) jetstream.ConsumerConfig {
	policy := opts.DeliverPolicy
	if policy == 0 {
		policy = b.policy
	}
	ack := opts.AckWait
	if ack <= 0 {
		ack = defaultAckWait
	}
	cfg := jetstream.ConsumerConfig{
		Name:          opts.Consumer,
		Durable:       opts.Consumer,
		FilterSubject: opts.FilterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: policy,
		AckWait:       ack,
	}
	if maxDeliver > 0 {
		cfg.MaxDeliver = maxDeliver
		cfg.BackOff = backoff
		// AckWait must outlive the longest backoff or the broker
		// would resend before the handler finished waiting.
		if backoff[len(backoff)-1] > cfg.AckWait {
			cfg.AckWait = backoff[len(backoff)-1] + time.Minute
		}
	}
	return cfg
}

// consume registers the consumer, starts the message iterator, and
// drives the dispatch goroutine. Centralised so both subscribe
// shapes share the cancellation + ack/nak semantics.
func (b *Bus) consume(
	ctx context.Context,
	opts SubscribeOptions,
	cfg jetstream.ConsumerConfig,
	process func(jetstream.Msg) error,
) error {
	cons, err := b.js.CreateOrUpdateConsumer(ctx, opts.Stream, cfg)
	if err != nil {
		return fmt.Errorf("natsx consumer %s/%s: %w", opts.Stream, opts.Consumer, err)
	}
	msgs, err := messagesFn(cons)
	if err != nil {
		return fmt.Errorf("natsx consumer %s/%s messages: %w", opts.Stream, opts.Consumer, err)
	}

	// Stop the iterator when the lifecycle ctx is cancelled.
	// msgs.Next() blocks otherwise, so without this hook the
	// dispatch goroutine never reaches its exit branch and Drain
	// hangs forever.
	go func() {
		<-ctx.Done()
		msgs.Stop()
	}()

	b.wg.Add(1)
	go b.dispatch(ctx, opts, msgs, process)
	return nil
}

// dispatch is the per-consumer goroutine. Pulled out of consume so
// the function name shows up in stack traces and the panic-free
// loop is easy to reason about (no closures over loop vars).
func (b *Bus) dispatch(
	ctx context.Context,
	opts SubscribeOptions,
	msgs jetstream.MessagesContext,
	process func(jetstream.Msg) error,
) {
	defer b.wg.Done()
	for {
		msg, err := msgs.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Error("natsx consumer next",
				"stream", opts.Stream, "consumer", opts.Consumer, "err", err)
			continue
		}
		if err := process(msg); err != nil {
			b.log.Error("natsx handler",
				"stream", opts.Stream, "consumer", opts.Consumer, "err", err)
			_ = msg.Nak()
			continue
		}
		_ = msg.Ack()
	}
}
