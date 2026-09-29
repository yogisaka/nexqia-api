//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// auditAPIList decodes the {"data": [...], "meta": {...}} envelope of the audit
// read endpoints; rows are generic maps because the API speaks snake_case.
type auditAPIList struct {
	Data []map[string]any `json:"data"`
	Meta map[string]int   `json:"meta"`
}

// seedRestrictedAuditUser inserts a user whose only role at merchantID lacks
// audit.log.view (same shape as the restricted user in access_log_test.go).
func seedRestrictedAuditUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, username string) string {
	t.Helper()
	userID := "6c6c6c6c-6c6c-6c6c-6c6c-6c6c6c6c6c6c"
	roleID := "6d6d6d6d-6d6d-6d6d-6d6d-6d6d6d6d6d6d"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Audit Access')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, companyID, username, passwordHash); err != nil {
		t.Fatalf("seed restricted user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	return userID
}

// TestAuditAPI_RequiresPermission — a user whose role lacks audit.log.view
// gets 403 on both audit list endpoints.
func TestAuditAPI_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "audit.apiview", "081234570201")
	restrictedUserID := seedRestrictedAuditUser(t, ctx, pool, companyID, merchantID, "audit.noview")
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "audit.noview", "audit-noview-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	for _, path := range []string{"/api/v1/audit/access-logs", "/api/v1/audit/change-logs"} {
		rec := accountRequest(router, http.MethodGet, path, restrictedToken, companyID, merchantID, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s without audit.log.view expected 403, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAuditAPI_ListsOwnCompanyOnly — rows written for another company never
// appear in either list, while the caller's own rows do.
func TestAuditAPI_ListsOwnCompanyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	otherCompanyID, otherMerchantID, _, otherToken, _ := seedAccountOwner(t, ctx, pool, router, "audit.apiother", "081234570211")
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apimine", "081234570212")

	// One person read per company so each has one person/view access_log row and
	// one person insert audit_log row.
	createPersonViaAPI(t, router, otherToken, otherCompanyID, otherMerchantID, "Audit API Other Person", "male")
	otherPersonID, _ := latestPerson(t, ctx, pool, otherCompanyID, "Audit API Other Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+otherPersonID, otherToken, otherCompanyID, otherMerchantID, nil)

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Own Person", "male")
	ownPersonID, _ := latestPerson(t, ctx, pool, companyID, "Audit API Own Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+ownPersonID, token, companyID, merchantID, nil)

	var otherAccessID, otherChangeID string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM core.access_log WHERE company_id = $1 AND resource = 'person' AND action = 'view'", otherCompanyID).Scan(&otherAccessID); err != nil {
		t.Fatalf("resolve other company access row: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT id::text FROM core.audit_log WHERE company_id = $1 AND record_id = $2 AND action = 'insert'", otherCompanyID, otherPersonID).Scan(&otherChangeID); err != nil {
		t.Fatalf("resolve other company change row: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list access logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var accessResp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &accessResp); err != nil {
		t.Fatalf("decode access-logs response: %v", err)
	}
	sawOwn := false
	for _, row := range accessResp.Data {
		if row["id"] == otherAccessID {
			t.Fatalf("another company's access_log row leaked into the response")
		}
		if row["resource_id"] == ownPersonID {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Fatalf("own company access_log row missing from the response")
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/change-logs", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list change logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var changeResp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &changeResp); err != nil {
		t.Fatalf("decode change-logs response: %v", err)
	}
	sawOwn = false
	for _, row := range changeResp.Data {
		if row["id"] == otherChangeID {
			t.Fatalf("another company's audit_log row leaked into the response")
		}
		if row["record_id"] == ownPersonID && row["action"] == "insert" {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Fatalf("own company audit_log row missing from the response")
	}
}

// TestAuditAPI_RangeTooLong — a 100-day from/to window is rejected with 400 on
// both audit list endpoints.
func TestAuditAPI_RangeTooLong(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apirange", "081234570221")

	to := time.Now().UTC().Format(time.RFC3339)
	from := time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339)
	for _, path := range []string{"/api/v1/audit/access-logs", "/api/v1/audit/change-logs"} {
		rec := accountRequest(router, http.MethodGet, path+"?from="+from+"&to="+to, token, companyID, merchantID, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s with a 100-day range expected 400, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAuditAPI_ChangeLogFilterByRecord — after a PATCH person that changes the
// phone, the record_id filter returns that person's update row with
// changed_fields=[phone] attributed to the caller.
func TestAuditAPI_ChangeLogFilterByRecord(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apirec", "081234570231")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Filter Person", "female")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit API Filter Person")
	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID,
		patchPersonBody("Audit API Filter Person", "female", mrn, "081234570232"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/change-logs?record_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list change logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode change-logs response: %v", err)
	}
	updates := 0
	for _, row := range resp.Data {
		if row["action"] != "update" {
			continue
		}
		updates++
		if row["table_name"] != "core.person" {
			t.Fatalf("expected table_name core.person, got %v", row["table_name"])
		}
		if row["record_id"] != personID {
			t.Fatalf("expected record_id %s, got %v", personID, row["record_id"])
		}
		fields, ok := row["changed_fields"].([]any)
		if !ok || len(fields) != 1 || fields[0] != "phone" {
			t.Fatalf("expected changed_fields [phone], got %v", row["changed_fields"])
		}
		if row["changed_by"] != userID {
			t.Fatalf("expected changed_by %s, got %v", userID, row["changed_by"])
		}
	}
	if updates != 1 {
		t.Fatalf("expected exactly 1 update row for the person, got %d", updates)
	}
}
