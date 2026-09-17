// internal/mfa/mfa.go
// TOTP 2FA — see docs/design/specs/2026-09-15-totp-2fa-design.md.
package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// KeySize is the required length, in bytes, of the AES-256-GCM key used by Encryptor.
const KeySize = 32

// ErrInvalidCiphertext means a stored value could not be decrypted — wrong key,
// truncated data, or tampering (GCM auth tag mismatch).
var ErrInvalidCiphertext = errors.New("mfa: invalid encrypted secret")

// Encryptor encrypts/decrypts TOTP secrets at rest with AES-256-GCM (spec §6).
// The key comes from config.Config.MFASecretEncryptionKey (env
// MFA_SECRET_ENCRYPTION_KEY) — deliberately a dedicated key, separate from any
// future field-encryption key (e.g. core.person.nik), so rotating one never
// invalidates the other.
type Encryptor struct {
	key []byte
}

func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != KeySize {
		return nil, errors.New("mfa: encryption key must be 32 bytes")
	}
	return &Encryptor{key: key}, nil
}

// NewEncryptorFromBase64 decodes a base64-encoded key (the format
// MFA_SECRET_ENCRYPTION_KEY is stored in, see config.Config) and builds an Encryptor.
func NewEncryptorFromBase64(encoded string) (*Encryptor, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("mfa: MFA_SECRET_ENCRYPTION_KEY is not valid base64")
	}
	return NewEncryptor(key)
}

// Encrypt returns nonce||ciphertext, base64-encoded — the format stored directly
// in the existing core.app_user.mfa_secret column.
func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	gcm, err := e.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (e *Encryptor) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	gcm, err := e.gcm()
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", ErrInvalidCiphertext
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plaintext), nil
}

func (e *Encryptor) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// GenerateSecret creates a new TOTP secret plus an otpauth:// provisioning URI for
// QR rendering (spec §5, POST /auth/mfa/setup) — nothing is persisted here.
func GenerateSecret(issuer, accountName string) (secret string, otpauthURI string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
	})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// Validate checks a 6-digit code against secret with ±1 time-step (30s) tolerance
// for clock drift (spec §4, §9).
func Validate(code, secret string) bool {
	valid, _ := totp.ValidateCustom(code, secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return valid
}
