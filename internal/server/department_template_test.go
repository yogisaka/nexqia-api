//go:build integration

package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestDepartmentTemplate_DB_SeedComplete — migration 000054 seeds the six
// platform department templates with the expected poli counts and the default
// BPJS VClaim code map (138 = 144 - 6; klinik_pratama codes are internal-only).
func TestDepartmentTemplate_DB_SeedComplete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	// Per-template poli counts from spec §3: 6 + 14 + 33 + 62 + 7 + 22 = 144.
	want := map[string]int{
		"klinik_pratama":  6,
		"klinik_utama":    14,
		"rumah_sakit":     33,
		"rs_subspesialis": 62,
		"layanan_program": 7,
		"unit_penunjang":  22,
	}
	var total int
	for code, w := range want {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM core.template_department td
			JOIN core.template t ON t.id = td.template_id
			WHERE t.code = $1 AND t.scope = 'platform' AND t.kind = 'department'`, code).Scan(&n)
		if err != nil {
			t.Fatalf("%s: count template_department: %v", code, err)
		}
		if n != w {
			t.Fatalf("%s: expected %d departments, got %d", code, w, n)
		}
		total += n
	}
	if total != 144 {
		t.Fatalf("expected 144 departments across templates (6+14+33+62+7+22), got %d", total)
	}

	var maps int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_department_code_map m
		JOIN core.template_department td ON td.id = m.template_department_id
		JOIN core.template t ON t.id = td.template_id
		WHERE m.system = 'bpjs-vclaim' AND t.scope = 'platform' AND t.kind = 'department'`).Scan(&maps); err != nil {
		t.Fatalf("count bpjs-vclaim mappings: %v", err)
	}
	// Every department except klinik_pratama's 6 internal-only polis gets a
	// bpjs-vclaim mapping: 144 - 6 = 138.
	if maps != 138 {
		t.Fatalf("expected 138 bpjs-vclaim mappings, got %d", maps)
	}

	var specialties int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_department td
		JOIN core.template t ON t.id = td.template_id
		WHERE t.scope = 'platform' AND t.kind = 'department' AND td.specialty_code IS NOT NULL`).Scan(&specialties); err != nil {
		t.Fatalf("count specialty codes: %v", err)
	}
	// klinik_utama (SPEC-DALAM/ANAK/OBGYN/BEDAH) + rumah_sakit (same four) = 8.
	if specialties != 8 {
		t.Fatalf("expected 8 specialty-coded departments, got %d", specialties)
	}
}

// TestDepartmentTemplate_DB_RuntimeCannotWriteTemplates — app_runtime can read
// the seeded template departments but INSERT into template tables is revoked
// (migration 000054 REVOKE).
func TestDepartmentTemplate_DB_RuntimeCannotWriteTemplates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "DTW")

	denied := []struct {
		name string
		sql  string
	}{
		{"insert template_department", "INSERT INTO core.template_department (template_id, code, name, sort_order) SELECT id, 'X', 'X', 99 FROM core.template WHERE code = 'klinik_utama' AND kind = 'department'"},
		{"insert template_department_code_map", "INSERT INTO core.template_department_code_map (template_department_id, system, code) SELECT id, 'bpjs-vclaim', 'X' FROM core.template_department LIMIT 1"},
	}
	for _, d := range denied {
		tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
		_, err := tx.Exec(ctx, d.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: expected permission denied (42501), got %v", d.name, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback %s: %v", d.name, err)
		}
	}

	// Reads still work: all 144 seeded rows are visible under the visibility policy.
	tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template_department").Scan(&visible); err != nil {
		t.Fatalf("count template_department as app_runtime: %v", err)
	}
	if visible != 144 {
		t.Fatalf("expected 144 template departments visible to app_runtime, got %d", visible)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback read tx: %v", err)
	}
}

// TestDepartmentTemplate_DB_CodeMapTenantFilled — the BEFORE INSERT trigger
// trg_00_fill_tenant fills company_id/merchant_id on core.department_code_map
// from the referenced department, so the caller never sends tenant columns.
func TestDepartmentTemplate_DB_CodeMapTenantFilled(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "DTF")

	var companyID, merchantID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO core.department_code_map (department_id, system, code, display)
		VALUES ($1, 'bpjs-vclaim', 'UMU', 'Poli Umum')
		RETURNING company_id::text, merchant_id::text`,
		tn.departmentID).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert code map without tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("trigger fill: expected company %s merchant %s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}

	// UNIQUE (department_id, system) — a second mapping for the same system is rejected.
	_, err := pool.Exec(ctx, `
		INSERT INTO core.department_code_map (department_id, system, code)
		VALUES ($1, 'bpjs-vclaim', 'UMU-DUP')`, tn.departmentID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate (department_id, system): expected unique violation (23505), got %v", err)
	}
}

// TestDepartmentTemplate_DB_CodeMapMerchantIsolation — the RLS policy
// department_code_map_isolation exposes rows only to the runtime whose
// app.current_merchant_id GUC matches merchant_id.
func TestDepartmentTemplate_DB_CodeMapMerchantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "DTA")
	tnB := seedChildRLSTenant(t, ctx, pool, "DTB")

	for _, tn := range []childRLSTenant{tnA, tnB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO core.department_code_map (department_id, system, code)
			VALUES ($1, 'bpjs-vclaim', 'UMU')`, tn.departmentID); err != nil {
			t.Fatalf("seed code map: %v", err)
		}
	}

	for _, tn := range []childRLSTenant{tnA, tnB} {
		tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
		var own, other int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.department_code_map").Scan(&own); err != nil {
			t.Fatalf("count own code map rows: %v", err)
		}
		if own != 1 {
			t.Fatalf("expected 1 visible code map row for own merchant, got %d", own)
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.department_code_map
			WHERE merchant_id = current_setting('app.current_merchant_id')::uuid
			  AND department_id <> $1`, tn.departmentID).Scan(&other); err != nil {
			t.Fatalf("count foreign code map rows: %v", err)
		}
		if other != 0 {
			t.Fatalf("expected 0 foreign rows visible under own GUC, got %d", other)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}
