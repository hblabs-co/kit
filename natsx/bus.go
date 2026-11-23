// Package natsx is the HB Labs wrapper around NATS JetStream.
// One Bus instance per process opens the connection, ensures the
// streams the service participates in, and exposes the four shapes
// every service actually uses:
//
//   - Publish: JSON-marshal a payload onto a JetStream subject.
//   - Request: a core-NATS synchronous request/reply (one-off RPC
//     between services in the cluster, e.g. an edge pod asking the
//     payment service for the current snapshot at boot).
//   - SubscribeDurable: a durable JetStream consumer that survives
//     restarts and replays unacked messages, with ack-on-nil /
//     nak-on-error semantics.
//   - SubscribeDurableWithBackoff: the same shape with an explicit
//     retry curve and a last-attempt flag the handler can read to
//     persist the failure instead of looping forever.
//
// Singletons are deliberately avoided: every callsite receives the
// *Bus as a dependency, so tests build their own against an
// embedded nats-server without touching package-level state.
//
// Bus satisfies the system.Module and system.ShutdownModule
// interfaces by duck typing — Register is a no-op (the connection
// is already up by the time Connect returns), and Shutdown invokes
// Drain to wait for in-flight handlers before closing the socket.
// Callers can therefore pass `bus` straight into
// `system.RunForever(ctx, ..., bus, ...)` without a wrapper.
//
// The convention for stream definitions lives in stream.go:
// each "owning" service (or shared helper) declares the FULL subject
// list for the streams it cares about, and Bus.EnsureStream calls
// CreateOrUpdateStream — so two services agreeing on a stream name
// but listing different subjects produces a deterministic union
// rather than silently truncating each other's surfaces.
package natsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	natsio "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hblabs-co/kit/core/envcfg"
	"github.com/hblabs-co/kit/core/log"
)

// Defaults picked when the matching Options field is zero. Tuned
// for "service inside the cluster talking to a healthy NATS in the
// same network" — adjust the option when running against a noisy
// connection or a slow link.
const (
	// defaultConnectTimeout caps the initial TCP/TLS + NATS protocol
	// handshake before Connect gives up and returns. Independent from
	// the per-publish / per-request timeouts the caller controls via
	// their own context.
	defaultConnectTimeout = 5 * time.Second

	// defaultReconnectWait is the pause between reconnect attempts
	// once the link drops. Short enough that a flapping broker
	// recovers fast, long enough that a sustained outage doesn't
	// spam the logs.
	defaultReconnectWait = 2 * time.Second

	// defaultMaxReconnects = -1 means "reconnect forever". We never
	// want the client to silently give up — a service that's lost
	// NATS should keep trying, not turn into a black hole.
	defaultMaxReconnects = -1

	// defaultDrainTimeout fences Drain when the caller passes a
	// context without a deadline. Picked under
	// system.defaultShutdownTimeout (25s) so the bus drains well
	// before the rest of the shutdown budget is consumed.
	defaultDrainTimeout = 20 * time.Second
)

// logger is the minimal logging surface natsx needs. *slog.Logger
// (and therefore *log.Logger) satisfies it directly — keeping the
// interface unexported avoids advertising a new type to callers
// who already work with the canonical logger.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Options configures Connect. URL is the only required field — every
// other knob has a sensible default tuned for inside-cluster traffic.
type Options struct {
	// URL is the connection string passed to nats.Connect. Empty →
	// Connect reads EnvURL (HB_NATS_URL) from the environment.
	// Bare host:port is accepted (we prefix nats://); use tls:// to
	// terminate TLS at the broker.
	URL string

	// Name is the human-readable client identifier the broker logs
	// and exposes via /connz. Defaults to "hblabs" if unset;
	// services should pass their own service-name constant so
	// `nats top` shows a meaningful caller per connection.
	Name string

	// ConnectTimeout caps the initial connect handshake. Zero →
	// defaultConnectTimeout.
	ConnectTimeout time.Duration

	// ReconnectWait is the pause between reconnect attempts. Zero →
	// defaultReconnectWait.
	ReconnectWait time.Duration

	// MaxReconnects bounds the reconnect retry loop. Zero → -1
	// (reconnect forever). Set explicitly only when a job has a
	// finite lifetime and an unreachable broker should hard-fail.
	MaxReconnects int

	// DeliverPolicy overrides the jetstream.DeliverPolicy applied to
	// every durable consumer built by SubscribeDurable* on this Bus.
	// Zero value (DeliverNewPolicy) is the production default —
	// consumers start at the next published message. Tests typically
	// don't set this; ops set "all" via EnvNATSDeliverPolicy when
	// replaying after a consumer reset.
	DeliverPolicy jetstream.DeliverPolicy

	// Logger receives connection-lifecycle events (reconnects,
	// closed, handler errors). nil → log.Default().
	Logger logger
}

// Bus is the process-wide handle to NATS. Methods are safe for
// concurrent use; subscribers may be created from any goroutine
// after Connect returns.
type Bus struct {
	conn   *natsio.Conn
	js     jetstream.JetStream
	log    logger
	policy jetstream.DeliverPolicy

	// wg tracks in-flight handler goroutines created by
	// SubscribeDurable / SubscribeDurableWithBackoff. Drain blocks
	// on it so we don't yank the connection out from under a
	// handler that's still writing to Mongo or hitting an external
	// API.
	wg sync.WaitGroup

	// drainOnce makes Drain idempotent — multiple shutdown paths
	// can call it without double-closing the connection.
	drainOnce sync.Once
	drainErr  error
}

// Connect opens a connection to NATS and creates the JetStream
// context. The returned Bus is ready to publish or subscribe.
// Connect performs the synchronous handshake within the ctx (or
// ConnectTimeout, whichever fires first); transient reconnects
// after success are handled in the background by the nats.go
// reconnect loop.
//
// Caller owns the lifecycle: defer bus.Drain(shutdownCtx) when
// not wiring through system.RunForever.
func Connect(ctx context.Context, opts Options) (*Bus, error) {
	if opts.URL == "" {
		opts.URL = envcfg.String(EnvURL, "")
	}
	if opts.URL == "" {
		return nil, errors.New("natsx: URL is empty (set Options.URL or HB_NATS_URL)")
	}
	if opts.Name == "" {
		opts.Name = "hblabs"
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	if opts.ReconnectWait <= 0 {
		opts.ReconnectWait = defaultReconnectWait
	}
	if opts.MaxReconnects == 0 {
		opts.MaxReconnects = defaultMaxReconnects
	}
	if opts.DeliverPolicy == 0 {
		opts.DeliverPolicy = deliverPolicyFromEnv()
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}

	url := normalizeURL(opts.URL)
	nc, err := natsConnectFn(url,
		natsio.Name(opts.Name),
		natsio.Timeout(opts.ConnectTimeout),
		natsio.ReconnectWait(opts.ReconnectWait),
		natsio.MaxReconnects(opts.MaxReconnects),
		natsio.DisconnectErrHandler(onDisconnect(opts.Logger, url)),
		natsio.ReconnectHandler(onReconnect(opts.Logger)),
		natsio.ClosedHandler(onClosed(opts.Logger, url)),
	)
	if err != nil {
		return nil, fmt.Errorf("natsx connect %s: %w", url, err)
	}
	// Respect the caller's context even though nats.Connect was
	// synchronous: if ctx was already cancelled by the time the
	// dial returned, surface that rather than handing back a Bus
	// the caller is about to abandon.
	if err := ctx.Err(); err != nil {
		nc.Close()
		return nil, err
	}

	js, err := jetstreamNewFn(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsx jetstream: %w", err)
	}

	opts.Logger.Info("nats connected", "url", nc.ConnectedUrl(), "name", opts.Name)
	return &Bus{conn: nc, js: js, log: opts.Logger, policy: opts.DeliverPolicy}, nil
}

// Conn returns the underlying NATS connection. Needed for core
// pub/sub patterns that can't run on JetStream (request/reply with
// a non-durable responder, queue groups without persistence, etc.).
func (b *Bus) Conn() *natsio.Conn { return b.conn }

// PublishCore JSON-marshals payload and publishes it on subject over
// CORE NATS — fire-and-forget, no JetStream, no stream required, no
// persistence. Use it for best-effort fan-out signals (cache
// invalidation, "value changed, refetch" nudges) where a missed message
// is tolerable because a poll or HTTP fallback fills the gap. For durable,
// replayable events use Publish (JetStream) instead.
func (b *Bus) PublishCore(subject string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("natsx publishcore marshal: %w", err)
	}
	if err := b.conn.Publish(subject, data); err != nil {
		return fmt.Errorf("natsx publishcore %s: %w", subject, err)
	}
	return nil
}

// SubscribeCore registers a CORE NATS (non-durable, no-ack, no-replay)
// handler for subject. Every connected subscriber receives each message
// (fan-out); a subscriber that was down misses messages sent meanwhile —
// the right semantics for ephemeral "latest value" signals backed by a
// poll/HTTP fallback. The subscription lives until the bus drains.
// handler runs on the NATS client's dispatcher goroutine, so keep it
// quick and hand off slow work. raw bytes are passed through so the
// caller owns the unmarshal.
func (b *Bus) SubscribeCore(subject string, handler func(data []byte)) error {
	if _, err := b.conn.Subscribe(subject, func(m *natsio.Msg) { handler(m.Data) }); err != nil {
		return fmt.Errorf("natsx subscribecore %s: %w", subject, err)
	}
	return nil
}

// JS returns the JetStream context. Escape hatch for stream
// administration that the package doesn't yet wrap (stream info,
// purge, deletion). Day-to-day publish / subscribe go through the
// methods on Bus.
func (b *Bus) JS() jetstream.JetStream { return b.js }

// Publish JSON-marshals payload and publishes it to subject. The
// subject must belong to a stream the bus has already ensured (via
// EnsureStream) — JetStream silently drops publishes that don't
// match any stream, so the caller is responsible for declaring the
// stream before the first publish.
func (b *Bus) Publish(ctx context.Context, subject string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("natsx publish marshal: %w", err)
	}
	if _, err := b.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("natsx publish %s: %w", subject, err)
	}
	return nil
}

// PublishMsgID is Publish with an explicit "Nats-Msg-Id" header.
// JetStream deduplicates publishes that share the same Msg-Id
// within the stream's Duplicates window — use it whenever a retry
// could resend the same logical event (idempotent producers,
// outbox-pattern transactions).
func (b *Bus) PublishMsgID(ctx context.Context, subject, msgID string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("natsx publish marshal: %w", err)
	}
	msg := &natsio.Msg{Subject: subject, Data: data, Header: natsio.Header{}}
	msg.Header.Set(natsio.MsgIdHdr, msgID)
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("natsx publish %s msgid %s: %w", subject, msgID, err)
	}
	return nil
}

// Request issues a core-NATS request/reply. The payload is
// JSON-marshalled; the reply is JSON-unmarshalled into result when
// non-nil. Use it for short synchronous RPCs between cluster
// services — anything async or durable belongs on a JetStream
// stream instead.
func (b *Bus) Request(ctx context.Context, subject string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("natsx request marshal: %w", err)
	}
	msg, err := b.conn.RequestWithContext(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("natsx request %s: %w", subject, err)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Data, result); err != nil {
		return fmt.Errorf("natsx request %s unmarshal: %w", subject, err)
	}
	return nil
}

// EnsureStream creates the stream when missing and updates it
// (matching the supplied config) when present. Using
// CreateOrUpdateStream — not CreateStream — means re-running the
// bootstrapper after a subject list change is safe: existing
// messages stay, the new subjects start flowing. See stream.go for
// the declarative helpers.
func (b *Bus) EnsureStream(ctx context.Context, cfg jetstream.StreamConfig) error {
	if _, err := b.js.CreateOrUpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("natsx ensure stream %s: %w", cfg.Name, err)
	}
	return nil
}

// Register satisfies system.Module so a Bus can be passed straight
// into RunForever. The connection is already up by the time
// Connect returned, so there is nothing to do here — the no-op is
// the contract.
func (b *Bus) Register(_ context.Context) error { return nil }

// Shutdown satisfies system.ShutdownModule by forwarding to Drain.
// RunForever calls modules' Shutdown in reverse order; placing the
// Bus near the start of the module list (so it shuts down last)
// keeps publishes from late-stopping modules valid.
func (b *Bus) Shutdown(shutdownCtx context.Context) error { return b.Drain(shutdownCtx) }

// Drain waits for in-flight subscriber handlers to finish, then
// closes the connection. Idempotent — subsequent calls return the
// same result without acting again. If shutdownCtx has no deadline
// Drain enforces defaultDrainTimeout so a stuck handler can't keep
// the process alive forever.
func (b *Bus) Drain(shutdownCtx context.Context) error {
	b.drainOnce.Do(func() {
		ctx := shutdownCtx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultDrainTimeout)
			defer cancel()
		}
		done := make(chan struct{})
		go func() {
			b.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			b.conn.Close()
		case <-ctx.Done():
			b.conn.Close()
			b.drainErr = fmt.Errorf("natsx drain: %w", ctx.Err())
		}
	})
	return b.drainErr
}

// normalizeURL prefixes a bare host:port with nats://. Pass-through
// for any value that already starts with a scheme — including tls://
// for brokers that terminate TLS at the listener.
func normalizeURL(u string) string {
	if strings.Contains(u, "://") {
		return u
	}
	return "nats://" + u
}

// deliverPolicyFromEnv reads EnvDeliverPolicy and
// maps it to a jetstream.DeliverPolicy. Unknown / unset → "new".
// Pulled out so tests can drive the switch through the env var
// without rebuilding the Bus.
func deliverPolicyFromEnv() jetstream.DeliverPolicy {
	switch strings.ToLower(strings.TrimSpace(envcfg.String(EnvDeliverPolicy, "new"))) {
	case "all":
		return jetstream.DeliverAllPolicy
	case "last":
		return jetstream.DeliverLastPolicy
	default:
		return jetstream.DeliverNewPolicy
	}
}

// natsConnectFn and jetstreamNewFn are variable seams the tests
// swap when they need to exercise the rare error paths (jetstream
// context creation failing after a successful connect, etc.).
// Production paths use them transparently.
var (
	natsConnectFn  = natsio.Connect
	jetstreamNewFn = func(nc *natsio.Conn) (jetstream.JetStream, error) { return jetstream.New(nc) }
)

// onDisconnect builds the DisconnectErrHandler nats.go invokes when
// the link drops. err is nil when the disconnect is graceful (caller
// initiated Close); we only log when nats.go observed a real fault.
func onDisconnect(log logger, url string) natsio.ConnErrHandler {
	return func(_ *natsio.Conn, err error) {
		if err == nil {
			return
		}
		log.Warn("nats disconnected", "url", url, "err", err)
	}
}

// onReconnect builds the ReconnectHandler. The reconnect target may
// differ from the original URL (cluster gossip), so we log the
// resolved one nats.go is actually talking to.
func onReconnect(log logger) natsio.ConnHandler {
	return func(c *natsio.Conn) {
		log.Info("nats reconnected", "url", c.ConnectedUrl())
	}
}

// onClosed builds the ClosedHandler. nats.go calls this once the
// connection is permanently torn down (MaxReconnects hit or Close
// invoked) — observability hook only, no recovery action.
func onClosed(log logger, url string) natsio.ConnHandler {
	return func(_ *natsio.Conn) {
		log.Info("nats closed", "url", url)
	}
}
