// internal/mfa/mfa_test.go
package mfa

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func testKey() []byte {
	key := []byte("0123456789012345678901234567890123456789")
	return key[:KeySize]
}

func TestEncryptor_RoundTrip(t *testing.T) {
	enc, err := NewEncryptor(testKey())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ciphertext, err := enc.Encrypt("super-secret-totp-key")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	plaintext, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if plaintext != "super-secret-totp-key" {
		t.Errorf("expected round-tripped plaintext, got %q", plaintext)
	}
}

func TestEncryptor_DecryptWithWrongKeyFails(t *testing.T) {
	enc, _ := NewEncryptor(testKey())
	ciphertext, err := enc.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	otherKey := make([]byte, KeySize)
	copy(otherKey, []byte("different-key-different-key-abcd"))
	other, _ := NewEncryptor(otherKey)
	if _, err := other.Decrypt(ciphertext); err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestEncryptor_DecryptGarbageFails(t *testing.T) {
	enc, _ := NewEncryptor(testKey())
	if _, err := enc.Decrypt("not-valid-base64-ciphertext!!"); err == nil {
		t.Error("expected error decrypting garbage input")
	}
}

func TestNewEncryptor_RejectsWrongKeySize(t *testing.T) {
	if _, err := NewEncryptor([]byte("too-short")); err == nil {
		t.Error("expected error for key that isn't 32 bytes")
	}
}

func TestNewEncryptorFromBase64_RoundTrip(t *testing.T) {
	enc, err := NewEncryptorFromBase64("MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ciphertext, err := enc.Encrypt("value")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	plaintext, err := enc.Decrypt(ciphertext)
	if err != nil || plaintext != "value" {
		t.Errorf("expected round-tripped plaintext, got %q, err %v", plaintext, err)
	}
}

func TestNewEncryptorFromBase64_RejectsInvalidBase64(t *testing.T) {
	if _, err := NewEncryptorFromBase64("not base64!!"); err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestGenerateSecret_ValidateRoundTrip(t *testing.T) {
	secret, uri, err := GenerateSecret("Nexqia", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secret == "" {
		t.Fatal("expected non-empty secret")
	}
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Errorf("expected otpauth:// URI, got %q", uri)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("failed to generate code: %v", err)
	}
	if !Validate(code, secret) {
		t.Error("expected freshly generated code to validate")
	}
}

func TestQRDataURI_ProducesValidPNG(t *testing.T) {
	_, uri, err := GenerateSecret("Nexqia", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dataURI, err := QRDataURI(uri)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(dataURI, prefix) {
		t.Fatalf("expected %q prefix, got %q", prefix, dataURI)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURI, prefix))
	if err != nil {
		t.Fatalf("failed to decode base64 payload: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("payload is not a valid PNG: %v", err)
	}
	if got := img.Bounds(); got.Dx() != 200 || got.Dy() != 200 {
		t.Errorf("expected 200x200 image, got %v", got)
	}
}

func TestValidate_RejectsWrongCode(t *testing.T) {
	secret, _, err := GenerateSecret("Nexqia", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if Validate("000000", secret) {
		// Astronomically unlikely to collide with the real code; if this ever
		// flakes, the secret above did too.
		t.Error("expected wrong code to fail validation")
	}
}

func TestValidate_ToleratesOneStepClockDrift(t *testing.T) {
	secret, _, err := GenerateSecret("Nexqia", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	past := time.Now().Add(-30 * time.Second)
	code, err := totp.GenerateCode(secret, past)
	if err != nil {
		t.Fatalf("failed to generate code: %v", err)
	}
	if !Validate(code, secret) {
		t.Error("expected code from one step ago to validate (skew tolerance)")
	}
}

func TestValidate_RejectsBeyondSkewTolerance(t *testing.T) {
	secret, _, err := GenerateSecret("Nexqia", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tooOld := time.Now().Add(-3 * time.Minute)
	code, err := totp.GenerateCode(secret, tooOld)
	if err != nil {
		t.Fatalf("failed to generate code: %v", err)
	}
	if Validate(code, secret) {
		t.Error("expected code from 3 minutes ago to fail validation")
	}
}
