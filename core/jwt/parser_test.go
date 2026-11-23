package jwt

import (
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("test-secret")

// sign creates a signed HS256 token with the given claims for use in tests.
func sign(t *testing.T, claims gojwt.Claims) string {
	t.Helper()
	raw, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func TestParseUnverified(t *testing.T) {
	parser := NewParser()

	t.Run("valid token with future expiry", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		raw := sign(t, gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(exp),
		})

		token, err := parser.ParseUnverified(raw, &gojwt.RegisteredClaims{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !token.ExpiresAt.Equal(exp.Truncate(time.Second)) && !token.ExpiresAt.Round(time.Second).Equal(exp.Round(time.Second)) {
			t.Errorf("ExpiresAt = %v, want ~%v", token.ExpiresAt, exp)
		}
		if token.Raw != raw {
			t.Errorf("Raw not preserved")
		}
	})

	t.Run("expired token still parses (no claim validation in ParseUnverified)", func(t *testing.T) {
		exp := time.Now().Add(-time.Hour)
		raw := sign(t, gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(exp),
		})

		token, err := parser.ParseUnverified(raw, &gojwt.RegisteredClaims{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token.IsValid() {
			t.Error("expected IsValid() = false for expired token")
		}
	})

	t.Run("token expiring within buffer is parsed but IsValid returns false", func(t *testing.T) {
		exp := time.Now().Add(30 * time.Second)
		raw := sign(t, gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(exp),
		})

		token, err := parser.ParseUnverified(raw, &gojwt.RegisteredClaims{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token.IsValid() {
			t.Error("expected IsValid() = false for token within buffer window")
		}
	})

	t.Run("malformed token string", func(t *testing.T) {
		_, err := parser.ParseUnverified("not.a.jwt", &gojwt.RegisteredClaims{})
		if err == nil {
			t.Fatal("expected error for malformed token")
		}
	})

	t.Run("missing exp claim", func(t *testing.T) {
		// RegisteredClaims with no ExpiresAt set
		raw := sign(t, gojwt.RegisteredClaims{
			Issuer: "test",
		})

		_, err := parser.ParseUnverified(raw, &gojwt.RegisteredClaims{})
		if err == nil {
			t.Fatal("expected error when exp claim is missing")
		}
	})

	t.Run("preserves custom claim fields", func(t *testing.T) {
		// Production usage: each platform inspector embeds
		// RegisteredClaims and adds vendor-specific fields. This guards
		// the round-trip so a future change to ParseUnverified that
		// ignored the caller's claims pointer (or shadowed its fields)
		// would surface here.
		type platformClaims struct {
			gojwt.RegisteredClaims
			UserID string `json:"user_id"`
			Scope  string `json:"scope"`
		}
		exp := time.Now().Add(time.Hour)
		raw := sign(t, platformClaims{
			RegisteredClaims: gojwt.RegisteredClaims{
				ExpiresAt: gojwt.NewNumericDate(exp),
			},
			UserID: "u_123",
			Scope:  "read:profile",
		})

		var claims platformClaims
		token, err := parser.ParseUnverified(raw, &claims)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if claims.UserID != "u_123" {
			t.Errorf("UserID = %q, want %q", claims.UserID, "u_123")
		}
		if claims.Scope != "read:profile" {
			t.Errorf("Scope = %q, want %q", claims.Scope, "read:profile")
		}
		if token == nil || !token.ExpiresAt.Equal(exp.Truncate(time.Second)) {
			t.Errorf("ExpiresAt round-trip wrong on custom-claims path")
		}
	})

	t.Run("ignores invalid signature", func(t *testing.T) {
		// Locks the documented contract: ParseUnverified consumes
		// third-party tokens without access to the signing key, so it
		// must succeed even when the signature segment is garbage.
		// If a future change silently flips this to verified parsing,
		// this test fails and forces a conscious decision.
		exp := time.Now().Add(time.Hour)
		raw := sign(t, gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(exp),
		})
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			t.Fatalf("expected 3-segment JWT, got %d", len(parts))
		}
		tampered := parts[0] + "." + parts[1] + ".AAAAAA"

		token, err := parser.ParseUnverified(tampered, &gojwt.RegisteredClaims{})
		if err != nil {
			t.Fatalf("ParseUnverified must accept tampered signatures: %v", err)
		}
		if token == nil || token.Raw != tampered {
			t.Error("expected DecodedToken with Raw == tampered token string")
		}
	})

	t.Run("exp claim with wrong type", func(t *testing.T) {
		// MapClaims.GetExpirationTime returns an error (not nil-with-
		// no-error) when exp isn't numeric — exercises the err != nil
		// branch of the "missing or invalid exp" guard, complementing
		// the exp == nil branch covered by "missing exp claim".
		raw, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
			"exp": "not-a-number",
		}).SignedString(testSecret)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}

		_, err = parser.ParseUnverified(raw, &gojwt.MapClaims{})
		if err == nil {
			t.Fatal("expected error when exp claim has the wrong type")
		}
	})
}
