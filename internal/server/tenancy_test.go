// internal/server/tenancy_test.go
package server

import "testing"

func TestCompanyCodeFormat_AcceptsSixCharUppercaseAlphanumeric(t *testing.T) {
	valid := []string{"000000", "123456", "999999", "ABCDEF", "A1B2C3", "RSSS01"}
	for _, v := range valid {
		if !companyCodeFormat.MatchString(v) {
			t.Errorf("expected %q to match companyCodeFormat", v)
		}
	}
}

func TestCompanyCodeFormat_RejectsInvalid(t *testing.T) {
	invalid := []string{"", "12345", "1234567", "12345a", "abcdef", "123 456", "AB-123", "ÀBCDEF"}
	for _, v := range invalid {
		if companyCodeFormat.MatchString(v) {
			t.Errorf("expected %q to NOT match companyCodeFormat", v)
		}
	}
}
