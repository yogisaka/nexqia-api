//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
