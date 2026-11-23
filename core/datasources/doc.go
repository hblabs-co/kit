// Package datasources is the umbrella over the persistence backends
// HB Labs services use. The platform is single-store: MongoDB. The
// reasoning, briefly:
//
//   - One operational footprint. Running Postgres + Redis HA at
//     small scale doubles the on-call burden without doubling the
//     value; a Mongo replica set / sharded cluster covers structured
//     data, ephemeral state with TTL, and high-cardinality logs in
//     one cluster.
//   - TTL indexes give us "Redis-style" expiration on auth tokens,
//     sessions, magic-link nonces and rate-limit blocks without a
//     second store.
//   - Multi-document transactions on replica sets cover the atomic
//     ops we actually need (membership flips, billing state).
//   - Schema flexibility removes the migration tax for early
//     iterations of Site.Config and similar JSON-shaped fields.
//
// Trade-off accepted: no native row-level security. The "every
// query filters by organization_id" invariant lives in the wrapper
// (see kit/mongo.Collection) so it's hard to
// accidentally leak across tenants.
//
// Layout:
//
//   - datasource.go          — Datasource interface (Get/Set/Delete/
//     Exists/Ping/Close) for code that only needs key/value with TTL.
//   - memory.go              — in-process Datasource impl for tests.
//   - kit/mongo — typed Mongo client, implements
//     Datasource on a kv collection, exposes the raw *mongo.Client
//     and per-collection helpers for everything else.
//
// The Datasource interface stays even with one backend so unit
// tests can swap a MemoryDatasource without spinning up Mongo.
// Production code typically reaches for the typed mongo client
// directly — Datasource is the narrow door, mongo is the wide one.
package datasources
