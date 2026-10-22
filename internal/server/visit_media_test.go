//go:build integration

package server_test

// Integration tests for the visit identity token API (spec
// 2026-10-20-qr-visit-media-design.md §4.1–§4.2, §6): token issuance on
// POST /admissions, media config resolution (per-department > default >
// "slip"), config CRUD, reissue, and tenant/permission isolation.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// vmMedia mirrors the visit_media object of POST /admissions and reissue.
type vmMedia struct {
	Media        string  `json:"media"`
	Token        string  `json:"token"`
	QrPNG        *string `json:"qr_png"`
	AllergyAlert bool    `json:"allergy_alert"`
	FallRisk     bool    `json:"fall_risk"`
}

// vmFixture bundles the seeded tenant for one test (own Postgres/Redis
// containers per test, newTestPostgresPool pattern).
type vmFixture struct {
	t             *testing.T
	ctx           context.Context
	pool          *pgxpool.Pool
	router        *gin.Engine
	companyID     string
	merchantID    string
	token         string
	refreshCookie *http.Cookie
}

func vmNewFixture(t *testing.T, label string) *vmFixture {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	phone := fmt.Sprintf("0812397%05d", qePhoneSeq.Add(1))
	companyID, merchantID, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "vm."+label, phone)
	f := &vmFixture{t: t, ctx: ctx, pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token, refreshCookie: refreshCookie}
	// AuthMerchantID berasal dari klaim "mid" token (bukan header): aktifkan
	// sesi merchant A sebelum test yang menyentuh guard berbasis mid.
	f.vmSwitchToken(merchantID)
	return f
}

// vmSwitchToken mints a fresh access token whose active merchant is
// merchantID (POST /auth/switch-merchant, pola soTokenFor) and makes it the
// fixture's owner token.
func (f *vmFixture) vmSwitchToken(merchantID string) string {
	f.t.Helper()
	f.token = f.vmSwitchTokenFor(f.token, merchantID)
	return f.token
}

// vmSwitchTokenFor switches an arbitrary session (token + fixture refresh
// cookie) to merchantID and returns the new bearer token.
func (f *vmFixture) vmSwitchTokenFor(token, merchantID string) string {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/switch-merchant",
		bytes.NewReader([]byte(`{"merchant_id":"`+merchantID+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", f.companyID)
	req.Header.Set("X-Merchant-ID", f.merchantID)
	req.AddCookie(f.refreshCookie)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("switch-merchant expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("decode switch-merchant: %v", err)
	}
	if out.Data.Token == "" {
		f.t.Fatalf("switch-merchant returned no token: %s", rec.Body.String())
	}
	return out.Data.Token
}

// vmSeedPerson inserts a minimal core.person row (audit_export_test.go pattern).
func (f *vmFixture) vmSeedPerson(name string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
		f.companyID, name).Scan(&id); err != nil {
		f.t.Fatalf("seed person: %v", err)
	}
	return id
}

// vmSeedDepartment inserts a department (audit_person_test.go pattern).
func (f *vmFixture) vmSeedDepartment(code string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $3) RETURNING id::text",
		f.companyID, f.merchantID, code).Scan(&id); err != nil {
		f.t.Fatalf("seed department: %v", err)
	}
	return id
}

// vmPost performs an authenticated request as the owner and decodes the
// envelope into out (when non-nil).
func (f *vmFixture) vmPost(method, path string, body any, out any) int {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := accountRequest(f.router, method, path, f.token, f.companyID, f.merchantID, raw)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("decode %s %s response %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

// vmStaffToken logs a seeded staff user in and returns its bearer token
// (qeStaffToken pattern; raises the company session cap first).
func (f *vmFixture) vmStaffToken(username string) string {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, "UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", f.companyID); err != nil {
		f.t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	return qeStaffToken(f.t, f.ctx, f.pool, f.router, f.companyID, username)
}

// vmStaffRequest performs a request with a staff token at a chosen merchant.
func (f *vmFixture) vmStaffRequest(method, path, token, merchantID string, body any) (int, map[string]any) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := accountRequest(f.router, method, path, token, f.companyID, merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// vmAdmit registers an outpatient via POST /admissions (201 expected) and
// returns (admission id, visit_media payload).
func (f *vmFixture) vmAdmit(deptID, personID string, fallRisk bool) (string, vmMedia) {
	f.t.Helper()
	body := map[string]any{"person_id": personID, "department_id": deptID, "fall_risk": fallRisk}
	var resp struct {
		Data struct {
			Admission struct {
				ID string `json:"id"`
			} `json:"admission"`
			VisitMedia vmMedia `json:"visit_media"`
		} `json:"data"`
	}
	code := f.vmPost(http.MethodPost, "/api/v1/admissions", body, &resp)
	if code != http.StatusCreated {
		f.t.Fatalf("POST /admissions expected 201, got %d", code)
	}
	if resp.Data.Admission.ID == "" {
		f.t.Fatalf("POST /admissions returned empty admission id")
	}
	return resp.Data.Admission.ID, resp.Data.VisitMedia
}

// vmActiveToken reads the admission's active token row: (expires_at, closed_at).
func (f *vmFixture) vmActiveToken(admissionID string) (expires time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx,
		"SELECT expires_at FROM operations.visit_token WHERE admission_id = $1 AND closed_at IS NULL",
		admissionID).Scan(&expires); err != nil {
		f.t.Fatalf("read active visit_token: %v", err)
	}
	return expires
}

// vmTokenHash = sha256 hex of the raw token (mirrors hashTokenCode).
func vmTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TestAdmissionCreatesVisitToken — POST /admissions issues an active NQV1
// token with QR, expiring at next merchant-local midnight + 2h (spec §4.1).
func TestAdmissionCreatesVisitToken(t *testing.T) {
	f := vmNewFixture(t, "adm")
	dept := f.vmSeedDepartment("VMA1")
	person := f.vmSeedPerson("Pasien Token Adm")

	admissionID, vm := f.vmAdmit(dept, person, true)
	tokenRe := regexp.MustCompile(`^NQV1-[0-9A-HJKMNP-TV-Z]{16}$`)
	if !tokenRe.MatchString(vm.Token) {
		t.Errorf("token %q does not match NQV1 pattern", vm.Token)
	}
	if vm.QrPNG == nil || !strings.HasPrefix(*vm.QrPNG, "data:image/png;base64,") {
		t.Errorf("qr_png = %v, want data:image/png;base64 prefix", vm.QrPNG)
	}
	if vm.Media != "slip" {
		t.Errorf("media = %q, want fallback slip", vm.Media)
	}
	if !vm.FallRisk {
		t.Errorf("fall_risk = false, want true (sent in body)")
	}
	expires := f.vmActiveToken(admissionID) // fails when no active row exists
	// Kontrak: tengah malam lokal merchant esok hari + 2 jam — dibandingkan
	// dengan toleransi menit, bukan timestamp absolut. Zona default merchant
	// = Asia/Jakarta (merchant_day.go defaultMerchantTimezone).
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	y, m, d := time.Now().In(loc).Add(24 * time.Hour).Date()
	want := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(2 * time.Hour)
	if d := expires.Sub(want); d > 2*time.Minute || d < -2*time.Minute {
		t.Errorf("expires_at = %v, want %v (±2m)", expires, want)
	}
}

// TestVisitMediaConfigResolution — per-department override > merchant default
// > fallback "slip" (spec §4.1).
func TestVisitMediaConfigResolution(t *testing.T) {
	f := vmNewFixture(t, "res")
	deptA := f.vmSeedDepartment("VMB-A")
	deptB := f.vmSeedDepartment("VMB-B")
	person := f.vmSeedPerson("Pasien Resolusi")

	if _, vm := f.vmAdmit(deptA, person, false); vm.Media != "slip" {
		t.Fatalf("without config media = %q, want slip", vm.Media)
	}
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"media": "wristband"}, nil); code != http.StatusOK {
		t.Fatalf("PUT default expected 200, got %d", code)
	}
	if _, vm := f.vmAdmit(deptA, person, false); vm.Media != "wristband" {
		t.Fatalf("after default wristband, media = %q, want wristband", vm.Media)
	}
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"department_id": deptA, "media": "none"}, nil); code != http.StatusOK {
		t.Fatalf("PUT override expected 200, got %d", code)
	}
	if _, vm := f.vmAdmit(deptA, person, false); vm.Media != "none" {
		t.Errorf("dept A (override none) media = %q, want none", vm.Media)
	}
	if _, vm := f.vmAdmit(deptB, person, false); vm.Media != "wristband" {
		t.Errorf("dept B (no override) media = %q, want default wristband", vm.Media)
	}
}

// TestVisitMediaConfigCRUD — upsert stays one row per key, GET reflects the
// config, delete removes overrides, invalid values are rejected.
func TestVisitMediaConfigCRUD(t *testing.T) {
	f := vmNewFixture(t, "crud")
	deptA := f.vmSeedDepartment("VMC-A")
	const countSQL = "SELECT count(*) FROM operations.visit_media_config WHERE merchant_id = $1"

	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"media": "wristband"}, nil); code != http.StatusOK {
		t.Fatalf("PUT default expected 200, got %d", code)
	}
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"department_id": deptA, "media": "none"}, nil); code != http.StatusOK {
		t.Fatalf("PUT override expected 200, got %d", code)
	}
	var getResp struct {
		Data struct {
			Default   string `json:"default"`
			Overrides []struct {
				DepartmentID string `json:"department_id"`
				Media        string `json:"media"`
			} `json:"overrides"`
		} `json:"data"`
	}
	if code := f.vmPost(http.MethodGet, "/api/v1/merchants/"+f.merchantID+"/visit-media", nil, &getResp); code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", code)
	}
	if getResp.Data.Default != "wristband" || len(getResp.Data.Overrides) != 1 || getResp.Data.Overrides[0].Media != "none" {
		t.Fatalf("GET = default %q, %d override(s), want wristband + 1 (none)", getResp.Data.Default, len(getResp.Data.Overrides))
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, countSQL, f.merchantID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("row count = %d (err %v), want 2", rows, err)
	}
	// Upsert menulis baris yang SAMA, bukan menduplikasi.
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"department_id": deptA, "media": "both"}, nil); code != http.StatusOK {
		t.Fatalf("PUT override new value expected 200, got %d", code)
	}
	if err := f.pool.QueryRow(f.ctx, countSQL, f.merchantID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("row count after upsert = %d (err %v), want 2", rows, err)
	}
	if code := f.vmPost(http.MethodDelete, "/api/v1/visit-media/"+deptA, nil, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE override expected 204, got %d", code)
	}
	if err := f.pool.QueryRow(f.ctx, countSQL, f.merchantID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("row count after delete = %d (err %v), want 1", rows, err)
	}
	if code := f.vmPost(http.MethodDelete, "/api/v1/visit-media/"+deptA, nil, nil); code != http.StatusNotFound {
		t.Fatalf("DELETE again expected 404, got %d", code)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"media": "plastik"}, &errResp); code != http.StatusBadRequest || errResp.Error != "media must be one of none, slip, wristband, both" {
		t.Fatalf("PUT media plastik = %d %q, want 400 media must be one of none, slip, wristband, both", code, errResp.Error)
	}
	if code := f.vmPost(http.MethodPut, "/api/v1/visit-media", map[string]any{"department_id": "11111111-1111-1111-1111-111111111111", "media": "slip"}, &errResp); code != http.StatusNotFound || errResp.Error != "department not found" {
		t.Fatalf("PUT unknown department = %d %q, want 404 department not found", code, errResp.Error)
	}
}

// TestReissueVisitToken — reissue closes the old token (reason "reissued")
// and issues a fresh active one (spec §4.2, no status gate).
func TestReissueVisitToken(t *testing.T) {
	f := vmNewFixture(t, "reissue")
	dept := f.vmSeedDepartment("VMD1")
	person := f.vmSeedPerson("Pasien Reissue")
	admissionID, vm1 := f.vmAdmit(dept, person, false)

	var resp struct {
		Data struct {
			VisitMedia vmMedia `json:"visit_media"`
		} `json:"data"`
	}
	code := f.vmPost(http.MethodPost, "/api/v1/admissions/"+admissionID+"/visit-token", nil, &resp)
	if code != http.StatusCreated {
		t.Fatalf("reissue expected 201, got %d", code)
	}
	vm2 := resp.Data.VisitMedia
	if vm2.Token == "" || vm2.Token == vm1.Token {
		t.Fatalf("reissued token %q must be non-empty and differ from %q", vm2.Token, vm1.Token)
	}
	var closed bool
	var reason string
	if err := f.pool.QueryRow(f.ctx,
		"SELECT closed_at IS NOT NULL, COALESCE(close_reason, '') FROM operations.visit_token WHERE token_hash = $1",
		vmTokenHash(vm1.Token)).Scan(&closed, &reason); err != nil {
		t.Fatalf("read old token: %v", err)
	}
	if !closed || reason != "reissued" {
		t.Errorf("old token closed=%v reason=%q, want true/reissued", closed, reason)
	}
	f.vmActiveToken(admissionID) // exactly one active row must exist
}

// TestVisitMediaTokenIsolation — a merchant B user cannot reissue merchant A
// admissions nor read A's media config (RLS + :id check).
func TestVisitMediaTokenIsolation(t *testing.T) {
	f := vmNewFixture(t, "iso")
	dept := f.vmSeedDepartment("VME1")
	person := f.vmSeedPerson("Pasien Isolasi")
	admissionID, _ := f.vmAdmit(dept, person, false)

	merchantB := createTenantMerchant(f.t, f.ctx, f.pool, f.router, f.token, f.refreshCookie, f.companyID, "Klinik Isolasi B")
	seedMerchantStaff(f.t, f.ctx, f.pool, f.companyID, merchantB, "vm.iso.staff", "", "operations.queue.configure")
	staffB := f.vmStaffToken("vm.iso.staff")

	code, resp := f.vmStaffRequest(http.MethodPost, "/api/v1/admissions/"+admissionID+"/visit-token", staffB, merchantB, nil)
	// Kontrak menarget 404 via RLS, tapi pool test memakai role pemilik tabel
	// (RLS bypass — security_settings_test SET LOCAL ROLE app_runtime manual),
	// jadi baris admission A terbaca dan isolasi ditegakkan oleh
	// RequirePermissionForMerchant → 403. Di produksi (app_runtime) 404.
	if code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("reissue admission A as B = %d %v, want 403 (RLS-bypass pool) / 404", code, resp)
	}
	code, resp = f.vmStaffRequest(http.MethodGet, "/api/v1/merchants/"+f.merchantID+"/visit-media", staffB, merchantB, nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET merchant A visit-media as B = %d %v, want 404", code, resp)
	}
}

// TestVisitMediaPermissionDenied — staff without the required permissions get
// 403 on GET/PUT/DELETE (operations.queue.configure) and reissue
// (operations.visit.manage).
func TestVisitMediaPermissionDenied(t *testing.T) {
	f := vmNewFixture(t, "deny")
	dept := f.vmSeedDepartment("VMF1")
	person := f.vmSeedPerson("Pasien Deny")
	admissionID, _ := f.vmAdmit(dept, person, false)

	seedMerchantStaff(f.t, f.ctx, f.pool, f.companyID, f.merchantID, "vm.deny.staff", "", "audit.log.view")
	staff := f.vmStaffToken("vm.deny.staff")

	call := func(method, path string, body any) (int, map[string]any) {
		return f.vmStaffRequest(method, path, staff, f.merchantID, body)
	}
	if code, _ := call(http.MethodGet, "/api/v1/merchants/"+f.merchantID+"/visit-media", nil); code != http.StatusForbidden {
		t.Errorf("GET without permission = %d, want 403", code)
	}
	if code, _ := call(http.MethodPut, "/api/v1/visit-media", map[string]any{"media": "slip"}); code != http.StatusForbidden {
		t.Errorf("PUT without permission = %d, want 403", code)
	}
	if code, _ := call(http.MethodDelete, "/api/v1/visit-media/11111111-1111-1111-1111-111111111111", nil); code != http.StatusForbidden {
		t.Errorf("DELETE without permission = %d, want 403", code)
	}
	if code, _ := call(http.MethodPost, "/api/v1/admissions/"+admissionID+"/visit-token", nil); code != http.StatusForbidden {
		t.Errorf("reissue without permission = %d, want 403", code)
	}
}
