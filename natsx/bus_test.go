package natsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsio "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// startEmbedded boots an in-process nats-server with JetStream on a
// random port and returns its URL plus a teardown hook. Every test
// gets its own server so subject names don't collide across cases.
func startEmbedded(t *testing.T) (string, func()) {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		t.Fatal("embedded nats-server failed to become ready")
	}
	return srv.ClientURL(), func() { shutdownEmbedded(srv) }
}

// shutdownEmbedded forces an immediate shutdown — Drain blocks if a
// JetStream consumer is still attached, which would leak the
// goroutine into the next test.
func shutdownEmbedded(srv *natsserver.Server) {
	srv.Shutdown()
	srv.WaitForShutdown()
}

// busFixture spins up the embedded server plus a connected Bus and
// returns both with a teardown that drains + shuts down in order.
// The bus is wired with a tiny in-memory logger that asserts on
// the test's log captures when needed.
func busFixture(t *testing.T) (*Bus, *bufLogger, func()) {
	t.Helper()
	url, stop := startEmbedded(t)
	lg := &bufLogger{}
	bus, err := Connect(context.Background(), Options{URL: url, Logger: lg, Name: "test"})
	if err != nil {
		stop()
		t.Fatalf("Connect: %v", err)
	}
	return bus, lg, func() {
		_ = bus.Drain(context.Background())
		stop()
	}
}

// bufLogger is a deterministic logger that captures every call.
// Tests assert on the messages rather than on a real *slog.Logger
// (which would dump to stderr and hide the contract).
type bufLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	level string
	msg   string
	args  []any
}

func (l *bufLogger) Info(msg string, args ...any)  { l.add("info", msg, args) }
func (l *bufLogger) Warn(msg string, args ...any)  { l.add("warn", msg, args) }
func (l *bufLogger) Error(msg string, args ...any) { l.add("error", msg, args) }

func (l *bufLogger) add(level, msg string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{level, msg, args})
}

func (l *bufLogger) findLevel(level string) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, 0)
	for _, e := range l.entries {
		if e.level == level {
			out = append(out, e)
		}
	}
	return out
}

// ─── Connect / lifecycle ─────────────────────────────────────────

func TestConnect_URLFromEnv(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	t.Setenv(EnvURL, url)

	bus, err := Connect(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Drain(context.Background())
	if bus.Conn() == nil || bus.JS() == nil {
		t.Fatal("Conn / JS unexpectedly nil")
	}
}

func TestConnect_EmptyURL(t *testing.T) {
	t.Setenv(EnvURL, "")
	_, err := Connect(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "URL is empty") {
		t.Fatalf("expected empty-URL error, got %v", err)
	}
}

func TestConnect_InvalidURL(t *testing.T) {
	_, err := Connect(context.Background(), Options{URL: "nats://127.0.0.1:1"})
	if err == nil {
		t.Fatal("expected connect failure on unreachable URL")
	}
}

func TestConnect_BareHostNormalised(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	bare := strings.TrimPrefix(url, "nats://")

	bus, err := Connect(context.Background(), Options{URL: bare})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Drain(context.Background())
}

func TestConnect_CtxAlreadyCancelled(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Connect(ctx, Options{URL: url})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestConnect_JetStreamErr(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	orig := jetstreamNewFn
	jetstreamNewFn = func(_ *natsio.Conn) (jetstream.JetStream, error) {
		return nil, errors.New("synthetic jetstream failure")
	}
	defer func() { jetstreamNewFn = orig }()

	_, err := Connect(context.Background(), Options{URL: url})
	if err == nil || !strings.Contains(err.Error(), "jetstream") {
		t.Fatalf("expected jetstream error, got %v", err)
	}
}

func TestConnect_DeliverPolicyFromEnv(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	t.Setenv(EnvDeliverPolicy, "all")
	bus, err := Connect(context.Background(), Options{URL: url})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Drain(context.Background())
	if bus.policy != jetstream.DeliverAllPolicy {
		t.Fatalf("expected DeliverAllPolicy, got %v", bus.policy)
	}
}

func TestConnect_AllDefaultsApplied(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	// MaxReconnects must be set explicitly (non-zero) to test that
	// branch independently of the "0 → -1" default.
	bus, err := Connect(context.Background(), Options{URL: url, MaxReconnects: 3})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Drain(context.Background())
}

func TestRegister_NoOp(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestShutdown_CallsDrain(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Second Shutdown is idempotent — Drain caches via sync.Once.
	if err := bus.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown repeat: %v", err)
	}
}

func TestDrain_TimeoutOnStuckHandler(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("stuck", "stuck.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	gotMsg := make(chan struct{}, 1)
	release := make(chan struct{})
	err := bus.SubscribeDurable(subCtx, SubscribeOptions{
		Stream:        "stuck",
		Consumer:      "stuck-consumer",
		FilterSubject: "stuck.go",
	}, func(_ context.Context, _ []byte) error {
		select {
		case gotMsg <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}

	if err := bus.Publish(context.Background(), "stuck.go", "x"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-gotMsg:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed message")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	drainErr := bus.Drain(ctx)
	if drainErr == nil {
		t.Fatal("expected Drain timeout error")
	}
	close(release)
	cancelSub()
}

func TestDrain_DefaultTimeout(t *testing.T) {
	// Exercise the "no deadline on shutdownCtx" branch by passing a
	// plain context.Background — Drain installs its own
	// defaultDrainTimeout. The fixture has no in-flight handlers,
	// so Drain returns instantly with err == nil.
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// ─── Publish / Request ───────────────────────────────────────────

func TestPublish_RoundTrip(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("pub", "pub.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	sub, err := bus.Conn().SubscribeSync("pub.hi")
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	defer sub.Unsubscribe()

	if err := bus.Publish(context.Background(), "pub.hi", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("NextMsg: %v", err)
	}
	if string(msg.Data) != `{"k":"v"}` {
		t.Fatalf("payload = %q", msg.Data)
	}
}

func TestPublish_MarshalError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.Publish(context.Background(), "pub.hi", make(chan int))
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestPublish_NoMatchingStream(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.Publish(context.Background(), "no.such.stream.subject", "x")
	if err == nil {
		t.Fatal("expected publish error for subject with no stream")
	}
}

func TestPublishMsgID_RoundTrip(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("dedup", "dedup.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	if err := bus.PublishMsgID(context.Background(), "dedup.x", "msg-1", "first"); err != nil {
		t.Fatalf("PublishMsgID: %v", err)
	}
	// Repeat → still no error; broker deduplicates silently.
	if err := bus.PublishMsgID(context.Background(), "dedup.x", "msg-1", "second"); err != nil {
		t.Fatalf("PublishMsgID dedup: %v", err)
	}
}

func TestPublishMsgID_MarshalError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.PublishMsgID(context.Background(), "dedup.x", "m", make(chan int))
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestPublishMsgID_PublishError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	// No stream covers this subject → JetStream rejects.
	err := bus.PublishMsgID(context.Background(), "absent.x", "m", "v")
	if err == nil {
		t.Fatal("expected publish error for subject with no stream")
	}
}

func TestRequest_RoundTrip(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	_, err := bus.Conn().Subscribe("rpc.echo", func(msg *natsio.Msg) {
		_ = msg.Respond([]byte(`{"got":` + string(msg.Data) + `}`))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var resp struct {
		Got int `json:"got"`
	}
	if err := bus.Request(context.Background(), "rpc.echo", 7, &resp); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Got != 7 {
		t.Fatalf("Got = %d", resp.Got)
	}
}

func TestRequest_NilResult(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	_, err := bus.Conn().Subscribe("rpc.fire", func(msg *natsio.Msg) {
		_ = msg.Respond([]byte("anything"))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Request(context.Background(), "rpc.fire", "p", nil); err != nil {
		t.Fatalf("Request: %v", err)
	}
}

func TestRequest_MarshalError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.Request(context.Background(), "x", make(chan int), nil)
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestRequest_NoResponder(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := bus.Request(ctx, "rpc.absent", "p", nil)
	if err == nil {
		t.Fatal("expected no-responder error")
	}
}

func TestRequest_UnmarshalError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	_, err := bus.Conn().Subscribe("rpc.bad", func(msg *natsio.Msg) {
		_ = msg.Respond([]byte("not json"))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var out struct{ X int }
	err = bus.Request(context.Background(), "rpc.bad", "p", &out)
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("expected unmarshal error, got %v", err)
	}
}

// ─── Streams ─────────────────────────────────────────────────────

func TestEnsureStream_OK(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("s1", "s1.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
}

func TestEnsureStream_Err(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	// Stream names cannot contain dots — JetStream rejects.
	err := bus.EnsureStream(context.Background(), jetstream.StreamConfig{
		Name: "in.va.lid", Subjects: []string{"x"},
	})
	if err == nil {
		t.Fatal("expected error for stream name with dots")
	}
}

func TestEnsureStreams_Multi(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	cfgA := NewStreamConfig("ma", "ma.>")
	cfgB := NewStreamConfig("mb", "mb.>")
	if err := bus.EnsureStreams(context.Background(), cfgA, cfgB); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
}

func TestEnsureStreams_StopsOnError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	good := NewStreamConfig("good", "good.>")
	bad := jetstream.StreamConfig{Name: "ba.d", Subjects: []string{"x"}}
	err := bus.EnsureStreams(context.Background(), good, bad)
	if err == nil {
		t.Fatal("expected EnsureStreams to surface the bad cfg")
	}
}

func TestNewStreamConfig_Defaults(t *testing.T) {
	cfg := NewStreamConfig("name", "a", "b")
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Fatalf("Retention = %v", cfg.Retention)
	}
	if cfg.Storage != jetstream.FileStorage {
		t.Fatalf("Storage = %v", cfg.Storage)
	}
	if cfg.MaxAge != defaultStreamMaxAge {
		t.Fatalf("MaxAge = %v", cfg.MaxAge)
	}
	if cfg.Duplicates != defaultStreamDuplicates {
		t.Fatalf("Duplicates = %v", cfg.Duplicates)
	}
	if len(cfg.Subjects) != 2 || cfg.Subjects[0] != "a" {
		t.Fatalf("Subjects = %v", cfg.Subjects)
	}
}

// ─── Subscribe ───────────────────────────────────────────────────

func TestSubscribeDurable_RoundTrip(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("sub", "sub.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	got := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := bus.SubscribeDurable(ctx, SubscribeOptions{
		Stream:        "sub",
		Consumer:      "sub-ok",
		FilterSubject: "sub.go",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       2 * time.Second,
	}, func(_ context.Context, data []byte) error {
		got <- string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}
	if err := bus.Publish(context.Background(), "sub.go", "hello"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case s := <-got:
		if s != `"hello"` {
			t.Fatalf("data = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
}

func TestSubscribeDurable_HandlerErrNaks(t *testing.T) {
	bus, lg, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("nak", "nak.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	var attempts atomic.Int32
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := bus.SubscribeDurable(ctx, SubscribeOptions{
		Stream:        "nak",
		Consumer:      "nak-consumer",
		FilterSubject: "nak.go",
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, func(_ context.Context, _ []byte) error {
		n := attempts.Add(1)
		if n >= 2 {
			select {
			case <-done:
			default:
				close(done)
			}
			return nil
		}
		return errors.New("synthetic handler failure")
	})
	if err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}
	if err := bus.Publish(context.Background(), "nak.go", "x"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never recovered, attempts=%d", attempts.Load())
	}
	if got := errCount(lg); got == 0 {
		t.Fatal("expected an error log line for the failed attempt")
	}
}

func TestSubscribeDurable_CreateError(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.SubscribeDurable(context.Background(), SubscribeOptions{
		Stream:        "missing-stream",
		Consumer:      "x",
		FilterSubject: "x",
	}, func(context.Context, []byte) error { return nil })
	if err == nil {
		t.Fatal("expected error when stream is missing")
	}
}

func TestSubscribeDurable_NextErrorLogged(t *testing.T) {
	// Force the dispatch loop's "msgs.Next() error while ctx is
	// alive" branch by deleting the consumer mid-consume. The
	// iterator surfaces the deletion as an error; the dispatch
	// loop logs it and loops back to Next(), where ctx.Err() is
	// still nil so the goroutine keeps running until we cancel.
	bus, lg, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("kill", "kill.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bus.SubscribeDurable(ctx, SubscribeOptions{
		Stream: "kill", Consumer: "kc", FilterSubject: "kill.go",
	}, func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}
	// Settle so Messages() is iterating before we yank the consumer.
	time.Sleep(100 * time.Millisecond)
	if err := bus.js.DeleteConsumer(context.Background(), "kill", "kc"); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errCount(lg) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if errCount(lg) == 0 {
		t.Fatal("expected an error log from msgs.Next after consumer delete")
	}
}

// ─── Subscribe with backoff ──────────────────────────────────────

func TestSubscribeDurableWithBackoff_EmptyBackoff(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	err := bus.SubscribeDurableWithBackoff(context.Background(), SubscribeOptions{
		Stream: "s", Consumer: "c", FilterSubject: "s.x",
	}, nil, func(context.Context, []byte, RetryAttempt) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "backoff is empty") {
		t.Fatalf("expected empty-backoff error, got %v", err)
	}
}

func TestSubscribeDurableWithBackoff_RetryThenAck(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("retry", "retry.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	type record struct {
		number uint64
		last   bool
	}
	got := make(chan record, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := bus.SubscribeDurableWithBackoff(ctx, SubscribeOptions{
		Stream:        "retry",
		Consumer:      "retry-c",
		FilterSubject: "retry.go",
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
		func(_ context.Context, _ []byte, a RetryAttempt) error {
			got <- record{a.Number, a.IsLast}
			if a.Number < 3 {
				return errors.New("force retry")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Publish(context.Background(), "retry.go", "x"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	timeout := time.After(5 * time.Second)
	var attempts []record
	for len(attempts) < 3 {
		select {
		case r := <-got:
			attempts = append(attempts, r)
		case <-timeout:
			t.Fatalf("only saw %d attempts: %+v", len(attempts), attempts)
		}
	}
	if attempts[2].number != 3 || !attempts[2].last {
		t.Fatalf("final attempt unexpected: %+v", attempts[2])
	}
}

func TestSubscribeDurableWithBackoff_AckWaitBumped(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	cfg := bus.consumerConfig(SubscribeOptions{
		Stream: "s", Consumer: "c", FilterSubject: "s.x", AckWait: time.Second,
	}, 3, []time.Duration{500 * time.Millisecond, 90 * time.Second})
	if cfg.AckWait <= 90*time.Second {
		t.Fatalf("expected AckWait > 90s, got %v", cfg.AckWait)
	}
}

// ─── Helpers / branches ──────────────────────────────────────────

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, out string }{
		{"nats://localhost:4222", "nats://localhost:4222"},
		{"tls://localhost:4222", "tls://localhost:4222"},
		{"localhost:4222", "nats://localhost:4222"},
	}
	for _, c := range cases {
		if got := normalizeURL(c.in); got != c.out {
			t.Errorf("normalizeURL(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestDeliverPolicyFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want jetstream.DeliverPolicy
	}{
		{"all", jetstream.DeliverAllPolicy},
		{"last", jetstream.DeliverLastPolicy},
		{"new", jetstream.DeliverNewPolicy},
		{"", jetstream.DeliverNewPolicy},
		{"bogus", jetstream.DeliverNewPolicy},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("val=%q", c.val), func(t *testing.T) {
			t.Setenv(EnvDeliverPolicy, c.val)
			if got := deliverPolicyFromEnv(); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

// errCount returns the number of error-level log entries captured.
func errCount(lg *bufLogger) int { return len(lg.findLevel("error")) }

// ─── Connection lifecycle handlers ───────────────────────────────

func TestOnDisconnect_WithErrLogsWarn(t *testing.T) {
	lg := &bufLogger{}
	onDisconnect(lg, "nats://x")(nil, errors.New("boom"))
	if got := len(lg.findLevel("warn")); got != 1 {
		t.Fatalf("warn count = %d", got)
	}
}

func TestOnDisconnect_NilErrSilent(t *testing.T) {
	lg := &bufLogger{}
	onDisconnect(lg, "nats://x")(nil, nil)
	if got := len(lg.findLevel("warn")); got != 0 {
		t.Fatalf("warn count = %d (expected silent)", got)
	}
}

func TestOnReconnect_LogsInfo(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()
	nc, err := natsio.Connect(url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()
	lg := &bufLogger{}
	onReconnect(lg)(nc)
	infos := lg.findLevel("info")
	if len(infos) != 1 {
		t.Fatalf("info count = %d", len(infos))
	}
}

func TestOnClosed_LogsInfo(t *testing.T) {
	lg := &bufLogger{}
	onClosed(lg, "nats://x")(nil)
	if got := len(lg.findLevel("info")); got != 1 {
		t.Fatalf("info count = %d", got)
	}
}

// ─── messagesFn seam ─────────────────────────────────────────────

func TestSubscribeDurable_MessagesErr(t *testing.T) {
	bus, _, teardown := busFixture(t)
	defer teardown()
	if err := bus.EnsureStream(context.Background(), NewStreamConfig("mfn", "mfn.>")); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	orig := messagesFn
	messagesFn = func(_ jetstream.Consumer) (jetstream.MessagesContext, error) {
		return nil, errors.New("synthetic messages failure")
	}
	defer func() { messagesFn = orig }()

	err := bus.SubscribeDurable(context.Background(), SubscribeOptions{
		Stream: "mfn", Consumer: "mfn-c", FilterSubject: "mfn.x",
	}, func(context.Context, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("expected messages err, got %v", err)
	}
}
