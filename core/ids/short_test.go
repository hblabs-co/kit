package ids

import (
	"strings"
	"testing"
)

func TestNewSlugShape(t *testing.T) {
	s := NewSlug()
	if len(s) != slugLength {
		t.Fatalf("slug length = %d, want %d", len(s), slugLength)
	}
	for i, r := range s {
		if !strings.ContainsRune(slugAlphabet, r) {
			t.Errorf("slug char %d (%q) not in alphabet", i, r)
		}
	}
}

func TestNewSlugUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		s := NewSlug()
		if seen[s] {
			t.Fatalf("duplicate slug %q within 100 draws", s)
		}
		seen[s] = true
	}
}

func TestShortHash(t *testing.T) {
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	got := ShortHash([]byte("hello"))
	if want := "2cf24dba"; got != want {
		t.Fatalf("ShortHash(hello) = %q, want %q", got, want)
	}
	if len(got) != shortHashLength {
		t.Fatalf("ShortHash length = %d, want %d", len(got), shortHashLength)
	}
}

func TestShortHashDeterministic(t *testing.T) {
	a := ShortHash([]byte("same input"))
	b := ShortHash([]byte("same input"))
	if a != b {
		t.Fatalf("ShortHash not deterministic: %q != %q", a, b)
	}
	if c := ShortHash([]byte("other input")); c == a {
		t.Fatalf("ShortHash collided for different inputs: %q", c)
	}
}
