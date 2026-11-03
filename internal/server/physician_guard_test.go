//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestPhysicianRoleGuard covers the role-based guard on the physician list
// (plan 2026-11-03-user-mgmt-ux-alerts Task 2): a physician row only shows in
// GET /api/v1/merchants/{id}/physicians when its person has an ACTIVE user
// holding an assignment of a role with requires_physician_data=true AT THAT
// merchant; a person without any user, a removed assignment, a role at
// another merchant, and a soft-deleted role all fail the guard. The schedule
// calendar hides patterns of soft-deleted physicians.
func TestPhysicianRoleGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	// The with-person endpoint encrypts/hashes NIK via pgcrypto with config
	// keys — testConfig leaves them empty (pattern user_with_person_test.go).
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 1000
	cfg.RateLimitAPIBurst = 1000
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.NIKSearchKey = "test-search-key"
	router := server.NewRouter(pool, redisClient, cfg)
	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "grd.owner", "081234640001")
	env := &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}

	// gSeedDoctorUser creates user+person via POST /api/v1/users/with-person;
	// nik must be unique (uq_person_nik_hash is global).
	gSeedDoctorUser := func(t *testing.T, suffix, nik string) (userID, personID string) {
		t.Helper()
		rec := postWithPersonUser(t, router, token, companyID, merchantID, map[string]string{
			"company_id": companyID,
			"full_name":  "Dokter Guard " + suffix,
			"nik":        nik,
			"birth_date": "1990-05-10",
			"gender":     "male",
			"phone":      "081234640" + suffix,
			"email":      "grd." + suffix + "@guard.test",
			"address":    "Jl. Guard 1",
			"username":   "grd." + suffix,
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

	// gSeedPerson inserts a bare person (no user account).
	gSeedPerson := func(t *testing.T, name string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
			companyID, name).Scan(&id); err != nil {
			t.Fatalf("seed person: %v", err)
		}
		return id
	}

	// gSeedRole inserts a doctor role (UNIQUE (company_id, name) → distinct).
	gSeedRole := func(t *testing.T, name string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.role (company_id, name, requires_physician_data) VALUES ($1, $2, true) RETURNING id::text",
			companyID, name).Scan(&id); err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
		return id
	}

	// gAssign inserts a user_merchant_role and returns its id.
	gAssign := func(t *testing.T, userID, roleID, merchant string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3) RETURNING id::text",
			userID, merchant, roleID).Scan(&id); err != nil {
			t.Fatalf("seed assignment: %v", err)
		}
		return id
	}

	// gCreatePhysician creates a physician via the API for a person and
	// returns its id.
	gCreatePhysician := func(t *testing.T, personID string) string {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodPost, "/api/v1/physicians", map[string]any{
			"merchant_id": merchantID, "person_id": personID,
		})
		if status != http.StatusCreated {
			t.Fatalf("create physician = %d, want 201: %v", status, out)
		}
		data, _ := out["data"].(map[string]any)
		id, _ := data["ID"].(string)
		if id == "" {
			t.Fatalf("physician id missing: %v", data)
		}
		return id
	}

	// gListPhysicianIDs returns the set of physician ids in the M1 list.
	gListPhysicianIDs := func(t *testing.T) map[string]bool {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+merchantID+"/physicians", nil)
		if status != http.StatusOK {
			t.Fatalf("list physicians = %d: %v", status, out)
		}
		ids := map[string]bool{}
		for _, row := range payDataSlice(t, out) {
			if id, _ := row["ID"].(string); id != "" {
				ids[id] = true
			}
		}
		return ids
	}

	// 1. person tanpa user tersembunyi — physician (P1, M1) via API TANPA
	// user terkait: list TIDAK memuat P1.
	t.Run("person tanpa user tersembunyi", func(t *testing.T) {
		personID := gSeedPerson(t, "Dokter Tanpa Akun")
		p1 := gCreatePhysician(t, personID)
		if ids := gListPhysicianIDs(t); ids[p1] {
			t.Fatalf("physician of person without user must be hidden: %v", ids)
		}
	})

	// 2. user aktif ber-role dokter tampil — person P2 + user (with-person)
	// + assignment role dokter M1 + physician (P2, M1): list MEMUAT P2.
	t.Run("user aktif ber-role dokter tampil", func(t *testing.T) {
		userID, personID := gSeedDoctorUser(t, "02", "3201010101010012")
		roleID := gSeedRole(t, "Guard Dokter 2")
		gAssign(t, userID, roleID, merchantID)
		p2 := gCreatePhysician(t, personID)
		if ids := gListPhysicianIDs(t); !ids[p2] {
			t.Fatalf("physician of active doctor-role user must be listed: %v", ids)
		}
	})

	// 3. role dilepas hilang dari list — DELETE assignment → cleanup Task 1
	// soft-delete physician → list TIDAK memuat P3.
	t.Run("role dilepas hilang dari list", func(t *testing.T) {
		userID, personID := gSeedDoctorUser(t, "03", "3201010101010013")
		roleID := gSeedRole(t, "Guard Dokter 3")
		assignmentID := gAssign(t, userID, roleID, merchantID)
		p3 := gCreatePhysician(t, personID)
		status, out := qeRequest(t, env, http.MethodDelete, "/api/v1/merchant-roles/"+assignmentID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("delete assignment = %d, want 204: %v", status, out)
		}
		if ids := gListPhysicianIDs(t); ids[p3] {
			t.Fatalf("physician must leave the list after role removal: %v", ids)
		}
	})

	// 4. role di merchant lain tidak mengklaim — user U4 ber-role dokter di
	// M2 SAJA; physician (P4, M1) → list M1 TIDAK memuat P4.
	t.Run("role di merchant lain tidak mengklaim", func(t *testing.T) {
		merchant2 := "3d3d3d3d-3d3d-4d3d-8d3d-3d3d3d3d0004"
		if _, err := pool.Exec(ctx,
			"INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MGRD2', 'Klinik Guard B')",
			merchant2, companyID); err != nil {
			t.Fatalf("seed second merchant: %v", err)
		}
		userID, personID := gSeedDoctorUser(t, "04", "3201010101010014")
		roleID := gSeedRole(t, "Guard Dokter 4")
		gAssign(t, userID, roleID, merchant2)
		p4 := gCreatePhysician(t, personID)
		if ids := gListPhysicianIDs(t); ids[p4] {
			t.Fatalf("role at another merchant must not claim: %v", ids)
		}
	})

	// 5. role mati tidak mengklaim — role dokter di-soft-delete
	// (deleted_at + deleted_by bersama), assignment dibiarkan → list TIDAK
	// memuat P5.
	t.Run("role mati tidak mengklaim (guard list)", func(t *testing.T) {
		userID, personID := gSeedDoctorUser(t, "05", "3201010101010015")
		roleID := gSeedRole(t, "Guard Dokter 5")
		gAssign(t, userID, roleID, merchantID)
		p5 := gCreatePhysician(t, personID)
		if _, err := pool.Exec(ctx,
			"UPDATE core.role SET deleted_at = now(), deleted_by = $1 WHERE id = $2",
			ownerID, roleID); err != nil {
			t.Fatalf("soft-delete role: %v", err)
		}
		if ids := gListPhysicianIDs(t); ids[p5] {
			t.Fatalf("soft-deleted role must not claim: %v", ids)
		}
	})

	// 6. jadwal physician soft-deleted hilang dari kalender — query-DB
	// langsung (preseden TestScheduleDB_*): pattern milik physician yang
	// di-soft-delete tidak dikembalikan ListSchedulePatternsForCalendar.
	t.Run("jadwal physician soft-deleted hilang dari kalender", func(t *testing.T) {
		e := newScheduleTestEnv(t, ctx, '0', "GRD", "")
		scheduleID, rec := e.createSchedule(t, map[string]any{
			"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
			"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create schedule: expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
		_ = scheduleID

		var merchantUUID, physicianUUID pgtype.UUID
		if err := merchantUUID.Scan(e.merchantID); err != nil {
			t.Fatalf("scan merchant uuid: %v", err)
		}
		if err := physicianUUID.Scan(e.physicianID); err != nil {
			t.Fatalf("scan physician uuid: %v", err)
		}
		// The schedule env has its OWN postgres container — read through it.
		tx := locationTx(t, ctx, e.pool, e.companyID, e.merchantID)
		defer tx.Rollback(ctx)
		q := sqlcgen.New(tx)
		rows, err := q.ListSchedulePatternsForCalendar(ctx, merchantUUID)
		if err != nil {
			t.Fatalf("list calendar patterns: %v", err)
		}
		before := 0
		for _, row := range rows {
			if row.PhysicianID == physicianUUID {
				before++
			}
		}
		if before != 1 {
			t.Fatalf("patterns for physician before delete = %d, want 1 (%d rows total)", before, len(rows))
		}

		if _, err := e.pool.Exec(ctx,
			"UPDATE core.physician SET deleted_at = now(), deleted_by = (SELECT id FROM core.app_user WHERE company_id = $1 LIMIT 1) WHERE id = $2",
			e.companyID, e.physicianID); err != nil {
			t.Fatalf("soft-delete physician: %v", err)
		}

		rows, err = q.ListSchedulePatternsForCalendar(ctx, merchantUUID)
		if err != nil {
			t.Fatalf("list calendar patterns after delete: %v", err)
		}
		for _, row := range rows {
			if row.PhysicianID == physicianUUID {
				t.Fatalf("soft-deleted physician pattern still listed: %v", fmt.Sprintf("%s", row.ID))
			}
		}
	})
}
