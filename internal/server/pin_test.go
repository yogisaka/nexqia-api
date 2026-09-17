// internal/server/pin_test.go
package server

import "testing"

func TestPinFormat_AcceptsExactlySixDigits(t *testing.T) {
	valid := []string{"000000", "123456", "999999"}
	for _, v := range valid {
		if !pinFormat.MatchString(v) {
			t.Errorf("expected %q to match pinFormat", v)
		}
	}
}

func TestPinFormat_RejectsNonSixDigit(t *testing.T) {
	invalid := []string{"", "12345", "1234567", "12345a", "abcdef", "123 456"}
	for _, v := range invalid {
		if pinFormat.MatchString(v) {
			t.Errorf("expected %q to NOT match pinFormat", v)
		}
	}
}
