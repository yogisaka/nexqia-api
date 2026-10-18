//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestFormAccountFields_MerchantTimezone — a merchant timezone outside
// WIB/WITA/WIT must be rejected with the per-field map
// (spec 2026-10-07-form-validation-stage2 §4.3): fields.timezone =
// "Zona waktu harus WIB, WITA, atau WIT"; Asia/Makassar (WITA) succeeds.
func TestFormAccountFields_MerchantTimezone(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "ffa.tz", "081234570301")

	// POST /merchants reissues a merchant-scoped token from the refresh cookie
	// (CreateMerchantHandler), so both requests must carry it.
	newMerchant := func(t *testing.T, name, timezone string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{
			"company_id": companyID, "name": name, "timezone": timezone,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Company-ID", companyID)
		req.AddCookie(refreshCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := newMerchant(t, "Klinik Tokyo", "Asia/Tokyo")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["timezone"] != "Zona waktu harus WIB, WITA, atau WIT" {
		t.Fatalf("expected fields.timezone message, got %q", fields["timezone"])
	}

	rec = newMerchant(t, "Klinik Makassar", "Asia/Makassar")
	if rec.Code != http.StatusCreated {
		t.Fatalf("Asia/Makassar expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFormAccountFields_MeEmail — PATCH /auth/me with a malformed email keeps
// the old error text "invalid email format" and now carries fields.email =
// "Email tidak valid" (spec §4.3).
func TestFormAccountFields_MeEmail(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "ffa.email", "081234570302")

	body, _ := json.Marshal(map[string]string{"email": "bukan-email"})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "invalid email format" {
		t.Fatalf("expected error invalid email format, got %q", errMsg)
	}
	if fields["email"] != "Email tidak valid" {
		t.Fatalf("expected fields.email message, got %q", fields["email"])
	}
}

// TestFormAccountFields_RegisterMismatch — a registration whose
// password_confirmation differs must keep the old error text and now carry
// fields.password_confirmation = "Konfirmasi kata sandi tidak sama"
// (spec §4.5).
func TestFormAccountFields_RegisterMismatch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	payload := map[string]string{
		"company_name":          "Form Fields Register Co",
		"full_name":             "FFA Owner",
		"username":              "ffa.mismatch",
		"email":                 "ffa.mismatch@example.com",
		"phone":                 "081234570303",
		"password":              "Passw0rd123!",
		"password_confirmation": "Passw0rd124!",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "form-account-fields")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "password and password_confirmation do not match" {
		t.Fatalf("expected old error text, got %q", errMsg)
	}
	if fields["password_confirmation"] != "Konfirmasi kata sandi tidak sama" {
		t.Fatalf("expected fields.password_confirmation message, got %q", fields["password_confirmation"])
	}
}

// TestFormAccountFields_UserMissingPassword — POST /users without a password
// is answered by the binding with error=validation_failed and
// fields.password = "Wajib diisi" (spec §4.3, Task 5 handler).
func TestFormAccountFields_UserMissingPassword(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "ffa.user", "081234570304")
	createPersonViaAPI(t, router, token, companyID, merchantID, "FFA User Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "FFA User Person")

	body, _ := json.Marshal(map[string]string{
		"company_id": companyID,
		"person_id":  personID,
		"username":   "ffa.newuser",
		// password deliberately absent
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/users", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["password"] != "Wajib diisi" {
		t.Fatalf("expected fields.password = Wajib diisi, got %q", fields["password"])
	}
}
