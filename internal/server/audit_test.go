//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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

	for _, path := range []string{
		"/api/v1/audit/access-logs",
		"/api/v1/audit/change-logs",
		"/api/v1/audit/patients?q=ab",
		"/api/v1/audit/users?q=ab",
	} {
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

// TestAuditAPI_FilterByPerson — two patients are viewed, then the person_id
// filter returns only that patient's rows with patient_name and patient_mrn
// resolved (spec §4.1).
func TestAuditAPI_FilterByPerson(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apipers", "081234570241")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Person Alpha", "male")
	personA, mrnA := latestPerson(t, ctx, pool, companyID, "Audit API Person Alpha")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Person Beta", "female")
	personB, _ := latestPerson(t, ctx, pool, companyID, "Audit API Person Beta")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personA, token, companyID, merchantID, nil)
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personB, token, companyID, merchantID, nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs?person_id="+personA, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list access logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode access-logs response: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatalf("expected at least 1 row for the filtered patient")
	}
	sawView := false
	for _, row := range resp.Data {
		if row["person_id"] != personA {
			t.Fatalf("expected only person %s rows, got person_id %v", personA, row["person_id"])
		}
		if row["resource"] == "person" && row["action"] == "view" {
			sawView = true
			if row["patient_name"] != "Audit API Person Alpha" {
				t.Fatalf("expected patient_name resolved, got %v", row["patient_name"])
			}
			if row["patient_mrn"] != mrnA {
				t.Fatalf("expected patient_mrn %s, got %v", mrnA, row["patient_mrn"])
			}
		}
	}
	if !sawView {
		t.Fatalf("person/view row missing from the filtered response")
	}
}

// TestAuditAPI_NamesIncludeDeleted — after the patient is soft-deleted, the
// logged rows still resolve patient_name/patient_mrn and /audit/patients keeps
// returning the patient flagged deleted (spec §4.3).
func TestAuditAPI_NamesIncludeDeleted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apidel", "081234570251")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Gone Person", "male")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit API Gone Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
	if _, err := pool.Exec(ctx, "UPDATE core.person SET deleted_at = now(), deleted_by = $2 WHERE id = $1", personID, ownerID); err != nil {
		t.Fatalf("soft delete person: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list access logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode access-logs response: %v", err)
	}
	sawView := false
	for _, row := range resp.Data {
		if row["resource"] == "person" && row["action"] == "view" {
			sawView = true
			if row["patient_name"] != "Audit API Gone Person" {
				t.Fatalf("deleted patient name lost from the log, got %v", row["patient_name"])
			}
			if row["patient_mrn"] != mrn {
				t.Fatalf("deleted patient mrn lost from the log, got %v", row["patient_mrn"])
			}
		}
	}
	if !sawView {
		t.Fatalf("person/view row missing from the response")
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/patients?q="+url.QueryEscape("Audit API Gone Person"), token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search patients expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var searchResp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &searchResp); err != nil {
		t.Fatalf("decode patients response: %v", err)
	}
	if len(searchResp.Data) != 1 {
		t.Fatalf("expected exactly 1 patient hit, got %d: %s", len(searchResp.Data), rec.Body.String())
	}
	hit := searchResp.Data[0]
	if hit["id"] != personID || hit["deleted"] != true {
		t.Fatalf("expected the soft-deleted patient flagged deleted, got %v", hit)
	}
	if hit["medical_record_no"] != mrn {
		t.Fatalf("expected medical_record_no %s, got %v", mrn, hit["medical_record_no"])
	}
}

// TestAuditAPI_SearchEscapesWildcards — two patients exist; a q of literal
// "%%" matches none (wildcards are escaped) and a 1-character q is a 400.
func TestAuditAPI_SearchEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apiesc", "081234570261")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Wild Person", "male")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Wilder Person", "female")

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/patients?q=%25%25", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search patients expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode patients response: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("wildcards must match literally, got %d hits for %%", len(resp.Data))
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/patients?q=x", token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("1-character q expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAuditAPI_SearchOwnCompanyOnly — patients and users of another company
// never appear in the search results (spec §4.3).
func TestAuditAPI_SearchOwnCompanyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	otherCompanyID, otherMerchantID, _, otherToken, _ := seedAccountOwner(t, ctx, pool, router, "audit.apico", "081234570271")
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apimy", "081234570272")

	createPersonViaAPI(t, router, otherToken, otherCompanyID, otherMerchantID, "Audit API Isolated Person", "male")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Isolated Person", "female")
	ownPersonID, _ := latestPerson(t, ctx, pool, companyID, "Audit API Isolated Person")

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/patients?q="+url.QueryEscape("Audit API Isolated Person"), token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search patients expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var patients auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &patients); err != nil {
		t.Fatalf("decode patients response: %v", err)
	}
	if len(patients.Data) != 1 || patients.Data[0]["id"] != ownPersonID {
		t.Fatalf("expected only the own-company patient, got %s", rec.Body.String())
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/users?q=audit.api", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search users expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var users auditAPIList
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users response: %v", err)
	}
	for _, row := range users.Data {
		if row["username"] == "audit.apico" {
			t.Fatalf("another company's user leaked into the search results")
		}
	}
	sawMine := false
	for _, row := range users.Data {
		if row["username"] == "audit.apimy" {
			sawMine = true
		}
	}
	if !sawMine {
		t.Fatalf("own user missing from the search results: %s", rec.Body.String())
	}
}

// TestAuditAPI_AuditCallsAreLogged — audit list and search calls record their
// own access_log rows, and the query string never lands in any column
// (spec §4.4; probe pattern from access_log_test.go).
func TestAuditAPI_AuditCallsAreLogged(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.apilog", "081234570281")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit API Logged Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Audit API Logged Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list access logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'audit' AND action = 'list_access' AND person_id = $2", companyID, personID).Scan(&listRows); err != nil {
		t.Fatalf("count list_access rows: %v", err)
	}
	if listRows != 1 {
		t.Fatalf("expected exactly 1 audit/list_access row with person_id, got %d", listRows)
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/patients?q=xx", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search patients expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var searchRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'audit' AND action = 'search_patient'", companyID).Scan(&searchRows); err != nil {
		t.Fatalf("count search_patient rows: %v", err)
	}
	if searchRows != 1 {
		t.Fatalf("expected exactly 1 audit/search_patient row, got %d", searchRows)
	}

	var leaked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_log WHERE company_id = $1 AND row_to_json(access_log)::text ILIKE '%xx%'", companyID).Scan(&leaked); err != nil {
		t.Fatalf("leak probe: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("search term leaked into core.access_log: %d row(s) contain it", leaked)
	}
}
