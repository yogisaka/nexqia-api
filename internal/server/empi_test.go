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

// empiMergeSetup seeds two persons on two SEPARATE masters (hash berbeda,
// nama/tanggal lahir sama — tidak auto-link), satu candidate_pair pending,
// dan token empi.master.manage. Mengembalikan id master per sisi.
func empiMergeSetup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, idx string) (
	router *gin.Engine, companyA, companyB, merchantA, token, a1, b1, masterA, masterB, pairID string) {
	t.Helper()
	router = mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ = seedAccountOwner(t, ctx, pool, router, "empimge."+label, "08123483"+idx)
	companyB = empiSeedCompany(t, ctx, pool, "eeeeeeee-e000-0000-0000-0000000000"+idx, "empi-mge-"+label)
	token = mergeUserToken(t, ctx, pool, companyA, merchantA, "empimgem-"+label, []string{server.PermEmpiMasterManage})

	// Hash unik per setup (hex sintetis 32 char): dua setup dalam satu test
	// berbagi satu DB — hash statis membuat ingest auto-link lintas setup.
	hashA := "aa11bb22cc33dd44ee55ff6677" + idx + "0000"
	hashB := "ff00112233445566778899aabb" + idx + "0000"
	a1 = empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", hashA, "1990-04-12")
	b1 = empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", hashB, "1990-04-12")
	empiIngest(t, ctx, pool, companyA, a1)
	empiIngest(t, ctx, pool, companyB, b1)
	masterA = empiMasterOfPerson(t, ctx, pool, a1)
	masterB = empiMasterOfPerson(t, ctx, pool, b1)
	if masterA == masterB {
		t.Fatalf("precondition: expected separate masters, got %s for both", masterA)
	}
	pairID = empiEnsureCandidatePair(t, ctx, pool, a1, companyA, b1, companyB, 100, "same_nik")
	return router, companyA, companyB, merchantA, token, a1, b1, masterA, masterB, pairID
}

func empiMasterOfPerson(t *testing.T, ctx context.Context, pool *pgxpool.Pool, personID string) string {
	t.Helper()
	var master string
	if err := pool.QueryRow(ctx,
		"SELECT master_person_id::text FROM empi.person_link WHERE person_id = $1 AND status = 'active'", personID,
	).Scan(&master); err != nil {
		t.Fatalf("master of person %s: %v", personID, err)
	}
	return master
}

func empiMasterField(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query, masterID string) any {
	t.Helper()
	var v any
	if err := pool.QueryRow(ctx, query, masterID).Scan(&v); err != nil {
		t.Fatalf("master field query %q: %v", query, err)
	}
	return v
}

// empiConfirmMerge POSTs the confirm-link endpoint and asserts success.
func empiConfirmMerge(t *testing.T, router *gin.Engine, token, companyA, merchantA, pairID, winner string) {
	t.Helper()
	rec := accountRequest(router, http.MethodPost,
		"/api/v1/empi/candidate-pairs/"+pairID+"/link", token, companyA, merchantA,
		[]byte(`{"winner_master_person_id":"`+winner+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm link expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if resp.Data["id"] != pairID || resp.Data["status"] != "linked" {
		t.Errorf("expected linked pair %s, got %v", pairID, resp.Data)
	}
}

// empiMergeLogID returns the single merge log id for a winner/loser pair.
func empiMergeLogID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, winner, loser string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		"SELECT id::text FROM empi.merge_log WHERE winner_master_person_id = $1 AND loser_master_person_id = $2",
		winner, loser).Scan(&id); err != nil {
		t.Fatalf("merge log %s->%s: %v", winner, loser, err)
	}
	return id
}

// empiUnmerge POSTs the unmerge endpoint and returns the status code.
func empiUnmerge(t *testing.T, router *gin.Engine, token, companyA, merchantA, logID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return accountRequest(router, http.MethodPost,
		"/api/v1/empi/merge-logs/"+logID+"/unmerge", token, companyA, merchantA, []byte(body))
}

// empiEnsureCandidatePair seeds one empi.candidate_pair row, reusing the pair
// already created by ingest detection when present (UNIQUE person_a/person_b).
func empiEnsureCandidatePair(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	personA, companyA, personB, companyB string, score int32, reason string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		"INSERT INTO empi.candidate_pair (person_a_id, person_b_id, person_a_company_id, person_b_company_id, score, reason) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (person_a_id, person_b_id) DO UPDATE SET score = EXCLUDED.score RETURNING id::text",
		personA, personB, companyA, companyB, score, reason).Scan(&id); err != nil {
		t.Fatalf("seed candidate pair: %v", err)
	}
	return id
}

func TestEmpiCandidateConfirm_MergesMasters(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, _, merchantA, token, _, b1, masterA, masterB, pairID := empiMergeSetup(t, ctx, pool, "m1", "01")

	empiConfirmMerge(t, router, token, companyA, merchantA, pairID, masterA)

	// Semua link aktif menunjuk winner; loser merged_into terisi.
	if got := empiMasterOfPerson(t, ctx, pool, b1); got != masterA {
		t.Errorf("expected loser link moved to %s, got %s", masterA, got)
	}
	if got := empiMasterField(t, ctx, pool,
		"SELECT merged_into_master_person_id::text FROM empi.master_person WHERE id = $1", masterB); got != masterA {
		t.Errorf("expected loser merged_into %s, got %v", masterA, got)
	}

	// Merge log 1 baris: snapshot berisi kedua link lama + loser master.
	if n := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.merge_log"); n != 1 {
		t.Fatalf("expected 1 merge log, got %d", n)
	}
	var snapRaw []byte
	if err := pool.QueryRow(ctx, "SELECT snapshot FROM empi.merge_log").Scan(&snapRaw); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap struct {
		LoserMaster struct {
			ID string `json:"id"`
		} `json:"loser_master"`
		Links []struct {
			LinkID         string `json:"link_id"`
			MasterPersonID string `json:"master_person_id"`
		} `json:"links"`
	}
	if err := json.Unmarshal(snapRaw, &snap); err != nil {
		t.Fatalf("parse snapshot: %v (%s)", err, snapRaw)
	}
	if snap.LoserMaster.ID != masterB {
		t.Errorf("expected snapshot loser %s, got %s", masterB, snap.LoserMaster.ID)
	}
	if len(snap.Links) != 1 || snap.Links[0].MasterPersonID != masterB {
		t.Errorf("expected snapshot with 1 old link of %s, got %+v", masterB, snap.Links)
	}

	// Pair linked + golden winner recompute dari link aktif (a1 sisi pertama).
	var status string
	var decidedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx,
		"SELECT status, decided_at FROM empi.candidate_pair WHERE id = $1", pairID,
	).Scan(&status, &decidedAt); err != nil {
		t.Fatalf("read pair: %v", err)
	}
	if status != "linked" || !decidedAt.Valid {
		t.Errorf("expected linked pair with decided_at, got %s %v", status, decidedAt)
	}
	if got := empiMasterField(t, ctx, pool,
		"SELECT full_name FROM empi.master_person WHERE id = $1", masterA); got != "Siti Aminah" {
		t.Errorf("expected winner golden 'Siti Aminah', got %v", got)
	}
}

func TestEmpiCandidateConfirm_SameMasterJustLinks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "empimge.s1", "08123483002")
	companyB := empiSeedCompany(t, ctx, pool, "eeeeeeee-e000-0000-0000-000000000002", "empi-mge-s1")
	token := mergeUserToken(t, ctx, pool, companyA, merchantA, "empimgem-s1", []string{server.PermEmpiMasterManage})

	// Hash sama → ingest auto-link ke SATU master.
	a1 := empiInsertPerson(t, ctx, pool, companyA, "Siti Aminah", empiHashH1, "1990-04-12")
	b1 := empiInsertPerson(t, ctx, pool, companyB, "Siti Aminah", empiHashH1, "1990-04-12")
	empiIngest(t, ctx, pool, companyA, a1)
	empiIngest(t, ctx, pool, companyB, b1)
	master := empiMasterOfPerson(t, ctx, pool, a1)
	if empiMasterOfPerson(t, ctx, pool, b1) != master {
		t.Fatalf("precondition: expected shared master %s", master)
	}
	pairID := empiEnsureCandidatePair(t, ctx, pool, a1, companyA, b1, companyB, 100, "same_nik")

	empiConfirmMerge(t, router, token, companyA, merchantA, pairID, master)
	if n := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.merge_log"); n != 0 {
		t.Errorf("expected no merge log for same-master confirm, got %d", n)
	}
}

func TestEmpiCandidateConfirm_WinnerNotSideMaster(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, _, merchantA, token, _, _, _, _, pairID := empiMergeSetup(t, ctx, pool, "w1", "03")

	rec := accountRequest(router, http.MethodPost,
		"/api/v1/empi/candidate-pairs/"+pairID+"/link", token, companyA, merchantA,
		[]byte(`{"winner_master_person_id":"99999999-9999-9999-9999-999999999999"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v (%s)", err, rec.Body.String())
	}
	if resp.Error != "winner must be one of the pair masters" {
		t.Errorf("unexpected error %q", resp.Error)
	}
	if n := empiCount(t, ctx, pool, "SELECT count(*) FROM empi.merge_log"); n != 0 {
		t.Errorf("expected no merge log after rejected winner, got %d", n)
	}
}

// empiUnmergeSetup merges two seeded masters (winner = sisi a) dan menandai
// master loser dengan golden + manual_fields khusus SEBELUM merge supaya
// restore-nya teramati. Mengembalikan log id merge.
func empiUnmergeSetup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, idx string) (
	router *gin.Engine, companyA, merchantA, token, b1, masterA, masterB, logID string) {
	t.Helper()
	router, companyA, _, merchantA, token, _, b1, masterA, masterB, pairID := empiMergeSetup(t, ctx, pool, label, idx)
	// Golden loser khusus + provenance tiga sumber terpisah + manual_fields.
	if _, err := pool.Exec(ctx,
		"UPDATE empi.master_person SET full_name = 'Nama Loser', birth_date = '1981-02-03', gender = 'male', full_name_source_person_id = $1, birth_date_source_person_id = $1, gender_source_person_id = $1, manual_fields = ARRAY['gender'] WHERE id = $2",
		b1, masterB); err != nil {
		t.Fatalf("set loser golden: %v", err)
	}
	empiConfirmMerge(t, router, token, companyA, merchantA, pairID, masterA)
	return router, companyA, merchantA, token, b1, masterA, masterB, empiMergeLogID(t, ctx, pool, masterA, masterB)
}

func TestEmpiUnmerge_RestoresSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, merchantA, token, b1, masterA, masterB, logID := empiUnmergeSetup(t, ctx, pool, "u2", "04")

	rec := empiUnmerge(t, router, token, companyA, merchantA, logID, `{"reason":"salah merge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if resp.Data["id"] != logID {
		t.Errorf("expected log id %s, got %v", logID, resp.Data)
	}
	if _, ok := resp.Data["unmerged_at"].(string); !ok || resp.Data["unmerged_at"] == "" {
		t.Errorf("expected RFC3339 unmerged_at, got %v", resp.Data["unmerged_at"])
	}

	// Link kembali ke master loser; merged_into NULL.
	if got := empiMasterOfPerson(t, ctx, pool, b1); got != masterB {
		t.Errorf("expected link restored to %s, got %s", masterB, got)
	}
	if got := empiMasterField(t, ctx, pool,
		"SELECT merged_into_master_person_id::text FROM empi.master_person WHERE id = $1", masterB); got != nil {
		t.Errorf("expected loser merged_into NULL, got %v", got)
	}

	// Golden loser + tiga provenance terpisah + manual_fields dipulihkan.
	if got := empiMasterField(t, ctx, pool,
		"SELECT full_name FROM empi.master_person WHERE id = $1", masterB); got != "Nama Loser" {
		t.Errorf("expected restored full_name 'Nama Loser', got %v", got)
	}
	var fullNameSrc, birthSrc, genderSrc string
	var manualFields []string
	if err := pool.QueryRow(ctx,
		"SELECT full_name_source_person_id::text, birth_date_source_person_id::text, gender_source_person_id::text, manual_fields FROM empi.master_person WHERE id = $1",
		masterB).Scan(&fullNameSrc, &birthSrc, &genderSrc, &manualFields); err != nil {
		t.Fatalf("read restored master: %v", err)
	}
	if fullNameSrc != b1 || birthSrc != b1 || genderSrc != b1 {
		t.Errorf("expected provenance all %s, got %s/%s/%s", b1, fullNameSrc, birthSrc, genderSrc)
	}
	if len(manualFields) != 1 || manualFields[0] != "gender" {
		t.Errorf("expected manual_fields ['gender'], got %v", manualFields)
	}

	// Golden winner dihitung ulang dari link aktif tersisa (a1).
	if got := empiMasterField(t, ctx, pool,
		"SELECT full_name FROM empi.master_person WHERE id = $1", masterA); got != "Siti Aminah" {
		t.Errorf("expected winner golden recomputed to 'Siti Aminah', got %v", got)
	}

	// Log ditandai unmerged.
	if got := empiMasterField(t, ctx, pool,
		"SELECT unmerged_at::text FROM empi.merge_log WHERE id = $1", logID); got == nil {
		t.Errorf("expected merge log unmerged_at filled, got nil")
	}
}

func TestEmpiUnmerge_TwiceConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, merchantA, token, _, _, _, logID := empiUnmergeSetup(t, ctx, pool, "u3", "05")

	rec := empiUnmerge(t, router, token, companyA, merchantA, logID, `{"reason":"ulang"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = empiUnmerge(t, router, token, companyA, merchantA, logID, `{"reason":"ulang lagi"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second unmerge expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v (%s)", err, rec.Body.String())
	}
	if resp.Error != "merge log already unmerged" {
		t.Errorf("unexpected error %q", resp.Error)
	}
}

func TestEmpiMergeLogs_List(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router, companyA, merchantA, token, _, masterA1, masterB1, logOld := empiUnmergeSetup(t, ctx, pool, "l1", "06")
	// Merge kedua (setup terpisah, commit lebih baru).
	router2, companyA2, merchantA2, token2, _, masterA2, masterB2, logNew := empiUnmergeSetup(t, ctx, pool, "l2", "07")
	_ = router2

	// Unmerge merge TERBARU → baris pertama list (urut merged_at DESC) unmerged.
	rec := empiUnmerge(t, router2, token2, companyA2, merchantA2, logNew, `{"reason":"batal"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/empi/merge-logs", token, companyA, merchantA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 merge logs, got %d: %v", len(resp.Data), resp.Data)
	}
	first, second := resp.Data[0], resp.Data[1]
	if first["id"] != logNew || second["id"] != logOld {
		t.Errorf("expected newest (%s) first then %s, got %v / %v", logNew, logOld, first["id"], second["id"])
	}
	if first["unmerged_at"] == nil || first["unmerged"] != true {
		t.Errorf("expected first log unmerged, got %v", first)
	}
	if second["unmerged_at"] != nil || second["unmerged"] != false {
		t.Errorf("expected second log still merged, got %v", second)
	}
	if first["winner_master_person_id"] != masterA2 || first["loser_master_person_id"] != masterB2 {
		t.Errorf("first log winner/loser mismatch: %v", first)
	}
	if second["winner_master_person_id"] != masterA1 || second["loser_master_person_id"] != masterB1 {
		t.Errorf("second log winner/loser mismatch: %v", second)
	}
	if _, hasSnapshot := first["snapshot"]; hasSnapshot {
		t.Errorf("list must not include snapshot payload: %v", first)
	}
}
