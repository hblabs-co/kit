package mongo

import (
	"context"

	"github.com/hblabs-co/kit/core/system"
)

// Module returns a system.Module adapter that ties this Mongo
// client's lifecycle into the service's orderly shutdown. Pass it
// directly to system.RunForever:
//
//	if err := system.RunForever(ctx, server, mongo.Module()); err != nil { ... }
//
// Register is a no-op: by the time RunForever runs the
// registration phase the client is already open (cloudmongo.New
// returned successfully, including the initial Ping). Shutdown
// calls Close, which disconnects the underlying *mongo.Client and
// reports any error back to the lifecycle so it can be logged in
// reverse-registration order alongside the rest of the modules.
//
// Why this lives on the Mongo type and not in each service's
// main.go: every service that opens Mongo needs the same wrapper,
// and a per-service mongoModule struct is pure duplication —
// service-api and service-ingest used to ship identical copies.
// Centralising here means a future change to the teardown
// contract (e.g. honouring the shutdown ctx instead of background)
// lands in one place.
func (m *Mongo) Module() system.Module {
	return mongoModule{m: m}
}

// mongoModule is the unexported system.Module + ShutdownModule
// implementation. Unexported because callers go through
// (*Mongo).Module() and never spell the type. Implements both
// halves of the system.Module / ShutdownModule pair so RunForever
// drives the Close on teardown via the optional-shutdown type
// assertion.
type mongoModule struct{ m *Mongo }

// Register is a no-op. The Mongo client is opened eagerly by
// cloudmongo.New (which Pings before returning), so by the time
// RunForever calls Register the client is already serving
// queries. Returning nil here keeps the wrapper a satisfied
// system.Module without forcing Register to know anything about
// the data layer.
func (mongoModule) Register(_ context.Context) error { return nil }

// Shutdown closes the Mongo client. The context is currently
// ignored because (*Mongo).Close uses context.Background for the
// underlying Disconnect — keeping the wrapper's signature in line
// with what the package actually does. If teardown gains a budget
// here later, Close is the place to plumb it.
func (mm mongoModule) Shutdown(_ context.Context) error { return mm.m.Close() }
