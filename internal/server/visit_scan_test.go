//go:build integration

package server_test

// Integration tests for POST /queue/scan (spec
// 2026-10-20-qr-visit-media-design.md §4.2/§4.4/§4.5): scan-by-token check-in
// with layered validation, patient two-marker payload, access log, permission.

import (
	"context"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// qsEnv wraps the queue engine fixture (fresh merchant + router per test)
// with a background context.
type qsEnv struct {
	*qeEnv
	ctx context.Context
}

func qsNewEnv(t *testing.T, label string) *qsEnv {
	return &qsEnv{qeEnv: qeNewEnv(t, label), ctx: context.Background()}
}

// qsStaff seeds a merchant staff user with perms and logs it in.
func (e *qsEnv) qsStaff(t *testing.T, label string, perms ...string) string {
	t.Helper()
	seedMerchantStaff(t, e.ctx, e.pool, e.companyID, e.merchantID, label, "", perms...)
	return qeStaffToken(t, e.ctx, e.pool, e.router, e.companyID, label)
}

// qsAdmit registers an outpatient and returns (admission id, raw visit token).
func (e *qsEnv) qsAdmit(t *testing.T, personID, flowID, deptID string) (string, string) {
	t.Helper()
	code, resp := qeRequest(t, e.qeEnv, http.MethodPost, "/api/v1/admissions",
		map[string]string{"person_id": personID, "department_id": deptID, "flow_id": flowID})
	if code != http.StatusCreated {
		t.Fatalf("POST /admissions expected 201, got %d: %v", code, resp)
	}
	data := resp["data"].(map[string]any)
	admission := data["admission"].(map[string]any)
	vm := data["visit_media"].(map[string]any)
	return admission["id"].(string), vm["token"].(string)
}

// qsCheckinSetup builds a two-stage flow whose stage 2 requires check-in,
// seeds a counter at each stage, admits a person with birth_date and walks
// stage 1 to done — leaving the journey awaiting_checkin.
func (e *qsEnv) qsCheckinSetup(t *testing.T, label, fullName string) (admissionID, token, stageCounterID, otherCounterID string) {
	t.Helper()
	stage2 := qeStage{seq: 2, name: "Lab", kind: "checkin", requiresChecki: true, perm: server.PermVisitManage}
	flowID := e.seedFlow(t, "Alur Scan "+label, []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
		stage2,
	})
	stageCounterID = e.seedCounter(t, "", "", "fixed", "checkin", "CHK-"+label)
	if _, err := e.pool.Exec(e.ctx,
		"UPDATE operations.counter SET stage_id = (SELECT id FROM operations.queue_stage WHERE flow_id = $1 AND seq = 2) WHERE id = $2",
		flowID, stageCounterID); err != nil {
		t.Fatalf("bind checkin counter stage: %v", err)
	}
	// Counter at stage 1 (a different stage) for the wrong-counter case.
	otherCounterID = e.seedCounter(t, "", "", "fixed", "admission", "ADM-"+label)
	if _, err := e.pool.Exec(e.ctx,
		"UPDATE operations.counter SET stage_id = (SELECT id FROM operations.queue_stage WHERE flow_id = $1 AND seq = 1) WHERE id = $2",
		flowID, otherCounterID); err != nil {
		t.Fatalf("bind admission counter stage: %v", err)
	}

	deptID := e.seedDepartment(t, "QS"+label)
	personID := e.seedPerson(t, fullName)
	if _, err := e.pool.Exec(e.ctx,
		"UPDATE core.person SET birth_date = '1990-05-12', medical_record_no = $2 WHERE id = $1",
		personID, "RM-"+label); err != nil {
		t.Fatalf("set person birth_date/mrn: %v", err)
	}

	admissionID, token = e.qsAdmit(t, personID, flowID, deptID)
	e.qeDone(t, e.qeFirstWaitingTicket(t, admissionID))
	if status, kind := e.qeJourney(t, admissionID); status != "awaiting_checkin" {
		t.Fatalf("journey expected awaiting_checkin at %s, got %s", kind, status)
	}
	return admissionID, token, stageCounterID, otherCounterID
}

// qsScan performs POST /queue/scan with the given bearer token.
func (e *qsEnv) qsScan(t *testing.T, bearer, token, counterID string) (int, map[string]any) {
	t.Helper()
	return qeRequestToken(t, e.router, http.MethodPost, "/api/v1/queue/scan",
		bearer, e.companyID, e.merchantID,
		map[string]string{"token": token, "counter_id": counterID})
}

// TestQueueScanHappyPath — awaiting_checkin journey + valid token → 200 with
// a waiting ticket, journey back to in_progress, and the two patient markers
// (full_name + birth_date, spec §4.4).
func TestQueueScanHappyPath(t *testing.T) {
	env := qsNewEnv(t, "scanhappy")
	staff := env.qsStaff(t, "qs.happy.staff", server.PermVisitManage)
	_, token, counterID, _ := env.qsCheckinSetup(t, "H", "Siti Aminah")

	code, resp := env.qsScan(t, staff, token, counterID)
	if code != http.StatusOK {
		t.Fatalf("scan expected 200, got %d: %v", code, resp)
	}
	data := resp["data"].(map[string]any)
	queue := data["queue"].(map[string]any)
	journey := data["journey"].(map[string]any)
	patient := data["patient"].(map[string]any)
	// sqlc structs carry no json tags — the raw queue/journey payload keys are
	// PascalCase, exactly as POST /queue/checkin has always returned them.
	if queue["Status"] != "waiting" {
		t.Errorf("queue Status = %v, want waiting", queue["Status"])
	}
	if journey["Status"] != "in_progress" {
		t.Errorf("journey Status = %v, want in_progress", journey["Status"])
	}
	if patient["full_name"] != "Siti Aminah" {
		t.Errorf("patient full_name = %v, want Siti Aminah", patient["full_name"])
	}
	if patient["birth_date"] != "1990-05-12" {
		t.Errorf("patient birth_date = %v, want 1990-05-12", patient["birth_date"])
	}
	if patient["medical_record_no"] != "RM-H" {
		t.Errorf("patient medical_record_no = %v, want RM-H", patient["medical_record_no"])
	}
}

// TestQueueScanInvalidToken — wrong prefix / wrong shape → 400 invalid token.
func TestQueueScanInvalidToken(t *testing.T) {
	env := qsNewEnv(t, "scaninv")
	staff := env.qsStaff(t, "qs.inv.staff", server.PermVisitManage)
	for _, tok := range []string{"ABC", "NQP1-7X2K9M4QTR8ZHJ3F"} {
		code, resp := env.qsScan(t, staff, tok, "11111111-1111-1111-1111-111111111111")
		if code != http.StatusBadRequest || resp["error"] != "invalid token" {
			t.Errorf("scan %q = %d %v, want 400 invalid token", tok, code, resp)
		}
	}
}

// TestQueueScanUnknownToken — well-formed NQV1- token with unknown hash → 404.
func TestQueueScanUnknownToken(t *testing.T) {
	env := qsNewEnv(t, "scanunk")
	staff := env.qsStaff(t, "qs.unk.staff", server.PermVisitManage)
	code, resp := env.qsScan(t, staff, "NQV1-7X2K9M4QTR8ZHJ3F", "11111111-1111-1111-1111-111111111111")
	if code != http.StatusNotFound || resp["error"] != "visit token not found" {
		t.Fatalf("scan unknown = %d %v, want 404 visit token not found", code, resp)
	}
}

// TestQueueScanWrongCounter — counter bound to a different stage → 409
// nothing to check in (same gate as POST /queue/checkin).
func TestQueueScanWrongCounter(t *testing.T) {
	env := qsNewEnv(t, "scanwrong")
	staff := env.qsStaff(t, "qs.wrong.staff", server.PermVisitManage)
	_, token, _, otherCounterID := env.qsCheckinSetup(t, "W", "Pasien Salah Loket")

	code, resp := env.qsScan(t, staff, token, otherCounterID)
	if code != http.StatusConflict || resp["error"] != "nothing to check in" {
		t.Fatalf("scan wrong counter = %d %v, want 409 nothing to check in", code, resp)
	}
}

// TestQueueScanExpiredToken — expires_at in the past → 409 no longer active.
func TestQueueScanExpiredToken(t *testing.T) {
	env := qsNewEnv(t, "scanexp")
	staff := env.qsStaff(t, "qs.exp.staff", server.PermVisitManage)
	admissionID, token, counterID, _ := env.qsCheckinSetup(t, "E", "Pasien Kadaluarsa")

	if _, err := env.pool.Exec(env.ctx,
		"UPDATE operations.visit_token SET expires_at = now() - interval '1 hour' WHERE admission_id = $1",
		admissionID); err != nil {
		t.Fatalf("expire visit token: %v", err)
	}

	code, resp := env.qsScan(t, staff, token, counterID)
	if code != http.StatusConflict || resp["error"] != "visit token is no longer active" {
		t.Fatalf("scan expired = %d %v, want 409 visit token is no longer active", code, resp)
	}
}

// TestQueueScanAccessLog — a successful scan writes exactly one
// core.access_log row (resource=visit_token, action=scan, person_id set).
func TestQueueScanAccessLog(t *testing.T) {
	env := qsNewEnv(t, "scanlog")
	staff := env.qsStaff(t, "qs.log.staff", server.PermVisitManage)
	admissionID, token, counterID, _ := env.qsCheckinSetup(t, "L", "Pasien Log Scan")

	if code, resp := env.qsScan(t, staff, token, counterID); code != http.StatusOK {
		t.Fatalf("scan expected 200, got %d: %v", code, resp)
	}
	var personID string
	if err := env.pool.QueryRow(env.ctx,
		"SELECT person_id::text FROM operations.admission WHERE id = $1",
		admissionID).Scan(&personID); err != nil {
		t.Fatalf("find admission person: %v", err)
	}
	var logCount int
	if err := env.pool.QueryRow(env.ctx,
		"SELECT count(*) FROM core.access_log WHERE resource = 'visit_token' AND action = 'scan' AND person_id = $1",
		personID).Scan(&logCount); err != nil {
		t.Fatalf("count access_log: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("expected exactly 1 access_log row with person id, got %d", logCount)
	}
}

// TestQueueScanPermissionDenied — staff without operations.visit.manage → 403.
func TestQueueScanPermissionDenied(t *testing.T) {
	env := qsNewEnv(t, "scandeny")
	staff := env.qsStaff(t, "qs.deny.staff", "audit.log.view")
	_, token, counterID, _ := env.qsCheckinSetup(t, "D", "Pasien Tanpa Izin")

	code, resp := env.qsScan(t, staff, token, counterID)
	if code != http.StatusForbidden {
		t.Fatalf("scan without permission = %d %v, want 403", code, resp)
	}
}

// qsAdmitFull registers an outpatient and returns (admission id, raw token,
// resolved media) straight from the POST /admissions response.
func (e *qsEnv) qsAdmitFull(t *testing.T, personID, flowID, deptID string) (string, string, string) {
	t.Helper()
	code, resp := qeRequest(t, e.qeEnv, http.MethodPost, "/api/v1/admissions",
		map[string]string{"person_id": personID, "department_id": deptID, "flow_id": flowID})
	if code != http.StatusCreated {
		t.Fatalf("POST /admissions expected 201, got %d: %v", code, resp)
	}
	data := resp["data"].(map[string]any)
	admission := data["admission"].(map[string]any)
	vm := data["visit_media"].(map[string]any)
	return admission["id"].(string), vm["token"].(string), vm["media"].(string)
}

// qsCompleteSetup builds a two-stage flow with no check-in gate and admits a
// person. Stage 1 is walked to done, leaving the journey in_progress with a
// waiting stage-2 ticket. Returns (admission id, raw token, media, stage-1
// done response).
func (e *qsEnv) qsCompleteSetup(t *testing.T, label, fullName string) (admissionID, token, media string, stage1Resp map[string]any) {
	t.Helper()
	flowID := e.seedFlow(t, "Alur Selesai "+label, []string{"umum", "bpjs", "asuransi"}, nil, true, []qeStage{
		{seq: 1, name: "Pendaftaran", kind: "admission", perm: "operations.counter.manage"},
		{seq: 2, name: "Dokter", kind: "physician", perm: server.PermVisitManage},
	})
	deptID := e.seedDepartment(t, "QC"+label)
	personID := e.seedPerson(t, fullName)
	admissionID, token, media = e.qsAdmitFull(t, personID, flowID, deptID)
	stage1Resp = e.qeDone(t, e.qeFirstWaitingTicket(t, admissionID))
	if status, _ := e.qeJourney(t, admissionID); status != "in_progress" {
		t.Fatalf("journey expected in_progress, got %s", status)
	}
	return admissionID, token, media, stage1Resp
}

// TestVisitTokenClosedOnJourneyCompletion — finishing the last stage marks the
// journey completed and closes the admission's active visit token
// (close_reason='journey_completed'); the final PATCH response carries
// visit_closed:true + media, an advance while the journey is still
// in_progress does not (spec §4.5).
func TestVisitTokenClosedOnJourneyCompletion(t *testing.T) {
	env := qsNewEnv(t, "tokenclose")
	admissionID, _, media, stage1Resp := env.qsCompleteSetup(t, "TC", "Pasien Tutup Token")

	// Advance while the journey is still in_progress: stage-1 done response
	// must NOT carry visit_closed.
	if data := stage1Resp["data"].(map[string]any); data["visit_closed"] != nil {
		t.Errorf("in-progress PATCH visit_closed = %v, want absent", data["visit_closed"])
	}

	// Last stage done → journey completed + token closed.
	resp := env.qeDone(t, env.qeFirstWaitingTicket(t, admissionID))
	if status, _ := env.qeJourney(t, admissionID); status != "completed" {
		t.Fatalf("journey expected completed, got %s", status)
	}
	data := resp["data"].(map[string]any)
	if data["visit_closed"] != true {
		t.Errorf("final PATCH visit_closed = %v, want true", data["visit_closed"])
	}
	if data["media"] != media {
		t.Errorf("final PATCH media = %v, want %q", data["media"], media)
	}

	// Token row is closed with the journey_completed reason.
	var n int
	if err := env.pool.QueryRow(env.ctx, `
		SELECT count(*) FROM operations.visit_token
		WHERE admission_id = $1 AND closed_at IS NOT NULL AND close_reason = 'journey_completed'`,
		admissionID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("closed token rows = %d (err %v), want 1 with close_reason journey_completed", n, err)
	}
}

// TestQueueScanAfterCompletion — a closed token scans to 409 no longer active
// (spec §4.5).
func TestQueueScanAfterCompletion(t *testing.T) {
	env := qsNewEnv(t, "scanafter")
	staff := env.qsStaff(t, "qs.after.staff", server.PermVisitManage)
	admissionID, token, counterID, _ := env.qsCheckinSetup(t, "A", "Pasien Selesai Scan")

	// Close the token the way journey completion does, then scan.
	if _, err := env.pool.Exec(env.ctx,
		"UPDATE operations.visit_token SET closed_at = now(), close_reason = 'journey_completed' WHERE admission_id = $1 AND closed_at IS NULL",
		admissionID); err != nil {
		t.Fatalf("close token: %v", err)
	}

	code, resp := env.qsScan(t, staff, token, counterID)
	if code != http.StatusConflict || resp["error"] != "visit token is no longer active" {
		t.Fatalf("scan closed token = %d %v, want 409 visit token is no longer active", code, resp)
	}
}
