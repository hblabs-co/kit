package mongo

import (
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	mdb "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// IndexSpec is a declarative wish: "this collection should have
// this index with this name". An app keeps its own manifest (a
// []IndexSpec next to its models); Reconcile compares that manifest
// against the live cluster and reports drift, ValidateManifest checks
// it statically in a unit test.
//
// Field semantics:
//
//   - Name. Stable across runs so drift compares are reliable. The
//     conventions used in the manifest:
//     "<field>_unique"            single-field unique
//     "<field>"                   single-field non-unique
//     "<a>_<b>"                   compound non-unique (order = key order)
//     "<a>_<b>_unique"            compound unique
//     "<field>_ttl"               TTL index
//     "<fields>_<condition>"      partial-unique with a condition tag
//
//   - Keys. Ordered bson.D — direction is +1 (ascending) or -1
//     (descending). For compound indexes the first key dominates
//     sort order; downstream keys are tiebreakers.
//
//   - Partial. partialFilterExpression. Mongo restricts the
//     supported operators (equality, $exists:true, $gt/$gte/$lt/$lte,
//     $type, top-level $and). $exists:false is NOT permitted, which
//     is why a model that needs "field absent" semantics carries
//     an explicit discriminator field instead.
//
//   - TTLSeconds. nil = no TTL. 0 = "the field carries an absolute
//     expiry instant" (Mongo's documented sentinel for our use of
//     time.Time `expires_at` columns).
type IndexSpec struct {
	Collection string
	Name       string
	Keys       bson.D
	Unique     bool
	Sparse     bool
	Partial    bson.M
	TTLSeconds *int32
}

// IndexModel converts the declarative spec to the driver's
// IndexModel used by coll.Indexes().CreateOne. Pure conversion, no
// I/O — safe for tests to call.
func (s IndexSpec) IndexModel() mdb.IndexModel {
	opts := options.Index().SetName(s.Name)
	if s.Unique {
		opts = opts.SetUnique(true)
	}
	if s.Sparse {
		opts = opts.SetSparse(true)
	}
	if s.Partial != nil {
		opts = opts.SetPartialFilterExpression(s.Partial)
	}
	if s.TTLSeconds != nil {
		opts = opts.SetExpireAfterSeconds(*s.TTLSeconds)
	}
	return mdb.IndexModel{
		Keys:    s.Keys,
		Options: opts,
	}
}

// TTL returns a *int32 pointing at the given second count. Lets a
// manifest read "TTLSeconds: mongo.TTL(0)" instead of "var s int32 = 0;
// TTLSeconds: &s".
func TTL(seconds int32) *int32 { return &seconds }

// ValidateManifest statically checks an index manifest for the
// mistakes Mongo would only reject at index-creation time:
//
//   - two specs with the same name in one collection (the second
//     CreateOne fails with a conflict),
//   - a spec without Collection, Name or Keys,
//   - a partialFilterExpression that uses $exists:false (not
//     supported by Mongo).
//
// It returns every problem found, joined with errors.Join, or nil.
// Meant for a unit test next to the app's manifest.
func ValidateManifest(manifest []IndexSpec) error {
	var errs []error
	seen := make(map[string]map[string]struct{})
	for i, s := range manifest {
		if s.Collection == "" || s.Name == "" {
			errs = append(errs, fmt.Errorf("mongo: manifest entry %d has empty Collection or Name", i))
			continue
		}
		if seen[s.Collection] == nil {
			seen[s.Collection] = make(map[string]struct{})
		}
		if _, dup := seen[s.Collection][s.Name]; dup {
			errs = append(errs, fmt.Errorf("mongo: duplicate index name in %s: %s", s.Collection, s.Name))
		}
		seen[s.Collection][s.Name] = struct{}{}
		if len(s.Keys) == 0 {
			errs = append(errs, fmt.Errorf("mongo: %s/%s has empty Keys", s.Collection, s.Name))
		}
		for field, v := range s.Partial {
			m, ok := v.(bson.M)
			if !ok {
				continue
			}
			if ex, has := m["$exists"]; has {
				if b, _ := ex.(bool); !b {
					errs = append(errs, fmt.Errorf("mongo: %s/%s has $exists:false on field %q (not allowed by Mongo)", s.Collection, s.Name, field))
				}
			}
		}
	}
	return errors.Join(errs...)
}
