package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// slugAlphabet is base32-ish lowercase+digits with ambiguous chars
// removed (no l/o/0/1), for short public slugs like
// sign.hblabs.co/s/<slug>. Trades a little entropy for slugs a human
// can read aloud or retype without confusion.
const slugAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// slugLength is the slug size in characters. 8 chars over a 31-symbol
// alphabet is ~39 bits, ample for the per-site volume here; uniqueness
// is ultimately guarded by the unique index on the slug column, so a
// one-shot generate (no retry loop) is fine.
const slugLength = 8

// shortHashLength is the number of hex chars ShortHash returns. Matches
// the content-addressing convention used by storage-sync (first 8 hex
// of the SHA-256).
const shortHashLength = 8

// NewSlug returns an 8-char random slug drawn from slugAlphabet.
// Collisions are guarded by a unique index at the storage layer, so the
// caller does not retry.
func NewSlug() string {
	b := make([]byte, slugLength)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = slugAlphabet[int(b[i])%len(slugAlphabet)]
	}
	return string(b)
}

// ShortHash returns the first 8 hex chars of the SHA-256 of data. Used
// to content-address stored object keys (same convention as
// storage-sync's file hasher), so a byte change yields a new key.
func ShortHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:shortHashLength]
}
