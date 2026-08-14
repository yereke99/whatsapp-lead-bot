// Package auth implements administrator authentication: Argon2id password
// hashing, database-backed sessions, CSRF tokens and login rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

var (
	ErrInvalidHash    = errors.New("password hash is malformed")
	ErrHashMismatch   = errors.New("password does not match")
	ErrWeakPassword   = errors.New("password is too weak")
	ErrUnsupportedAlg = errors.New("unsupported password hashing algorithm")
)

// argon2Params are the cost parameters for new hashes.
//
// 64 MiB with one pass over three lanes is the OWASP-recommended balance for
// interactive logins: expensive enough to make offline cracking impractical,
// fast enough that an admin login stays under ~100ms on modest hardware.
// Existing hashes carry their own parameters, so these can be raised later
// without invalidating anyone's password.
type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

func defaultParams() argon2Params {
	lanes := uint8(runtime.NumCPU())
	if lanes < 1 {
		lanes = 1
	}
	if lanes > 4 {
		lanes = 4
	}

	return argon2Params{
		memory:      64 * 1024,
		iterations:  1,
		parallelism: lanes,
		saltLength:  16,
		keyLength:   32,
	}
}

// HashPassword produces a PHC-formatted Argon2id hash.
func HashPassword(password string) (string, error) {
	if err := ValidatePasswordStrength(password); err != nil {
		return "", err
	}

	p := defaultParams()
	salt := make([]byte, p.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.iterations, p.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks a password against a stored hash in constant time.
func VerifyPassword(password, encodedHash string) error {
	p, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLength)

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrHashMismatch
	}
	return nil
}

func decodeHash(encoded string) (argon2Params, []byte, []byte, error) {
	var p argon2Params

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, ErrUnsupportedAlg
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrUnsupportedAlg
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	p.saltLength = uint32(len(salt))
	p.keyLength = uint32(len(key))
	return p, salt, key, nil
}

// ValidatePasswordStrength enforces a floor on new passwords.
//
// Length carries most of the entropy, so the bar is a 10-character minimum
// with at least two character classes, rather than a tangle of rules that push
// operators toward "Passw0rd!".
func ValidatePasswordStrength(password string) error {
	if len([]rune(password)) < 10 {
		return fmt.Errorf("%w: кемінде 10 таңба болуы керек", ErrWeakPassword)
	}
	if len(password) > 512 {
		// Argon2 cost is independent of input length, but an unbounded body
		// is still worth rejecting.
		return fmt.Errorf("%w: құпия сөз тым ұзын", ErrWeakPassword)
	}

	var hasLetter, hasDigit, hasSymbol bool
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSymbol = true
		}
	}

	classes := 0
	for _, ok := range []bool{hasLetter, hasDigit, hasSymbol} {
		if ok {
			classes++
		}
	}
	if classes < 2 {
		return fmt.Errorf("%w: әріп пен сан немесе таңба қолданыңыз", ErrWeakPassword)
	}
	return nil
}

// GenerateToken returns a URL-safe random token with n bytes of entropy.
func GenerateToken(n int) (string, error) {
	if n < 16 {
		n = 32
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
