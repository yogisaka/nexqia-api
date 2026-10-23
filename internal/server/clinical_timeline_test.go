//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// Composed clinical timeline read model (plan 2026-10-21-cppt-rajal-b Task 2,
// spec 2026-10-21-cppt-rajal-fase1-design §9). Fixtures reuse the shared
// clinical helpers; expectations come from the contract, not the code:
// one admission holding a draft nurse assessment, a signed SOAP (whose
// diagnoses are persisted at sign) and a diet order must surface everything,
// merged newest first; an inactive allergy must not appear; a user without
// clinical.record.view gets 403 and another merchant 404.
func TestClinicalTimeline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "tl")
	cdSeedICD11(t, ctx, pool)
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "tl")

	// One active + one inactive allergy (contract: inactive tidak muncul).
	for _, a := range []struct {
		substance string
		active    bool
	}{{"TL-Penisilin", true}, {"TL-Kacang", false}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, is_active)
			 VALUES ($1, $2, 'drug', $3, $4)`, companyID, personID, a.substance, a.active); err != nil {
			t.Fatalf("seed allergy %s: %v", a.substance, err)
		}
	}

	// Oldest entry first: draft nurse assessment.
	nurseData := `{"sections":{"keluhan":"Pusing sejak pagi.","tanda_vital":{"sistolic":130,"diastolic":85}}}`
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_awal_perawat", "form_data": cfRawJSON(t, nurseData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create nurse draft = %d: %s", rec.Code, rec.Body.String())
	}
	draftID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)

	// Signed SOAP with the §5.2 diagnoses (persisted at sign). Merchant has no
	// diagnosis_config → default standard ICD-10 → primary = ICD-10 J06.9
	// (resolveDiagnosisCodeSystem default), dual icd10 columns empty.
	soapID := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+soapID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("sign soap = %d: %s", rec.Code, rec.Body.String())
	}

	// Newest: diet order.
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "TL-DIET", true)
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": dietID, "route": "oral",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create order = %d: %s", rec.Code, rec.Body.String())
	}
	orderID := resp["data"].(map[string]any)["id"].(string)

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)

	adm := data["admission"].(map[string]any)
	if adm["id"] != admissionID || adm["status"] != "registered" {
		t.Fatalf("admission = %v", adm)
	}
	person := data["person"].(map[string]any)
	if person["id"] != personID || person["name"] != "Pasien CPPT tl" || person["gender"] != "male" {
		t.Fatalf("person = %v", person)
	}
	if mrn, _ := person["mrn"].(string); mrn == "" {
		t.Fatalf("mrn = %v, want the primary medical record number", person["mrn"])
	}
	if bd, _ := person["birth_date"].(string); len(bd) != 0 && len(bd) != 10 {
		t.Fatalf("birth_date = %v, want YYYY-MM-DD", person["birth_date"])
	}

	allergies := data["allergies"].([]any)
	if len(allergies) != 1 || allergies[0].(map[string]any)["substance_name"] != "TL-Penisilin" {
		t.Fatalf("allergies = %v (inactive TL-Kacang must not appear)", allergies)
	}

	diagnoses := data["diagnoses"].([]any)
	if len(diagnoses) != 1 {
		t.Fatalf("diagnoses = %v, want 1", diagnoses)
	}
	dx := diagnoses[0].(map[string]any)
	if dx["dx_role"] != "primary" || dx["primary_system"] != "ICD-10" || dx["primary_code"] != "J06.9" {
		t.Fatalf("diagnosis = %v (default std ICD-10 → primary J06.9)", dx)
	}
	if dx["icd10_code"] != nil || dx["icd10_source"] != nil {
		t.Fatalf("icd10 dual columns = %v/%v, want null under the ICD-10 standard", dx["icd10_code"], dx["icd10_source"])
	}

	orders := data["nutrition_orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("nutrition_orders = %v, want 1", orders)
	}
	if o := orders[0].(map[string]any); o["id"] != orderID || o["diet_type_name"] != "TL-DIET" {
		t.Fatalf("order = %v", o)
	}

	// items: newest first — order, signed SOAP, draft nurse assessment.
	items := data["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v, want 3", items)
	}
	first := items[0].(map[string]any)
	if first["kind"] != "nutrition_order" || first["id"] != orderID || first["diet_type_name"] != "TL-DIET" || first["status"] != "active" {
		t.Fatalf("items[0] = %v, want the diet order", first)
	}
	soap := items[1].(map[string]any)
	if soap["kind"] != "form" || soap["id"] != soapID || soap["status"] != "signed" {
		t.Fatalf("items[1] = %v, want the signed SOAP", soap)
	}
	if soap["signed_at"] == nil || soap["filled_by"] == nil {
		t.Fatalf("soap item fields = %v", soap)
	}
	draft := items[2].(map[string]any)
	if draft["kind"] != "form" || draft["id"] != draftID || draft["status"] != "draft" {
		t.Fatalf("items[2] = %v, want the nurse draft", draft)
	}
	if draft["signed_at"] != nil || draft["counter_signed_at"] != nil || draft["addendum_to_id"] != nil {
		t.Fatalf("draft item optional fields = %v, want null", draft)
	}

	// Permission: a user without clinical.record.view → 403.
	noPerm := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.tl.noview")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", noPerm, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no-perm timeline = %d %v, want 403", rec.Code, resp)
	}
	// Viewer with only clinical.record.view → 200.
	viewer := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.tl.viewer", server.PermClinicalRecordView)
	if rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", viewer, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("viewer timeline = %d: %s", rec.Code, rec.Body.String())
	}

	// Another merchant's admission → 404 with the contract message.
	companyB, merchantB, _, tokenB := cfSeedEnv(t, ctx, pool, router, "tlb")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", tokenB, companyB, merchantB, nil); rec.Code != http.StatusNotFound || resp["error"] != "admission tidak ditemukan" {
		t.Fatalf("cross-merchant timeline = %d %v, want 404", rec.Code, resp)
	}
}
