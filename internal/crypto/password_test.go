package crypto

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordHashCostMeetsBaseline(t *testing.T) {
	// OWASP 2025 baseline for new deployments is bcrypt cost 12.
	// Anything below this should be treated as a regression — if a
	// future contributor relaxes this constant for "faster CI" the
	// test will catch it.
	if PasswordHashCost < 12 {
		t.Fatalf("PasswordHashCost regressed below OWASP baseline: got %d, want >= 12", PasswordHashCost)
	}
}

func TestHashPasswordEmbedsConfiguredCost(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	cost, err := bcrypt.Cost(hash)
	if err != nil {
		t.Fatalf("bcrypt.Cost on returned hash: %v", err)
	}
	if cost != PasswordHashCost {
		t.Fatalf("hash cost: got %d, want %d", cost, PasswordHashCost)
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	pw := "my-test-password"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(pw)); err != nil {
		t.Fatalf("CompareHashAndPassword failed on round-trip: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte("wrong-password")); err == nil {
		t.Fatalf("CompareHashAndPassword should reject wrong password")
	}
}

func TestHashPasswordProducesUniqueSaltsPerInvocation(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	// bcrypt's $2a$cc$salt+hash format encodes a random 16-byte salt
	// per invocation; two hashes of the same password must therefore
	// differ even though both verify against the original.
	if strings.EqualFold(string(a), string(b)) {
		t.Fatalf("expected different hashes for the same password (random salt per call)")
	}
}
