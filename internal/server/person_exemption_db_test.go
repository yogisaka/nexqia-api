//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
)

// TestPersonExemptionDB_Constraints — spec 2026-10-07-registration-validation §3:
// 'other' without a note must fail (chk_person_nik_exemption_note), a NIK set
// alongside an exemption must fail (chk_person_nik_or_exemption), and a
// 'newborn' exemption without NIK must succeed.
func TestPersonExemptionDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "PXC")

	insert := "INSERT INTO core.person (company_id, full_name, gender, nik, nik_exemption, nik_exemption_note) VALUES ($1, 'NIK Exemption Probe', 'male', $2, $3, $4) RETURNING id"

	if _, err := pool.Exec(ctx, insert, tn.companyID, "3201234500010001", "other", nil); err == nil || !strings.Contains(err.Error(), "chk_person_nik_exemption_note") {
		t.Fatalf("'other' without note must violate chk_person_nik_exemption_note, got %v", err)
	}
	if _, err := pool.Exec(ctx, insert, tn.companyID, "3201234500010002", "newborn", nil); err == nil || !strings.Contains(err.Error(), "chk_person_nik_or_exemption") {
		t.Fatalf("NIK + exemption must violate chk_person_nik_or_exemption, got %v", err)
	}
	var id string
	if err := pool.QueryRow(ctx, insert, tn.companyID, nil, "newborn", nil).Scan(&id); err != nil || id == "" {
		t.Fatalf("newborn without NIK must succeed, got id=%s (%v)", id, err)
	}
}
