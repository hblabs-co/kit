// Package og generates Open Graph PNG cards on demand and caches
// them via a pluggable Storage so multiple pods don't redo the work.
//
// One Generator per service produces 1200x630 PNG bytes from a
// (Title, Subtitle, Brand, Lang, Theme) Card. Output is deterministic
// — same Card → identical bytes — so a content hash of the Card
// doubles as the storage key. Two pods that race on first generation
// produce the same bytes, and Storage.PutIfAbsent keeps the write
// idempotent (`If-None-Match: *` on MinIO/S3, sync.Mutex for the
// in-memory dev store).
//
// Two-layer wiring:
//
//   - Generator → pure render. Pulls in golang.org/x/image fonts and
//     `image/png`; no I/O.
//   - Handler → cache-aside HTTP serving. A Resolver maps the request
//     to a Card; on storage hit it serves bytes; on miss it renders,
//     PutIfAbsents, then serves.
//
// In production Storage is the MinIO-backed implementation from
// packages/storageclient (TBD); URLs point at the CDN in front of
// MinIO so the og handler is bypassed entirely on warm cards. The
// in-process MemoryStorage is enough for local dev and tests.
//
// The render.Site auto-wiring (see render.Options.OG) registers the
// handler at /og/, exposes an `ogImage` template helper, and uses
// locale strings `og.<page>.title` / `og.<page>.subtitle` (with
// fallbacks to `site.title` / `site.description`) for content. A
// service that wants finer control bypasses the auto-wiring and
// constructs Generator + NewHandler directly.
package og
