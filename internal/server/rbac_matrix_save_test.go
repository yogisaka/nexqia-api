//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// PUT /roles/:id/matrix (spec 2026-10-06-role-matrix-fixes §2.1): one
// atomic save of details + permission set + widget overrides with an
// optimistic lock and one combined role_change_log row. Fixtures:
// roleMatrixSeed (rbac_matrix_db_test.go) plus a system role and a user
// without any role.

const (
	rmRoleSystem   = "00000000-0000-0000-0060-0000000000e1"
	rmUserNoRole   = "00000000-0000-0000-0060-0000000000ab"
	rmPersonNoRole = "00000000-0000-0000-0060-000000000056"
	rmMissingRole  = "00000000-0000-0000-0060-0000000000ff"
)

func rmSaveSetup(t *testing.T) (context.Context, *pgxpool.Pool, *gin.Engine, string) {
	t.Helper()
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.role (id, company_id, name, description, is_system, requires_physician_data) VALUES ($1, $2, 'Matrix System', 'bawaan', true, false)`,
		rmRoleSystem, rmCompany)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.person (id, company_id, full_name) VALUES ($1, $2, 'Tanpa Role')`, rmPersonNoRole, rmCompany)
	roleMatrixExec(t, ctx, pool,
		`INSERT INTO core.app_user (id, company_id, person_id, username, password_hash, is_active) VALUES ($1, $2, $3, 'matrix.norole', 'x-hash', true)`,
		rmUserNoRole, rmCompany, rmPersonNoRole)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, rmUser, rmCompany, rmMerchant, "", "matrix.admin", "matrix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return ctx, pool, router, token
}

// rmMatrixRowVersion reads data.role.row_version from GET /roles/:id/matrix.
func rmMatrixRowVersion(t *testing.T, router *gin.Engine, token, roleID string) float64 {
	t.Helper()
	rec := roleMatrixRequest(t, router, token, http.MethodGet, "/api/v1/roles/"+roleID+"/matrix", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET matrix expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Role struct {
				RowVersion float64 `json:"row_version"`
			} `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	return body.Data.Role.RowVersion
}

// rmSaveBody is a PUT /matrix body with Matrix Target's seeded details.
func rmSaveBody(rowVersion float64, permIDs []string, widgets []map[string]any, reason string) map[string]any {
	return map[string]any{
		"row_version": rowVersion, "name": "Matrix Target", "description": "target role",
		"requires_physician_data": false, "permission_ids": permIDs, "widgets": widgets, "reason": reason,
	}
}

func rmErrorOf(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return resp.Error
}

func TestRoleMatrixSave_CombinedSaveOneLog(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	rv := rmMatrixRowVersion(t, router, token, rmRoleTarget)
	body := rmSaveBody(rv, []string{rmPermA, rmPermC}, []map[string]any{{"key": "kpi", "visible": false}}, "  ubah semua  ")
	body["name"] = "Matrix Target Baru"
	rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleTarget+"/matrix", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Role struct {
				Name       string  `json:"name"`
				RowVersion float64 `json:"row_version"`
			} `json:"role"`
			PermissionIDs []string `json:"permission_ids"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if resp.Data.Role.Name != "Matrix Target Baru" || resp.Data.Role.RowVersion <= rv || len(resp.Data.PermissionIDs) != 2 {
		t.Fatalf("unexpected save response: %+v (old row_version %v)", resp.Data, rv)
	}
	if codes := roleMatrixPermCodes(t, ctx, pool, rmRoleTarget); !equalStrings(codes, []string{"test.rm.perm.a", "test.rm.perm.c"}) {
		t.Fatalf("permission codes = %v, want [a c]", codes)
	}
	if n := roleMatrixWidgetCount(t, ctx, pool, rmRoleTarget); n != 1 {
		t.Fatalf("widget overrides = %d, want 1", n)
	}
	logs := roleMatrixLogChanges(t, ctx, pool, rmRoleTarget)
	if len(logs) != 1 {
		t.Fatalf("want exactly 1 change-log row, got %d", len(logs))
	}
	if logs[0].Reason != "ubah semua" {
		t.Fatalf("reason = %q, want trimmed %q", logs[0].Reason, "ubah semua")
	}
	meta, _ := logs[0].Changes["meta"].(map[string]any)
	nameChange, _ := meta["name"].(map[string]any)
	if len(meta) != 1 || nameChange["from"] != "Matrix Target" || nameChange["to"] != "Matrix Target Baru" {
		t.Fatalf("meta change = %v", logs[0].Changes["meta"])
	}
	perm, _ := logs[0].Changes["permission"].(map[string]any)
	if added := toStrings(perm["added"]); len(added) != 1 || added[0] != "test.rm.perm.c" {
		t.Fatalf("permission added = %v, want [test.rm.perm.c]", perm["added"])
	}
	if removed := toStrings(perm["removed"]); len(removed) != 1 || removed[0] != "test.rm.perm.b" {
		t.Fatalf("permission removed = %v, want [test.rm.perm.b]", perm["removed"])
	}
	widgets, _ := logs[0].Changes["widget"].([]any)
	if len(widgets) != 1 {
		t.Fatalf("widget change = %v, want exactly 1 entry", logs[0].Changes["widget"])
	}
	w0, _ := widgets[0].(map[string]any)
	if w0["key"] != "kpi" || w0["from"] != nil || w0["to"] != false {
		t.Fatalf("widget change = %v, want [{kpi from:null to:false}]", logs[0].Changes["widget"])
	}
}

func TestRoleMatrixSave_NoChangeNoLog(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	rv := rmMatrixRowVersion(t, router, token, rmRoleTarget)
	rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleTarget+"/matrix",
		rmSaveBody(rv, []string{rmPermB, rmPermA}, []map[string]any{}, "tanpa perubahan"))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := roleMatrixLogCount(t, ctx, pool, rmRoleTarget); n != 0 {
		t.Fatalf("no-op save must not log, got %d rows", n)
	}
	if got := rmMatrixRowVersion(t, router, token, rmRoleTarget); got != rv {
		t.Fatalf("no-op save must not touch the role: row_version %v → %v", rv, got)
	}
}

func TestRoleMatrixSave_StaleRowVersion409(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	rv := rmMatrixRowVersion(t, router, token, rmRoleTarget)
	path := "/api/v1/roles/" + rmRoleTarget + "/matrix"
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, path,
		rmSaveBody(rv, []string{rmPermA}, []map[string]any{}, "pertama")); rec.Code != http.StatusOK {
		t.Fatalf("first save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := roleMatrixRequest(t, router, token, http.MethodPut, path,
		rmSaveBody(rv, []string{rmPermA, rmPermB, rmPermC}, []map[string]any{}, "kedua basi"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale save expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := rmErrorOf(t, rec.Body.Bytes()); msg != "role was changed by someone else, reload and try again" {
		t.Fatalf("stale save error = %q", msg)
	}
	if n := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); n != 1 {
		t.Fatalf("stale save must not write: %d grants, want 1", n)
	}
	if n := roleMatrixLogCount(t, ctx, pool, rmRoleTarget); n != 1 {
		t.Fatalf("stale save must not log: %d rows, want 1", n)
	}
}

func TestRoleMatrixSave_SystemRoleDetailsLocked(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	path := "/api/v1/roles/" + rmRoleSystem + "/matrix"
	body := map[string]any{
		"row_version": rmMatrixRowVersion(t, router, token, rmRoleSystem), "name": "Matrix System Diubah",
		"description": "bawaan", "requires_physician_data": false,
		"permission_ids": []string{rmPermA}, "widgets": []map[string]any{}, "reason": "ubah bawaan",
	}
	rec := roleMatrixRequest(t, router, token, http.MethodPut, path, body)
	if rec.Code != http.StatusConflict || rmErrorOf(t, rec.Body.Bytes()) != "system role details cannot be changed" {
		t.Fatalf("system rename expected 409 system role details cannot be changed, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := roleMatrixPermCount(t, ctx, pool, rmRoleSystem); n != 0 {
		t.Fatalf("rejected save must not write: %d grants", n)
	}
	body["name"] = "Matrix System"
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("system permission save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := roleMatrixPermCount(t, ctx, pool, rmRoleSystem); n != 1 {
		t.Fatalf("system role grants = %d, want 1", n)
	}
	logs := roleMatrixLogChanges(t, ctx, pool, rmRoleSystem)
	if len(logs) != 1 || logs[0].Changes["meta"] != nil || logs[0].Changes["permission"] == nil {
		t.Fatalf("system role log = %+v, want one row with permission and no meta", logs)
	}
}

func TestRoleMatrixSave_EmptySetsClearEverything(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	body := map[string]any{
		"row_version": rmMatrixRowVersion(t, router, token, rmRoleDupSrc), "name": "Matrix Dup Source",
		"description": "source desc", "requires_physician_data": true,
		"permission_ids": []string{}, "widgets": []map[string]any{}, "reason": "kosongkan",
	}
	if rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleDupSrc+"/matrix", body); rec.Code != http.StatusOK {
		t.Fatalf("empty save expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := roleMatrixPermCount(t, ctx, pool, rmRoleDupSrc); n != 0 {
		t.Fatalf("grants after empty save = %d, want 0", n)
	}
	if n := roleMatrixWidgetCount(t, ctx, pool, rmRoleDupSrc); n != 0 {
		t.Fatalf("widget overrides after empty save = %d, want 0", n)
	}
	logs := roleMatrixLogChanges(t, ctx, pool, rmRoleDupSrc)
	if len(logs) != 1 {
		t.Fatalf("want 1 change-log row, got %d", len(logs))
	}
	perm, _ := logs[0].Changes["permission"].(map[string]any)
	if removed := toStrings(perm["removed"]); len(removed) != 5 {
		t.Fatalf("permission removed = %v, want 5 codes", perm["removed"])
	}
	widgets, _ := logs[0].Changes["widget"].([]any)
	if len(widgets) != 1 {
		t.Fatalf("widget change = %v, want exactly 1 entry", logs[0].Changes["widget"])
	}
	w0, _ := widgets[0].(map[string]any)
	if w0["key"] != "kpi" || w0["from"] != true || w0["to"] != nil {
		t.Fatalf("widget change = %v, want [{kpi from:true to:null}]", logs[0].Changes["widget"])
	}
}

func TestRoleMatrixSave_ValidationNoWrite(t *testing.T) {
	ctx, pool, router, token := rmSaveSetup(t)
	rv := rmMatrixRowVersion(t, router, token, rmRoleTarget)
	cases := []struct {
		name   string
		mutate func(map[string]any)
		code   int
	}{
		{"blank reason", func(b map[string]any) { b["reason"] = "   " }, http.StatusBadRequest},
		{"unknown permission", func(b map[string]any) {
			b["permission_ids"] = []string{rmPermA, "99999999-9999-9999-9999-999999999999"}
		}, http.StatusBadRequest},
		{"duplicate permission", func(b map[string]any) { b["permission_ids"] = []string{rmPermA, rmPermA} }, http.StatusBadRequest},
		{"unknown widget", func(b map[string]any) {
			b["widgets"] = []map[string]any{{"key": "bogus", "visible": true}}
		}, http.StatusBadRequest},
		{"duplicate widget", func(b map[string]any) {
			b["widgets"] = []map[string]any{{"key": "kpi", "visible": true}, {"key": "kpi", "visible": false}}
		}, http.StatusBadRequest},
		{"blank name", func(b map[string]any) { b["name"] = "  " }, http.StatusBadRequest},
		{"permission_ids absent", func(b map[string]any) { delete(b, "permission_ids") }, http.StatusBadRequest},
		{"row_version absent", func(b map[string]any) { delete(b, "row_version") }, http.StatusBadRequest},
		{"name taken", func(b map[string]any) { b["name"] = "Matrix Admin" }, http.StatusConflict},
	}
	for _, tc := range cases {
		body := rmSaveBody(rv, []string{rmPermA, rmPermB, rmPermC}, []map[string]any{{"key": "queue", "visible": false}}, "validasi")
		tc.mutate(body)
		rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleTarget+"/matrix", body)
		if rec.Code != tc.code {
			t.Fatalf("%s: expected %d, got %d: %s", tc.name, tc.code, rec.Code, rec.Body.String())
		}
	}
	if n := roleMatrixPermCount(t, ctx, pool, rmRoleTarget); n != 2 {
		t.Fatalf("failed saves must not write: %d grants, want 2", n)
	}
	if n := roleMatrixWidgetCount(t, ctx, pool, rmRoleTarget); n != 0 {
		t.Fatalf("failed saves must not write widgets: %d", n)
	}
	if n := roleMatrixLogCount(t, ctx, pool, rmRoleTarget); n != 0 {
		t.Fatalf("failed saves must not log: %d", n)
	}
	if got := rmMatrixRowVersion(t, router, token, rmRoleTarget); got != rv {
		t.Fatalf("failed saves must not touch the role: row_version %v → %v", rv, got)
	}
}

func TestRoleMatrixSave_PermissionBeforeLookup(t *testing.T) {
	_, _, router, _ := rmSaveSetup(t)
	noRole, err := auth.GenerateToken(testJWTSecret, rmUserNoRole, rmCompany, rmMerchant, "", "matrix.norole", "norole-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	for _, roleID := range []string{rmRoleTarget, rmMissingRole} {
		if rec := roleMatrixRequest(t, router, noRole, http.MethodGet, "/api/v1/roles/"+roleID+"/matrix", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("GET matrix %s without permission expected 403, got %d", roleID, rec.Code)
		}
		if rec := roleMatrixRequest(t, router, noRole, http.MethodPut, "/api/v1/roles/"+roleID+"/matrix",
			rmSaveBody(1, []string{}, []map[string]any{}, "x")); rec.Code != http.StatusForbidden {
			t.Fatalf("PUT matrix %s without permission expected 403, got %d", roleID, rec.Code)
		}
		if rec := roleMatrixRequest(t, router, noRole, http.MethodPost, "/api/v1/roles/"+roleID+"/duplicate",
			map[string]any{"name": "x"}); rec.Code != http.StatusForbidden {
			t.Fatalf("duplicate %s without permission expected 403, got %d", roleID, rec.Code)
		}
	}
}

func TestRoleMatrixSave_AuditLimitAndOldEndpoints(t *testing.T) {
	_, _, router, token := rmSaveSetup(t)
	path := "/api/v1/roles/" + rmRoleTarget + "/matrix"
	for _, perms := range [][]string{{rmPermA}, {rmPermA, rmPermB}} {
		if rec := roleMatrixRequest(t, router, token, http.MethodPut, path,
			rmSaveBody(rmMatrixRowVersion(t, router, token, rmRoleTarget), perms, []map[string]any{}, "riwayat")); rec.Code != http.StatusOK {
			t.Fatalf("save expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct {
		query     string
		wantLimit float64
		wantRows  int
	}{{"?limit=1000000", 100, 2}, {"?limit=0", 1, 1}, {"", 20, 2}} {
		rec := roleMatrixRequest(t, router, token, http.MethodGet, "/api/v1/roles/"+rmRoleTarget+"/audit"+tc.query, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("audit%s expected 200, got %d: %s", tc.query, rec.Code, rec.Body.String())
		}
		var audit struct {
			Data []map[string]any `json:"data"`
			Meta map[string]any   `json:"meta"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
			t.Fatalf("decode audit: %v", err)
		}
		if audit.Meta["limit"] != tc.wantLimit || len(audit.Data) != tc.wantRows {
			t.Fatalf("audit%s: limit %v rows %d, want %v and %d", tc.query, audit.Meta["limit"], len(audit.Data), tc.wantLimit, tc.wantRows)
		}
	}
	for _, old := range []string{"/permissions", "/widgets"} {
		rec := roleMatrixRequest(t, router, token, http.MethodPut, "/api/v1/roles/"+rmRoleTarget+old,
			map[string]any{"reason": "lama"})
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("removed PUT %s expected 404/405, got %d", old, rec.Code)
		}
	}
	if rec := roleMatrixRequest(t, router, token, http.MethodPost, "/api/v1/roles/"+rmRoleTarget+"/duplicate",
		map[string]any{"name": "   "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank duplicate name expected 400, got %d", rec.Code)
	}
}
