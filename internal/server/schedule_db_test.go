//go:build integration

package server_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// scheduleSeed inserts the physician/department/physician_schedule chain needed
// by schedule_session tests, inside the given tenant-scoped transaction, and
// returns the schedule, physician, and department IDs.
func scheduleSeed(t *testing.T, ctx context.Context, tx pgx.Tx, companyID, merchantID, tag string) (scheduleID, physicianID, departmentID string) {
	t.Helper()
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.person (company_id, full_name, gender)
		VALUES ($1, $2, 'male') RETURNING id::text`,
		companyID, "Dokter "+tag).Scan(&physicianID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.physician (company_id, merchant_id, person_id)
		VALUES ($1, $2, $3) RETURNING id::text`,
		companyID, merchantID, physicianID).Scan(&physicianID); err != nil {
		t.Fatalf("seed physician: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.department (company_id, merchant_id, code, name)
		VALUES ($1, $2, $3, $4) RETURNING id::text`,
		companyID, merchantID, tag+"-DEPT", "Poli "+tag).Scan(&departmentID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.physician_schedule
			(company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, slot_quota, effective_from)
		VALUES ($1, $2, $3, $4, 1, '08:00', '14:00', 10, '2026-10-01')
		RETURNING id::text`,
		companyID, merchantID, physicianID, departmentID).Scan(&scheduleID); err != nil {
		t.Fatalf("seed physician_schedule: %v", err)
	}
	return scheduleID, physicianID, departmentID
}

// TestScheduleDB_Constraints — DB-level guards from migration 000056: inverted
// times are rejected on physician_schedule and schedule_session, an unknown
// service_type violates the subset CHECK, a 'leave' session without leave_id
// violates chk_schedule_session_leave, and (schedule_id, session_date) is
// UNIQUE. Each failing statement runs in a savepoint (CHECK = 23514,
// UNIQUE = 23505); expectations come from the DDL definitions.
func TestScheduleDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "sched.cons", "081234580101")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	scheduleID, physicianID, departmentID := scheduleSeed(t, ctx, tx, companyID, merchantID, "SCHC")

	// chk_physician_schedule_time: end_time > start_time → 08:00–08:00 fails.
	locationExpectError(t, ctx, tx, "schedule inverted time", "23514",
		"INSERT INTO operations.physician_schedule (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, effective_from) VALUES ($1, $2, $3, $4, 2, '08:00', '08:00', '2026-10-01')",
		companyID, merchantID, physicianID, departmentID)

	// service_types subset CHECK: 'spa' is not in umum/bpjs/asuransi → 23514.
	locationExpectError(t, ctx, tx, "unknown service type", "23514",
		"INSERT INTO operations.physician_schedule (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, effective_from, service_types) VALUES ($1, $2, $3, $4, 3, '08:00', '14:00', '2026-10-01', '{umum,spa}')",
		companyID, merchantID, physicianID, departmentID)

	// Valid service_types insert fine.
	if _, err := tx.Exec(ctx,
		"INSERT INTO operations.physician_schedule (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, effective_from, service_types) VALUES ($1, $2, $3, $4, 4, '08:00', '14:00', '2026-10-01', '{bpjs,asuransi}')",
		companyID, merchantID, physicianID, departmentID); err != nil {
		t.Fatalf("insert schedule with valid service_types: %v", err)
	}

	// chk_schedule_session_time: end <= start → 23514.
	locationExpectError(t, ctx, tx, "session inverted time", "23514",
		"INSERT INTO operations.schedule_session (company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time) VALUES ($1, $2, $3, '2026-10-05', $4, $5, '14:00', '08:00')",
		companyID, merchantID, scheduleID, physicianID, departmentID)

	// A valid session occupies (schedule_id, '2026-10-05').
	var sessionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.schedule_session
			(company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time)
		VALUES ($1, $2, $3, '2026-10-05', $4, $5, '08:00', '14:00')
		RETURNING id::text`,
		companyID, merchantID, scheduleID, physicianID, departmentID).Scan(&sessionID); err != nil {
		t.Fatalf("insert valid session: %v", err)
	}

	// chk_schedule_session_leave: status 'leave' without leave_id → 23514.
	locationExpectError(t, ctx, tx, "leave session without leave_id", "23514",
		"INSERT INTO operations.schedule_session (company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time, status) VALUES ($1, $2, $3, '2026-10-06', $4, $5, '08:00', '14:00', 'leave')",
		companyID, merchantID, scheduleID, physicianID, departmentID)

	// UNIQUE (schedule_id, session_date): same schedule + same date → 23505.
	locationExpectError(t, ctx, tx, "duplicate schedule session date", "23505",
		"INSERT INTO operations.schedule_session (company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time) VALUES ($1, $2, $3, '2026-10-05', $4, $5, '08:00', '14:00')",
		companyID, merchantID, scheduleID, physicianID, departmentID)
}

// TestScheduleDB_SessionLeaveWithLeaveID — a 'leave' session that references an
// operations.physician_leave row passes chk_schedule_session_leave.
func TestScheduleDB_SessionLeaveWithLeaveID(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "sched.leave", "081234580102")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	scheduleID, physicianID, departmentID := scheduleSeed(t, ctx, tx, companyID, merchantID, "SCHL")

	var leaveID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.physician_leave (company_id, merchant_id, physician_id, date_from, date_to, reason)
		VALUES ($1, $2, $3, '2026-10-06', '2026-10-08', 'cuti tahunan') RETURNING id::text`,
		companyID, merchantID, physicianID).Scan(&leaveID); err != nil {
		t.Fatalf("insert physician_leave: %v", err)
	}

	// A leave row cannot name the physician himself as substitute
	// (chk_physician_leave_substitute → 23514).
	locationExpectError(t, ctx, tx, "self substitute", "23514",
		"INSERT INTO operations.physician_leave (company_id, merchant_id, physician_id, date_from, date_to, substitute_physician_id) VALUES ($1, $2, $3, '2026-10-10', '2026-10-11', $3)",
		companyID, merchantID, physicianID)

	// A 'leave' session WITH leave_id is accepted.
	var sessionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.schedule_session
			(company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time, status, leave_id)
		VALUES ($1, $2, $3, '2026-10-06', $4, $5, '08:00', '14:00', 'leave', $6)
		RETURNING id::text`,
		companyID, merchantID, scheduleID, physicianID, departmentID, leaveID).Scan(&sessionID); err != nil {
		t.Fatalf("leave session with leave_id must be accepted: %v", err)
	}
}

// TestScheduleDB_RLS — a schedule_session row of merchant B is invisible to a
// transaction scoped to merchant A (RLS policy schedule_session_isolation).
func TestScheduleDB_RLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "sched.rls.a", "081234580103")

	// Seed merchant B's full chain with the owner pool (bypasses RLS).
	companyB := "e1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1"
	merchantB := "e2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2"
	personB := "e3b3b3b3-b3b3-b3b3-b3b3-b3b3b3b3b3b3"
	physicianB := "e4b4b4b4-b4b4-b4b4-b4b4-b4b4b4b4b4b4"
	departmentB := "e5b5b5b5-b5b5-b5b5-b5b5-b5b5b5b5b5b5"
	scheduleB := "e6b6b6b6-b6b6-b6b6-b6b6-b6b6b6b6b6b6"
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed B failed: %v", err)
		}
	}
	seed("INSERT INTO core.company (id, code, name) VALUES ($1, 'SCHB', 'Schedule B Co')", companyB)
	seed("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MSCHB', 'Klinik B')", merchantB, companyB)
	seed("INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Dokter B', 'male')", personB, companyB)
	seed("INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)", physicianB, companyB, merchantB, personB)
	seed("INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, 'SCHB-DEPT', 'Poli B')", departmentB, companyB, merchantB)
	seed("INSERT INTO operations.physician_schedule (id, company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, effective_from) VALUES ($1, $2, $3, $4, $5, 1, '08:00', '14:00', '2026-10-01')", scheduleB, companyB, merchantB, physicianB, departmentB)
	seed("INSERT INTO operations.schedule_session (company_id, merchant_id, schedule_id, session_date, physician_id, department_id, start_time, end_time) VALUES ($1, $2, $3, '2026-10-05', $4, $5, '08:00', '14:00')", companyB, merchantB, scheduleB, physicianB, departmentB)

	// As merchant A, merchant B's session is invisible.
	tx := locationTx(t, ctx, pool, companyA, merchantA)
	defer tx.Rollback(ctx)
	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM operations.schedule_session WHERE merchant_id = $1", merchantB).Scan(&visible); err != nil {
		t.Fatalf("count merchant B sessions as merchant A: %v", err)
	}
	if visible != 0 {
		t.Fatalf("merchant B sessions visible to merchant A: got %d rows, want 0", visible)
	}
}

// TestScheduleDB_AdmissionScheduleLink — operations.admission.schedule_id can
// be set to an existing physician_schedule id and round-trips.
func TestScheduleDB_AdmissionScheduleLink(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "sched.adm", "081234580104")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	scheduleID, _, departmentID := scheduleSeed(t, ctx, tx, companyID, merchantID, "SCHA")

	var personID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.person (company_id, full_name, gender)
		VALUES ($1, 'Pasien Rawat Jalan', 'female') RETURNING id::text`,
		companyID).Scan(&personID); err != nil {
		t.Fatalf("seed patient person: %v", err)
	}
	var admissionID, linkedSchedule string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, schedule_id)
		VALUES ($1, $2, 'SCHA-0001', $3, 'outpatient', $4, $5)
		RETURNING id::text, schedule_id::text`,
		companyID, merchantID, personID, departmentID, scheduleID).Scan(&admissionID, &linkedSchedule); err != nil {
		t.Fatalf("insert admission with schedule_id: %v", err)
	}
	if linkedSchedule != scheduleID {
		t.Fatalf("admission.schedule_id = %s, want %s", linkedSchedule, scheduleID)
	}
}
