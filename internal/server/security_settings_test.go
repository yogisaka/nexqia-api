//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
		ORDER BY changed_at, id`, merchantID)
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
