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
	companyID, merchantID, userID, token := cfSeedEnv(t, ctx, pool, router, "tl")
	cdSeedICD11(t, ctx, pool)
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "tl")

	// Another admission of the SAME person (berkas RM revisi Task 3): a
	// document linked only to admissionID must not leak into its timeline.
	admissionBID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "tl-b")

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

	// Two person documents over core.media (contract), seeded LAST so the
	// linked document is newer than every form/order entry (contract e):
	// a surat_rujukan linked to admissionID ('berkas_rm') and a ktp WITHOUT
	// any link.
	seedTimelineDoc := func(checksum, filename, docType string) string {
		t.Helper()
		var mediaID, docID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO core.media (company_id, merchant_id, storage_provider, storage_key, original_filename, mime_type, size_bytes, checksum_sha256, status, uploaded_by)
			 VALUES ($1, $2::uuid, 's3_cdn', 'merchant/'||$2::text||'/'||$3, $4, 'application/pdf', 2048, $3, 'ready', $5::uuid)
			 RETURNING id::text`,
			companyID, merchantID, checksum, filename, userID).Scan(&mediaID); err != nil {
			t.Fatalf("seed media %s: %v", filename, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO core.person_document (company_id, merchant_id, person_id, media_id, document_type, label, created_by)
			 VALUES ($1, $2, $3::uuid, $4::uuid, $5, 'label-'||$5, $6::uuid)
			 RETURNING id::text`,
			companyID, merchantID, personID, mediaID, docType, userID).Scan(&docID); err != nil {
			t.Fatalf("seed person_document %s: %v", docType, err)
		}
		return docID
	}
	rujukanID := seedTimelineDoc("tl-rujukan-checksum", "rujukan-a.pdf", "surat_rujukan")
	seedTimelineDoc("tl-ktp-checksum", "ktp-a.pdf", "ktp")
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.media_link (media_id, owner_table, owner_id, purpose, created_by)
		 SELECT m.id, 'admission', $1::uuid, 'berkas_rm', $2::uuid
		 FROM core.media m WHERE m.checksum_sha256 = 'tl-rujukan-checksum'`,
		admissionID, userID); err != nil {
		t.Fatalf("seed media_link: %v", err)
	}

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

	// items: newest first — person_document (linked, seeded last), diet
	// order, signed SOAP, draft nurse assessment. The unlinked ktp must not
	// appear at all (contract b).
	items := data["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("items = %v, want 4", items)
	}
	doc := items[0].(map[string]any)
	if doc["kind"] != "person_document" || doc["id"] != rujukanID {
		t.Fatalf("items[0] = %v, want the linked surat_rujukan", doc)
	}
	if doc["document_type_name"] != "Surat Rujukan" || doc["document_type"] != "surat_rujukan" {
		t.Fatalf("document item type = %v, want Surat Rujukan/surat_rujukan", doc)
	}
	if doc["filename"] != "rujukan-a.pdf" || doc["size_bytes"] != float64(2048) || doc["label"] != "label-surat_rujukan" {
		t.Fatalf("document item file fields = %v", doc)
	}
	if name, _ := doc["created_by_name"].(string); name == "" {
		t.Fatalf("created_by_name = %v, want filled", doc["created_by_name"])
	}
	if doc["created_at"] == nil {
		t.Fatalf("document created_at = nil")
	}
	for _, it := range items[1:] {
		if it.(map[string]any)["kind"] == "person_document" {
			t.Fatalf("unlinked ktp leaked into items = %v", items)
		}
	}
	first := items[1].(map[string]any)
	if first["kind"] != "nutrition_order" || first["id"] != orderID || first["diet_type_name"] != "TL-DIET" || first["status"] != "active" {
		t.Fatalf("items[1] = %v, want the diet order", first)
	}
	soap := items[2].(map[string]any)
	if soap["kind"] != "form" || soap["id"] != soapID || soap["status"] != "signed" {
		t.Fatalf("items[2] = %v, want the signed SOAP", soap)
	}
	if soap["signed_at"] == nil || soap["filled_by"] == nil {
		t.Fatalf("soap item fields = %v", soap)
	}
	draft := items[3].(map[string]any)
	if draft["kind"] != "form" || draft["id"] != draftID || draft["status"] != "draft" {
		t.Fatalf("items[3] = %v, want the nurse draft", draft)
	}
	if draft["signed_at"] != nil || draft["counter_signed_at"] != nil || draft["addendum_to_id"] != nil {
		t.Fatalf("draft item optional fields = %v, want null", draft)
	}

	// (c) The document linked to admissionID must not surface on the OTHER
	// admission's timeline (same person, no link of its own).
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionBID+"/clinical-timeline", token, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("timeline B = %d: %s", rec.Code, rec.Body.String())
	}
	itemsB := resp["data"].(map[string]any)["items"].([]any)
	for _, it := range itemsB {
		if it.(map[string]any)["kind"] == "person_document" {
			t.Fatalf("admission B items = %v, want no person_document", itemsB)
		}
	}

	// (d) Soft-delete the linked document (deleted_at + deleted_by together,
	// CHECK migration 000073) → entry gone after reload.
	if _, err := pool.Exec(ctx,
		`UPDATE core.person_document SET deleted_at = now(), deleted_by = $2::uuid WHERE id = $1::uuid`,
		rujukanID, userID); err != nil {
		t.Fatalf("soft-delete person_document: %v", err)
	}
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", token, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("timeline reload = %d: %s", rec.Code, rec.Body.String())
	}
	items = resp["data"].(map[string]any)["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["kind"] != "nutrition_order" {
		t.Fatalf("items after soft-delete = %v, want 3 with the diet order on top", items)
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
