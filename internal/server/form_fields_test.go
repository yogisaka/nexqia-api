//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// decodeFieldErrors decodes the {"error": ..., "fields": {...}} envelope used
// by respondFieldErrors.
func decodeFieldErrors(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]string) {
	t.Helper()
	var resp struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v: %s", err, rec.Body.String())
	}
	return resp.Error, resp.Fields
}

// TestFormFields_AdmissionCompanion — needs_companion without a name must be
// rejected per field before any write (spec 2026-10-07-form-validation-stage2
// §4.1).
func TestFormFields_AdmissionCompanion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "formfields.companion", "081234570201")

	createPersonViaAPI(t, router, token, companyID, merchantID, "FormFields Companion Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FormFields Companion Person")

	departmentID := "61616161-6161-6161-6161-616161616161"
	if _, err := pool.Exec(ctx, "INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, 'FFC01', 'Poli FF Companion')", departmentID, companyID, merchantID); err != nil {
		t.Fatalf("seed department: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"person_id":       personID,
		"department_id":   departmentID,
		"needs_companion": true,
		"companion_name":  "   ",
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["companion_name"] != "Nama pendamping wajib diisi bila butuh pendamping" {
		t.Fatalf("expected companion_name message, got %q", fields["companion_name"])
	}
}

// TestFormFields_AdmissionBPJSCard — policy_number on a bpjs payer must be a
// 13-digit JKN card number; 12 digits → 400 fields.policy_number, 13 digits or
// absent → 201 (spec §4.1: card number is not required, only its format).
func TestFormFields_AdmissionBPJSCard(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "formfields.bpjs", "081234570202")

	createPersonViaAPI(t, router, token, companyID, merchantID, "FormFields BPJS Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FormFields BPJS Person")

	departmentID := "62626262-6262-6262-6262-626262626262"
	payerID := "63636363-6363-6363-6363-636363636363"
	if _, err := pool.Exec(ctx, "INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, 'FFB01', 'Poli FF BPJS')", departmentID, companyID, merchantID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.payer (id, company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, 'FF-PAY', 'Payer FF BPJS', 'bpjs')", payerID, companyID, merchantID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}

	admit := func(policy string) *httptest.ResponseRecorder {
		payload := map[string]any{
			"person_id":        personID,
			"department_id":    departmentID,
			"primary_payer_id": payerID,
		}
		if policy != "" {
			payload["policy_number"] = policy
		}
		body, _ := json.Marshal(payload)
		return accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	}

	// 12 digits → 400 fields.policy_number (contract: computed from the
	// ^[0-9]{13}$ rule, not from implementation output).
	rec := admit("000012345678")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("12-digit policy expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["policy_number"] != "Nomor kartu BPJS harus 13 digit angka" {
		t.Fatalf("expected policy_number message, got %q", fields["policy_number"])
	}

	// 13 digits → 201.
	if rec := admit("0000123456789"); rec.Code != http.StatusCreated {
		t.Fatalf("13-digit policy expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// No number → 201 (format-only rule).
	if rec := admit(""); rec.Code != http.StatusCreated {
		t.Fatalf("absent policy expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFormFields_AdmissionMissingRequired — a body without department_id is
// answered by bindJSONOrFields with error=validation_failed and the per-field
// message from the validator (contract: "Wajib diisi").
func TestFormFields_AdmissionMissingRequired(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "formfields.missing", "081234570203")

	createPersonViaAPI(t, router, token, companyID, merchantID, "FormFields Missing Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FormFields Missing Person")

	body, _ := json.Marshal(map[string]any{"person_id": personID})
	rec := accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["department_id"] != "Wajib diisi" {
		t.Fatalf("expected department_id=Wajib diisi, got %q", fields["department_id"])
	}
}

// TestFormFields_FamilyRules — family per-field rules (spec §4.2): required
// relationship, responsible-person phone, and a complete payload → 201.
func TestFormFields_FamilyRules(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "formfields.family", "081234570204")

	createPersonViaAPI(t, router, token, companyID, merchantID, "FormFields Family Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FormFields Family Person")

	// Seed a family_relationship_status concept (no seed data ships one).
	var csID, conceptID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test:family-rel:formfields', 'Test Family Rel', '1') RETURNING id",
	).Scan(&csID); err != nil {
		t.Fatalf("seed code_system: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, 'SPOUSE', 'Suami/Istri') RETURNING id",
		csID).Scan(&conceptID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}

	familyPost := func(payload map[string]any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(payload)
		return accountRequest(router, http.MethodPost, "/api/v1/persons/"+personID+"/families", token, companyID, merchantID, body)
	}

	// Without relationship_concept_id → 400 fields.relationship_concept_id.
	rec := familyPost(map[string]any{"full_name": "Keluarga FF Tanpa Hubungan"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing relationship expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["relationship_concept_id"] != "Hubungan keluarga wajib dipilih" {
		t.Fatalf("expected relationship message, got %q", fields["relationship_concept_id"])
	}

	// Responsible person without a phone → 400 fields.phone.
	rec = familyPost(map[string]any{
		"full_name":               "Keluarga FF PJ",
		"is_responsible_person":   true,
		"relationship_concept_id": conceptID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("responsible without phone expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	_, fields = decodeFieldErrors(t, rec)
	if fields["phone"] != "Nomor HP penanggung jawab wajib diisi" {
		t.Fatalf("expected phone message, got %q", fields["phone"])
	}

	// Complete payload → 201.
	rec = familyPost(map[string]any{
		"full_name":               "Keluarga FF Lengkap",
		"is_responsible_person":   true,
		"phone":                   "081234567890",
		"email":                   "keluarga@example.com",
		"relationship_concept_id": conceptID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete family expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFormFields_FamilyBirthDateTodayMerchantTZ — birth_date = today in the
// merchant's timezone is NOT the future (contract "zona merchant"): the check
// must follow core.merchant.timezone, not the UTC calendar date. The merchant
// is pinned to Asia/Jayapura (UTC+9), where the local calendar date runs ahead
// of UTC for 15h of every day.
func TestFormFields_FamilyBirthDateTodayMerchantTZ(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "formfields.familytz", "081234570205")
	if _, err := pool.Exec(ctx, "UPDATE core.merchant SET timezone = 'Asia/Jayapura' WHERE id = $1", merchantID); err != nil {
		t.Fatalf("set merchant timezone: %v", err)
	}

	createPersonViaAPI(t, router, token, companyID, merchantID, "FormFields Family TZ Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FormFields Family TZ Person")

	var csID, conceptID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test:family-rel:formfields-tz', 'Test Family Rel TZ', '1') RETURNING id",
	).Scan(&csID); err != nil {
		t.Fatalf("seed code_system: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, 'SPOUSE', 'Suami/Istri') RETURNING id",
		csID).Scan(&conceptID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}

	// Today's calendar date in the merchant's timezone (Asia/Jayapura).
	var birthDate string
	if err := pool.QueryRow(ctx, "SELECT (now() AT TIME ZONE 'Asia/Jayapura')::date::text").Scan(&birthDate); err != nil {
		t.Fatalf("resolve merchant-local today: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"full_name":               "Keluarga FF TZ",
		"relationship_concept_id": conceptID,
		"birth_date":              birthDate,
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/persons/"+personID+"/families", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("birth_date=%s (merchant-local today) expected 201, got %d: %s", birthDate, rec.Code, rec.Body.String())
	}
}
