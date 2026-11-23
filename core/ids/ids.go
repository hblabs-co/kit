// Package ids generates short, URL-safe identifiers for HB Labs services
// services. Two flavours, both backed by `crypto/rand`:
//
//   - Nonce-style strings via gonanoid (URL-safe alphabet,
//     ~6 bits per char). Used for request-id, device-id, magic-link
//     tokens, API tokens — anything where the value is opaque and
//     comparable, not sortable.
//
//   - ULID — 128-bit, lexicographically sortable by creation time,
//     26 chars in Crockford base32. Used for database primary keys
//     so that "newest" sort orders match insertion order without an
//     extra timestamp column.
//
// All helpers panic on entropy-source failure. That's intentional —
// `crypto/rand` failing is OS-level wrong; serving a request with a
// non-random ID is worse than crashing.
package ids

import (
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/oklog/ulid/v2"
)

// DefaultLength is the nanoid length used by New(). 21 chars at the
// default URL-safe alphabet (64 symbols) gives ~126 bits of entropy
// — same as a UUID v4 minus the dashes, more compact, and safe in
// any URL or header value without escaping.
const DefaultLength = 21

// New returns a fresh DefaultLength-char nanoid. Suitable for
// request-id, device-id and any other ephemeral identifier.
func New() string {
	return gonanoid.Must(DefaultLength)
}

// NewSize returns a nanoid of the given length. Use 32 for tokens
// that need brute-force resistance (magic links, API tokens) — gives
// ~190 bits of entropy.
func NewSize(n int) string {
	return gonanoid.Must(n)
}

// ULID returns a fresh ULID string in Crockford base32 (26 chars).
// Use as the primary key in Postgres tables: time-sortable so the
// natural order of rows is also the chronological order, no clock
// skew issues across pods (entropy is the dominant factor between
// IDs created in the same millisecond), and string-cheap to compare.
func ULID() string {
	return ulid.Make().String()
}
