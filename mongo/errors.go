package mongo

import (
	"errors"

	mdb "go.mongodb.org/mongo-driver/v2/mongo"
)

// duplicateKeyCode is Mongo's well-known error code for a unique-
// index violation. The driver surfaces it as the numeric Code field
// on a WriteError; matching on the number rather than on message
// wording keeps the check resilient to server-version phrasing
// changes (the "E11000 duplicate key error collection: ..." string
// has shifted between releases).
const duplicateKeyCode = 11000

// IsDuplicateKey reports whether err is a Mongo duplicate-key
// (E11000) violation — surfaced either as a single-op WriteException
// (UpdateOne / InsertOne with a clashing unique key) or as part of
// a BulkWriteException where at least one component failed for the
// same reason.
//
// The "at least one" semantics matter: a BulkWrite that mixes a
// duplicate-key conflict with a network error must NOT be silently
// swallowed. Callers that need "everything was a duplicate, nothing
// else" should use IsOnlyDuplicateKey instead.
//
// Typical use is to distinguish a benign race (two callers proposing
// the same row at the same instant) from a real failure at the
// call site where idempotent upserts are expected to win.
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var bwe mdb.BulkWriteException
	if errors.As(err, &bwe) {
		for _, we := range bwe.WriteErrors {
			if we.Code == duplicateKeyCode {
				return true
			}
		}
		return false
	}
	var we mdb.WriteException
	if errors.As(err, &we) {
		for _, w := range we.WriteErrors {
			if w.Code == duplicateKeyCode {
				return true
			}
		}
	}
	return false
}

// IsOnlyDuplicateKey reports whether err is a BulkWriteException
// in which every component is a duplicate-key (E11000) violation.
// Used at sites that bulk-insert idempotent rows under a unique
// index where a racing peer is the expected case — the bulk write
// surfaces a partial-success error and the caller wants to treat
// "only dupes" as success while still reporting any other failure.
//
// Returns false for single-op errors and for BulkWrites that mix
// dupes with any other error code. A bulk write with zero
// WriteErrors (succeeded fully but failed at a different layer)
// also returns false — there's nothing for the "swallow dupes"
// branch to cover.
func IsOnlyDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var bwe mdb.BulkWriteException
	if !errors.As(err, &bwe) {
		return false
	}
	if len(bwe.WriteErrors) == 0 {
		return false
	}
	for _, we := range bwe.WriteErrors {
		if we.Code != duplicateKeyCode {
			return false
		}
	}
	return true
}
