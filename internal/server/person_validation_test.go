// internal/server/person_validation_test.go
package server

import (
	"testing"
	"time"
)

// Contract expectation values come from spec 2026-10-07-registration-validation
// §4 (literal messages), not from the implementation.
func TestValidatePatient(t *testing.T) {
	today := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)

	t.Run("patient_required", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{Profile: "patient"}, today)
		want := map[string]string{
			"full_name":  "Nama lengkap wajib diisi",
			"gender":     "Jenis kelamin wajib dipilih",
			"birth_date": "Tanggal lahir wajib diisi",
			"nik":        "NIK wajib diisi, atau pilih alasan tidak punya NIK",
		}
		for k, v := range want {
			if fields[k] != v {
				t.Fatalf("fields[%s] = %q, want %q", k, fields[k], v)
			}
		}
	})

	t.Run("staff_lenient", func(t *testing.T) {
		// staff profile only needs name+gender (createPersonViaAPI shape).
		fields, _ := validatePerson(personRequest{FullName: "Budi", Gender: "male"}, today)
		if len(fields) != 0 {
			t.Fatalf("expected no fields, got %v", fields)
		}
	})

	t.Run("nik_format", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIK: "12345",
		}, today)
		if fields["nik"] != "NIK harus 16 digit angka" {
			t.Fatalf("fields[nik] = %q", fields["nik"])
		}
	})

	t.Run("nik_with_exemption", func(t *testing.T) {
		// Valid: no NIK, exemption given.
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIKExemption: "newborn",
		}, today)
		if len(fields) != 0 {
			t.Fatalf("expected no fields, got %v", fields)
		}
		// NIK + exemption together is rejected.
		fields, _ = validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIK: "3201010101900001", NIKExemption: "newborn",
		}, today)
		if fields["nik_exemption"] != "Kosongkan alasan tanpa NIK bila NIK diisi" {
			t.Fatalf("fields[nik_exemption] = %q", fields["nik_exemption"])
		}
	})

	t.Run("exemption_other_needs_note", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIKExemption: "other",
		}, today)
		if fields["nik_exemption_note"] != "Jelaskan alasan tanpa NIK" {
			t.Fatalf("fields[nik_exemption_note] = %q", fields["nik_exemption_note"])
		}
		fields, _ = validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIKExemption: "other", NIKExemptionNote: "Tanpa dokumen",
		}, today)
		if len(fields) != 0 {
			t.Fatalf("expected no fields, got %v", fields)
		}
	})

	t.Run("exemption_other_note_spaces", func(t *testing.T) {
		// Blank after trimming must fail validation (review fix 2026-10-17).
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1990-01-01", NIKExemption: "other", NIKExemptionNote: "   ",
		}, today)
		if fields["nik_exemption_note"] != "Jelaskan alasan tanpa NIK" {
			t.Fatalf("fields[nik_exemption_note] = %q", fields["nik_exemption_note"])
		}
	})

	t.Run("future_birth", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "2026-10-18",
		}, today)
		if fields["birth_date"] != "Tanggal lahir tidak boleh di masa depan" {
			t.Fatalf("fields[birth_date] = %q", fields["birth_date"])
		}
	})

	t.Run("too_old", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{
			Profile: "patient", FullName: "Budi", Gender: "male",
			BirthDate: "1876-01-01", // > 150 years before 2026-10-17
		}, today)
		if fields["birth_date"] != "Tanggal lahir tidak wajar" {
			t.Fatalf("fields[birth_date] = %q", fields["birth_date"])
		}
	})

	t.Run("phone_formats", func(t *testing.T) {
		for _, ok := range []string{"081234567890", "+6281234567890", "0812-3456-7890"} {
			fields, _ := validatePerson(personRequest{FullName: "Budi", Gender: "male", Phone: ok}, today)
			if fields["phone"] != "" {
				t.Fatalf("phone %q: unexpected %q", ok, fields["phone"])
			}
		}
		fields, _ := validatePerson(personRequest{FullName: "Budi", Gender: "male", Phone: "0212345678"}, today)
		if fields["phone"] != "Nomor HP tidak valid (contoh 081234567890)" {
			t.Fatalf("fields[phone] = %q", fields["phone"])
		}
	})

	t.Run("email", func(t *testing.T) {
		fields, _ := validatePerson(personRequest{FullName: "Budi", Gender: "male", Email: "budi@example.com"}, today)
		if fields["email"] != "" {
			t.Fatalf("valid email rejected: %v", fields)
		}
		for _, bad := range []string{"not-an-email", "Budi <budi@example.com>"} {
			fields, _ = validatePerson(personRequest{FullName: "Budi", Gender: "male", Email: bad}, today)
			if fields["email"] != "Email tidak valid" {
				t.Fatalf("email %q: fields[email] = %q", bad, fields["email"])
			}
		}
	})

	t.Run("nik_warning_female", func(t *testing.T) {
		// DD 41 → female, dd-40=1 matches 1990-01-01 → no warnings.
		nik := "3201014101900001"
		_, warnings := validatePerson(personRequest{
			FullName: "Budi", Gender: "female", BirthDate: "1990-01-01", NIK: nik,
		}, today)
		if len(warnings) != 0 {
			t.Fatalf("expected no warnings, got %v", warnings)
		}
		// Same NIK with male gender → gender mismatch (DD41 is female).
		_, warnings = validatePerson(personRequest{
			FullName: "Budi", Gender: "male", BirthDate: "1990-01-01", NIK: nik,
		}, today)
		if len(warnings) != 1 || warnings[0] != "nik_gender_mismatch" {
			t.Fatalf("want [nik_gender_mismatch], got %v", warnings)
		}
	})

	t.Run("nik_warning_mismatch", func(t *testing.T) {
		_, warnings := validatePerson(personRequest{
			FullName: "Budi", Gender: "female", BirthDate: "1991-02-02", NIK: "3201014101900001",
		}, today)
		if len(warnings) != 1 || warnings[0] != "nik_birthdate_mismatch" {
			t.Fatalf("want [nik_birthdate_mismatch], got %v", warnings)
		}
	})
}
