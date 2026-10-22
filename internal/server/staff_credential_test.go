//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bytes"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedCredentialUser inserts a staff app_user (email included, "" = NULL) at
// merchantID attached to a fresh role granted exactly perms — mirrors
// seedMerchantStaff WITHOUT the pin: userID/roleID/username must be unique per
// call within a test (core.role UNIQUE (company_id,name), core.app_user
// UNIQUE (company_id,username), migration 000004).
func seedCredentialUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID, roleID, username, email string, perms ...string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, companyID, username); err != nil {
		t.Fatalf("seed credential role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash credential staff password: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.app_user (id, company_id, username, email, password_hash, is_active) VALUES ($1, $2, $3, NULLIF($4,''), $5, true)",
		userID, companyID, username, email, passwordHash,
	); err != nil {
		t.Fatalf("seed credential staff user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed credential user_merchant_role: %v", err)
	}
	for _, code := range perms {
		if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, code); err != nil {
			t.Fatalf("grant %s to credential role: %v", code, err)
		}
	}
	return userID
}

// credentialToken mints an access token for a seeded credential staff user.
func credentialToken(t *testing.T, userID, companyID, merchantID, roleID, username string) string {
	t.Helper()
	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, roleID, username, "cred-device", time.Hour)
	if err != nil {
		t.Fatalf("generate credential token: %v", err)
	}
	return token
}

// credentialRequest performs a staff-credential route request with the
// standard tenant/auth headers.
func credentialRequest(t *testing.T, router http.Handler, token, companyID, merchantID, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// credentialError decodes the {"error": ...} body of a non-2xx response.
func credentialError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return body.Error
}

// seedPhysicianWithLicenses inserts core.person + core.physician with STR/SIP
// numbers directly (pool role bypasses RLS; pattern schedule_calendar_test.go).
func seedPhysicianWithLicenses(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, personID, physicianID, fullName, strNumber, sipNumber, sipValidUntil string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, $3, 'male')", personID, companyID, fullName); err != nil {
		t.Fatalf("seed physician person: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.physician (id, company_id, merchant_id, person_id, str_number, sip_number, sip_valid_until) VALUES ($1, $2, $3, $4, $5, $6, $7::date)",
		physicianID, companyID, merchantID, personID, strNumber, sipNumber, sipValidUntil,
	); err != nil {
		t.Fatalf("seed physician: %v", err)
	}
}

// insertStaffCredential seeds one core.staff_credential row directly.
func insertStaffCredential(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID, credID, kind, licenseNumber, validUntil, createdBy string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.staff_credential (id, company_id, merchant_id, user_id, credential_kind, license_number, valid_until, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8)",
		credID, companyID, merchantID, userID, kind, licenseNumber, validUntil, createdBy,
	); err != nil {
		t.Fatalf("seed staff_credential: %v", err)
	}
}

// merchantToday formats a day offset from today in the merchant timezone
// (Asia/Jakarta — core.merchant default) as YYYY-MM-DD.
func merchantToday(t *testing.T, offsetDays int) string {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}
	return time.Now().In(loc).AddDate(0, 0, offsetDays).Format("2006-01-02")
}

// statusRow is one entry of GET /staff-credential-status.
type statusRow struct {
	StaffKind      string  `json:"staff_kind"`
	UserID         *string `json:"user_id"`
	Name           string  `json:"name"`
	CredentialKind string  `json:"credential_kind"`
	LicenseNumber  string  `json:"license_number"`
	ValidUntil     *string `json:"valid_until"`
	DaysRemaining  *int    `json:"days_remaining"`
	Status         string  `json:"status"`
}

// decodeCredentialStatus pulls data[] + meta.count out of a status response.
func decodeCredentialStatus(t *testing.T, rec *httptest.ResponseRecorder) ([]statusRow, int) {
	t.Helper()
	var resp struct {
		Data []statusRow `json:"data"`
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status response %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return resp.Data, resp.Meta.Count
}

// findStatusRow returns the row with the given credential kind + license.
func findStatusRow(t *testing.T, rows []statusRow, kind, license string) statusRow {
	t.Helper()
	for _, r := range rows {
		if r.CredentialKind == kind && r.LicenseNumber == license {
			return r
		}
	}
	t.Fatalf("status row %s/%s not found in %d rows", kind, license, len(rows))
	return statusRow{}
}

func TestStaffCredentialStatusIncludesPhysicianAndStaff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "cred.status.owner", "081234570001")

	staffUserID := seedCredentialUser(t, ctx, pool, companyID, merchantID,
		"11111111-1111-4111-8111-111111111111", "12121212-2121-4212-8121-212121212121",
		"cred.status.staff", "cred.status.staff@example.com", server.PermUserManage)
	token := credentialToken(t, staffUserID, companyID, merchantID, "12121212-2121-4212-8121-212121212121", "cred.status.staff")

	// Physician with STR (no valid_until column → unknown) + SIP (future → valid).
	seedPhysicianWithLicenses(t, ctx, pool, companyID, merchantID,
		"31313131-3131-4311-8131-313131313131", "32323232-3232-4322-8232-323232323232",
		"dr. Andi Kredensial", "STR-ANDI-1", "SIP-ANDI-1", "2030-06-30")
	// One staff_credential row with a valid_until far in the future.
	insertStaffCredential(t, ctx, pool, companyID, merchantID, staffUserID,
		"41414141-4141-4141-8141-414141414141", "sipa", "SIPA/STAFF/1", "2031-01-31", staffUserID)

	rec := credentialRequest(t, router, token, companyID, merchantID, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows, count := decodeCredentialStatus(t, rec)

	// 2 physician rows (str + sip) + 1 staff row.
	if count != 3 || len(rows) != 3 {
		t.Fatalf("expected 3 status rows, got count=%d len=%d: %s", count, len(rows), rec.Body.String())
	}
	str := findStatusRow(t, rows, "str", "STR-ANDI-1")
	if str.Status != "unknown" || str.ValidUntil != nil || str.DaysRemaining != nil {
		t.Fatalf("physician str row must be unknown with null valid_until/days, got %+v", str)
	}
	if str.StaffKind != "physician" || str.Name != "dr. Andi Kredensial" {
		t.Fatalf("physician str row fields wrong: %+v", str)
	}
	sip := findStatusRow(t, rows, "sip", "SIP-ANDI-1")
	if sip.Status != "valid" || sip.ValidUntil == nil || *sip.ValidUntil != "2030-06-30" {
		t.Fatalf("physician sip row must be valid until 2030-06-30, got %+v", sip)
	}
	sipa := findStatusRow(t, rows, "sipa", "SIPA/STAFF/1")
	if sipa.StaffKind != "staff" || sipa.Status != "valid" {
		t.Fatalf("staff sipa row wrong: %+v", sipa)
	}
}

func TestStaffCredentialStatusBoundary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "cred.boundary.owner", "081234570002")

	uid := "11111111-1111-4111-8111-111111111111"
	rid := "12121212-2121-4212-8121-212121212121"
	staffUserID := seedCredentialUser(t, ctx, pool, companyID, merchantID, uid, rid, "cred.boundary.staff", "", server.PermUserManage)
	token := credentialToken(t, staffUserID, companyID, merchantID, rid, "cred.boundary.staff")

	// Three credential holders: yesterday (expired), today (expiring, 0), +31d (valid).
	users := []struct {
		id, username string
		offset       int
	}{
		{"51515151-5151-4151-8151-515151515151", "cred.boundary.u1", -1},
		{"52525252-5252-4252-8252-525252525252", "cred.boundary.u2", 0},
		{"53535353-5353-4353-8353-535353535353", "cred.boundary.u3", 31},
	}
	for i, u := range users {
		seedCredentialUser(t, ctx, pool, companyID, merchantID, u.id,
			// role ids distinct per call
			[]string{"61616161-6161-4161-8161-616161616161", "62626262-6262-4262-8262-626262626262", "63636363-6363-4363-8363-636363636363"}[i],
			u.username, "")
		insertStaffCredential(t, ctx, pool, companyID, merchantID, u.id,
			[]string{"71717171-7171-4171-8171-717171717171", "72727272-7272-4272-8272-727272727272", "73737373-7373-4373-8373-737373737373"}[i],
			"str", "STR-BND-"+u.username, merchantToday(t, u.offset), staffUserID)
	}

	rec := credentialRequest(t, router, token, companyID, merchantID, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows, count := decodeCredentialStatus(t, rec)
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d: %s", count, rec.Body.String())
	}
	// Boundary: days computed against merchant-local today (contract).
	y := findStatusRow(t, rows, "str", "STR-BND-cred.boundary.u1")
	if y.Status != "expired" || y.DaysRemaining == nil || *y.DaysRemaining != -1 {
		t.Fatalf("yesterday must be expired days=-1, got %+v", y)
	}
	t0 := findStatusRow(t, rows, "str", "STR-BND-cred.boundary.u2")
	if t0.Status != "expiring" || t0.DaysRemaining == nil || *t0.DaysRemaining != 0 {
		t.Fatalf("today must be expiring days=0, got %+v", t0)
	}
	f := findStatusRow(t, rows, "str", "STR-BND-cred.boundary.u3")
	if f.Status != "valid" || f.DaysRemaining == nil || *f.DaysRemaining != 31 {
		t.Fatalf("today+31 must be valid days=31, got %+v", f)
	}
	// Severity ordering: expired first, then expiring, then valid.
	if rows[0].Status != "expired" || rows[1].Status != "expiring" || rows[2].Status != "valid" {
		t.Fatalf("rows must be ordered by severity, got %s/%s/%s", rows[0].Status, rows[1].Status, rows[2].Status)
	}
}

func TestStaffCredentialCRUDLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "cred.crud.owner", "081234570003")

	uid := "11111111-1111-4111-8111-111111111111"
	rid := "12121212-2121-4212-8121-212121212121"
	staffUserID := seedCredentialUser(t, ctx, pool, companyID, merchantID, uid, rid, "cred.crud.staff", "", server.PermUserManage)
	token := credentialToken(t, staffUserID, companyID, merchantID, rid, "cred.crud.staff")

	body := `{"user_id":"` + uid + `","credential_kind":"sip","license_number":"SIP/CRUD/1","valid_until":"2030-01-31"}`
	rec := credentialRequest(t, router, token, companyID, merchantID, http.MethodPost, "/api/v1/staff-credentials", []byte(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			ID         string  `json:"id"`
			UserID     string  `json:"user_id"`
			Kind       string  `json:"credential_kind"`
			License    string  `json:"license_number"`
			ValidUntil *string `json:"valid_until"`
			RowVersion int     `json:"row_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode POST response: %v: %s", err, rec.Body.String())
	}
	if created.Data.ID == "" || created.Data.UserID != uid || created.Data.Kind != "sip" ||
		created.Data.License != "SIP/CRUD/1" || created.Data.ValidUntil == nil || *created.Data.ValidUntil != "2030-01-31" ||
		created.Data.RowVersion != 1 {
		t.Fatalf("POST response wrong: %s", rec.Body.String())
	}
	credID := created.Data.ID

	// PATCH replaces both fields.
	patch := `{"license_number":"SIP/CRUD/2","valid_until":"2031-02-28"}`
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodPatch, "/api/v1/staff-credentials/"+credID, []byte(patch))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET status reflects the new values.
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status GET expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows, _ := decodeCredentialStatus(t, rec)
	row := findStatusRow(t, rows, "sip", "SIP/CRUD/2")
	if row.ValidUntil == nil || *row.ValidUntil != "2031-02-28" {
		t.Fatalf("status must reflect patched values, got %+v", row)
	}

	// DELETE → 204, row gone from status.
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodDelete, "/api/v1/staff-credentials/"+credID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodGet, "/api/v1/staff-credential-status", nil)
	rows, _ = decodeCredentialStatus(t, rec)
	for _, r := range rows {
		if r.LicenseNumber == "SIP/CRUD/2" {
			t.Fatalf("deleted credential must not appear in status: %s", rec.Body.String())
		}
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", len(rows))
	}
}

func TestStaffCredentialDuplicateKind409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "cred.dup.owner", "081234570004")

	uid := "11111111-1111-4111-8111-111111111111"
	rid := "12121212-2121-4212-8121-212121212121"
	staffUserID := seedCredentialUser(t, ctx, pool, companyID, merchantID, uid, rid, "cred.dup.staff", "", server.PermUserManage)
	token := credentialToken(t, staffUserID, companyID, merchantID, rid, "cred.dup.staff")

	postBody := `{"user_id":"` + uid + `","credential_kind":"str","license_number":"STR/DUP/1"}`
	rec := credentialRequest(t, router, token, companyID, merchantID, http.MethodPost, "/api/v1/staff-credentials", []byte(postBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first POST expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodPost, "/api/v1/staff-credentials", []byte(postBody))
	if rec.Code != http.StatusConflict || credentialError(t, rec) != "staff credential already exists" {
		t.Fatalf("duplicate POST expected 409 staff credential already exists, got %d: %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if _, count := decodeCredentialStatus(t, rec); count != 1 {
		t.Fatalf("expected exactly 1 credential before delete, got %d", count)
	}
	// Re-run via sipa: create → capture id → delete → re-create (the str pair
	// above stays, so the unique index must not block the soft-deleted row).
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodPost, "/api/v1/staff-credentials", []byte(`{"user_id":"`+uid+`","credential_kind":"sipa","license_number":"SIPA/DUP/1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("sipa POST expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode sipa POST: %v", err)
	}
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodDelete, "/api/v1/staff-credentials/"+created.Data.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = credentialRequest(t, router, token, companyID, merchantID, http.MethodPost, "/api/v1/staff-credentials", []byte(`{"user_id":"`+uid+`","credential_kind":"sipa","license_number":"SIPA/DUP/1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-POST after soft-delete expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStaffCredentialTenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantA, _, ownerToken, ownerCookie := seedAccountOwner(t, ctx, pool, router, "cred.iso.owner", "081234570005")

	// Merchant B under the same company, through the real API (needs the
	// refresh cookie — CreateMerchantHandler reissues a merchant token).
	merchantB := createTenantMerchant(t, ctx, pool, router, ownerToken, ownerCookie, companyID, "Klinik Iso B")

	// Merchant A staff (has core.user.manage) + Merchant B staff.
	uidA := "11111111-1111-4111-8111-111111111111"
	ridA := "12121212-2121-4212-8121-212121212121"
	userA := seedCredentialUser(t, ctx, pool, companyID, merchantA, uidA, ridA, "cred.iso.staffA", "", server.PermUserManage)
	tokenA := credentialToken(t, userA, companyID, merchantA, ridA, "cred.iso.staffA")

	uidB := "81818181-8181-4181-8181-818181818181"
	ridB := "82828282-8282-4282-8282-828282828282"
	userB := seedCredentialUser(t, ctx, pool, companyID, merchantB, uidB, ridB, "cred.iso.staffB", "", server.PermUserManage)
	tokenB := credentialToken(t, userB, companyID, merchantB, ridB, "cred.iso.staffB")

	// A credential owned by merchant A's user, seeded directly.
	insertStaffCredential(t, ctx, pool, companyID, merchantA, userA,
		"41414141-4141-4141-8141-414141414141", "sip", "SIP/ISO/A", "2030-01-31", userA)

	// B's status does not include A's license.
	rec := credentialRequest(t, router, tokenB, companyID, merchantB, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("B status expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rows, count := decodeCredentialStatus(t, rec); count != 0 {
		t.Fatalf("B must not see A's credential, got %d rows: %s", count, rec.Body.String())
	} else {
		_ = rows
	}

	// PATCH/DELETE of A's id from B's context → 404.
	rec = credentialRequest(t, router, tokenB, companyID, merchantB, http.MethodPatch, "/api/v1/staff-credentials/41414141-4141-4141-8141-414141414141", []byte(`{"license_number":"HACKED"}`))
	if rec.Code != http.StatusNotFound || credentialError(t, rec) != "staff credential not found" {
		t.Fatalf("B PATCH of A credential expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = credentialRequest(t, router, tokenB, companyID, merchantB, http.MethodDelete, "/api/v1/staff-credentials/41414141-4141-4141-8141-414141414141", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("B DELETE of A credential expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// POST from A's context with B's user (same company, non-member of A) → 404,
	// indistinguishable from a missing user.
	rec = credentialRequest(t, router, tokenA, companyID, merchantA, http.MethodPost, "/api/v1/staff-credentials",
		[]byte(`{"user_id":"`+uidB+`","credential_kind":"sip","license_number":"SIP/X","valid_until":"2030-01-31"}`))
	if rec.Code != http.StatusNotFound || credentialError(t, rec) != "user not found" {
		t.Fatalf("A POST with B's user expected 404 user not found, got %d: %s", rec.Code, rec.Body.String())
	}

	// Sanity: A still sees its own credential.
	rec = credentialRequest(t, router, tokenA, companyID, merchantA, http.MethodGet, "/api/v1/staff-credential-status", nil)
	if _, count := decodeCredentialStatus(t, rec); count != 1 {
		t.Fatalf("A must still see its own credential, got %d: %s", count, rec.Body.String())
	}
}

func TestStaffCredentialPermissionDenied(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "cred.perm.owner", "081234570006")

	// Staff WITHOUT core.user.manage.
	uid := "91919191-9191-4191-8191-919191919191"
	rid := "92929292-9292-4292-8292-929292929292"
	staffUserID := seedCredentialUser(t, ctx, pool, companyID, merchantID, uid, rid, "cred.perm.staff", "")
	token := credentialToken(t, staffUserID, companyID, merchantID, rid, "cred.perm.staff")

	cases := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/api/v1/staff-credential-status", nil},
		{http.MethodPost, "/api/v1/staff-credentials", []byte(`{"user_id":"` + uid + `","credential_kind":"sip","license_number":"SIP/X"}`)},
		{http.MethodPatch, "/api/v1/staff-credentials/41414141-4141-4141-8141-414141414141", []byte(`{"license_number":"X"}`)},
		{http.MethodDelete, "/api/v1/staff-credentials/41414141-4141-4141-8141-414141414141", nil},
	}
	for _, tc := range cases {
		rec := credentialRequest(t, router, token, companyID, merchantID, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s expected 403, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if msg := credentialError(t, rec); !strings.Contains(msg, "core.user.manage") {
			t.Fatalf("%s %s error must mention core.user.manage, got %q", tc.method, tc.path, msg)
		}
	}
}
