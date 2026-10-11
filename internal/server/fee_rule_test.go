//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
)

// TestFeeRule_* / TestPhysician_EmploymentFields cover the practitioner
// employment fields and the fee rule CRUD API (spec
// 2026-10-07-tariff-d-practitioner-fees-tax §3/§4.1).

// frNewEnv is a Jakarta payer-style env for fee-rule tests.
func frNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	return payNewEnv(t, "fr."+label)
}

// frCreatePhysician creates a physician via the API (plus its person) with
// optional employment fields; returns the physician id and the data object.
func frCreatePhysician(t *testing.T, env *qeEnv, label string, fields map[string]any) (string, map[string]any) {
	t.Helper()
	personID := env.seedPerson(t, "Nakes "+label)
	body := map[string]any{"merchant_id": env.merchantID, "person_id": personID}
	for k, v := range fields {
		body[k] = v
	}
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/physicians", body)
	data := payMapData(t, status, out, http.StatusCreated)
	// Physician endpoints return the raw sqlcgen struct (existing shape).
	id, _ := data["ID"].(string)
	if id == "" {
		t.Fatalf("physician id missing: %v", data)
	}
	return id, data
}

// frCreateRule posts a fee rule and asserts the given status; returns data.
func frCreateRule(t *testing.T, env *qeEnv, body map[string]any, want int) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-rules", body)
	if status != want {
		t.Fatalf("create fee rule = %d, want %d: %v", status, want, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

// seedFeeNoPermUser inserts a user whose only role at merchantID lacks
// billing.fee.manage; returns the user id (token via auth.GenerateToken).
func seedFeeNoPermUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, username string) string {
	t.Helper()
	userID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e6e"
	roleID := "6f6f6f6f-6f6f-6f6f-6f6f-6f6f6f6f6f6f"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Fee Access')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, companyID, username, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	return userID
}

// TestPhysician_EmploymentFields — create accepts profession, employment_type,
// ptkp_status and npwp; defaults apply when omitted; an employee without PTKP
// is rejected with the contract message; the list supports ?profession=.
func TestPhysician_EmploymentFields(t *testing.T) {
	env := frNewEnv(t, "emp")

	// Employee with all fields set.
	id, data := frCreatePhysician(t, env, "Dok Gigi", map[string]any{
		"profession": "dentist", "employment_type": "employee",
		"ptkp_status": "TK/0", "npwp": "12.345.678.9-012.345",
	})
	if data["Profession"] != "dentist" || data["EmploymentType"] != "employee" || data["PtkpStatus"] != "TK/0" || data["Npwp"] != "12.345.678.9-012.345" {
		t.Fatalf("employment fields not echoed: %v", data)
	}
	// Existing fields are preserved alongside the new ones.
	if data["MerchantID"] != env.merchantID || data["ID"] != id {
		t.Fatalf("existing physician fields changed: %v", data)
	}

	// Omitted fields → DB defaults (partner/doctor, no PTKP).
	_, def := frCreatePhysician(t, env, "Dok Umum", nil)
	if def["Profession"] != "doctor" || def["EmploymentType"] != "partner" || def["PtkpStatus"] != nil {
		t.Fatalf("defaults wrong: %v", def)
	}

	// Employee without PTKP violates chk_physician_ptkp → contract 400.
	personID := env.seedPerson(t, "Nakes Tanpa PTKP")
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/physicians", map[string]any{
		"merchant_id": env.merchantID, "person_id": personID, "employment_type": "employee",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("employee without ptkp = %d, want 400: %v", status, out)
	}
	if msg, _ := out["error"].(string); msg != "ptkp_status is required for employees only" {
		t.Fatalf("error = %q", msg)
	}

	// ?profession= filter.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/physicians?profession=dentist", nil)
	if status != http.StatusOK {
		t.Fatalf("list dentists = %d: %v", status, out)
	}
	rows := payDataSlice(t, out)
	if len(rows) != 1 || rows[0]["Profession"] != "dentist" {
		t.Fatalf("dentist filter = %d rows: %v", len(rows), rows)
	}
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/physicians?profession=midwife", nil)
	if status != http.StatusOK {
		t.Fatalf("list midwives = %d: %v", status, out)
	}
	if rows := payDataSlice(t, out); len(rows) != 0 {
		t.Fatalf("midwife filter = %d rows: %v", len(rows), rows)
	}
}

// TestFeeRule_CRUD — create, list, patch with row_version (mismatch → 409)
// and soft delete, with the §4.1 specificity score in responses.
func TestFeeRule_CRUD(t *testing.T) {
	env := frNewEnv(t, "crud")
	physicianID, _ := frCreatePhysician(t, env, "FR Crud", nil)

	body := map[string]any{
		"physician_id": physicianID, "item_type": "consultation",
		"basis": "percent_of_total", "percent": "50.00", "effective_from": "2026-01-01",
	}
	data := frCreateRule(t, env, body, http.StatusCreated)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("fee rule id missing: %v", data)
	}
	// physician + item_type → specificity 40 (spec §4.1, no bonuses).
	if data["specificity"] != float64(40) {
		t.Fatalf("specificity = %v, want 40", data["specificity"])
	}
	if data["percent"] != "50.00" || data["basis"] != "percent_of_total" || data["row_version"] != float64(1) {
		t.Fatalf("created rule wrong: %v", data)
	}

	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/fee-rules", nil)
	if status != http.StatusOK {
		t.Fatalf("list fee rules = %d: %v", status, out)
	}
	if rows := payDataSlice(t, out); len(rows) != 1 || rows[0]["id"] != id {
		t.Fatalf("list = %v", rows)
	}

	// PATCH with the current row_version.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/fee-rules/"+id, map[string]any{
		"row_version": 1, "percent": "60.00",
	})
	data = payMapData(t, status, out, http.StatusOK)
	if data["percent"] != "60.00" || data["row_version"] != float64(2) {
		t.Fatalf("patched rule wrong: %v", data)
	}

	// Stale row_version → 409.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/fee-rules/"+id, map[string]any{
		"row_version": 1, "percent": "70.00",
	})
	if status != http.StatusConflict {
		t.Fatalf("stale patch = %d, want 409: %v", status, out)
	}

	// Soft delete: gone from the list, second delete → 404.
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/fee-rules/"+id, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d: %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/fee-rules", nil)
	if status != http.StatusOK {
		t.Fatalf("list after delete = %d: %v", status, out)
	}
	if rows := payDataSlice(t, out); len(rows) != 0 {
		t.Fatalf("rows after delete = %v", rows)
	}
	status, _ = qeRequest(t, env, http.MethodDelete, "/api/v1/fee-rules/"+id, nil)
	if status != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", status)
	}
}

// TestFeeRule_Validation — references must belong to the merchant (404) and
// DB constraints surface as 400 "invalid fee rule: <constraint>".
func TestFeeRule_Validation(t *testing.T) {
	env := frNewEnv(t, "valid")
	other := frNewEnv(t, "validother")
	otherPhysician, _ := frCreatePhysician(t, other, "FR Valid Lain", nil)

	// Physician of another merchant (invisible under RLS) → 404.
	data := frCreateRule(t, env, map[string]any{
		"physician_id": otherPhysician, "basis": "percent_of_total",
		"percent": "10.00", "effective_from": "2026-01-01",
	}, http.StatusNotFound)
	_ = data

	// Unknown physician UUID → 404.
	frCreateRule(t, env, map[string]any{
		"physician_id": "00000000-0000-0000-0000-0000000000aa", "basis": "percent_of_total",
		"percent": "10.00", "effective_from": "2026-01-01",
	}, http.StatusNotFound)

	// chk_fee_rule_value: percent basis without percent → 400.
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-rules", map[string]any{
		"basis": "percent_of_total", "effective_from": "2026-01-01",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("percent basis without percent = %d, want 400: %v", status, out)
	}
	if msg, _ := out["error"].(string); msg != "invalid fee rule: chk_fee_rule_value" {
		t.Fatalf("error = %q", msg)
	}

	// chk_fee_rule_target: service_item_id + item_type together → 400.
	itemID := plCreateItem(t, env, "FR-VAL", "consultation")
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-rules", map[string]any{
		"service_item_id": itemID, "item_type": "consultation", "basis": "percent_of_total",
		"percent": "10.00", "effective_from": "2026-01-01",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("item + item_type = %d, want 400: %v", status, out)
	}
	if msg, _ := out["error"].(string); msg != "invalid fee rule: chk_fee_rule_target" {
		t.Fatalf("error = %q", msg)
	}
}

// TestFeeRule_RequiresPermission — a user whose role lacks billing.fee.manage
// gets 403 on the fee rule endpoints.
func TestFeeRule_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	env := frNewEnv(t, "perm")
	userID := seedFeeNoPermUser(t, ctx, env.pool, env.companyID, env.merchantID, "fr.noperm")
	token, err := auth.GenerateToken(testJWTSecret, userID, env.companyID, env.merchantID, "", "fr.noperm", "fr-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	physicianID, _ := frCreatePhysician(t, env, "FR Perm", nil)

	status, _ := qeRequestToken(t, env.router, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/fee-rules", token, env.companyID, env.merchantID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("GET without billing.fee.manage = %d, want 403", status)
	}
	status, _ = qeRequestToken(t, env.router, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-rules", token, env.companyID, env.merchantID, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_total",
		"percent": "10.00", "effective_from": "2026-01-01",
	})
	if status != http.StatusForbidden {
		t.Fatalf("POST without billing.fee.manage = %d, want 403", status)
	}
}
