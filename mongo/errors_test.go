package mongo

import (
	"errors"
	"testing"

	mdb "go.mongodb.org/mongo-driver/v2/mongo"
)

// TestIsDuplicateKey walks every type the helper claims to
// recognise: a bare BulkWriteException with E11000, a
// WriteException with E11000, a BulkWriteException with a
// different code, a non-Mongo error, and nil. Together these
// pin the contract: "at least one E11000 anywhere in the chain
// reports true; everything else reports false".
func TestIsDuplicateKey(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("network down"), want: false},
		{
			name: "bulk with E11000",
			err: mdb.BulkWriteException{WriteErrors: []mdb.BulkWriteError{
				{WriteError: mdb.WriteError{Code: 11000, Message: "duplicate key"}},
			}},
			want: true,
		},
		{
			name: "bulk with other code only",
			err: mdb.BulkWriteException{WriteErrors: []mdb.BulkWriteError{
				{WriteError: mdb.WriteError{Code: 121, Message: "validation failed"}},
			}},
			want: false,
		},
		{
			name: "bulk with mixed codes",
			err: mdb.BulkWriteException{WriteErrors: []mdb.BulkWriteError{
				{WriteError: mdb.WriteError{Code: 121, Message: "validation failed"}},
				{WriteError: mdb.WriteError{Code: 11000, Message: "duplicate key"}},
			}},
			want: true, // at least one E11000 → true
		},
		{
			name: "single-op WriteException E11000",
			err:  mdb.WriteException{WriteErrors: []mdb.WriteError{{Code: 11000, Message: "duplicate key"}}},
			want: true,
		},
		{
			name: "single-op WriteException other code",
			err:  mdb.WriteException{WriteErrors: []mdb.WriteError{{Code: 121, Message: "validation"}}},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDuplicateKey(c.err); got != c.want {
				t.Errorf("IsDuplicateKey = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIsOnlyDuplicateKey covers the stricter "every component is
// a dupe" contract. Used by bulk-insert callers that race with
// other replicas on a unique index: "only dupes" is the expected
// race outcome (swallow); "dupes + something else" is a real
// failure (surface).
func TestIsOnlyDuplicateKey(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("network down"), want: false},
		{name: "single-op WriteException", err: mdb.WriteException{WriteErrors: []mdb.WriteError{{Code: 11000}}}, want: false},
		{
			name: "bulk with empty WriteErrors",
			err:  mdb.BulkWriteException{},
			want: false,
		},
		{
			name: "bulk all E11000",
			err: mdb.BulkWriteException{WriteErrors: []mdb.BulkWriteError{
				{WriteError: mdb.WriteError{Code: 11000}},
				{WriteError: mdb.WriteError{Code: 11000}},
			}},
			want: true,
		},
		{
			name: "bulk mixed codes",
			err: mdb.BulkWriteException{WriteErrors: []mdb.BulkWriteError{
				{WriteError: mdb.WriteError{Code: 11000}},
				{WriteError: mdb.WriteError{Code: 121}},
			}},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsOnlyDuplicateKey(c.err); got != c.want {
				t.Errorf("IsOnlyDuplicateKey = %v, want %v", got, c.want)
			}
		})
	}
}
