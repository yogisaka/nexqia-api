//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedExtraPhysician inserts one more physician (plus person) in the env's
// merchant and returns its id. n is the schedUUID slot base.
func seedExtraPhysician(t *testing.T, ctx context.Context, e *scheduleTestEnv, prefix byte, n int, name string) string {
	t.Helper()
	personID := schedUUID(prefix, n)
	physicianID := schedUUID(prefix, n+1)
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, $3, 'male')", personID, e.companyID, name); err != nil {
		t.Fatalf("seed person %s: %v", name, err)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)", physicianID, e.companyID, e.merchantID, personID); err != nil {
		t.Fatalf("seed physician %s: %v", name, err)
	}
	return physicianID
}

// grantVisitManage adds operations.visit.manage to the env admin role so the
// admissions endpoints accept its token.
func grantVisitManage(t *testing.T, ctx context.Context, e *scheduleTestEnv, prefix byte) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.permission (code, description, module) VALUES ($1, 'test', 'operations') ON CONFLICT (code) DO NOTHING", server.PermVisitManage); err != nil {
		t.Fatalf("grant permission: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO core.role_permission (role_id, permission_id)
		SELECT $1, id FROM core.permission WHERE code = $2
		AND NOT EXISTS (SELECT 1 FROM core.role_permission WHERE role_id = $1 AND permission_id = core.permission.id)`,
		schedUUID(prefix, 3), server.PermVisitManage); err != nil {
		t.Fatalf("grant role permission: %v", err)
	}
}

// createLeave POSTs a physician leave and returns the created id.
func (e *scheduleTestEnv) createLeave(t *testing.T, body map[string]any) (string, *httptest.ResponseRecorder) {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/physician-leaves", body, e.token)
	if rec.Code != http.StatusCreated {
		return "", rec
	}
	var got struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode leave response: %v (%s)", err, rec.Body.String())
	}
	return got.Data.ID, rec
}

// affectedList decodes the affected array of a leave response.
func affectedList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var got struct {
		Data struct {
			Affected []map[string]any `json:"affected"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode affected: %v (%s)", err, body)
	}
	return got.Data.Affected
}

// getAdmission fetches one admission and returns its data map.
func (e *scheduleTestEnv) getAdmission(t *testing.T, id string) map[string]any {
	t.Helper()
	rec := e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/admissions/%s", id), nil, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("get admission %s: expected 200, got %d: %s", id, rec.Code, rec.Body.String())
	}
	var got struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode admission: %v", err)
	}
	return got.Data
}

// createAdmission POSTs a registration and returns the admission id.
func (e *scheduleTestEnv) createAdmission(t *testing.T, body map[string]any) (string, *httptest.ResponseRecorder) {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/admissions", body, e.token)
	if rec.Code != http.StatusCreated {
		return "", rec
	}
	var got struct {
		Data struct {
			Admission struct {
				ID string `json:"id"`
			} `json:"admission"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode admission response: %v (%s)", err, rec.Body.String())
	}
	return got.Data.Admission.ID, rec
}

// TestPhysicianLeave_LeaveMarksSessionsAndAffected — a leave without
// substitute marks the range's sessions as "leave" and lists the affected
// patients; a cancelled admission disappears from the list.
func TestPhysicianLeave_LeaveMarksSessionsAndAffected(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'a', "LVA", "")
	grantVisitManage(t, ctx, e, 'a')

	scheduleID, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "quota_jkn": 4, "effective_from": "2026-10-05",
		"room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule: %d %s", rec.Code, rec.Body.String())
	}
	e.seedAdmission(t, ctx, "LVA-0001", e.payerBPJS, scheduleID, "2026-10-05T01:30:00+00:00")
	e.seedAdmission(t, ctx, "LVA-0002", e.payerPrivate, scheduleID, "2026-10-05T02:30:00+00:00")

	leaveID, rec := e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05", "reason": "cuti tahunan",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	affected := affectedList(t, rec.Body.Bytes())
	if len(affected) != 2 {
		t.Fatalf("affected = %d items, want 2: %s", len(affected), rec.Body.String())
	}
	if affected[0]["date"] != "2026-10-05" {
		t.Fatalf("affected[0].date = %v, want 2026-10-05", affected[0]["date"])
	}
	payer, _ := affected[0]["payer"].(map[string]any)
	if payer == nil || payer["type"] == "" {
		t.Fatalf("affected[0].payer missing: %v", affected[0])
	}

	s := findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s == nil || s["status"] != "leave" {
		t.Fatalf("session status = %v, want leave", s)
	}

	// The list endpoint returns the leave.
	rec = e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/physician-leaves?merchant_id=%s&from=2026-10-01&to=2026-10-31", e.merchantID), nil, e.token)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Data) != 1 {
		t.Fatalf("list leaves: %d %s", rec.Code, rec.Body.String())
	}

	// Resolve one affected admission as cancel → it leaves the affected list.
	admID, _ := affected[0]["id"].(string)
	rec = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/physician-leaves/%s/affected/%s/resolve", leaveID, admID),
		map[string]any{"action": "cancel"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve cancel: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	adm := e.getAdmission(t, admID)
	if adm["status"] != "cancelled" {
		t.Fatalf("admission status = %v, want cancelled", adm["status"])
	}
	rec = e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/physician-leaves/%s/affected", leaveID), nil, e.token)
	var aff struct {
		Data struct {
			Affected []map[string]any `json:"affected"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &aff); err != nil || len(aff.Data.Affected) != 1 {
		t.Fatalf("affected after cancel = %d, want 1 (%s)", len(aff.Data.Affected), rec.Body.String())
	}
}

// TestPhysicianLeave_SubstituteConflict409 — a substitute with overlapping
// sessions on the replaced sessions is rejected with 409 + dates.
func TestPhysicianLeave_SubstituteConflict409(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'b', "LVB", "")

	_, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule: %d %s", rec.Code, rec.Body.String())
	}
	sub := seedExtraPhysician(t, ctx, e, 'b', 30, "Dokter Pengganti LVB")
	origPhys := e.physicianID
	e.physicianID = sub
	_, rec = e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 5, "effective_from": "2026-10-01", "room_id": e.practiceRoom2,
	})
	e.physicianID = origPhys
	if rec.Code != http.StatusCreated {
		t.Fatalf("create substitute schedule: %d %s", rec.Code, rec.Body.String())
	}

	_, rec = e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05",
		"substitute_physician_id": sub,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting substitute: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string   `json:"error"`
		Dates []string `json:"dates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error != "substitute physician has overlapping sessions" {
		t.Fatalf("conflict body = %s", rec.Body.String())
	}
	if len(got.Dates) != 1 || got.Dates[0] != "2026-10-05" {
		t.Fatalf("dates = %v, want [2026-10-05]", got.Dates)
	}
}

// TestPhysicianLeave_SubstituteAndResolveActions — with a non-conflicting
// substitute the sessions become "substituted"; the three resolve actions
// behave per spec and foreign admissions are 404.
func TestPhysicianLeave_SubstituteAndResolveActions(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'c', "LVC", "")
	grantVisitManage(t, ctx, e, 'c')

	scheduleID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	sub := seedExtraPhysician(t, ctx, e, 'c', 30, "Dokter Pengganti LVC")
	e.seedAdmission(t, ctx, "LVC-0001", e.payerBPJS, scheduleID, "2026-10-05T01:30:00+00:00")
	e.seedAdmission(t, ctx, "LVC-0002", e.payerPrivate, scheduleID, "2026-10-05T02:30:00+00:00")

	leaveID, rec := e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05",
		"substitute_physician_id": sub,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave: %d %s", rec.Code, rec.Body.String())
	}
	s := findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s == nil || s["status"] != "substituted" {
		t.Fatalf("session status = %v, want substituted", s)
	}
	affected := affectedList(t, rec.Body.Bytes())
	if len(affected) != 2 {
		t.Fatalf("affected = %d, want 2", len(affected))
	}
	adm1, _ := affected[0]["id"].(string)
	adm2, _ := affected[1]["id"].(string)

	// substitute: admission physician becomes the substitute, schedule stays.
	rec = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/physician-leaves/%s/affected/%s/resolve", leaveID, adm1),
		map[string]any{"action": "substitute"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve substitute: %d %s", rec.Code, rec.Body.String())
	}
	if adm := e.getAdmission(t, adm1); adm["physician_id"] != sub {
		t.Fatalf("admission physician = %v, want %s", adm["physician_id"], sub)
	}

	// reschedule without target_date → 400.
	rec = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/physician-leaves/%s/affected/%s/resolve", leaveID, adm2),
		map[string]any{"action": "reschedule"}, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reschedule without target: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/physician-leaves/%s/affected/%s/resolve", leaveID, adm2),
		map[string]any{"action": "reschedule", "target_date": "2026-10-12"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("reschedule: %d %s", rec.Code, rec.Body.String())
	}
	adm := e.getAdmission(t, adm2)
	if adm["status"] != "cancelled" || !strings.Contains(fmt.Sprint(adm["note"]), "dijadwal ulang ke 2026-10-12") {
		t.Fatalf("rescheduled admission = status %v note %v", adm["status"], adm["note"])
	}

	// An admission that is not part of the leave → 404.
	rec = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/physician-leaves/%s/affected/%s/resolve", leaveID, schedUUID('c', 99)),
		map[string]any{"action": "cancel"}, e.token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign admission: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPhysicianLeave_DeleteRestoresSessions — deleting a leave restores the
// sessions to the pattern physician; pure rows (no override, no admission)
// are removed, rows with admissions stay as plain overrides.
func TestPhysicianLeave_DeleteRestoresSessions(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'd', "LVD", "")

	scheduleID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	leaveID, rec := e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05", "reason": "cuti",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/physician-leaves/%s", leaveID), nil, e.token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete leave: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	s := findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s == nil || s["projected"] != true || s["status"] != "available" {
		t.Fatalf("session after delete = %v, want projected available", s)
	}

	// Second leave with a registered patient: the stored row must survive as
	// a plain active override (not deleted).
	leaveID, rec = e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05", "reason": "cuti lagi",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave 2: %d %s", rec.Code, rec.Body.String())
	}
	e.seedAdmission(t, ctx, "LVD-0001", e.payerPrivate, scheduleID, "2026-10-05T01:30:00+00:00")
	rec = e.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/physician-leaves/%s", leaveID), nil, e.token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete leave 2: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	s = findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s == nil || s["projected"] != false || s["status"] != "available" {
		t.Fatalf("session with patient after delete = %v, want stored available", s)
	}
}

// TestPhysicianLeave_CopyWeekSkipsConflicts — copy-week copies active stored
// overrides one week ahead and skips targets that already have a session.
func TestPhysicianLeave_CopyWeekSkipsConflicts(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'e', "LVE", "")

	scheduleID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	rec := e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-05", scheduleID),
		map[string]any{"notes": "override minggu 1"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("override session: %d %s", rec.Code, rec.Body.String())
	}

	copyURL := fmt.Sprintf("/api/v1/merchants/%s/schedule/copy-week", e.merchantID)
	rec = e.do(t, http.MethodPost, copyURL,
		map[string]any{"from_week_start": "2026-10-05", "to_week_start": "2026-10-12"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("copy week: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			Copied  int              `json:"copied"`
			Skipped []map[string]any `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Data.Copied != 1 || len(got.Data.Skipped) != 0 {
		t.Fatalf("first copy = %+v (%s)", got.Data, rec.Body.String())
	}

	// Copying again: the target already has a stored session → skipped.
	rec = e.do(t, http.MethodPost, copyURL,
		map[string]any{"from_week_start": "2026-10-05", "to_week_start": "2026-10-12"}, e.token)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Data.Copied != 0 || len(got.Data.Skipped) != 1 {
		t.Fatalf("second copy = %+v (%s)", got.Data, rec.Body.String())
	}
	if got.Data.Skipped[0]["schedule_id"] != scheduleID || got.Data.Skipped[0]["date"] != "2026-10-12" || got.Data.Skipped[0]["reason"] == "" {
		t.Fatalf("skipped[0] = %v", got.Data.Skipped[0])
	}

	// Non-Monday week starts → 400.
	rec = e.do(t, http.MethodPost, copyURL,
		map[string]any{"from_week_start": "2026-10-06", "to_week_start": "2026-10-13"}, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tuesday week start: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPhysicianLeave_CSVExport — the calendar CSV endpoint serves text/csv
// with the Indonesian headers.
func TestPhysicianLeave_CSVExport(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'f', "LVF", "")

	_, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "quota_jkn": 4, "effective_from": "2026-10-05",
		"room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule: %d %s", rec.Code, rec.Body.String())
	}
	// Setup (assertion untouched): mark the Monday session as leave so the CSV
	// exercises the leave status value of schedule.Status.
	if _, rec := e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": "2026-10-05", "date_to": "2026-10-05", "reason": "cuti ekspor",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create leave for csv: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/merchants/%s/schedule/calendar.csv?from=2026-10-05&to=2026-10-11", e.merchantID), nil, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("csv export: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %s, want text/csv", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"tanggal", "poli", "dokter", "kuota JKN", "kuota non-JKN", "terdaftar lain", "status", "Poli LVF", "leave"} {
		if !strings.Contains(body, want) {
			t.Fatalf("csv missing %q in:\n%s", want, body)
		}
	}
}

// TestAdmission_ScheduleQuota — registering with a schedule_id enforces the
// session quota (combined & split), validates the pattern, replaces the
// physician with the substitute on substituted sessions, and registration
// without schedule_id still works.
func TestAdmission_ScheduleQuota(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '0', "AQ", "")
	grantVisitManage(t, ctx, e, '0')

	// All patterns sit on today's weekday (merchant tz Asia/Jakarta) so the
	// registration date matches.
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	wd := int(today.Weekday())
	effFrom := today.AddDate(0, 0, -7).Format("2006-01-02")

	combinedID, rec := e.createSchedule(t, map[string]any{
		"day_of_week": wd, "start_time": "08:00", "end_time": "09:00",
		"slot_quota": 1, "quota_jkn": 0, "effective_from": effFrom,
		"room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create combined schedule: %d %s", rec.Code, rec.Body.String())
	}

	body := func(payer, schedule string) map[string]any {
		return map[string]any{
			"person_id": e.patientID, "department_id": e.departmentID,
			"physician_id": e.physicianID, "primary_payer_id": payer,
			"schedule_id": schedule,
		}
	}

	// Combined: slot_quota 1 — first registration passes, second hits 409.
	admID, rec := e.createAdmission(t, body(e.payerPrivate, combinedID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first admission: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if adm := e.getAdmission(t, admID); adm["physician_id"] != e.physicianID {
		t.Fatalf("admission physician = %v, want pattern physician", adm["physician_id"])
	}
	_, rec = e.createAdmission(t, body(e.payerBPJS, combinedID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second admission: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if errBody := func() string {
		var got struct {
			Error string `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &got)
		return got.Error
	}(); errBody != "session quota full" {
		t.Fatalf("error = %q, want session quota full", errBody)
	}

	// Split: quota_jkn 1 + slot_quota 1 — the second bpjs admission is 409,
	// a private admission still passes (separate pool).
	splitID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": wd, "start_time": "10:00", "end_time": "11:00",
		"slot_quota": 1, "quota_jkn": 1, "effective_from": effFrom,
		"room_id": e.practiceRoom,
	})
	if _, rec = e.createAdmission(t, body(e.payerBPJS, splitID)); rec.Code != http.StatusCreated {
		t.Fatalf("split bpjs: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, rec = e.createAdmission(t, body(e.payerBPJS, splitID)); rec.Code != http.StatusConflict {
		t.Fatalf("split bpjs 2: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, rec = e.createAdmission(t, body(e.payerPrivate, splitID)); rec.Code != http.StatusCreated {
		t.Fatalf("split private: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Schedule/physician mismatch → 400 "schedule does not match".
	sub := seedExtraPhysician(t, ctx, e, '0', 30, "Dokter Lain AQ")
	_, rec = e.createAdmission(t, map[string]any{
		"person_id": e.patientID, "department_id": e.departmentID,
		"physician_id": sub, "primary_payer_id": e.payerPrivate,
		"schedule_id": combinedID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatch: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Leave-marked session → 409.
	leaveID, rec := e.createLeave(t, map[string]any{
		"merchant_id": e.merchantID, "physician_id": e.physicianID,
		"date_from": today.Format("2006-01-02"), "date_to": today.Format("2006-01-02"),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave: %d %s", rec.Code, rec.Body.String())
	}
	_, rec = e.createAdmission(t, body(e.payerPrivate, splitID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("leave session admission: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/physician-leaves/%s", leaveID), nil, e.token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete leave: %d %s", rec.Code, rec.Body.String())
	}

	// Without schedule_id the legacy behavior is unchanged.
	for i := 0; i < 2; i++ {
		_, rec = e.createAdmission(t, map[string]any{
			"person_id": e.patientID, "department_id": e.departmentID,
			"physician_id": e.physicianID, "primary_payer_id": e.payerPrivate,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("admission without schedule: expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
	}
}
