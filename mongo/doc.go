// Package mongo is the MongoDB backend behind the datasources
// umbrella, and currently the only persistence backend HB Labs
// services run. It wraps go.mongodb.org/mongo-driver/v2 with the
// conventions every service shares:
//
//   - One *mongo.Client per process, opened from MONGO_URL at boot
//     and Disconnect()ed at shutdown. Replica-set and sharded URLs
//     are equally valid — pick the topology in the URL, not in code.
//   - A typed Mongo wrapper that exposes Database() and Client()
//     for callers who need raw collection access (most domain repos),
//     plus Datasource methods (Get/Set/Delete/Exists with TTL) on a
//     built-in kv collection for code that only wants opaque key/
//     value-with-TTL semantics.
//   - Tenant-scoped Collection helper that auto-injects
//     `organization_id` filters when the request carries one in
//     context. This is the substitute for Postgres RLS: the
//     boundary lives at the package boundary instead of the DB,
//     and the helper makes the safe path the easy path.
//   - Index reconciliation (Reconcile / FindSpec / IndexLister /
//     Entry / Status) that diffs an app's declarative manifest
//     ([]IndexSpec) against a live cluster. The pure-Go core has no
//     driver dependency so it is unit-testable without Mongo;
//     the caller (a db-migrate style tool) supplies the driver
//     adapter and drives create/drop on apply.
//
// TTL strategy: every collection that holds expiring documents
// declares a TTL index on `expires_at` with `expireAfterSeconds: 0`.
// Mongo's background sweep removes expired docs ±60 seconds, which
// is fine for sessions / magic links / rate-limit blocks where the
// app rejects a "live" doc whose expires_at is already past anyway
// (defence in depth at read time).
//
// Atomic ops: where Postgres would use a transaction, the Mongo
// equivalents are either (a) a single-document update with an
// expression (most CAS cases collapse here) or (b) an explicit
// session.WithTransaction on the replica set. Use the typed Client()
// for both — Datasource doesn't pretend the primitives exist.
package mongo
