//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// registerTenantMFAOwner registers a company + Owner via the real endpoint and
// returns the company id, user id, access token and refresh cookie.
func registerTenantMFAOwner(t *testing.T, router *gin.Engine, username, phone string) (companyID, userID, token string, refreshCookie *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload(username, username+"@example.com", phone)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "tenant-mfa-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			CompanyID string `json:"company_id"`
			UserID    string `json:"user_id"`
			Token     string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	refreshCookie = cookieNamed(t, rec, "refresh_token")
	return resp.Data.CompanyID, resp.Data.UserID, resp.Data.Token, refreshCookie
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	t.Fatalf("expected %q cookie in response", name)
	return nil
}

// createTenantMerchant creates a merchant through the real endpoint and
// resolves its id from the DB (the response shape isn't this test's concern).
// CreateMerchantHandler reissues a merchant-scoped token by reading the
// refresh cookie, so the cookie must be attached (see auth.go
// refreshCookiePath comment).
func createTenantMerchant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token string, refreshCookie *http.Cookie, companyID, name string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"company_id": companyID, "name": name, "timezone": "Asia/Jakarta",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.AddCookie(refreshCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create merchant %q expected 201, got %d: %s", name, rec.Code, rec.Body.String())
	}
	var merchantID string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.merchant WHERE company_id = $1 AND name = $2", companyID, name).Scan(&merchantID); err != nil {
		t.Fatalf("resolve merchant id for %q: %v", name, err)
	}
	return merchantID
}

// enableRequireTOTP turns the merchant's auth.require_totp policy flag on.
func enableRequireTOTP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.feature_flag (merchant_id, flag_key, flag_value) VALUES ($1, 'auth.require_totp', 'true'::jsonb) ON CONFLICT (merchant_id, flag_key) DO UPDATE SET flag_value = 'true'::jsonb", merchantID); err != nil {
		t.Fatalf("enable auth.require_totp: %v", err)
	}
}

// expireTenantMFAGrace backdates the user's grace window so the next login
// must return mfa_setup_required.
func expireTenantMFAGrace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE core.app_user SET mfa_grace_until = now() - interval '1 minute' WHERE username = $1", username); err != nil {
		t.Fatalf("expire grace: %v", err)
	}
}

// tenantLogin posts to /auth/login; totpCode "" means the field is omitted.
func tenantLogin(t *testing.T, router *gin.Engine, companyID, username, password, totpCode string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]string{"username": username, "password": password}
	if totpCode != "" {
		payload["totp_code"] = totpCode
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", "tenant-mfa-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func setupTenantMFAUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, username, phone string) (companyID, userID, merchantID string) {
	t.Helper()
	companyID, userID, token, refreshCookie := registerTenantMFAOwner(t, router, username, phone)
	merchantID = createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Klinik MFA "+username)
	// Logout revokes the auto-login session so the subsequent login doesn't
	// hit checkDeviceLimit (same pattern as TestLoginHandler_OwnerWithNoMerchantCanLogin).
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.Header.Set("X-Company-ID", companyID)
	logoutReq.AddCookie(refreshCookie)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}
	return companyID, userID, merchantID
}

// enrollTenantMFA finishes mandatory enrollment from a fresh mfa_setup_required
// login: backdate the grace window (so the next login must return
// mfa_setup_required), login, then enroll. Returns the enrolled secret and the
// one-time recovery codes.
func enrollTenantMFA(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, companyID, username string) (secret string, codes []string) {
	t.Helper()
	expireTenantMFAGrace(t, ctx, pool, username)
	rec := tenantLogin(t, router, companyID, username, "Passw0rd123!", "")
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
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

// seedTenantNonAdminUser inserts, via SQL, a user in the same company/merchant
// whose only role holds just core.person.manage — no admin permission, so MFA
// stays optional for them even though the Owner is forced to enroll (spec §2).
// Fixed UUIDs are safe: every test gets its own Postgres container.
func seedTenantNonAdminUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, username string) {
	t.Helper()
	roleID := "8f8f8f8f-8f8f-8f8f-8f8f-8f8f8f8f8f8f"
	userID := "8e8e8e8e-8e8e-8e8e-8e8e-8e8e8e8e8e8e"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Person Only')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (code, description, module) VALUES ('core.person.manage', 'test', 'core') ON CONFLICT (code) DO NOTHING"); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = 'core.person.manage'", roleID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, companyID, username, passwordHash); err != nil {
		t.Fatalf("seed non-admin user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
}

func TestTenantMFA_FlagOffUnchanged(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfaflagoff.owner", "081234510001")
	// The Owner is admin-required regardless of the flag (spec §2), so the
	// no-policy behaviour for a flag-off merchant must be asserted on a plain
	// staff user whose only role holds core.person.manage.
	seedTenantNonAdminUser(t, ctx, pool, companyID, merchantID, "mfaflagoff.staff")

	rec := tenantLogin(t, router, companyID, "mfaflagoff.staff", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["token"] == "" {
		t.Fatalf("expected a token, got %+v", data)
	}
	if _, ok := data["mfa_setup_due_at"]; ok {
		t.Fatalf("expected no mfa_setup_due_at with the flag off, got %+v", data)
	}
	var graceUntil *time.Time
	if err := pool.QueryRow(ctx, "SELECT mfa_grace_until FROM core.app_user WHERE username = 'mfaflagoff.staff'").Scan(&graceUntil); err != nil {
		t.Fatalf("read mfa_grace_until: %v", err)
	}
	if graceUntil != nil {
		t.Fatalf("expected mfa_grace_until to stay NULL with the flag off, got %v", graceUntil)
	}
}

func TestTenantMFA_FlagOnStartsGrace(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfaflagon.owner", "081234510002")
	enableRequireTOTP(t, ctx, pool, merchantID)

	rec := tenantLogin(t, router, companyID, "mfaflagon.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["token"] == "" {
		t.Fatalf("expected a token within the grace window, got %+v", data)
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
	if err := pool.QueryRow(ctx, "SELECT mfa_grace_until FROM core.app_user WHERE username = 'mfaflagon.owner'").Scan(&graceUntil); err != nil {
		t.Fatalf("read mfa_grace_until: %v", err)
	}
	if d := time.Until(graceUntil); d < week-time.Minute || d > week+time.Minute {
		t.Fatalf("expected DB mfa_grace_until ≈ now+7d, got %v (diff %v)", graceUntil, d)
	}
}

func TestTenantMFA_ExpiredGraceRequiresSetup(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfaexpired.owner", "081234510003")
	enableRequireTOTP(t, ctx, pool, merchantID)

	if rec := tenantLogin(t, router, companyID, "mfaexpired.owner", "Passw0rd123!", ""); rec.Code != http.StatusOK {
		t.Fatalf("first login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	expireTenantMFAGrace(t, ctx, pool, "mfaexpired.owner")

	rec := tenantLogin(t, router, companyID, "mfaexpired.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login after grace expected 200, got %d: %s", rec.Code, rec.Body.String())
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
}

func TestTenantMFA_EnrollThenLoginWithCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfaenroll.owner", "081234510004")
	enableRequireTOTP(t, ctx, pool, merchantID)

	secret, codes := enrollTenantMFA(t, ctx, pool, router, companyID, "mfaenroll.owner")
	if len(codes) != 10 {
		t.Fatalf("expected 10 recovery codes, got %d", len(codes))
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	rec := tenantLogin(t, router, companyID, "mfaenroll.owner", "Passw0rd123!", code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodePlatformData(t, rec)["token"] == "" {
		t.Fatalf("expected token after valid code, got %+v", decodePlatformData(t, rec))
	}
}

func TestTenantMFA_EnrollRejectsPlatformPurposeToken(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, userID, _ := setupTenantMFAUser(t, ctx, pool, router, "mfawrongtok.owner", "081234510005")

	// Platform-purpose token must NOT work on the tenant enroll endpoint.
	token, err := auth.GenerateMFAEnrollmentToken(testJWTSecret, userID, auth.PlatformMFAEnrollmentPurpose, 10*time.Minute)
	if err != nil {
		t.Fatalf("generate platform enrollment token: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_token": token, "secret": "JBSWY3DPEHPK3PXP", "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTenantMFA_EnrollTwiceConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, userID, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfatwice.owner", "081234510006")
	enableRequireTOTP(t, ctx, pool, merchantID)

	if _, codes := enrollTenantMFA(t, ctx, pool, router, companyID, "mfatwice.owner"); len(codes) != 10 {
		t.Fatalf("first enroll expected 10 codes, got %d", len(codes))
	}

	// Already enrolled → 409 even with a fresh, valid tenant-purpose token.
	token, err := auth.GenerateMFAEnrollmentToken(testJWTSecret, userID, auth.MFAEnrollmentPurpose, 10*time.Minute)
	if err != nil {
		t.Fatalf("generate tenant enrollment token: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"enrollment_token": token, "secret": "JBSWY3DPEHPK3PXP", "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTenantMFA_SwitchIntoFlaggedMerchantAfterGrace(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, token, ownerCookie := registerTenantMFAOwner(t, router, "mfaswitch.owner", "081234510007")
	merchantA := createTenantMerchant(t, ctx, pool, router, token, ownerCookie, companyID, "Klinik A mfaswitch")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, ownerCookie, companyID, "Klinik B mfaswitch")

	// Logout revokes the register auto-login session so select-merchant below
	// doesn't hit checkDeviceLimit.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.Header.Set("X-Company-ID", companyID)
	logoutReq.AddCookie(ownerCookie)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}

	// Two merchants → login pauses for selection; pick the UNflagged merchant A.
	loginRec := tenantLogin(t, router, companyID, "mfaswitch.owner", "Passw0rd123!", "")
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	loginData := decodePlatformData(t, loginRec)
	selectionToken, _ := loginData["selection_token"].(string)
	if selectionToken == "" {
		t.Fatalf("expected requires_merchant_selection payload, got %+v", loginData)
	}
	selBody, _ := json.Marshal(map[string]string{"selection_token": selectionToken, "merchant_id": merchantA})
	selReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/select-merchant", bytes.NewReader(selBody))
	selReq.Header.Set("Content-Type", "application/json")
	selReq.Header.Set("X-Company-ID", companyID)
	selReq.Header.Set("X-Device-Id", "tenant-mfa-test-device")
	selRec := httptest.NewRecorder()
	router.ServeHTTP(selRec, selReq)
	if selRec.Code != http.StatusOK {
		t.Fatalf("select-merchant expected 200, got %d: %s", selRec.Code, selRec.Body.String())
	}
	selData := decodePlatformData(t, selRec)
	sessionToken, _ := selData["token"].(string)
	if sessionToken == "" {
		t.Fatalf("expected a session token after selecting merchant A, got %+v", selData)
	}

	// Flag merchant B and backdate the grace window: switching into B after
	// grace must be blocked even though the session itself is still valid.
	enableRequireTOTP(t, ctx, pool, merchantB)
	expireTenantMFAGrace(t, ctx, pool, "mfaswitch.owner")

	body, _ := json.Marshal(map[string]string{"merchant_id": merchantB})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/switch-merchant", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantA)
	req.AddCookie(cookieNamed(t, selRec, "refresh_token"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != "mfa_setup_required" {
		t.Fatalf("expected code mfa_setup_required, got %+v", resp)
	}
}

func TestTenantMFA_StatusShape(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfastatus.owner", "081234510009")
	enableRequireTOTP(t, ctx, pool, merchantID)

	rec := tenantLogin(t, router, companyID, "mfastatus.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	loginToken, _ := decodePlatformData(t, rec)["token"].(string)
	if loginToken == "" {
		t.Fatalf("expected a token within the grace window")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mfa", nil)
	req.Header.Set("Authorization", "Bearer "+loginToken)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, req)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status expected 200, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
	data := decodePlatformData(t, statusRec)
	enabled, okEnabled := data["enabled"]
	required, okRequired := data["required"]
	graceUntil, okGrace := data["grace_until"]
	if !okEnabled || !okRequired || !okGrace {
		t.Fatalf("expected enabled/required/grace_until keys, got %+v", data)
	}
	if enabled != false {
		t.Fatalf("expected enabled=false before enrollment, got %v", enabled)
	}
	if required != true {
		t.Fatalf("expected required=true with the flag on, got %v", required)
	}
	if _, err := time.Parse(time.RFC3339, graceUntil.(string)); err != nil {
		t.Fatalf("expected RFC3339 grace_until, got %v", graceUntil)
	}
}

func TestTenantMFA_AdminRequiredWithoutFlag(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// No enableRequireTOTP: the Owner's admin permissions alone make TOTP
	// mandatory (spec §2).
	companyID, _, _ := setupTenantMFAUser(t, ctx, pool, router, "mfaadminreq.owner", "081234510013")

	rec := tenantLogin(t, router, companyID, "mfaadminreq.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["token"] == "" {
		t.Fatalf("expected a token within the grace window, got %+v", data)
	}
	if _, ok := data["mfa_setup_due_at"].(string); !ok {
		t.Fatalf("expected mfa_setup_due_at in login response, got %+v", data)
	}

	expireTenantMFAGrace(t, ctx, pool, "mfaadminreq.owner")
	rec = tenantLogin(t, router, companyID, "mfaadminreq.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login after grace expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodePlatformData(t, rec)["mfa_setup_required"] != true {
		t.Fatalf("expected mfa_setup_required after grace, got %+v", decodePlatformData(t, rec))
	}
}

func TestTenantMFA_NonAdminWithoutFlagUnchanged(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfanonadmin.owner", "081234510014")
	seedTenantNonAdminUser(t, ctx, pool, companyID, merchantID, "mfanonadmin.staff")

	rec := tenantLogin(t, router, companyID, "mfanonadmin.staff", "correct-horse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := decodePlatformData(t, rec)
	if data["token"] == "" {
		t.Fatalf("expected a normal login, got %+v", data)
	}
	if _, ok := data["mfa_setup_due_at"]; ok {
		t.Fatalf("expected no mfa_setup_due_at for a non-admin, got %+v", data)
	}
}

func TestTenantMFA_DisableRefusedWhenRequired(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfadisableref.owner", "081234510015")
	secret, _ := enrollTenantMFA(t, ctx, pool, router, companyID, "mfadisableref.owner")

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	rec := tenantLogin(t, router, companyID, "mfadisableref.owner", "Passw0rd123!", code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	token, _ := decodePlatformData(t, rec)["token"].(string)
	if token == "" {
		t.Fatalf("expected a session token, got %+v", decodePlatformData(t, rec))
	}

	body, _ := json.Marshal(map[string]string{"password": "Passw0rd123!"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/disable", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	disableRec := httptest.NewRecorder()
	router.ServeHTTP(disableRec, req)

	if disableRec.Code != http.StatusForbidden {
		t.Fatalf("disable expected 403, got %d: %s", disableRec.Code, disableRec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(disableRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode disable response: %v", err)
	}
	if resp.Error != "mfa is required for this account" {
		t.Fatalf("expected refusal error, got %+v", resp)
	}
	var secretSet bool
	if err := pool.QueryRow(ctx, "SELECT mfa_secret IS NOT NULL FROM core.app_user WHERE username = 'mfadisableref.owner'").Scan(&secretSet); err != nil {
		t.Fatalf("read mfa_secret: %v", err)
	}
	if !secretSet {
		t.Fatalf("expected TOTP secret to survive the refused disable")
	}
}

func TestTenantMFA_DisableRefusedWithForeignMerchantHeader(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _ := setupTenantMFAUser(t, ctx, pool, router, "mfadisableforeign.owner", "081234510017")
	secret, _ := enrollTenantMFA(t, ctx, pool, router, companyID, "mfadisableforeign.owner")

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	rec := tenantLogin(t, router, companyID, "mfadisableforeign.owner", "Passw0rd123!", code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	token, _ := decodePlatformData(t, rec)["token"].(string)
	if token == "" {
		t.Fatalf("expected a session token, got %+v", decodePlatformData(t, rec))
	}

	body, _ := json.Marshal(map[string]string{"password": "Passw0rd123!"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/disable", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", "7a7a7a7a-7a7a-7a7a-7a7a-7a7a7a7a7a7a")
	disableRec := httptest.NewRecorder()
	router.ServeHTTP(disableRec, req)

	if disableRec.Code != http.StatusForbidden {
		t.Fatalf("disable expected 403, got %d: %s", disableRec.Code, disableRec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(disableRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode disable response: %v", err)
	}
	if resp.Error != "mfa is required for this account" {
		t.Fatalf("expected refusal error, got %+v", resp)
	}
	var secretSet bool
	if err := pool.QueryRow(ctx, "SELECT mfa_secret IS NOT NULL FROM core.app_user WHERE username = 'mfadisableforeign.owner'").Scan(&secretSet); err != nil {
		t.Fatalf("read mfa_secret: %v", err)
	}
	if !secretSet {
		t.Fatalf("expected TOTP secret to survive the refused disable")
	}
}

func TestTenantMFA_StatusRequiredForAdmin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// Owner WITHOUT the flag → required=true purely from admin permissions.
	companyID, _, merchantID := setupTenantMFAUser(t, ctx, pool, router, "mfastatusadmin.owner", "081234510016")

	rec := tenantLogin(t, router, companyID, "mfastatusadmin.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	loginToken, _ := decodePlatformData(t, rec)["token"].(string)
	if loginToken == "" {
		t.Fatalf("expected a token within the grace window")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mfa", nil)
	req.Header.Set("Authorization", "Bearer "+loginToken)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, req)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status expected 200, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
	data := decodePlatformData(t, statusRec)
	if data["required"] != true {
		t.Fatalf("expected required=true for an admin without the flag, got %+v", data)
	}
	if data["enabled"] != false {
		t.Fatalf("expected enabled=false before enrollment, got %v", data["enabled"])
	}
}
