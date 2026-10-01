// internal/auth/password.go
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const argon2Prefix = "$argon2id$"

// ErrInvalidHashFormat means a stored hash string could not be parsed by
// either the Argon2id or bcrypt verifier.
var ErrInvalidHashFormat = errors.New("invalid password hash format")

// Argon2Params controls the cost of new Argon2id hashes. Values come from
// config.Config (ARGON2_MEMORY_KIB/ARGON2_ITERATIONS/ARGON2_PARALLELISM) —
// see 2026-09-15-ratelimit-hardening-design.md §11.
// Existing hashes embed the params they were created with, so changing these
// only affects new/rehashed passwords, never breaks verifying old ones.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

const argon2SaltLen = 16
const argon2KeyLen = 32

func hashArgon2(password string, p Argon2Params) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, argon2KeyLen)
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.MemoryKiB, p.Iterations, p.Parallelism, b64Salt, b64Hash), nil
}

func verifyArgon2(encodedHash, password string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return false, ErrInvalidHashFormat
	}
	var version int
	if n, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || n != 1 {
		return false, ErrInvalidHashFormat
	}
	if version != argon2.Version {
		return false, ErrInvalidHashFormat
	}
	var memory, iterations uint32
	var parallelism uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil || n != 3 {
		return false, ErrInvalidHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHashFormat
	}
	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHashFormat
	}
	computedHash := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expectedHash)))
	return subtle.ConstantTimeCompare(expectedHash, computedHash) == 1, nil
}

// IsArgon2Hash reports whether hash was produced by hashArgon2, as opposed to a
// legacy bcrypt hash — used by PasswordHasher.NeedsRehash to trigger lazy rehash.
func IsArgon2Hash(hash string) bool {
	return strings.HasPrefix(hash, argon2Prefix)
}

func verifyPassword(hash, password string) (bool, error) {
	if IsArgon2Hash(hash) {
		return verifyArgon2(hash, password)
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
