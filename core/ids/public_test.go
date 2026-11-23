package ids

import (
	"strings"
	"testing"
)

func TestNewPublicIDShape(t *testing.T) {
	id := NewPublicID(PublicKindOrganization)
	prefix := PublicIDPrefix + PublicKindOrganization + publicIDSeparator
	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("NewPublicID(%q) = %q, want prefix %q", PublicKindOrganization, id, prefix)
	}
	suffix := strings.TrimPrefix(id, prefix)
	if got, want := len(suffix), DefaultLength; got != want {
		t.Errorf("NewPublicID suffix length = %d, want %d", got, want)
	}
	if !onlyContains(suffix, nanoidAlphabet) {
		t.Errorf("NewPublicID suffix %q contains chars outside the nanoid alphabet", suffix)
	}
}

func TestNewPublicIDUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewPublicID(PublicKindUser)
		if _, dup := seen[id]; dup {
			t.Fatalf("NewPublicID collision after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewPublicIDAllKinds exercises every advertised kind so that
// adding a new PublicKind* constant without an accompanying entry
// here doesn't silently slip through. Each generated ID must
// validate against its own kind.
func TestNewPublicIDAllKinds(t *testing.T) {
	kinds := []string{
		PublicKindOrganization,
		PublicKindUser,
		PublicKindUserEmail,
		PublicKindMembership,
		PublicKindSite,
		PublicKindSiteHost,
		PublicKindPaymentState,
		PublicKindMagicLink,
		PublicKindSession,
		PublicKindAuthIntent,
		PublicKindAuthBlock,
		PublicKindAuthOptOut,
		PublicKindAPIKey,
		PublicKindInvitation,
		PublicKindRequest,
	}
	for _, k := range kinds {
		t.Run(k, func(t *testing.T) {
			id := NewPublicID(k)
			if !ValidatePublicID(k, id) {
				t.Errorf("ValidatePublicID(%q, %q) = false, want true", k, id)
			}
		})
	}
}

func TestValidatePublicID(t *testing.T) {
	valid := NewPublicID(PublicKindOrganization)
	cases := []struct {
		name string
		kind string
		id   string
		want bool
	}{
		{"valid", PublicKindOrganization, valid, true},
		{"wrong-kind", PublicKindUser, valid, false},
		{"missing-prefix", PublicKindOrganization, "org_abc", false},
		{"empty-suffix", PublicKindOrganization, "hb_org_", false},
		{"empty-input", PublicKindOrganization, "", false},
		{"only-prefix", PublicKindOrganization, "hb_", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidatePublicID(c.kind, c.id); got != c.want {
				t.Errorf("ValidatePublicID(%q, %q) = %v, want %v", c.kind, c.id, got, c.want)
			}
		})
	}
}
