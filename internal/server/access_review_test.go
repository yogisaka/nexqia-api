//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// insertAccessReview inserts one core.access_review row through the owner pool
// (bypasses RLS). Empty reviewedBy falls back to ownerID.
func insertAccessReview(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, ownerID, notes string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, now() - interval '30 days', now(), $2, 0, $3)`,
		companyID, ownerID, notes); err != nil {
		t.Fatalf("insert access_review: %v", err)
	}
}

// TestAccessReview_AppRuntimeCannotUpdateOrDelete — as app_runtime an INSERT is
// allowed (append-only ledger) but UPDATE and DELETE are permission denied.
func TestAccessReview_AppRuntimeCannotUpdateOrDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.append", "081234590101")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 1, 'quarterly review ok')`,
		companyID, time.Now().Add(-30*24*time.Hour), time.Now(), userID); err != nil {
		t.Fatalf("insert as app_runtime must succeed: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}

	// Denials each get their own tx: a failed statement aborts it.
	for _, stmt := range []string{
		"UPDATE core.access_review SET notes = 'tampered'",
		"DELETE FROM core.access_review",
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			t.Fatalf("set local role: %v", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
			t.Fatalf("set company GUC: %v", err)
		}
		if _, err := tx.Exec(ctx, stmt); err == nil {
			t.Fatalf("%s as app_runtime must be denied, got no error", stmt)
		} else if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("expected permission denied for %s, got: %v", stmt, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback tx: %v", err)
		}
	}
}

// TestAccessReview_RLSIsolatesCompanies — as app_runtime bound to company A,
// rows of company B stay invisible even with an explicit WHERE (RLS policy).
func TestAccessReview_RLSIsolatesCompanies(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, _, userA, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.rlsa", "081234590111")
	companyB, _, userB, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.rlsb", "081234590112")

	insertAccessReview(t, ctx, pool, companyA, userA, "company A review")
	insertAccessReview(t, ctx, pool, companyB, userB, "company B review")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyA); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}

	var bVisible, aVisible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.access_review WHERE company_id = $1", companyB).Scan(&bVisible); err != nil {
		t.Fatalf("count company B rows as app_runtime: %v", err)
	}
	if bVisible != 0 {
		t.Fatalf("company B rows must be invisible under company A RLS, got %d", bVisible)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.access_review WHERE company_id = $1", companyA).Scan(&aVisible); err != nil {
		t.Fatalf("count company A rows as app_runtime: %v", err)
	}
	if aVisible != 1 {
		t.Fatalf("company A row must stay visible under company A RLS, got %d", aVisible)
	}
}

// TestAccessReview_ChecksRejectBadRows — the table CHECKs reject an inverted
// review period and an empty notes field.
func TestAccessReview_ChecksRejectBadRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.checks", "081234590121")

	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 0, 'inverted period')`,
		companyID, time.Now(), time.Now().Add(-24*time.Hour), userID); err == nil {
		t.Fatalf("period_to <= period_from must be rejected")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 0, '')`,
		companyID, time.Now().Add(-24*time.Hour), time.Now(), userID); err == nil {
		t.Fatalf("empty notes must be rejected")
	}
}

// reviewUID builds a valid pgtype.UUID distinguishable by its last byte.
func reviewUID(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{0xaa, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, b}, Valid: true}
}

// flagsContains reports whether the flag list holds want.
func flagsContains(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// TestAccessReview_ComputeFlags — pure flag/median/sort logic (spec §6.2):
// each flag fires exactly at its threshold and stays silent one below it, the
// median is the mean of the two middle views for an even set, volume never
// fires for a single user, an empty input yields median 0 without panicking,
// and rows sort by flag count desc, then views desc, then actor_id asc.
func TestAccessReview_ComputeFlags(t *testing.T) {
	cfg := testConfig()

	cases := []struct {
		name       string
		rows       []sqlcgen.AccessReviewCountsRow
		wantFlags  map[byte][]string
		wantMedian float64
	}{
		{
			name: "after_hours boundary",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), AfterHours: 1},
				{ActorID: reviewUID(2), AfterHours: 0},
			},
			wantFlags:  map[byte][]string{1: {"after_hours"}, 2: {}},
			wantMedian: 0,
		},
		{
			name: "denied at threshold and one below",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), Denied: 5},
				{ActorID: reviewUID(2), Denied: 4},
			},
			wantFlags:  map[byte][]string{1: {"denied"}, 2: {}},
			wantMedian: 0,
		},
		{
			name: "self_family boundary",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), SelfFamily: 1},
				{ActorID: reviewUID(2), SelfFamily: 0},
			},
			wantFlags:  map[byte][]string{1: {"self_family"}, 2: {}},
			wantMedian: 0,
		},
		{
			name: "platform_admin boundary",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), PlatformAdmin: 1},
				{ActorID: reviewUID(2), PlatformAdmin: 0},
			},
			wantFlags:  map[byte][]string{1: {"platform_admin"}, 2: {}},
			wantMedian: 0,
		},
		{
			name: "volume odd median fires only above multiplier x median",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), Views: 10},
				{ActorID: reviewUID(2), Views: 40},
				{ActorID: reviewUID(3), Views: 121}, // median 40, 3*40 = 120 < 121
			},
			wantFlags:  map[byte][]string{1: {}, 2: {}, 3: {"volume"}},
			wantMedian: 40,
		},
		{
			name: "volume even median no flag",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), Views: 10},
				{ActorID: reviewUID(2), Views: 40}, // median 25, 3*25 = 75 > 40
			},
			wantFlags:  map[byte][]string{1: {}, 2: {}},
			wantMedian: 25,
		},
		{
			name: "volume never fires for a single user",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), Views: 1000}, // median = own views
			},
			wantFlags:  map[byte][]string{1: {}},
			wantMedian: 1000,
		},
		{
			name:       "empty input",
			rows:       nil,
			wantFlags:  map[byte][]string{},
			wantMedian: 0,
		},
		{
			name: "flag order within a row",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(1), Views: 3001, Denied: 5, AfterHours: 1, SelfFamily: 1, PlatformAdmin: 1},
				{ActorID: reviewUID(2), Views: 1},
				{ActorID: reviewUID(3), Views: 1},
			},
			wantFlags: map[byte][]string{
				1: {"after_hours", "denied", "self_family", "volume", "platform_admin"},
				2: {},
				3: {},
			},
			wantMedian: 1,
		},
		{
			name: "ordering flags desc, views desc, actor asc",
			rows: []sqlcgen.AccessReviewCountsRow{
				{ActorID: reviewUID(4), Views: 5, SelfFamily: 1},
				{ActorID: reviewUID(2), Views: 20, SelfFamily: 1},
				{ActorID: reviewUID(3), Views: 5, AfterHours: 1, Denied: 5},
				{ActorID: reviewUID(1), Views: 20, SelfFamily: 1},
			},
			wantFlags: map[byte][]string{
				1: {"self_family"},
				2: {"self_family"},
				3: {"after_hours", "denied"},
				4: {"self_family"},
			},
			wantMedian: 12.5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, median := server.ComputeReviewFlags(tc.rows, cfg)
			if median != tc.wantMedian {
				t.Fatalf("median = %v, want %v", median, tc.wantMedian)
			}
			if len(out) != len(tc.rows) {
				t.Fatalf("got %d rows, want %d", len(out), len(tc.rows))
			}
			for _, r := range out {
				b := r.ActorID.Bytes[15]
				want := tc.wantFlags[b]
				if len(r.Flags) != len(want) {
					t.Fatalf("actor %d flags = %v, want %v", b, r.Flags, want)
				}
				for i := range want {
					if r.Flags[i] != want[i] {
						t.Fatalf("actor %d flags = %v, want %v", b, r.Flags, want)
					}
				}
			}
			// Explicit expected order for the ordering case.
			if tc.name == "ordering flags desc, views desc, actor asc" {
				wantOrder := []byte{3, 1, 2, 4}
				for i, r := range out {
					if r.ActorID.Bytes[15] != wantOrder[i] {
						t.Fatalf("row %d = actor %d, want %d", i, r.ActorID.Bytes[15], wantOrder[i])
					}
				}
			}
		})
	}
}

// insertAccessLogRow inserts one core.access_log row through the owner pool
// (bypasses RLS); personID "" leaves person_id NULL.
func insertAccessLogRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, actorID, resource, action string, status int16, createdAt time.Time, personID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO core.access_log
		(company_id, merchant_id, actor_id, resource, action, status_code, created_at, person_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid)`,
		companyID, merchantID, actorID, resource, action, status, createdAt, personID); err != nil {
		t.Fatalf("insert access_log row: %v", err)
	}
}

// seedBareAuditUser inserts an app_user directly (no role/permission) so its
// access_log rows are the only noise in the review summary.
func seedBareAuditUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, username, id string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO core.app_user (id, company_id, username, password_hash, is_active)
		VALUES ($1::uuid, $2::uuid, $3, 'not-a-real-hash', true)`, id, companyID, username); err != nil {
		t.Fatalf("seed bare user: %v", err)
	}
}

type summaryResponse struct {
	Data []map[string]any `json:"data"`
	Meta map[string]any   `json:"meta"`
}

// summaryRequest calls GET /audit/review/summary with the standard headers and
// decodes the {"data": [...], "meta": {...}} envelope.
func summaryRequest(t *testing.T, router *gin.Engine, token, companyID, merchantID, query string) summaryResponse {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/review/summary"+query, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("review summary expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode review summary: %v", err)
	}
	return resp
}

// summaryRowByID returns the row whose actor_id equals want.
func summaryRowByID(t *testing.T, resp summaryResponse, want string) map[string]any {
	t.Helper()
	for _, row := range resp.Data {
		if row["actor_id"] == want {
			return row
		}
	}
	t.Fatalf("actor %s missing from summary response", want)
	return nil
}

// TestAccessReview_SummaryAfterHoursBoundary — access_log rows at 06:59 and
// 21:00 WIB count as after-hours, 07:00 and 20:59 do not (spec §6.2).
func TestAccessReview_SummaryAfterHoursBoundary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.hour", "081234590131")
	actorID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e01"
	seedBareAuditUser(t, ctx, pool, companyID, "accrev.night", actorID)

	// Local WIB hours: 06:59 = 23:59 UTC (previous day), 07:00 = 00:00 UTC,
	// 20:59 = 13:59 UTC, 21:00 = 14:00 UTC — all for the same WIB day.
	utcMidnight := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), time.Now().UTC().Day(), 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Duration{-time.Minute, 0, 13*time.Hour + 59*time.Minute, 14 * time.Hour} {
		insertAccessLogRow(t, ctx, pool, companyID, merchantID, actorID, "person", "view", 200, utcMidnight.Add(at), "")
	}

	from := url.QueryEscape(utcMidnight.Add(-2 * time.Hour).Format(time.RFC3339))
	to := url.QueryEscape(utcMidnight.Add(15 * time.Hour).Format(time.RFC3339))
	resp := summaryRequest(t, router, token, companyID, merchantID, "?from="+from+"&to="+to)
	row := summaryRowByID(t, resp, actorID)
	if row["after_hours"] != float64(2) {
		t.Fatalf("after_hours = %v, want 2", row["after_hours"])
	}
	if !flagsContains(anyToStrings(row["flags"]), "after_hours") {
		t.Fatalf("flags = %v, want after_hours", row["flags"])
	}
}

// anyToStrings decodes a JSON flag array ([]any) back to []string.
func anyToStrings(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestAccessReview_SummarySelfFamily — viewing self (P1 = S), a family member
// (P2.family_id = S), the family head (P3 with S.family_id = P3) and a sibling
// member (P4.family_id = S.family_id) all count, an unrelated patient (P5)
// does not → self_family = 4 (spec §6.1 related predicate).
func TestAccessReview_SummarySelfFamily(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.fam", "081234590141")

	var selfID string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM core.app_user WHERE id = $1::uuid", userID).Scan(&selfID); err != nil {
		t.Fatalf("resolve owner person: %v", err)
	}
	insertPerson := func(name string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO core.person (company_id, full_name, gender)
			VALUES ($1::uuid, $2, 'male') RETURNING id::text`, companyID, name).Scan(&id); err != nil {
			t.Fatalf("insert person %s: %v", name, err)
		}
		return id
	}
	p2 := insertPerson("Accrev Family P2")
	p3 := insertPerson("Accrev Family P3")
	p4 := insertPerson("Accrev Family P4")
	p5 := insertPerson("Accrev Family P5")
	if _, err := pool.Exec(ctx, "UPDATE core.person SET family_id = $2::uuid WHERE id = $1::uuid", p2, selfID); err != nil {
		t.Fatalf("link P2: %v", err)
	}
	// P3 is the family head: S.family_id = P3.
	if _, err := pool.Exec(ctx, "UPDATE core.person SET family_id = $2::uuid WHERE id = $1::uuid", selfID, p3); err != nil {
		t.Fatalf("link S: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.person SET family_id = $2::uuid WHERE id = $1::uuid", p4, p3); err != nil {
		t.Fatalf("link P4: %v", err)
	}

	for _, personID := range []string{selfID, p2, p3, p4, p5} {
		insertAccessLogRow(t, ctx, pool, companyID, merchantID, userID, "person", "view", 200, time.Now().UTC(), personID)
	}

	resp := summaryRequest(t, router, token, companyID, merchantID, "")
	row := summaryRowByID(t, resp, userID)
	if row["self_family"] != float64(4) {
		t.Fatalf("self_family = %v, want 4", row["self_family"])
	}
	flags := anyToStrings(row["flags"])
	if !flagsContains(flags, "self_family") {
		t.Fatalf("flags = %v, want self_family", flags)
	}
	if flagsContains(flags, "denied") || flagsContains(flags, "volume") || flagsContains(flags, "platform_admin") {
		t.Fatalf("unexpected flags: %v", flags)
	}
}

// TestAccessReview_SummaryExcludesAuditRows — access_log rows with
// resource='audit' feed only audit_actions and never views/flags (spec §6.1).
func TestAccessReview_SummaryExcludesAuditRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.excl", "081234590151")

	if _, err := pool.Exec(ctx, `INSERT INTO core.person (company_id, full_name, gender)
		VALUES ($1::uuid, 'Accrev Excl Person', 'male') RETURNING id::text`, companyID); err != nil {
		t.Fatalf("insert person: %v", err)
	}
	var personID string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM core.person WHERE company_id = $1::uuid AND full_name = 'Accrev Excl Person'", companyID).Scan(&personID); err != nil {
		t.Fatalf("resolve person: %v", err)
	}
	insertAccessLogRow(t, ctx, pool, companyID, merchantID, userID, "person", "view", 200, time.Now().UTC(), personID)
	insertAccessLogRow(t, ctx, pool, companyID, merchantID, userID, "person", "view", 200, time.Now().UTC(), personID)
	insertAccessLogRow(t, ctx, pool, companyID, merchantID, userID, "audit", "list_access", 200, time.Now().UTC(), "")

	resp := summaryRequest(t, router, token, companyID, merchantID, "")
	row := summaryRowByID(t, resp, userID)
	if row["views"] != float64(2) {
		t.Fatalf("views = %v, want 2 (audit rows must not count)", row["views"])
	}
	// The seeded audit row; the summary call's own audit row is written by
	// the AccessLog middleware after the handler runs, so the query never
	// sees it.
	if row["audit_actions"] != float64(1) {
		t.Fatalf("audit_actions = %v, want 1", row["audit_actions"])
	}
	if row["denied"] != float64(0) || row["lists"] != float64(0) {
		t.Fatalf("unexpected counts: %v", row)
	}
}

// TestAccessReview_SummaryRequiresPermission — a user whose role lacks
// audit.log.view gets 403 on the review summary.
func TestAccessReview_SummaryRequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.view", "081234590161")
	restrictedUserID := seedRestrictedAuditUser(t, ctx, pool, companyID, merchantID, "accrev.noview")
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "accrev.noview", "accrev-noview-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/review/summary", restrictedToken, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /audit/review/summary without audit.log.view expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func postReview(router *gin.Engine, token, companyID, merchantID string, from, to time.Time, notes string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "notes": notes})
	return accountRequest(router, http.MethodPost, "/api/v1/audit/reviews", token, companyID, merchantID, body)
}

// TestAccessReview_CreateComputesFlaggedUsers — one actor with >= threshold 403s
// makes flagged_users = 1.
func TestAccessReview_CreateComputesFlaggedUsers(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.create", "081234590171")
	actorID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e11"
	seedBareAuditUser(t, ctx, pool, companyID, "accrev.denied", actorID)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ { // testConfig denied threshold = 5; 11:00 UTC = 18:00 WIB (in hours)
		insertAccessLogRow(t, ctx, pool, companyID, merchantID, actorID, "person", "view", 403, time.Date(now.Year(), now.Month(), now.Day(), 11, 0, 0, 0, time.UTC).Add(-24*time.Hour), "")
	}

	rec := postReview(router, token, companyID, merchantID, now.Add(-72*time.Hour), now, "  quarterly ok  ")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data["flagged_users"] != float64(1) {
		t.Fatalf("flagged_users = %v, want 1", resp.Data["flagged_users"])
	}
	if resp.Data["notes"] != "quarterly ok" || resp.Data["reviewed_by"] != userID {
		t.Fatalf("unexpected data: %v", resp.Data)
	}
}

// TestAccessReview_CreateValidates — bad notes / ranges are 400 and store nothing.
func TestAccessReview_CreateValidates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.valid", "081234590181")
	now := time.Now().UTC()
	cases := []struct {
		name     string
		from, to time.Time
		notes    string
		want     string
	}{
		{"whitespace notes", now.Add(-48 * time.Hour), now, "   ", "notes must be 1-2000 characters"},
		{"notes too long", now.Add(-48 * time.Hour), now, strings.Repeat("a", 2001), "notes must be 1-2000 characters"},
		{"range too wide", now.Add(-94 * 24 * time.Hour), now, "ok", "invalid time range"},
		{"to before from", now, now.Add(-48 * time.Hour), "ok", "invalid time range"},
	}
	for _, tc := range cases {
		rec := postReview(router, token, companyID, merchantID, tc.from, tc.to, tc.notes)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: expected 400 %q, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	rec := accountRequest(router, http.MethodPost, "/api/v1/audit/reviews", token, companyID, merchantID, []byte(`{"from":"x","to":"y","notes":"ok"}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid from timestamp") {
		t.Fatalf("bad from: got %d: %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.access_review WHERE company_id = $1::uuid", companyID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows stored = %d (err %v), want 0", n, err)
	}
}

// TestAccessReview_ListNewestFirst — newest review first, reviewer name filled.
func TestAccessReview_ListNewestFirst(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.list", "081234590191")
	now := time.Now().UTC()
	for _, note := range []string{"first review", "second review"} {
		if rec := postReview(router, token, companyID, merchantID, now.Add(-48*time.Hour), now, note); rec.Code != http.StatusCreated {
			t.Fatalf("create %q: %d: %s", note, rec.Code, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := accountRequest(router, http.MethodGet, "/api/v1/audit/reviews", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp summaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 || resp.Data[0]["notes"] != "second review" || resp.Data[1]["notes"] != "first review" {
		t.Fatalf("unexpected order: %v", resp.Data)
	}
	if name, _ := resp.Data[0]["reviewed_by_name"].(string); name == "" {
		t.Fatalf("reviewed_by_name empty: %v", resp.Data[0])
	}
	if resp.Meta["limit"] != float64(50) || resp.Meta["offset"] != float64(0) {
		t.Fatalf("meta = %v", resp.Meta)
	}
}

// TestAccessReview_IsLogged — POST /audit/reviews writes an audit access_log row.
func TestAccessReview_IsLogged(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "accrev.logged", "081234590201")
	now := time.Now().UTC()
	if rec := postReview(router, token, companyID, merchantID, now.Add(-48*time.Hour), now, "logged"); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d: %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.access_log
		WHERE company_id = $1::uuid AND resource = 'audit' AND action = 'review_create'`, companyID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("review_create access_log rows = %d, want 1", n)
	}
}
