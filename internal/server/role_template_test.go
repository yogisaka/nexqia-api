//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// runtimeTx opens a transaction acting as app_runtime for companyID, so RLS and
// the app_runtime grants apply (the test pool itself is the table owner).
func runtimeTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	return tx
}

// TestRoleTemplate_SeedComplete — migration 000051 seeds the three platform role
// templates with the expected roles, permission counts and physician flags.
func TestRoleTemplate_SeedComplete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	want := map[string]struct{ roles, perms int }{
		"klinik_pratama": {5, 20},
		"klinik_utama":   {6, 22},
		"rumah_sakit":    {7, 24},
	}
	for code, w := range want {
		var version, roles, perms int
		err := pool.QueryRow(ctx, `
			SELECT t.version, count(DISTINCT tr.id), count(trp.permission_id)
			FROM core.template t
			JOIN core.template_role tr ON tr.template_id = t.id
			LEFT JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
			WHERE t.code = $1 AND t.scope = 'platform' AND t.kind = 'role'
			GROUP BY t.version`, code).Scan(&version, &roles, &perms)
		if err != nil {
			t.Fatalf("%s: query template: %v", code, err)
		}
		if version != 3 || roles != w.roles || perms != w.perms {
			t.Fatalf("%s: expected version 3, %d roles, %d perms; got %d, %d, %d", code, w.roles, w.perms, version, roles, perms)
		}
	}

	var platformCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.template WHERE scope = 'platform' AND kind = 'role'").Scan(&platformCount); err != nil {
		t.Fatalf("count platform templates: %v", err)
	}
	if platformCount != 3 {
		t.Fatalf("expected 3 platform role templates, got %d", platformCount)
	}

	rows, err := pool.Query(ctx, `
		SELECT t.code, tr.name, tr.requires_physician_data
		FROM core.template_role tr JOIN core.template t ON t.id = tr.template_id
		WHERE t.scope = 'platform' AND t.kind = 'role'`)
	if err != nil {
		t.Fatalf("query template roles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code, name string
		var physician bool
		if err := rows.Scan(&code, &name, &physician); err != nil {
			t.Fatalf("scan template role: %v", err)
		}
		wantPhysician := name == "Dokter" || name == "Dokter Spesialis"
		if physician != wantPhysician {
			t.Fatalf("%s/%s: requires_physician_data = %v, want %v", code, name, physician, wantPhysician)
		}
		if name == "Admin RS" && code != "rumah_sakit" {
			t.Fatalf("Admin RS found in template %s", code)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate template roles: %v", err)
	}
	var adminRS int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_role tr JOIN core.template t ON t.id = tr.template_id
		WHERE tr.name = 'Admin RS' AND t.code = 'rumah_sakit'`).Scan(&adminRS); err != nil {
		t.Fatalf("count Admin RS: %v", err)
	}
	if adminRS != 1 {
		t.Fatalf("expected Admin RS once in rumah_sakit, got %d", adminRS)
	}
}

// TestRoleTemplate_RuntimeCannotWriteTemplates — app_runtime can read platform
// templates but cannot write template tables or rewrite apply history.
func TestRoleTemplate_RuntimeCannotWriteTemplates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.runtime", "081234570901")

	tx := runtimeTx(t, ctx, pool, companyID)

	stmts := []struct {
		name string
		sql  string
		args []any
	}{
		{"insert template", "INSERT INTO core.template (kind, code, name, scope, company_id, usage) VALUES ('role', 'mine', 'Mine', 'company', $1, 'copy')", []any{companyID}},
		{"update template", "UPDATE core.template SET name = 'x'", nil},
		{"delete template_role", "DELETE FROM core.template_role", nil},
		{"insert template_role_permission", "INSERT INTO core.template_role_permission (template_role_id, permission_id) SELECT template_role_id, permission_id FROM core.template_role_permission LIMIT 1", nil},
		{"update template_application", "UPDATE core.template_application SET template_version = 2", nil},
		{"delete template_application", "DELETE FROM core.template_application", nil},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err := tx.Exec(ctx, s.sql, s.args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: expected permission denied (42501), got %v", s.name, err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	}

	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template WHERE kind = 'role'").Scan(&visible); err != nil {
		t.Fatalf("count templates as app_runtime: %v", err)
	}
	if visible != 3 {
		t.Fatalf("expected 3 platform templates visible to app_runtime, got %d", visible)
	}
}

// TestRoleTemplate_CompanyIsolation — apply history is visible only to its own
// company under app_runtime.
func TestRoleTemplate_CompanyIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.iso.a", "081234570902")
	companyB, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.iso.b", "081234570903")

	if _, err := pool.Exec(ctx, `
		INSERT INTO core.template_application (company_id, template_id, template_version)
		SELECT $1, id, 1 FROM core.template WHERE code = 'klinik_pratama' AND scope = 'platform' AND kind = 'role'`, companyB); err != nil {
		t.Fatalf("seed template_application: %v", err)
	}

	for _, c := range []struct {
		company string
		want    int
	}{{companyA, 0}, {companyB, 1}} {
		tx := runtimeTx(t, ctx, pool, c.company)
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template_application").Scan(&n); err != nil {
			t.Fatalf("count template_application: %v", err)
		}
		if n != c.want {
			t.Fatalf("company %s: expected %d template_application rows, got %d", c.company, c.want, n)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}

type roleTemplateRoleView struct {
	ID                    string                  `json:"id"`
	Name                  string                  `json:"name"`
	RequiresPhysicianData bool                    `json:"requires_physician_data"`
	RequiresMFA           bool                    `json:"requires_mfa"`
	Permissions           []struct{ Code string } `json:"permissions"`
	Status                string                  `json:"status"`
	SimilarTo             json.RawMessage         `json:"similar_to"`
}

type roleTemplateView struct {
	ID      string                 `json:"id"`
	Code    string                 `json:"code"`
	Version int                    `json:"version"`
	Roles   []roleTemplateRoleView `json:"roles"`
}

type applyTemplateResponse struct {
	Data struct {
		Created []struct {
			RoleID string `json:"role_id"`
			Name   string `json:"name"`
		} `json:"created"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	} `json:"data"`
}

func listRoleTemplatesAPI(t *testing.T, router *gin.Engine, token, companyID, merchantID string) map[string]roleTemplateView {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/role-templates", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /role-templates expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []roleTemplateView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode role templates: %v", err)
	}
	byCode := map[string]roleTemplateView{}
	for _, tpl := range resp.Data {
		byCode[tpl.Code] = tpl
	}
	return byCode
}

func templateRoleByName(t *testing.T, tpl roleTemplateView, name string) roleTemplateRoleView {
	t.Helper()
	for _, r := range tpl.Roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("template %s has no role %q", tpl.Code, name)
	return roleTemplateRoleView{}
}

func templateRoleIDs(tpl roleTemplateView) []string {
	ids := make([]string, 0, len(tpl.Roles))
	for _, r := range tpl.Roles {
		ids = append(ids, r.ID)
	}
	return ids
}

func applyRoleTemplateAPI(t *testing.T, router *gin.Engine, token, companyID, merchantID, templateID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal apply body: %v", err)
	}
	return accountRequest(router, http.MethodPost, "/api/v1/role-templates/"+templateID+"/apply", token, companyID, merchantID, raw)
}

func decodeApply(t *testing.T, rec *httptest.ResponseRecorder) applyTemplateResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp applyTemplateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	return resp
}

// seedStatusRoles gives the company an active "kasir " and a deleted "Perawat".
func seedStatusRoles(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (company_id, name) VALUES ($1, 'kasir ')", companyID); err != nil {
		t.Fatalf("seed active role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (company_id, name, deleted_at, deleted_by) VALUES ($1, 'Perawat', now(), $2)", companyID, userID); err != nil {
		t.Fatalf("seed deleted role: %v", err)
	}
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != code || resp.Error != msg {
		t.Fatalf("expected %d %q, got %d: %s", code, msg, rec.Code, rec.Body.String())
	}
}

// TestRoleTemplate_ListShowsStatus — names match case/space-insensitively,
// deleted roles still count, and MFA/physician flags come from the template.
func TestRoleTemplate_ListShowsStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.list", "081234570904")
	seedStatusRoles(t, ctx, pool, companyID, userID)

	tpls := listRoleTemplatesAPI(t, router, token, companyID, merchantID)
	pratama := tpls["klinik_pratama"]
	if r := templateRoleByName(t, pratama, "Kasir"); r.Status != "exists" || r.RequiresMFA {
		t.Fatalf("Kasir: expected status exists, requires_mfa false; got %+v", r)
	}
	if r := templateRoleByName(t, pratama, "Perawat"); r.Status != "deleted_exists" {
		t.Fatalf("Perawat: expected deleted_exists, got %q", r.Status)
	}
	if r := templateRoleByName(t, pratama, "Dokter"); r.Status != "new" || !r.RequiresPhysicianData {
		t.Fatalf("Dokter: expected new + requires_physician_data; got %+v", r)
	}
	if r := templateRoleByName(t, pratama, "Admin Klinik"); !r.RequiresMFA {
		t.Fatal("Admin Klinik: expected requires_mfa true")
	}
	if r := templateRoleByName(t, tpls["rumah_sakit"], "Manajemen"); !r.RequiresMFA {
		t.Fatal("Manajemen: expected requires_mfa true")
	}
	kasir := templateRoleByName(t, pratama, "Kasir")
	var codes []string
	for _, p := range kasir.Permissions {
		codes = append(codes, p.Code)
	}
	if len(codes) != 2 || codes[0] != "billing.invoice.create" || codes[1] != "billing.invoice.view" {
		t.Fatalf("Kasir permissions: got %v", codes)
	}
}

// TestRoleTemplate_SimilarOnlyWarns — a similar (not equal) name keeps the
// role selectable and only reports the similar name.
func TestRoleTemplate_SimilarOnlyWarns(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.similar", "081234570905")
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (company_id, name) VALUES ($1, 'Kasir Rajal')", companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]
	kasir := templateRoleByName(t, pratama, "Kasir")
	if kasir.Status != "new" || string(kasir.SimilarTo) != `["Kasir Rajal"]` {
		t.Fatalf("Kasir: expected new + similar_to [Kasir Rajal], got %q %s", kasir.Status, kasir.SimilarTo)
	}
	if perawat := templateRoleByName(t, pratama, "Perawat"); string(perawat.SimilarTo) != "[]" {
		t.Fatalf("Perawat: expected similar_to [], got %s", perawat.SimilarTo)
	}
}

// TestRoleTemplate_ApplyCreatesRoles — applying a whole template copies roles,
// permissions and flags, and records the application.
func TestRoleTemplate_ApplyCreatesRoles(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.apply", "081234570906")
	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]

	resp := decodeApply(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"role_ids": templateRoleIDs(pratama)}))
	if len(resp.Data.Created) != 5 || len(resp.Data.Skipped) != 0 {
		t.Fatalf("expected 5 created / 0 skipped, got %+v", resp.Data)
	}

	var roles, systemRoles int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE is_system) FROM core.role
		WHERE company_id = $1 AND name IN ('Admin Klinik', 'Pendaftaran', 'Dokter', 'Perawat', 'Kasir')`, companyID).Scan(&roles, &systemRoles); err != nil {
		t.Fatalf("count created roles: %v", err)
	}
	if roles != 5 || systemRoles != 0 {
		t.Fatalf("expected 5 non-system roles, got %d (%d system)", roles, systemRoles)
	}
	var physician bool
	if err := pool.QueryRow(ctx, "SELECT requires_physician_data FROM core.role WHERE company_id = $1 AND name = 'Dokter'", companyID).Scan(&physician); err != nil || !physician {
		t.Fatalf("Dokter requires_physician_data: %v %v", physician, err)
	}
	var adminPerms int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.role_permission rp JOIN core.role r ON r.id = rp.role_id
		WHERE r.company_id = $1 AND r.name = 'Admin Klinik'`, companyID).Scan(&adminPerms); err != nil {
		t.Fatalf("count admin permissions: %v", err)
	}
	if adminPerms != 12 {
		t.Fatalf("expected 12 Admin Klinik permissions, got %d", adminPerms)
	}

	var version int
	var created []string
	var appliedBy string
	var platformAdmin *string
	if err := pool.QueryRow(ctx, `
		SELECT template_version, roles_created, applied_by::text, platform_admin_id::text
		FROM core.template_application WHERE company_id = $1`, companyID).Scan(&version, &created, &appliedBy, &platformAdmin); err != nil {
		t.Fatalf("read template_application: %v", err)
	}
	sort.Strings(created)
	if version != 3 || len(created) != 5 || appliedBy != userID || platformAdmin != nil {
		t.Fatalf("template_application: version %d, created %v, applied_by %s, platform_admin %v", version, created, appliedBy, platformAdmin)
	}
}

// TestRoleTemplate_SkipsExactAndDeleted — exact names (any case/spacing) and
// names held by deleted roles are skipped, never duplicated or a 500.
func TestRoleTemplate_SkipsExactAndDeleted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.skip", "081234570907")
	seedStatusRoles(t, ctx, pool, companyID, userID)
	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]

	resp := decodeApply(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"role_ids": templateRoleIDs(pratama)}))
	reasons := map[string]string{}
	for _, s := range resp.Data.Skipped {
		reasons[s.Name] = s.Reason
	}
	if len(resp.Data.Created) != 3 || len(reasons) != 2 || reasons["Kasir"] != "exists" || reasons["Perawat"] != "deleted_exists" {
		t.Fatalf("expected 3 created, Kasir exists + Perawat deleted_exists skipped; got %+v", resp.Data)
	}
	var kasirRoles int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.role WHERE company_id = $1 AND lower(trim(name)) = 'kasir'", companyID).Scan(&kasirRoles); err != nil {
		t.Fatalf("count kasir roles: %v", err)
	}
	if kasirRoles != 1 {
		t.Fatalf("expected a single Kasir role, got %d", kasirRoles)
	}
}

// TestRoleTemplate_ApplyTwiceSkipsAll — a second apply (e.g. a stale dialog)
// skips everything and still records the attempt.
func TestRoleTemplate_ApplyTwiceSkipsAll(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.twice", "081234570908")
	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]
	body := map[string]any{"role_ids": templateRoleIDs(pratama)}

	decodeApply(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, body))
	second := decodeApply(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, body))
	if len(second.Data.Created) != 0 || len(second.Data.Skipped) != 5 {
		t.Fatalf("second apply: expected 0 created / 5 skipped, got %+v", second.Data)
	}
	for _, s := range second.Data.Skipped {
		if s.Reason != "exists" {
			t.Fatalf("second apply: %s skipped with %q, want exists", s.Name, s.Reason)
		}
	}
	var applications int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.template_application WHERE company_id = $1", companyID).Scan(&applications); err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applications != 2 {
		t.Fatalf("expected 2 template_application rows, got %d", applications)
	}
}

// TestRoleTemplate_RejectsBadInput — exact 400/404 messages from spec §4.3.
func TestRoleTemplate_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.bad", "081234570909")
	tpls := listRoleTemplatesAPI(t, router, token, companyID, merchantID)
	pratama, utama := tpls["klinik_pratama"], tpls["klinik_utama"]
	firstID := pratama.Roles[0].ID

	assertError(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"role_ids": []string{utama.Roles[0].ID}}),
		http.StatusBadRequest, "role_ids must belong to the template")
	assertError(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"role_ids": []string{}}),
		http.StatusBadRequest, "role_ids is required")
	assertError(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, pratama.ID, map[string]any{"role_ids": []string{firstID, firstID}}),
		http.StatusBadRequest, "role_ids must not repeat")
	assertError(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, "0190f1a2-0000-7000-8000-000000000000", map[string]any{"role_ids": []string{firstID}}),
		http.StatusNotFound, "template not found")
	assertError(t, applyRoleTemplateAPI(t, router, token, companyID, merchantID, "abc", map[string]any{"role_ids": []string{firstID}}),
		http.StatusBadRequest, "invalid id")
}

// TestRoleTemplate_RequiresPermission — both endpoints need core.role.manage.
func TestRoleTemplate_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.perm", "081234570910")
	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]

	restrictedUserID := "7a7a7a7a-7a7a-7a7a-7a7a-7a7a7a7a7a7a"
	roleID := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b7b"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Role Manage')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'roletpl.norole', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed restricted user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "roletpl.norole", "roletpl-norole-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	if rec := accountRequest(router, http.MethodGet, "/api/v1/role-templates", restrictedToken, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := applyRoleTemplateAPI(t, router, restrictedToken, companyID, merchantID, pratama.ID, map[string]any{"role_ids": templateRoleIDs(pratama)}); rec.Code != http.StatusForbidden {
		t.Fatalf("apply without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRoleTemplate_RuntimeReadsAndRecords — the production role (app_runtime,
// which the API tests' owner pool bypasses) can run the GET query over the
// child tables and record an application for its own company only.
func TestRoleTemplate_RuntimeReadsAndRecords(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, _, userA, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.rt.a", "081234570911")
	companyB, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.rt.b", "081234570912")

	tx := runtimeTx(t, ctx, pool, companyA)
	q := sqlcgen.New(tx)
	rows, err := q.ListRoleTemplateRoles(ctx)
	if err != nil {
		t.Fatalf("ListRoleTemplateRoles as app_runtime: %v", err)
	}
	if len(rows) != 20+22+24 {
		t.Fatalf("expected 66 template role/permission rows as app_runtime, got %d", len(rows))
	}
	tpl, err := q.GetRoleTemplate(ctx, rows[0].TemplateID)
	if err != nil {
		t.Fatalf("GetRoleTemplate as app_runtime: %v", err)
	}

	var companyAID, userAID, companyBID pgtype.UUID
	for _, p := range []struct {
		dst *pgtype.UUID
		src string
	}{{&companyAID, companyA}, {&userAID, userA}, {&companyBID, companyB}} {
		if err := p.dst.Scan(p.src); err != nil {
			t.Fatalf("parse uuid %s: %v", p.src, err)
		}
	}
	record := func(company pgtype.UUID) error {
		return q.CreateTemplateApplication(ctx, sqlcgen.CreateTemplateApplicationParams{
			CompanyID:       company,
			TemplateID:      tpl.ID,
			TemplateVersion: tpl.Version,
			AppliedBy:       userAID,
			RolesCreated:    []string{},
			RolesSkipped:    []string{},
		})
	}
	if err := record(companyAID); err != nil {
		t.Fatalf("record own company application as app_runtime: %v", err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := record(companyBID); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("recording another company's application: expected RLS violation (42501), got %v", err)
	}
}

// TestRoleTemplate_ApplyUnderImpersonationRecordsAdmin — applying through an
// impersonation session is allowed and records the platform admin.
func TestRoleTemplate_ApplyUnderImpersonationRecordsAdmin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.imp", "081234570913")
	pratama := listRoleTemplatesAPI(t, router, token, companyID, merchantID)["klinik_pratama"]

	adminID := "0190f1a2-1111-7000-8000-000000000001"
	impToken, err := auth.GenerateImpersonationToken(testJWTSecret, userID, companyID, merchantID, "roletpl.imp", "roletpl-imp-device", adminID, time.Hour)
	if err != nil {
		t.Fatalf("GenerateImpersonationToken: %v", err)
	}
	resp := decodeApply(t, applyRoleTemplateAPI(t, router, impToken, companyID, merchantID, pratama.ID, map[string]any{"role_ids": templateRoleIDs(pratama)[:1]}))
	if len(resp.Data.Created) != 1 {
		t.Fatalf("expected 1 created role under impersonation, got %+v", resp.Data)
	}
	var recorded, appliedBy string
	if err := pool.QueryRow(ctx, "SELECT platform_admin_id::text, applied_by::text FROM core.template_application WHERE company_id = $1", companyID).Scan(&recorded, &appliedBy); err != nil {
		t.Fatalf("read template_application: %v", err)
	}
	if recorded != adminID || appliedBy != userID {
		t.Fatalf("expected platform_admin_id %s + applied_by %s, got %s + %s", adminID, userID, recorded, appliedBy)
	}
}
