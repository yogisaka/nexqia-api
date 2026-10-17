// internal/server/person_validation.go
// Pure validation for core.person create/update payloads
// (spec 2026-10-07-registration-validation §4). Messages are LITERAL contract
// values — do not reword them.
package server

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// nikExemptionValues mirrors the CHECK on core.person.nik_exemption
// (migration 000072_person_nik_exemption).
var nikExemptionValues = map[string]bool{
	"newborn":          true,
	"foreign_national": true,
	"unidentified":     true,
	"other":            true,
}

var (
	phoneIDPattern = regexp.MustCompile(`^(\+62|62|0)8[0-9]{7,12}$`)
	nikPattern     = regexp.MustCompile(`^[0-9]{16}$`)
)

// validPhoneID accepts an Indonesian mobile number after stripping spaces and
// hyphens (shared with the form-validation stage-2 plan).
func validPhoneID(s string) bool {
	cleaned := strings.NewReplacer(" ", "", "-", "").Replace(s)
	return phoneIDPattern.MatchString(cleaned)
}

// validEmail accepts only a bare address: mail.ParseAddress must succeed AND
// return the exact submitted string ("Budi <b@x.com>" is rejected).
func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}

// validatePerson returns per-field error messages (keyed by JSON field name)
// and non-blocking warnings (NIK vs birth date / gender consistency).
// today must be the business date the birth date is compared against.
func validatePerson(req personRequest, today time.Time) (fields map[string]string, warnings []string) {
	fields = map[string]string{}
	warnings = []string{}
	profile := req.Profile
	if profile == "" {
		profile = "staff"
	}
	// Compare in UTC: time.Parse("2006-01-02") yields UTC midnight, so the
	// reference date must be the UTC calendar date, not local-midnight instants.
	today = today.UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)

	if profile == "patient" {
		if req.FullName == "" {
			fields["full_name"] = "Nama lengkap wajib diisi"
		}
		if req.Gender == "" {
			fields["gender"] = "Jenis kelamin wajib dipilih"
		}
		if req.BirthDate == "" {
			fields["birth_date"] = "Tanggal lahir wajib diisi"
		}
		if req.NIK == "" && req.NIKExemption == "" {
			fields["nik"] = "NIK wajib diisi, atau pilih alasan tidak punya NIK"
		}
	}

	birth, birthErr := time.Parse("2006-01-02", req.BirthDate)
	if birthErr == nil {
		if birth.After(today) {
			fields["birth_date"] = "Tanggal lahir tidak boleh di masa depan"
		}
		if birth.Before(today.AddDate(-150, 0, 0)) {
			fields["birth_date"] = "Tanggal lahir tidak wajar"
		}
	}

	if req.NIK != "" {
		if !nikPattern.MatchString(req.NIK) {
			fields["nik"] = "NIK harus 16 digit angka"
		}
		if req.NIKExemption != "" {
			fields["nik_exemption"] = "Kosongkan alasan tanpa NIK bila NIK diisi"
		}
	}
	if req.NIKExemption != "" && !nikExemptionValues[req.NIKExemption] {
		fields["nik_exemption"] = "Alasan tanpa NIK tidak valid"
	}
	if req.NIKExemption == "other" && req.NIKExemptionNote == "" {
		fields["nik_exemption_note"] = "Jelaskan alasan tanpa NIK"
	}

	if req.Phone != "" && !validPhoneID(req.Phone) {
		fields["phone"] = "Nomor HP tidak valid (contoh 081234567890)"
	}
	if req.Email != "" && !validEmail(req.Email) {
		fields["email"] = "Email tidak valid"
	}
	if req.Gender != "" && req.Gender != "male" && req.Gender != "female" {
		fields["gender"] = "Jenis kelamin tidak valid"
	}

	// Consistency warnings only apply to a well-formed NIK + valid birth date
	// (spec §4: peringatan, bukan blokir).
	if nikPattern.MatchString(req.NIK) && birthErr == nil {
		warnings = append(warnings, nikConsistencyWarnings(req.NIK, birth, req.Gender)...)
	}
	return fields, warnings
}

// nikConsistencyWarnings decodes the embedded DDMMYY of a 16-digit NIK
// (DD > 40 encodes a female) and flags mismatches against the birth date and
// submitted gender.
func nikConsistencyWarnings(nik string, birth time.Time, gender string) []string {
	dd, _ := strconv.Atoi(nik[6:8])
	mm, _ := strconv.Atoi(nik[8:10])
	yy, _ := strconv.Atoi(nik[10:12])
	expectedGender := "male"
	if dd > 40 {
		expectedGender = "female"
		dd -= 40
	}
	var warnings []string
	if dd >= 1 && dd <= 31 && mm >= 1 && mm <= 12 {
		if dd != birth.Day() || mm != int(birth.Month()) || yy != birth.Year()%100 {
			warnings = append(warnings, "nik_birthdate_mismatch")
		}
	}
	if gender != "" && gender != expectedGender {
		warnings = append(warnings, "nik_gender_mismatch")
	}
	return warnings
}
