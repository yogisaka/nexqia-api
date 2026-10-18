//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
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
