package mongo

import (
	"context"
	"sort"
)

// Reconcile compares a declarative index manifest against the live
// indexes on a MongoDB cluster and reports the diff. The caller
// supplies an IndexLister (in production, a thin adapter over the
// Mongo driver; in tests, an in-memory fake) and a manifest (the
// app's []IndexSpec). The caller decides what
// to do with the returned []Entry — print it, gate CI on non-OK
// rows, drive create/drop calls.

// mongoIDIndex is the primary-key index Mongo creates on every
// collection. It is never in the manifest and can't be dropped; the
// reconciler skips it so it doesn't show up as drift on every run.
const mongoIDIndex = "_id_"

// Status is the outcome for one reconciliation entry.
type Status string

const (
	// StatusOK — the manifest entry exists on the cluster with the
	// expected name. Spec drift (different keys, missing unique flag,
	// etc.) is intentionally NOT detected here; the reconciler trusts
	// the name as a stable identifier. Add deeper comparison only
	// once a concrete bug demands it.
	StatusOK Status = "OK"
	// StatusMissing — the manifest names this index but the cluster
	// doesn't have it. db-migrate ensure --apply creates it.
	StatusMissing Status = "MISSING"
	// StatusExtra — the cluster has this index but the manifest
	// doesn't. db-migrate ensure --apply --allow-drop removes it;
	// otherwise it's reported and left alone.
	StatusExtra Status = "EXTRA"
)

// Entry is one row in the reconciliation report.
type Entry struct {
	Collection string `json:"collection"`
	Name       string `json:"name"`
	Status     Status `json:"status"`
}

// IndexLister is the only Mongo capability Reconcile needs. The
// binary's driverLister adapts the real driver to this interface;
// tests supply an in-memory fake. Keeping the surface this small
// is what makes the reconciler trivially unit-testable.
type IndexLister interface {
	ListIndexes(ctx context.Context, collection string) ([]string, error)
}

// Reconcile walks the manifest and the live cluster, returning one
// Entry per manifest spec (OK or MISSING) plus one Entry per
// unmanaged live index (EXTRA, excluding `_id_`). Output is sorted
// by collection then by index name for stable diff-friendly text.
func Reconcile(ctx context.Context, lister IndexLister, manifest []IndexSpec) ([]Entry, error) {
	byColl := make(map[string]map[string]struct{})
	collections := make([]string, 0)
	for _, s := range manifest {
		if _, seen := byColl[s.Collection]; !seen {
			byColl[s.Collection] = make(map[string]struct{})
			collections = append(collections, s.Collection)
		}
		byColl[s.Collection][s.Name] = struct{}{}
	}
	sort.Strings(collections)

	out := make([]Entry, 0, len(manifest))

	for _, coll := range collections {
		live, err := lister.ListIndexes(ctx, coll)
		if err != nil {
			return nil, err
		}
		liveSet := make(map[string]struct{}, len(live))
		for _, n := range live {
			liveSet[n] = struct{}{}
		}

		names := make([]string, 0, len(byColl[coll]))
		for n := range byColl[coll] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if _, ok := liveSet[n]; ok {
				out = append(out, Entry{Collection: coll, Name: n, Status: StatusOK})
			} else {
				out = append(out, Entry{Collection: coll, Name: n, Status: StatusMissing})
			}
		}

		extras := make([]string, 0)
		for n := range liveSet {
			if n == mongoIDIndex {
				continue
			}
			if _, ok := byColl[coll][n]; !ok {
				extras = append(extras, n)
			}
		}
		sort.Strings(extras)
		for _, n := range extras {
			out = append(out, Entry{Collection: coll, Name: n, Status: StatusExtra})
		}
	}

	return out, nil
}

// FindSpec returns the manifest entry for the given
// (collection, name), or nil if not in the manifest. Linear scan —
// the manifest has ~60 entries, so the cost is irrelevant compared
// to the round-trip a real query would take.
func FindSpec(manifest []IndexSpec, collection, name string) *IndexSpec {
	for i := range manifest {
		if manifest[i].Collection == collection && manifest[i].Name == name {
			return &manifest[i]
		}
	}
	return nil
}
