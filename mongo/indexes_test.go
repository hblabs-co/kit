package mongo

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestTTL(t *testing.T) {
	p := TTL(0)
	if p == nil || *p != 0 {
		t.Fatalf("TTL(0) = %v, want pointer to 0", p)
	}
	q := TTL(3600)
	if *q != 3600 {
		t.Fatalf("TTL(3600) = %d", *q)
	}
	if p == q {
		t.Fatal("TTL must return a fresh pointer per call")
	}
}

func TestIndexModel(t *testing.T) {
	spec := IndexSpec{
		Collection: "c",
		Name:       "expires_at_ttl",
		Keys:       bson.D{{Key: "expires_at", Value: 1}},
		Unique:     true,
		Sparse:     true,
		Partial:    bson.M{"state": "pending"},
		TTLSeconds: TTL(0),
	}
	m := spec.IndexModel()
	if got, ok := m.Keys.(bson.D); !ok || len(got) != 1 || got[0].Key != "expires_at" {
		t.Fatalf("Keys = %#v", m.Keys)
	}
	if m.Options == nil {
		t.Fatal("Options is nil")
	}
	// Plain spec: only the name is set, and conversion must not panic.
	if m := (IndexSpec{Name: "a", Keys: bson.D{{Key: "a", Value: 1}}}).IndexModel(); m.Options == nil {
		t.Fatal("Options is nil for plain spec")
	}
}

func TestValidateManifest(t *testing.T) {
	ok := []IndexSpec{
		{Collection: "a", Name: "x", Keys: bson.D{{Key: "x", Value: 1}}},
		{Collection: "b", Name: "x", Keys: bson.D{{Key: "x", Value: 1}}}, // same name, other collection
		{Collection: "a", Name: "y", Keys: bson.D{{Key: "y", Value: 1}},
			Partial: bson.M{"y": bson.M{"$exists": true}, "state": "open"}},
	}
	if err := ValidateManifest(ok); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if err := ValidateManifest(nil); err != nil {
		t.Fatalf("empty manifest rejected: %v", err)
	}

	cases := []struct {
		name     string
		manifest []IndexSpec
		want     string
	}{
		{"duplicate name", []IndexSpec{
			{Collection: "a", Name: "x", Keys: bson.D{{Key: "x", Value: 1}}},
			{Collection: "a", Name: "x", Keys: bson.D{{Key: "z", Value: 1}}},
		}, "duplicate index name in a: x"},
		{"empty keys", []IndexSpec{{Collection: "a", Name: "x"}}, "a/x has empty Keys"},
		{"empty name", []IndexSpec{{Collection: "a", Keys: bson.D{{Key: "x", Value: 1}}}}, "entry 0 has empty Collection or Name"},
		{"exists false", []IndexSpec{{Collection: "a", Name: "x", Keys: bson.D{{Key: "x", Value: 1}},
			Partial: bson.M{"deleted_at": bson.M{"$exists": false}}}}, "$exists:false on field \"deleted_at\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateManifest(tc.manifest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// Every problem is reported, not just the first.
	err := ValidateManifest([]IndexSpec{{Collection: "a", Name: "x"}, {Collection: "a", Name: "x"}})
	if err == nil || strings.Count(err.Error(), "\n") != 2 {
		t.Fatalf("want 3 joined errors, got %v", err)
	}
}
