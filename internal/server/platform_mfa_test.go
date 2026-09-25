//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedPlatformMFAAdmin seeds a platform admin (password correct-horse). With
// withPermission the admin also gets a role holding platform.admin.manage.
// Permission rows use ON CONFLICT (code) DO NOTHING so several tests can call
// this helper with their own literal IDs.
func seedPlatformMFAAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username, adminID, roleID, permID string, withPermission bool) {
	t.Helper()
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.admin.manage', 'test', 'platform') ON CONFLICT (code) DO NOTHING", permID); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	var resolvedPermID string
	if err := pool.QueryRow(ctx, "SELECT id FROM platform.permission WHERE code = 'platform.admin.manage'").Scan(&resolvedPermID); err != nil {
		t.Fatalf("resolve permission id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, $2, true)", roleID, "MFA Test Role "+username); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if withPermission {
		if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, resolvedPermID); err != nil {
			t.Fatalf("seed role_permission: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, $2, $2 || '@nexqia.internal', 'Test', $3, true)", adminID, username, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if withPermission {
		if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, roleID); err != nil {
			t.Fatalf("seed admin_user_role: %v", err)
		}
	}
}

// expirePlatformMFAGrace backdates the admin's grace window so the next login
// must return mfa_setup_required.
func expirePlatformMFAGrace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE platform.admin_user SET mfa_grace_until = now() - interval '1 minute' WHERE username = $1", username); err != nil {
		t.Fatalf("expire grace: %v", err)
	}
}

// platformLogin posts to /platform/login; totpCode "" means the field is omitted.
func platformLogin(t *testing.T, router *gin.Engine, username, password, totpCode string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]string{"username": username, "password": password}
	if totpCode != "" {
		payload["totp_code"] = totpCode
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "platform-mfa-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodePlatformData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.Data
}

// enrollPlatformMFA finishes the mandatory enrollment for an admin whose grace
// window already ended: login (mfa_setup_required) → enroll. Returns the
// enrolled TOTP secret and the one-time recovery codes.
func enrollPlatformMFA(t *testing.T, router *gin.Engine, username string) (secret string, codes []string) {
	t.Helper()
	rec := platformLogin(t, router, username, "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	token, _ := data["enrollment_token"].(string)
	secret, _ = data["secret"].(string)
	if token == "" || secret == "" {
		t.Fatalf("expected mfa_setup_required payload, got %+v", data)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_token": token, "secret": secret, "code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("enroll expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	enrolled := decodePlatformData(t, rec2)
	raw, ok := enrolled["recovery_codes"].([]any)
	if !ok || len(raw) != 10 {
		t.Fatalf("expected 10 recovery codes, got %+v", enrolled)
	}
	codes = make([]string, len(raw))
	for i, c := range raw {
		codes[i], _ = c.(string)
	}
	return secret, codes
}

func TestPlatformMFA_FirstLoginStartsGrace(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	seedPlatformMFAAdmin(t, ctx, pool, "graceadmin",
		"61616161-6161-6161-6161-616161616161",
		"61616161-a001-a001-a001-616161616161",
		"61616161-b001-b001-b001-616161616161", true)

	rec := platformLogin(t, router, "graceadmin", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["token"] == "" {
		t.Fatalf("expected a token on first login within grace, got %+v", data)
	}
	dueAt, ok := data["mfa_setup_due_at"].(string)
	if !ok || dueAt == "" {
		t.Fatalf("expected mfa_setup_due_at in login response, got %+v", data)
	}
	parsed, err := time.Parse(time.RFC3339, dueAt)
	if err != nil {
		t.Fatalf("parse mfa_setup_due_at %q: %v", dueAt, err)
	}
	week := 7 * 24 * time.Hour
	if d := time.Until(parsed); d < week-time.Minute || d > week+time.Minute {
		t.Fatalf("expected mfa_setup_due_at ≈ now+7d, got %v (diff %v)", parsed, d)
	}
	var graceUntil time.Time
	if err := pool.QueryRow(ctx, "SELECT mfa_grace_until FROM platform.admin_user WHERE username = 'graceadmin'").Scan(&graceUntil); err != nil {
		t.Fatalf("read mfa_grace_until: %v", err)
	}
	if d := time.Until(graceUntil); d < week-time.Minute || d > week+time.Minute {
		t.Fatalf("expected DB mfa_grace_until ≈ now+7d, got %v (diff %v)", graceUntil, d)
	}
}

func TestPlatformMFA_ExpiredGraceRequiresSetup(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	seedPlatformMFAAdmin(t, ctx, pool, "expiredadmin",
		"62626262-6262-6262-6262-626262626262",
		"62626262-a001-a001-a001-626262626262",
		"62626262-b001-b001-b001-626262626262", false)
	expirePlatformMFAGrace(t, ctx, pool, "expiredadmin")

	rec := platformLogin(t, router, "expiredadmin", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["mfa_setup_required"] != true {
		t.Fatalf("expected mfa_setup_required, got %+v", data)
	}
	for _, key := range []string{"enrollment_token", "secret", "otpauth_uri", "qr_png"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("expected %q in setup payload, got %+v", key, data)
		}
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("expected NO Set-Cookie on mfa_setup_required, got %v", cookies)
	}
	var lastLogin pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT last_login_at FROM platform.admin_user WHERE username = 'expiredadmin'").Scan(&lastLogin); err != nil {
		t.Fatalf("read last_login_at: %v", err)
	}
	if lastLogin.Valid {
		t.Fatalf("expected last_login_at to stay NULL for the setup-only response")
	}
}

func TestPlatformMFA_EnrollThenLoginNeedsCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	seedPlatformMFAAdmin(t, ctx, pool, "enrolladmin",
		"63636363-6363-6363-6363-636363636363",
		"63636363-a001-a001-a001-636363636363",
		"63636363-b001-b001-b001-636363636363", false)
	expirePlatformMFAGrace(t, ctx, pool, "enrolladmin")

	secret, codes := enrollPlatformMFA(t, router, "enrolladmin")
	if len(codes) != 10 {
		t.Fatalf("expected 10 recovery codes, got %d", len(codes))
	}

	// Login without a code → totp_required, no token.
	rec := platformLogin(t, router, "enrolladmin", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login without code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["totp_required"] != true {
		t.Fatalf("expected totp_required, got %+v", data)
	}

	// Login with a valid TOTP code → token.
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	rec2 := platformLogin(t, router, "enrolladmin", "correct-horse", code)
	if rec2.Code != http.StatusOK {
		t.Fatalf("login with code expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if decodePlatformData(t, rec2)["token"] == "" {
		t.Fatalf("expected token after valid code, got %+v", decodePlatformData(t, rec2))
	}
}

func TestPlatformMFA_EnrollRejectsTenantPurposeToken(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "64646464-6464-6464-6464-646464646464"
	seedPlatformMFAAdmin(t, ctx, pool, "wrongtokenadmin",
		adminID, "64646464-a001-a001-a001-646464646464", "64646464-b001-b001-b001-646464646464", false)

	// Tenant-purpose token must NOT work on the platform enroll endpoint.
	token, err := auth.GenerateMFAEnrollmentToken(testJWTSecret, adminID, auth.MFAEnrollmentPurpose, 10*time.Minute)
	if err != nil {
		t.Fatalf("generate tenant enrollment token: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_token": token, "secret": "JBSWY3DPEHPK3PXP", "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformMFA_EnrollTwiceConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "65656565-6565-6565-6565-656565656565"
	seedPlatformMFAAdmin(t, ctx, pool, "twiceadmin",
		adminID, "65656565-a001-a001-a001-656565656565", "65656565-b001-b001-b001-656565656565", false)
	expirePlatformMFAGrace(t, ctx, pool, "twiceadmin")

	if _, codes := enrollPlatformMFA(t, router, "twiceadmin"); len(codes) != 10 {
		t.Fatalf("first enroll expected 10 codes, got %d", len(codes))
	}

	// Already enrolled → 409 even with a fresh, valid platform-purpose token.
	token, err := auth.GenerateMFAEnrollmentToken(testJWTSecret, adminID, auth.PlatformMFAEnrollmentPurpose, 10*time.Minute)
	if err != nil {
		t.Fatalf("generate platform enrollment token: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_token": token, "secret": "JBSWY3DPEHPK3PXP", "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformMFA_RecoveryCodeWorksOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	seedPlatformMFAAdmin(t, ctx, pool, "recoveryadmin",
		"67676767-6767-6767-6767-676767676767",
		"67676767-a001-a001-a001-676767676767",
		"67676767-b001-b001-b001-676767676767", false)
	expirePlatformMFAGrace(t, ctx, pool, "recoveryadmin")

	_, codes := enrollPlatformMFA(t, router, "recoveryadmin")

	rec := platformLogin(t, router, "recoveryadmin", "correct-horse", codes[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("login with recovery code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodePlatformData(t, rec)["token"] == "" {
		t.Fatalf("expected token when logging in with a recovery code")
	}

	rec2 := platformLogin(t, router, "recoveryadmin", "correct-horse", codes[0])
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed recovery code expected 401, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestPlatformMFA_ResetForcesEnrollmentAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	callerID := "6a6a6a6a-6a6a-6a6a-6a6a-6a6a6a6a6a6a"
	targetID := "6b6b6b6b-6b6b-6b6b-6b6b-6b6b6b6b6b6b"
	seedPlatformMFAAdmin(t, ctx, pool, "resetadmin",
		callerID, "6a6a6a6a-a001-a001-a001-6a6a6a6a6a6a", "6a6a6a6a-b001-b001-b001-6a6a6a6a6a6a", true)
	seedPlatformMFAAdmin(t, ctx, pool, "resettarget",
		targetID, "6b6b6b6b-a001-a001-a001-6b6b6b6b6b6b", "6b6b6b6b-b001-b001-b001-6b6b6b6b6b6b", false)
	expirePlatformMFAGrace(t, ctx, pool, "resettarget")

	secret, _ := enrollPlatformMFA(t, router, "resettarget")
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}

	// Target logs in (valid code) and holds a live refresh-token session.
	targetLogin := platformLogin(t, router, "resettarget", "correct-horse", code)
	if targetLogin.Code != http.StatusOK {
		t.Fatalf("target login expected 200, got %d: %s", targetLogin.Code, targetLogin.Body.String())
	}
	var refreshCookie *http.Cookie
	for _, ck := range targetLogin.Result().Cookies() {
		if ck.Name == "platform_refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected platform_refresh_token cookie from target login")
	}

	callerToken, err := auth.GeneratePlatformAdminToken(testJWTSecret, callerID, "resetadmin", "reset-caller-device", time.Hour)
	if err != nil {
		t.Fatalf("generate caller token: %v", err)
	}

	resetReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/admin-users/"+targetID+"/mfa/reset", nil)
	resetReq.Header.Set("Authorization", "Bearer "+callerToken)
	resetRec := httptest.NewRecorder()
	router.ServeHTTP(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset expected 200, got %d: %s", resetRec.Code, resetRec.Body.String())
	}

	// The target's live session must be revoked.
	refreshReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/refresh", nil)
	refreshReq.AddCookie(refreshCookie)
	refreshRec := httptest.NewRecorder()
	router.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusUnauthorized {
		t.Fatalf("target refresh after reset expected 401, got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}

	// Next target login must force enrollment again (secret cleared, grace
	// backdated to now by the reset query).
	rec := platformLogin(t, router, "resettarget", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("target post-reset login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodePlatformData(t, rec)["mfa_setup_required"] != true {
		t.Fatalf("expected mfa_setup_required after reset, got %+v", decodePlatformData(t, rec))
	}
}

func TestPlatformMFA_ResetSelfRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "6c6c6c6c-6c6c-6c6c-6c6c-6c6c6c6c6c6c"
	seedPlatformMFAAdmin(t, ctx, pool, "selfresetadmin",
		adminID, "6c6c6c6c-a001-a001-a001-6c6c6c6c6c6c", "6c6c6c6c-b001-b001-b001-6c6c6c6c6c6c", true)

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "selfresetadmin", "self-reset-device", time.Hour)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/admin-users/"+adminID+"/mfa/reset", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self reset, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformMFA_ResetWithoutPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// Deliberately NO role/permission.
	adminID := "6d6d6d6d-6d6d-6d6d-6d6d-6d6d6d6d6d6d"
	seedPlatformMFAAdmin(t, ctx, pool, "nopermresetadmin",
		adminID, "6d6d6d6d-a001-a001-a001-6d6d6d6d6d6d", "6d6d6d6d-b001-b001-b001-6d6d6d6d6d6d", false)

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "nopermresetadmin", "no-perm-reset-device", time.Hour)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/admin-users/00000000-0000-0000-0000-000000000000/mfa/reset", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (missing platform.admin.manage), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformMFA_CreateAdminResponseHasNoPasswordHash(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	callerID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e6e"
	newRoleID := "6e6e6e6e-a001-a001-a001-6e6e6e6e6e6e"
	seedPlatformMFAAdmin(t, ctx, pool, "createadmin",
		callerID, "6e6e6e6e-b001-b001-b001-6e6e6e6e6e6e", "6e6e6e6e-c001-c001-c001-6e6e6e6e6e6e", true)
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Create Admin Target Role', true)", newRoleID); err != nil {
		t.Fatalf("seed target role: %v", err)
	}

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, callerID, "createadmin", "create-admin-device", time.Hour)
	if err != nil {
		t.Fatalf("generate caller token: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"username": "mfaNewAdmin", "email": "mfaNewAdmin@nexqia.internal",
		"full_name": "New Admin", "password": "Passw0rd123!", "role_id": newRoleID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/admin-users", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "PasswordHash") || strings.Contains(raw, "password_hash") {
		t.Fatalf("create-admin response leaks the password hash: %s", raw)
	}
	data := decodePlatformData(t, rec)
	for _, key := range []string{"id", "username", "email", "full_name"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("expected %q in create-admin response, got %+v", key, data)
		}
	}
}
