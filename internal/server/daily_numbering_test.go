//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestDailyNumbering_* and TestQueueNumbering_* cover spec
// 2026-10-06-daily-numbering-integrity: merchant-local "today" for daily
// sequences/widgets and the advisory lock that keeps concurrent
// registrations from sharing a number.

// dnTimezone (UTC+14) is far from UTC and from the usual dev/CI zones, so
// the merchant's local date differs from the DB/process date for much of
// the day.
const dnTimezone = "Pacific/Kiritimati"

// dnNewEnv is qeNewEnv with a high API rate limit (concurrent bursts) and
// the merchant timezone set to tz.
func dnNewEnv(t *testing.T, label, tz string) *qeEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 6000
	cfg.RateLimitAPIBurst = 1000
	router := server.NewRouter(pool, redisClient, cfg)
	phone := fmt.Sprintf("0812388%05d", qePhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dnum."+label, phone)
	if _, err := pool.Exec(ctx, "UPDATE core.merchant SET timezone = $1 WHERE id = $2", tz, merchantID); err != nil {
		t.Fatalf("set merchant timezone: %v", err)
	}
	return &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}
}

// dnLocalMidnight is today's midnight in tz.
func dnLocalMidnight(t *testing.T, tz string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	y, m, d := time.Now().In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// dnPost sends one tenant POST as the env owner without touching t, so it
// is safe to call from goroutines.
func dnPost(env *qeEnv, path string, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("X-Company-ID", env.companyID)
	req.Header.Set("X-Merchant-ID", env.merchantID)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// dnConcurrentAdmit fires one POST /admissions per (department, person)
// pair at once and returns the status codes in input order.
func dnConcurrentAdmit(env *qeEnv, depts, persons []string) []int {
	codes := make([]int, len(depts))
	var wg sync.WaitGroup
	for i := range depts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = dnPost(env, "/api/v1/admissions", map[string]string{"person_id": persons[i], "department_id": depts[i]})
		}(i)
	}
	wg.Wait()
	return codes
}

// dnSeedAdmission inserts an admission with an explicit admission_at.
func (e *qeEnv) dnSeedAdmission(t *testing.T, deptID, personID, visitNo string, at time.Time) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		"INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, admission_at) VALUES ($1, $2, $3, $4, 'outpatient', $5, $6)",
		e.companyID, e.merchantID, visitNo, personID, deptID, at); err != nil {
		t.Fatalf("seed admission %s: %v", visitNo, err)
	}
}

// TestDailyNumbering_ConcurrentRegistrationUniqueVisitNo — concurrent
// registrations to one department get consecutive, unique visit numbers
// instead of a UNIQUE (merchant_id, visit_no) 500.
func TestDailyNumbering_ConcurrentRegistrationUniqueVisitNo(t *testing.T) {
	env := dnNewEnv(t, "visitconc", dnTimezone)
	deptID := env.seedDepartment(t, "DNC")
	// Warm-up: provisions the default flow before the burst (that race is
	// covered by TestQueueNumbering_DefaultFlowProvisionedOnce).
	env.qeAdmit(t, deptID, env.seedPerson(t, "Warmup"), "", "", http.StatusCreated)

	const n = 8
	depts := make([]string, n)
	persons := make([]string, n)
	for i := 0; i < n; i++ {
		depts[i] = deptID
		persons[i] = env.seedPerson(t, fmt.Sprintf("Concurrent %d", i))
	}
	for i, code := range dnConcurrentAdmit(env, depts, persons) {
		if code != http.StatusCreated {
			t.Fatalf("registration %d expected 201, got %d", i, code)
		}
	}

	rows, err := env.pool.Query(context.Background(),
		"SELECT visit_no FROM operations.admission WHERE merchant_id = $1 AND department_id = $2 ORDER BY visit_no",
		env.merchantID, deptID)
	if err != nil {
		t.Fatalf("list visit numbers: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan visit_no: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate visit numbers: %v", err)
	}
	if len(got) != n+1 {
		t.Fatalf("expected %d admissions, got %d: %v", n+1, len(got), got)
	}
	prefix := "DNC" + dnLocalMidnight(t, dnTimezone).Format("20060102") + "-"
	for i, v := range got {
		if want := fmt.Sprintf("%s%04d", prefix, i+1); v != want {
			t.Fatalf("visit_no[%d] = %s, want %s (all: %v)", i, v, want, got)
		}
	}
}

// TestDailyNumbering_VisitNoUsesMerchantLocalDate — an admission just before
// local midnight belongs to yesterday: today's first visit is -0001 and
// carries the merchant-local date.
func TestDailyNumbering_VisitNoUsesMerchantLocalDate(t *testing.T) {
	env := dnNewEnv(t, "visitdate", dnTimezone)
	deptID := env.seedDepartment(t, "DND")
	midnight := dnLocalMidnight(t, dnTimezone)
	env.dnSeedAdmission(t, deptID, env.seedPerson(t, "Yesterday"), "DND-SEED-1", midnight.Add(-30*time.Minute))

	admissionID := env.qeAdmit(t, deptID, env.seedPerson(t, "Today"), "", "", http.StatusCreated)
	var visitNo string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT visit_no FROM operations.admission WHERE id = $1", admissionID).Scan(&visitNo); err != nil {
		t.Fatalf("read visit_no: %v", err)
	}
	if want := "DND" + midnight.Format("20060102") + "-0001"; visitNo != want {
		t.Fatalf("visit_no = %s, want %s", visitNo, want)
	}
}

// TestDailyNumbering_PayerSummaryCountsMerchantToday — the "Jenis Pasien"
// widget counts from the merchant's local midnight, not the DB's date.
func TestDailyNumbering_PayerSummaryCountsMerchantToday(t *testing.T) {
	env := dnNewEnv(t, "payer", dnTimezone)
	deptID := env.seedDepartment(t, "DNP")
	midnight := dnLocalMidnight(t, dnTimezone)
	env.dnSeedAdmission(t, deptID, env.seedPerson(t, "Before"), "DNP-SEED-1", midnight.Add(-time.Hour))
	env.dnSeedAdmission(t, deptID, env.seedPerson(t, "After"), "DNP-SEED-2", midnight.Add(time.Minute))

	code, resp := qeRequest(t, env, http.MethodGet, "/api/v1/admissions/payer-summary", nil)
	if code != http.StatusOK {
		t.Fatalf("payer-summary expected 200, got %d: %v", code, resp)
	}
	rows, ok := resp["data"].([]any)
	if !ok {
		t.Fatalf("payer-summary data is not a list: %v", resp)
	}
	total := 0.0
	for _, r := range rows {
		total += r.(map[string]any)["Total"].(float64)
	}
	if total != 1 {
		t.Fatalf("payer-summary total = %v, want 1 (only the admission after local midnight): %v", total, rows)
	}
}
