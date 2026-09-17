// internal/auth/token_test.go
package auth

import (
	"testing"
	"time"
)

const testTokenSecret = "test-secret"

func TestGenerateToken_RoundTrip(t *testing.T) {
	token, err := GenerateToken(testTokenSecret, "user-1", "company-1", "merchant-1", "alice", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	claims, err := ParseToken(testTokenSecret, token)
	if err != nil {
		t.Fatalf("unexpected error parsing: %v", err)
	}
	if claims.UserID != "user-1" || claims.CompanyID != "company-1" || claims.MerchantID != "merchant-1" || claims.Username != "alice" || claims.DeviceID != "device-1" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestParseToken_RejectsExpired(t *testing.T) {
	token, err := GenerateToken(testTokenSecret, "user-1", "company-1", "merchant-1", "alice", "device-1", -time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ParseToken(testTokenSecret, token); err == nil {
		t.Error("expected error parsing expired token")
	}
}

func TestParseToken_RejectsWrongSecret(t *testing.T) {
	token, err := GenerateToken(testTokenSecret, "user-1", "company-1", "merchant-1", "alice", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ParseToken("wrong-secret", token); err == nil {
		t.Error("expected error parsing token signed with a different secret")
	}
}

func TestMerchantSelectionToken_RoundTrip(t *testing.T) {
	token, err := GenerateMerchantSelectionToken(testTokenSecret, "user-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	claims, err := ParseMerchantSelectionToken(testTokenSecret, token)
	if err != nil {
		t.Fatalf("unexpected error parsing: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("expected UserID user-1, got %q", claims.UserID)
	}
}

func TestParseMerchantSelectionToken_RejectsNormalAccessToken(t *testing.T) {
	// A regular access token has no "purpose" claim — ParseMerchantSelectionToken
	// must reject it even though the signature is valid (see token.go's purpose check).
	token, err := GenerateToken(testTokenSecret, "user-1", "company-1", "merchant-1", "alice", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ParseMerchantSelectionToken(testTokenSecret, token); err != ErrNotMerchantSelectionToken {
		t.Errorf("expected ErrNotMerchantSelectionToken, got %v", err)
	}
}

func TestParseToken_RejectsMerchantSelectionToken(t *testing.T) {
	// A merchant-selection token must never be usable as a real access token
	// even though it's a validly signed JWT (token-confusion guard, see
	// accessTokenPurpose in token.go).
	token, err := GenerateMerchantSelectionToken(testTokenSecret, "user-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ParseToken(testTokenSecret, token); err == nil {
		t.Error("expected error parsing merchant-selection token as an access token")
	}
}
