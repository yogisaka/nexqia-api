package mail

import (
	"strings"
	"testing"
)

func TestRenderCredentialExpiryAdminDigest(t *testing.T) {
	// Contract: subject literal from templates.go; text must contain item
	// fields and the negative days value -7 (sudah lewat).
	msg, err := RenderCredentialExpiry(CredentialExpiryData{
		RecipientName: "Budi",
		MerchantName:  "Klinik Sehat",
		IsAdmin:       true,
		Items: []CredentialExpiryItem{
			{StaffName: "dr. Ani", CredentialKind: "str", LicenseNumber: "STR-123", ValidUntil: "2026-11-01", DaysRemaining: 10},
			{StaffName: "drg. Sari", CredentialKind: "sip", LicenseNumber: "SIP-456", ValidUntil: "2026-10-15", DaysRemaining: -7},
		},
	})
	if err != nil {
		t.Fatalf("RenderCredentialExpiry: %v", err)
	}
	if msg.Subject != "Pengingat: masa berlaku STR/SIP/SIPA" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	for _, want := range []string{"dr. Ani", "drg. Sari", "STR-123", "SIP-456", "2026-11-01", "2026-10-15", "-7"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("Text missing %q", want)
		}
	}
	if strings.TrimSpace(msg.HTML) == "" {
		t.Error("HTML is empty")
	}
}
