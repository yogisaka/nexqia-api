//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedAccountOwner registers a fresh Owner (person + contact fields populated)
// through the real /auth/register endpoint and creates their first merchant
// through the real /merchants endpoint — reuses the tenant seed helpers from
// mfa_test.go. Everything the locked-group account tests need.
func seedAccountOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, username, phone string) (companyID, merchantID, userID, token string, refreshCookie *http.Cookie) {
	t.Helper()
	companyID, userID, token, refreshCookie = registerTenantMFAOwner(t, router, username, phone)
	merchantID = createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Klinik Akun Saya")
	return companyID, merchantID, userID, token, refreshCookie
}

// loginAccountSecondDevice logs the seeded owner in again from a different
// device. Raises the company's max_concurrent_sessions first — the DDL default
// is 1 and register's auto-login already used the only slot.
func loginAccountSecondDevice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, companyID, username string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", companyID); err != nil {
		t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": "Passw0rd123!"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", "account-second-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second device login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return resp.Data.Token
}

// accountRequest performs a locked-group account request with the standard
// tenant/auth headers.
func accountRequest(router *gin.Engine, method, path, token, companyID, merchantID string, body []byte) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

type accountMeResponse struct {
	Data struct {
		UserID         string  `json:"user_id"`
		Username       string  `json:"username"`
		Email          *string `json:"email"`
		Phone          *string `json:"phone"`
		PersonID       *string `json:"person_id"`
		FullName       *string `json:"full_name"`
		PhotoURL       *string `json:"photo_url"`
		HasPin         bool    `json:"has_pin"`
		PinLockEnabled bool    `json:"pin_lock_enabled"`
	} `json:"data"`
}

type accountSessionsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DeviceLabel string `json:"device_label"`
		MerchantID  string `json:"merchant_id"`
		IsCurrent   bool   `json:"is_current"`
	} `json:"data"`
}

type accountErrorResponse struct {
	Error string `json:"error"`
}

// currentSessionID returns the id of the single is_current row.
func currentSessionID(t *testing.T, sessions accountSessionsResponse) string {
	t.Helper()
	id := ""
	count := 0
	for _, s := range sessions.Data {
		if s.IsCurrent {
			count++
			id = s.ID
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 is_current session, got %d in %+v", count, sessions.Data)
	}
	return id
}

// TestAccount_GetMe_Shape — spec §5: me GET shape. full_name/photo_url null
// rules, has_pin false; pin_lock_enabled true for a merchant created via the API.
func TestAccount_GetMe_Shape(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "account.me", "081234500001")

	rec := accountRequest(router, http.MethodGet, "/api/v1/auth/me", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/me expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountMeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.UserID != userID {
		t.Errorf("user_id = %q, want %q", resp.Data.UserID, userID)
	}
	if resp.Data.Username != "account.me" {
		t.Errorf("username = %q, want account.me", resp.Data.Username)
	}
	if resp.Data.Email == nil || *resp.Data.Email != "account.me@example.com" {
		t.Errorf("email = %v, want account.me@example.com", resp.Data.Email)
	}
	if resp.Data.Phone == nil || *resp.Data.Phone != "081234500001" {
		t.Errorf("phone = %v, want 081234500001", resp.Data.Phone)
	}
	if resp.Data.PersonID == nil {
		t.Errorf("person_id = nil, want non-null for a registered owner")
	}
	if resp.Data.FullName == nil || *resp.Data.FullName != "Test Owner" {
		t.Errorf("full_name = %v, want Test Owner", resp.Data.FullName)
	}
	if resp.Data.PhotoURL != nil {
		t.Errorf("photo_url = %v, want null (no photo uploaded yet)", *resp.Data.PhotoURL)
	}
	if resp.Data.HasPin {
		t.Errorf("has_pin = true, want false for fresh owner")
	}
	if !resp.Data.PinLockEnabled {
		t.Errorf("pin_lock_enabled = false, want true (new merchants get PIN lock by default)")
	}
}

// TestAccount_PatchMe_TrimsAndClearsContact — spec §5: PATCH trims + empty
// email → NULL (never ""), and the change persists across a reload.
func TestAccount_PatchMe_TrimsAndClearsContact(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "account.patch", "081234500002")

	body, _ := json.Marshal(map[string]string{"full_name": "  Nama Baru  ", "email": "  patched@example.com  ", "phone": "  "})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /auth/me expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountMeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.FullName == nil || *resp.Data.FullName != "Nama Baru" {
		t.Errorf("full_name = %v, want trimmed Nama Baru", resp.Data.FullName)
	}
	if resp.Data.Email == nil || *resp.Data.Email != "patched@example.com" {
		t.Errorf("email = %v, want trimmed patched@example.com", resp.Data.Email)
	}
	if resp.Data.Phone != nil {
		t.Errorf("phone = %v, want null (whitespace-only cleared)", *resp.Data.Phone)
	}

	// Reload — GET must show the same persisted values.
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/me", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var reloaded accountMeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reloaded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if reloaded.Data.FullName == nil || *reloaded.Data.FullName != "Nama Baru" {
		t.Errorf("reloaded full_name = %v, want Nama Baru", reloaded.Data.FullName)
	}
	if reloaded.Data.Email == nil || *reloaded.Data.Email != "patched@example.com" {
		t.Errorf("reloaded email = %v, want patched@example.com", reloaded.Data.Email)
	}
	if reloaded.Data.Phone != nil {
		t.Errorf("reloaded phone = %v, want null", *reloaded.Data.Phone)
	}
}

// TestAccount_PatchMe_DuplicateEmail_409 — spec §5: duplicate email → 409.
func TestAccount_PatchMe_DuplicateEmail_409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "account.dup.a", "081234500003")
	// Second owner, different company — email/phone uniqueness is global.
	seedAccountOwner(t, ctx, pool, router, "account.dup.b", "081234500004")

	body, _ := json.Marshal(map[string]string{"email": "account.dup.b@example.com"})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH with duplicated email expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "email or phone already used" {
		t.Errorf("error = %q, want %q", resp.Error, "email or phone already used")
	}
}

// TestAccount_PatchMe_InvalidEmail_400 — stricter email validation
// (net/mail.ParseAddress, display-name forms rejected): "budi@" and
// "Budi <budi@example.com>" → 400, a plain address → 200.
func TestAccount_PatchMe_InvalidEmail_400(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "account.email.bad", "081234500010")

	for _, email := range []string{"budi@", "Budi <budi@example.com>"} {
		body, _ := json.Marshal(map[string]string{"email": email})
		rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH email %q expected 400, got %d: %s", email, rec.Code, rec.Body.String())
		}
		var resp accountErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Error != "invalid email format" {
			t.Errorf("email %q: error = %q, want %q", email, resp.Error, "invalid email format")
		}
	}

	body, _ := json.Marshal(map[string]string{"email": "budi@example.com"})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH valid email expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAccount_PatchMe_FullNameWithoutPerson_400 — spec §5: full_name given
// while the account has no person record → 400; empty after trim → 400.
func TestAccount_PatchMe_FullNameWithoutPerson_400(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "", false, 5)
	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"full_name": "Baru"})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH full_name without person expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "account has no person record" {
		t.Errorf("error = %q, want %q", resp.Error, "account has no person record")
	}

	emptyBody, _ := json.Marshal(map[string]string{"full_name": "   "})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, emptyBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH whitespace full_name expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAccount_ChangePassword_WrongCurrent_401 — spec §5: wrong current → 401.
func TestAccount_ChangePassword_WrongCurrent_401(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "account.pw.wrong", "081234500005")

	body, _ := json.Marshal(map[string]string{"current_password": "WrongPass1!", "new_password": "NewPassw0rd!"})
	rec := accountRequest(router, http.MethodPost, "/api/v1/auth/password", token, companyID, merchantID, body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /auth/password with wrong current expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "invalid password" {
		t.Errorf("error = %q, want %q", resp.Error, "invalid password")
	}
}

// TestAccount_ChangePassword_SameNew_400 — spec §3: new must differ from current.
func TestAccount_ChangePassword_SameNew_400(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "account.pw.same", "081234500006")

	body, _ := json.Marshal(map[string]string{"current_password": "Passw0rd123!", "new_password": "Passw0rd123!"})
	rec := accountRequest(router, http.MethodPost, "/api/v1/auth/password", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /auth/password with identical new password expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAccount_ChangePassword_Success_RevokesOthersKeepsCurrent — spec §5:
// success revokes other sessions but keeps the caller's current one.
func TestAccount_ChangePassword_Success_RevokesOthersKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, tokenA, cookieA := seedAccountOwner(t, ctx, pool, router, "account.pw.ok", "081234500007")
	tokenB := loginAccountSecondDevice(t, ctx, pool, router, companyID, "account.pw.ok")

	body, _ := json.Marshal(map[string]string{"current_password": "Passw0rd123!", "new_password": "NewPassw0rd!"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenA)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	req.AddCookie(cookieA)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/password expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Current device (A): exactly one session left, and it's current.
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", tokenA, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions as device A expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionsA accountSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsA); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(sessionsA.Data) != 1 {
		t.Fatalf("expected 1 remaining session for device A, got %d: %+v", len(sessionsA.Data), sessionsA.Data)
	}
	if !sessionsA.Data[0].IsCurrent {
		t.Errorf("remaining session should be is_current for device A: %+v", sessionsA.Data)
	}

	// Other device (B): its session was revoked — B still sees the user's one
	// surviving session (device A's), but it is not current for B.
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", tokenB, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions as device B expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionsB accountSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsB); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(sessionsB.Data) != 1 {
		t.Errorf("expected exactly 1 surviving session for device B's view, got %d: %+v", len(sessionsB.Data), sessionsB.Data)
	}
	if len(sessionsB.Data) == 1 && sessionsB.Data[0].IsCurrent {
		t.Errorf("surviving session must not be current for device B: %+v", sessionsB.Data[0])
	}
}

// TestAccount_Sessions_IsCurrentOnlyForCallerDevice — spec §5: is_current true
// only for the caller's device.
func TestAccount_Sessions_IsCurrentOnlyForCallerDevice(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, tokenA, _ := seedAccountOwner(t, ctx, pool, router, "account.sess", "081234500008")
	tokenB := loginAccountSecondDevice(t, ctx, pool, router, companyID, "account.sess")

	rec := accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", tokenA, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions as device A expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionsA accountSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsA); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(sessionsA.Data) != 2 {
		t.Fatalf("expected 2 active sessions, got %d: %+v", len(sessionsA.Data), sessionsA.Data)
	}
	idA := currentSessionID(t, sessionsA)

	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", tokenB, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions as device B expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionsB accountSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsB); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	idB := currentSessionID(t, sessionsB)

	if idA == idB {
		t.Errorf("is_current must follow the caller's device: device A saw %q, device B saw the same", idA)
	}
}

// TestAccount_RevokeSession_CurrentDevice_400 — spec §5: deleting the current
// device's session → 400 (logout is the intended path); deleting someone
// else's still works.
func TestAccount_RevokeSession_CurrentDevice_400(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, tokenA, _ := seedAccountOwner(t, ctx, pool, router, "account.revoke", "081234500009")
	tokenB := loginAccountSecondDevice(t, ctx, pool, router, companyID, "account.revoke")

	rec := accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", tokenA, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions as device A expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessions accountSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	idA := currentSessionID(t, sessions)

	rec = accountRequest(router, http.MethodDelete, "/api/v1/auth/sessions/"+idA, tokenA, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE own current session expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp accountErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "use logout for the current device" {
		t.Errorf("error = %q, want %q", resp.Error, "use logout for the current device")
	}

	// The other device may still revoke that same session.
	rec = accountRequest(router, http.MethodDelete, "/api/v1/auth/sessions/"+idA, tokenB, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE other device's session expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAccount_PinSet_SeedsApplockKeyAndReturnsIdleMinutes — spec §5: PIN set
// with auth.pin_lock on → next locked-group request is 200 (not 423) and the
// response carries pin_lock_idle_minutes.
func TestAccount_PinSet_SeedsApplockKeyAndReturnsIdleMinutes(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "", true, 7)
	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"password": "correct-horse", "pin": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pin/set", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/pin/set expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			PinLockIdleMinutes int `json:"pin_lock_idle_minutes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.PinLockIdleMinutes != 7 {
		t.Errorf("pin_lock_idle_minutes = %d, want 7 (merchant flag idle_minutes)", resp.Data.PinLockIdleMinutes)
	}

	// The bug fix: the very next locked-group request must NOT be 423.
	pingReq := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	pingReq.Header.Set("X-Company-ID", companyID)
	pingReq.Header.Set("X-Merchant-ID", merchantID)
	pingReq.Header.Set("Authorization", "Bearer "+token)
	pingRec := httptest.NewRecorder()
	router.ServeHTTP(pingRec, pingReq)
	if pingRec.Code != http.StatusOK {
		t.Errorf("expected 200 from locked group right after PIN set, got %d: %s", pingRec.Code, pingRec.Body.String())
	}
}
