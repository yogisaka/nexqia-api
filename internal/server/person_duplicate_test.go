//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// dupJSON marshals a request body for accountRequest.
func dupJSON(t *testing.T, body map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return raw
}

// TestPersonDuplicate_Candidates — spec §8.4: the scoring rules
// (100 same_nik, 80 birth_gender_name, 70 phone_name) and that unrelated
// records never appear.
func TestPersonDuplicate_Candidates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dupcand.owner", "081234602001")

	// Pair 1: same birth date + gender + similar names, no NIK → 80
	// (until the same_nik case below re-scores it to 100).
	aID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Siti Aminah", "gender": "female", "birth_date": "1990-01-01", "phone": "081111110011",
	})
	bID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Siti Aminah S", "gender": "female", "birth_date": "1990-01-01", "phone": "082222220022",
	})
	// Pair 2: same phone + similar names, different birth dates → 70.
	cID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Rina Maulana", "gender": "female", "birth_date": "1985-05-05", "phone": "083333330033",
	})
	dID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Rina Maulana S", "gender": "female", "birth_date": "1987-07-07", "phone": "083333330033",
	})
	// Unrelated record must never appear.
	mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Zulkarnain Wibowo", "gender": "male", "birth_date": "1975-12-12", "phone": "084444440044",
	})

	fetch := func() []map[string]any {
		t.Helper()
		rec := accountRequest(router, http.MethodGet, "/api/v1/persons/duplicates", token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET duplicates expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode duplicates: %v", err)
		}
		return out.Data
	}

	// Score 100 same_nik: drop the unique hash index, then give the two Siti
	// records the same nik_search_hash via the pool (container owned by this test).
	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS core.uq_person_nik_hash"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.person SET nik_search_hash = 'dup' WHERE id = ANY($1)",
		[]string{aID, bID}); err != nil {
		t.Fatalf("set nik_search_hash: %v", err)
	}

	pairs := fetch()
	if len(pairs) != 2 {
		t.Fatalf("expected exactly 2 candidate pairs, got %d: %v", len(pairs), pairs)
	}
	byKey := map[string][2]any{} // "a|b" sorted → [score, reason]
	for _, p := range pairs {
		pa := p["person_a"].(map[string]any)["id"].(string)
		pb := p["person_b"].(map[string]any)["id"].(string)
		if pa > pb {
			pa, pb = pb, pa
		}
		byKey[pa+"|"+pb] = [2]any{p["score"], p["reason"]}
	}
	if p, ok := byKey[aID+"|"+bID]; !ok || p[0] != float64(100) || p[1] != "same_nik" {
		t.Fatalf("Siti pair = %v (present=%v), want 100 same_nik", p, ok)
	}
	if p, ok := byKey[cID+"|"+dID]; !ok || p[0] != float64(70) || p[1] != "phone_name" {
		t.Fatalf("Rina pair = %v (present=%v), want 70 phone_name", p, ok)
	}
	// The 100-score pair sorts first.
	if pairs[0]["reason"] != "same_nik" {
		t.Fatalf("top pair reason = %v, want same_nik first", pairs[0]["reason"])
	}
}

// TestPersonDuplicate_RequestAndDismiss — spec §8.4: a merge request sorts
// first with status requested; dismiss hides the pair; requesting a merged
// pair → 409.
func TestPersonDuplicate_RequestAndDismiss(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dupreq.owner", "081234602002")

	xID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Budi Santoso", "gender": "male", "birth_date": "1980-02-02", "phone": "085555550011",
	})
	yID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Budi Santoso S", "gender": "male", "birth_date": "1980-02-02", "phone": "085555550022",
	})

	post := func(path string, body map[string]any) int {
		t.Helper()
		rec := accountRequest(router, http.MethodPost, path, token, companyID, merchantID, dupJSON(t, body))
		return rec.Code
	}

	if code := post("/api/v1/persons/duplicates", map[string]any{
		"person_a_id": xID, "person_b_id": yID, "note": "kembar data",
	}); code != http.StatusCreated {
		t.Fatalf("POST duplicates expected 201, got %d", code)
	}

	// The requested pair must sort first with status requested.
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/duplicates", token, companyID, merchantID, nil)
	pairs := mergeRespList(t, rec)
	if len(pairs) == 0 {
		t.Fatalf("expected at least one candidate pair")
	}
	top := pairs[0].(map[string]any)
	pa := top["person_a"].(map[string]any)["id"].(string)
	pb := top["person_b"].(map[string]any)["id"].(string)
	if !((pa == xID && pb == yID) || (pa == yID && pb == xID)) || top["status"] != "requested" {
		t.Fatalf("top pair %s/%s status %v, want x/y requested", pa, pb, top["status"])
	}

	// Dismiss → the pair disappears from the list.
	if code := post("/api/v1/persons/duplicates/dismiss", map[string]any{
		"person_a_id": xID, "person_b_id": yID, "note": "bukan duplikat",
	}); code != http.StatusOK {
		t.Fatalf("dismiss expected 200, got %d", code)
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/persons/duplicates", token, companyID, merchantID, nil)
	for _, r := range mergeRespList(t, rec) {
		p := r.(map[string]any)
		idA := p["person_a"].(map[string]any)["id"].(string)
		idB := p["person_b"].(map[string]any)["id"].(string)
		if (idA == xID && idB == yID) || (idA == yID && idB == xID) {
			t.Fatalf("dismissed pair still listed: %v", p)
		}
	}

	// A pair already merged refuses a new request with 409 pair already merged.
	// The pair row already exists (requested→dismissed above), so flip its
	// status directly; LEAST/GREATEST match the stored id orientation.
	if _, err := pool.Exec(ctx,
		"UPDATE core.person_duplicate_pair SET status = 'merged' WHERE person_a_id = LEAST($1::uuid, $2::uuid) AND person_b_id = GREATEST($1::uuid, $2::uuid)",
		xID, yID); err != nil {
		t.Fatalf("seed merged pair: %v", err)
	}
	rec = accountRequest(router, http.MethodPost, "/api/v1/persons/duplicates", token, companyID, merchantID,
		dupJSON(t, map[string]any{"person_a_id": xID, "person_b_id": yID}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pair already merged") {
		t.Fatalf("request on merged pair expected 409 pair already merged, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonDuplicate_Check — spec §8.5: name + birth date + gender match →
// score 80; no match → []; missing full_name / bad birth_date → 400.
func TestPersonDuplicate_Check(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dupcheck.owner", "081234602003")

	mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Sari Dewi Utami", "gender": "female", "birth_date": "1992-03-03", "phone": "086666660011",
	})

	// Matching candidate → score 80.
	rec := accountRequest(router, http.MethodGet,
		"/api/v1/persons/duplicates/check?full_name=Sari%20Dewi%20Utami&birth_date=1992-03-03&gender=female",
		token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("check expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	items := mergeRespList(t, rec)
	if len(items) != 1 {
		t.Fatalf("check expected 1 candidate, got %s", rec.Body.String())
	}
	cand := items[0].(map[string]any)
	if cand["score"].(float64) != 80 || cand["full_name"] != "Sari Dewi Utami" || cand["birth_date"] != "1992-03-03" {
		t.Fatalf("candidate = %v, want score 80 Sari Dewi Utami 1992-03-03", cand)
	}

	// No match → empty array (not null).
	rec = accountRequest(router, http.MethodGet,
		"/api/v1/persons/duplicates/check?full_name=Nobody%20Here&birth_date=2000-01-01&gender=male",
		token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("check (no match) expected 200, got %d", rec.Code)
	}
	if items := mergeRespList(t, rec); len(items) != 0 {
		t.Fatalf("expected empty candidate list, got %s", rec.Body.String())
	}

	// Missing full_name → 400.
	rec = accountRequest(router, http.MethodGet, "/api/v1/persons/duplicates/check", token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("check without full_name expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid birth_date format → 400.
	rec = accountRequest(router, http.MethodGet,
		"/api/v1/persons/duplicates/check?full_name=Sari&birth_date=03-03-1992",
		token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("check with bad birth_date expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonDuplicate_Permissions — spec §8.4/8.5: merge_request guards the
// list and the request endpoint; dismiss needs core.person.merge, so a
// Pendaftaran-style user (merge_request only) gets 403 on dismiss.
func TestPersonDuplicate_Permissions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dupperm.owner", "081234602004")
	xID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Perm Cek A", "gender": "female", "birth_date": "1970-07-07",
	})
	yID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Perm Cek B", "gender": "female", "birth_date": "1970-07-07",
	})
	raw := dupJSON(t, map[string]any{"person_a_id": xID, "person_b_id": yID})

	noPerm := mergeUserToken(t, ctx, pool, companyID, merchantID, "dupperm.nope", nil)
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/duplicates", noPerm, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("list without merge_request expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodPost, "/api/v1/persons/duplicates", noPerm, companyID, merchantID, raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("request without merge_request expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Pendaftaran-style: has merge_request, lacks core.person.merge → dismiss 403.
	registrar := mergeUserToken(t, ctx, pool, companyID, merchantID, "dupperm.reg", []string{server.PermPersonMergeRequest})
	rec = accountRequest(router, http.MethodPost, "/api/v1/persons/duplicates/dismiss", registrar, companyID, merchantID, raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("dismiss without core.person.merge expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	// …but the registrar can submit a request (sanity check on the token).
	rec = accountRequest(router, http.MethodPost, "/api/v1/persons/duplicates", registrar, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request with merge_request expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}
