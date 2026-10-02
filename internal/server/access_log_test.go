//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// accessRow is one core.access_log row relevant to the middleware tests
// (empty string = NULL column).
type accessRow struct {
	ResourceID      string
	PlatformAdminID string
	StatusCode      int16
	ActorID         string
}

// accessLogRows reads the access_log rows for (company, resource, action). The
// test pool connects as the table owner, so RLS on core.access_log does not
// apply to these reads.
func accessLogRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, resource, action string) []accessRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(resource_id::text, ''), COALESCE(platform_admin_id::text, ''), status_code, COALESCE(actor_id::text, '')
		FROM core.access_log
		WHERE company_id = $1 AND resource = $2 AND action = $3
		ORDER BY created_at`, companyID, resource, action)
	if err != nil {
		t.Fatalf("query core.access_log: %v", err)
	}
	defer rows.Close()
	out := []accessRow{}
	for rows.Next() {
		var r accessRow
		if err := rows.Scan(&r.ResourceID, &r.PlatformAdminID, &r.StatusCode, &r.ActorID); err != nil {
			t.Fatalf("scan access_log row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate access_log rows: %v", err)
	}
	return out
}

// TestAccessLog_ViewRecorded — GET /persons/:id records exactly one person/view
// row with the person id as resource_id, status 200 and the caller as actor.
func TestAccessLog_ViewRecorded(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.accview", "081234570101")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Access View Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Access View Person")

	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := accessLogRows(t, ctx, pool, companyID, "person", "view")
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 person/view row, got %d", len(rows))
	}
	if rows[0].ResourceID != personID {
		t.Fatalf("expected resource_id %s, got %q", personID, rows[0].ResourceID)
	}
	if rows[0].StatusCode != 200 {
		t.Fatalf("expected status_code 200, got %d", rows[0].StatusCode)
	}
	if rows[0].ActorID != userID {
		t.Fatalf("expected actor_id %s, got %q", userID, rows[0].ActorID)
	}
	if rows[0].PlatformAdminID != "" {
		t.Fatalf("expected platform_admin_id NULL for an ordinary session, got %q", rows[0].PlatformAdminID)
	}
}

// TestAccessLog_SearchDoesNotStoreQuery — GET /persons/search?q=Budi records a
// person/search row with resource_id NULL, and the search term never appears
// anywhere in the logged row (spec §4: no query strings).
func TestAccessLog_SearchDoesNotStoreQuery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.accsearch", "081234570111")

	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/search?q=Budi", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search persons expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := accessLogRows(t, ctx, pool, companyID, "person", "search")
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 person/search row, got %d", len(rows))
	}
	if rows[0].ResourceID != "" {
		t.Fatalf("expected resource_id NULL for a search, got %q", rows[0].ResourceID)
	}

	var leaked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE row_to_json(access_log)::text ILIKE '%budi%'").Scan(&leaked); err != nil {
		t.Fatalf("leak probe: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("search term leaked into core.access_log: %d row(s) contain it", leaked)
	}
}

// TestAccessLog_ForbiddenRecordedWithStatus — a caller whose role lacks
// core.person.manage gets 403 from RequirePermission (plain c.JSON, not an
// abort), so the attempt is recorded with status_code 403.
func TestAccessLog_ForbiddenRecordedWithStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, ownerToken, _ := seedAccountOwner(t, ctx, pool, router, "audit.accforb", "081234570121")

	createPersonViaAPI(t, router, ownerToken, companyID, merchantID, "Access Forbidden Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Access Forbidden Person")

	// Restricted user: a role without core.person.manage at this merchant.
	restrictedUserID := "6a6a6a6a-6a6a-6a6a-6a6a-6a6a6a6a6a6a"
	roleID := "6b6b6b6b-6b6b-6b6b-6b6b-6b6b6b6b6b6b"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Person Access')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'audit.noaccess', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed restricted user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "audit.noaccess", "audit-noaccess-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, restrictedToken, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("get person without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := accessLogRows(t, ctx, pool, companyID, "person", "view")
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 person/view row for the forbidden attempt, got %d", len(rows))
	}
	if rows[0].StatusCode != 403 {
		t.Fatalf("expected status_code 403, got %d", rows[0].StatusCode)
	}
	if rows[0].ResourceID != personID {
		t.Fatalf("expected resource_id %s, got %q", personID, rows[0].ResourceID)
	}
}

// TestAccessLog_ImpersonationRecordsAdmin — a read performed through an
// impersonation token records platform_admin_id = the impersonating admin.
func TestAccessLog_ImpersonationRecordsAdmin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.accimp", "081234570131")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Access Impersonated Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Access Impersonated Person")

	impToken := impersonationToken(t, tokenToUserID(t, pool, token), companyID, merchantID)
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, impToken, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get person under impersonation expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := accessLogRows(t, ctx, pool, companyID, "person", "view")
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 person/view row, got %d", len(rows))
	}
	if rows[0].PlatformAdminID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("expected platform_admin_id = impersonating admin, got %q", rows[0].PlatformAdminID)
	}
	if rows[0].ActorID != userID {
		t.Fatalf("expected actor_id = impersonated user %s, got %q", userID, rows[0].ActorID)
	}
}
