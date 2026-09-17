// internal/mfa/recovery_test.go
package mfa

import "testing"

func TestGenerateRecoveryCodes_CountAndFormat(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("expected %d codes, got %d", RecoveryCodeCount, len(codes))
	}
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		if len(code) != 11 || code[5] != '-' {
			t.Errorf("expected format XXXXX-XXXXX, got %q", code)
		}
		if seen[code] {
			t.Errorf("duplicate recovery code generated: %q", code)
		}
		seen[code] = true
	}
}
