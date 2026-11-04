//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Clinical board preview endpoint tests (plan 2026-11-04-clinical-board-fase2a-plan
// Task 2, spec 2026-11-04-clinical-board-fase2a-design §API-2). Expectations
// come from the contract: last_soap = newest soap_dokter (filled_at NULLS
// LAST, created_at DESC), at = filled_at ?? created_at, excerpt = first
// non-empty of sections.subjective then .plan truncated to 160 + "…".
func TestClinicalBoardPreview(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "pv")

	get := func(admissionID, tok, comp, merch string) (*int, map[string]any) {
		rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-preview", tok, comp, merch, nil)
		return &rec.Code, resp
	}

	// (a) admission tanpa SOAP → last_soap null, diagnoses/allergies array.
	_, admA := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "pv-a")
	code, resp := get(admA, token, companyID, merchantID)
	if *code != http.StatusOK {
		t.Fatalf("preview no-soap = %d: %s", *code, resp["error"])
	}
	data := resp["data"].(map[string]any)
	if data["last_soap"] != nil {
		t.Fatalf("last_soap = %v, want nil", data["last_soap"])
	}
	if _, ok := data["diagnoses"].([]any); !ok {
		t.Fatalf("diagnoses must be an array, got %T", data["diagnoses"])
	}
	if _, ok := data["allergies"].([]any); !ok {
		t.Fatalf("allergies must be an array, got %T", data["allergies"])
	}

	// (b) dua SOAP (draft lama + draft baru) → last_soap.at milik terbaru,
	// author_name nama (bukan UUID), excerpt dari subjective terpotong 160+"…".
	longSubjective := strings.Repeat("Keluhan demam dan batuk berdahak sejak tiga hari. ", 5) // 250 chars → > 160
	oldForm := `{"sections":{"subjective":"Catatan lama."}}`
	newForm := fmt.Sprintf(`{"sections":{"subjective":%q}}`, longSubjective)
	oldID := cfCreateDraft(t, router, token, companyID, merchantID, admA, oldForm)
	cfCreateDraft(t, router, token, companyID, merchantID, admA, newForm)
	// Deterministic ordering: force the "old" draft one hour earlier
	// (created_at column update, no new writer).
	if _, err := pool.Exec(ctx,
		"UPDATE clinical_form.form_instance SET created_at = created_at - interval '1 hour' WHERE id = $1::uuid", oldID); err != nil {
		t.Fatalf("backdate old draft: %v", err)
	}
	code, resp = get(admA, token, companyID, merchantID)
	if *code != http.StatusOK {
		t.Fatalf("preview two-soap = %d: %s", *code, resp["error"])
	}
	data = resp["data"].(map[string]any)
	soap, ok := data["last_soap"].(map[string]any)
	if !ok {
		t.Fatalf("last_soap = %v, want object", data["last_soap"])
	}
	if soap["at"] == nil || soap["at"] == "" {
		t.Fatalf("last_soap.at = %v, want RFC3339 timestamp", soap["at"])
	}
	author, _ := soap["author_name"].(string)
	if author == "" || strings.Contains(author, "-") && len(author) == 36 {
		t.Fatalf("author_name = %v, want display name not UUID", soap["author_name"])
	}
	wantExcerpt := longSubjective[:160] + "…"
	if soap["excerpt"] != wantExcerpt {
		t.Fatalf("excerpt len = %d, want %d chars (160 + …)", len(soap["excerpt"].(string)), len(wantExcerpt))
	}

	// (c) SOAP tanpa subjective tapi ada plan → excerpt dari plan.
	_, admC := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "pv-c")
	cfCreateDraft(t, router, token, companyID, merchantID, admC,
		`{"sections":{"plan":"Parasetamol 500 mg 3×1 bila demam."}}`)
	code, resp = get(admC, token, companyID, merchantID)
	if *code != http.StatusOK {
		t.Fatalf("preview plan-only = %d: %s", *code, resp["error"])
	}
	soap = resp["data"].(map[string]any)["last_soap"].(map[string]any)
	if soap["excerpt"] != "Parasetamol 500 mg 3×1 bila demam." {
		t.Fatalf("plan-only excerpt = %v", soap["excerpt"])
	}

	// (d) alergi aktif muncul dengan field kontrak.
	_, admD := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "pv-d")
	personD, _ := latestPerson(t, ctx, pool, companyID, "Pasien CPPT pv-d")
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, reaction, severity, is_active)
		 VALUES ($1, $2, 'drug', 'PV-Penisilin', 'ruam', 'moderate', true)`,
		companyID, personD); err != nil {
		t.Fatalf("seed allergy: %v", err)
	}
	code, resp = get(admD, token, companyID, merchantID)
	if *code != http.StatusOK {
		t.Fatalf("preview allergy = %d: %s", *code, resp["error"])
	}
	allergies := resp["data"].(map[string]any)["allergies"].([]any)
	if len(allergies) != 1 {
		t.Fatalf("allergies = %v, want 1", allergies)
	}
	al := allergies[0].(map[string]any)
	if al["substance_name"] != "PV-Penisilin" || al["allergy_type"] != "drug" || al["reaction"] != "ruam" || al["severity"] != "moderate" {
		t.Fatalf("allergy = %v", al)
	}

	// (e) admission merchant lain → 404.
	companyB, merchantB, _, tokenB := cfSeedEnv(t, ctx, pool, router, "pvb")
	code, _ = get(admA, tokenB, companyB, merchantB)
	if *code != http.StatusNotFound {
		t.Fatalf("other merchant = %d, want 404", *code)
	}

	// (f) tanpa izin clinical.record.view → 403.
	none := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.pv.noperm")
	code, _ = get(admA, none, companyID, merchantID)
	if *code != http.StatusForbidden {
		t.Fatalf("no-perm = %d, want 403", *code)
	}
}
