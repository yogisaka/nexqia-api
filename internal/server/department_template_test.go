//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestDepartmentTemplate_DB_SeedComplete — migration 000054 seeds the six
// platform department templates with the expected poli counts and the default
// BPJS VClaim code map (138 = 144 - 6; klinik_pratama codes are internal-only).
func TestDepartmentTemplate_DB_SeedComplete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	// Per-template poli counts from spec §3: 6 + 14 + 33 + 62 + 7 + 22 = 144.
	want := map[string]int{
		"klinik_pratama":  6,
		"klinik_utama":    14,
		"rumah_sakit":     33,
		"rs_subspesialis": 62,
		"layanan_program": 7,
		"unit_penunjang":  22,
	}
	var total int
	for code, w := range want {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM core.template_department td
			JOIN core.template t ON t.id = td.template_id
			WHERE t.code = $1 AND t.scope = 'platform' AND t.kind = 'department'`, code).Scan(&n)
		if err != nil {
			t.Fatalf("%s: count template_department: %v", code, err)
		}
		if n != w {
			t.Fatalf("%s: expected %d departments, got %d", code, w, n)
		}
		total += n
	}
	if total != 144 {
		t.Fatalf("expected 144 departments across templates (6+14+33+62+7+22), got %d", total)
	}

	var maps int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_department_code_map m
		JOIN core.template_department td ON td.id = m.template_department_id
		JOIN core.template t ON t.id = td.template_id
		WHERE m.system = 'bpjs-vclaim' AND t.scope = 'platform' AND t.kind = 'department'`).Scan(&maps); err != nil {
		t.Fatalf("count bpjs-vclaim mappings: %v", err)
	}
	// Every department except klinik_pratama's 6 internal-only polis gets a
	// bpjs-vclaim mapping: 144 - 6 = 138.
	if maps != 138 {
		t.Fatalf("expected 138 bpjs-vclaim mappings, got %d", maps)
	}

	var specialties int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_department td
		JOIN core.template t ON t.id = td.template_id
		WHERE t.scope = 'platform' AND t.kind = 'department' AND td.specialty_code IS NOT NULL`).Scan(&specialties); err != nil {
		t.Fatalf("count specialty codes: %v", err)
	}
	// klinik_utama (SPEC-DALAM/ANAK/OBGYN/BEDAH) + rumah_sakit (same four) = 8.
	if specialties != 8 {
		t.Fatalf("expected 8 specialty-coded departments, got %d", specialties)
	}
}

// TestDepartmentTemplate_DB_RuntimeCannotWriteTemplates — app_runtime can read
// the seeded template departments but INSERT into template tables is revoked
// (migration 000054 REVOKE).
func TestDepartmentTemplate_DB_RuntimeCannotWriteTemplates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "DTW")

	denied := []struct {
		name string
		sql  string
	}{
		{"insert template_department", "INSERT INTO core.template_department (template_id, code, name, sort_order) SELECT id, 'X', 'X', 99 FROM core.template WHERE code = 'klinik_utama' AND kind = 'department'"},
		{"insert template_department_code_map", "INSERT INTO core.template_department_code_map (template_department_id, system, code) SELECT id, 'bpjs-vclaim', 'X' FROM core.template_department LIMIT 1"},
	}
	for _, d := range denied {
		tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
		_, err := tx.Exec(ctx, d.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: expected permission denied (42501), got %v", d.name, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback %s: %v", d.name, err)
		}
	}

	// Reads still work: all 144 seeded rows are visible under the visibility policy.
	tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template_department").Scan(&visible); err != nil {
		t.Fatalf("count template_department as app_runtime: %v", err)
	}
	if visible != 144 {
		t.Fatalf("expected 144 template departments visible to app_runtime, got %d", visible)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback read tx: %v", err)
	}
}

// TestDepartmentTemplate_DB_CodeMapTenantFilled — the BEFORE INSERT trigger
// trg_00_fill_tenant fills company_id/merchant_id on core.department_code_map
// from the referenced department, so the caller never sends tenant columns.
func TestDepartmentTemplate_DB_CodeMapTenantFilled(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "DTF")

	var companyID, merchantID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO core.department_code_map (department_id, system, code, display)
		VALUES ($1, 'bpjs-vclaim', 'UMU', 'Poli Umum')
		RETURNING company_id::text, merchant_id::text`,
		tn.departmentID).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert code map without tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("trigger fill: expected company %s merchant %s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}

	// UNIQUE (department_id, system) — a second mapping for the same system is rejected.
	_, err := pool.Exec(ctx, `
		INSERT INTO core.department_code_map (department_id, system, code)
		VALUES ($1, 'bpjs-vclaim', 'UMU-DUP')`, tn.departmentID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate (department_id, system): expected unique violation (23505), got %v", err)
	}
}

// TestDepartmentTemplate_DB_CodeMapMerchantIsolation — the RLS policy
// department_code_map_isolation exposes rows only to the runtime whose
// app.current_merchant_id GUC matches merchant_id.
func TestDepartmentTemplate_DB_CodeMapMerchantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "DTA")
	tnB := seedChildRLSTenant(t, ctx, pool, "DTB")

	for _, tn := range []childRLSTenant{tnA, tnB} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO core.department_code_map (department_id, system, code)
			VALUES ($1, 'bpjs-vclaim', 'UMU')`, tn.departmentID); err != nil {
			t.Fatalf("seed code map: %v", err)
		}
	}

	for _, tn := range []childRLSTenant{tnA, tnB} {
		tx := childRLSAppRuntimeTx(t, ctx, pool, tn.companyID, tn.merchantID)
		var own, other int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.department_code_map").Scan(&own); err != nil {
			t.Fatalf("count own code map rows: %v", err)
		}
		if own != 1 {
			t.Fatalf("expected 1 visible code map row for own merchant, got %d", own)
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.department_code_map
			WHERE merchant_id = current_setting('app.current_merchant_id')::uuid
			  AND department_id <> $1`, tn.departmentID).Scan(&other); err != nil {
			t.Fatalf("count foreign code map rows: %v", err)
		}
		if other != 0 {
			t.Fatalf("expected 0 foreign rows visible under own GUC, got %d", other)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}

type departmentCodeMapView struct {
	System string `json:"system"`
	Code   string `json:"code"`
}

type departmentTemplateDepartmentView struct {
	ID            string                  `json:"id"`
	Code          string                  `json:"code"`
	Name          string                  `json:"name"`
	SpecialtyCode *string                 `json:"specialty_code"`
	CodeMaps      []departmentCodeMapView `json:"code_maps"`
	Status        string                  `json:"status"`
	SimilarTo     []string                `json:"similar_to"`
}

type departmentTemplateView struct {
	ID          string                             `json:"id"`
	Code        string                             `json:"code"`
	Version     int                                `json:"version"`
	Departments []departmentTemplateDepartmentView `json:"departments"`
}

func listDepartmentTemplatesAPI(t *testing.T, router *gin.Engine, token, companyID, merchantID string) map[string]departmentTemplateView {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/department-templates", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /department-templates expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []departmentTemplateView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode department templates: %v", err)
	}
	byCode := map[string]departmentTemplateView{}
	for _, tpl := range resp.Data {
		byCode[tpl.Code] = tpl
	}
	return byCode
}

func templateDepartmentByCode(t *testing.T, tpl departmentTemplateView, code string) departmentTemplateDepartmentView {
	t.Helper()
	for _, d := range tpl.Departments {
		if d.Code == code {
			return d
		}
	}
	t.Fatalf("template %s has no department %q", tpl.Code, code)
	return departmentTemplateDepartmentView{}
}

func templateDepartmentIDs(tpl departmentTemplateView) []string {
	ids := make([]string, 0, len(tpl.Departments))
	for _, d := range tpl.Departments {
		ids = append(ids, d.ID)
	}
	return ids
}

func applyDepartmentTemplateAPI(t *testing.T, router *gin.Engine, token, companyID, merchantID, templateID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal apply body: %v", err)
	}
	return accountRequest(router, http.MethodPost, "/api/v1/department-templates/"+templateID+"/apply", token, companyID, merchantID, raw)
}

func decodeDepartmentApply(t *testing.T, rec *httptest.ResponseRecorder) (created, skipped []map[string]string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Created []struct {
				DepartmentID string `json:"department_id"`
				Code         string `json:"code"`
				Name         string `json:"name"`
			} `json:"created"`
			Skipped []struct {
				Code   string `json:"code"`
				Name   string `json:"name"`
				Reason string `json:"reason"`
			} `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	for _, cr := range resp.Data.Created {
		created = append(created, map[string]string{"department_id": cr.DepartmentID, "code": cr.Code, "name": cr.Name})
	}
	for _, sk := range resp.Data.Skipped {
		skipped = append(skipped, map[string]string{"code": sk.Code, "name": sk.Name, "reason": sk.Reason})
	}
	return created, skipped
}

// seedStatusDepartments seeds departments that exercise every status: same code
// (UMU vs Poli Umum), same normalized name with a different code (poli  gigi vs
// Poli Gigi/X99), a deleted same-code poli (KIA), and a similar active name
// (Poli Gizi Dewasa ⊃ Poli Gizi).
func seedStatusDepartments(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID string) {
	t.Helper()
	for _, d := range []struct{ code, name string }{
		{"UMU", "Klinik Umum Sejahtera"},
		{"X99", "poli  gigi"},
		{"GZ1", "Poli Gizi Dewasa"},
	} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $4)",
			companyID, merchantID, d.code, d.name); err != nil {
			t.Fatalf("seed department %s: %v", d.code, err)
		}
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name, deleted_at, deleted_by) VALUES ($1, $2, 'KIA', 'Poli Anakku', now(), $3)",
		companyID, merchantID, userID); err != nil {
		t.Fatalf("seed deleted department: %v", err)
	}
}

// TestDepartmentTemplate_ListShowsStatus — statuses against the merchant's
// departments: exists via code, exists via normalized name, deleted_exists
// (soft-deleted same code), and new with similar_to.
func TestDepartmentTemplate_ListShowsStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.list", "081234570921")
	seedStatusDepartments(t, ctx, pool, companyID, merchantID, userID)

	pratama := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]
	if d := templateDepartmentByCode(t, pratama, "UMU"); d.Status != "exists" {
		t.Fatalf("UMU: expected exists (code match), got %q", d.Status)
	}
	if d := templateDepartmentByCode(t, pratama, "GIG"); d.Status != "exists" {
		t.Fatalf("GIG: expected exists (normalized name match), got %q", d.Status)
	}
	if d := templateDepartmentByCode(t, pratama, "KIA"); d.Status != "deleted_exists" {
		t.Fatalf("KIA: expected deleted_exists, got %q", d.Status)
	}
	giz := templateDepartmentByCode(t, pratama, "GIZ")
	if giz.Status != "new" || len(giz.SimilarTo) != 1 || giz.SimilarTo[0] != "Poli Gizi Dewasa" {
		t.Fatalf("GIZ: expected new + similar_to [Poli Gizi Dewasa], got %q %v", giz.Status, giz.SimilarTo)
	}
	// klinik_pratama departments carry no code maps (internal-only codes).
	if d := templateDepartmentByCode(t, pratama, "UMU"); len(d.CodeMaps) != 0 {
		t.Fatalf("UMU: expected no code maps in klinik_pratama, got %v", d.CodeMaps)
	}
	utama := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)["klinik_utama"]
	if d := templateDepartmentByCode(t, utama, "ANA"); len(d.CodeMaps) != 1 || d.CodeMaps[0].System != "bpjs-vclaim" || d.CodeMaps[0].Code != "ANA" {
		t.Fatalf("ANA: expected bpjs-vclaim/ANA code map, got %v", d.CodeMaps)
	}
	if d := templateDepartmentByCode(t, utama, "INT"); d.SpecialtyCode == nil || *d.SpecialtyCode != "SPEC-DALAM" {
		t.Fatalf("INT: expected specialty_code SPEC-DALAM, got %v", d.SpecialtyCode)
	}
}

// TestDepartmentTemplate_ApplyCreatesWithMappings — applying klinik_utama
// creates 14 departments, copies the bpjs-vclaim mappings, fills the
// specialty concept only when the concept code exists (SPEC-ANAK seeded,
// SPEC-DALAM not), and records the application.
func TestDepartmentTemplate_ApplyCreatesWithMappings(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.apply", "081234570922")
	var systemID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test:depttpl', 'Test', '1') RETURNING id::text").Scan(&systemID); err != nil {
		t.Fatalf("seed code system: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, 'SPEC-ANAK', 'Spesialis Anak')", systemID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}

	utama := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)["klinik_utama"]
	created, skipped := decodeDepartmentApply(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, utama.ID, map[string]any{"department_ids": templateDepartmentIDs(utama)}))
	if len(created) != 14 || len(skipped) != 0 {
		t.Fatalf("expected 14 created / 0 skipped, got %d / %d", len(created), len(skipped))
	}

	var anakSpecialty, dalamSpecialty *string
	if err := pool.QueryRow(ctx, `
		SELECT specialty_concept_id::text FROM core.department WHERE merchant_id = $1 AND code = 'ANA'`, merchantID).Scan(&anakSpecialty); err != nil {
		t.Fatalf("read ANA department: %v", err)
	}
	var conceptID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM terminology.concept WHERE code = 'SPEC-ANAK'`).Scan(&conceptID); err != nil {
		t.Fatalf("read concept: %v", err)
	}
	if anakSpecialty == nil || *anakSpecialty != conceptID {
		t.Fatalf("ANA: expected specialty_concept_id %s, got %v", conceptID, anakSpecialty)
	}
	if err := pool.QueryRow(ctx, `
		SELECT specialty_concept_id::text FROM core.department WHERE merchant_id = $1 AND code = 'INT'`, merchantID).Scan(&dalamSpecialty); err != nil {
		t.Fatalf("read INT department: %v", err)
	}
	if dalamSpecialty != nil {
		t.Fatalf("INT: expected NULL specialty (SPEC-DALAM has no concept), got %s", *dalamSpecialty)
	}
	var maps int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.department_code_map WHERE merchant_id = $1 AND system = 'bpjs-vclaim'`, merchantID).Scan(&maps); err != nil {
		t.Fatalf("count code maps: %v", err)
	}
	// Every klinik_utama department has a bpjs-vclaim mapping: 14.
	if maps != 14 {
		t.Fatalf("expected 14 bpjs-vclaim mappings, got %d", maps)
	}

	var createdNames []string
	if err := pool.QueryRow(ctx,
		"SELECT roles_created FROM core.template_application WHERE company_id = $1", companyID).Scan(&createdNames); err != nil {
		t.Fatalf("read template_application: %v", err)
	}
	// The history columns are named roles_created/roles_skipped but carry the
	// department names.
	if len(createdNames) != 14 {
		t.Fatalf("expected 14 recorded department names, got %d", len(createdNames))
	}
}

// TestDepartmentTemplate_ApplyTwiceSkipsAll — a second apply skips everything;
// soft-deleted polis with the same code skip too instead of 500-ing.
func TestDepartmentTemplate_ApplyTwiceSkipsAll(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.twice", "081234570923")
	// A deleted same-code poli must not make the apply fail: the savepoint
	// rolls back and the department is reported skipped.
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name, deleted_at, deleted_by) VALUES ($1, $2, 'GIG', 'Poli Gigi Lama', now(), $3)",
		companyID, merchantID, userID); err != nil {
		t.Fatalf("seed deleted department: %v", err)
	}
	pratama := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]
	body := map[string]any{"department_ids": templateDepartmentIDs(pratama)}

	created, skipped := decodeDepartmentApply(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, body))
	// GIG matches the deleted same-code poli → skipped, never a 500.
	if len(created) != 5 || len(skipped) != 1 || skipped[0]["code"] != "GIG" || skipped[0]["reason"] != "deleted_exists" {
		t.Fatalf("first apply: expected 5 created / GIG deleted_exists, got %d / %v", len(created), skipped)
	}
	created, skipped = decodeDepartmentApply(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, body))
	if len(created) != 0 || len(skipped) != 6 {
		t.Fatalf("second apply: expected 0 created / 6 skipped, got %d / %d", len(created), len(skipped))
	}
	for _, s := range skipped {
		if s["reason"] != "exists" && s["reason"] != "deleted_exists" {
			t.Fatalf("second apply: %s skipped with %q, want exists or deleted_exists", s["name"], s["reason"])
		}
	}
	var applications int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.template_application WHERE company_id = $1", companyID).Scan(&applications); err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applications != 2 {
		t.Fatalf("expected 2 template_application rows, got %d", applications)
	}
}

// TestDepartmentTemplate_RejectsBadInput — exact 400/404 messages.
func TestDepartmentTemplate_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.bad", "081234570924")
	tpls := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)
	pratama, utama := tpls["klinik_pratama"], tpls["klinik_utama"]
	firstID := pratama.Departments[0].ID

	assertError(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"department_ids": []string{utama.Departments[0].ID}}),
		http.StatusBadRequest, "department_ids must belong to the template")
	assertError(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"department_ids": []string{}}),
		http.StatusBadRequest, "department_ids is required")
	assertError(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"department_ids": []string{firstID, firstID}}),
		http.StatusBadRequest, "department_ids must not repeat")
	assertError(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, "0190f1a2-0000-7000-8000-000000000000", map[string]any{"department_ids": []string{firstID}}),
		http.StatusNotFound, "template not found")
	assertError(t, applyDepartmentTemplateAPI(t, router, token, companyID, merchantID, "abc", map[string]any{"department_ids": []string{firstID}}),
		http.StatusBadRequest, "invalid id")
}

// TestDepartmentTemplate_RequiresPermission — templates and code-maps endpoints
// all need core.department.manage.
func TestDepartmentTemplate_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.perm", "081234570925")
	raw, err := json.Marshal(map[string]any{"merchant_id": merchantID, "code": "PERM", "name": "Poli Izin"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := accountRequest(router, http.MethodPost, "/api/v1/departments", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create department expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var dept struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dept); err != nil {
		t.Fatalf("decode department: %v", err)
	}

	restrictedUserID := "7c7c7c7c-7c7c-7c7c-7c7c-7c7c7c7c7c7c"
	roleID := "7d7d7d7d-7d7d-7d7d-7d7d-7d7d7d7d7d7d"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Dept Manage')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'depttpl.norole', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed restricted user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "depttpl.norole", "depttpl-norole-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	pratama := listDepartmentTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]
	if rec := accountRequest(router, http.MethodGet, "/api/v1/department-templates", restrictedToken, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := applyDepartmentTemplateAPI(t, router, restrictedToken, companyID, merchantID, pratama.ID, map[string]any{"department_ids": templateDepartmentIDs(pratama)}); rec.Code != http.StatusForbidden {
		t.Fatalf("apply without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := accountRequest(router, http.MethodGet, "/api/v1/departments/"+dept.Data.ID+"/code-maps", restrictedToken, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET code-maps without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := accountRequest(router, http.MethodPut, "/api/v1/departments/"+dept.Data.ID+"/code-maps", restrictedToken, companyID, merchantID, raw); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT code-maps without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDepartmentTemplate_CodeMapsReplaceSet — PUT replaces the whole set
// atomically: systems no longer sent are deleted, sent systems are upserted.
func TestDepartmentTemplate_CodeMapsReplaceSet(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.maps", "081234570926")
	deptID := createDepartmentForMaps(t, router, token, companyID, merchantID)

	putMaps := func(items []map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"items": items})
		if err != nil {
			t.Fatalf("marshal items: %v", err)
		}
		return accountRequest(router, http.MethodPut, "/api/v1/departments/"+deptID+"/code-maps", token, companyID, merchantID, raw)
	}
	getMaps := func() []departmentCodeMapView {
		t.Helper()
		rec := accountRequest(router, http.MethodGet, "/api/v1/departments/"+deptID+"/code-maps", token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET code-maps expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data []departmentCodeMapView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode code maps: %v", err)
		}
		return resp.Data
	}

	if rec := putMaps([]map[string]any{
		{"system": "bpjs-vclaim", "code": "UMU", "display": "Poli Umum"},
		{"system": "satusehat", "code": "poli-umum"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("first PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	maps := getMaps()
	// ORDER BY system: bpjs-vclaim < satusehat.
	if len(maps) != 2 || maps[0].System != "bpjs-vclaim" || maps[0].Code != "UMU" || maps[1].System != "satusehat" || maps[1].Code != "poli-umum" {
		t.Fatalf("expected bpjs-vclaim/UMU + satusehat/poli-umum, got %v", maps)
	}

	// Second PUT: only bpjs-vclaim with a changed code — satusehat is dropped.
	if rec := putMaps([]map[string]any{{"system": "bpjs-vclaim", "code": "UMU2"}}); rec.Code != http.StatusOK {
		t.Fatalf("second PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	maps = getMaps()
	if len(maps) != 1 || maps[0].System != "bpjs-vclaim" || maps[0].Code != "UMU2" {
		t.Fatalf("expected single bpjs-vclaim/UMU2, got %v", maps)
	}
}

// createDepartmentForMaps creates one poli through the API and returns its id.
func createDepartmentForMaps(t *testing.T, router *gin.Engine, token, companyID, merchantID string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"merchant_id": merchantID, "code": "MAPS", "name": "Poli Mapping"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := accountRequest(router, http.MethodPost, "/api/v1/departments", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create department expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode department: %v", err)
	}
	return resp.Data.ID
}

// TestDepartmentTemplate_CodeMapsValidation — invalid system (migration regex),
// blank code, and duplicate systems are rejected with 400 before any write.
func TestDepartmentTemplate_CodeMapsValidation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "depttpl.valid", "081234570927")
	deptID := createDepartmentForMaps(t, router, token, companyID, merchantID)

	putMapsRaw := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return accountRequest(router, http.MethodPut, "/api/v1/departments/"+deptID+"/code-maps", token, companyID, merchantID, raw)
	}

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"uppercase system", map[string]any{"items": []map[string]any{{"system": "BPJS", "code": "X"}}}},
		{"space in system", map[string]any{"items": []map[string]any{{"system": "bpjs vclaim", "code": "X"}}}},
		{"leading punctuation", map[string]any{"items": []map[string]any{{"system": "-bpjs", "code": "X"}}}},
		{"blank code", map[string]any{"items": []map[string]any{{"system": "bpjs-vclaim", "code": "  "}}}},
		{"duplicate systems", map[string]any{"items": []map[string]any{
			{"system": "bpjs-vclaim", "code": "A"},
			{"system": "bpjs-vclaim", "code": "B"},
		}}},
	} {
		if rec := putMapsRaw(tc.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.department_code_map WHERE department_id::text = $1", deptID).Scan(&remaining); err != nil {
		t.Fatalf("count code maps: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("validation failures must not write, got %d rows", remaining)
	}
}

// TestDepartmentTemplate_RuntimeApply — the production role (app_runtime with
// the merchant GUC) can run the apply writes: CreateDepartment plus the
// department_code_map inserts, visible only under the matching merchant GUC.
func TestDepartmentTemplate_RuntimeApply(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "RPA")
	tnB := seedChildRLSTenant(t, ctx, pool, "RPB")

	// Committed map for tenant A, inserted by the owner pool (no RLS).
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.department_code_map (department_id, system, code) VALUES ($1, 'bpjs-vclaim', 'RPA')", tnA.departmentID); err != nil {
		t.Fatalf("seed code map: %v", err)
	}

	// Another merchant's GUC sees none of tenant A's rows.
	txB := childRLSAppRuntimeTx(t, ctx, pool, tnB.companyID, tnB.merchantID)
	var foreign int
	if err := txB.QueryRow(ctx, "SELECT count(*) FROM core.department_code_map").Scan(&foreign); err != nil {
		t.Fatalf("count code maps under merchant B GUC: %v", err)
	}
	if foreign != 0 {
		t.Fatalf("expected 0 rows under merchant B GUC, got %d", foreign)
	}
	if err := txB.Rollback(ctx); err != nil {
		t.Fatalf("rollback B: %v", err)
	}

	// Apply writes under app_runtime + tenant A's GUC.
	txA := childRLSAppRuntimeTx(t, ctx, pool, tnA.companyID, tnA.merchantID)
	q := sqlcgen.New(txA)
	var templateID pgtype.UUID
	if err := templateID.Scan(func() string {
		var id string
		if err := pool.QueryRow(ctx,
			"SELECT id::text FROM core.template WHERE code = 'klinik_utama' AND kind = 'department'").Scan(&id); err != nil {
			t.Fatalf("read klinik_utama template: %v", err)
		}
		return id
	}()); err != nil {
		t.Fatalf("scan template id: %v", err)
	}
	departments, err := q.ListTemplateDepartments(ctx)
	if err != nil {
		t.Fatalf("ListTemplateDepartments as app_runtime: %v", err)
	}
	var umu sqlcgen.ListTemplateDepartmentsRow
	for _, d := range departments {
		if d.TemplateID == templateID && d.Code == "UMU" {
			umu = d
			break
		}
	}
	var merchantID, companyID pgtype.UUID
	if err := merchantID.Scan(tnA.merchantID); err != nil {
		t.Fatalf("scan merchant id: %v", err)
	}
	if err := companyID.Scan(tnA.companyID); err != nil {
		t.Fatalf("scan company id: %v", err)
	}
	dept, err := q.CreateDepartment(ctx, sqlcgen.CreateDepartmentParams{
		CompanyID:  companyID,
		MerchantID: merchantID,
		Code:       umu.Code,
		Name:       umu.Name,
	})
	if err != nil {
		t.Fatalf("CreateDepartment as app_runtime: %v", err)
	}
	for _, m := range []struct{ system, code string }{{"bpjs-vclaim", umu.Code}, {"satusehat", "poli-umum"}} {
		if err := q.CreateDepartmentCodeMap(ctx, sqlcgen.CreateDepartmentCodeMapParams{
			DepartmentID: dept.ID,
			System:       m.system,
			Code:         m.code,
		}); err != nil {
			t.Fatalf("CreateDepartmentCodeMap %s as app_runtime: %v", m.system, err)
		}
	}
	var own int
	if err := txA.QueryRow(ctx, "SELECT count(*) FROM core.department_code_map").Scan(&own); err != nil {
		t.Fatalf("count code maps under merchant A GUC: %v", err)
	}
	// 1 committed seed row + 2 rows from the in-tx apply.
	if own != 3 {
		t.Fatalf("expected 3 visible rows under merchant A GUC, got %d", own)
	}
	if err := txA.Rollback(ctx); err != nil {
		t.Fatalf("rollback A: %v", err)
	}
}
