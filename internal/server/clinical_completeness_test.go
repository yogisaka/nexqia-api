//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
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
// (e) ?date= only covers the merchant-local day.
func TestClinicalCompleteness(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "cc")
	cdSeedICD11(t, ctx, pool)
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "cc")

	path := "/api/v1/clinical/completeness"

	// Neither ?date= nor ?admission_id= → 400 with the contract message.
	rec, resp := cfDo(t, router, http.MethodGet, path, token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest || resp["error"] != "parameter date atau admission_id wajib" {
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

	// (e) ?date= covers only the merchant-local day: the today admission is
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
	data = get("?date=" + dateStr)
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
	data = get("?date=" + dateStr)
	if len(data) != 1 || data[0].(map[string]any)["admission_id"] != admissionID {
		t.Fatalf("(e) rows after yesterday insert = %v, want only %s", data, admissionID)
	}

	// Permission: a user without clinical.record.view → 403; a holder → 200.
	noPerm := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cc.noview")
	if rec, _ = cfDo(t, router, http.MethodGet, path+"?admission_id="+admissionID, noPerm, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("(perm) no-view = %d, want 403", rec.Code)
	}
	viewer := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.cc.viewer", server.PermClinicalRecordView)
	if rec, _ = cfDo(t, router, http.MethodGet, path+"?admission_id="+admissionID, viewer, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("(perm) viewer = %d: %s", rec.Code, rec.Body.String())
	}
}
