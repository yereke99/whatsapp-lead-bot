package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const password = "Airan2026!Strong"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not in Argon2id PHC format: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Error("the plaintext password leaked into the hash")
	}

	if err := VerifyPassword(password, hash); err != nil {
		t.Errorf("VerifyPassword rejected the correct password: %v", err)
	}
	if err := VerifyPassword("wrong-password-entirely", hash); !errors.Is(err, ErrHashMismatch) {
		t.Errorf("VerifyPassword accepted a wrong password: %v", err)
	}
}

// TestHashesAreSalted guards against two identical passwords producing the
// same stored value, which would make a leaked table trivially groupable.
func TestHashesAreSalted(t *testing.T) {
	const password = "Airan2026!Strong"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Error("two hashes of the same password are identical: the salt is not random")
	}
	if err := VerifyPassword(password, second); err != nil {
		t.Errorf("the second hash does not verify: %v", err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"$argon2id$",
		"$bcrypt$v=19$m=65536,t=1,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=99$m=65536,t=1,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$bad-params$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=1,p=2$!!!$aGFzaA",
	}

	for _, hash := range cases {
		if err := VerifyPassword("anything", hash); err == nil {
			t.Errorf("VerifyPassword accepted a malformed hash: %q", hash)
		}
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	valid := []string{
		"Airan2026!Strong",
		"kaimak-business-2026",
		"өтеқұпия2026",
		"1234567890abc",
	}
	for _, password := range valid {
		if err := ValidatePasswordStrength(password); err != nil {
			t.Errorf("ValidatePasswordStrength(%q) = %v, want nil", password, err)
		}
	}

	invalid := []string{
		"",
		"short1!",
		"123456789",               // digits only, one class
		"abcdefghijk",             // letters only, one class
		strings.Repeat("a1", 300), // too long
	}
	for _, password := range invalid {
		if err := ValidatePasswordStrength(password); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("ValidatePasswordStrength(%q) = %v, want ErrWeakPassword", password, err)
		}
	}
}

func TestHashPasswordRejectsWeakInput(t *testing.T) {
	if _, err := HashPassword("weak"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("HashPassword should refuse a weak password, got %v", err)
	}
}

func TestGenerateToken(t *testing.T) {
	first, err := GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Error("two generated tokens collided")
	}
	if len(first) < 40 {
		t.Errorf("token is shorter than expected: %d chars", len(first))
	}
	// URL-safe base64 must not need escaping in a cookie or header.
	if strings.ContainsAny(first, "+/= ") {
		t.Errorf("token contains characters that need escaping: %q", first)
	}
}

// TestGenerateTokenEnforcesMinimumEntropy ensures a caller cannot accidentally
// request a guessable token.
func TestGenerateTokenEnforcesMinimumEntropy(t *testing.T) {
	token, err := GenerateToken(4)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 {
		t.Errorf("short request should be raised to the 32-byte floor, got %d chars", len(token))
	}
}

// TestDummyHashIsUsable keeps the timing-equalisation path honest: if the
// constant ever stops parsing, the unknown-account branch would return early
// and become distinguishable by response time.
func TestDummyHashIsUsable(t *testing.T) {
	err := VerifyPassword("whatever", dummyHash)
	if err != nil && !errors.Is(err, ErrHashMismatch) {
		t.Errorf("the dummy hash must parse and simply mismatch, got %v", err)
	}
}

func BenchmarkHashPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := HashPassword("Airan2026!Strong"); err != nil {
			b.Fatal(err)
		}
	}
}
