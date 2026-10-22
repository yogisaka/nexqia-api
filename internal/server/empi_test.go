//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// EMPI G2 Task 1 endpoint tests (plan 2026-10-20-empi-g2 Task 1, spec §6.3):
// resolve + master detail lintas company. Person dibuat INSERT langsung dengan
// nik_search_hash hex SINTETIS — TANPA NIK plaintext (pola empi_ingest_test);
// MRN di-UPDATE langsung (UNIQUE per company). Token izin lewat mergeUserToken
// (pola person_merge_test.go); request lewat accountRequest.

func empiStrPtr(s string) *string { return &s }

// empiResolveSetup seeds company A (caller, via seedAccountOwner) dan company B,
// persons A1/B1 hash sama (auto-link ke satu master), MRN eksplisit per company,
// lalu ingest keduanya. Mengembalikan router + id yang relevan.
func empiResolveSetup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, idx string) (router *gin.Engine, companyA, companyB, merchantA, a1, b1, master string) {
	t.Helper()
	router = mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ = seedAccountOwner(t, ctx, pool, router, "empires."+label, "0812348"+idx)
	companyB = empiSeedCompany(t, ctx, pool, "eeeeeeee-b000-0000-0000-0000000000"+idx, "empi-res-"+label)

	a1 = empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 = empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", empiHashH1, "1990-04-12")
	if _, err := pool.Exec(ctx, "UPDATE core.person SET medical_record_no = $1 WHERE id = $2", "RM-A-"+label, a1); err != nil {
		t.Fatalf("set mrn a1: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.person SET medical_record_no = $1 WHERE id = $2", "RM-B-"+label, b1); err != nil {
		t.Fatalf("set mrn b1: %v", err)
	}
	empiIngest(t, ctx, pool, companyA, a1)
	empiIngest(t, ctx, pool, companyB, b1)

	if err := pool.QueryRow(ctx,
		"SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1", a1,
	).Scan(&master); err != nil {
		t.Fatalf("master a1: %v", err)
	}
	var linkCount int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM empi.person_link WHERE master_person_id = $1 AND status = 'active'", master,
	).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 2 {
		t.Fatalf("precondition: expected master with 2 active links, got %d", linkCount)
	}
	return router, companyA, companyB, merchantA, a1, b1, master
}

func empiResolveData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return resp.Data
}

func empiLinkByCompany(t *testing.T, data map[string]any, companyID string) map[string]any {
	t.Helper()
	rawLinks, ok := data["links"].([]any)
	if !ok {
		t.Fatalf("data.links missing: %v", data)
	}
	for _, raw := range rawLinks {
		link := raw.(map[string]any)
		if link["company_id"] == companyID {
			return link
		}
	}
	t.Fatalf("no link for company %s in %v", companyID, data)
	return nil
}

func TestEmpiResolve_ByPersonAndCrossCompanyLinks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, companyB, merchantA, a1, b1, master := empiResolveSetup(t, ctx, pool, "a1", "01")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empiview-a1", []string{server.PermEmpiMasterView})

	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/resolve?person_id="+a1, token, companyA, merchantA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := empiResolveData(t, rec)
	if data["master_person_id"] != master {
		t.Errorf("expected master %s, got %v", master, data["master_person_id"])
	}
	if data["merged_into_master_person_id"] != nil {
		t.Errorf("expected merged_into null, got %v", data["merged_into_master_person_id"])
	}
	if got, ok := data["candidate_pairs_pending"].(float64); !ok || got != 0 {
		t.Errorf("expected 0 pending candidate pairs, got %v", data["candidate_pairs_pending"])
	}

	// Dua link lintas company dengan mrn + company_name benar per company.
	linkA := empiLinkByCompany(t, data, companyA)
	if linkA["medical_record_no"] != "RM-A-a1" {
		t.Errorf("expected company A mrn RM-A-a1, got %v", linkA["medical_record_no"])
	}
	if linkA["full_name"] != "Siti Aminah" || linkA["status"] != "active" {
		t.Errorf("unexpected link A content: %v", linkA)
	}
	var companyNameA string
	if err := pool.QueryRow(ctx, "SELECT name FROM core.company WHERE id = $1", companyA).Scan(&companyNameA); err != nil {
		t.Fatalf("read company A name: %v", err)
	}
	if linkA["company_name"] != companyNameA {
		t.Errorf("expected company A name %q, got %v", companyNameA, linkA["company_name"])
	}

	// Link company B terlihat dari caller company A (fungsi SECURITY DEFINER
	// 000086) — inti bug ledger: JOIN company di bawah RLS dulu menghilangkannya.
	linkB := empiLinkByCompany(t, data, companyB)
	if linkB["medical_record_no"] != "RM-B-a1" {
		t.Errorf("expected company B mrn RM-B-a1, got %v", linkB["medical_record_no"])
	}
	if linkB["company_name"] != "EMPI empi-res-a1" {
		t.Errorf("expected company B name 'EMPI empi-res-a1', got %v", linkB["company_name"])
	}
	if linkB["person_id"] != b1 {
		t.Errorf("expected company B person %s, got %v", b1, linkB["person_id"])
	}
}

func TestEmpiResolve_PermissionDenied(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, _, merchantA, a1, _, _ := empiResolveSetup(t, ctx, pool, "p1", "02")
	// Token TANPA izin empi.master.view.
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empinoperm-p1", nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/resolve?person_id="+a1, token, companyA, merchantA, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEmpiResolve_PersonOfOtherCompany(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, _, merchantA, _, b1, _ := empiResolveSetup(t, ctx, pool, "o1", "03")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empiview-o1", []string{server.PermEmpiMasterView})

	// person milik company B: dari caller company A → 404 tanpa detail.
	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/resolve?person_id="+b1, token, companyA, merchantA, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v (%s)", err, rec.Body.String())
	}
	if resp.Error != "person not linked" {
		t.Errorf("expected error 'person not linked', got %q", resp.Error)
	}
}

func TestEmpiResolve_UnlinkedPerson(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, _, merchantA, _, _, _ := empiResolveSetup(t, ctx, pool, "u1", "04")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empiview-u1", []string{server.PermEmpiMasterView})

	// Person company A TANPA ingest → tidak punya link empi.
	unlinked := empiInsertPerson(t, ctx, pool, companyA, "Tanpa Link", "", "")
	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/resolve?person_id="+unlinked, token, companyA, merchantA, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEmpiMasterDetail_IncludesMergeState(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "empimd.m1", "081234801")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-c000-0000-0000-000000000001", "empi-md-m1")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empiview-m1", []string{server.PermEmpiMasterView})

	// Dua master di-seed langsung; m1 di-merge ke m2.
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah Lain", empiHashH2, "1991-05-13")
	masterM1 := "eeeeeeee-c000-0000-0000-0000000000aa"
	masterM2 := "eeeeeeee-c000-0000-0000-0000000000bb"
	empiSeedMaster(t, ctx, pool, masterM1, "Siti Aminah", empiStrPtr("1990-04-12"))
	empiSeedMaster(t, ctx, pool, masterM2, "Siti Aminah Lain", empiStrPtr("1991-05-13"))
	empiSeedLink(t, ctx, pool, "eeeeeeee-c000-0000-0000-0000000000a1", masterM1, companyA, a1, empiHashH1, "Siti Aminah", empiStrPtr("1990-04-12"))
	empiSeedLink(t, ctx, pool, "eeeeeeee-c000-0000-0000-0000000000b1", masterM2, companyB, b1, empiHashH2, "Siti Aminah Lain", empiStrPtr("1991-05-13"))
	if _, err := pool.Exec(ctx,
		"UPDATE empi.master_person SET merged_into_master_person_id = $1 WHERE id = $2", masterM2, masterM1); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/master-persons/"+masterM1, token, companyA, merchantA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("master detail expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := empiResolveData(t, rec)
	if data["merged_into_master_person_id"] != masterM2 {
		t.Errorf("expected merged_into %s, got %v", masterM2, data["merged_into_master_person_id"])
	}
	if data["master_person_id"] != masterM1 || data["full_name"] != "Siti Aminah" {
		t.Errorf("unexpected master fields: %v", data)
	}
	links := data["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	// Master tak dikenal → 404.
	rec = accountRequest(router, http.MethodGet,
		"/api/v1/empi/master-persons/99999999-9999-9999-9999-999999999999", token, companyA, merchantA, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown master expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// empiInsertCandidatePair seeds one empi.candidate_pair row directly and
// returns its id (constraint 000085: person_a_company_id <> person_b_company_id,
// UNIQUE (person_a_id, person_b_id), reason enum same_nik/birth_gender_name/phone_name).
func empiInsertCandidatePair(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	personA, companyA, personB, companyB string, score int32, reason string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		"INSERT INTO empi.candidate_pair (person_a_id, person_b_id, person_a_company_id, person_b_company_id, score, reason) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text",
		personA, personB, companyA, companyB, score, reason).Scan(&id); err != nil {
		t.Fatalf("seed candidate pair: %v", err)
	}
	return id
}

func TestEmpiCandidateList_PendingOnlyWithSides(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "empicp.l1", "081234820001")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-d000-0000-0000-000000000001", "empi-cp-l1")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empicpm-l1", []string{server.PermEmpiMasterManage})

	// Pasangan pending: a di company A (hash ada), b di company B (link TANPA
	// hash → has_nik false). Master + link di-seed agar kedua sisi ter-render.
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", empiHashH1, "1990-04-12")
	master := "eeeeeeee-d000-0000-0000-00000000d001"
	empiSeedMaster(t, ctx, pool, master, "Siti Aminah", empiStrPtr("1990-04-12"))
	empiSeedLink(t, ctx, pool, "eeeeeeee-d000-0000-0000-0000000001a1", master, companyA, a1, empiHashH1, "Siti Aminah", empiStrPtr("1990-04-12"))
	empiSeedLink(t, ctx, pool, "eeeeeeee-d000-0000-0000-0000000001b1", master, companyB, b1, "", "Siti Aminah", empiStrPtr("1990-04-12"))
	pairID := empiInsertCandidatePair(t, ctx, pool, a1, companyA, b1, companyB, 100, "same_nik")

	// Pasangan kedua di-dismiss via UPDATE langsung → tak boleh muncul.
	c2a := empiInsertPerson(t, ctx, pool, companyA, "Budi Santoso", empiHashH2, "1985-01-02")
	c2b := empiInsertPerson(t, ctx, pool, companyB, "Budi Santoso", empiHashH2, "1985-01-02")
	empiInsertCandidatePair(t, ctx, pool, c2a, companyA, c2b, companyB, 70, "birth_gender_name")
	if _, err := pool.Exec(ctx,
		"UPDATE empi.candidate_pair SET status = 'dismissed' WHERE person_a_id = $1 AND person_b_id = $2", c2a, c2b); err != nil {
		t.Fatalf("dismiss second pair: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/candidate-pairs", token, companyA, merchantA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected only the pending pair, got %d rows: %v", len(resp.Data), resp.Data)
	}
	item := resp.Data[0]
	if item["id"] != pairID || item["status"] != "pending" {
		t.Errorf("expected pending pair %s, got %v", pairID, item)
	}
	if got, ok := item["score"].(float64); !ok || got != 100 {
		t.Errorf("expected score 100, got %v", item["score"])
	}
	if item["reason"] != "same_nik" {
		t.Errorf("expected reason same_nik, got %v", item["reason"])
	}
	if item["note"] != nil {
		t.Errorf("expected note null, got %v", item["note"])
	}
	var nameA, nameB string
	if err := pool.QueryRow(ctx, "SELECT name FROM core.company WHERE id = $1", companyA).Scan(&nameA); err != nil {
		t.Fatalf("read company A name: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT name FROM core.company WHERE id = $1", companyB).Scan(&nameB); err != nil {
		t.Fatalf("read company B name: %v", err)
	}
	sideA := item["a"].(map[string]any)
	if sideA["company_id"] != companyA || sideA["company_name"] != nameA || sideA["person_id"] != a1 {
		t.Errorf("side A mismatch: %v", sideA)
	}
	if sideA["full_name"] != "Siti Aminah" || sideA["birth_date"] != "1990-04-12" || sideA["gender"] != "female" {
		t.Errorf("side A identity mismatch: %v", sideA)
	}
	if sideA["has_nik"] != true || sideA["master_person_id"] != master {
		t.Errorf("side A has_nik/master mismatch: %v", sideA)
	}
	sideB := item["b"].(map[string]any)
	if sideB["company_id"] != companyB || sideB["company_name"] != nameB || sideB["person_id"] != b1 {
		t.Errorf("side B mismatch: %v", sideB)
	}
	// Link B di-seed tanpa nik_search_hash → has_nik false.
	if sideB["has_nik"] != false || sideB["master_person_id"] != master {
		t.Errorf("side B has_nik/master mismatch: %v", sideB)
	}
}

func TestEmpiCandidateDismiss_SetsStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "empicp.d1", "081234820002")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-d000-0000-0000-000000000002", "empi-cp-d1")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empicpm-d1", []string{server.PermEmpiMasterManage})
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", empiHashH1, "1990-04-12")
	pairID := empiInsertCandidatePair(t, ctx, pool, a1, companyA, b1, companyB, 100, "same_nik")

	rec := accountRequest(router, http.MethodPost,
		"/api/v1/empi/candidate-pairs/"+pairID+"/dismiss", token, companyA, merchantA,
		[]byte(`{"note":"bukan pasien sama"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if resp.Data["id"] != pairID || resp.Data["status"] != "dismissed" {
		t.Errorf("expected dismissed pair %s, got %v", pairID, resp.Data)
	}

	// decided_by/decided_at terisi + note tersimpan.
	var note string
	var decidedBy pgtype.UUID
	var decidedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx,
		"SELECT note, decided_by, decided_at FROM empi.candidate_pair WHERE id = $1", pairID,
	).Scan(&note, &decidedBy, &decidedAt); err != nil {
		t.Fatalf("read dismissed pair: %v", err)
	}
	if note != "bukan pasien sama" || !decidedBy.Valid || !decidedAt.Valid {
		t.Errorf("expected note + decided_by/decided_at filled, got %q %v %v", note, decidedBy, decidedAt)
	}

	// Dismiss ulang → 409 (bukan pending lagi).
	rec = accountRequest(router, http.MethodPost,
		"/api/v1/empi/candidate-pairs/"+pairID+"/dismiss", token, companyA, merchantA, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-dismiss expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v (%s)", err, rec.Body.String())
	}
	if errResp.Error != "candidate pair is not pending" {
		t.Errorf("expected error 'candidate pair is not pending', got %q", errResp.Error)
	}
}

func TestEmpiCandidateEndpoints_RequireManagePermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "empicp.p1", "081234820003")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-d000-0000-0000-000000000003", "empi-cp-p1")
	// Token hanya empi.master.view — tanpa empi.master.manage.
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empicpv-p1", []string{server.PermEmpiMasterView})
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", empiHashH1, "1990-04-12")
	pairID := empiInsertCandidatePair(t, ctx, pool, a1, companyA, b1, companyB, 100, "same_nik")

	rec := accountRequest(router, http.MethodGet, "/api/v1/empi/candidate-pairs", token, companyA, merchantA, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("list with view-only token expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodPost,
		"/api/v1/empi/candidate-pairs/"+pairID+"/dismiss", token, companyA, merchantA, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("dismiss with view-only token expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}
