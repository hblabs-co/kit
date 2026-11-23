package mongo

import (
	"context"
	"testing"

	mdb "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hblabs-co/kit/core/system"
)

// newClientForTest opens a *mdb.Client pointing at an unreachable
// loopback port. The v2 driver's Connect is deferred — it builds
// the client and starts monitoring goroutines without round-
// tripping to the server, so the call returns regardless of
// whether anything is listening. Disconnect then tears the same
// goroutines down cleanly. This gives the lifecycle test a real
// (*Mongo, error) pair without requiring a docker-compose Mongo.
func newClientForTest(t *testing.T) *mdb.Client {
	t.Helper()
	client, err := mdb.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/test"))
	if err != nil {
		t.Fatalf("driver Connect: %v", err)
	}
	return client
}

// TestModule_RegisterIsNoOpAndAssertable confirms the two
// invariants RunForever relies on: Register never errors, and the
// returned value also satisfies system.ShutdownModule so the
// orderly-teardown phase actually fires Close.
func TestModule_RegisterIsNoOpAndAssertable(t *testing.T) {
	m := &Mongo{client: newClientForTest(t), dbName: "test"}
	mod := m.Module()

	if err := mod.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := mod.(system.ShutdownModule); !ok {
		t.Fatal("Module() result must also satisfy system.ShutdownModule")
	}
	// Clean up the background goroutines the driver started.
	_ = m.Close()
}

// TestModule_ShutdownClosesClient drives the Shutdown branch:
// after one call to Shutdown, the client should be disconnected
// and any further operation against it should fail. Asserting on
// that downstream failure is the cheapest portable proof that the
// adapter actually called Close.
func TestModule_ShutdownClosesClient(t *testing.T) {
	m := &Mongo{client: newClientForTest(t), dbName: "test"}
	mod := m.Module().(system.ShutdownModule)

	if err := mod.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Second Disconnect should report the "client is disconnected"
	// state — the exact error string varies across driver releases,
	// so the assertion is just "non-nil" to stay forward-compatible.
	if err := m.client.Disconnect(context.Background()); err == nil {
		t.Fatal("expected client to be disconnected after Shutdown")
	}
}
