//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Person-forms ("Salin dari catatan lalu") + patient summary drawer #32
// (plan 2026-11-04-cppt-2a-soap-editor-a-backend Task 4, spec
// 2026-11-04-cppt-2a-soap-editor-design §7). Fixtures reuse cfDo,
// cfSeedEnv/cfSeedAdmission/cfSeedInpatientAdmission/cfSeedUserWithPerms
// (clinical_form_test.go) and cfCreateDraft (clinical_form_sign_test.go).

// cfSOAPWithSections builds a minimal valid soap_dokter v2 form_data with the
// given rekomendasi/plan texts plus the §5 assessment (diagnoses persist at
// sign — clinical_form_sign.go).
func cfSOAPWithSections(rekomendasi, plan string) string {
	sections := `"subjective":"Demam 3 hari.","assessment":{"narrative":"Suspek infeksi","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]}`
	if rekomendasi != "" {
		sections += `,"rekomendasi":"` + rekomendasi + `"`
	}
	if plan != "" {
		sections += `,"plan":"` + plan + `"`
	}
	return `{"sections":{` + sections + `}}`
}

// cfSignEntry signs a draft entry with the owner password (TTE re-auth).
func cfSignEntry(t *testing.T, router *gin.Engine, token, companyID, merchantID, id string) {
	t.Helper()
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID, map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign %s = %d: %s", id, rec.Code, rec.Body.String())
	}
}

// cfCreateFormAny creates a draft of any form_type (empty sections pass —
// soft warnings only) and returns its id.
func cfCreateFormAny(t *testing.T, router *gin.Engine, token, companyID, merchantID, admissionID, formType string) string {
	t.Helper()
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": formType, "form_data": cfRawJSON(t, `{"sections":{}}`),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s draft = %d: %s", formType, rec.Code, rec.Body.String())
	}
	return resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
}

// TestClinicalPersonForms — signed-only, cross-admission, form_type filter,
// default non-gizi filter, merchant isolation (spec §7).
func TestClinicalPersonForms(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "pforms")

	personID, admission1 := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "pforms")
	admission2, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "pforms-b")
	// ICD-11 concept for the SOAP assessment diagnoses (reused helper).
	cdSeedICD11(t, ctx, pool)

	// Signed SOAP on admission 1; draft stays invisible.
	soapID := cfCreateDraft(t, router, token, companyID, merchantID, admission1, cfSOAPWithSections("", "Plan A"))
	cfSignEntry(t, router, token, companyID, merchantID, soapID)
	cfCreateDraft(t, router, token, companyID, merchantID, admission2, cfSOAPWithSections("", "Plan draft"))

	// ── form_type filter: only the signed SOAP, never the draft ──
	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-forms?person_id="+personID+"&form_type=soap_dokter", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("person-forms = %d: %s", rec.Code, rec.Body.String())
	}
	data, _ := resp["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("person-forms form_type=soap_dokter: got %d entries, want 1 (signed only)", len(data))
	}
	if data[0].(map[string]any)["id"] != soapID {
		t.Fatalf("person-forms returned %v, want signed soap %s", data[0], soapID)
	}

	// ── Default non-gizi: gizi entries never come back, non-gizi do ──
	giziAdime := cfCreateFormAny(t, router, token, companyID, merchantID, admission1, "ases_gizi_adime")
	cfSignEntry(t, router, token, companyID, merchantID, giziAdime)
	giziSkrining := cfCreateFormAny(t, router, token, companyID, merchantID, admission1, "ases_gizi_skrining")
	cfSignEntry(t, router, token, companyID, merchantID, giziSkrining)
	resumeID := cfCreateFormAny(t, router, token, companyID, merchantID, admission2, "resume_rajal")
	cfSignEntry(t, router, token, companyID, merchantID, resumeID)

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-forms?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("person-forms (no filter) = %d: %s", rec.Code, rec.Body.String())
	}
	data, _ = resp["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("person-forms default: got %d entries, want 2 (soap+resume; gizi excluded)", len(data))
	}
	formTypes := map[string]bool{}
	for _, it := range data {
		formTypes[it.(map[string]any)["form_type"].(string)] = true
	}
	if !formTypes["soap_dokter"] || !formTypes["resume_rajal"] || formTypes["ases_gizi_adime"] || formTypes["ases_gizi_skrining"] {
		t.Fatalf("person-forms default form_types = %v, want soap_dokter+resume_rajal only", formTypes)
	}

	// ── Merchant isolation: another merchant → 404 ──
	companyB, merchantB, _, tokenB := cfSeedEnv(t, ctx, pool, router, "pformsx")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-forms?person_id="+personID, tokenB, companyB, merchantB, nil); rec.Code != http.StatusNotFound || resp["error"] != "pasien tidak ditemukan" {
		t.Fatalf("person-forms other merchant = %d %v, want 404 pasien tidak ditemukan", rec.Code, resp["error"])
	}
}

// TestClinicalPatientSummary — 5-domain composition, recommendation fallback,
// diagnosis dedup, admission_type filter, empty domains, permission/tenant
// gates (spec §7).
func TestClinicalPatientSummary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "psum")

	personID, admission1 := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "psum")
	admission2, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "psum-b")
	// ICD-11 concept for the SOAP assessment diagnoses (reused helper).
	cdSeedICD11(t, ctx, pool)
	// Non-outpatient admission must NOT surface in recent_visits (filter
	// admission_type, konsisten clinical_timeline.sql:50).
	_ = cfSeedInpatientAdmission(t, ctx, pool, companyID, merchantID, personID, "psum")

	// Allergy from the patient master (pattern clinical_timeline_test.go).
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, is_active) VALUES ($1, $2, 'drug', 'Penisilin', true)",
		companyID, personID); err != nil {
		t.Fatalf("seed allergy: %v", err)
	}

	// SOAP 1: rekomendasi kosong tapi plan terisi → text falls back to plan.
	soap1 := cfCreateDraft(t, router, token, companyID, merchantID, admission1, cfSOAPWithSections("", "Plan satu"))
	cfSignEntry(t, router, token, companyID, merchantID, soap1)

	// Diagnoses dedup happens per signed SOAP; the second SOAP carries the
	// same primary code across admission 2 (dedup keeps newest last_at).
	soap2 := cfCreateDraft(t, router, token, companyID, merchantID, admission2, cfSOAPWithSections("Kontrol 3 hari", "Plan dua"))
	cfSignEntry(t, router, token, companyID, merchantID, soap2)

	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/patient-summary?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("patient-summary = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)

	// person domain (spec §7 shape).
	person := data["person"].(map[string]any)
	if person["id"] != personID || person["name"] != "Pasien CPPT psum" {
		t.Fatalf("person = %v", person)
	}

	// allergies from the patient master.
	allergies := data["allergies"].([]any)
	if len(allergies) != 1 || allergies[0].(map[string]any)["substance_name"] != "Penisilin" {
		t.Fatalf("allergies = %v, want Penisilin", allergies)
	}

	// active_diagnoses: same code on both admissions → deduped to 1.
	dx := data["active_diagnoses"].([]any)
	if len(dx) != 1 {
		t.Fatalf("active_diagnoses = %d entries, want 1 (dedup)", len(dx))
	}
	dxRow := dx[0].(map[string]any)
	if dxRow["primary_code"] != "J06.9" || dxRow["last_at"] == nil {
		t.Fatalf("active_diagnoses[0] = %v, want J06.9 with last_at", dxRow)
	}

	// recent_visits: only the 2 outpatient admissions (inpatient filtered).
	visits := data["recent_visits"].([]any)
	if len(visits) != 2 {
		t.Fatalf("recent_visits = %d entries, want 2 (outpatient only)", len(visits))
	}

	// latest_recommendation: newest signed SOAP (soap2) wins; its
	// rekomendasi terisi → text = rekomendasi.
	rec2 := data["latest_recommendation"].(map[string]any)
	if rec2["text"] != "Kontrol 3 hari" {
		t.Fatalf("latest_recommendation.text = %v, want 'Kontrol 3 hari'", rec2["text"])
	}
	if rec2["admission_id"] != admission2 {
		t.Fatalf("latest_recommendation.admission_id = %v, want %s", rec2["admission_id"], admission2)
	}

	// ── Fallback to plan: a person whose only SOAP has empty rekomendasi ──
	person2, admissionP2 := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "psum-c")
	soapPlan := cfCreateDraft(t, router, token, companyID, merchantID, admissionP2, cfSOAPWithSections("", "Plan fallback"))
	cfSignEntry(t, router, token, companyID, merchantID, soapPlan)
	if _, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/patient-summary?person_id="+person2, token, companyID, merchantID, nil); resp["data"].(map[string]any)["latest_recommendation"].(map[string]any)["text"] != "Plan fallback" {
		t.Fatalf("latest_recommendation.text = %v, want plan fallback", resp["data"].(map[string]any)["latest_recommendation"])
	}

	// ── Person without history → empty domains, not 404 ──
	person3, _ := cfSeedAdmissionNoAdmission(t, ctx, pool, router, token, companyID, merchantID, "psum-d")
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/patient-summary?person_id="+person3, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("patient-summary no-history = %d: %s", rec.Code, rec.Body.String())
	}
	empty := resp["data"].(map[string]any)
	if len(empty["allergies"].([]any)) != 0 || len(empty["active_diagnoses"].([]any)) != 0 || len(empty["recent_visits"].([]any)) != 0 || empty["latest_recommendation"] != nil {
		t.Fatalf("no-history domains = %v, want all empty + null recommendation", empty)
	}

	// ── Permission gate: no clinical.record.view → 403 ──
	none := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, fmt.Sprintf("cf.psum.none.%d", time.Now().UnixNano()%100000))
	if rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/patient-summary?person_id="+personID, none, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("patient-summary without perm = %d, want 403", rec.Code)
	}

	// ── Tenant isolation: other merchant → 404 ──
	companyB, merchantB, _, tokenB := cfSeedEnv(t, ctx, pool, router, "psumx")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/patient-summary?person_id="+personID, tokenB, companyB, merchantB, nil); rec.Code != http.StatusNotFound || resp["error"] != "pasien tidak ditemukan" {
		t.Fatalf("patient-summary other merchant = %d %v, want 404", rec.Code, resp["error"])
	}
}

// cfSeedAdmissionNoAdmission creates a person via API without any admission
// (person tanpa riwayat → domain kosong, bukan 404).
func cfSeedAdmissionNoAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID, label string) (personID string, _ string) {
	t.Helper()
	createPersonViaAPI(t, router, token, companyID, merchantID, "Pasien CPPT "+label, "female")
	personID, _ = latestPerson(t, ctx, pool, companyID, "Pasien CPPT "+label)
	return personID, ""
}
