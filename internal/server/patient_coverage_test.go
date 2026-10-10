//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
)

// TestPatientCoverage_* covers the kepesertaan / guarantor / guarantee-letter
// API (spec 2026-10-07-tariff-b1-billing-core §3.7–3.9, §6): coverage CRUD on
// a person, duplicate member numbers, visit guarantor full-set replace with
// the BPJS-first ordering and primary payer, and the guarantee-letter replace.

// pcNewPerson creates a person and returns its id.
func pcNewPerson(t *testing.T, env *qeEnv, name string) string {
	t.Helper()
	id, _ := mergeCreatePerson(t, context.Background(), env.pool, env.router, env.token,
		env.companyID, env.merchantID, map[string]string{
			"full_name": name, "gender": "male", "birth_date": "1990-01-01",
		})
	return id
}

// pcCreatePayer creates a payer and returns its id.
func pcCreatePayer(t *testing.T, env *qeEnv, code, name, payerType string) string {
	t.Helper()
	status, out := payCreatePayer(t, env, code, name, payerType, "")
	data := payMapData(t, status, out, http.StatusCreated)
	id, _ := data["id"].(string)
	return id
}

// pcCreatePlan creates a plan under a payer and returns its id.
func pcCreatePlan(t *testing.T, env *qeEnv, payerID, code string) string {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+payerID+"/plans",
		map[string]any{"code": code, "name": "Plan " + code})
	data := payMapData(t, status, out, http.StatusCreated)
	id, _ := data["id"].(string)
	return id
}

// pcPost posts a coverage membership for a person.
func pcPost(t *testing.T, env *qeEnv, personID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	return qeRequest(t, env, http.MethodPost, "/api/v1/persons/"+personID+"/coverages", body)
}

// TestPatientCoverage_CRUD — create/list/patch a coverage, plan-payer
// mismatch, bpjs_class guards, TPA pairing and optimistic locking.
func TestPatientCoverage_CRUD(t *testing.T) {
	env := payNewEnv(t, "pc.crud")
	personID := pcNewPerson(t, env, "Kepesertaan Satu")
	payerID := pcCreatePayer(t, env, "PRV-A", "Asuransi A", "private_insurance")
	planID := pcCreatePlan(t, env, payerID, "PL-A")
	otherPayerID := pcCreatePayer(t, env, "PRV-B", "Asuransi B", "private_insurance")
	otherPlanID := pcCreatePlan(t, env, otherPayerID, "PL-B")

	body := map[string]any{
		"payer_id": payerID, "plan_id": planID, "member_number": "MB-001",
		"valid_from": "2026-01-01", "valid_to": "2026-12-31",
		"tpa_remaining_annual": "50000.00", "tpa_remaining_as_of": "2026-01-01",
	}
	status, out := pcPost(t, env, personID, body)
	created := payMapData(t, status, out, http.StatusCreated)
	if created["member_number"] != "MB-001" || created["plan_id"] != planID ||
		created["valid_from"] != "2026-01-01" || created["valid_to"] != "2026-12-31" ||
		created["tpa_remaining_annual"] != "50000.00" || created["tpa_remaining_as_of"] != "2026-01-01" ||
		created["is_active"] != true || created["row_version"] != float64(1) {
		t.Fatalf("unexpected create payload: %v", created)
	}
	payerObj, _ := created["payer"].(map[string]any)
	if payerObj == nil || payerObj["id"] != payerID || payerObj["payer_type"] != "private_insurance" {
		t.Fatalf("payer object = %v", payerObj)
	}
	if created["bpjs_class"] != nil {
		t.Fatalf("bpjs_class = %v, want nil", created["bpjs_class"])
	}
	coverageID, _ := created["id"].(string)

	// GET list shows the membership.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/persons/"+personID+"/coverages", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, out)
	}
	rows := payDataSlice(t, out)
	if len(rows) != 1 || rows[0]["id"] != coverageID {
		t.Fatalf("coverage list = %v", out)
	}

	// Unknown person → 404.
	status, out = pcPost(t, env, "11111111-1111-1111-1111-111111111111", body)
	if status != http.StatusNotFound || plErrorMsg(out) != "person not found" {
		t.Fatalf("unknown person = %d %v", status, out)
	}

	// Plan of another payer → 400.
	badPlan := map[string]any{"payer_id": payerID, "plan_id": otherPlanID, "member_number": "MB-002"}
	status, out = pcPost(t, env, personID, badPlan)
	if status != http.StatusBadRequest || plErrorMsg(out) != "plan does not belong to payer" {
		t.Fatalf("wrong payer plan = %d %v", status, out)
	}

	// bpjs_class on a non-BPJS payer → 400.
	status, out = pcPost(t, env, personID, map[string]any{
		"payer_id": payerID, "member_number": "MB-003", "bpjs_class": "1",
	})
	if status != http.StatusBadRequest || plErrorMsg(out) != "bpjs_class applies only to a BPJS payer" {
		t.Fatalf("bpjs_class on private = %d %v", status, out)
	}

	// bpjs_class outside 1..3 → 400 even on a BPJS payer.
	bpjsID := pcCreatePayer(t, env, "BPJS-A", "BPJS A", "bpjs")
	status, out = pcPost(t, env, personID, map[string]any{
		"payer_id": bpjsID, "member_number": "MB-B1", "bpjs_class": "9",
	})
	if status != http.StatusBadRequest || plErrorMsg(out) != "invalid bpjs_class" {
		t.Fatalf("bad bpjs_class = %d %v", status, out)
	}

	// Unpaired TPA fields → 400.
	status, out = pcPost(t, env, personID, map[string]any{
		"payer_id": payerID, "member_number": "MB-004", "tpa_remaining_annual": "1000.00",
	})
	if status != http.StatusBadRequest || plErrorMsg(out) != "tpa_remaining_annual and tpa_remaining_as_of must be set together" {
		t.Fatalf("unpaired tpa = %d %v", status, out)
	}

	// Patch: stale row_version → 409, then ok.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/patient-coverages/"+coverageID,
		map[string]any{"member_number": "MB-001X", "row_version": 99})
	if status != http.StatusConflict {
		t.Fatalf("stale patch = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/patient-coverages/"+coverageID,
		map[string]any{"member_number": "MB-001X", "is_active": false, "row_version": 1})
	updated := payMapData(t, status, out, http.StatusOK)
	if updated["member_number"] != "MB-001X" || updated["is_active"] != false ||
		updated["row_version"] != float64(2) {
		t.Fatalf("patched coverage = %v", updated)
	}
}

// TestPatientCoverage_DuplicateMember — UNIQUE (merchant, payer, member
// number) on live rows → 409; another payer may reuse the number.
func TestPatientCoverage_DuplicateMember(t *testing.T) {
	env := payNewEnv(t, "pc.dup")
	personID := pcNewPerson(t, env, "Kepesertaan Dup")
	payerA := pcCreatePayer(t, env, "PRV-D1", "Asuransi D1", "private_insurance")
	payerB := pcCreatePayer(t, env, "PRV-D2", "Asuransi D2", "private_insurance")

	body := map[string]any{"payer_id": payerA, "member_number": "MB-DUP"}
	status, out := pcPost(t, env, personID, body)
	payMapData(t, status, out, http.StatusCreated)

	status, out = pcPost(t, env, personID, body)
	if status != http.StatusConflict || plErrorMsg(out) != "member number already registered" {
		t.Fatalf("duplicate member = %d %v", status, out)
	}

	// Same number under another payer is fine.
	status, out = pcPost(t, env, personID, map[string]any{"payer_id": payerB, "member_number": "MB-DUP"})
	payMapData(t, status, out, http.StatusCreated)
}

// TestPatientCoverage_Guarantors — BPJS sent second is stored at sequence 1,
// primary_payer_id follows the first guarantor, no-BPJS-cooperation and
// coverage-mismatch guards, duplicates, clearing and the cancelled visit.
func TestPatientCoverage_Guarantors(t *testing.T) {
	env := payNewEnv(t, "pc.guar")
	ctx := context.Background()
	personID := pcNewPerson(t, env, "Kunjungan Jamin")
	otherPersonID := pcNewPerson(t, env, "Kunjungan Lain")
	bpjsID := pcCreatePayer(t, env, "BPJS-G", "BPJS G", "bpjs")
	privID := pcCreatePayer(t, env, "PRV-G", "Asuransi G", "private_insurance")

	// Coverage for the visit's person and (mismatched) one for another person.
	status, out := pcPost(t, env, personID, map[string]any{
		"payer_id": bpjsID, "member_number": "MB-G1", "bpjs_class": "2",
	})
	covA := payMapData(t, status, out, http.StatusCreated)
	covAID, _ := covA["id"].(string)
	status, out = pcPost(t, env, otherPersonID, map[string]any{
		"payer_id": bpjsID, "member_number": "MB-G2", "bpjs_class": "1",
	})
	covB := payMapData(t, status, out, http.StatusCreated)
	covBID, _ := covB["id"].(string)

	// Seed an active visit for personID.
	mergeSeedAdmission(t, ctx, env.pool, env.companyID, env.merchantID, personID, "pcguar")
	var admissionID string
	if err := env.pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionID); err != nil {
		t.Fatalf("find admission: %v", err)
	}
	put := func(guarantors []map[string]any) (int, map[string]any) {
		return qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
			map[string]any{"guarantors": guarantors})
	}

	// Default profile bpjs_cooperation = 'none' → BPJS payer rejected (409),
	// even though it is sent second.
	status, out = put([]map[string]any{
		{"payer_id": privID},
		{"payer_id": bpjsID, "coverage_id": covAID, "policy_number": "POL-1"},
	})
	if status != http.StatusConflict || plErrorMsg(out) != "merchant has no BPJS cooperation" {
		t.Fatalf("no cooperation = %d %v", status, out)
	}

	// Enable FKTP capitation cooperation → the same payload now succeeds and
	// BPJS is moved to sequence 1.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_capitation"})
	payMapData(t, status, out, http.StatusOK)
	status, out = put([]map[string]any{
		{"payer_id": privID, "guarantor_name": "Asuransi Gantung"},
		{"payer_id": bpjsID, "coverage_id": covAID, "policy_number": "POL-1"},
	})
	if status != http.StatusOK {
		t.Fatalf("put guarantors = %d %v", status, out)
	}
	guarantors, _ := out["data"].(map[string]any)["guarantors"].([]any)
	if len(guarantors) != 2 {
		t.Fatalf("guarantors = %v", out)
	}
	g0, _ := guarantors[0].(map[string]any)
	g1, _ := guarantors[1].(map[string]any)
	if g0["payer_id"] != bpjsID || g0["sequence"] != float64(1) ||
		g0["coverage_id"] != covAID || g0["policy_number"] != "POL-1" {
		t.Fatalf("guarantor[0] = %v", g0)
	}
	if g1["payer_id"] != privID || g1["sequence"] != float64(2) || g1["guarantor_name"] != "Asuransi Gantung" {
		t.Fatalf("guarantor[1] = %v", g1)
	}
	// primary_payer_id follows the first guarantor (BPJS).
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/admissions/"+admissionID, nil)
	adm := payMapData(t, status, out, http.StatusOK)
	if adm["primary_payer_id"] != bpjsID {
		t.Fatalf("primary_payer_id = %v, want %s", adm["primary_payer_id"], bpjsID)
	}

	// Duplicate payer → 400.
	status, out = put([]map[string]any{{"payer_id": privID}, {"payer_id": privID}})
	if status != http.StatusBadRequest || plErrorMsg(out) != "duplicate payer" {
		t.Fatalf("duplicate payer = %d %v", status, out)
	}

	// Coverage of another person / mismatch → 400.
	status, out = put([]map[string]any{{"payer_id": bpjsID, "coverage_id": covBID}})
	if status != http.StatusBadRequest || plErrorMsg(out) != "coverage does not match" {
		t.Fatalf("coverage mismatch = %d %v", status, out)
	}

	// Clearing the set → primary_payer_id becomes NULL.
	status, out = put([]map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("clear guarantors = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/admissions/"+admissionID, nil)
	adm = payMapData(t, status, out, http.StatusOK)
	if adm["primary_payer_id"] != nil {
		t.Fatalf("primary_payer_id after clear = %v, want nil", adm["primary_payer_id"])
	}

	// Cancelled visit → 409.
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.admission SET status = 'cancelled' WHERE id = $1", admissionID); err != nil {
		t.Fatalf("cancel admission: %v", err)
	}
	status, out = put([]map[string]any{{"payer_id": privID}})
	if status != http.StatusConflict {
		t.Fatalf("cancelled visit guarantors = %d %v", status, out)
	}
}

// TestPatientCoverage_GuaranteeLetters — full-set replace with soft delete of
// the previous letters; payers must guarantee the visit.
func TestPatientCoverage_GuaranteeLetters(t *testing.T) {
	env := payNewEnv(t, "pc.gl")
	ctx := context.Background()
	personID := pcNewPerson(t, env, "Surat Jaminan")
	payerID := pcCreatePayer(t, env, "PRV-L", "Asuransi L", "private_insurance")
	otherPayerID := pcCreatePayer(t, env, "PRV-X", "Asuransi X", "private_insurance")

	mergeSeedAdmission(t, ctx, env.pool, env.companyID, env.merchantID, personID, "pcgl")
	var admissionID string
	if err := env.pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionID); err != nil {
		t.Fatalf("find admission: %v", err)
	}
	// The payer must first guarantee the visit.
	status, out := qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{{"payer_id": payerID}}})
	if status != http.StatusOK {
		t.Fatalf("seed guarantor = %d %v", status, out)
	}
	put := func(letters []map[string]any) (int, map[string]any) {
		return qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantee-letters",
			map[string]any{"letters": letters})
	}

	// Payer that does not guarantee the visit → 400.
	status, out = put([]map[string]any{{
		"payer_id": otherPayerID, "gl_number": "GL-X", "amount_limit": "100000.00", "valid_from": "2026-01-01",
	}})
	if status != http.StatusBadRequest || plErrorMsg(out) != "payer is not a guarantor of this visit" {
		t.Fatalf("non-guarantor payer = %d %v", status, out)
	}

	// amount_limit must be > 0.
	status, out = put([]map[string]any{{
		"payer_id": payerID, "gl_number": "GL-0", "amount_limit": "0.00", "valid_from": "2026-01-01",
	}})
	if status != http.StatusBadRequest || plErrorMsg(out) != "invalid amount_limit" {
		t.Fatalf("zero amount_limit = %d %v", status, out)
	}

	// Create GL-1, then replace with GL-2 (old letter soft-deleted).
	status, out = put([]map[string]any{{
		"payer_id": payerID, "gl_number": "GL-1", "amount_limit": "500000.00",
		"valid_from": "2026-01-01", "valid_to": "2026-12-31", "note": "awal",
	}})
	if status != http.StatusOK {
		t.Fatalf("put letters = %d %v", status, out)
	}
	letters, _ := out["data"].(map[string]any)["letters"].([]any)
	if len(letters) != 1 {
		t.Fatalf("letters = %v", out)
	}
	l0, _ := letters[0].(map[string]any)
	if l0["gl_number"] != "GL-1" || l0["amount_limit"] != "500000.00" ||
		l0["valid_from"] != "2026-01-01" || l0["valid_to"] != "2026-12-31" || l0["note"] != "awal" {
		t.Fatalf("letter[0] = %v", l0)
	}

	status, out = put([]map[string]any{{
		"payer_id": payerID, "gl_number": "GL-2", "amount_limit": "250000.00", "valid_from": "2026-02-01",
	}})
	payMapData(t, status, out, http.StatusOK)
	letters, _ = out["data"].(map[string]any)["letters"].([]any)
	if len(letters) != 1 {
		t.Fatalf("letters after replace = %v", out)
	}
	l0, _ = letters[0].(map[string]any)
	if l0["gl_number"] != "GL-2" || l0["amount_limit"] != "250000.00" {
		t.Fatalf("replaced letter = %v", l0)
	}

	// Finalized billing account → 409 (letters and guarantors both guarded).
	if _, err := env.pool.Exec(ctx,
		"INSERT INTO operations.billing_account (company_id, merchant_id, admission_id, status, finalized_at, patient_total, payer_total) "+
			"VALUES ($1, $2, $3, 'finalized', now(), 0, 0)",
		env.companyID, env.merchantID, admissionID); err != nil {
		t.Fatalf("seed finalized account: %v", err)
	}
	status, out = put([]map[string]any{{
		"payer_id": payerID, "gl_number": "GL-3", "amount_limit": "100000.00", "valid_from": "2026-01-01",
	}})
	if status != http.StatusConflict || plErrorMsg(out) != "billing is finalized" {
		t.Fatalf("finalized letters = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{{"payer_id": payerID}}})
	if status != http.StatusConflict || plErrorMsg(out) != "billing is finalized" {
		t.Fatalf("finalized guarantors = %d %v", status, out)
	}
}
