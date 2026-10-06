//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestQueueNumbering_ConcurrentStageTickets — registrations to different
// departments share the default flow's first stage but not the visit lock,
// so only the queue-stage lock keeps their ticket numbers apart.
func TestQueueNumbering_ConcurrentStageTickets(t *testing.T) {
	env := dnNewEnv(t, "stageconc", dnTimezone)
	env.qeAdmit(t, env.seedDepartment(t, "QW0"), env.seedPerson(t, "Warmup"), "", "", http.StatusCreated)

	const n = 6
	depts := make([]string, n)
	persons := make([]string, n)
	for i := 0; i < n; i++ {
		depts[i] = env.seedDepartment(t, fmt.Sprintf("QC%d", i))
		persons[i] = env.seedPerson(t, fmt.Sprintf("Stage %d", i))
	}
	for i, code := range dnConcurrentAdmit(env, depts, persons) {
		if code != http.StatusCreated {
			t.Fatalf("registration %d expected 201, got %d", i, code)
		}
	}

	rows, err := env.pool.Query(context.Background(), `
		SELECT q.queue_number FROM operations.queue q
		JOIN operations.queue_stage st ON st.id = q.stage_id
		WHERE q.merchant_id = $1 AND st.seq = 1`, env.merchantID)
	if err != nil {
		t.Fatalf("list stage-1 tickets: %v", err)
	}
	defer rows.Close()
	var suffixes []string
	for rows.Next() {
		var number string
		if err := rows.Scan(&number); err != nil {
			t.Fatalf("scan queue_number: %v", err)
		}
		suffixes = append(suffixes, number[strings.LastIndex(number, "-")+1:])
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tickets: %v", err)
	}
	sort.Strings(suffixes)
	if len(suffixes) != n+1 {
		t.Fatalf("expected %d stage-1 tickets, got %d: %v", n+1, len(suffixes), suffixes)
	}
	for i, s := range suffixes {
		if want := fmt.Sprintf("%03d", i+1); s != want {
			t.Fatalf("stage-1 ticket sequence[%d] = %s, want %s (all: %v)", i, s, want, suffixes)
		}
	}
}

// TestQueueNumbering_DefaultFlowProvisionedOnce — the first registrations of
// a merchant without any flow race to auto-provision the built-in flow;
// exactly one is created and none fails. Different departments, so the
// visit lock does not serialize them first.
func TestQueueNumbering_DefaultFlowProvisionedOnce(t *testing.T) {
	env := dnNewEnv(t, "flowonce", dnTimezone)
	const n = 4
	depts := make([]string, n)
	persons := make([]string, n)
	for i := 0; i < n; i++ {
		depts[i] = env.seedDepartment(t, fmt.Sprintf("QF%d", i))
		persons[i] = env.seedPerson(t, fmt.Sprintf("First %d", i))
	}
	for i, code := range dnConcurrentAdmit(env, depts, persons) {
		if code != http.StatusCreated {
			t.Fatalf("registration %d expected 201, got %d", i, code)
		}
	}
	var defaults int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM operations.queue_flow WHERE merchant_id = $1 AND is_default AND deleted_at IS NULL",
		env.merchantID).Scan(&defaults); err != nil {
		t.Fatalf("count default flows: %v", err)
	}
	if defaults != 1 {
		t.Fatalf("default flows = %d, want 1", defaults)
	}
}

// TestQueueNumbering_LegacyCallNextOnce — two stage-less counters calling at
// once with one waiting ticket: exactly one gets it, the other gets 404.
func TestQueueNumbering_LegacyCallNextOnce(t *testing.T) {
	env := dnNewEnv(t, "callnext", dnTimezone)
	deptID := env.seedDepartment(t, "QL")
	personID := env.seedPerson(t, "Legacy Caller")
	counterA := env.seedCounter(t, "", "", "fixed", "dokter", "QL-A")
	counterB := env.seedCounter(t, "", "", "fixed", "dokter", "QL-B")
	var ticketID string
	if err := env.pool.QueryRow(context.Background(),
		"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, queue_number, status) VALUES ($1, $2, 'dokter', $3, $4, 'QL-001', 'waiting') RETURNING id::text",
		env.companyID, env.merchantID, deptID, personID).Scan(&ticketID); err != nil {
		t.Fatalf("seed waiting ticket: %v", err)
	}

	counters := []string{counterA, counterB}
	codes := make([]int, len(counters))
	var wg sync.WaitGroup
	for i := range counters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = dnPost(env, "/api/v1/counters/"+counters[i]+"/call-next", nil)
		}(i)
	}
	wg.Wait()
	sort.Ints(codes)
	if codes[0] != http.StatusOK || codes[1] != http.StatusNotFound {
		t.Fatalf("call-next codes = %v, want [200 404]", codes)
	}
	var status string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT status FROM operations.queue WHERE id = $1", ticketID).Scan(&status); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if status != "called" {
		t.Fatalf("ticket status = %s, want called", status)
	}
}

// TestQueueNumbering_AllFlowsInactive409 — a merchant whose only flow (the
// built-in default) was deactivated gets 409 "no queue flow configured" on
// registration, not a 500 from re-creating the default.
func TestQueueNumbering_AllFlowsInactive409(t *testing.T) {
	env := dnNewEnv(t, "inactive", dnTimezone)
	deptID := env.seedDepartment(t, "QI")
	env.qeAdmit(t, deptID, env.seedPerson(t, "First"), "", "", http.StatusCreated)
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE operations.queue_flow SET is_active = false WHERE merchant_id = $1", env.merchantID); err != nil {
		t.Fatalf("deactivate flows: %v", err)
	}
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/admissions",
		map[string]string{"person_id": env.seedPerson(t, "Second"), "department_id": deptID})
	if code != http.StatusConflict || resp["error"] != "no queue flow configured" {
		t.Fatalf("registration with every flow inactive = %d %v, want 409 no queue flow configured", code, resp)
	}
	var flows int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM operations.queue_flow WHERE merchant_id = $1 AND deleted_at IS NULL", env.merchantID).Scan(&flows); err != nil {
		t.Fatalf("count flows: %v", err)
	}
	if flows != 1 {
		t.Fatalf("flows = %d, want 1 (no new default created)", flows)
	}
}
