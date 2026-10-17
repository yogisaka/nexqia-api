//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedAuditRole inserts a core.role row and returns its id.
func seedAuditRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, id, name string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", id, companyID, name); err != nil {
		t.Fatalf("seed role %s: %v", name, err)
	}
}

// seedRoleChangeLog inserts one core.role_change_log row with an explicit
// changed_at offset ("1 hour" style, passed straight into the interval).
func seedRoleChangeLog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, roleID, changedBy, age string, reason, changes string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.role_change_log (company_id, role_id, changed_by, changed_at, reason, changes) "+
			"VALUES ($1, $2, $3, now() - ($4::text)::interval, $5, $6::jsonb)",
		companyID, roleID, changedBy, age, reason, changes); err != nil {
		t.Fatalf("seed role_change_log: %v", err)
	}
}

// TestAuditRoleChanges_List — company A sees only its own rows of the last 7
// days, newest first, with role/actor resolution; the role_id, changed_by,
// from and limit/offset filters and the over-wide range 400 all behave
// (spec 2026-10-08-role-change-history §3.1).
func TestAuditRoleChanges_List(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.rolelist", "081234570301")
	otherCompanyID, _, otherOwnerID, _, _ := seedAccountOwner(t, ctx, pool, router, "audit.roleother", "081234570302")

	kasirID := "7a7a0001-0000-0000-0000-000000000001"
	lamaID := "7a7a0001-0000-0000-0000-000000000002"
	otherRoleID := "7a7a0001-0000-0000-0000-000000000003"
	seedAuditRole(t, ctx, pool, companyID, kasirID, "Kasir")
	seedAuditRole(t, ctx, pool, companyID, lamaID, "Lama")
	seedAuditRole(t, ctx, pool, otherCompanyID, otherRoleID, "Role Other")
	if _, err := pool.Exec(ctx, "UPDATE core.role SET deleted_at = now(), deleted_by = $2 WHERE id = $1", lamaID, ownerID); err != nil {
		t.Fatalf("soft delete role Lama: %v", err)
	}

	kasirChanges := `{"meta":{"name":{"from":"Kasir","to":"Kasir Senior"}}}`
	var kasirChangesAny map[string]any
	if err := json.Unmarshal([]byte(kasirChanges), &kasirChangesAny); err != nil {
		t.Fatalf("decode seeded changes: %v", err)
	}
	// Company A: 2 Kasir rows (1h & 2h ago), 1 deleted-Lama row (3h ago),
	// 1 Kasir row 10 days ago (outside the default 7-day window).
	seedRoleChangeLog(t, ctx, pool, companyID, kasirID, ownerID, "1 hour", "naik jabatan", kasirChanges)
	seedRoleChangeLog(t, ctx, pool, companyID, kasirID, ownerID, "2 hours", "rapor semester", `{"meta":{"description":{"from":"a","to":"b"}}}`)
	seedRoleChangeLog(t, ctx, pool, companyID, lamaID, ownerID, "3 hours", "role pensiun", `{"permission":{"removed":["x.y"]}}`)
	seedRoleChangeLog(t, ctx, pool, companyID, kasirID, ownerID, "10 days", "usulan lama", `{"meta":{"name":{"from":"Kasir","to":"Kasir"}}}`)
	// Company B: 1 row that must never leak into company A's list.
	seedRoleChangeLog(t, ctx, pool, otherCompanyID, otherRoleID, otherOwnerID, "1 hour", "company lain", `{"meta":{"name":{"from":"a","to":"b"}}}`)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list role changes expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	type roleChangeList struct {
		Data []map[string]any `json:"data"`
		Meta map[string]int   `json:"meta"`
	}
	var resp roleChangeList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode role-changes response: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("expected exactly 3 rows in the default 7-day window, got %d: %s", len(resp.Data), rec.Body.String())
	}
	first := resp.Data[0]
	if first["role_name"] != "Kasir" {
		t.Fatalf("expected newest row role_name Kasir, got %v", first["role_name"])
	}
	if first["reason"] != "naik jabatan" {
		t.Fatalf("expected reason seeded verbatim, got %v", first["reason"])
	}
	gotChanges, err := json.Marshal(first["changes"])
	if err != nil {
		t.Fatalf("re-marshal changes: %v", err)
	}
	var changesMap map[string]any
	if err := json.Unmarshal(gotChanges, &changesMap); err != nil {
		t.Fatalf("decode response changes %s: %v", gotChanges, err)
	}
	if !reflect.DeepEqual(changesMap, kasirChangesAny) {
		t.Fatalf("changes mismatch: got %s, want %s", gotChanges, kasirChanges)
	}
	if first["changed_by"] != ownerID {
		t.Fatalf("expected changed_by %s, got %v", ownerID, first["changed_by"])
	}
	if resp.Meta["limit"] != 50 || resp.Meta["offset"] != 0 {
		t.Fatalf("expected meta {limit:50, offset:0}, got %v", resp.Meta)
	}

	var lamaRow map[string]any
	for _, row := range resp.Data {
		if row["role_name"] == "Lama" {
			lamaRow = row
		}
	}
	if lamaRow == nil || lamaRow["role_deleted"] != true {
		t.Fatalf("expected the soft-deleted role flagged role_deleted, got %v", lamaRow)
	}

	// role_id filter: only the deleted role's row.
	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes?role_id="+lamaID, token, companyID, merchantID, nil)
	var filtered roleChangeList
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("role_id filter expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(filtered.Data) != 1 || filtered.Data[0]["role_id"] != lamaID {
		t.Fatalf("expected exactly 1 row for the role filter, got %s", rec.Body.String())
	}

	// changed_by filter with a random UUID: no rows.
	rec = accountRequest(router, http.MethodGet,
		"/api/v1/audit/role-changes?changed_by=00000000-0000-0000-0000-0000000000ff", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("changed_by filter expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var none roleChangeList
	if err := json.Unmarshal(rec.Body.Bytes(), &none); err != nil {
		t.Fatalf("decode empty filter response: %v", err)
	}
	if len(none.Data) != 0 {
		t.Fatalf("expected 0 rows for an unknown changed_by, got %d", len(none.Data))
	}

	// limit/offset: the second page item is the full list's second row.
	q := url.Values{"limit": {"1"}, "offset": {"1"}}
	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes?"+q.Encode(), token, companyID, merchantID, nil)
	var paged roleChangeList
	if err := json.Unmarshal(rec.Body.Bytes(), &paged); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("paged list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(paged.Data) != 1 || paged.Data[0]["id"] != resp.Data[1]["id"] {
		t.Fatalf("expected the second row of the full list, got %s", rec.Body.String())
	}

	// from=11 days ago widens the window to all 4 company A rows.
	q = url.Values{"from": {time.Now().UTC().Add(-11 * 24 * time.Hour).Format(time.RFC3339)}}
	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes?"+q.Encode(), token, companyID, merchantID, nil)
	var wide roleChangeList
	if err := json.Unmarshal(rec.Body.Bytes(), &wide); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("widened list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(wide.Data) != 4 {
		t.Fatalf("expected 4 rows with from=11 days ago, got %d: %s", len(wide.Data), rec.Body.String())
	}

	// from=100 days ago exceeds the 93-day audit cap → 400 invalid time range.
	q = url.Values{"from": {time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339)}}
	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes?"+q.Encode(), token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("100-day range expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAuditRoleChanges_RequiresPermission — a user without audit.log.view gets
// 403 on all three role-audit endpoints.
func TestAuditRoleChanges_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "audit.roleperm", "081234570311")
	restrictedUserID := seedRestrictedAuditUser(t, ctx, pool, companyID, merchantID, "audit.role.noview")
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "audit.role.noview", "audit-role-noview-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	for _, path := range []string{
		"/api/v1/audit/role-changes",
		"/api/v1/audit/role-changes/export",
		"/api/v1/audit/roles",
	} {
		rec := accountRequest(router, http.MethodGet, path, restrictedToken, companyID, merchantID, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s without audit.log.view expected 403, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAuditRoleChanges_Export — the CSV has a BOM, the exact header row, the
// csvSafe'd reason, the spec §3.4 summary text, and " (dihapus)" for a
// soft-deleted role (spec §3.2, §3.4).
func TestAuditRoleChanges_Export(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.roleexp", "081234570321")

	kasirID := "7a7a0002-0000-0000-0000-000000000001"
	goneID := "7a7a0002-0000-0000-0000-000000000002"
	seedAuditRole(t, ctx, pool, companyID, kasirID, "Kasir")
	seedAuditRole(t, ctx, pool, companyID, goneID, "Role Tua")
	if _, err := pool.Exec(ctx, "UPDATE core.role SET deleted_at = now(), deleted_by = $2 WHERE id = $1", goneID, ownerID); err != nil {
		t.Fatalf("soft delete role Tua: %v", err)
	}

	changes := `{"meta":{"name":{"from":"Kasir","to":"Kasir Senior"},"requires_physician_data":{"from":false,"to":true}},"permission":{"added":["a.b"],"removed":["c.d"]},"widget":[{"key":"kpi","from":null,"to":false}]}`
	seedRoleChangeLog(t, ctx, pool, companyID, kasirID, ownerID, "1 hour", "=SUM(1)", changes)
	seedRoleChangeLog(t, ctx, pool, companyID, goneID, ownerID, "2 hours", "role dihapus", `{"meta":{"name":{"from":"Role Tua","to":"Role Tua"}}}`)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); len(ct) < 8 || ct[:8] != "text/csv" {
		t.Fatalf("expected Content-Type text/csv, got %q", ct)
	}
	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("\xEF\xBB\xBF")) {
		t.Fatalf("expected a UTF-8 BOM at the start of the body")
	}
	table, err := csv.NewReader(bytes.NewReader(body[3:])).ReadAll()
	if err != nil {
		t.Fatalf("parse export CSV: %v", err)
	}
	if len(table) != 3 {
		t.Fatalf("expected header + 2 data rows, got %d: %v", len(table), table)
	}
	header := table[0]
	wantHeader := []string{"Waktu", "User", "Role", "Alasan", "Perubahan"}
	if !reflect.DeepEqual(header, wantHeader) {
		t.Fatalf("header mismatch: got %v, want %v", header, wantHeader)
	}
	first := table[1]
	if first[3] != "'=SUM(1)" {
		t.Fatalf("expected csvSafe'd reason '=SUM(1), got %q", first[3])
	}
	wantSummary := "Nama: Kasir → Kasir Senior; Kredensial STR/SIP: tidak → ya; + a.b; − c.d; Widget kpi: default → sembunyi"
	if first[4] != wantSummary {
		t.Fatalf("summary mismatch:\n got %q\nwant %q", first[4], wantSummary)
	}
	second := table[2]
	if len(second) < 5 || second[2] != "Role Tua (dihapus)" {
		t.Fatalf("expected the deleted role's name suffixed (dihapus), got %v", second)
	}
}

// TestAuditRoleChanges_RolesAndAccessLog — GET /audit/roles returns active and
// soft-deleted roles of the caller's company only, and each role-audit call
// records exactly one access_log row of its own action (spec §3.3, §3.4).
func TestAuditRoleChanges_RolesAndAccessLog(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.roleroles", "081234570331")
	otherCompanyID, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "audit.rolerolestwo", "081234570332")

	activeID := "7a7a0003-0000-0000-0000-000000000001"
	deletedID := "7a7a0003-0000-0000-0000-000000000002"
	otherRoleID := "7a7a0003-0000-0000-0000-000000000003"
	seedAuditRole(t, ctx, pool, companyID, activeID, "Perawat Audit")
	seedAuditRole(t, ctx, pool, companyID, deletedID, "Apoteker Audit")
	seedAuditRole(t, ctx, pool, otherCompanyID, otherRoleID, "Role Other")
	if _, err := pool.Exec(ctx, "UPDATE core.role SET deleted_at = now(), deleted_by = $2 WHERE id = $1", deletedID, ownerID); err != nil {
		t.Fatalf("soft delete role Apoteker Audit: %v", err)
	}
	seedRoleChangeLog(t, ctx, pool, companyID, activeID, ownerID, "1 hour", "cek", `{"meta":{"name":{"from":"a","to":"b"}}}`)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/role-changes", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list role changes expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/roles", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list audit roles expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	type rolesList struct {
		Data []map[string]any `json:"data"`
	}
	var resp rolesList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode audit roles response: %v", err)
	}
	var sawActive, sawDeleted bool
	for _, row := range resp.Data {
		if row["id"] == otherRoleID {
			t.Fatalf("another company's role leaked into /audit/roles")
		}
		if row["id"] == activeID {
			sawActive = row["deleted"] == false
		}
		if row["id"] == deletedID {
			sawDeleted = row["deleted"] == true
		}
	}
	if !sawActive || !sawDeleted {
		t.Fatalf("expected the active and the soft-deleted role flagged correctly, got %s", rec.Body.String())
	}

	var listRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'audit' AND action = 'list_role_change'", companyID).Scan(&listRows); err != nil {
		t.Fatalf("count list_role_change rows: %v", err)
	}
	if listRows != 1 {
		t.Fatalf("expected exactly 1 audit/list_role_change row, got %d", listRows)
	}
	var roleRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'audit' AND action = 'list_role'", companyID).Scan(&roleRows); err != nil {
		t.Fatalf("count list_role rows: %v", err)
	}
	if roleRows != 1 {
		t.Fatalf("expected exactly 1 audit/list_role row, got %d", roleRows)
	}
}
