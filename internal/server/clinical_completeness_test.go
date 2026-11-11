//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// Daily clinical record completeness board (plan 2026-10-21-cppt-rajal-b
// Task 3, spec §11). Fixtures reuse the shared clinical helpers; expectations
// come from the contract: (a) a visit with nothing → checks incomplete,
// overall belum_lengkap; (b) a SOAP draft → incomplete + "belum sign";
// (c) everything signed + diagnosis + countersign done → lengkap;
// (d) a signed nurse entry without counter-sign → countersign pending;
// (e) ?date_from=&date_to= only covers the merchant-local day range.
func TestClinicalCompleteness(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "cc")
	cdSeedICD11(t, ctx, pool)
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "cc")

	path := "/api/v1/clinical/completeness"

	// Neither ?date_from=&date_to= nor ?admission_id= → 400 with the contract message.
	rec, resp := cfDo(t, router, http.MethodGet, path, token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest || resp["error"] != "parameter date_from/date_to atau admission_id wajib" {
		t.Fatalf("missing params = %d %v, want 400", rec.Code, resp)
	}

	get := func(query string) []any {
		rec, resp := cfDo(t, router, http.MethodGet, path+query, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		data, ok := resp["data"].([]any)
		if !ok {
			t.Fatalf("GET %s: data = %v, want array", query, resp["data"])
		}
		return data
	}
	findCheck := func(row map[string]any, key string) map[string]any {
		for _, c := range row["checks"].([]any) {
			chk := c.(map[string]any)
			if chk["key"] == key {
				return chk
			}
		}
		t.Fatalf("check %s missing in %v", key, row["checks"])
		return nil
	}

	// (a) Fresh visit: nothing filled yet.
	data := get("?admission_id=" + admissionID)
	if len(data) != 1 {
		t.Fatalf("(a) rows = %d, want 1", len(data))
	}
	row := data[0].(map[string]any)
	if row["admission_id"] != admissionID || row["overall"] != "belum_lengkap" {
		t.Fatalf("(a) row = %v", row)
	}
	if row["person_name"] != "Pasien CPPT cc" || row["visit_no"] == "" || row["department_name"] == "" {
		t.Fatalf("(a) identity fields = %v", row)
	}
	for _, key := range []string{"ases_awal_perawat", "soap_dokter", "diagnosis", "resume_rajal"} {
		if chk := findCheck(row, key); chk["status"] != "incomplete" {
			t.Fatalf("(a) check %s = %v, want incomplete", key, chk)
		}
	}
	// No counter-sign-category entries at all → complete (contract rule).
	if chk := findCheck(row, "countersign"); chk["status"] != "complete" {
		t.Fatalf("(a) countersign = %v, want complete", chk)
	}

	// (b) SOAP draft → incomplete + label "belum sign".
	soapID := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	row = get("?admission_id=" + admissionID)[0].(map[string]any)
	if chk := findCheck(row, "soap_dokter"); chk["status"] != "incomplete" || chk["label"] != "belum sign" {
		t.Fatalf("(b) soap check = %v, want incomplete + belum sign", chk)
	}
	if row["overall"] != "belum_lengkap" {
		t.Fatalf("(b) overall = %v", row["overall"])
	}

	// (d) Signed nurse assessment WITHOUT counter-sign → countersign pending.
	nurseData := `{"sections":{"keluhan":"Pusing sejak pagi."}}`
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_awal_perawat", "form_data": cfRawJSON(t, nurseData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("(d) create nurse draft = %d: %s", rec.Code, rec.Body.String())
	}
	nurseID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+nurseID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(d) sign nurse = %d: %s", rec.Code, rec.Body.String())
	}
	row = get("?admission_id=" + admissionID)[0].(map[string]any)
	if chk := findCheck(row, "ases_awal_perawat"); chk["status"] != "complete" {
		t.Fatalf("(d) nurse check = %v, want complete", chk)
	}
	if chk := findCheck(row, "countersign"); chk["status"] != "pending" {
		t.Fatalf("(d) countersign = %v, want pending", chk)
	}

	// (c) Sign the SOAP (persisting its diagnosis), sign a resume, and
	// counter-sign the nurse entry → overall lengkap.
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+soapID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(c) sign soap = %d: %s", rec.Code, rec.Body.String())
	}
	resumeData := `{"sections":{}}`
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "resume_rajal", "form_data": cfRawJSON(t, resumeData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("(c) create resume draft = %d: %s", rec.Code, rec.Body.String())
	}
	resumeID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+resumeID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(c) sign resume = %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+nurseID+"/countersign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(c) countersign nurse = %d: %s", rec.Code, rec.Body.String())
	}
	row = get("?admission_id=" + admissionID)[0].(map[string]any)
	if row["overall"] != "lengkap" {
		t.Fatalf("(c) overall = %v, want lengkap", row["overall"])
	}
	for _, chk := range row["checks"].([]any) {
		if chk.(map[string]any)["status"] != "complete" {
			t.Fatalf("(c) checks not all complete: %v", row["checks"])
		}
	}

	// (e) ?date_from=&date_to= covers only the merchant-local day: the today admission is
	// listed; a directly-inserted yesterday admission is not.
	var tz string
	if err := pool.QueryRow(ctx,
		"SELECT timezone FROM core.merchant WHERE id = $1::uuid", merchantID).Scan(&tz); err != nil {
		t.Fatalf("read merchant timezone: %v", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz %s: %v", tz, err)
	}
	now := time.Now().In(loc)
	y, m, d := now.Date()
	dateStr := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	data = get("?date_from=" + dateStr + "&date_to=" + dateStr)
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admissionID {
		t.Fatalf("(e) today rows = %v, want only %s", data, admissionID)
	}

	var departmentID string
	if err := pool.QueryRow(ctx,
		"SELECT department_id::text FROM operations.admission WHERE id = $1::uuid", admissionID).Scan(&departmentID); err != nil {
		t.Fatalf("read department: %v", err)
	}
	yesterday := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(-12 * time.Hour)
	if _, err := pool.Exec(ctx,
		`INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, status, admission_at)
		 VALUES ($1, $2, $3, $4, 'outpatient', $5::uuid, 'registered', $6)`,
		companyID, merchantID, fmt.Sprintf("CC-%d-YESTERDAY", time.Now().UnixNano()), personID, departmentID, yesterday); err != nil {
		t.Fatalf("seed yesterday admission: %v", err)
	}
	data = get("?date_from=" + dateStr + "&date_to=" + dateStr)
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admissionID {
		t.Fatalf("(e) rows after yesterday insert = %v, want only %s", data, admissionID)
	}

	// --- Fase 2a (plan 2026-11-04-clinical-board-fase2a Task 1): visit
	// metadata, filters, pagination, cancelled exclusion, queue priority.
	// Expectations are derived from the contract/spec §API-1, not from the
	// handler output. Extra fixtures are direct inserts so queue tickets stay
	// under control (the API auto-issues a pendaftaran ticket).

	// Second person + second department + extra today admissions.
	var person2ID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'Pasien Klinik Lain', 'female') RETURNING id::text",
		companyID).Scan(&person2ID); err != nil {
		t.Fatalf("seed person2: %v", err)
	}
	var dept2ID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, 'CC2-D2', 'CC2-D2') RETURNING id::text",
		companyID, merchantID).Scan(&dept2ID); err != nil {
		t.Fatalf("seed department2: %v", err)
	}
	seedToday := func(personID, deptID, status string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, status, admission_at)
			 VALUES ($1, $2, $3, $4, 'outpatient', $5::uuid, $6, $7) RETURNING id::text`,
			companyID, merchantID, fmt.Sprintf("CC2-%d", cfNext()), personID, deptID, status,
			time.Date(y, m, d, 12, 0, 0, 0, loc)).Scan(&id); err != nil {
			t.Fatalf("seed today admission (%s): %v", status, err)
		}
		return id
	}
	admB := seedToday(person2ID, dept2ID, "registered") // different person + department, no queue ticket
	admP := seedToday(personID, departmentID, "registered")
	admC := seedToday(person2ID, departmentID, "cancelled") // must be excluded (f)

	// Physician on the main admission for the metadata columns.
	var docPersonID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'dr. Klinik Utama', 'male') RETURNING id::text",
		companyID).Scan(&docPersonID); err != nil {
		t.Fatalf("seed doctor person: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.physician (company_id, merchant_id, person_id, str_number) VALUES ($1, $2, $3, 'STR-CC2')",
		companyID, merchantID, docPersonID); err != nil {
		t.Fatalf("seed physician: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE operations.admission SET physician_id = (SELECT id FROM core.physician WHERE person_id = $1 AND merchant_id = $2) WHERE id = $3::uuid",
		docPersonID, merchantID, admissionID); err != nil {
		t.Fatalf("attach physician: %v", err)
	}

	findRow := func(rows []any, id string) map[string]any {
		for _, r := range rows {
			if rm := r.(map[string]any); rm["admission_id"] == id {
				return rm
			}
		}
		t.Fatalf("row %s missing in %d rows", id, len(rows))
		return nil
	}
	// Fase-2a subtests run under a SEPARATE permission-holder token: the
	// per-user API rate-limit bucket (burst 20, refill 100/min) would 429
	// the owner token after the fase-1 section already spent it.
	viewer := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cc.viewer", server.PermClinicalRecordView)
	getViewer := func(query string) []any {
		rec, resp := cfDo(t, router, http.MethodGet, path+query, viewer, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		return resp["data"].([]any)
	}
	// getViewerWithMeta fetches data + meta in ONE request (keep request
	// count low).
	getViewerWithMeta := func(query string) ([]any, map[string]any) {
		rec, resp := cfDo(t, router, http.MethodGet, path+query, viewer, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		return resp["data"].([]any), resp["meta"].(map[string]any)
	}

	// (a2) New fields present in admission_id mode: physician via
	// GetPhysicianByID → person.full_name, payer via GetPayerByID ("APSP-cf-cc"
	// is the payer code/name cfSeedAdmission seeds), MRN/birth_date null.
	rowA := getViewer("?admission_id=" + admissionID)[0].(map[string]any)
	if rowA["physician_name"] != "dr. Klinik Utama" || rowA["payer_name"] != "APSP-cf-cc" {
		t.Fatalf("(a2) physician/payer = %v / %v", rowA["physician_name"], rowA["payer_name"])
	}
	if rowA["birth_date"] != nil || rowA["gender"] != "male" {
		t.Fatalf("(a2) person fields = %v", rowA)
	}
	if mrn, _ := rowA["mrn"].(string); mrn == "" {
		t.Fatalf("(a2) mrn missing: %v", rowA)
	}
	if rowA["admission_status"] != "registered" || rowA["admission_at"] == nil || rowA["person_id"] == nil ||
		rowA["department_id"] == nil || rowA["visit_no"] == "" {
		t.Fatalf("(a2) visit fields = %v", rowA)
	}
	if rowA["queue_number"] == nil {
		t.Fatalf("(a2) queue_number = nil, want the pendaftaran ticket number")
	}
	// Date mode returns the SAME row shape with metadata from the joins.
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr)
	rowM := findRow(data, admissionID)
	if rowM["physician_name"] != "dr. Klinik Utama" || rowM["payer_name"] != "APSP-cf-cc" || rowM["gender"] != "male" {
		t.Fatalf("(a2) date-mode row = %v", rowM)
	}

	// (a3) admission_id mode IGNORES pagination (spec §API-1): offset=1 must
	// still return the row, with the contract meta {"limit":1,"offset":0,"total":1}.
	one, metaOne := getViewerWithMeta("?admission_id=" + admissionID + "&offset=1")
	if len(one) != 1 || one[0].(map[string]any)["admission_id"] != admissionID {
		t.Fatalf("(a3) rows with offset=1 = %v, want the admission row", one)
	}
	if metaOne["limit"] != float64(1) || metaOne["offset"] != float64(0) || metaOne["total"] != float64(1) {
		t.Fatalf("(a3) meta = %v, want limit 1 offset 0 total 1", metaOne)
	}

	// (b) q matches person name and MRN (contains, case-insensitive).
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&q=Klinik%20Lain")
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admB {
		t.Fatalf("(b) q by name = %v, want only %s", data, admB)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE core.person SET medical_record_no = 'RM-CC2-99' WHERE id = $1::uuid", person2ID); err != nil {
		t.Fatalf("set mrn: %v", err)
	}
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&q=cc2-99")
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admB {
		t.Fatalf("(b) q by mrn = %v, want only %s", data, admB)
	}

	// (c) department_id filters; malformed values → contract errors.
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&department_id=" + dept2ID)
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admB {
		t.Fatalf("(c) department filter = %v, want only %s", data, admB)
	}
	if rec, resp = cfDo(t, router, http.MethodGet, path+"?date_from="+dateStr+"&date_to="+dateStr+"&department_id=nope", viewer, companyID, merchantID, nil); rec.Code != http.StatusBadRequest || resp["error"] != "invalid department_id" {
		t.Fatalf("(c) bad department_id = %d %v, want 400 invalid department_id", rec.Code, resp)
	}
	if rec, resp = cfDo(t, router, http.MethodGet, path+"?date_from="+dateStr+"&date_to="+dateStr+"&status=selesai", viewer, companyID, merchantID, nil); rec.Code != http.StatusBadRequest || resp["error"] != "invalid status" {
		t.Fatalf("(c) bad status = %d %v, want 400 invalid status", rec.Code, resp)
	}

	// --- Rentang tanggal (plan 2026-11-04-clinical-board-filter Task 1):
	// date_from+date_to wajib bersama, format tervalidasi, from <= to.
	// Token viewer2 = bucket rate-limit sendiri (pola viewer, catatan atas).
	viewer2 := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cc.dates", server.PermClinicalRecordView)
	yd, ymo, ydd := yesterday.Date()
	yesterdayStr := fmt.Sprintf("%04d-%02d-%02d", yd, ymo, ydd)
	wantErr := func(query, msg string) {
		t.Helper()
		rec2, resp2 := cfDo(t, router, http.MethodGet, path+query, viewer2, companyID, merchantID, nil)
		if rec2.Code != http.StatusBadRequest || resp2["error"] != msg {
			t.Fatalf("GET %s = %d %v, want 400 %q", query, rec2.Code, resp2, msg)
		}
	}
	wantErr("?date_from="+dateStr, "parameter date_from dan date_to wajib bersama")
	wantErr("?date_to="+dateStr, "parameter date_from dan date_to wajib bersama")
	wantErr("?date_from=not-a-date&date_to="+dateStr, "invalid date_from")
	wantErr("?date_from="+dateStr+"&date_to=not-a-date", "invalid date_to")
	wantErr("?date_from="+dateStr+"&date_to="+yesterdayStr, "date_from melebihi date_to")

	// Rentang multi-hari: kemarin + 3 kunjungan hari ini (cancelled
	// admC tetap eksklusif) = 4; baris kemarin hadir (visit_no -YESTERDAY).
	data = getViewer("?date_from=" + yesterdayStr + "&date_to=" + dateStr)
	if len(data) != 4 {
		t.Fatalf("(range) rows = %d, want 4 (yesterday + 3 today)", len(data))
	}
	hasYesterday := false
	for _, r := range data {
		if strings.HasSuffix(r.(map[string]any)["visit_no"].(string), "-YESTERDAY") {
			hasYesterday = true
		}
	}
	if !hasYesterday {
		t.Fatalf("(range) yesterday admission missing in %d rows", len(data))
	}

	// (d) status semantics: admP gets a signed nurse entry without
	// counter-sign → countersign pending → overall belum_lengkap, and the
	// pending row MUST stay inside belum_lengkap (pending ⊂ belum_lengkap).
	nurseData2 := `{"sections":{"keluhan":"Demam dua hari."}}`
	if rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admP, "form_type": "ases_awal_perawat", "form_data": cfRawJSON(t, nurseData2),
	}); rec.Code != http.StatusCreated {
		t.Fatalf("(d) create admP nurse = %d: %s", rec.Code, rec.Body.String())
	}
	nurse2ID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+nurse2ID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(d) sign admP nurse = %d: %s", rec.Code, rec.Body.String())
	}
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&status=lengkap")
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admissionID {
		t.Fatalf("(d) status=lengkap = %v, want only %s", data, admissionID)
	}
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&status=pending")
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admP {
		t.Fatalf("(d) status=pending = %v, want only %s", data, admP)
	}
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr + "&status=belum_lengkap")
	if len(data) != 2 || findRow(data, admP)["overall"] != "belum_lengkap" {
		t.Fatalf("(d) status=belum_lengkap = %v, want %s + %s (pending row included)", data, admissionID, admP)
	}

	// (e) limit/offset slice the filtered rows; meta.total = rows after
	// filter, before pagination = 3 (main + admB + admP; cancelled excluded,
	// which also covers (f)).
	page, meta := getViewerWithMeta("?date_from=" + dateStr + "&date_to=" + dateStr + "&limit=2&offset=1")
	if meta["total"] != float64(3) || meta["limit"] != float64(2) || meta["offset"] != float64(1) {
		t.Fatalf("(e) meta = %v, want total 3 limit 2 offset 1", meta)
	}
	if len(page) != 2 {
		t.Fatalf("(e) page rows = %d, want 2", len(page))
	}
	for _, r := range page {
		if r.(map[string]any)["admission_id"] == admC {
			t.Fatalf("(f) cancelled admission %s must not appear", admC)
		}
	}

	// (g) queue: loket alone wins over pendaftaran; poli beats loket;
	// cancelled/skipped tickets are ignored; no tickets → null. Each stage
	// reuses ONE fetch to stay inside the rate-limit bucket.
	queueBase := time.Date(y, m, d, 13, 0, 0, 0, loc)
	seedTicket := func(qtype, number, status string, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number, status, created_at)
			 VALUES ($1, $2, $3, $4::uuid, $5, $6::uuid, $7, $8, $9)`,
			companyID, merchantID, qtype, departmentID, personID, admissionID, number, status, at); err != nil {
			t.Fatalf("seed %s ticket %s: %v", qtype, status, err)
		}
	}
	seedTicket("loket", "L-77", "waiting", queueBase)
	if rn := findRow(getViewer("?date_from="+dateStr+"&date_to="+dateStr), admissionID)["queue_number"]; rn != "L-77" {
		t.Fatalf("(g) loket only = %v, want L-77", rn)
	}
	seedTicket("poli", "P-9", "waiting", queueBase.Add(time.Minute))
	if rn := findRow(getViewer("?date_from="+dateStr+"&date_to="+dateStr), admissionID)["queue_number"]; rn != "P-9" {
		t.Fatalf("(g) poli+loket = %v, want P-9 (poli wins)", rn)
	}
	seedTicket("loket", "L-88", "cancelled", queueBase.Add(2*time.Minute))
	seedTicket("poli", "P-10", "skipped", queueBase.Add(3*time.Minute))
	data = getViewer("?date_from=" + dateStr + "&date_to=" + dateStr)
	if rn := findRow(data, admissionID)["queue_number"]; rn != "P-9" {
		t.Fatalf("(g) cancelled/skipped ignored = %v, want P-9", rn)
	}
	if rn := findRow(data, admB)["queue_number"]; rn != nil {
		t.Fatalf("(g) no tickets = %v, want nil", rn)
	}

	// Permission: a user without clinical.record.view → 403; a holder → 200.
	noPerm := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cc.noview")
	if rec, _ = cfDo(t, router, http.MethodGet, path+"?admission_id="+admissionID, noPerm, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("(perm) no-view = %d, want 403", rec.Code)
	}
	if rec, _ = cfDo(t, router, http.MethodGet, path+"?admission_id="+admissionID, viewer, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("(perm) viewer = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestClinicalCompletenessAdmissionMode — admission-mode enrichment (plan
// 2026-11-11-cppt-revisi-a-zona-atas Task 1, spec §4.3): every check gains
// who/at/detail, a 6th "alergi" check appears, and the date-mode response
// stays WITHOUT the new fields (5 checks, byte-identical shape).
// Expectations come from the contract literals, not from handler output:
// soap detail = "Subjektif <s>/2 · Objektif <o>/1 · Assessment <a>/1 ·
// Plan <p>/1" (S: keluhan_utama trim≠"" + anamnesis_info terisi; O: ≥1 dari
// tanda_vital/pemeriksaan_fisik terisi; A: ≥1 diagnosa; P: rtl.moda terisi);
// alergi detail = "<zat> (<reaksi>)" join ", " maks 3 + "+<n> lainnya";
// masters win over soap rows (who "master", at null).
func TestClinicalCompletenessAdmissionMode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "cam")
	cdSeedICD11(t, ctx, pool)
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "cam")
	// Second person/admission WITHOUT master allergies → soap-only alergi case.
	_, admission2ID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "cam2")

	// Viewer token = rate-limit bucket sendiri (pola TestClinicalCompleteness).
	viewer := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cam.viewer", server.PermClinicalRecordView)
	path := "/api/v1/clinical/completeness"
	get := func(token, query string) []any {
		rec, resp := cfDo(t, router, http.MethodGet, path+query, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		return resp["data"].([]any)
	}
	findCheck := func(row map[string]any, key string) map[string]any {
		for _, c := range row["checks"].([]any) {
			chk := c.(map[string]any)
			if chk["key"] == key {
				return chk
			}
		}
		t.Fatalf("check %s missing in %v", key, row["checks"])
		return nil
	}
	rfc := func(v any) time.Time {
		t.Helper()
		s, ok := v.(string)
		if !ok {
			t.Fatalf("at = %v, want RFC3339 string", v)
		}
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("at %q not RFC3339: %v", s, err)
		}
		return at
	}

	// (a) Fresh visit: 6 checks, all with the who/at/detail keys; alergi
	// incomplete with the contract literal detail; diagnosis/resume literals
	// always present; form checks without forms → who/at/detail null.
	row := get(viewer, "?admission_id="+admissionID)[0].(map[string]any)
	checks := row["checks"].([]any)
	if len(checks) != 6 {
		t.Fatalf("(a) checks = %d, want 6", len(checks))
	}
	aler := findCheck(row, "alergi")
	if aler["status"] != "incomplete" || aler["label"] != "Catatan alergi" || aler["who"] != nil ||
		aler["at"] != nil || aler["detail"] != "Belum ada alergi tercatat" {
		t.Fatalf("(a) alergi = %v", aler)
	}
	if chk := findCheck(row, "diagnosis"); chk["detail"] != "Wajib utk TTE (PMK 24/2022 Ps. 46) — kosong = TTE diblok" ||
		chk["who"] != nil || chk["at"] != nil {
		t.Fatalf("(a) diagnosis = %v", chk)
	}
	if chk := findCheck(row, "resume_rajal"); chk["detail"] != "Diisi di tab Resume & Pulang sebelum discharge" {
		t.Fatalf("(a) resume = %v", chk)
	}
	if chk := findCheck(row, "countersign"); chk["status"] != "complete" || chk["who"] != nil || chk["at"] != nil || chk["detail"] != nil {
		t.Fatalf("(a) countersign = %v", chk)
	}
	for _, key := range []string{"ases_awal_perawat", "soap_dokter"} {
		if chk := findCheck(row, key); chk["who"] != nil || chk["at"] != nil || chk["detail"] != nil {
			t.Fatalf("(a) %s = %v, want who/at/detail null", key, chk)
		}
	}

	// (b) 4 master allergies → complete, who "master", at null, detail =
	// 3 items + "+1 lainnya" (contract join rule; query orders by
	// recorded_at DESC → pinned timestamps fix the item order).
	for i, al := range [][2]string{{"Sulfa", "ruam"}, {"Telur", "gatal"}, {"Aspirin", "sesak"}, {"Kacang", "benjol"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, reaction, is_active, recorded_at)
			 VALUES ($1, $2, $3, $4, $5, true, $6)`,
			companyID, personID, []string{"drug", "food", "drug", "food"}[i], al[0], al[1],
			time.Now().Add(-time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("(b) seed master allergy %s: %v", al[0], err)
		}
	}
	row = get(viewer, "?admission_id="+admissionID)[0].(map[string]any)
	aler = findCheck(row, "alergi")
	if aler["status"] != "complete" || aler["who"] != "master" || aler["at"] != nil ||
		aler["detail"] != "Sulfa (ruam), Telur (gatal), Aspirin (sesak), +1 lainnya" {
		t.Fatalf("(b) alergi = %v", aler)
	}

	// (c) soap_dokter draft: S keluhan+jenis terisi, tanda_vital terisi,
	// diagnosa 1 entry, rtl kosong → detail "Subjektif 2/2 · Objektif 1/1 ·
	// Assessment 1/1 · Plan 0/1" (contract example); who = pengisi via
	// GetUserDisplayName (non-null), at = filled_at RFC3339; master alergi
	// wins over the soap alergi row.
	camSoap := `{"sections":{
	  "anamnesis_info":{"jenis":"autoanamnesis"},
	  "keluhan_utama":"Demam 3 hari.",
	  "tanda_vital":{"nafas":22},
	  "diagnosa":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"},"suspect":false}]},
	  "alergi":[{"pemicu":"Seafood","jenis":"food","reaksi":"Urtikaria"}]
	}}`
	soapID := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, camSoap)
	row = get(viewer, "?admission_id="+admissionID)[0].(map[string]any)
	chk := findCheck(row, "soap_dokter")
	if chk["status"] != "incomplete" || chk["detail"] != "Subjektif 2/2 · Objektif 1/1 · Assessment 1/1 · Plan 0/1" ||
		chk["who"] == nil || rfc(chk["at"]).IsZero() {
		t.Fatalf("(c) soap = %v", chk)
	}

	// (d) Sign the draft → at switches to signed_at, detail unchanged shape.
	if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+soapID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(d) sign soap = %d: %s", rec.Code, rec.Body.String())
	}
	row = get(viewer, "?admission_id="+admissionID)[0].(map[string]any)
	chk = findCheck(row, "soap_dokter")
	if chk["status"] != "complete" || rfc(chk["at"]).IsZero() || chk["who"] == nil {
		t.Fatalf("(d) signed soap = %v", chk)
	}

	// (e) Nurse entry signed + counter-signed → countersign detail ends with
	// " — selesai" and at = counter_signed_at.
	nurseData := `{"sections":{"keluhan":"Pusing sejak pagi."}}`
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_awal_perawat", "form_data": cfRawJSON(t, nurseData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("(e) create nurse = %d: %s", rec.Code, rec.Body.String())
	}
	nurseID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+nurseID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(e) sign nurse = %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+nurseID+"/countersign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("(e) countersign nurse = %d: %s", rec.Code, rec.Body.String())
	}
	row = get(viewer, "?admission_id="+admissionID)[0].(map[string]any)
	chk = findCheck(row, "countersign")
	if chk["status"] != "complete" || chk["at"] == nil || rfc(chk["at"]).IsZero() {
		t.Fatalf("(e) countersign = %v", chk)
	}
	if detail, ok := chk["detail"].(string); !ok || !strings.HasSuffix(detail, " — selesai") {
		t.Fatalf("(e) countersign detail = %v, want '<nama> — selesai'", chk["detail"])
	}

	// (f) soap-only alergi (person cam2 has no master allergy) → complete,
	// who null, detail "<zat> (<reaksi>)".
	cfCreateDraft(t, router, token, companyID, merchantID, admission2ID, camSoap)
	row = get(viewer, "?admission_id="+admission2ID)[0].(map[string]any)
	aler = findCheck(row, "alergi")
	if aler["status"] != "complete" || aler["who"] != nil || aler["at"] != nil || aler["detail"] != "Seafood (Urtikaria)" {
		t.Fatalf("(f) alergi = %v", aler)
	}

	// (g) Date mode stays byte-identical: 5 checks, no who/at/detail keys
	// anywhere in the response.
	var tz string
	if err := pool.QueryRow(ctx,
		"SELECT timezone FROM core.merchant WHERE id = $1::uuid", merchantID).Scan(&tz); err != nil {
		t.Fatalf("read merchant timezone: %v", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz %s: %v", tz, err)
	}
	y, m, d := time.Now().In(loc).Date()
	dateStr := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	rows := get(viewer, "?date_from="+dateStr+"&date_to="+dateStr)
	var mainRow map[string]any
	for _, r := range rows {
		if rm := r.(map[string]any); rm["admission_id"] == admissionID {
			mainRow = rm
		}
	}
	if mainRow == nil {
		t.Fatalf("(g) admission row missing in date mode (%d rows)", len(rows))
	}
	for _, bad := range []string{"who", "detail"} {
		if _, present := mainRow[bad]; present {
			t.Fatalf("(g) date-mode row carries %q: %v", bad, mainRow)
		}
	}
	if dmChecks, ok := mainRow["checks"].([]any); !ok || len(dmChecks) != 5 {
		t.Fatalf("(g) date-mode checks = %v, want 5", mainRow["checks"])
	}
	for _, c := range mainRow["checks"].([]any) {
		chk := c.(map[string]any)
		for _, bad := range []string{"who", "at", "detail"} {
			if _, present := chk[bad]; present {
				t.Fatalf("(g) date-mode check carries %q: %v", bad, chk)
			}
		}
	}
}
