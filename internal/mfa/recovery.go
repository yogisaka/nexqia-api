// internal/mfa/recovery.go
package mfa

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// RecoveryCodeCount is how many one-time recovery codes are issued per
// enrollment (spec §7).
const RecoveryCodeCount = 10

// GenerateRecoveryCodes creates RecoveryCodeCount random one-time codes,
// formatted "XXXXX-XXXXX" for readability. Callers hash each with the
// existing Argon2id hasher (internal/auth.PasswordHasher) before persisting —
// this package never stores anything itself.
func GenerateRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes[i] = code
	}
	return codes, nil
}

func generateRecoveryCode() (string, error) {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	encoded := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))[:10]
	return encoded[:5] + "-" + encoded[5:], nil
}
