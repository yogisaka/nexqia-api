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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedActiveRole creates a role named `name` at companyID, assigns it to userID
// at merchantID, and grants it exactly perms. Returns the role id.
func seedActiveRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID, name string, perms ...string) string {
	t.Helper()
	var roleID string
	if err := pool.QueryRow(ctx, "INSERT INTO core.role (company_id, name) VALUES ($1, $2) RETURNING id", companyID, name).Scan(&roleID); err != nil {
		t.Fatalf("seed role %s: %v", name, err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", userID, merchantID, roleID); err != nil {
		t.Fatalf("assign role %s: %v", name, err)
	}
	for _, code := range perms {
		if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, code); err != nil {
			t.Fatalf("grant %s to %s: %v", code, name, err)
		}
	}
	return roleID
}

// seedActiveUnassignedRole creates a role without assigning it to anyone.
func seedActiveUnassignedRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, name string) string {
	t.Helper()
	var roleID string
	if err := pool.QueryRow(ctx, "INSERT INTO core.role (company_id, name) VALUES ($1, $2) RETURNING id", companyID, name).Scan(&roleID); err != nil {
		t.Fatalf("seed unassigned role %s: %v", name, err)
	}
	return roleID
}

// softDeleteRole marks the role deleted — default_user_role/user_role_assigned
// and UserHasPermission all exclude deleted roles.
func softDeleteRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, roleID string) {
	t.Helper()
	// chk_role_soft_delete demands deleted_at/deleted_by set together.
	if _, err := pool.Exec(ctx, "UPDATE core.role SET deleted_at = now(), deleted_by = '00000000-0000-0000-0000-000000000001' WHERE id = $1", roleID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
}

// ownerRoleID resolves the "Owner" role the registration bootstrap created.
func ownerRoleID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.role WHERE company_id = $1 AND name = 'Owner'", companyID).Scan(&id); err != nil {
		t.Fatalf("resolve Owner role: %v", err)
	}
	return id
}

// switchRole posts POST /auth/switch-role with the refresh cookie.
func switchRole(router *gin.Engine, token, companyID, merchantID, roleID string, cookie *http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"role_id": roleID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/switch-role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// tokenRoleID parses the "rid" claim off an access token.
func tokenRoleID(t *testing.T, token string) string {
	t.Helper()
	claims, err := auth.ParseToken(testJWTSecret, token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	return claims.RoleID
}

// dataStringMap decodes the response's data object.
func dataStringMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodePlatformData(t, rec)
}

// permissionCodesOf GETs /users/:id/permissions and returns the codes array.
func permissionCodesOf(t *testing.T, router *gin.Engine, token, companyID, merchantID, userID string) []string {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/users/"+userID+"/permissions?merchant_id="+merchantID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions endpoint expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode permissions response: %v", err)
	}
	return resp.Data
}

// TestActiveRole_LimitsPermissions — with the Perawat role active the user
// loses core.user.manage (GET /users 403); switching back to Owner restores it
// (200). Perawat is seeded WITHOUT core.user.manage; Owner has it.
func TestActiveRole_LimitsPermissions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, cookie := seedAccountOwner(t, ctx, pool, router, "actrole.limit", "081234080001")
	nurse := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Perawat")
	owner := ownerRoleID(t, ctx, pool, companyID)

	rec := switchRole(router, token, companyID, merchantID, nurse, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch-role to Perawat expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := dataStringMap(t, rec)["active_role_id"]; got != nurse {
		t.Fatalf("active_role_id = %v, want %s", got, nurse)
	}
	// Requests after the switch must carry the REISSUED token: only it has
	// rid=Perawat. The pre-switch token has no rid and falls back to the
	// default role (Owner) — contract TestActiveRole_TokenWithoutRidUsesDefault.
	nurseToken := dataStringMap(t, rec)["token"].(string)
	if rec = accountRequest(router, http.MethodGet, "/api/v1/users", nurseToken, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /users as Perawat expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = switchRole(router, token, companyID, merchantID, owner, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch-role back to Owner expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := dataStringMap(t, rec)["active_role_id"]; got != owner {
		t.Fatalf("active_role_id = %v, want %s", got, owner)
	}
	ownerToken := dataStringMap(t, rec)["token"].(string)
	if rec = accountRequest(router, http.MethodGet, "/api/v1/users", ownerToken, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /users as Owner expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_SwitchRole — happy path reissues a token whose rid is the
// chosen role; an unassigned role, a deleted role and a malformed role_id are
// rejected with 403/403/400 respectively.
func TestActiveRole_SwitchRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, cookie := seedAccountOwner(t, ctx, pool, router, "actrole.switch", "081234080011")
	nurse := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Perawat")
	ghost := seedActiveUnassignedRole(t, ctx, pool, companyID, "Dokter")

	rec := switchRole(router, token, companyID, merchantID, nurse, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch-role expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	newToken := dataStringMap(t, rec)["token"].(string)
	if got := tokenRoleID(t, newToken); got != nurse {
		t.Fatalf("token rid = %q, want %s", got, nurse)
	}
	if rec = accountRequest(router, http.MethodGet, "/api/v1/users", newToken, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /users as Perawat expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Role exists at the company but is NOT assigned to this user → 403.
	if rec = switchRole(router, token, companyID, merchantID, ghost, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("switch to unassigned role expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Deleting the Perawat role revokes the assignment → 403.
	softDeleteRole(t, ctx, pool, nurse)
	if rec = switchRole(router, token, companyID, merchantID, nurse, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("switch to deleted role expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Garbage role_id → 400 (parse failure, before any session check).
	if rec = switchRole(router, token, companyID, merchantID, "not-a-uuid", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("switch with malformed role_id expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_RefreshKeepsRole — the chosen role survives refresh rotation
// (active_role_id is stored on the refresh-token row).
func TestActiveRole_RefreshKeepsRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, cookie := seedAccountOwner(t, ctx, pool, router, "actrole.refkeep", "081234080021")
	nurse := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Perawat")
	if rec := switchRole(router, token, companyID, merchantID, nurse, cookie); rec.Code != http.StatusOK {
		t.Fatalf("switch-role expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("X-Company-ID", companyID)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := dataStringMap(t, rec)
	if data["active_role_id"] != nurse {
		t.Fatalf("refresh active_role_id = %v, want %s", data["active_role_id"], nurse)
	}
	if got := tokenRoleID(t, data["token"].(string)); got != nurse {
		t.Fatalf("refreshed token rid = %q, want %s", got, nurse)
	}
	if rec = accountRequest(router, http.MethodGet, "/api/v1/users", data["token"].(string), companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /users after refresh expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_RefreshFallsBackWhenRevoked — deleting the stored active role
// makes the next refresh fall back to the default role (here Owner, the only
// remaining assignment) instead of keeping a dangling rid.
func TestActiveRole_RefreshFallsBackWhenRevoked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, cookie := seedAccountOwner(t, ctx, pool, router, "actrole.revfell", "081234080031")
	nurse := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Perawat")
	if rec := switchRole(router, token, companyID, merchantID, nurse, cookie); rec.Code != http.StatusOK {
		t.Fatalf("switch-role expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	softDeleteRole(t, ctx, pool, nurse)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("X-Company-ID", companyID)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Default = first assigned, non-deleted role by name: Owner is the only one left.
	owner := ownerRoleID(t, ctx, pool, companyID)
	data := dataStringMap(t, rec)
	if data["active_role_id"] != owner {
		t.Fatalf("refresh active_role_id = %v, want default %s", data["active_role_id"], owner)
	}
	if rec = accountRequest(router, http.MethodGet, "/api/v1/users", data["token"].(string), companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /users after fallback expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_TokenWithoutRidUsesDefault — a legacy token without "rid"
// gets the DEFAULT role (first assigned non-deleted by name), not the union.
// Fixture: Owner (all perms) + Astaff (no perms). lower("astaff") <
// lower("owner"), so default = Astaff → GET /users must be 403 even though
// the union across roles would allow it.
func TestActiveRole_TokenWithoutRidUsesDefault(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "actrole.norid", "081234080041")
	seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Astaff") // sorts before "Owner"

	legacyToken, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "actrole.norid", "actrole-norid-device", time.Hour)
	if err != nil {
		t.Fatalf("generate legacy token: %v", err)
	}
	rec := accountRequest(router, http.MethodGet, "/api/v1/users", legacyToken, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /users with default-role-only perms expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_ImpersonationUsesDefaultRole — impersonation tokens carry no
// mid/rid, so permission checks use the target's DEFAULT role at the merchant
// named in X-Merchant-ID: not a blanket 403, and not the union.
// Fixture A: owner + Astaff (no perms) — default = Astaff → 403.
// Fixture B: staff with a single role holding core.user.manage → default = that
// role → 200.
func TestActiveRole_ImpersonationUsesDefaultRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, ownerID, _, _ := seedAccountOwner(t, ctx, pool, router, "actrole.imp", "081234080051")
	seedActiveRole(t, ctx, pool, companyID, merchantID, ownerID, "Astaff")
	impOwner := impersonationTokenNoMerchant(t, ownerID, companyID)
	if rec := accountRequest(router, http.MethodGet, "/api/v1/users", impOwner, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /users as impersonated owner (default Astaff) expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	staffID := seedMerchantStaff(t, ctx, pool, companyID, merchantID, "actrole.imp.staff", "", "core.user.manage")
	impStaff := impersonationTokenNoMerchant(t, staffID, companyID)
	if rec := accountRequest(router, http.MethodGet, "/api/v1/users", impStaff, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /users as impersonated staff (default role has the perm) expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// impersonationTokenNoMerchant issues an impersonation access token with an
// empty merchant claim — the shape POST /platform/impersonate produces (the
// platform admin impersonates the USER, not one of their merchants).
func impersonationTokenNoMerchant(t *testing.T, userID, companyID string) string {
	t.Helper()
	token, err := auth.GenerateImpersonationToken(testJWTSecret, userID, companyID, "", "platform-admin", "impersonator-device", "00000000-0000-0000-0000-000000000001", time.Hour)
	if err != nil {
		t.Fatalf("GenerateImpersonationToken: %v", err)
	}
	return token
}

// TestActiveRole_MFAStillUnion — the mandatory-TOTP rule keeps using the UNION
// of permissions (mfa.go mfaRequired is deliberately not role-scoped): the
// owner's default/active role is now Kasir ("kasir" < "owner" by name), yet
// login after the grace window still demands TOTP setup because Owner — held
// by the same user — carries admin permissions.
func TestActiveRole_MFAStillUnion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "actrole.mfa", "081234080061")
	seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Kasir") // becomes the default active role
	if _, err := pool.Exec(ctx, "UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", companyID); err != nil {
		t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	expireTenantMFAGrace(t, ctx, pool, "actrole.mfa")

	rec := tenantLogin(t, router, companyID, "actrole.mfa", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := dataStringMap(t, rec)
	if data["mfa_setup_required"] != true {
		t.Fatalf("expected mfa_setup_required despite non-admin active role, got %+v", data)
	}
}

// TestActiveRole_PermissionsEndpointSelf — the self-access permissions endpoint
// reports only the ACTIVE role's codes; an admin viewing ANOTHER user still
// gets the union across all their roles.
// Fixture: staff role "actrole.perm.staff" holds audit.log.view, added role
// "Zadmin" holds core.user.manage. Self token rid = staff role → exactly
// ["audit.log.view"]; union = ["audit.log.view", "core.user.manage"] (sorted).
func TestActiveRole_PermissionsEndpointSelf(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, ownerToken, _ := seedAccountOwner(t, ctx, pool, router, "actrole.perm", "081234080071")
	staffID := seedMerchantStaff(t, ctx, pool, companyID, merchantID, "actrole.perm.staff", "", "audit.log.view")
	var staffRoleID string
	if err := pool.QueryRow(ctx, "SELECT role_id FROM core.user_merchant_role umr JOIN core.app_user u ON u.id = umr.user_id WHERE u.username = 'actrole.perm.staff'").Scan(&staffRoleID); err != nil {
		t.Fatalf("resolve staff role: %v", err)
	}
	seedActiveRole(t, ctx, pool, companyID, merchantID, staffID, "Zadmin", "core.user.manage")

	staffToken, err := auth.GenerateToken(testJWTSecret, staffID, companyID, merchantID, staffRoleID, "actrole.perm.staff", "actrole-perm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate staff token: %v", err)
	}
	self := permissionCodesOf(t, router, staffToken, companyID, merchantID, staffID)
	if len(self) != 1 || self[0] != "audit.log.view" {
		t.Fatalf("self codes = %v, want [audit.log.view] (active role only)", self)
	}

	union := permissionCodesOf(t, router, ownerToken, companyID, merchantID, staffID)
	want := []string{"audit.log.view", "core.user.manage"}
	if !equalStrings(union, want) {
		t.Fatalf("admin-view codes = %v, want union %v", union, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// TestActiveRole_CompanyLevelUsesActiveRole — company-level checks follow the
// active role too: PATCH /companies/:id (core.company.manage.own OR
// core.company.manage) passes under Owner but 403s once Kasir — which holds
// neither permission — is active.
func TestActiveRole_CompanyLevelUsesActiveRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, cookie := seedAccountOwner(t, ctx, pool, router, "actrole.colevel", "081234080081")
	kasir := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Kasir")

	body := []byte(`{"name":"Renamed Co","is_active":true}`)
	if rec := accountRequest(router, http.MethodPatch, "/api/v1/companies/"+companyID, token, companyID, merchantID, body); rec.Code != http.StatusOK {
		t.Fatalf("PATCH /companies as Owner expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	switchRec := switchRole(router, token, companyID, merchantID, kasir, cookie)
	if switchRec.Code != http.StatusOK {
		t.Fatalf("switch-role to Kasir expected 200, got %d: %s", switchRec.Code, switchRec.Body.String())
	}
	// Use the reissued token: it carries rid=Kasir; the pre-switch token has no
	// rid and would fall back to the default role (Owner) → wrongly 200.
	kasirToken := dataStringMap(t, switchRec)["token"].(string)
	if rec := accountRequest(router, http.MethodPatch, "/api/v1/companies/"+companyID, kasirToken, companyID, merchantID, body); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH /companies as Kasir expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestActiveRole_RuntimeFunctions — core.default_user_role and
// core.user_role_assigned are SECURITY DEFINER precisely so app_runtime can
// call them WITHOUT app.current_merchant_id set (login/refresh run before any
// merchant context exists). runtimeTx sets the company GUC only.
func TestActiveRole_RuntimeFunctions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	companyID := "71717171-7171-7171-7171-717171717171"
	merchantID := "72727272-7272-7272-7272-727272727272"
	userID := "73737373-7373-7373-7373-737373737373"
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'actrole-rt', 'Active Role RT')", companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'rt-merchant', 'RT Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'actrole.rt', $3, true)", userID, companyID, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	roleA := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Astaff")
	roleB := seedActiveRole(t, ctx, pool, companyID, merchantID, userID, "Zadmin")
	ghost := seedActiveUnassignedRole(t, ctx, pool, companyID, "Hantu")

	tx := runtimeTx(t, ctx, pool, companyID)

	// default = first assigned non-deleted role by name: "astaff" < "zadmin" → roleA.
	var def string
	if err := tx.QueryRow(ctx, "SELECT core.default_user_role($1, $2)", userID, merchantID).Scan(&def); err != nil {
		t.Fatalf("default_user_role: %v", err)
	}
	if def != roleA {
		t.Fatalf("default_user_role = %s, want %s (Astaff sorts before Zadmin)", def, roleA)
	}

	var assigned bool
	if err := tx.QueryRow(ctx, "SELECT core.user_role_assigned($1, $2, $3)", userID, merchantID, roleB).Scan(&assigned); err != nil {
		t.Fatalf("user_role_assigned: %v", err)
	}
	if !assigned {
		t.Fatal("user_role_assigned(Zadmin) = false, want true")
	}
	if err := tx.QueryRow(ctx, "SELECT core.user_role_assigned($1, $2, $3)", userID, merchantID, ghost).Scan(&assigned); err != nil {
		t.Fatalf("user_role_assigned: %v", err)
	}
	if assigned {
		t.Fatal("user_role_assigned(unassigned role) = true, want false")
	}
}
