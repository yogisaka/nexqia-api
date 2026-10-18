//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestFormSettingsFields_ScheduleTimes — an end_time that is not after
// start_time must be rejected with the old error text plus a per-field map
// (spec 2026-10-07-form-validation-stage2 §4.3): fields.end_time =
// "Jam selesai harus setelah jam mulai".
func TestFormSettingsFields_ScheduleTimes(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'e', "FFST", "")

	body := map[string]any{
		"merchant_id":    e.merchantID,
		"physician_id":   e.physicianID,
		"department_id":  e.departmentID,
		"day_of_week":    1,
		"start_time":     "10:00",
		"end_time":       "08:00",
		"effective_from": "2026-10-01",
	}
	rec := e.do(t, http.MethodPost, "/api/v1/physician-schedules", body, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "end_time must be after start_time" {
		t.Fatalf("expected error end_time must be after start_time, got %q", errMsg)
	}
	if fields["end_time"] != "Jam selesai harus setelah jam mulai" {
		t.Fatalf("expected end_time message, got %q", fields["end_time"])
	}
}

// TestFormSettingsFields_LeaveDates — a leave with date_to before date_from
// must carry fields.date_to (spec §4.3), with the old error text unchanged.
func TestFormSettingsFields_LeaveDates(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, 'f', "FFLV", "")

	body := map[string]any{
		"merchant_id":  e.merchantID,
		"physician_id": e.physicianID,
		"date_from":    "2026-10-10",
		"date_to":      "2026-10-05",
	}
	rec := e.do(t, http.MethodPost, "/api/v1/physician-leaves", body, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "date_to must be on or after date_from" {
		t.Fatalf("expected error date_to must be on or after date_from, got %q", errMsg)
	}
	if fields["date_to"] != "Tanggal akhir tidak boleh sebelum tanggal mulai" {
		t.Fatalf("expected date_to message, got %q", fields["date_to"])
	}
}

// TestFormSettingsFields_DepartmentRequired — creating a department without a
// name must be rejected by the binding with error=validation_failed and
// fields.name = "Wajib diisi" (spec 2026-10-07-form-validation-stage2 §4.3).
func TestFormSettingsFields_DepartmentRequired(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "fs.dept", "081234570111")

	body, _ := json.Marshal(map[string]any{
		"merchant_id": merchantID,
		"code":        "FS-DEPT",
		// name deliberately absent
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/departments", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "validation_failed" {
		t.Fatalf("expected error validation_failed, got %q", errMsg)
	}
	if fields["name"] != "Wajib diisi" {
		t.Fatalf("expected fields.name = Wajib diisi, got %q", fields["name"])
	}
}

// TestFormSettingsFields_LocationSite — a site without coordinates must keep
// the old error text "site requires latitude and longitude" and now carry
// fields.latitude + fields.longitude = "Wajib diisi untuk lokasi site".
func TestFormSettingsFields_LocationSite(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "fs.loc", "081234570112")

	body, _ := json.Marshal(map[string]any{
		"merchant_id": merchantID,
		"kind":        "site",
		"code":        "FS-SITE",
		"name":        "Kampus Utama",
		// latitude/longitude deliberately absent → chk_location_site_position
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/locations", token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	errMsg, fields := decodeFieldErrors(t, rec)
	if errMsg != "site requires latitude and longitude" {
		t.Fatalf("expected error site requires latitude and longitude, got %q", errMsg)
	}
	if fields["latitude"] != "Wajib diisi untuk lokasi site" {
		t.Fatalf("expected fields.latitude message, got %q", fields["latitude"])
	}
	if fields["longitude"] != "Wajib diisi untuk lokasi site" {
		t.Fatalf("expected fields.longitude message, got %q", fields["longitude"])
	}
}
