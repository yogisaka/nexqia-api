//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// Role Matrix bulk endpoints (spec 2026-10-05-role-matrix-rbac-design §4):
// PUT /roles/:id/matrix (rbac_matrix_save_test.go), read endpoints, POST /roles/:id/duplicate.
// Fixtures follow role_consolidation_db_test.go (fresh container, manual INSERTs);
// requests go through the full router like user_conflict_test.go so TenantMiddleware,
// auth and permission gating are exercised (RLS is NOT tested here).

const (
	rmPermA = "00000000-0000-0000-0004-0000000001a1" // test.rm.perm.a
	rmPermB = "00000000-0000-0000-0004-0000000001a2" // test.rm.perm.b
	rmPermC = "00000000-0000-0000-0004-0000000001a3" // test.rm.perm.c
	rmPermD = "00000000-0000-0000-0004-0000000001a4" // test.rm.perm.d
	rmPermE = "00000000-0000-0000-0004-0000000001a5" // test.rm.perm.e

	rmCompany     = "00000000-0000-0000-0060-000000000001"
	rmMerchant    = "00000000-0000-0000-0060-000000000011"
	rmMerchant2   = "00000000-0000-0000-0060-000000000012"
	rmPerson      = "00000000-0000-0000-0060-000000000055"
	rmUser        = "00000000-0000-0000-0060-0000000000aa"
	rmRoleAdmin   = "00000000-0000-0000-0060-0000000000a1" // grants core.role.manage
	rmRoleTarget  = "00000000-0000-0000-0060-0000000000b1" // PUT permissions target
	rmRoleWidgets = "00000000-0000-0000-0060-0000000000b2" // PUT widgets target
	rmRoleDupSrc  = "00000000-0000-0000-0060-0000000000c1" // duplicate source
	rmRoleDeleted = "00000000-0000-0000-0060-0000000000d1" // soft-deleted role

	rmSeeder = "00000000-0000-0000-0000-0000000000dd"
)

// roleMatrixPool starts a fresh Postgres container migrated to 000060 (role_widget,
// role_change_log) — same container image and migrate setup as roleConsolidationPool.
func roleMatrixPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, *migrate.Migrate) {
	t.Helper()
	container, err := postgres.Run(ctx, "nexqia-api-postgres-test:latest",
		postgres.WithDatabase("nexqia_test"),
		postgres.WithUsername("nexqia"),
		postgres.WithPassword("nexqia"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}
	m, err := migrate.New("file://../../migrations", dsn)
	if err != nil {
		t.Fatalf("failed to init migrate: %v", err)
	}
	if err := m.Migrate(60); err != nil {
		t.Fatalf("failed to migrate to 000060: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, m
}

func roleMatrixExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec failed: %v\nsql: %s", err, sql)
	}
}

func roleMatrixSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.company (id, code, name) VALUES ($1, 'RM-A', 'RS Role Matrix')`, rmCompany)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'M1', 'RS Role Matrix utama')`,
		rmMerchant, rmCompany)
	// Second merchant: same user + role assigned here too, to prove
	// UserCount counts DISTINCT users (see TestRoleMatrixRolesListCounts).
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'M2', 'RS Role Matrix cabang')`,
		rmMerchant2, rmCompany)
	// Person behind the admin user — change-log rows join through it for
	// changed_by_name.
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.person (id, company_id, full_name) VALUES ($1, $2, 'Admin Matrix')`,
		rmPerson, rmCompany)

	// Permission catalog entries (ON CONFLICT keeps a pre-seeded row's own id;
	// the tests always look ids up by code afterwards).
	for i, code := range []string{"test.rm.perm.a", "test.rm.perm.b", "test.rm.perm.c", "test.rm.perm.d", "test.rm.perm.e"} {
		roleMatrixExec(t, ctx, pool,
			`INSERT INTO core.permission (id, code, description, module) VALUES ($1, $2, 'role matrix test perm', 'core')
			 ON CONFLICT (code) DO NOTHING`,
			[]string{rmPermA, rmPermB, rmPermC, rmPermD, rmPermE}[i], code)
	}
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.permission (id, code, description, module) VALUES ($1, 'core.role.manage', 'manage roles', 'core')
		 ON CONFLICT (code) DO NOTHING`,
		"00000000-0000-0000-0004-0000000000f1")

	// Users (password_hash NOT NULL; login not needed — token generated directly).
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.app_user (id, company_id, person_id, username, password_hash, is_active) VALUES ($1, $2, $3, 'matrix.admin', 'x-hash', true)`,
		rmUser, rmCompany, rmPerson)

	// Roles. UNIQUE(company_id, name); soft-delete CHECK needs deleted_at+deleted_by together.
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role (id, company_id, name, description, is_system, requires_physician_data) VALUES
		 ($1, $2, 'Matrix Admin', NULL, false, false),
		 ($3, $4, 'Matrix Target', 'target role', false, false),
		 ($5, $6, 'Matrix Widgets', NULL, false, false),
		 ($7, $8, 'Matrix Dup Source', 'source desc', false, true),
		 ($9, $10, 'Matrix Deleted', NULL, false, false)`,
		rmRoleAdmin, rmCompany,
		rmRoleTarget, rmCompany,
		rmRoleWidgets, rmCompany,
		rmRoleDupSrc, rmCompany,
		rmRoleDeleted, rmCompany)

	// Soft-delete the fixture role (both columns, per CHECK).
	roleMatrixExec(t, ctx, pool,
		`UPDATE core.role SET deleted_at = now(), deleted_by = $1 WHERE id = $2`, rmSeeder, rmRoleDeleted)

	// Admin role holds core.role.manage; target roles hold their grants.
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role_permission (role_id, permission_id)
		 SELECT $1, id FROM core.permission WHERE code = 'core.role.manage'`, rmRoleAdmin)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role_permission (role_id, permission_id) VALUES
		 ($1, $2), ($1, $3)`,
		rmRoleTarget, rmPermA, rmPermB)
	// Duplicate source: 5 grants (a..e) + 1 widget override (kpi visible).
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role_permission (role_id, permission_id) VALUES
		 ($1, $2), ($1, $3), ($1, $4), ($1, $5), ($1, $6)`,
		rmRoleDupSrc, rmPermA, rmPermB, rmPermC, rmPermD, rmPermE)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role_widget (role_id, widget_key, visible) VALUES ($1, 'kpi', true)`,
		rmRoleDupSrc)

	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)`,
		rmUser, rmMerchant, rmRoleAdmin)
	// Same user, same admin role, second merchant — 2 rows, still 1 distinct user.
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)`,
		rmUser, rmMerchant2, rmRoleAdmin)
}

func TestRoleMatrixReadEndpoints(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// Admin token without an active role id (legacy shape) — fine for the
	// PermRoleManage-gated read endpoints.
	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		return roleMatrixRequest(t, router, token, http.MethodGet, path, nil)
	}

	// Make one permission change first so matrix last_edit is populated.
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleTarget+"/matrix",
		rmSaveBody(rmMatrixRowVersion(t, router, token, rmRoleTarget), []string{rmPermA, rmPermB, rmPermC}, []map[string]any{}, "ubah izin target")); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// --- matrix shape ---
	rec := get("/api/v1/roles/" + rmRoleTarget + "/matrix")
	if rec.Code != http.StatusOK {
		t.Fatalf("matrix expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var matrix struct {
		Data struct {
			Role struct {
				ID                    string `json:"id"`
				Name                  string `json:"name"`
				Description           string `json:"description"`
				IsSystem              bool   `json:"is_system"`
				RequiresPhysicianData bool   `json:"requires_physician_data"`
			} `json:"role"`
			PermissionIDs []string `json:"permission_ids"`
			Widgets       []struct {
				Key     string `json:"key"`
				Visible bool   `json:"visible"`
			} `json:"widgets"`
			LastEdit *struct {
				ByName string `json:"by_name"`
				At     string `json:"at"`
				Reason string `json:"reason"`
			} `json:"last_edit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &matrix); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	if matrix.Data.Role.Name != "Matrix Target" || matrix.Data.Role.Description != "target role" ||
		matrix.Data.Role.IsSystem || matrix.Data.Role.RequiresPhysicianData {
		t.Fatalf("unexpected role block: %+v", matrix.Data.Role)
	}
	if len(matrix.Data.PermissionIDs) != 3 { // 3 = 2 lama (a,b) + 1 baru (c)
		t.Fatalf("want 3 permission ids, got %v", matrix.Data.PermissionIDs)
	}
	for _, want := range []string{rmPermA, rmPermB, rmPermC} {
		found := false
		for _, got := range matrix.Data.PermissionIDs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("permission id %s missing from %v", want, matrix.Data.PermissionIDs)
		}
	}
	if len(matrix.Data.Widgets) != 0 {
		t.Fatalf("want no widget overrides, got %v", matrix.Data.Widgets)
	}
	if matrix.Data.LastEdit == nil {
		t.Fatal("last_edit must be populated after a PUT")
	}
	// by_name comes from person.full_name of the fixture user.
	if matrix.Data.LastEdit.ByName != "Admin Matrix" {
		t.Fatalf("want by_name %q, got %q", "Admin Matrix", matrix.Data.LastEdit.ByName)
	}
	if matrix.Data.LastEdit.Reason != "ubah izin target" {
		t.Fatalf("want last_edit reason %q, got %q", "ubah izin target", matrix.Data.LastEdit.Reason)
	}
	if _, err := time.Parse(time.RFC3339, matrix.Data.LastEdit.At); err != nil {
		t.Fatalf("last_edit.at not RFC3339 (%q): %v", matrix.Data.LastEdit.At, err)
	}

	// --- matrix widgets sorted by key ---
	widgetsBody := func(widgets []map[string]any, reason string) map[string]any {
		return map[string]any{
			"row_version": rmMatrixRowVersion(t, router, token, rmRoleWidgets), "name": "Matrix Widgets", "description": "",
			"requires_physician_data": false, "permission_ids": []string{}, "widgets": widgets, "reason": reason,
		}
	}
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleWidgets+"/matrix",
		widgetsBody([]map[string]any{{"key": "queue", "visible": false}, {"key": "kpi", "visible": true}}, "widget awal")); rec.Code != http.StatusOK {
		t.Fatalf("widget save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleWidgets+"/matrix",
		widgetsBody([]map[string]any{{"key": "kpi", "visible": false}, {"key": "queue", "visible": true}}, "widget ubah")); rec.Code != http.StatusOK {
		t.Fatalf("widget save 2 expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = get("/api/v1/roles/" + rmRoleWidgets + "/matrix")
	if rec.Code != http.StatusOK {
		t.Fatalf("matrix widgets expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var wmat struct {
		Data struct {
			Widgets []struct {
				Key     string `json:"key"`
				Visible bool   `json:"visible"`
			} `json:"widgets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wmat); err != nil {
		t.Fatalf("decode widget matrix: %v", err)
	}
	// Stored overrides: kpi=false (2nd PUT), queue=true; keys alphabetical.
	if len(wmat.Data.Widgets) != 2 ||
		wmat.Data.Widgets[0].Key != "kpi" || wmat.Data.Widgets[0].Visible ||
		wmat.Data.Widgets[1].Key != "queue" || !wmat.Data.Widgets[1].Visible {
		t.Fatalf("want sorted overrides [kpi:false queue:true], got %+v", wmat.Data.Widgets)
	}

	// --- audit list: newest first + limit ---
	rec = get("/api/v1/roles/" + rmRoleWidgets + "/audit")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var audit struct {
		Data []struct {
			ChangedAt     string         `json:"changed_at"`
			ChangedByName string         `json:"changed_by_name"`
			Reason        string         `json:"reason"`
			Changes       map[string]any `json:"changes"`
		} `json:"data"`
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	// 2 log rows were written by the two widget PUTs above.
	if len(audit.Data) != 2 {
		t.Fatalf("want 2 audit entries, got %d", len(audit.Data))
	}
	// DESC: newest ("widget ubah") first.
	if audit.Data[0].Reason != "widget ubah" || audit.Data[1].Reason != "widget awal" {
		t.Fatalf("want desc order [widget ubah, widget awal], got [%q, %q]",
			audit.Data[0].Reason, audit.Data[1].Reason)
	}
	if audit.Data[0].ChangedByName != "Admin Matrix" {
		t.Fatalf("want changed_by_name %q, got %q", "Admin Matrix", audit.Data[0].ChangedByName)
	}
	if _, err := time.Parse(time.RFC3339, audit.Data[0].ChangedAt); err != nil {
		t.Fatalf("changed_at not RFC3339 (%q): %v", audit.Data[0].ChangedAt, err)
	}
	if audit.Meta["limit"] != float64(20) { // default limit = 20
		t.Fatalf("want default meta.limit 20, got %v", audit.Meta["limit"])
	}
	rec = get("/api/v1/roles/" + rmRoleWidgets + "/audit?limit=1")
	var limited struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil {
		t.Fatalf("decode limited audit: %v", err)
	}
	if len(limited.Data) != 1 || limited.Meta["limit"] != float64(1) {
		t.Fatalf("want 1 entry with meta.limit 1, got %d entries, meta %v", len(limited.Data), limited.Meta)
	}

	// --- self widgets: active role override ---
	// Token WITH an active role id: dup source has one stored override (kpi=true).
	selfToken, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, rmRoleDupSrc, "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate self token: %v", err)
	}
	rec = roleMatrixRequest(t, router, selfToken, http.MethodGet,
		"/api/v1/users/"+rmUser+"/widgets?merchant_id="+rmMerchant, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("self widgets expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var selfw struct {
		Data []struct {
			Key     string `json:"key"`
			Visible bool   `json:"visible"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &selfw); err != nil {
		t.Fatalf("decode self widgets: %v", err)
	}
	if len(selfw.Data) != 1 || selfw.Data[0].Key != "kpi" || !selfw.Data[0].Visible {
		t.Fatalf("want [{kpi true}], got %+v", selfw.Data)
	}
	// Legacy token without a role id → fallback empty list.
	rec = roleMatrixRequest(t, router, token, http.MethodGet,
		"/api/v1/users/"+rmUser+"/widgets?merchant_id="+rmMerchant, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy self widgets expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var legacy struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("decode legacy self widgets: %v", err)
	}
	if len(legacy.Data) != 0 {
		t.Fatalf("want [] for token without role id, got %v", legacy.Data)
	}
}

func TestRoleMatrixRolesListCounts(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	rec := roleMatrixRequest(t, router, token, http.MethodGet, "/api/v1/roles", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /roles expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode roles list: %v", err)
	}
	byName := make(map[string]map[string]any, len(list.Data))
	for _, item := range list.Data {
		byName[item["Name"].(string)] = item
	}
	// Old PascalCase fields preserved.
	admin, ok := byName["Matrix Admin"]
	if !ok {
		t.Fatalf("Matrix Admin missing from list: %v", byName)
	}
	for _, field := range []string{"ID", "Name", "Description", "IsSystem", "RequiresPhysicianData"} {
		if _, present := admin[field]; !present {
			t.Fatalf("legacy field %s missing on role item", field)
		}
	}
	// UserCount = COUNT(DISTINCT user_id) in user_merchant_role for the role:
	// rmUser is assigned Matrix Admin on 2 merchants → 2 rows, 1 DISTINCT user.
	if admin["UserCount"] != float64(1) {
		t.Fatalf("want Matrix Admin UserCount 1, got %v", admin["UserCount"])
	}
	// PermissionCount = 1 grant (core.role.manage).
	if admin["PermissionCount"] != float64(1) {
		t.Fatalf("want Matrix Admin PermissionCount 1, got %v", admin["PermissionCount"])
	}
	dupSrc := byName["Matrix Dup Source"]
	// 5 = grants a..e seeded on the duplicate source.
	if dupSrc["PermissionCount"] != float64(5) {
		t.Fatalf("want Dup Source PermissionCount 5, got %v", dupSrc["PermissionCount"])
	}
	// No user_merchant_role row references the dup source → 0.
	if dupSrc["UserCount"] != float64(0) {
		t.Fatalf("want Dup Source UserCount 0, got %v", dupSrc["UserCount"])
	}
	// Soft-deleted role excluded (deleted_at IS NULL in ListRolesWithCounts).
	if _, present := byName["Matrix Deleted"]; present {
		t.Fatal("soft-deleted role must not be listed")
	}
}

func roleMatrixRequest(t *testing.T, router *gin.Engine, token, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", rmCompany)
	req.Header.Set("X-Merchant-ID", rmMerchant)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// roleMatrixLogChanges returns the reason and changes JSON of every
// role_change_log row for the role, oldest first.
func roleMatrixLogChanges(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) []struct {
	Reason  string
	Changes map[string]any
} {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT reason, changes::text FROM core.role_change_log WHERE role_id = $1 ORDER BY changed_at`, roleID)
	if err != nil {
		t.Fatalf("query role_change_log: %v", err)
	}
	defer rows.Close()
	out := []struct {
		Reason  string
		Changes map[string]any
	}{}
	for rows.Next() {
		var reason, raw string
		if err := rows.Scan(&reason, &raw); err != nil {
			t.Fatalf("scan role_change_log: %v", err)
		}
		var changes map[string]any
		if err := json.Unmarshal([]byte(raw), &changes); err != nil {
			t.Fatalf("unmarshal changes: %v", err)
		}
		out = append(out, struct {
			Reason  string
			Changes map[string]any
		}{reason, changes})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows role_change_log: %v", err)
	}
	return out
}

func roleMatrixPermCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM core.role_permission WHERE role_id = $1`, roleID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func roleMatrixWidgetCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM core.role_widget WHERE role_id = $1`, roleID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func roleMatrixLogCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM core.role_change_log WHERE role_id = $1`, roleID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func roleMatrixPermCodes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT p.code FROM core.role_permission rp JOIN core.permission p ON p.id = rp.permission_id
		 WHERE rp.role_id = $1`, roleID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	codes := []string{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(codes)
	return codes
}

func TestRoleMatrixDuplicate(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	dup := func(roleID string, body map[string]any) *httptest.ResponseRecorder {
		return roleMatrixRequest(t, router, token, http.MethodPost, "/api/v1/roles/"+roleID+"/duplicate", body)
	}

	rec := dup(rmRoleDupSrc, map[string]any{"name": "Matrix Dup Copy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}
	if resp.Data.Name != "Matrix Dup Copy" || resp.Data.ID == "" {
		t.Fatalf("want new id + name, got %+v", resp.Data)
	}
	newID := resp.Data.ID

	// Copy: tepat 5 permission (5 grant sumber) + 1 override widget (kpi).
	if got := roleMatrixPermCount(t, ctx, pool, newID); got != 5 {
		t.Fatalf("want exactly 5 copied grants, got %d", got)
	}
	codes := roleMatrixPermCodes(t, ctx, pool, newID)
	want := []string{"test.rm.perm.a", "test.rm.perm.b", "test.rm.perm.c", "test.rm.perm.d", "test.rm.perm.e"}
	if len(codes) != 5 {
		t.Fatalf("want 5 codes, got %v", codes)
	}
	for i, code := range want {
		if codes[i] != code {
			t.Fatalf("want codes %v, got %v", want, codes)
		}
	}
	if got := roleMatrixWidgetCount(t, ctx, pool, newID); got != 1 {
		t.Fatalf("want exactly 1 copied widget override, got %d", got)
	}
	var isSystem bool
	if err := pool.QueryRow(ctx,
		`SELECT is_system FROM core.role WHERE id = $1`, newID).Scan(&isSystem); err != nil {
		t.Fatal(err)
	}
	if isSystem {
		t.Fatal("duplicate must have is_system=false")
	}

	// Nama sama lagi → 409 (UNIQUE(company_id, name) juga mengikat soft-deleted).
	if rec := dup(rmRoleDupSrc, map[string]any{"name": "Matrix Dup Copy"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Sumber soft-deleted → 404.
	if rec := dup(rmRoleDeleted, map[string]any{"name": "Matrix Dup Deleted"}); rec.Code != http.StatusNotFound {
		t.Fatalf("soft-deleted source expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
