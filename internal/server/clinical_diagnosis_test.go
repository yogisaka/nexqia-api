//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Dual-coded visit diagnosis at sign (plan 2026-10-21-cppt-rajal-a Task 4,
// spec 2026-10-21-cppt-rajal-fase1-design §5.2, §7). Fixtures reuse the
// Task 2/3 helpers. The ICD-11 code system + CA00.00 concept are seeded
// straight through the pool (terminology is read-only for app_runtime but the
// test pool is the migration owner); core.diagnosis_config is inserted the
// same way (pool owner bypasses RLS, same pattern as the direct form_instance
// UPDATEs in clinical_form_sign_test.go).

const cdICD11SystemID = "00000000-0000-0000-0004-000000000001"
const cdICD11ConceptID = "00000000-0000-0000-0005-000000000001"

// cdSeedICD11 inserts the ICD-11 diagnosis standard (tagged "diagnosis" so it
// is a valid diagnosis_config value) and the spec §5.2 example concept.
func cdSeedICD11(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO terminology.code_system (id, system_uri, name, version, tags)
VALUES ($1::uuid, 'http://id.who.int/icd11', 'ICD-11', '2024-01', '{diagnosis}')
ON CONFLICT (id) DO NOTHING`, cdICD11SystemID); err != nil {
		t.Fatalf("seed ICD-11 code_system: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO terminology.concept (id, code_system_id, code, display)
VALUES ($1::uuid, $2::uuid, 'CA00.00', 'Acute upper respiratory infection')
ON CONFLICT DO NOTHING`, cdICD11ConceptID, cdICD11SystemID); err != nil {
		t.Fatalf("seed ICD-11 concept: %v", err)
	}
}

// cdSetMerchantStd pins the merchant's diagnosis standard to ICD-11 so the
// dual ICD-10 columns apply (merchant default is ICD-10 → single row).
func cdSetMerchantStd(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.diagnosis_config (company_id, merchant_id, code_system, created_by)
VALUES ($1::uuid, $2::uuid, 'ICD-11', NULLIF($3, '')::uuid)`, companyID, merchantID, userID); err != nil {
		t.Fatalf("seed diagnosis_config: %v", err)
	}
}

func cdDiagnoses(t *testing.T, ctx context.Context, pool *pgxpool.Pool, admissionID string) []sqlcgen.ClinicalDiagnosis {
	t.Helper()
	var admID pgtype.UUID
	if err := admID.Scan(admissionID); err != nil {
		t.Fatalf("scan admission id: %v", err)
	}
	rows, err := sqlcgen.New(pool).ListAdmissionDiagnoses(ctx, admID)
	if err != nil {
		t.Fatalf("list diagnoses: %v", err)
	}
	return rows
}

// soapNoICD10 is the §5.2 example minus the icd10 mapping (license §7.4:
// kolom ICD-10 NULL dibolehkan, baris tetap tersimpan).
const soapNoICD10 = `{"sections":{"subjective":"Demam 3 hari, batuk berdahak.","objective":{"sistolic":120,"diastolic":80},"assessment":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"}}]},"plan":"Parasetamol."}}`

// TestClinicalDiagnosisDualCode — signing the LITERAL spec §5.2 SOAP persists
// one clinical.diagnosis row: role primary, concept snapshot (ICD-11 CA00.00),
// ICD-10 mapping source=auto with map_set and chosen_by=signer; a diagnosis
// without icd10 saves with NULL ICD-10 columns.
func TestClinicalDiagnosisDualCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, userID, token := cfSeedEnv(t, ctx, pool, router, "dualdx")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "dualdx")
	cdSeedICD11(t, ctx, pool)
	cdSetMerchantStd(t, ctx, pool, companyID, merchantID, userID)

	// Full §5.2 example (soapFormData carries the LITERAL diagnoses mapping).
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign = %d: %s", rec.Code, rec.Body.String())
	}
	rows := cdDiagnoses(t, ctx, pool, admissionID)
	if len(rows) != 1 {
		t.Fatalf("diagnosis rows = %d, want 1", len(rows))
	}
	dx := rows[0]
	if dx.DxRole != "primary" || dx.FormInstanceID.String() != id {
		t.Fatalf("dx role/entry = %s/%s", dx.DxRole, dx.FormInstanceID)
	}
	if dx.ConceptID.String() != cdICD11ConceptID {
		t.Fatalf("concept_id = %s, want %s", dx.ConceptID, cdICD11ConceptID)
	}
	if dx.PrimarySystem != "ICD-11" || dx.PrimaryCode != "CA00.00" || dx.PrimaryDisplay != "Acute upper respiratory infection" {
		t.Fatalf("primary snapshot = %s/%s/%s", dx.PrimarySystem, dx.PrimaryCode, dx.PrimaryDisplay)
	}
	if !dx.Icd10Code.Valid || dx.Icd10Code.String != "J06.9" || dx.Icd10Display.String != "Acute upper respiratory infection, unspecified" {
		t.Fatalf("icd10 = %v/%v", dx.Icd10Code, dx.Icd10Display)
	}
	if dx.Icd10Source.String != "auto" || dx.Icd10MapSet.String != "icd10-icd11-map" {
		t.Fatalf("icd10 source/map_set = %v/%s", dx.Icd10Source, dx.Icd10MapSet.String)
	}
	if dx.Icd10ChosenBy.String() != userID || dx.Icd10ChosenAt.Valid {
		t.Fatalf("auto chosen_by/at = %s/%v (at harus NULL bila auto)", dx.Icd10ChosenBy, dx.Icd10ChosenAt)
	}

	// Without icd10 → row saved, ICD-10 columns NULL (spec §7.4).
	id2 := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapNoICD10)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id2+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("sign no-icd10 = %d: %s", rec.Code, rec.Body.String())
	}
	rows = cdDiagnoses(t, ctx, pool, admissionID)
	if len(rows) != 2 {
		t.Fatalf("diagnosis rows = %d, want 2", len(rows))
	}
	dx2 := rows[1]
	if dx2.PrimaryCode != "CA00.00" || dx2.Icd10Code.Valid || dx2.Icd10Source.Valid {
		t.Fatalf("no-icd10 row = code %v source %v (want NULL)", dx2.Icd10Code, dx2.Icd10Source)
	}
}

// TestClinicalDiagnosisInvalidShape — malformed role/concept/icd10 →
// 400 {"error":"invalid diagnosis entry"}; entry stays draft, no rows.
func TestClinicalDiagnosisInvalidShape(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "baddx")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "baddx")
	cdSeedICD11(t, ctx, pool)

	cases := map[string]string{
		"role salah":       `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"utama","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"}}]}}}`,
		"concept kosong":   `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00"}}]}}}`,
		"icd10 source":     `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","source":"guess"}}]}}}`,
		"item bukan objek": `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":["ISPA"]}}}`,
	}
	for name, fd := range cases {
		id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, fd)
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
			map[string]any{"password": ownerPassword})
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid diagnosis entry" {
			t.Fatalf("%s: sign = %d %v", name, rec.Code, resp)
		}
		row := cfGetEntryRow(t, ctx, pool, id)
		if row.Status != "draft" {
			t.Fatalf("%s: entry status = %s, want draft (rollback)", name, row.Status)
		}
	}
	if rows := cdDiagnoses(t, ctx, pool, admissionID); len(rows) != 0 {
		t.Fatalf("diagnosis rows = %d, want 0", len(rows))
	}
}

// TestClinicalDiagnosisICD10StandardRequiresMapping — merchant standard
// ICD-10 (default, no diagnosis_config) + concept ICD-11 tanpa icd10 →
// 400 "invalid diagnosis entry" (review A2); dengan mapping icd10 → sign
// 200 dan primary = kode ICD-10 (spec §7.3).
func TestClinicalDiagnosisICD10StandardRequiresMapping(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "std10")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "std10")
	// Concept exists, but the merchant standard stays the default ICD-10.
	cdSeedICD11(t, ctx, pool)

	const noMap = `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"}}]}}}`
	const withMap = `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"}}]}}}`

	// Without icd10 mapping → 400, entry stays draft.
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, noMap)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid diagnosis entry" {
		t.Fatalf("no mapping = %d %v", rec.Code, resp)
	}
	if row := cfGetEntryRow(t, ctx, pool, id); row.Status != "draft" {
		t.Fatalf("entry status = %s, want draft (rollback)", row.Status)
	}

	// With manual icd10 mapping → 200, primary = the ICD-10 code.
	id2 := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, withMap)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id2+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("with mapping = %d: %s", rec.Code, rec.Body.String())
	}
	rows := cdDiagnoses(t, ctx, pool, admissionID)
	if len(rows) != 1 {
		t.Fatalf("diagnosis rows = %d, want 1", len(rows))
	}
	if rows[0].PrimarySystem != "ICD-10" || rows[0].PrimaryCode != "J06.9" {
		t.Fatalf("primary = %s/%s, want ICD-10/J06.9", rows[0].PrimarySystem, rows[0].PrimaryCode)
	}
}

// TestClinicalFormSignAtomic — unresolvable concept.code → 400 invalid
// diagnosis entry and the whole sign transaction rolls back: entry TETAP
// draft (status write undone by RunRequestTx), TIDAK ada baris diagnosis.
func TestClinicalFormSignAtomic(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "atomic")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "atomic")
	cdSeedICD11(t, ctx, pool)
	cdSetMerchantStd(t, ctx, pool, companyID, merchantID, "")

	fd := `{"sections":{"subjective":"Demam.","objective":{},"assessment":{"narrative":"Suspek.","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"TIDAK-ADA","display":"Kode fiktif"}}]},"plan":"."}}`
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, fd)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid diagnosis entry" {
		t.Fatalf("sign atomic = %d %v", rec.Code, resp)
	}
	row := cfGetEntryRow(t, ctx, pool, id)
	if row.Status != "draft" || row.SignatureHash.Valid {
		t.Fatalf("entry not rolled back: status=%s hash=%v", row.Status, row.SignatureHash)
	}
	if rows := cdDiagnoses(t, ctx, pool, admissionID); len(rows) != 0 {
		t.Fatalf("diagnosis rows = %d, want 0 (rollback)", len(rows))
	}
}
