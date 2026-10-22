//go:build integration

package server

// Integration tests for the credential expiry check worker (plan
// 2026-10-20-staff-credential-expiry-a2-email-worker Task 2, spec
// 2026-10-20-staff-credential-expiry-design §4.3). processCredentialChecks is
// unexported, so this file is in package server; the container/tenant
// container helper is reused from stock_ledger_test.go and the user seeding
// mirrors the package server_test helpers (seedCredentialUser pattern) in
// minimal form.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/config"
)

// credentialWorkerTenant seeds one company + merchant with fixed UUIDs; every
// test runs in its own container so IDs can repeat across tests.
func credentialWorkerTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (companyID, merchantID string) {
	t.Helper()
	companyID = "aa000000-0000-4000-8000-0000000000c1"
	merchantID = "bb000000-0000-4000-8000-0000000000c2"
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, $2, $3)",
		companyID, "credw", "Klinik Credential Worker"); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, $3, $4)",
		merchantID, companyID, "credw-main", "Klinik Credential Worker"); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	return companyID, merchantID
}

// credentialWorkerSeedUser mirrors seedCredentialUser (staff_credential_test.go,
// package server_test): userID/roleID/username unique per call, "" email = NULL.
func credentialWorkerSeedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID, roleID, username, email string, perms ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, companyID, username); err != nil {
		t.Fatalf("seed worker role: %v", err)
	}
	passwordHash := "$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$RdescudvJCsgt3ub+b+dWRWJTmaaJObG"
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.app_user (id, company_id, username, email, password_hash, is_active) VALUES ($1, $2, $3, NULLIF($4,''), $5, true)",
		userID, companyID, username, email, passwordHash,
	); err != nil {
		t.Fatalf("seed worker user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed worker user_merchant_role: %v", err)
	}
	for _, code := range perms {
		if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, code); err != nil {
			t.Fatalf("grant %s to worker role: %v", code, err)
		}
	}
}

// credentialWorkerSeedLicense inserts one core.staff_credential row directly;
// validUntil takes a "YYYY-MM-DD" string or nil (unknown).
func credentialWorkerSeedLicense(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, userID, credID, kind, licenseNumber string, validUntil any, createdBy string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.staff_credential (id, company_id, merchant_id, user_id, credential_kind, license_number, valid_until, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8)",
		credID, companyID, merchantID, userID, kind, licenseNumber, validUntil, createdBy,
	); err != nil {
		t.Fatalf("seed worker staff_credential: %v", err)
	}
}

// credentialToday formats a day offset from today in the merchant timezone
// (Asia/Jakarta — core.merchant default) as YYYY-MM-DD.
func credentialToday(offsetDays int) string {
	loc, _ := time.LoadLocation(defaultMerchantTimezone)
	return time.Now().In(loc).AddDate(0, 0, offsetDays).Format("2006-01-02")
}

// credentialOutboxCount counts staff_credential_expiry outbox rows.
func credentialOutboxCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.email_outbox WHERE kind = 'staff_credential_expiry'").Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// credentialWorkerSeedFixture wires the shared four-user tenant: an admin with
// core.user.manage, a license owner with an email, a license owner without an
// email, and a user holding only core.role.manage.
func credentialWorkerSeedFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (companyID, merchantID, adminID, ownerID string) {
	t.Helper()
	companyID, merchantID = credentialWorkerTenant(t, ctx, pool)
	adminID = "cc000000-0000-4000-8000-000000000001"
	ownerID = "cc000000-0000-4000-8000-000000000002"
	noEmailID := "cc000000-0000-4000-8000-000000000003"
	otherID := "cc000000-0000-4000-8000-000000000004"
	credentialWorkerSeedUser(t, ctx, pool, companyID, merchantID, adminID, "cd000000-0000-4000-8000-000000000001", "credw.admin", "credw.admin@example.com", PermUserManage)
	credentialWorkerSeedUser(t, ctx, pool, companyID, merchantID, ownerID, "cd000000-0000-4000-8000-000000000002", "credw.owner", "credw.owner@example.com")
	credentialWorkerSeedUser(t, ctx, pool, companyID, merchantID, noEmailID, "cd000000-0000-4000-8000-000000000003", "credw.noemail", "")
	credentialWorkerSeedUser(t, ctx, pool, companyID, merchantID, otherID, "cd000000-0000-4000-8000-000000000004", "credw.other", "credw.other@example.com", PermRoleManage)
	return companyID, merchantID, adminID, ownerID
}

func TestCredentialCheckWorkerDedupOncePerDay(t *testing.T) {
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	_, merchantID, adminID, ownerID := credentialWorkerSeedFixture(t, ctx, pool)

	credentialWorkerSeedLicense(t, ctx, pool,
		"aa000000-0000-4000-8000-0000000000c1", merchantID, ownerID,
		"ee000000-0000-4000-8000-000000000001", "str", "STR/WRK/1", credentialToday(7), ownerID)

	cfg := testCredentialWorkerCfg()
	// Expectation from the contract: recipients = 1 emailed owner + 1 admin.
	if err := processCredentialChecks(ctx, pool, cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if n := credentialOutboxCount(t, ctx, pool); n != 2 {
		t.Fatalf("first run must enqueue 2 emails (owner + admin), got %d", n)
	}
	if err := processCredentialChecks(ctx, pool, cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := credentialOutboxCount(t, ctx, pool); n != 2 {
		t.Fatalf("dedup: second run must not duplicate, got %d", n)
	}
	if err := processCredentialChecks(ctx, pool, cfg); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if n := credentialOutboxCount(t, ctx, pool); n != 2 {
		t.Fatalf("dedup: third run must not duplicate, got %d", n)
	}
	_ = adminID
}

// TestCredentialCheckWorkerSkipsValidAndUnknown covers rows the scan/classifier
// must skip: +60 days (valid, beyond the padded deadline), NULL valid_until
// (unknown) and a physician STR without a license number — 0 outbox rows.
func TestCredentialCheckWorkerSkipsValidAndUnknown(t *testing.T) {
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	companyID, merchantID, _, ownerID := credentialWorkerSeedFixture(t, ctx, pool)

	credentialWorkerSeedLicense(t, ctx, pool, companyID, merchantID, ownerID,
		"ee000000-0000-4000-8000-000000000002", "str", "STR/WRK/VALID", credentialToday(60), ownerID)
	credentialWorkerSeedLicense(t, ctx, pool, companyID, merchantID, ownerID,
		"ee000000-0000-4000-8000-000000000003", "sip", "SIP/WRK/NULL", nil, ownerID)
	// Physician whose STR number is NULL (no license_number to remind about).
	personID := "dd000000-0000-4000-8000-000000000001"
	physicianID := "dd000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, $3, 'male')", personID, companyID, "dr. Tanpa STR"); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)",
		physicianID, companyID, merchantID, personID); err != nil {
		t.Fatalf("seed physician: %v", err)
	}

	if err := processCredentialChecks(ctx, pool, testCredentialWorkerCfg()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := credentialOutboxCount(t, ctx, pool); n != 0 {
		t.Fatalf("valid/unknown/no-license rows must enqueue nothing, got %d", n)
	}
}

func TestCredentialCheckWorkerAdminDigest(t *testing.T) {
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	companyID, merchantID, adminID, ownerID := credentialWorkerSeedFixture(t, ctx, pool)
	noEmailID := "cc000000-0000-4000-8000-000000000003"

	credentialWorkerSeedLicense(t, ctx, pool, companyID, merchantID, ownerID,
		"ee000000-0000-4000-8000-000000000004", "str", "STR/WRK/D1", credentialToday(7), ownerID)
	credentialWorkerSeedLicense(t, ctx, pool, companyID, merchantID, noEmailID,
		"ee000000-0000-4000-8000-000000000005", "sip", "SIP/WRK/D2", credentialToday(3), ownerID)

	if err := processCredentialChecks(ctx, pool, testCredentialWorkerCfg()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Recipients: owner (email) + admin digest; the no-email owner joins only
	// the digest, the core.role.manage-only user gets nothing.
	if n := credentialOutboxCount(t, ctx, pool); n != 2 {
		t.Fatalf("expected 2 outbox rows (owner + admin), got %d", n)
	}
	var to, subject, body string
	if err := pool.QueryRow(ctx,
		"SELECT to_address, subject, body_text FROM core.email_outbox WHERE kind = 'staff_credential_expiry' AND to_address = $1",
		"credw.owner@example.com").Scan(&to, &subject, &body); err != nil {
		t.Fatalf("owner email missing: %v", err)
	}
	if subject != "Pengingat: masa berlaku STR/SIP/SIPA" {
		t.Fatalf("wrong subject: %q", subject)
	}
	if !containsAll(body, "STR/WRK/D1") || containsAll(body, "SIP/WRK/D2") {
		t.Fatalf("owner email must carry only its own license, got: %s", body)
	}
	if err := pool.QueryRow(ctx,
		"SELECT to_address, subject, body_text FROM core.email_outbox WHERE kind = 'staff_credential_expiry' AND to_address = $1",
		"credw.admin@example.com").Scan(&to, &subject, &body); err != nil {
		t.Fatalf("admin digest missing: %v", err)
	}
	if subject != "Pengingat: masa berlaku STR/SIP/SIPA" {
		t.Fatalf("wrong digest subject: %q", subject)
	}
	if !containsAll(body, "STR/WRK/D1", "SIP/WRK/D2") {
		t.Fatalf("admin digest must carry ALL items, got: %s", body)
	}
	var others int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.email_outbox WHERE kind = 'staff_credential_expiry' AND to_address = ANY($1)",
		[]string{"credw.other@example.com", "credw.noemail@example.com"}).Scan(&others); err != nil {
		t.Fatalf("count others: %v", err)
	}
	if others != 0 {
		t.Fatalf("no-email and other-permission users must get nothing, got %d", others)
	}
	_ = adminID
}

func TestCredentialCheckWorkerExpiredIncluded(t *testing.T) {
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	companyID, merchantID, _, ownerID := credentialWorkerSeedFixture(t, ctx, pool)

	credentialWorkerSeedLicense(t, ctx, pool, companyID, merchantID, ownerID,
		"ee000000-0000-4000-8000-000000000006", "str", "STR/WRK/EXP", credentialToday(-1), ownerID)

	if err := processCredentialChecks(ctx, pool, testCredentialWorkerCfg()); err != nil {
		t.Fatalf("run: %v", err)
	}
	var body string
	if err := pool.QueryRow(ctx,
		"SELECT body_text FROM core.email_outbox WHERE kind = 'staff_credential_expiry' AND to_address = $1",
		"credw.owner@example.com").Scan(&body); err != nil {
		t.Fatalf("expired owner email missing: %v", err)
	}
	// Expired yesterday → days_remaining = -1, rendered in the body.
	if !containsAll(body, "STR/WRK/EXP", "-1 hari") {
		t.Fatalf("expired email must carry the license and negative days, got: %s", body)
	}
}

// testCredentialWorkerCfg is the direct-call config (no ticker): only the warn
// window matters to processCredentialChecks.
func testCredentialWorkerCfg() config.Config {
	return config.Config{CredentialWarnDays: 30}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
