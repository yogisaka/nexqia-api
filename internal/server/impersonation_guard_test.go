//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// impersonationToken issues an access token that authenticates AS the seeded
// owner but carries an ImpersonatedBy admin claim — the shape POST
// /platform/impersonate produces.
func impersonationToken(t *testing.T, userID, companyID, merchantID string) string {
	t.Helper()
	token, err := auth.GenerateImpersonationToken(
		testJWTSecret, userID, companyID, merchantID,
		"platform-admin", "impersonator-device", "00000000-0000-0000-0000-000000000001", time.Hour,
	)
	if err != nil {
		t.Fatalf("GenerateImpersonationToken: %v", err)
	}
	return token
}

// TestImpersonationGuard_WritesRejected — self-service account writes on an
// impersonation session are rejected with 403, even when the endpoint requires
// the user's password (those already asked for it, blocked anyway for one rule).
func TestImpersonationGuard_WritesRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "imp.guard", "081234509001")
	token := impersonationToken(t, userID, companyID, merchantID)

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"patch me", http.MethodPatch, "/api/v1/auth/me", []byte(`{"full_name":"Hacked Name"}`)},
		{"change password", http.MethodPost, "/api/v1/auth/password", []byte(`{"current_password":"x","new_password":"Passw0rd123!"}`)},
		{"pin set", http.MethodPost, "/api/v1/auth/pin/set", []byte(`{"password":"x","pin":"123456"}`)},
		{"pin disable", http.MethodPost, "/api/v1/auth/pin/disable", []byte(`{"password":"x"}`)},
		{"mfa setup", http.MethodPost, "/api/v1/auth/mfa/setup", nil},
		{"mfa confirm", http.MethodPost, "/api/v1/auth/mfa/confirm", []byte(`{"secret":"JBSWY3DPEHPK3PXP","code":"123456","password":"x"}`)},
		{"mfa disable", http.MethodPost, "/api/v1/auth/mfa/disable", []byte(`{"password":"x"}`)},
		{"revoke session", http.MethodDelete, "/api/v1/auth/sessions/00000000-0000-0000-0000-000000000002", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := accountRequest(router, tc.method, tc.path, token, companyID, merchantID, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp accountErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error != "not allowed during impersonation" {
				t.Fatalf("unexpected error: %q", resp.Error)
			}
		})
	}
}

// TestImpersonationGuard_ReadsAllowed — read-only self-service endpoints stay
// usable during impersonation.
func TestImpersonationGuard_ReadsAllowed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "imp.read", "081234509002")
	token := impersonationToken(t, userID, companyID, merchantID)

	rec := accountRequest(router, http.MethodGet, "/api/v1/auth/me", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/me expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/sessions", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/mfa", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/mfa expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestImpersonationGuard_NormalSessionUnaffected — the owner's ordinary session
// still writes their own profile normally.
func TestImpersonationGuard_NormalSessionUnaffected(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "imp.normal", "081234509003")

	rec := accountRequest(router, http.MethodPatch, "/api/v1/auth/me", token, companyID, merchantID, []byte(`{"full_name":"Owner Renamed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /auth/me expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
