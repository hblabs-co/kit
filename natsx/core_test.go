package natsx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// recv waits for one message on ch or fails the test.
func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a core NATS message")
		return nil
	}
}

// TestCore_PublishSubscribeRoundTrip — the payload is JSON-marshalled
// on the way out and the raw bytes are handed to the handler.
func TestCore_PublishSubscribeRoundTrip(t *testing.T) {
	bus, _, done := busFixture(t)
	defer done()

	got := make(chan []byte, 4)
	if err := bus.SubscribeCore("prices.changed", func(data []byte) { got <- append([]byte(nil), data...) }); err != nil {
		t.Fatalf("SubscribeCore: %v", err)
	}
	// The subscription must be registered on the server before the
	// publish, or core NATS (no persistence) drops the message.
	if err := bus.Conn().Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	type priceChange struct {
		Tier  string `json:"tier"`
		Cents int    `json:"cents"`
	}
	if err := bus.PublishCore("prices.changed", priceChange{Tier: "gold", Cents: 4200}); err != nil {
		t.Fatalf("PublishCore: %v", err)
	}

	var pc priceChange
	if err := json.Unmarshal(recv(t, got), &pc); err != nil {
		t.Fatalf("payload is not the JSON that was published: %v", err)
	}
	if pc.Tier != "gold" || pc.Cents != 4200 {
		t.Errorf("payload = %+v", pc)
	}
}

// TestCore_FanOut — every subscriber gets every message (no queue
// group, no work-queue semantics), including a second connection.
func TestCore_FanOut(t *testing.T) {
	url, stop := startEmbedded(t)
	defer stop()

	connect := func() *Bus {
		b, err := Connect(context.Background(), Options{URL: url, Logger: &bufLogger{}, Name: "fanout"})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		t.Cleanup(func() { _ = b.Drain(context.Background()) })
		return b
	}
	a, b := connect(), connect()

	gotA, gotA2, gotB := make(chan []byte, 2), make(chan []byte, 2), make(chan []byte, 2)
	for _, sub := range []struct {
		bus *Bus
		ch  chan []byte
	}{{a, gotA}, {a, gotA2}, {b, gotB}} {
		ch := sub.ch
		if err := sub.bus.SubscribeCore("legal.changed", func(data []byte) { ch <- data }); err != nil {
			t.Fatalf("SubscribeCore: %v", err)
		}
		if err := sub.bus.Conn().Flush(); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.PublishCore("legal.changed", "v7"); err != nil {
		t.Fatalf("PublishCore: %v", err)
	}
	for name, ch := range map[string]chan []byte{"A": gotA, "A2": gotA2, "B": gotB} {
		if s := string(recv(t, ch)); s != `"v7"` {
			t.Errorf("subscriber %s got %s", name, s)
		}
	}
}

// TestCore_NoReplay — core NATS keeps nothing: a subscriber that
// joins after the publish does not see the earlier message.
func TestCore_NoReplay(t *testing.T) {
	bus, _, done := busFixture(t)
	defer done()

	if err := bus.PublishCore("biz.changed", 1); err != nil {
		t.Fatal(err)
	}
	if err := bus.Conn().Flush(); err != nil {
		t.Fatal(err)
	}

	got := make(chan []byte, 2)
	if err := bus.SubscribeCore("biz.changed", func(data []byte) { got <- data }); err != nil {
		t.Fatal(err)
	}
	if err := bus.Conn().Flush(); err != nil {
		t.Fatal(err)
	}
	if err := bus.PublishCore("biz.changed", 2); err != nil {
		t.Fatal(err)
	}
	if s := string(recv(t, got)); s != "2" {
		t.Fatalf("first delivered message = %s, want 2 (message 1 predates the subscription)", s)
	}
	select {
	case extra := <-got:
		t.Fatalf("unexpected extra message %s", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCore_Errors(t *testing.T) {
	bus, _, done := busFixture(t)
	defer done()

	// A payload JSON cannot encode fails before anything is sent.
	if err := bus.PublishCore("x", make(chan int)); err == nil || !strings.Contains(err.Error(), "publishcore marshal") {
		t.Errorf("unmarshalable payload: err = %v", err)
	}
	// An invalid subject is rejected by the client.
	if err := bus.PublishCore("", "v"); err == nil || !strings.Contains(err.Error(), "natsx publishcore") {
		t.Errorf("empty publish subject: err = %v", err)
	}
	if err := bus.SubscribeCore("", func([]byte) {}); err == nil || !strings.Contains(err.Error(), "natsx subscribecore") {
		t.Errorf("empty subscribe subject: err = %v", err)
	}
}
