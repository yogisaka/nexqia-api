//go:build integration

package server_test

import (
	"context"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// auditExportBOM is the UTF-8 BOM every audit CSV export starts with.
const auditExportBOM = "\xEF\xBB\xBF"

// parseAuditExport decodes a CSV export response body: the BOM must be present
// and the rest must parse as CSV (header row included).
func parseAuditExport(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, auditExportBOM) {
		t.Fatalf("export body missing UTF-8 BOM, got prefix %q", body[:16])
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, auditExportBOM))).ReadAll()
	if err != nil {
		t.Fatalf("parse export CSV: %v", err)
	}
	return rows
}

// csvCellExists reports whether any parsed CSV cell equals want exactly.
func csvCellExists(rows [][]string, want string) bool {
	for _, row := range rows {
		for _, cell := range row {
			if cell == want {
				return true
			}
		}
	}
	return false
}

// TestAuditExport_HeaderBomAndRows — the access export starts with a BOM, uses
// the spec header, holds exactly one data row per person/view (create is not
// access-logged), carries the CSV content headers and Jakarta timestamps; the
// change export uses its own header and renders database-made changes
// (changed_by NULL) as User "Sistem".
func TestAuditExport_HeaderBomAndRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.exporthdr", "081234570231")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Export Header Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Export Header Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
	// Database-made change: no app.current_user_id GUC → changed_by NULL.
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'Export Sistem Person', 'male')", companyID); err != nil {
		t.Fatalf("seed sistem person: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export access logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("access export content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="audit-akses-`) || !strings.HasSuffix(cd, `.csv"`) {
		t.Fatalf("access export content-disposition = %q", cd)
	}
	rows := parseAuditExport(t, rec.Body.String())
	wantHeader := []string{"Waktu", "User", "Admin platform", "Aksi", "Data", "Pasien", "No. RM", "Hasil", "IP"}
	if len(rows) != 2 {
		t.Fatalf("access export expected header + 1 view row, got %d rows: %v", len(rows), rows)
	}
	for i, want := range wantHeader {
		if rows[0][i] != want {
			t.Fatalf("access header col %d = %q, want %q", i, rows[0][i], want)
		}
	}
	if rows[1][3] != "view" || rows[1][4] != "person" || rows[1][5] != "Export Header Person" || rows[1][7] != "200" || rows[1][2] != "" {
		t.Fatalf("access data row = %v", rows[1])
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", rows[1][0], jakarta); err != nil {
		t.Fatalf("Waktu cell %q: %v", rows[1][0], err)
	} else if d := time.Since(ts); d < -2*time.Minute || d > 2*time.Minute {
		t.Fatalf("Waktu cell %q not Jakarta time near now (drift %v)", rows[1][0], d)
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/audit/change-logs/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export change logs expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="audit-perubahan-`) || !strings.HasSuffix(cd, `.csv"`) {
		t.Fatalf("change export content-disposition = %q", cd)
	}
	rows = parseAuditExport(t, rec.Body.String())
	wantHeader = []string{"Waktu", "User", "Admin platform", "Aksi", "Data", "Pasien", "No. RM", "Field berubah"}
	for i, want := range wantHeader {
		if rows[0][i] != want {
			t.Fatalf("change header col %d = %q, want %q", i, rows[0][i], want)
		}
	}
	sawOwner, sawSistem := false, false
	for _, row := range rows[1:] {
		switch row[5] {
		case "Export Header Person":
			if row[3] != "insert" || row[4] != "core.person" || row[1] == "" {
				t.Fatalf("owner change row = %v", row)
			}
			sawOwner = true
		case "Export Sistem Person":
			if row[1] != "Sistem" {
				t.Fatalf("change without changed_by: User = %q, want Sistem", row[1])
			}
			sawSistem = true
		}
	}
	if !sawOwner || !sawSistem {
		t.Fatalf("change export missing rows: owner=%v sistem=%v (rows=%v)", sawOwner, sawSistem, rows)
	}
}

// TestAuditExport_EscapesFormulas — a patient named like a spreadsheet formula
// is exported with a leading apostrophe so Excel cannot execute it.
func TestAuditExport_EscapesFormulas(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.escape", "081234570232")
	createPersonViaAPI(t, router, token, companyID, merchantID, `=HYPERLINK("x")`, "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, `=HYPERLINK("x")`)
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows := parseAuditExport(t, rec.Body.String())
	if !csvCellExists(rows, `'=HYPERLINK("x")`) {
		t.Fatalf("escaped formula cell missing: %v", rows)
	}
	if csvCellExists(rows, `=HYPERLINK("x")`) {
		t.Fatalf("unescaped formula cell leaked: %v", rows)
	}
}

// TestAuditExport_TooManyRows — with AuditExportMaxRows 1, two log rows make
// the export refuse with 400 instead of streaming.
func TestAuditExport_TooManyRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.AuditExportMaxRows = 1
	router := server.NewRouter(pool, redisClient, cfg)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.toomany", "081234570233")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Export TooMany Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Export TooMany Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("export beyond max rows expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too many rows, narrow the filter") {
		t.Fatalf("400 body = %s", rec.Body.String())
	}
}

// TestAuditExport_IsLogged — an export call itself records one access_log row
// (resource audit, action export_access) carrying the filtered patient id.
func TestAuditExport_IsLogged(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.islogged", "081234570234")
	createPersonViaAPI(t, router, token, companyID, merchantID, "Export IsLogged Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Export IsLogged Person")
	accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs/export?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'audit' AND action = 'export_access' AND person_id = $2`, companyID, personID).Scan(&n); err != nil {
		t.Fatalf("count export access rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 export_access row with the filtered person_id, got %d", n)
	}
}

// TestAuditExport_CsvSafe — every dangerous leading character gets a leading
// apostrophe in the exported cells (formula-injection escape per prefix).
func TestAuditExport_CsvSafe(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.csvsafe", "081234570235")
	names := []string{"=s", "+s", "-s", "@s", "\ts", "\rs"}
	for _, name := range names {
		createPersonViaAPI(t, router, token, companyID, merchantID, name, "male")
		personID, _ := latestPerson(t, ctx, pool, companyID, name)
		accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/access-logs/export", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows := parseAuditExport(t, rec.Body.String())
	for _, name := range names {
		if !csvCellExists(rows, "'"+name) {
			t.Fatalf("escaped cell %q missing: %v", "'"+name, rows)
		}
		if csvCellExists(rows, name) {
			t.Fatalf("unescaped cell %q leaked: %v", name, rows)
		}
	}
}
