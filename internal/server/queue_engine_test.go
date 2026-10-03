//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestQueueEngine_* covers the flow-driven queue engine (spec
// 2026-10-01-c-queue-flow-display §4): auto-provisioned default flow, journey
// advancement, check-in, numbering prefixes, flow selection per payer/department,
// permission checks, priority calling, schedule-room counters, and legacy
// tickets without a journey.

var qePhoneSeq atomic.Int64

// qeEnv is one fresh merchant (registered owner + router) per test.
type qeEnv struct {
	pool       *pgxpool.Pool
	router     *gin.Engine
	companyID  string
	merchantID string
	token      string
}

func qeNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	phone := fmt.Sprintf("0812399%05d", qePhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "qeng."+label, phone)
	return &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}
}

// qeRequest performs an authenticated tenant request against the env owner.
func qeRequest(t *testing.T, env *qeEnv, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return qeRequestToken(t, env.router, method, path, env.token, env.companyID, env.merchantID, body)
}

func qeRequestToken(t *testing.T, router *gin.Engine, method, path, token, companyID, merchantID string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *qeEnv) seedDepartment(t *testing.T, code string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $3) RETURNING id::text",
		e.companyID, e.merchantID, code).Scan(&id); err != nil {
		t.Fatalf("seed department %s: %v", code, err)
	}
	return id
}

func (e *qeEnv) seedPerson(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
		e.companyID, name).Scan(&id); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	return id
}

func (e *qeEnv) seedPayer(t *testing.T, code, payerType string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $3, $4) RETURNING id::text",
		e.companyID, e.merchantID, code, payerType).Scan(&id); err != nil {
		t.Fatalf("seed payer %s: %v", code, err)
	}
	return id
}

type qeStage struct {
	seq            int
	name           string
	kind           string
	prefix         string // "" = NULL prefix (legacy numbering)
	skippable      bool
	requiresChecki bool
	perm           string
}

// qeSeedFlow inserts a flow + its stages straight over the owner pool.
func (e *qeEnv) seedFlow(t *testing.T, name string, services []string, deptIDs []string, isDefault bool, stages []qeStage) string {
	t.Helper()
	ctx := context.Background()
	var flowID string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO operations.queue_flow (company_id, merchant_id, name, service_types, department_ids, is_default) VALUES ($1, $2, $3, $4, $5::uuid[], $6) RETURNING id::text",
		e.companyID, e.merchantID, name, services, "{"+strings.Join(deptIDs, ",")+"}", isDefault).Scan(&flowID); err != nil {
		t.Fatalf("seed flow %s: %v", name, err)
	}
	for _, s := range stages {
		var prefix *string
		if s.prefix != "" {
			prefix = &s.prefix
		}
		if _, err := e.pool.Exec(ctx,
			"INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, number_prefix, skippable, requires_checkin, served_by_permission) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
			e.companyID, e.merchantID, flowID, s.seq, s.name, s.kind, prefix, s.skippable, s.requiresChecki, s.perm); err != nil {
			t.Fatalf("seed stage %s: %v", s.name, err)
		}
	}
	return flowID
}

func (e *qeEnv) seedCounter(t *testing.T, stageID, locationID, binding, queueType, code string) string {
	t.Helper()
	ctx := context.Background()
	var stageArg any
	if stageID != "" {
		stageArg = stageID
	}
	var id string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO operations.counter (company_id, merchant_id, queue_type, stage_id, location_id, binding, code, name) VALUES ($1, $2, $3, $4::uuid, NULLIF($5,'')::uuid, $6, $7, $7) RETURNING id::text",
		e.companyID, e.merchantID, queueType, stageArg, locationID, binding, code).Scan(&id); err != nil {
		t.Fatalf("seed counter %s: %v", code, err)
	}
	return id
}

// qeAdmit registers an outpatient through POST /admissions (flowID "" = no
// explicit flow, payerID "" = no payer) and returns the admission id.
func (e *qeEnv) qeAdmit(t *testing.T, deptID, personID, payerID, flowID string, wantCode int) string {
	t.Helper()
	body := map[string]string{"person_id": personID, "department_id": deptID}
	if payerID != "" {
		body["primary_payer_id"] = payerID
	}
	if flowID != "" {
		body["flow_id"] = flowID
	}
	code, resp := qeRequest(t, e, http.MethodPost, "/api/v1/admissions", body)
	if code != wantCode {
		t.Fatalf("POST /admissions expected %d, got %d: %v", wantCode, code, resp)
	}
	if wantCode != http.StatusCreated {
		return ""
	}
	data := resp["data"].(map[string]any)
	admission := data["admission"].(map[string]any)
	return admission["id"].(string)
}

// qeTickets lists an admission's tickets: (queue_number, status, stage kind).
func (e *qeEnv) qeTickets(t *testing.T, admissionID string) []map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := e.pool.Query(ctx, `
		SELECT q.queue_number, q.status, COALESCE(st.kind, ''), q.journey_id::text IS NOT NULL
		FROM operations.queue q LEFT JOIN operations.queue_stage st ON st.id = q.stage_id
		WHERE q.admission_id = $1 ORDER BY q.created_at, q.queue_number`, admissionID)
	if err != nil {
		t.Fatalf("list tickets: %v", err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var num, status, kind string
		var hasJourney bool
		if err := rows.Scan(&num, &status, &kind, &hasJourney); err != nil {
			t.Fatalf("scan ticket: %v", err)
		}
		out = append(out, map[string]string{"number": num, "status": status, "kind": kind})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tickets: %v", err)
	}
	return out
}

// qeJourney returns (status, current stage kind) of the admission's journey.
func (e *qeEnv) qeJourney(t *testing.T, admissionID string) (string, string) {
	t.Helper()
	var status, kind string
	if err := e.pool.QueryRow(t.Context(), `
		SELECT j.status, COALESCE(st.kind, '')
		FROM operations.queue_journey j LEFT JOIN operations.queue_stage st ON st.id = j.current_stage_id
		WHERE j.admission_id = $1`, admissionID).Scan(&status, &kind); err != nil {
		t.Fatalf("read journey: %v", err)
	}
	return status, kind
}

// qeDone walks the ticket through called → in_progress → done (the linear
// state machine in queue.go) and requires 200 on every step.
func (e *qeEnv) qeDone(t *testing.T, queueID string) map[string]any {
	t.Helper()
	for _, status := range []string{"called", "in_progress", "done"} {
		code, resp := qeRequest(t, e, http.MethodPatch, "/api/v1/queue/"+queueID, map[string]string{"status": status})
		if code != http.StatusOK {
			t.Fatalf("PATCH %s expected 200, got %d: %v", status, code, resp)
		}
		if status == "done" {
			return resp
		}
	}
	return nil
}

// qeFirstWaitingTicket returns the id of the admission's oldest waiting ticket.
func (e *qeEnv) qeFirstWaitingTicket(t *testing.T, admissionID string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(t.Context(),
		"SELECT id::text FROM operations.queue WHERE admission_id = $1 AND status = 'waiting' ORDER BY created_at LIMIT 1",
		admissionID).Scan(&id); err != nil {
		t.Fatalf("find waiting ticket: %v", err)
	}
	return id
}

// qeStaffToken logs a staff user (seedMerchantStaff password "correct-horse") in.
func qeStaffToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, companyID string, username string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", companyID); err != nil {
		t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", "qeng-staff")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode staff login: %v", err)
	}
	return resp.Data.Token
}

// TestQueueEngine_DefaultFlowAutoCreatedOnce — a merchant with no flow gets
// the built-in "Rawat Jalan (bawaan)" flow created on the first admission and
// only once.
func TestQueueEngine_DefaultFlowAutoCreatedOnce(t *testing.T) {
	env := qeNewEnv(t, "auto")
	deptID := env.seedDepartment(t, "AUTO")
	personID := env.seedPerson(t, "Auto Person")

	env.qeAdmit(t, deptID, personID, "", "", http.StatusCreated)

	var flows, stages int
	var name string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT count(*), min(name) FROM operations.queue_flow WHERE merchant_id = $1", env.merchantID).Scan(&flows, &name); err != nil {
		t.Fatalf("count flows: %v", err)
	}
	if flows != 1 || name != "Rawat Jalan (bawaan)" {
		t.Fatalf("expected 1 flow 'Rawat Jalan (bawaan)', got %d %q", flows, name)
	}
	if err := env.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM operations.queue_stage st JOIN operations.queue_flow f ON f.id = st.flow_id WHERE f.merchant_id = $1", env.merchantID).Scan(&stages); err != nil {
		t.Fatalf("count stages: %v", err)
	}
	if stages != 3 {
		t.Fatalf("expected 3 stages in default flow, got %d", stages)
	}

	env.qeAdmit(t, deptID, personID, "", "", http.StatusCreated)
	if err := env.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM operations.queue_flow WHERE merchant_id = $1", env.merchantID).Scan(&flows); err != nil {
		t.Fatalf("recount flows: %v", err)
	}
	if flows != 1 {
		t.Fatalf("default flow must be created once, got %d after second admission", flows)
	}
}

// TestQueueEngine_ThreeStageJourney — the default flow advances
// pendaftaran → perawat → dokter and completes the journey after the last
// stage (no fourth ticket).
func TestQueueEngine_ThreeStageJourney(t *testing.T) {
	env := qeNewEnv(t, "three")
	deptID := env.seedDepartment(t, "THRE")
	personID := env.seedPerson(t, "Three Person")
	admissionID := env.qeAdmit(t, deptID, personID, "", "", http.StatusCreated)

	tickets := env.qeTickets(t, admissionID)
	if len(tickets) != 1 || tickets[0]["kind"] != "admission" || tickets[0]["number"] != "THRE-001" || tickets[0]["status"] != "waiting" {
		t.Fatalf("first ticket: expected THRE-001 waiting admission, got %v", tickets)
	}
	if status, kind := env.qeJourney(t, admissionID); status != "in_progress" || kind != "admission" {
		t.Fatalf("journey after admission: expected in_progress/admission, got %s/%s", status, kind)
	}

	// done pendaftaran → perawat ticket appears, journey stays in_progress.
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))
	tickets = env.qeTickets(t, admissionID)
	if len(tickets) != 2 || tickets[1]["kind"] != "nurse" || tickets[1]["number"] != "THRE-001" || tickets[1]["status"] != "waiting" {
		t.Fatalf("after stage 1 done: expected nurse ticket THRE-001, got %v", tickets)
	}
	if status, kind := env.qeJourney(t, admissionID); status != "in_progress" || kind != "nurse" {
		t.Fatalf("journey after stage 1: expected in_progress/nurse, got %s/%s", status, kind)
	}

	// done perawat → dokter ticket.
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))
	tickets = env.qeTickets(t, admissionID)
	if len(tickets) != 3 || tickets[2]["kind"] != "physician" {
		t.Fatalf("after stage 2 done: expected physician ticket, got %v", tickets)
	}

	// done dokter (last stage) → journey completed, no fourth ticket.
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))
	if status, _ := env.qeJourney(t, admissionID); status != "completed" {
		t.Fatalf("journey after last stage: expected completed")
	}
	if tickets := env.qeTickets(t, admissionID); len(tickets) != 3 {
		t.Fatalf("expected exactly 3 tickets, got %v", tickets)
	}
}

// TestQueueEngine_RequiresCheckin — a requires_checkin stage puts the journey
// in awaiting_checkin without a ticket; POST /queue/checkin at a counter of
// that stage issues the ticket; check-in again → 409 nothing to check in.
func TestQueueEngine_RequiresCheckin(t *testing.T) {
	env := qeNewEnv(t, "checkin")
	deptID := env.seedDepartment(t, "CHKI")
	personID := env.seedPerson(t, "Checkin Person")
	stage2 := qeStage{seq: 2, name: "Lab", kind: "checkin", requiresChecki: true, perm: "operations.visit.manage"}
	flowID := env.seedFlow(t, "Alur Check-in", []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
		stage2,
	})
	counterID := env.seedCounter(t, "", "", "fixed", "checkin", "CHK-LAB") // stage bound below
	if _, err := env.pool.Exec(t.Context(),
		"UPDATE operations.counter SET stage_id = (SELECT id FROM operations.queue_stage WHERE flow_id = $1 AND seq = 2) WHERE id = $2",
		flowID, counterID); err != nil {
		t.Fatalf("bind counter stage: %v", err)
	}

	admissionID := env.qeAdmit(t, deptID, personID, "", flowID, http.StatusCreated)
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))

	if status, kind := env.qeJourney(t, admissionID); status != "awaiting_checkin" || kind != "checkin" {
		t.Fatalf("journey after stage 1: expected awaiting_checkin/checkin, got %s/%s", status, kind)
	}
	if tickets := env.qeTickets(t, admissionID); len(tickets) != 1 {
		t.Fatalf("awaiting_checkin must not create a ticket, got %v", tickets)
	}

	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue/checkin", map[string]string{
		"admission_id": admissionID, "counter_id": counterID,
	})
	if code != http.StatusOK {
		t.Fatalf("checkin expected 200, got %d: %v", code, resp)
	}
	tickets := env.qeTickets(t, admissionID)
	if len(tickets) != 2 || tickets[1]["kind"] != "checkin" || tickets[1]["status"] != "waiting" {
		t.Fatalf("after checkin: expected checkin ticket waiting, got %v", tickets)
	}
	if status, _ := env.qeJourney(t, admissionID); status != "in_progress" {
		t.Fatalf("journey after checkin: expected in_progress")
	}

	// Second check-in → nothing left to check in.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue/checkin", map[string]string{
		"admission_id": admissionID, "counter_id": counterID,
	})
	if code != http.StatusConflict {
		t.Fatalf("second checkin expected 409, got %d: %v", code, resp)
	}
	if msg, _ := resp["error"].(string); msg != "nothing to check in" {
		t.Fatalf("expected 'nothing to check in', got %v", resp)
	}
}

// TestQueueEngine_PrefixNumbering — number_prefix replaces the "<poli>-NNN"
// scheme: BPJ-001 then BPJ-002 on the same day (NNN = stage tickets today + 1).
func TestQueueEngine_PrefixNumbering(t *testing.T) {
	env := qeNewEnv(t, "prefix")
	deptID := env.seedDepartment(t, "PREF")
	personID := env.seedPerson(t, "Prefix Person")
	flowID := env.seedFlow(t, "Alur Prefix", []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Kasir", kind: "cashier", prefix: "BPJ", perm: "billing.invoice.create"},
	})

	first := env.qeAdmit(t, deptID, personID, "", flowID, http.StatusCreated)
	second := env.qeAdmit(t, deptID, personID, "", flowID, http.StatusCreated)

	t1, t2 := env.qeTickets(t, first), env.qeTickets(t, second)
	if t1[0]["number"] != "BPJ-001" {
		t.Fatalf("expected BPJ-001 (prefix + 0 tickets today + 1), got %v", t1[0])
	}
	if t2[0]["number"] != "BPJ-002" {
		t.Fatalf("expected BPJ-002 (prefix + 1 ticket today + 1), got %v", t2[0])
	}
}

// TestQueueEngine_FlowSelection — explicit flow must apply (else 400), a
// matching default wins, otherwise first matching flow by name; payer category
// and department scope filter the candidates.
func TestQueueEngine_FlowSelection(t *testing.T) {
	env := qeNewEnv(t, "select")
	deptA := env.seedDepartment(t, "SELA")
	deptB := env.seedDepartment(t, "SELB")
	personID := env.seedPerson(t, "Select Person")
	selfPay := env.seedPayer(t, "SEL-SP", "self_pay")
	bpjs := env.seedPayer(t, "SEL-BP", "bpjs")

	env.seedFlow(t, "A Khusus Poli A", []string{"umum"}, []string{deptA}, false, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
	})
	bpjsFlow := env.seedFlow(t, "BPJS Utama", []string{"bpjs"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
	})
	umum := env.seedFlow(t, "Z Umum", []string{"umum"}, nil, false, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
	})

	flowOf := func(admissionID string) string {
		t.Helper()
		var name string
		if err := env.pool.QueryRow(t.Context(),
			"SELECT f.name FROM operations.queue_journey j JOIN operations.queue_flow f ON f.id = j.flow_id WHERE j.admission_id = $1",
			admissionID).Scan(&name); err != nil {
			t.Fatalf("read journey flow: %v", err)
		}
		return name
	}

	// self_pay + dept A: "A Khusus Poli A" and "Z Umum" both match umum, no
	// default matches → first by name.
	if got := flowOf(env.qeAdmit(t, deptA, personID, selfPay, "", http.StatusCreated)); got != "A Khusus Poli A" {
		t.Fatalf("self_pay deptA: expected 'A Khusus Poli A', got %q", got)
	}
	// self_pay + dept B: only "Z Umum" matches (khusus is dept-scoped).
	if got := flowOf(env.qeAdmit(t, deptB, personID, selfPay, "", http.StatusCreated)); got != "Z Umum" {
		t.Fatalf("self_pay deptB: expected 'Z Umum', got %q", got)
	}
	// bpjs + dept B: the matching default "BPJS Utama" wins.
	if got := flowOf(env.qeAdmit(t, deptB, personID, bpjs, "", http.StatusCreated)); got != "BPJS Utama" {
		t.Fatalf("bpjs: expected default 'BPJS Utama', got %q", got)
	}
	// Explicit flow that does not apply → 400 "flow does not apply".
	env.qeAdmit(t, deptB, personID, selfPay, bpjsFlow, http.StatusBadRequest)
	_ = umum
}

// TestQueueEngine_NoMatchingFlow409 — a merchant with flows but none matching
// the payer category rejects the admission with 409 "no queue flow configured".
func TestQueueEngine_NoMatchingFlow409(t *testing.T) {
	env := qeNewEnv(t, "nomatch")
	deptID := env.seedDepartment(t, "NOMA")
	personID := env.seedPerson(t, "NoMatch Person")
	selfPay := env.seedPayer(t, "NOMA-SP", "self_pay")
	env.seedFlow(t, "BPJS Saja", []string{"bpjs"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
	})

	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/admissions", map[string]string{
		"person_id": personID, "department_id": deptID, "primary_payer_id": selfPay,
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %v", code, resp)
	}
	if msg, _ := resp["error"].(string); msg != "no queue flow configured" {
		t.Fatalf("expected 'no queue flow configured', got %v", resp)
	}
}

// TestQueueEngine_ServedByPermission403 — calling at a stage-bound counter
// requires the stage's served_by_permission; a caller with only visit.manage
// gets 403, a caller that also holds the stage permission succeeds.
func TestQueueEngine_ServedByPermission403(t *testing.T) {
	env := qeNewEnv(t, "perm")
	deptID := env.seedDepartment(t, "PERM")
	personID := env.seedPerson(t, "Perm Person")
	flowID := env.seedFlow(t, "Alur Kasir", []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
		{seq: 2, name: "Kasir", kind: "cashier", perm: "billing.invoice.create"},
	})
	stage2 := env.stageID(t, flowID, 2)
	counterID := env.seedCounter(t, stage2, "", "fixed", "kasir", "PERM-KASIR")

	admissionID := env.qeAdmit(t, deptID, personID, "", flowID, http.StatusCreated)
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))

	// One staff role (seedMerchantStaff uses a fixed role id, so once per
	// test): granted the stage permission the call succeeds; after revoking
	// it the same caller gets 403.
	ctx := context.Background()
	seedMerchantStaff(t, ctx, env.pool, env.companyID, env.merchantID, "qeng-perm-staff", "", "operations.counter.manage", "operations.visit.manage", "billing.invoice.create")
	staff := qeStaffToken(t, ctx, env.pool, env.router, env.companyID, "qeng-perm-staff")

	code, resp := qeRequestToken(t, env.router, http.MethodPost, "/api/v1/counters/"+counterID+"/call-next", staff, env.companyID, env.merchantID, nil)
	if code != http.StatusOK {
		t.Fatalf("call-next with stage permission: expected 200, got %d: %v", code, resp)
	}
	if _, err := env.pool.Exec(ctx,
		"DELETE FROM core.role_permission WHERE role_id = '6f6f6f6f-6f6f-6f6f-6f6f-6f6f6f6f6f6f' AND permission_id = (SELECT id FROM core.permission WHERE code = 'billing.invoice.create')"); err != nil {
		t.Fatalf("revoke stage permission: %v", err)
	}
	code, _ = qeRequestToken(t, env.router, http.MethodPost, "/api/v1/counters/"+counterID+"/call-next", staff, env.companyID, env.merchantID, nil)
	if code != http.StatusForbidden {
		t.Fatalf("call-next without stage permission: expected 403, got %d", code)
	}
}

// stageID reads a flow stage's id by seq.
func (e *qeEnv) stageID(t *testing.T, flowID string, seq int) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(t.Context(),
		"SELECT id::text FROM operations.queue_stage WHERE flow_id = $1 AND seq = $2", flowID, seq).Scan(&id); err != nil {
		t.Fatalf("read stage seq %d: %v", seq, err)
	}
	return id
}

// TestQueueEngine_Priority — call-next at a stage counter picks the priority
// ticket even when a non-priority ticket has been waiting longer.
func TestQueueEngine_Priority(t *testing.T) {
	env := qeNewEnv(t, "prio")
	deptID := env.seedDepartment(t, "PRIO")
	personID := env.seedPerson(t, "Priority Person")
	admissionID := env.qeAdmit(t, deptID, personID, "", "", http.StatusCreated)

	var flowID string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT flow_id::text FROM operations.queue_journey WHERE admission_id = $1", admissionID).Scan(&flowID); err != nil {
		t.Fatalf("read flow: %v", err)
	}
	nurseStage := env.stageID(t, flowID, 2)
	counterID := env.seedCounter(t, nurseStage, "", "fixed", "perawat", "PRIO-NR")

	// Low priority: waiting longer (checked_in_at 1h ago); high priority:
	// checked in just now but priority=true. priority DESC wins over time.
	legacy := "INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number, stage_id, status, priority, checked_in_at) VALUES ($1, $2, 'perawat', $3, $4, $5, $6, $7, 'waiting', $8, now() - $9::interval)"
	if _, err := env.pool.Exec(t.Context(), legacy, env.companyID, env.merchantID, deptID, personID, admissionID, "PRIO-L", nurseStage, false, "1 hour"); err != nil {
		t.Fatalf("seed low-priority ticket: %v", err)
	}
	if _, err := env.pool.Exec(t.Context(), legacy, env.companyID, env.merchantID, deptID, personID, admissionID, "PRIO-H", nurseStage, true, "1 minute"); err != nil {
		t.Fatalf("seed high-priority ticket: %v", err)
	}

	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/counters/"+counterID+"/call-next", nil)
	if code != http.StatusOK {
		t.Fatalf("call-next expected 200, got %d: %v", code, resp)
	}
	var called string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT queue_number FROM operations.queue WHERE counter_id = $1 AND status = 'called'", counterID).Scan(&called); err != nil {
		t.Fatalf("read called ticket: %v", err)
	}
	if called != "PRIO-H" {
		t.Fatalf("priority ticket must be called first, got %q", called)
	}
}

// TestQueueEngine_PhysicianScheduleRoom — a physician ticket at a
// binding='schedule_room' counter gets counter_id resolved from the session
// room (schedule_session.room_id, else physician_schedule.room_id).
func TestQueueEngine_PhysicianScheduleRoom(t *testing.T) {
	env := qeNewEnv(t, "room")
	deptID := env.seedDepartment(t, "ROOM")
	personID := env.seedPerson(t, "Room Person")
	physicianID := env.seedPerson(t, "Dr Room")
	var physicianRowID string
	if err := env.pool.QueryRow(t.Context(),
		"INSERT INTO core.physician (company_id, merchant_id, person_id) VALUES ($1, $2, $3) RETURNING id::text",
		env.companyID, env.merchantID, physicianID).Scan(&physicianRowID); err != nil {
		t.Fatalf("seed physician: %v", err)
	}
	var roomID string
	if err := env.pool.QueryRow(t.Context(),
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'room', 'ROOM-R1', 'Ruang 1') RETURNING id::text",
		env.companyID, env.merchantID).Scan(&roomID); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	var scheduleID string
	if err := env.pool.QueryRow(t.Context(),
		"INSERT INTO operations.physician_schedule (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, effective_from, room_id) VALUES ($1, $2, $3, $4, 1, '08:00', '14:00', '2026-01-01', $5) RETURNING id::text",
		env.companyID, env.merchantID, physicianRowID, deptID, roomID).Scan(&scheduleID); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	flowID := env.seedFlow(t, "Alur Ruang", []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
		{seq: 2, name: "Dokter", kind: "physician", perm: "operations.visit.manage"},
	})
	physStage := env.stageID(t, flowID, 2)
	roomCounter := env.seedCounter(t, physStage, roomID, "schedule_room", "dokter", "ROOM-DOK")

	admissionID := env.qeAdmit(t, deptID, personID, "", flowID, http.StatusCreated)
	if _, err := env.pool.Exec(t.Context(),
		"UPDATE operations.admission SET schedule_id = $2 WHERE id = $1", admissionID, scheduleID); err != nil {
		t.Fatalf("attach schedule: %v", err)
	}

	// No schedule_session row for today → fall back to physician_schedule.room_id.
	env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))
	var counterID *string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT counter_id::text FROM operations.queue WHERE admission_id = $1 AND stage_id = $2 AND status = 'waiting'",
		admissionID, physStage).Scan(&counterID); err != nil {
		t.Fatalf("read physician ticket counter: %v", err)
	}
	if counterID == nil || *counterID != roomCounter {
		t.Fatalf("physician ticket must sit at the schedule-room counter %s, got %v", roomCounter, counterID)
	}
}

// TestQueueEngine_LegacyTicketWithoutJourney — a migration-day ticket with no
// journey advances once through the default flow with the old numbering and
// stays journey-less.
func TestQueueEngine_LegacyTicketWithoutJourney(t *testing.T) {
	env := qeNewEnv(t, "legacy")
	deptID := env.seedDepartment(t, "LEG")
	personID := env.seedPerson(t, "Legacy Person")
	admissionID := env.qeAdmit(t, deptID, personID, "", "", http.StatusCreated)

	var flowID string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT flow_id::text FROM operations.queue_journey WHERE admission_id = $1", admissionID).Scan(&flowID); err != nil {
		t.Fatalf("read flow: %v", err)
	}
	nurseStage := env.stageID(t, flowID, 2)

	var legacyID string
	if err := env.pool.QueryRow(t.Context(),
		"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number, stage_id, status) VALUES ($1, $2, 'perawat', $3, $4, $5, 'LEG-001', $6, 'waiting') RETURNING id::text",
		env.companyID, env.merchantID, deptID, personID, admissionID, nurseStage).Scan(&legacyID); err != nil {
		t.Fatalf("seed legacy ticket: %v", err)
	}

	env.qeDone(t, legacyID)

	var nextNumber, nextType string
	var journeyID *string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT queue_number, queue_type, journey_id::text FROM operations.queue WHERE admission_id = $1 AND queue_type = 'dokter'",
		admissionID).Scan(&nextNumber, &nextType, &journeyID); err != nil {
		t.Fatalf("legacy next ticket missing: %v", err)
	}
	// Old numbering: count dokter tickets for the department today (0) + 1.
	if nextNumber != "LEG-001" || nextType != "dokter" {
		t.Fatalf("legacy next ticket: expected LEG-001 dokter, got %s %s", nextNumber, nextType)
	}
	if journeyID != nil {
		t.Fatalf("legacy next ticket must not carry a journey_id, got %s", *journeyID)
	}
}
