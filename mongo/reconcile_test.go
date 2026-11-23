package mongo

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fakeLister is the canonical test double: returns a pre-baked
// index list per collection, or a pre-baked error.
type fakeLister struct {
	byColl map[string][]string
	err    error
}

func (f *fakeLister) ListIndexes(_ context.Context, collection string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byColl[collection], nil
}

func spec(coll, name string) IndexSpec {
	return IndexSpec{
		Collection: coll,
		Name:       name,
		Keys:       bson.D{{Key: name, Value: 1}},
	}
}

func TestReconcile_AllOK(t *testing.T) {
	manifest := []IndexSpec{
		spec("organizations", "public_id_unique"),
		spec("organizations", "slug_unique"),
	}
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_", "public_id_unique", "slug_unique"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []Entry{
		{Collection: "organizations", Name: "public_id_unique", Status: StatusOK},
		{Collection: "organizations", Name: "slug_unique", Status: StatusOK},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries mismatch:\nwant: %+v\ngot:  %+v", want, got)
	}
}

func TestReconcile_Missing(t *testing.T) {
	manifest := []IndexSpec{
		spec("organizations", "public_id_unique"),
		spec("organizations", "slug_unique"),
	}
	// Live cluster only has _id_ — both manifest entries missing.
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, e := range got {
		if e.Status != StatusMissing {
			t.Errorf("expected MISSING for %s/%s, got %s", e.Collection, e.Name, e.Status)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d (%+v)", len(got), got)
	}
}

func TestReconcile_Extra(t *testing.T) {
	manifest := []IndexSpec{
		spec("organizations", "public_id_unique"),
	}
	// Cluster has the manifest index plus an unmanaged extra.
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_", "public_id_unique", "legacy_slug_2024"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []Entry{
		{Collection: "organizations", Name: "public_id_unique", Status: StatusOK},
		{Collection: "organizations", Name: "legacy_slug_2024", Status: StatusExtra},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries mismatch:\nwant: %+v\ngot:  %+v", want, got)
	}
}

func TestReconcile_IgnoresMongoIDIndex(t *testing.T) {
	// Even with no manifest entries, `_id_` should never appear as
	// EXTRA — it's always present and not ours to manage.
	manifest := []IndexSpec{spec("organizations", "anything")}
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, e := range got {
		if e.Name == "_id_" {
			t.Errorf("_id_ leaked into report: %+v", got)
		}
	}
}

func TestReconcile_MultipleCollectionsSorted(t *testing.T) {
	// Collections are reported in alphabetical order so diffs are
	// stable across runs.
	manifest := []IndexSpec{
		spec("users", "public_id_unique"),
		spec("organizations", "public_id_unique"),
		spec("auth_sessions", "public_id_unique"),
	}
	lister := &fakeLister{byColl: map[string][]string{
		"users":         {"_id_", "public_id_unique"},
		"organizations": {"_id_", "public_id_unique"},
		"auth_sessions": {"_id_", "public_id_unique"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantOrder := []string{"auth_sessions", "organizations", "users"}
	for i, e := range got {
		if e.Collection != wantOrder[i] {
			t.Errorf("entry %d collection = %s, want %s (full: %+v)", i, e.Collection, wantOrder[i], got)
		}
	}
}

func TestReconcile_IndexNamesSorted(t *testing.T) {
	// Within a collection, index names are reported alphabetically.
	manifest := []IndexSpec{
		spec("organizations", "z_index"),
		spec("organizations", "a_index"),
		spec("organizations", "m_index"),
	}
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_", "a_index", "m_index", "z_index"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantOrder := []string{"a_index", "m_index", "z_index"}
	for i, e := range got {
		if e.Name != wantOrder[i] {
			t.Errorf("entry %d name = %s, want %s", i, e.Name, wantOrder[i])
		}
	}
}

func TestReconcile_ExtrasSorted(t *testing.T) {
	// Multiple EXTRA entries within a collection are also sorted.
	manifest := []IndexSpec{spec("organizations", "kept")}
	lister := &fakeLister{byColl: map[string][]string{
		"organizations": {"_id_", "kept", "z_extra", "a_extra", "m_extra"},
	}}
	got, err := Reconcile(context.Background(), lister, manifest)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	extras := make([]string, 0)
	for _, e := range got {
		if e.Status == StatusExtra {
			extras = append(extras, e.Name)
		}
	}
	want := []string{"a_extra", "m_extra", "z_extra"}
	if !reflect.DeepEqual(extras, want) {
		t.Errorf("extras order = %v, want %v", extras, want)
	}
}

func TestReconcile_PropagatesListerError(t *testing.T) {
	wantErr := errors.New("boom")
	manifest := []IndexSpec{spec("organizations", "anything")}
	lister := &fakeLister{err: wantErr}
	_, err := Reconcile(context.Background(), lister, manifest)
	if !errors.Is(err, wantErr) {
		t.Errorf("Reconcile error = %v, want wrapping %v", err, wantErr)
	}
}

func TestFindSpec_Hit(t *testing.T) {
	manifest := []IndexSpec{
		spec("organizations", "public_id_unique"),
		spec("organizations", "slug_unique"),
	}
	got := FindSpec(manifest, "organizations", "slug_unique")
	if got == nil {
		t.Fatal("FindSpec returned nil for present spec")
	}
	if got.Name != "slug_unique" {
		t.Errorf("FindSpec.Name = %s, want slug_unique", got.Name)
	}
}

func TestFindSpec_Miss(t *testing.T) {
	manifest := []IndexSpec{spec("organizations", "public_id_unique")}
	if got := FindSpec(manifest, "organizations", "nope"); got != nil {
		t.Errorf("FindSpec for absent spec = %+v, want nil", got)
	}
	if got := FindSpec(manifest, "other_collection", "public_id_unique"); got != nil {
		t.Errorf("FindSpec for wrong collection = %+v, want nil", got)
	}
}
