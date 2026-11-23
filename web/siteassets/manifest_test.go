package siteassets

import "testing"

func TestManifest_Resolve_nilReceiver(t *testing.T) {
	t.Parallel()
	var m *Manifest
	if got := m.Resolve("logo.png"); got != "" {
		t.Fatalf("nil receiver: want %q, got %q", "", got)
	}
}

func TestManifest_Resolve_known(t *testing.T) {
	t.Parallel()
	m := &Manifest{Assets: map[string]string{"logo.png": "logo-abc12345.png"}}
	if got, want := m.Resolve("logo.png"), "logo-abc12345.png"; got != want {
		t.Fatalf("Resolve(logo.png): want %q, got %q", want, got)
	}
}

func TestManifest_Resolve_unknown(t *testing.T) {
	t.Parallel()
	m := &Manifest{Assets: map[string]string{"logo.png": "logo-abc12345.png"}}
	if got := m.Resolve("missing.png"); got != "" {
		t.Fatalf("Resolve(missing.png): want %q, got %q", "", got)
	}
}

func TestParseManifest_nilInput(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest(nil)
	if err != nil {
		t.Fatalf("ParseManifest(nil) err: %v", err)
	}
	if m != nil {
		t.Fatalf("ParseManifest(nil) m: want nil, got %+v", m)
	}
}

func TestParseManifest_emptyInput(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte{})
	if err != nil {
		t.Fatalf("ParseManifest(empty) err: %v", err)
	}
	if m != nil {
		t.Fatalf("ParseManifest(empty) m: want nil, got %+v", m)
	}
}

func TestParseManifest_valid(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"_v":1,"assets":{"logo.png":"logo-abc12345.png"}}`)
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m == nil {
		t.Fatal("ParseManifest returned nil for valid input")
	}
	if m.SchemaV != 1 {
		t.Fatalf("SchemaV: want 1, got %d", m.SchemaV)
	}
	if got, want := m.Assets["logo.png"], "logo-abc12345.png"; got != want {
		t.Fatalf("Assets[logo.png]: want %q, got %q", want, got)
	}
}

func TestParseManifest_nilAssetsBecomesEmptyMap(t *testing.T) {
	t.Parallel()
	// Manifest JSON with no `assets` field: ParseManifest normalises
	// the missing map to an empty (non-nil) one so Resolve doesn't
	// have to nil-check on every call.
	raw := []byte(`{"_v":1}`)
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m == nil {
		t.Fatal("ParseManifest returned nil for {} input")
	}
	if m.Assets == nil {
		t.Fatal("Assets should be empty map, not nil")
	}
	if len(m.Assets) != 0 {
		t.Fatalf("Assets len: want 0, got %d", len(m.Assets))
	}
}

func TestParseManifest_invalid(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte(`not json`))
	if err == nil {
		t.Fatal("ParseManifest(garbage) should error")
	}
	if m != nil {
		t.Fatalf("ParseManifest(garbage) m: want nil, got %+v", m)
	}
}
