//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// securityLogRow is one core.security_setting_log row ("" = NULL).
type securityLogRow struct {
	FlagKey         string
	OldValue        string
	NewValue        string
	ChangedBy       string
	PlatformAdminID string
	CompanyID       string
}

// securityLogQuerier is satisfied by *pgxpool.Pool (owner role, bypasses RLS)
// and pgx.Tx (used under SET LOCAL ROLE app_runtime).
type securityLogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// securityLogRows reads every security_setting_log row for one merchant.
func securityLogRows(t *testing.T, q securityLogQuerier, merchantID string) []securityLogRow {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		SELECT flag_key, COALESCE(old_value::text, ''), COALESCE(new_value::text, ''),
		       COALESCE(changed_by::text, ''), COALESCE(platform_admin_id::text, ''),
		       company_id::text
		FROM core.security_setting_log
		WHERE merchant_id = $1
		ORDER BY changed_at DESC, id DESC`, merchantID)
	if err != nil {
		t.Fatalf("query security_setting_log: %v", err)
	}
	defer rows.Close()
	out := []securityLogRow{}
	for rows.Next() {
		var r securityLogRow
		if err := rows.Scan(&r.FlagKey, &r.OldValue, &r.NewValue, &r.ChangedBy, &r.PlatformAdminID, &r.CompanyID); err != nil {
			t.Fatalf("scan security_setting_log row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate security_setting_log rows: %v", err)
	}
	return out
}

// setActorGUC attributes flag changes made inside tx to actorID ("" = skip, so
// the trigger records changed_by NULL).
func setActorGUC(t *testing.T, ctx context.Context, tx pgx.Tx, actorID string) {
	t.Helper()
	if actorID == "" {
		return
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", actorID); err != nil {
		t.Fatalf("set user GUC: %v", err)
	}
}

// upsertFlag performs the same INSERT ... ON CONFLICT upsert the API issues for
// a core.feature_flag row, inside a tx that carries the actor GUC.
func upsertFlag(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID, flagKey, value, actorID string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	setActorGUC(t, ctx, tx, actorID)
	if _, err := tx.Exec(ctx, `INSERT INTO core.feature_flag (merchant_id, flag_key, flag_value, created_by, updated_by)
		VALUES ($1, $2, $3::jsonb, NULLIF($4, '')::uuid, NULLIF($4, '')::uuid)
		ON CONFLICT (merchant_id, flag_key) DO UPDATE
		    SET flag_value = EXCLUDED.flag_value, updated_by = EXCLUDED.updated_by`,
		merchantID, flagKey, value, actorID); err != nil {
		t.Fatalf("upsert flag %s: %v", flagKey, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit flag upsert: %v", err)
	}
}

// deleteFlag removes a core.feature_flag row as actorID.
func deleteFlag(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID, flagKey, actorID string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	setActorGUC(t, ctx, tx, actorID)
	if _, err := tx.Exec(ctx, "DELETE FROM core.feature_flag WHERE merchant_id = $1 AND flag_key = $2", merchantID, flagKey); err != nil {
		t.Fatalf("delete flag %s: %v", flagKey, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit flag delete: %v", err)
	}
}

// TestSecuritySettingLog_RecordsInsertUpdateDelete — every auth.* flag change is
// logged: insert, update and delete each produce exactly one row with the
// correct old/new jsonb values and the merchant's company_id.
func TestSecuritySettingLog_RecordsInsertUpdateDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "seclog.insupd", "081234580001")

	rows := securityLogRows(t, pool, merchantID)
	if len(rows) != 1 {
		t.Fatalf("seed merchant must produce exactly 1 log row (pin_lock insert), got %d: %+v", len(rows), rows)
	}
	if rows[0].FlagKey != "auth.pin_lock" || rows[0].OldValue != "" || rows[0].NewValue == "" {
		t.Fatalf("seed log row must be a pin_lock insert, got %+v", rows[0])
	}

	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "true", userID)
	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "false", userID)
	deleteFlag(t, ctx, pool, merchantID, "auth.require_totp", userID)

	rows = securityLogRows(t, pool, merchantID)
	if len(rows) != 4 {
		t.Fatalf("expected 4 log rows (pin_lock insert + require_totp insert/update/delete), got %d: %+v", len(rows), rows)
	}

	inserts, updates, deletes := 0, 0, 0
	for _, r := range rows {
		if r.FlagKey != "auth.require_totp" {
			continue
		}
		if r.CompanyID != companyID {
			t.Fatalf("log row company_id must be the merchant company %s, got %s", companyID, r.CompanyID)
		}
		if r.ChangedBy != userID {
			t.Fatalf("log row changed_by must be %s, got %q", userID, r.ChangedBy)
		}
		switch {
		case r.OldValue == "" && r.NewValue == "true":
			inserts++
		case r.OldValue == "true" && r.NewValue == "false":
			updates++
		case r.OldValue == "false" && r.NewValue == "":
			deletes++
		}
	}
	if inserts != 1 || updates != 1 || deletes != 1 {
		t.Fatalf("expected exactly one insert/update/delete row for auth.require_totp, got %d/%d/%d: %+v", inserts, updates, deletes, rows)
	}
}

// TestSecuritySettingLog_SkipsNonAuthAndNoOp — the trigger logs only auth.*
// flags, and a save that changes nothing adds no row (plan review focus 3).
func TestSecuritySettingLog_SkipsNonAuthAndNoOp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	_, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "seclog.skip", "081234580011")

	base := len(securityLogRows(t, pool, merchantID))

	upsertFlag(t, ctx, pool, merchantID, "clinical_form.x", `{"a":1}`, userID)
	if got := len(securityLogRows(t, pool, merchantID)); got != base {
		t.Fatalf("non-auth flag change must not be logged, base %d got %d", base, got)
	}

	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "true", userID)
	if got := len(securityLogRows(t, pool, merchantID)); got != base+1 {
		t.Fatalf("auth flag insert must be logged, base %d got %d", base, got)
	}

	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "true", userID)
	if got := len(securityLogRows(t, pool, merchantID)); got != base+1 {
		t.Fatalf("no-op save must not add a log row, base %d got %d", base+1, got)
	}
}

// TestSecuritySettingLog_DirectSQLHasNullActor — an operator changing the flag
// straight through SQL (no session GUC) is still logged, with changed_by NULL
// (plan review focus 4).
func TestSecuritySettingLog_DirectSQLHasNullActor(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	_, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "seclog.direct", "081234580021")

	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "true", userID)

	// Direct SQL through the pool, no transaction GUC → NULL actor.
	if _, err := pool.Exec(ctx, `INSERT INTO core.feature_flag (merchant_id, flag_key, flag_value)
		VALUES ($1, 'auth.require_totp', 'false')
		ON CONFLICT (merchant_id, flag_key) DO UPDATE SET flag_value = EXCLUDED.flag_value`, merchantID); err != nil {
		t.Fatalf("direct flag update: %v", err)
	}

	found := false
	for _, r := range securityLogRows(t, pool, merchantID) {
		if r.FlagKey == "auth.require_totp" && r.NewValue == "false" {
			found = true
			if r.ChangedBy != "" || r.PlatformAdminID != "" {
				t.Fatalf("direct SQL change must have a NULL actor, got changed_by=%q platform_admin_id=%q", r.ChangedBy, r.PlatformAdminID)
			}
		}
	}
	if !found {
		t.Fatalf("expected a log row for the direct SQL update")
	}
}

// assertLogWriteDenied runs one statement as app_runtime inside a fresh tx and
// requires a permission-denied error (a failed statement aborts its tx, so each
// denial gets its own transaction).
func assertLogWriteDenied(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, sql string, args ...any) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	for _, g := range [][2]string{{"app.current_company_id", companyID}, {"app.current_merchant_id", merchantID}} {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", g[0], g[1]); err != nil {
			t.Fatalf("set %s GUC: %v", g[0], err)
		}
	}
	if _, err := tx.Exec(ctx, sql, args...); err == nil {
		t.Fatalf("write to core.security_setting_log as app_runtime must be denied, got no error for: %s", sql)
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied, got: %v", err)
	}
}

// TestSecuritySettingLog_AppRuntimeReadOnly — app_runtime may read the log (RLS
// filtered) but never write it; flag changes it makes on core.feature_flag are
// still logged by the SECURITY DEFINER trigger.
func TestSecuritySettingLog_AppRuntimeReadOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "seclog.role", "081234580031")

	upsertFlag(t, ctx, pool, merchantID, "auth.require_totp", "true", userID)

	assertLogWriteDenied(t, ctx, pool, companyID, merchantID,
		"INSERT INTO core.security_setting_log (company_id, merchant_id, flag_key, new_value) VALUES ($1, $2, 'auth.require_totp', 'true')",
		companyID, merchantID)
	assertLogWriteDenied(t, ctx, pool, companyID, merchantID,
		"UPDATE core.security_setting_log SET flag_key = flag_key")
	assertLogWriteDenied(t, ctx, pool, companyID, merchantID,
		"DELETE FROM core.security_setting_log")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	for _, g := range [][2]string{{"app.current_company_id", companyID}, {"app.current_merchant_id", merchantID}, {"app.current_user_id", userID}} {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", g[0], g[1]); err != nil {
			t.Fatalf("set %s GUC: %v", g[0], err)
		}
	}

	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.security_setting_log").Scan(&visible); err != nil {
		t.Fatalf("select log as app_runtime: %v", err)
	}
	if visible != 2 {
		t.Fatalf("expected 2 visible log rows (pin_lock + require_totp inserts), got %d", visible)
	}

	if _, err := tx.Exec(ctx, "UPDATE core.feature_flag SET flag_value = 'false' WHERE merchant_id = $1 AND flag_key = 'auth.require_totp'", merchantID); err != nil {
		t.Fatalf("update feature_flag as app_runtime: %v", err)
	}
	var after int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.security_setting_log").Scan(&after); err != nil {
		t.Fatalf("count log after app_runtime flag update: %v", err)
	}
	if after != 3 {
		t.Fatalf("app_runtime flag update must be logged by the DEFINER trigger, expected 3 rows, got %d", after)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
}

// TestSecuritySettingLog_RLSPerMerchant — as app_runtime bound to merchant A,
// rows of merchant B stay invisible even with an explicit WHERE (RLS policy).
func TestSecuritySettingLog_RLSPerMerchant(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "seclog.rls", "081234580041")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Klinik Kedua Seclog")

	// Owner pool bypasses RLS, no GUC → the require_totp insert is logged with
	// a NULL actor for merchant B.
	upsertFlag(t, ctx, pool, merchantB, "auth.require_totp", "true", "")
	if got := len(securityLogRows(t, pool, merchantB)); got != 2 {
		t.Fatalf("merchant B must have 2 log rows (pin_lock + require_totp inserts), got %d", got)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	for _, g := range [][2]string{{"app.current_company_id", companyID}, {"app.current_merchant_id", merchantA}} {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", g[0], g[1]); err != nil {
			t.Fatalf("set %s GUC: %v", g[0], err)
		}
	}

	var bVisible, aVisible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.security_setting_log WHERE merchant_id = $1", merchantB).Scan(&bVisible); err != nil {
		t.Fatalf("count merchant B rows as app_runtime: %v", err)
	}
	if bVisible != 0 {
		t.Fatalf("merchant B rows must be invisible under merchant A RLS, got %d", bVisible)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.security_setting_log WHERE merchant_id = $1", merchantA).Scan(&aVisible); err != nil {
		t.Fatalf("count merchant A rows as app_runtime: %v", err)
	}
	if aVisible != 1 {
		t.Fatalf("merchant A pin_lock insert must stay visible under merchant A RLS, got %d", aVisible)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
}

// seedMerchantStaff inserts a staff app_user at merchantID (PIN hashed from pin,
// or none when empty) attached to a fresh role granted exactly perms — mirrors
// the restricted-user seeding in access_log_test.go/audit_test.go.
func seedMerchantStaff(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, username, pin string, perms ...string) string {
	t.Helper()
	userID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e6e"
	roleID := "6f6f6f6f-6f6f-6f6f-6f6f-6f6f6f6f6f6f"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, companyID, username); err != nil {
		t.Fatalf("seed staff role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash staff password: %v", err)
	}
	var pinHash *string
	if pin != "" {
		h, err := testHasher().Hash(ctx, pin)
		if err != nil {
			t.Fatalf("hash staff pin: %v", err)
		}
		pinHash = &h
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, pin_hash, is_active) VALUES ($1, $2, $3, $4, $5, true)", userID, companyID, username, passwordHash, pinHash); err != nil {
		t.Fatalf("seed staff user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed staff user_merchant_role: %v", err)
	}
	for _, code := range perms {
		if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, code); err != nil {
			t.Fatalf("grant %s to staff role: %v", code, err)
		}
	}
	return userID
}

// rawFlagValue reads a core.feature_flag flag_value (jsonb) straight from the
// DB as text — exactly what the enforcement code will read back.
func rawFlagValue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, merchantID, flagKey string) string {
	t.Helper()
	var raw *string
	if err := pool.QueryRow(ctx, "SELECT flag_value::text FROM core.feature_flag WHERE merchant_id = $1 AND flag_key = $2", merchantID, flagKey).Scan(&raw); err != nil {
		t.Fatalf("read flag %s: %v", flagKey, err)
	}
	if raw == nil {
		return ""
	}
	return *raw
}

// securitySettingsDataResponse mirrors the GET/PUT /settings/security data body.
type securitySettingsDataResponse struct {
	PinLock struct {
		Enabled     bool `json:"enabled"`
		IdleMinutes int  `json:"idle_minutes"`
	} `json:"pin_lock"`
	Limits struct {
		MinIdleMinutes int `json:"min_idle_minutes"`
		MaxIdleMinutes int `json:"max_idle_minutes"`
	} `json:"limits"`
	RequireTOTP     bool  `json:"require_totp"`
	UsersTotal      int64 `json:"users_total"`
	UsersWithoutPin int64 `json:"users_without_pin"`
	UsersWithoutMfa int64 `json:"users_without_mfa"`
	CanEdit         bool  `json:"can_edit"`
}

// TestSecuritySettings_GetShapeAndStats — the GET shape: owner gets can_edit
// true, the merchant's default-enabled pin_lock with the config fallback idle
// window, the configured limits, and stats counting owner + a PIN-less/MFA-less
// staff member.
func TestSecuritySettings_GetShapeAndStats(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "secset.get", "081234560001")
	seedMerchantStaff(t, ctx, pool, companyID, merchantID, "secset.get.staff", "")

	rec := accountRequest(router, http.MethodGet, "/api/v1/settings/security", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/security expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data securitySettingsDataResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Data.CanEdit {
		t.Errorf("can_edit = false, want true for the owner")
	}
	if !resp.Data.PinLock.Enabled {
		t.Errorf("pin_lock.enabled = false, want true (new merchants get PIN lock by default)")
	}
	if resp.Data.PinLock.IdleMinutes != testConfig().AppLockDefaultIdleMinutes {
		t.Errorf("pin_lock.idle_minutes = %d, want config default %d", resp.Data.PinLock.IdleMinutes, testConfig().AppLockDefaultIdleMinutes)
	}
	if resp.Data.Limits.MinIdleMinutes != 1 || resp.Data.Limits.MaxIdleMinutes != 30 {
		t.Errorf("limits = {%d,%d}, want {1,30}", resp.Data.Limits.MinIdleMinutes, resp.Data.Limits.MaxIdleMinutes)
	}
	if resp.Data.UsersTotal != 2 || resp.Data.UsersWithoutPin != 2 || resp.Data.UsersWithoutMfa != 2 {
		t.Errorf("stats = total %d / no-pin %d / no-mfa %d, want 2/2/2 (owner + PIN-less staff)",
			resp.Data.UsersTotal, resp.Data.UsersWithoutPin, resp.Data.UsersWithoutMfa)
	}
}

// TestSecuritySettings_PutSavesFlagsReadByEnforcement — a PUT persists both
// flags exactly as the enforcement code (getPinLockFlag/checkMFANudge) reads
// them back, the GET reflects the new values, and the history gains one row per
// changed flag.
func TestSecuritySettings_PutSavesFlagsReadByEnforcement(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "secset.put", "081234560011")

	body := []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":true}`)
	rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /settings/security expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Raw flag_value as the DB stores it — what getPinLockFlag parses.
	var pinFlag map[string]any
	if err := json.Unmarshal([]byte(rawFlagValue(t, ctx, pool, merchantID, "auth.pin_lock")), &pinFlag); err != nil {
		t.Fatalf("decode stored auth.pin_lock %q: %v", rawFlagValue(t, ctx, pool, merchantID, "auth.pin_lock"), err)
	}
	if pinFlag["enabled"] != false || pinFlag["idle_minutes"] != float64(10) {
		t.Fatalf("stored auth.pin_lock = %+v, want enabled false idle_minutes 10", pinFlag)
	}
	var totp bool
	if err := json.Unmarshal([]byte(rawFlagValue(t, ctx, pool, merchantID, "auth.require_totp")), &totp); err != nil || !totp {
		t.Fatalf("stored auth.require_totp = %q (err %v), want true", rawFlagValue(t, ctx, pool, merchantID, "auth.require_totp"), err)
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/settings/security", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data securitySettingsDataResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if resp.Data.PinLock.Enabled || resp.Data.PinLock.IdleMinutes != 10 || !resp.Data.RequireTOTP {
		t.Fatalf("GET must reflect the saved flags, got %+v", resp.Data)
	}

	rows := securityLogRows(t, pool, merchantID)
	if len(rows) != 3 {
		t.Fatalf("expected 3 history rows (seed pin_lock insert + pin_lock update + require_totp insert), got %d: %+v", len(rows), rows)
	}
	// Newest first: require_totp insert, then the pin_lock update.
	if rows[0].FlagKey != "auth.require_totp" || rows[0].OldValue != "" || rows[0].NewValue != "true" || rows[0].ChangedBy != userID {
		t.Fatalf("history row 1 must be the require_totp insert by the owner, got %+v", rows[0])
	}
	if rows[1].FlagKey != "auth.pin_lock" || !strings.Contains(rows[1].OldValue, `"enabled": true`) || !strings.Contains(rows[1].NewValue, `"idle_minutes": 10`) {
		t.Fatalf("history row 2 must be the pin_lock update, got %+v", rows[1])
	}
}

// TestSecuritySettings_PutValidation — malformed bodies and out-of-range idle
// values are rejected with 400 before anything is saved; a wrong password is
// 401. Bind failures run before the rate limiter, so only the three bodies that
// reach it consume attempts (max 5 in testConfig).
func TestSecuritySettings_PutValidation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "secset.valid", "081234560021")

	cases := []struct {
		name    string
		body    string
		want    int
		wantErr string
	}{
		{"wrong password", `{"password":"wrong-pass","pin_lock":{"enabled":true,"idle_minutes":10},"require_totp":false}`, http.StatusUnauthorized, "invalid password"},
		{"missing pin_lock", `{"password":"Passw0rd123!","require_totp":true}`, http.StatusBadRequest, ""},
		{"idle as string", `{"password":"Passw0rd123!","pin_lock":{"enabled":true,"idle_minutes":"5"},"require_totp":true}`, http.StatusBadRequest, ""},
		{"idle zero", `{"password":"Passw0rd123!","pin_lock":{"enabled":true,"idle_minutes":0},"require_totp":true}`, http.StatusBadRequest, "idle_minutes must be between 1 and 30"},
		{"idle over max", `{"password":"Passw0rd123!","pin_lock":{"enabled":true,"idle_minutes":31},"require_totp":true}`, http.StatusBadRequest, "idle_minutes must be between 1 and 30"},
		{"missing require_totp", `{"password":"Passw0rd123!","pin_lock":{"enabled":true,"idle_minutes":10}}`, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", token, companyID, merchantID, []byte(tc.body))
		if rec.Code != tc.want {
			t.Errorf("%s: expected %d, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
			continue
		}
		if tc.wantErr != "" {
			var resp struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !strings.Contains(resp.Error, tc.wantErr) {
				t.Errorf("%s: expected error containing %q, got %q (err %v)", tc.name, tc.wantErr, resp.Error, err)
			}
		}
	}
}

// TestSecuritySettings_PutRateLimited — saving is rate-limited per user like
// password changes: with the limit at 2, the third PUT within the window is 429.
func TestSecuritySettings_PutRateLimited(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.RateLimitLoginMaxAttempts = 2
	router := server.NewRouter(pool, redisClient, cfg)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "secset.ratelim", "081234560031")

	body := []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":true}`)
	for i := 1; i <= 2; i++ {
		rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", token, companyID, merchantID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %d expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", token, companyID, merchantID, body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third PUT expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != "too many attempts, try again later" {
		t.Fatalf("expected 429 error message, got %q (err %v)", resp.Error, err)
	}
}

// TestSecuritySettings_PutRejectedDuringImpersonation — an impersonation session
// cannot save security settings, and the merchant's flags stay untouched
// (rejectDuringImpersonation runs before anything else).
func TestSecuritySettings_PutRejectedDuringImpersonation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "secset.imp", "081234560041")
	impToken := impersonationToken(t, userID, companyID, merchantID)

	body := []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":true}`)
	rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", impToken, companyID, merchantID, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT during impersonation expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != "not allowed during impersonation" {
		t.Fatalf("expected \"not allowed during impersonation\", got %q (err %v)", resp.Error, err)
	}
	var pinFlag map[string]any
	if err := json.Unmarshal([]byte(rawFlagValue(t, ctx, pool, merchantID, "auth.pin_lock")), &pinFlag); err != nil || pinFlag["enabled"] != true {
		t.Fatalf("auth.pin_lock must be unchanged, got %q (err %v)", rawFlagValue(t, ctx, pool, merchantID, "auth.pin_lock"), err)
	}
}

// TestSecuritySettings_AuditViewerReadOnly — a role holding only audit.log.view
// opens the page and its history (can_edit false, plan review focus 5) but PUT
// stays 403; a user with neither permission gets 403 on GET.
func TestSecuritySettings_AuditViewerReadOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "secset.view", "081234560051")
	viewerID := seedMerchantStaff(t, ctx, pool, companyID, merchantID, "secset.viewer", "", "audit.log.view")
	viewerToken, err := auth.GenerateToken(testJWTSecret, viewerID, companyID, merchantID, "", "secset.viewer", "secset-viewer-device", time.Hour)
	if err != nil {
		t.Fatalf("generate viewer token: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/settings/security", viewerToken, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET /settings/security expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data securitySettingsDataResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode viewer GET response: %v", err)
	}
	if resp.Data.CanEdit {
		t.Fatalf("viewer can_edit = true, want false (audit.log.view only)")
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/settings/security/history", viewerToken, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET history expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := []byte(`{"password":"correct-horse","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":true}`)
	rec = accountRequest(router, http.MethodPut, "/api/v1/settings/security", viewerToken, companyID, merchantID, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Neither permission → the page itself is 403.
	nobodyID := seedRestrictedAuditUser(t, ctx, pool, companyID, merchantID, "secset.noview")
	nobodyToken, err := auth.GenerateToken(testJWTSecret, nobodyID, companyID, merchantID, "", "secset.noview", "secset-noview-device", time.Hour)
	if err != nil {
		t.Fatalf("generate nobody token: %v", err)
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/settings/security", nobodyToken, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET without any of the two permissions expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSecuritySettings_EnablingPinLockKeepsSaverUnlocked — plan review focus 1:
// the admin who saves enabled:true keeps working on their own device (the PUT
// seeds their applock key, so the response carries pin_lock_idle_minutes and
// their next request is 200), while another PIN-enrolled user locks (423).
func TestSecuritySettings_EnablingPinLockKeepsSaverUnlocked(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, ownerToken, _ := seedAccountOwner(t, ctx, pool, router, "secset.lock", "081234560061")

	// New merchants get pin_lock enabled by default — turn it off first so
	// nobody is locked while the scenario is set up.
	body := []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":false}`)
	rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", ownerToken, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT disabling pin_lock expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Owner enrolls a PIN while the flag is off (no applock key is seeded).
	rec = accountRequest(router, http.MethodPost, "/api/v1/auth/pin/set", ownerToken, companyID, merchantID, []byte(`{"password":"Passw0rd123!","pin":"123456"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner /auth/pin/set expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	staffID := seedMerchantStaff(t, ctx, pool, companyID, merchantID, "secset.lock.staff", "654321")
	staffToken, err := auth.GenerateToken(testJWTSecret, staffID, companyID, merchantID, "", "secset.lock.staff", "secset-lock-staff-device", time.Hour)
	if err != nil {
		t.Fatalf("generate staff token: %v", err)
	}

	if rec = accountRequest(router, http.MethodGet, "/api/v1/auth/me", ownerToken, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner /auth/me while disabled expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec = accountRequest(router, http.MethodGet, "/api/v1/auth/me", staffToken, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("staff /auth/me while disabled expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Owner re-enables the lock — their device must not lock mid-session.
	body = []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":true,"idle_minutes":10},"require_totp":false}`)
	rec = accountRequest(router, http.MethodPut, "/api/v1/settings/security", ownerToken, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT enabling pin_lock expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var putResp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if got, ok := putResp.Data["pin_lock_idle_minutes"]; !ok || got != float64(10) {
		t.Fatalf("PUT response must carry pin_lock_idle_minutes 10, got %+v (present %t)", got, ok)
	}

	if rec = accountRequest(router, http.MethodGet, "/api/v1/auth/me", ownerToken, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner /auth/me after enabling expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/auth/me", staffToken, companyID, merchantID, nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("staff /auth/me after enabling expected 423, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSecuritySettings_PutOmitsIdleWhenDisabled — the pin_lock_idle_minutes key
// exists in the PUT response only when the lock is being enabled (and the saver
// has a PIN): disabling omits it even for a PIN-enrolled owner.
func TestSecuritySettings_PutOmitsIdleWhenDisabled(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "secset.omit", "081234560071")

	// Enroll a PIN first (merchant default flag is on, so /auth/pin/set seeds
	// the applock key) — the omission below is then driven by enabled:false
	// alone, not by the saver lacking a PIN.
	if rec := accountRequest(router, http.MethodPost, "/api/v1/auth/pin/set", token, companyID, merchantID, []byte(`{"password":"Passw0rd123!","pin":"123456"}`)); rec.Code != http.StatusOK {
		t.Fatalf("owner /auth/pin/set expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := []byte(`{"password":"Passw0rd123!","pin_lock":{"enabled":false,"idle_minutes":10},"require_totp":false}`)
	rec := accountRequest(router, http.MethodPut, "/api/v1/settings/security", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if _, exists := resp.Data["pin_lock_idle_minutes"]; exists {
		t.Fatalf("disabled pin_lock must not carry pin_lock_idle_minutes, got %+v", resp.Data)
	}
}
