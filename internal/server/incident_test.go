//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// incidentEnv bundles one seeded tenant + the full router (no storage
// injection needed for incident endpoints — pattern setupDocEnv without the
// fake object storage).
type incidentEnv struct {
	pool          *pgxpool.Pool
	router        *gin.Engine
	companyID     string
	merchantID    string
	userID        string
	token         string
	refreshCookie *http.Cookie
}

func setupIncidentEnv(t *testing.T, username string) *incidentEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, userID, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, username, docPhone())
	return &incidentEnv{pool: pool, router: router, companyID: companyID,
		merchantID: merchantID, userID: userID, token: token, refreshCookie: refreshCookie}
}

// incidentRequest performs an incident-route request with the standard
// tenant/auth headers; merchantID overrides X-Merchant-ID ("" = env default).
func incidentRequest(e *incidentEnv, token, merchantID, method, path string, body []byte) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", e.companyID)
	if merchantID == "" {
		merchantID = e.merchantID
	}
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// incidentError decodes the {"error": ...} body of a non-2xx response.
func incidentError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return body.Error
}

// validIncidentBody returns a valid report body (contract example:
// occurred_at RFC3339 "2026-10-20T01:30:00Z", jenis medikasi, grading D).
func validIncidentBody() map[string]any {
	return map[string]any{
		"incident_type":  "medikasi",
		"grading":        "D",
		"occurred_at":    "2026-10-20T01:30:00Z",
		"location":       "Ruang Rawat Inap Lantai 2",
		"narrative":      "Infus meleset saat pemasangan obat.",
		"patient_impact": "Pasien mengeluh nyeri singkat, tanpa cedera permanen.",
		"anonymous":      false,
	}
}

// createIncident POSTs a valid (or mutated) incident report as the owner.
func createIncident(t *testing.T, e *incidentEnv, mutate func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	body := validIncidentBody()
	if mutate != nil {
		mutate(body)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal incident body: %v", err)
	}
	return incidentRequest(e, e.token, "", http.MethodPost, "/api/v1/incidents", b)
}

// incidentIDOf decodes the created incident id from a 201 response.
func incidentIDOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Incident struct {
			ID string `json:"id"`
		} `json:"incident"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode incident response: %v", err)
	}
	return body.Incident.ID
}

// TestIncidentCatalogs — the data-driven catalogs match migration 000079:
// 10 incident type categories (including medikasi) and 9 NCC MERP gradings
// A–I with the ICPS outcome mapping (A–D near miss, E–H adverse, I sentinel).
func TestIncidentCatalogs(t *testing.T) {
	e := setupIncidentEnv(t, "incidentcat.owner")
	rec := incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incident-types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("incident-types expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var types struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &types); err != nil {
		t.Fatalf("decode incident-types: %v", err)
	}
	if len(types.Data) != 10 { // migration 000079 seeds exactly 10 categories
		t.Fatalf("expected 10 incident types, got %d: %s", len(types.Data), rec.Body.String())
	}
	foundMedikasi := false
	for _, item := range types.Data {
		if item["code"] == "medikasi" {
			foundMedikasi = true
			if item["name"] != "Medikasi" || item["is_active"] != true {
				t.Fatalf("unexpected medikasi entry: %+v", item)
			}
		}
	}
	if !foundMedikasi {
		t.Fatalf("catalog must contain medikasi: %+v", types.Data)
	}

	rec = incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incident-gradings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("incident-gradings expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var gradings struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gradings); err != nil {
		t.Fatalf("decode incident-gradings: %v", err)
	}
	if len(gradings.Data) != 9 { // NCC MERP A–I
		t.Fatalf("expected 9 gradings, got %d", len(gradings.Data))
	}
	if gradings.Data[8]["code"] != "I" || gradings.Data[8]["outcome"] != "sentinel" {
		t.Fatalf("expected data[8] = I/sentinel, got %+v", gradings.Data[8])
	}
	if gradings.Data[0]["outcome"] != "near_miss_no_harm" {
		t.Fatalf("expected data[0] near_miss_no_harm, got %+v", gradings.Data[0])
	}
	if gradings.Data[4]["outcome"] != "adverse_event" {
		t.Fatalf("expected data[4] adverse_event, got %+v", gradings.Data[4])
	}
}

// TestIncidentLifecycle — report → feedback (note wajib) → closed (tanpa
// note = tanpa entri baru); closed ditolak dari dilaporkan dan tanpa exit;
// kronologi tetap terbuka untuk komentar pada insiden closed.
func TestIncidentLifecycle(t *testing.T) {
	e := setupIncidentEnv(t, "incidentlife.owner")
	rec := createIncident(t, e, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Incident map[string]any `json:"incident"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Incident["status"] != "dilaporkan" {
		t.Fatalf("new incident must be dilaporkan, got %+v", created.Incident)
	}
	id := incidentIDOf(t, rec)

	// GET detail: exactly one report entry.
	rec = incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incidents/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Entries) != 1 || detail.Entries[0]["kind"] != "report" {
		t.Fatalf("expected exactly 1 report entry, got %+v", detail.Entries)
	}

	// closed dari dilaporkan → 409 invalid status transition.
	body, _ := json.Marshal(map[string]string{"status": "closed"})
	rec = incidentRequest(e, e.token, "", http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusConflict || incidentError(t, rec) != "invalid status transition" {
		t.Fatalf("closed from dilaporkan expected 409 invalid status transition, got %d: %s", rec.Code, rec.Body.String())
	}

	// feedback tanpa note → 400 catatan umpan balik wajib.
	body, _ = json.Marshal(map[string]string{"status": "feedback"})
	rec = incidentRequest(e, e.token, "", http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusBadRequest || incidentError(t, rec) != "catatan umpan balik wajib" {
		t.Fatalf("feedback without note expected 400 catatan umpan balik wajib, got %d: %s", rec.Code, rec.Body.String())
	}

	// feedback + note → 200, entries jadi 2 (kind feedback).
	body, _ = json.Marshal(map[string]string{"status": "feedback", "note": "Sudah ditindaki"})
	rec = incidentRequest(e, e.token, "", http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("feedback with note expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var patched struct {
		Incident map[string]any `json:"incident"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.Incident["status"] != "feedback" {
		t.Fatalf("status must be feedback, got %+v", patched.Incident)
	}
	rec = incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incidents/"+id, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Entries) != 2 || detail.Entries[1]["kind"] != "feedback" {
		t.Fatalf("expected 2 entries with feedback last, got %+v", detail.Entries)
	}

	// closed dari feedback tanpa note → 200, TANPA entri baru (tetap 2).
	body, _ = json.Marshal(map[string]string{"status": "closed"})
	rec = incidentRequest(e, e.token, "", http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("close expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode close: %v", err)
	}
	if patched.Incident["status"] != "closed" {
		t.Fatalf("status must be closed, got %+v", patched.Incident)
	}
	rec = incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incidents/"+id, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Entries) != 2 {
		t.Fatalf("closed without note must add no entry, got %+v", detail.Entries)
	}

	// closed lagi → 409 (tidak ada transisi keluar dari closed di B1).
	rec = incidentRequest(e, e.token, "", http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusConflict || incidentError(t, rec) != "invalid status transition" {
		t.Fatalf("closed again expected 409 invalid status transition, got %d: %s", rec.Code, rec.Body.String())
	}

	// Kronologi tetap terbuka: comment pada insiden closed → 201.
	body, _ = json.Marshal(map[string]string{"kind": "comment", "body": "Catatan"})
	rec = incidentRequest(e, e.token, "", http.MethodPost, "/api/v1/incidents/"+id+"/entries", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("comment on closed incident expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIncidentValidation — literal validation messages of the contract.
func TestIncidentValidation(t *testing.T) {
	e := setupIncidentEnv(t, "incidentvalid.owner")

	cases := []struct {
		name string
		mut  func(map[string]any)
		want string
	}{
		{"unknown type", func(b map[string]any) { b["incident_type"] = "tidakada" }, "jenis insiden tidak dikenal"},
		{"unknown grading", func(b map[string]any) { b["grading"] = "Z" }, "grading tidak dikenal"},
		{"bad occurred_at", func(b map[string]any) { b["occurred_at"] = "2026-10-20" }, "waktu kejadian wajib dan harus RFC3339"},
		{"blank narrative", func(b map[string]any) { b["narrative"] = "   " }, "narasi wajib"},
		{"person_id not uuid", func(b map[string]any) { b["person_id"] = "abc" }, "invalid id"},
	}
	for _, tc := range cases {
		rec := createIncident(t, e, tc.mut)
		if rec.Code != http.StatusBadRequest || incidentError(t, rec) != tc.want {
			t.Fatalf("%s: expected 400 %q, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}

	// person_id valid milik company lain → 400 pasien tidak ditemukan.
	ctx := context.Background()
	var otherCompany, otherPerson string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ($1, $2, 10) RETURNING id::text",
		"INC-OTHER-"+e.companyID[:8], "Other Company").Scan(&otherCompany); err != nil {
		t.Fatalf("seed other company: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
		otherCompany, "Other Company Person").Scan(&otherPerson); err != nil {
		t.Fatalf("seed other person: %v", err)
	}
	rec := createIncident(t, e, func(b map[string]any) { b["person_id"] = otherPerson })
	if rec.Code != http.StatusBadRequest || incidentError(t, rec) != "pasien tidak ditemukan" {
		t.Fatalf("foreign person expected 400 pasien tidak ditemukan, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIncidentPermissions — a user without any quality.* permission gets 403
// with the literal message (pattern: TestPersonConsentPermission).
func TestIncidentPermissions(t *testing.T) {
	e := setupIncidentEnv(t, "incidentperm.owner")
	ctx := context.Background()
	// This test owns its fresh Postgres container: dropping the grants from
	// the system Owner role cannot leak into other tests.
	if _, err := e.pool.Exec(ctx, `
		DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code LIKE 'quality.incident.%'`); err != nil {
		t.Fatalf("revoke quality.incident.* from Owner: %v", err)
	}

	rec := incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incidents", nil)
	if rec.Code != http.StatusForbidden || incidentError(t, rec) != "missing permission: quality.incident.view" {
		t.Fatalf("list expected 403 missing permission: quality.incident.view, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = createIncident(t, e, nil)
	if rec.Code != http.StatusForbidden || incidentError(t, rec) != "missing permission: quality.incident.report" {
		t.Fatalf("create expected 403 missing permission: quality.incident.report, got %d: %s", rec.Code, rec.Body.String())
	}
}

// seedIncidentViewer creates a second user whose role holds ONLY
// quality.incident.view + report (no manage) at the tenant merchant and
// returns a token for them (pattern: rbac tests' GenerateToken with rid).
func seedIncidentViewer(t *testing.T, e *incidentEnv, username string) string {
	t.Helper()
	ctx := context.Background()
	var personID, userID, roleID string
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'female') RETURNING id::text",
		e.companyID, "Incident Viewer "+username).Scan(&personID); err != nil {
		t.Fatalf("seed viewer person: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.app_user (company_id, person_id, username, password_hash, is_active) VALUES ($1, $2, $3, 'x-hash', true) RETURNING id::text",
		e.companyID, personID, username).Scan(&userID); err != nil {
		t.Fatalf("seed viewer user: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		"INSERT INTO core.role (company_id, name, is_system, requires_physician_data) VALUES ($1, $2, false, false) RETURNING id::text",
		e.companyID, "Incident Viewer Role "+username).Scan(&roleID); err != nil {
		t.Fatalf("seed viewer role: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		"INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, p.id FROM core.permission p WHERE p.code IN ('quality.incident.view', 'quality.incident.report')",
		roleID); err != nil {
		t.Fatalf("grant viewer role permissions: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		"INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)",
		userID, e.merchantID, roleID); err != nil {
		t.Fatalf("assign viewer role at merchant: %v", err)
	}
	token, err := auth.GenerateToken(testJWTSecret, userID, e.companyID, e.merchantID, roleID, username, "incident-viewer-device", time.Hour)
	if err != nil {
		t.Fatalf("generate viewer token: %v", err)
	}
	return token
}

// TestIncidentAnonymousReporter — anonymous incident hides reported_by and
// reported_by_name (and the report entry's created_by_name) from callers
// without quality.incident.manage; the manage caller sees everything.
func TestIncidentAnonymousReporter(t *testing.T) {
	e := setupIncidentEnv(t, "incidentanon.owner")
	rec := createIncident(t, e, func(b map[string]any) { b["anonymous"] = true })
	if rec.Code != http.StatusCreated {
		t.Fatalf("anonymous create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	id := incidentIDOf(t, rec)

	// Owner has quality.incident.manage → identities visible.
	rec = incidentRequest(e, e.token, "", http.MethodGet, "/api/v1/incidents/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner detail expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ownerView struct {
		Incident map[string]any   `json:"incident"`
		Entries  []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ownerView); err != nil {
		t.Fatalf("decode owner detail: %v", err)
	}
	if ownerView.Incident["reported_by"] == nil || ownerView.Incident["reported_by_name"] == nil {
		t.Fatalf("manage caller must see reporter identity: %+v", ownerView.Incident)
	}
	if len(ownerView.Entries) != 1 || ownerView.Entries[0]["kind"] != "report" || ownerView.Entries[0]["created_by_name"] == nil {
		t.Fatalf("manage caller must see report entry author: %+v", ownerView.Entries)
	}

	// Viewer with only view+report → reporter identity redacted.
	viewerToken := seedIncidentViewer(t, e, "incident.viewer."+e.companyID[:8])
	rec = incidentRequest(e, viewerToken, "", http.MethodGet, "/api/v1/incidents/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer detail expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var viewerView struct {
		Incident map[string]any   `json:"incident"`
		Entries  []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &viewerView); err != nil {
		t.Fatalf("decode viewer detail: %v", err)
	}
	if viewerView.Incident["anonymous"] != true {
		t.Fatalf("anonymous flag must be true: %+v", viewerView.Incident)
	}
	if viewerView.Incident["reported_by"] != nil || viewerView.Incident["reported_by_name"] != nil {
		t.Fatalf("viewer must not see reporter identity: %+v", viewerView.Incident)
	}
	if len(viewerView.Entries) != 1 || viewerView.Entries[0]["created_by_name"] != nil {
		t.Fatalf("kronologi must not leak the reporter: %+v", viewerView.Entries)
	}
}

// TestIncidentMerchantIsolation — merchant B (same company) does not see
// merchant A's incident: not in the list, detail and PATCH status both 404
// (pattern: TestPersonDocumentListTenantIsolation).
func TestIncidentMerchantIsolation(t *testing.T) {
	e := setupIncidentEnv(t, "incidentiso.owner")
	ctx := context.Background()
	rec := createIncident(t, e, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	id := incidentIDOf(t, rec)

	merchantB := createTenantMerchant(t, ctx, e.pool, e.router, e.token, e.refreshCookie, e.companyID, "Merchant Incident B")

	rec = incidentRequest(e, e.token, merchantB, http.MethodGet, "/api/v1/incidents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list at merchant B expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list at merchant B: %v", err)
	}
	if len(list.Data) != 0 {
		t.Fatalf("merchant B must not see merchant A incidents, got %+v", list.Data)
	}

	rec = incidentRequest(e, e.token, merchantB, http.MethodGet, "/api/v1/incidents/"+id, nil)
	if rec.Code != http.StatusNotFound || incidentError(t, rec) != "insiden tidak ditemukan" {
		t.Fatalf("detail at merchant B expected 404 insiden tidak ditemukan, got %d: %s", rec.Code, rec.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"status": "feedback", "note": "x"})
	rec = incidentRequest(e, e.token, merchantB, http.MethodPatch, "/api/v1/incidents/"+id+"/status", body)
	if rec.Code != http.StatusNotFound || incidentError(t, rec) != "insiden tidak ditemukan" {
		t.Fatalf("patch status at merchant B expected 404 insiden tidak ditemukan, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIncidentEntryAppendOnly — DB backstop: app_runtime may neither UPDATE
// nor DELETE quality.incident_entry rows (REVOKE, migration 000079; pattern:
// TestBillingDB_ConsentImmutable).
func TestIncidentEntryAppendOnly(t *testing.T) {
	e := setupIncidentEnv(t, "incidentapp.owner")
	rec := createIncident(t, e, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	tx := runtimeTenantTx(t, ctx, e.pool, e.companyID, e.merchantID)
	savepointExec(t, ctx, tx, "update incident entry", "permission denied",
		"UPDATE quality.incident_entry SET body = 'x'")
	savepointExec(t, ctx, tx, "delete incident entry", "permission denied",
		"DELETE FROM quality.incident_entry")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback probe tx: %v", err)
	}
}
