// internal/auth/password_test.go
package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

var testParams = Argon2Params{MemoryKiB: 19456, Iterations: 2, Parallelism: 1}

func TestHashArgon2_RoundTrip(t *testing.T) {
	hash, err := hashArgon2("correct horse battery staple", testParams)
	if err != nil {
		t.Fatalf("unexpected error hashing: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("expected $argon2id$ prefix, got %q", hash)
	}

	ok, err := verifyPassword(hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("unexpected error verifying: %v", err)
	}
	if !ok {
		t.Error("expected correct password to verify")
	}

	ok, err = verifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("unexpected error verifying wrong password: %v", err)
	}
	if ok {
		t.Error("expected incorrect password to fail verification")
	}
}

func TestVerifyPassword_StillAcceptsLegacyBcryptHash(t *testing.T) {
	bcryptHash, err := bcrypt.GenerateFromPassword([]byte("legacy-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("unexpected error generating bcrypt hash: %v", err)
	}

	ok, err := verifyPassword(string(bcryptHash), "legacy-password")
	if err != nil {
		t.Fatalf("unexpected error verifying bcrypt hash: %v", err)
	}
	if !ok {
		t.Error("expected legacy bcrypt hash to still verify correctly")
	}

	ok, err = verifyPassword(string(bcryptHash), "wrong-password")
	if err != nil {
		t.Fatalf("unexpected error verifying wrong password against bcrypt hash: %v", err)
	}
	if ok {
		t.Error("expected wrong password to fail against bcrypt hash")
	}
}

func TestIsArgon2Hash_DistinguishesFormats(t *testing.T) {
	argon2Hash, _ := hashArgon2("x", testParams)
	bcryptHash, _ := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.DefaultCost)

	if !IsArgon2Hash(argon2Hash) {
		t.Error("expected argon2 hash to be recognized as argon2")
	}
	if IsArgon2Hash(string(bcryptHash)) {
		t.Error("expected bcrypt hash to NOT be recognized as argon2")
	}
}
