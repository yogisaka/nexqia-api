//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestPersonIdentity_NIKUnique — spec §4.7: two live persons in one company
// cannot share a NIK; create conflicts name the colliding medical record.
func TestPersonIdentity_NIKUnique(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "nik.owner", "081234577001")

	body, _ := json.Marshal(map[string]string{"full_name": "NIK Dup One", "gender": "male", "nik": "3201234567890001"})
	if rec := accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, body); rec.Code != http.StatusCreated {
		t.Fatalf("first create with NIK expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	firstID, firstMRN := latestPerson(t, ctx, pool, companyID, "NIK Dup One")

	// Same NIK again → 409 naming the first record's MRN.
	rec := accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate NIK create expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nik already registered to medical record "+firstMRN) {
		t.Fatalf("duplicate NIK create should name MRN %s, got: %s", firstMRN, rec.Body.String())
	}

	// Updating a different person onto the same NIK → 409 too.
	patchBody, _ := json.Marshal(map[string]string{"full_name": "NIK Dup Two", "gender": "male"})
	if rec := accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, patchBody); rec.Code != http.StatusCreated {
		t.Fatalf("second person create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	secondID, _ := latestPerson(t, ctx, pool, companyID, "NIK Dup Two")
	if secondID == firstID {
		t.Fatalf("expected two distinct persons")
	}
	patchBody, _ = json.Marshal(map[string]string{"full_name": "NIK Dup Two", "gender": "male", "nik": "3201234567890001", "medical_record_no": ""})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/persons/"+secondID, token, companyID, merchantID, patchBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("update onto existing NIK expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nik already registered to medical record "+firstMRN) {
		t.Fatalf("update conflict should name MRN %s, got: %s", firstMRN, rec.Body.String())
	}
}

// TestPersonIdentity_MigrationStopsOnDuplicates — spec §4.7: the 000066 guard
// refuses to install the unique index while live patients share a NIK hash.
// Runs entirely inside one transaction (owner role) so the DDL and the probe
// rows roll back together; no other transaction is opened during it.
func TestPersonIdentity_MigrationStopsOnDuplicates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	sqlBytes, err := os.ReadFile("../../migrations/000066_person_nik_unique.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	tn := seedChildRLSTenant(t, ctx, pool, "NKD") // seed before opening the tx

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "DROP INDEX core.uq_person_nik_hash"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO core.person (company_id, full_name, gender, nik_search_hash) VALUES ($1, 'Dup A NKD', 'male', 'dup'), ($1, 'Dup B NKD', 'female', 'dup')",
		tn.companyID); err != nil {
		t.Fatalf("insert duplicate persons: %v", err)
	}
	if _, err := tx.Exec(ctx, string(sqlBytes)); err == nil {
		t.Fatalf("migration should fail while live patients share a NIK hash")
	} else if !strings.Contains(err.Error(), "share a NIK") {
		t.Fatalf("migration error should mention 'share a NIK', got: %v", err)
	}
	// DDL rolls back with the tx — the unique index stays installed for later tests.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// TestPersonIdentity_MergedLookups — spec §8.6: after B is merged into A,
// GET /persons/B points at A and searching B's old MRN surfaces A annotated
// with merged_from_mrn.
func TestPersonIdentity_MergedLookups(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "nik.merge", "081234577002")

	// Survivor holds the NIK, the merged record doesn't (same birth date &
	// gender) — matches the identity-check "ok" pattern from FullFlow.
	survivorID, survivorMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID,
		map[string]string{"full_name": "Merge Target NIK", "gender": "male", "birth_date": "1990-05-10", "nik": "3271012345678901", "phone": "081200010001"})
	mergedID, mergedMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID,
		map[string]string{"full_name": "Merge Target NIK", "gender": "male", "birth_date": "1990-05-10", "phone": "081200010002"})

	preview := mergePreview(t, router, token, companyID, merchantID, survivorID, mergedID)
	mergeExecBody := map[string]any{
		"survivor_id": survivorID, "merged_id": mergedID, "reason": "NIK sama",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"].(float64),
			"merged":   mergeSide(t, preview, "merged")["row_version"].(float64),
		},
		"confirm_mrn":   mergedMRN,
		"field_choices": map[string]string{"phone": "survivor"},
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, mergeExecBody); rec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET /persons/<merged> → 404 pointing at the survivor.
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+mergedID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET merged person expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var notFound struct {
		Error              string `json:"error"`
		MergedIntoPersonID string `json:"merged_into_person_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &notFound); err != nil {
		t.Fatalf("decode 404 body %s: %v", rec.Body.String(), err)
	}
	if notFound.Error != "person merged" || notFound.MergedIntoPersonID != survivorID {
		t.Fatalf("expected person merged into %s, got: %s %s", survivorID, notFound.Error, notFound.MergedIntoPersonID)
	}

	// Searching the merged person's old MRN surfaces the survivor annotated.
	rec = accountRequest(router, http.MethodGet, "/api/v1/persons/search?q="+mergedMRN, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search by old MRN expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	items := mergeRespList(t, rec)
	found := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["ID"] != survivorID {
			continue
		}
		found = true
		mrns, ok := item["merged_from_mrn"].([]any)
		if !ok {
			t.Fatalf("survivor item missing merged_from_mrn: %v", item)
		}
		hasOld := false
		for _, m := range mrns {
			if m == mergedMRN {
				hasOld = true
			}
		}
		if !hasOld {
			t.Fatalf("merged_from_mrn %v should contain old MRN %s", mrns, mergedMRN)
		}
	}
	if !found {
		t.Fatalf("search for old MRN %s should return survivor %s (%s), got: %v", mergedMRN, survivorID, survivorMRN, items)
	}
}
