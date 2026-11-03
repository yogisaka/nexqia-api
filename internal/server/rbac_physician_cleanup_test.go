//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestRemoveUserRolePhysicianCleanup covers the transactional physician
// cleanup when a user-merchant role with requires_physician_data is removed
// (plan 2026-11-03-user-mgmt-ux-alerts Task 1, spec §Item 1):
// soft-delete of the unclaimed physician, guard semantics of the cleanup
// query (other claimers, soft-deleted roles), the partial unique index
// allowing re-create after soft-delete, and no-op paths.
func TestRemoveUserRolePhysicianCleanup(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	// The with-person endpoint encrypts/hashes NIK via pgcrypto with config
	// keys — testConfig leaves them empty, which pgcrypto rejects (pattern
	// user_with_person_test.go:43-44).
	cfg := testConfig()
	// 8 subtests × several requests each exceeds testConfig's 100 req/min
	// API rate limit — lift it for this test only.
	cfg.RateLimitAPITokensPerMinute = 1000
	cfg.RateLimitAPIBurst = 1000
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.NIKSearchKey = "test-search-key"
	router := server.NewRouter(pool, redisClient, cfg)
	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "rmrole.owner", "081234630001")
	env := &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}

	// rcSeedDoctorUser creates user+person via POST /api/v1/users/with-person;
	// nik must be unique (uq_person_nik_hash is global).
	rcSeedDoctorUser := func(t *testing.T, suffix, nik string) (userID, personID string) {
		t.Helper()
		rec := postWithPersonUser(t, router, token, companyID, merchantID, map[string]string{
			"company_id": companyID,
			"full_name":  "Dokter Cleanup " + suffix,
			"nik":        nik,
			"birth_date": "1990-05-10",
			"gender":     "male",
			"phone":      "081234630" + suffix,
			"email":      "rmrole." + suffix + "@cleanup.test",
			"address":    "Jl. Cleanup 1",
			"username":   "rmrole." + suffix,
			"password":   "Passw0rd123!",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("with-person %s = %d, want 201: %s", suffix, rec.Code, rec.Body.String())
		}
		out := map[string]any{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode with-person response: %v", err)
		}
		data, _ := out["data"].(map[string]any)
		id, _ := data["ID"].(string)
		pid, _ := data["PersonID"].(string)
		if id == "" || pid == "" {
			t.Fatalf("user/person id missing: %v", data)
		}
		return id, pid
	}

	// rcSeedRole inserts a test role (UNIQUE (company_id, name) → distinct name).
	rcSeedRole := func(t *testing.T, name string, requires bool) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.role (company_id, name, requires_physician_data) VALUES ($1, $2, $3) RETURNING id::text",
			companyID, name, requires).Scan(&id); err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
		return id
	}

	// rcAssign inserts a user_merchant_role and returns its id.
	rcAssign := func(t *testing.T, userID, roleID string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3) RETURNING id::text",
			userID, merchantID, roleID).Scan(&id); err != nil {
			t.Fatalf("seed assignment: %v", err)
		}
		return id
	}

	// rcCreatePhysician creates a physician via the API for a person.
	rcCreatePhysician := func(t *testing.T, personID string) {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodPost, "/api/v1/physicians", map[string]any{
			"merchant_id": merchantID, "person_id": personID,
		})
		if status != http.StatusCreated {
			t.Fatalf("create physician = %d, want 201: %v", status, out)
		}
	}

	// rcPhysicianDeleted reports whether the (single) physician row of the
	// person at this merchant is soft-deleted.
	rcPhysicianDeleted := func(t *testing.T, personID string) bool {
		t.Helper()
		var deleted bool
		if err := pool.QueryRow(ctx,
			"SELECT deleted_at IS NOT NULL FROM core.physician WHERE person_id = $1 AND merchant_id = $2",
			personID, merchantID).Scan(&deleted); err != nil {
			t.Fatalf("query physician: %v", err)
		}
		return deleted
	}

	// rcDeleteAssignment DELETEs the assignment via the API, expecting 204.
	rcDeleteAssignment := func(t *testing.T, assignmentID string) {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodDelete, "/api/v1/merchant-roles/"+assignmentID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("delete assignment = %d, want 204: %v", status, out)
		}
	}

	// rcSoftDeleteRole soft-deletes a role (set deleted_at + deleted_by together).
	rcSoftDeleteRole := func(t *testing.T, roleID string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			"UPDATE core.role SET deleted_at = now(), deleted_by = $1 WHERE id = $2",
			ownerID, roleID); err != nil {
			t.Fatalf("soft-delete role: %v", err)
		}
	}

	// Subtest 1: hapus role dokter menonaktifkan physician.
	var s1Person string
	{
		userID, personID := rcSeedDoctorUser(t, "01", "3201010101010001")
		s1Person = personID
		roleID := rcSeedRole(t, "Dokter Uji 1", true)
		assignmentID := rcAssign(t, userID, roleID)
		rcCreatePhysician(t, personID)
		rcDeleteAssignment(t, assignmentID)
		if !rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest1: physician should be soft-deleted")
		}
		var byOwner bool
		if err := pool.QueryRow(ctx,
			"SELECT deleted_by = $1 FROM core.physician WHERE person_id = $2 AND merchant_id = $3",
			ownerID, personID, merchantID).Scan(&byOwner); err != nil || !byOwner {
			t.Fatalf("subtest1: deleted_by should be owner (err=%v byOwner=%v)", err, byOwner)
		}
	}

	// Subtest 2: dua role dokter — physician tetap.
	{
		userID, personID := rcSeedDoctorUser(t, "02", "3201010101010002")
		roleA := rcSeedRole(t, "Dokter Uji 2a", true)
		roleB := rcSeedRole(t, "Dokter Uji 2b", true)
		a := rcAssign(t, userID, roleA)
		rcAssign(t, userID, roleB)
		rcCreatePhysician(t, personID)
		rcDeleteAssignment(t, a)
		if rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest2: physician should stay (other dokter role claims it)")
		}
	}

	// Subtest 3: person dipakai user lain — physician tetap.
	{
		u1ID, personID := rcSeedDoctorUser(t, "03", "3201010101010003")
		roleID := rcSeedRole(t, "Dokter Uji 3", true)
		a := rcAssign(t, u1ID, roleID)
		// U2 shares U1's person (seeded via SQL, pattern seedFeeNoPermUser).
		u2ID := "7c2e0000-0000-0000-0000-000000000003"
		if _, err := pool.Exec(ctx,
			"INSERT INTO core.app_user (id, company_id, person_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, $5, true)",
			u2ID, companyID, personID, "rmrole.s3b", "$2a$10$abcdefghijklmnopqrstuv"); err != nil {
			t.Fatalf("seed second user on same person: %v", err)
		}
		rcAssign(t, u2ID, roleID)
		rcCreatePhysician(t, personID)
		rcDeleteAssignment(t, a)
		if rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest3: physician should stay (another user claims it)")
		}
	}

	// Subtest 4: role non-dokter tidak menyentuh physician.
	{
		userID, personID := rcSeedDoctorUser(t, "04", "3201010101010004")
		roleID := rcSeedRole(t, "Kasir Uji 4", false)
		assignmentID := rcAssign(t, userID, roleID)
		rcCreatePhysician(t, personID)
		rcDeleteAssignment(t, assignmentID)
		if rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest4: physician must stay (role without requires_physician_data)")
		}
	}

	// Subtest 5: re-create physician pasca soft-delete → 201 (partial unique).
	{
		status, out := qeRequest(t, env, http.MethodPost, "/api/v1/physicians", map[string]any{
			"merchant_id": merchantID, "person_id": s1Person,
		})
		if status != http.StatusCreated {
			t.Fatalf("subtest5: re-create physician = %d, want 201: %v", status, out)
		}
	}

	// Subtest 6: user tanpa person — hapus assignment tanpa error.
	{
		roleID := rcSeedRole(t, "Dokter Uji 6", true)
		userID := "7c2e0000-0000-0000-0000-000000000006"
		if _, err := pool.Exec(ctx,
			"INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)",
			userID, companyID, "rmrole.s6", "$2a$10$abcdefghijklmnopqrstuv"); err != nil {
			t.Fatalf("seed user without person: %v", err)
		}
		assignmentID := rcAssign(t, userID, roleID)
		rcDeleteAssignment(t, assignmentID)
	}

	// Subtest 7: assignment role soft-deleted → 204 tanpa cleanup.
	{
		userID, personID := rcSeedDoctorUser(t, "07", "3201010101010007")
		roleID := rcSeedRole(t, "Dokter Uji 7", true)
		assignmentID := rcAssign(t, userID, roleID)
		rcCreatePhysician(t, personID)
		rcSoftDeleteRole(t, roleID)
		rcDeleteAssignment(t, assignmentID)
		if rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest7: physician must stay (role already soft-deleted)")
		}
	}

	// Subtest 8: penjamin soft-deleted tidak menahan cleanup.
	{
		userID, personID := rcSeedDoctorUser(t, "08", "3201010101010008")
		roleX := rcSeedRole(t, "Dokter Uji 8x", true)
		roleY := rcSeedRole(t, "Dokter Uji 8y", true)
		rcAssign(t, userID, roleX)
		assignmentY := rcAssign(t, userID, roleY)
		rcCreatePhysician(t, personID)
		rcSoftDeleteRole(t, roleX) // assignment X tetap ada
		rcDeleteAssignment(t, assignmentY)
		if !rcPhysicianDeleted(t, personID) {
			t.Fatalf("subtest8: physician should be soft-deleted (soft-deleted role X is not a claimer)")
		}
	}
}
