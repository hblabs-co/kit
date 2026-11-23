package ids

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

const (
	// gonanoid's default URL-safe alphabet (64 symbols, ~6 bits/char).
	nanoidAlphabet = "_-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// Crockford base32 — what oklog/ulid encodes to.
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

func TestNew(t *testing.T) {
	id := New()
	if got, want := len(id), DefaultLength; got != want {
		t.Errorf("New() length = %d, want %d", got, want)
	}
	if !onlyContains(id, nanoidAlphabet) {
		t.Errorf("New() = %q contains chars outside the URL-safe nanoid alphabet", id)
	}
}

func TestNewUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("New() collision after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewSize(t *testing.T) {
	for _, size := range []int{1, 8, 16, 21, 32, 64, 128} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			id := NewSize(size)
			if got := len(id); got != size {
				t.Errorf("NewSize(%d) length = %d, want %d", size, got, size)
			}
			if !onlyContains(id, nanoidAlphabet) {
				t.Errorf("NewSize(%d) = %q contains chars outside the URL-safe nanoid alphabet", size, id)
			}
		})
	}
}

func TestULID(t *testing.T) {
	id := ULID()
	if got, want := len(id), 26; got != want {
		t.Errorf("ULID() length = %d, want %d", got, want)
	}
	if _, err := ulid.Parse(id); err != nil {
		t.Errorf("ULID() = %q is not parseable: %v", id, err)
	}
	if !onlyContains(id, crockfordAlphabet) {
		t.Errorf("ULID() = %q contains chars outside Crockford base32", id)
	}
}

// TestULIDMonotonic relies on ulid.Make()'s monotonic entropy source:
// within the same process, every call must return a string strictly
// greater than the last (sortable order matches creation order, even
// for IDs minted in the same millisecond).
func TestULIDMonotonic(t *testing.T) {
	const n = 1000
	prev := ULID()
	for i := 1; i < n; i++ {
		curr := ULID()
		if curr <= prev {
			t.Fatalf("ULID() at iteration %d (%q) is not greater than previous (%q)", i, curr, prev)
		}
		prev = curr
	}
}

// onlyContains reports whether every rune in s is in alphabet.
func onlyContains(s, alphabet string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune(alphabet, r)
	}) == -1
}
