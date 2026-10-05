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
// PUT /roles/:id/permissions, PUT /roles/:id/widgets, POST /roles/:id/duplicate.
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
		`INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'matrix.admin', 'x-hash', true)`,
		rmUser, rmCompany)

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

func TestRoleMatrixReplacePermissions(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	put := func(roleID string, body map[string]any) *httptest.ResponseRecorder {
		return roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+roleID+"/permissions", body)
	}

	// Start: 2 grants (perm a + b). PUT 3 ids = 2 lama + 1 baru → 204, DB tepat 3.
	if rec := put(rmRoleTarget, map[string]any{
		"permission_ids": []string{rmPermA, rmPermB, rmPermC},
		"reason":         "tambah perm c",
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("first PUT expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// 3 = 2 grant lama (a, b) + 1 grant baru (c).
	if got := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); got != 3 {
		t.Fatalf("want exactly 3 grants, got %d", got)
	}
	logs := roleMatrixLogChanges(t, ctx, pool, rmRoleTarget)
	if len(logs) != 1 {
		t.Fatalf("want exactly 1 role_change_log row, got %d", len(logs))
	}
	perm, _ := logs[0].Changes["permission"].(map[string]any)
	if perm == nil {
		t.Fatalf("changes.permission missing: %v", logs[0].Changes)
	}
	added := toStrings(perm["added"])
	removed := toStrings(perm["removed"])
	// added = baru − lama = [c]; removed = lama − baru = [].
	if len(added) != 1 || added[0] != "test.rm.perm.c" {
		t.Fatalf("want added [test.rm.perm.c], got %v", added)
	}
	if len(removed) != 0 {
		t.Fatalf("want removed [], got %v", removed)
	}
	if logs[0].Reason != "tambah perm c" {
		t.Fatalf("want reason %q, got %q", "tambah perm c", logs[0].Reason)
	}

	// Idempotent: PUT the same set (different reason) → 204, NO new log row (total stays 1).
	if rec := put(rmRoleTarget, map[string]any{
		"permission_ids": []string{rmPermA, rmPermB, rmPermC},
		"reason":         "ulang set sama",
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("idempotent PUT expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := roleMatrixLogCount(t, ctx, pool, rmRoleTarget); got != 1 {
		t.Fatalf("idempotent PUT must not log; want 1 log row, got %d", got)
	}
	if got := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); got != 3 {
		t.Fatalf("want still 3 grants, got %d", got)
	}

	// reason kosong → 400, DB tak berubah.
	if rec := put(rmRoleTarget, map[string]any{
		"permission_ids": []string{rmPermA},
		"reason":         "",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty reason expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); got != 3 {
		t.Fatalf("failed PUT must not write; want 3 grants, got %d", got)
	}

	// Id tak dikenal (UUID valid tapi bukan katalog) → 400 tanpa write.
	if rec := put(rmRoleTarget, map[string]any{
		"permission_ids": []string{rmPermA, "99999999-9999-9999-9999-999999999999"},
		"reason":         "unknown id",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown id expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); got != 3 {
		t.Fatalf("unknown-id PUT must not write; want 3 grants, got %d", got)
	}

	// permission_ids duplikat → 400 tanpa write.
	if rec := put(rmRoleTarget, map[string]any{
		"permission_ids": []string{rmPermA, rmPermA},
		"reason":         "dup",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate ids expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); got != 3 {
		t.Fatalf("duplicate-id PUT must not write; want 3 grants, got %d", got)
	}
	if got := roleMatrixLogCount(t, ctx, pool, rmRoleTarget); got != 1 {
		t.Fatalf("want still 1 log row after failures, got %d", got)
	}

	// Role soft-deleted → 404.
	if rec := put(rmRoleDeleted, map[string]any{
		"permission_ids": []string{rmPermA},
		"reason":         "deleted",
	}); rec.Code != http.StatusNotFound {
		t.Fatalf("soft-deleted role expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRoleMatrixWidgets(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	put := func(roleID string, body map[string]any) *httptest.ResponseRecorder {
		return roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+roleID+"/widgets", body)
	}

	// PUT 2 valid keys → 204, role_widget tepat 2 baris.
	if rec := put(rmRoleWidgets, map[string]any{
		"widgets": []map[string]any{{"key": "kpi", "visible": true}, {"key": "queue", "visible": false}},
		"reason":  "set widget awal",
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("first widget PUT expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// 2 = 2 key valid yang dikirim (kpi, queue).
	if got := roleMatrixWidgetCount(t, ctx, pool, rmRoleWidgets); got != 2 {
		t.Fatalf("want exactly 2 role_widget rows, got %d", got)
	}
	if got := roleMatrixLogCount(t, ctx, pool, rmRoleWidgets); got != 1 {
		t.Fatalf("want 1 log row after widget change, got %d", got)
	}

	// Key bogus → 400 unknown widget key.
	if rec := put(rmRoleWidgets, map[string]any{
		"widgets": []map[string]any{{"key": "bogus", "visible": true}},
		"reason":  "bogus",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus key expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Duplikat key dalam satu request → 400.
	if rec := put(rmRoleWidgets, map[string]any{
		"widgets": []map[string]any{{"key": "kpi", "visible": true}, {"key": "kpi", "visible": false}},
		"reason":  "dup",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate key expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Set ulang identik → 204 TANPA baris role_change_log baru (tetap 1).
	if rec := put(rmRoleWidgets, map[string]any{
		"widgets": []map[string]any{{"key": "queue", "visible": false}, {"key": "kpi", "visible": true}},
		"reason":  "set sama",
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("idempotent widget PUT expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := roleMatrixLogCount(t, ctx, pool, rmRoleWidgets); got != 1 {
		t.Fatalf("idempotent widget PUT must not log; want 1, got %d", got)
	}
	if got := roleMatrixWidgetCount(t, ctx, pool, rmRoleWidgets); got != 2 {
		t.Fatalf("want still 2 role_widget rows, got %d", got)
	}
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
