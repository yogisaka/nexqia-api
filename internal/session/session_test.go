// internal/session/session_test.go
package session

import (
	"strings"
	"testing"
)

func TestGenerateRawToken_UniqueAndURLSafe(t *testing.T) {
	a, err := generateRawToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := generateRawToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Error("expected two generated tokens to differ")
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("expected URL-safe base64 (no +/=), got %q", a)
	}
}

func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	h1 := hashToken("token-a")
	h2 := hashToken("token-a")
	h3 := hashToken("token-b")
	if h1 != h2 {
		t.Error("expected hashToken to be deterministic for the same input")
	}
	if h1 == h3 {
		t.Error("expected different inputs to hash differently")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex sha256 digest, got %d chars", len(h1))
	}
}
