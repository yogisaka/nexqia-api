//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// Spec 2026-10-06-role-matrix-fixes §2.3/§2.5/§2.5b: GET /roles keeps every
// core.role field; self permissions and self widgets follow the effective
// role (active role, else the default role — never the union).

func TestRoleMatrixRolesListFullShape(t *testing.T) {
	ctx := context.Background()
	pool, _ := roleMatrixPool(t, ctx)
	roleMatrixSeed(t, ctx, pool)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
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
		t.Fatalf("decode roles: %v", err)
	}
	for _, item := range list.Data {
		if item["Name"] != "Matrix Admin" {
			continue
		}
		for _, field := range []string{"ID", "CompanyID", "Name", "Description", "IsSystem", "CreatedAt", "CreatedBy",
			"UpdatedAt", "UpdatedBy", "DeletedAt", "DeletedBy", "RowVersion", "RequiresPhysicianData", "UserCount", "PermissionCount"} {
			if _, ok := item[field]; !ok {
				t.Fatalf("field %s missing on role item: %v", field, item)
			}
		}
		if item["UserCount"] != float64(1) || item["PermissionCount"] != float64(1) {
			t.Fatalf("counts = %v/%v, want 1/1", item["UserCount"], item["PermissionCount"])
		}
		return
	}
	t.Fatal("Matrix Admin missing from GET /roles")
}

// effectiveRoleFixture: staff user with role "rmfix.staff" (audit.log.view,
// sorts before "Zadmin" → the default role) and role "Zadmin"
// (core.user.manage). Returns router, ids and a staff token WITHOUT rid.
func effectiveRoleFixture(t *testing.T, label, phone string) (router *gin.Engine, companyID, merchantID, staffID, staffRoleID, ownerToken, legacyToken string) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router = server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, ownerToken, _ = seedAccountOwner(t, ctx, pool, router, label, phone)
	staffID = seedMerchantStaff(t, ctx, pool, companyID, merchantID, "rmfix.staff", "", "audit.log.view")
	if err := pool.QueryRow(ctx,
		"SELECT umr.role_id FROM core.user_merchant_role umr WHERE umr.user_id = $1", staffID).Scan(&staffRoleID); err != nil {
		t.Fatalf("resolve staff role: %v", err)
	}
	seedActiveRole(t, ctx, pool, companyID, merchantID, staffID, "Zadmin", "core.user.manage")
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.role_widget (role_id, widget_key, visible) VALUES ($1, 'kpi', false)", staffRoleID); err != nil {
		t.Fatalf("seed staff widget override: %v", err)
	}
	token, err := auth.GenerateToken(testJWTSecret, staffID, companyID, merchantID, "", "rmfix.staff", "rmfix-device", time.Hour)
	if err != nil {
		t.Fatalf("generate legacy token: %v", err)
	}
	return router, companyID, merchantID, staffID, staffRoleID, ownerToken, token
}

func TestEffectiveRole_SelfPermissionsWithoutRidUseDefault(t *testing.T) {
	router, companyID, merchantID, staffID, _, ownerToken, legacyToken := effectiveRoleFixture(t, "rmfix.perm", "081234080091")
	self := permissionCodesOf(t, router, legacyToken, companyID, merchantID, staffID)
	if len(self) != 1 || self[0] != "audit.log.view" {
		t.Fatalf("self codes without rid = %v, want [audit.log.view] (default role only)", self)
	}
	union := permissionCodesOf(t, router, ownerToken, companyID, merchantID, staffID)
	if !equalStrings(union, []string{"audit.log.view", "core.user.manage"}) {
		t.Fatalf("admin view = %v, want the union", union)
	}
}

func TestEffectiveRole_SelfWidgets(t *testing.T) {
	router, companyID, merchantID, staffID, _, ownerToken, legacyToken := effectiveRoleFixture(t, "rmfix.widget", "081234080092")
	path := "/api/v1/users/" + staffID + "/widgets?merchant_id=" + merchantID
	rec := accountRequest(router, http.MethodGet, path, legacyToken, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("self widgets expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			Key     string `json:"key"`
			Visible bool   `json:"visible"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode widgets: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Key != "kpi" || resp.Data[0].Visible {
		t.Fatalf("self widgets without rid = %+v, want the default role's [kpi:false]", resp.Data)
	}
	if rec := accountRequest(router, http.MethodGet, path, ownerToken, companyID, merchantID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's widgets expected 404, got %d", rec.Code)
	}
	other := "/api/v1/users/" + staffID + "/widgets?merchant_id=00000000-0000-0000-0000-0000000000ee"
	if rec := accountRequest(router, http.MethodGet, other, legacyToken, companyID, merchantID, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("merchant mismatch expected 400, got %d", rec.Code)
	}
}
