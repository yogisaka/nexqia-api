//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// locationTx opens a transaction acting as app_runtime with BOTH tenant GUCs
// (company and merchant) set — runtimeTx only sets the company one, but
// core.location's RLS policy and tenant-consistency trigger need the merchant
// GUC too.
func locationTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID string) pgx.Tx {
	t.Helper()
	tx := runtimeTx(t, ctx, pool, companyID)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
		t.Fatalf("set merchant GUC: %v", err)
	}
	return tx
}

// locationInsert inserts one core.location row inside the given transaction
// (RLS and the parent/tenant triggers apply to the caller).
func locationInsert(t *testing.T, ctx context.Context, tx pgx.Tx, companyID, merchantID, parentID, kind, code, name, functions string) (string, error) {
	t.Helper()
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, $7::text[])
		RETURNING id::text`,
		companyID, merchantID, parentID, kind, code, name, functions).Scan(&id)
	return id, err
}

// locationExpectError runs one statement inside a savepoint and requires it to
// fail with the given Postgres error code; the savepoint keeps the surrounding
// transaction usable (a failed statement would otherwise abort it).
func locationExpectError(t *testing.T, ctx context.Context, tx pgx.Tx, name, wantCode, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err == nil {
		t.Fatalf("%s: expected error %s, got no error", name, wantCode)
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != wantCode {
			t.Fatalf("%s: expected %s, got %v", name, wantCode, err)
		}
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// TestLocation_DB_ParentRules — the parent-check trigger accepts a valid
// building→level→room(ward_room)→bed chain and rejects beds without a ward
// room, beds without a parent, children under beds, and cyclic parents.
func TestLocation_DB_ParentRules(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.parent", "081234570101")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	buildingID, err := locationInsert(t, ctx, tx, companyID, merchantID, "", "building", "LOC-BLD", "Gedung Utama", "{}")
	if err != nil {
		t.Fatalf("insert building: %v", err)
	}
	levelID, err := locationInsert(t, ctx, tx, companyID, merchantID, buildingID, "level", "LOC-LVL", "Lantai 2", "{}")
	if err != nil {
		t.Fatalf("insert level: %v", err)
	}
	roomID, err := locationInsert(t, ctx, tx, companyID, merchantID, levelID, "room", "LOC-RM", "Kamar Rawat 1", "{ward_room}")
	if err != nil {
		t.Fatalf("insert ward room: %v", err)
	}
	if _, err := locationInsert(t, ctx, tx, companyID, merchantID, roomID, "bed", "LOC-BED", "Tempat Tidur 1", "{}"); err != nil {
		t.Fatalf("insert bed in ward room: %v", err)
	}

	// Bed under a room without the ward_room function → 23514.
	locationExpectError(t, ctx, tx, "bed under plain room", "23514",
		"INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name) VALUES ($1, $2, $3, 'bed', 'LOC-BED2', 'TT 2')",
		companyID, merchantID, levelID)

	// Bed without a parent → 23514.
	locationExpectError(t, ctx, tx, "bed without parent", "23514",
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'bed', 'LOC-BED3', 'TT 3')",
		companyID, merchantID)

	// A child under a bed → 23514 ('a bed cannot contain other locations').
	var bedID string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM core.location WHERE code = 'LOC-BED'").Scan(&bedID); err != nil {
		t.Fatalf("find bed: %v", err)
	}
	locationExpectError(t, ctx, tx, "child under bed", "23514",
		"INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name) VALUES ($1, $2, $3, 'room', 'LOC-RM2', 'Kamar 2')",
		companyID, merchantID, bedID)

	// Pointing the level at itself forms a cycle → 23514.
	locationExpectError(t, ctx, tx, "cyclic parent", "23514",
		"UPDATE core.location SET parent_id = id WHERE id = $1", levelID)
}

// TestLocation_DB_Constraints — table CHECK/UNIQUE constraints: sites need
// coordinates, functions only exist on rooms, unknown functions are rejected,
// and codes are unique per merchant.
func TestLocation_DB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.cons", "081234570102")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	// Site without latitude/longitude → 23514 (chk_location_site_position).
	locationExpectError(t, ctx, tx, "site without coordinates", "23514",
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'site', 'LOC-SITE', 'Kampus Utama')",
		companyID, merchantID)

	// Site WITH coordinates inserts fine.
	if _, err := tx.Exec(ctx,
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name, latitude, longitude) VALUES ($1, $2, 'site', 'LOC-SITE1', 'Kampus Utama', -6.2, 106.8)",
		companyID, merchantID); err != nil {
		t.Fatalf("insert site with coordinates: %v", err)
	}

	// functions on a non-room kind → 23514 (chk_location_functions_room).
	locationExpectError(t, ctx, tx, "functions on building", "23514",
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions) VALUES ($1, $2, 'building', 'LOC-BLD2', 'Gedung 2', '{practice}')",
		companyID, merchantID)

	// Unknown function → 23514 (functions subset check).
	locationExpectError(t, ctx, tx, "unknown function", "23514",
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions) VALUES ($1, $2, 'room', 'LOC-RM3', 'Kamar 3', '{spa}')",
		companyID, merchantID)

	// Duplicate code within one merchant → 23505 (UNIQUE (merchant_id, code)).
	if _, err := locationInsert(t, ctx, tx, companyID, merchantID, "", "building", "LOC-DUP", "Gedung A", "{}"); err != nil {
		t.Fatalf("insert first LOC-DUP: %v", err)
	}
	locationExpectError(t, ctx, tx, "duplicate code", "23505",
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'building', 'LOC-DUP', 'Gedung B')",
		companyID, merchantID)
}

// TestLocation_DB_RLS — as app_runtime scoped to merchant A, merchant B's
// locations are invisible, and attaching a child to merchant B's parent fails
// with 'parent location not found' (the trigger reads the parent through the
// caller's RLS).
func TestLocation_DB_RLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.rls.a", "081234570103")
	companyB, merchantB, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.rls.b", "081234570104")

	// Seed merchant B's site with the owner pool (bypasses RLS).
	var siteB string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name, latitude, longitude) VALUES ($1, $2, 'site', 'LOC-B-SITE', 'Kampus B', -6.2, 106.8) RETURNING id::text",
		companyB, merchantB).Scan(&siteB); err != nil {
		t.Fatalf("seed merchant B site: %v", err)
	}

	tx := locationTx(t, ctx, pool, companyA, merchantA)
	defer tx.Rollback(ctx)

	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.location WHERE id = $1", siteB).Scan(&visible); err != nil {
		t.Fatalf("count merchant B site as merchant A: %v", err)
	}
	if visible != 0 {
		t.Fatalf("merchant B site visible to merchant A: got %d rows, want 0", visible)
	}

	if _, err := tx.Exec(ctx,
		"SAVEPOINT sp"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err := tx.Exec(ctx,
		"INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name) VALUES ($1, $2, $3, 'building', 'LOC-A-BLD', 'Gedung di B')",
		companyA, merchantA, siteB)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "parent location not found" {
		t.Fatalf("cross-tenant parent: expected 23514 'parent location not found', got %v", err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// TestLocation_DB_PermissionGranted — migration 000055 grants
// core.location.manage to the seeded Owner role and to the platform role
// templates (Admin Klinik/Admin RS), bumping the template version to 2.
func TestLocation_DB_PermissionGranted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.perm", "081234570105")

	var ownerHas int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.role r
		JOIN core.role_permission rp ON rp.role_id = r.id
		JOIN core.permission p ON p.id = rp.permission_id
		WHERE r.company_id = $1 AND r.name = 'Owner' AND p.code = 'core.location.manage'`,
		companyID).Scan(&ownerHas); err != nil {
		t.Fatalf("check owner permission: %v", err)
	}
	if ownerHas != 1 {
		t.Fatalf("Owner role must hold core.location.manage, got %d", ownerHas)
	}

	var version, adminKlinik int
	if err := pool.QueryRow(ctx, `
		SELECT t.version, count(p.id)
		FROM core.template t
		JOIN core.template_role tr ON tr.template_id = t.id AND tr.name = 'Admin Klinik'
		LEFT JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
		LEFT JOIN core.permission p ON p.id = trp.permission_id AND p.code = 'core.location.manage'
		WHERE t.code = 'klinik_pratama' AND t.scope = 'platform' AND t.kind = 'role'
		GROUP BY t.version`).Scan(&version, &adminKlinik); err != nil {
		t.Fatalf("check klinik_pratama template: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected klinik_pratama version 2, got %d", version)
	}
	if adminKlinik != 1 {
		t.Fatalf("expected Admin Klinik (klinik_pratama) to hold core.location.manage, got %d", adminKlinik)
	}

	var adminRS int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_role tr
		JOIN core.template t ON t.id = tr.template_id AND t.code = 'rumah_sakit'
		JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
		JOIN core.permission p ON p.id = trp.permission_id
		WHERE tr.name = 'Admin RS' AND p.code = 'core.location.manage'`).Scan(&adminRS); err != nil {
		t.Fatalf("check Admin RS template: %v", err)
	}
	if adminRS != 1 {
		t.Fatalf("expected Admin RS (rumah_sakit) to hold core.location.manage, got %d", adminRS)
	}
}

// ---- API tests (handlers in location.go) ----

// locationCreate calls POST /api/v1/locations and returns the data object.
func locationCreate(t *testing.T, router *gin.Engine, token, companyID, merchantID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	rec := accountRequest(router, http.MethodPost, "/api/v1/locations", token, companyID, merchantID, raw)
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
	}
	return rec.Code, resp.Data
}

// locationByCode finds one item in a list response by code.
func locationByCode(t *testing.T, items []map[string]any, code string) map[string]any {
	t.Helper()
	for _, item := range items {
		if item["code"] == code {
			return item
		}
	}
	t.Fatalf("location %q not in list response", code)
	return nil
}

// locationError decodes {"error": "..."} responses.
func locationError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response %q: %v", rec.Body.String(), err)
	}
	return resp.Error
}

// TestLocation_API_TreePath — creating a 4-level chain (site → building →
// level → room) via POST, then GET list returns each item with path built from
// ancestor names joined " › " and children_count of active children.
func TestLocation_API_TreePath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.tree", "081234570110")

	create := func(body map[string]any) map[string]any {
		t.Helper()
		code, data := locationCreate(t, router, token, companyID, merchantID, body)
		if code != http.StatusCreated {
			t.Fatalf("create %v expected 201, got %d", body["code"], code)
		}
		return data
	}
	site := create(map[string]any{"merchant_id": merchantID, "kind": "site", "code": "API-SITE", "name": "Kampus Utama", "latitude": -6.2, "longitude": 106.8})
	building := create(map[string]any{"merchant_id": merchantID, "parent_id": site["id"], "kind": "building", "code": "API-BLD", "name": "Gedung Utama"})
	level := create(map[string]any{"merchant_id": merchantID, "parent_id": building["id"], "kind": "level", "code": "API-LVL", "name": "Lantai 2"})
	room := create(map[string]any{"merchant_id": merchantID, "parent_id": level["id"], "kind": "room", "code": "API-RM", "name": "Kamar Rawat 1", "functions": []string{"ward_room"}})
	create(map[string]any{"merchant_id": merchantID, "parent_id": room["id"], "kind": "bed", "code": "API-BED", "name": "TT 1"})

	rec := accountRequest(router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/locations", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	// path = names from root to node joined " › ": Kampus Utama › Gedung
	// Utama › Lantai 2 › Kamar Rawat 1 (4 parts). children_count = active
	// children: site 1 (the building), room 1 (the bed).
	roomItem := locationByCode(t, resp.Data, "API-RM")
	if roomItem["path"] != "Kampus Utama › Gedung Utama › Lantai 2 › Kamar Rawat 1" {
		t.Fatalf("room path = %v, want 4-part ancestor path", roomItem["path"])
	}
	if roomItem["children_count"] != float64(1) {
		t.Fatalf("room children_count = %v, want 1", roomItem["children_count"])
	}
	siteItem := locationByCode(t, resp.Data, "API-SITE")
	if siteItem["path"] != "Kampus Utama" {
		t.Fatalf("site path = %v, want root name only", siteItem["path"])
	}
	if siteItem["children_count"] != float64(1) {
		t.Fatalf("site children_count = %v, want 1", siteItem["children_count"])
	}
	bedItem := locationByCode(t, resp.Data, "API-BED")
	if bedItem["path"] != "Kampus Utama › Gedung Utama › Lantai 2 › Kamar Rawat 1 › TT 1" {
		t.Fatalf("bed path = %v, want 5-part ancestor path", bedItem["path"])
	}
}

// TestLocation_API_DuplicateCode409 — UNIQUE (merchant_id, code) surfaces as
// 409 "location code already exists".
func TestLocation_API_DuplicateCode409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.dup", "081234570111")
	body := map[string]any{"merchant_id": merchantID, "kind": "building", "code": "API-DUP", "name": "Gedung A"}
	if code, _ := locationCreate(t, router, token, companyID, merchantID, body); code != http.StatusCreated {
		t.Fatalf("first create expected 201, got %d", code)
	}
	raw, _ := json.Marshal(body)
	rec := accountRequest(router, http.MethodPost, "/api/v1/locations", token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate code expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := locationError(t, rec); msg != "location code already exists" {
		t.Fatalf("error = %q, want location code already exists", msg)
	}
}

// TestLocation_API_BedValidation400 — a bed under a room without ward_room is
// rejected by the parent-check trigger (23514, trigger message verbatim), and
// a site without coordinates hits chk_location_site_position.
func TestLocation_API_BedValidation400(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.bed", "081234570112")
	code, room := locationCreate(t, router, token, companyID, merchantID, map[string]any{
		"merchant_id": merchantID, "kind": "room", "code": "API-PLN", "name": "Kamar Biasa",
	})
	if code != http.StatusCreated {
		t.Fatalf("plain room create expected 201, got %d", code)
	}
	raw, _ := json.Marshal(map[string]any{"merchant_id": merchantID, "parent_id": room["id"], "kind": "bed", "code": "API-BED2", "name": "TT 2"})
	rec := accountRequest(router, http.MethodPost, "/api/v1/locations", token, companyID, merchantID, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bed in non-ward room expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := locationError(t, rec); msg != "bed must be inside a ward room" {
		t.Fatalf("error = %q, want bed must be inside a ward room", msg)
	}

	raw, _ = json.Marshal(map[string]any{"merchant_id": merchantID, "kind": "site", "code": "API-SITE2", "name": "Kampus 2"})
	rec = accountRequest(router, http.MethodPost, "/api/v1/locations", token, companyID, merchantID, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("site without coordinates expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := locationError(t, rec); msg != "site requires latitude and longitude" {
		t.Fatalf("error = %q, want site requires latitude and longitude", msg)
	}
}

// TestLocation_API_DeleteWithChild409 — deleting a node that still has active
// children is 409; once the child is gone the delete succeeds (204) and the
// row disappears (404).
func TestLocation_API_DeleteWithChild409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.del", "081234570113")
	_, site := locationCreate(t, router, token, companyID, merchantID, map[string]any{
		"merchant_id": merchantID, "kind": "site", "code": "API-DEL-S", "name": "Kampus", "latitude": -6.2, "longitude": 106.8,
	})
	_, building := locationCreate(t, router, token, companyID, merchantID, map[string]any{
		"merchant_id": merchantID, "parent_id": site["id"], "kind": "building", "code": "API-DEL-B", "name": "Gedung",
	})
	siteID := site["id"].(string)
	buildingID := building["id"].(string)

	rec := accountRequest(router, http.MethodDelete, "/api/v1/locations/"+siteID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete with child expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := locationError(t, rec); msg != "location still has child locations" {
		t.Fatalf("error = %q, want location still has child locations", msg)
	}
	rec = accountRequest(router, http.MethodDelete, "/api/v1/locations/"+buildingID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete leaf expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = accountRequest(router, http.MethodGet, "/api/v1/locations/"+buildingID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET deleted location expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLocation_API_RequiresPermission — a user whose role lacks
// core.location.manage gets 403 on both list and create.
func TestLocation_API_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.perm", "081234570114")
	restrictedUserID := seedRestrictedAuditUser(t, ctx, pool, companyID, merchantID, "loc.api.noperm")
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "loc.api.noperm", "loc-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate restricted token: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/locations", restrictedToken, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(map[string]any{"merchant_id": merchantID, "kind": "building", "code": "API-NOPE", "name": "Gedung"})
	rec = accountRequest(router, http.MethodPost, "/api/v1/locations", restrictedToken, companyID, merchantID, raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLocation_API_PatchRoomWithBeds409 — PATCHing a ward room's functions to
// drop ward_room while it still holds active beds is 409; kind is immutable
// (silently ignored on PATCH, the row stays a room).
func TestLocation_API_PatchRoomWithBeds409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "loc.api.patch", "081234570115")
	_, room := locationCreate(t, router, token, companyID, merchantID, map[string]any{
		"merchant_id": merchantID, "kind": "room", "code": "API-PATCH-RM", "name": "Kamar Rawat 2", "functions": []string{"ward_room"},
	})
	roomID := room["id"].(string)
	if code, _ := locationCreate(t, router, token, companyID, merchantID, map[string]any{
		"merchant_id": merchantID, "parent_id": roomID, "kind": "bed", "code": "API-PATCH-BED", "name": "TT 3",
	}); code != http.StatusCreated {
		t.Fatalf("bed create expected 201, got %d", code)
	}

	raw, _ := json.Marshal(map[string]any{"functions": []string{}})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/locations/"+roomID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH dropping ward_room with beds expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := locationError(t, rec); msg != "room still has beds" {
		t.Fatalf("error = %q, want room still has beds", msg)
	}

	// kind cannot be changed: the PATCH body's kind is ignored and the row
	// stays a room.
	raw, _ = json.Marshal(map[string]any{"kind": "site", "name": "Kamar Rawat 2 Renamed"})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/locations/"+roomID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH name expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if resp.Data["kind"] != "room" {
		t.Fatalf("kind changed to %v, want immutable room", resp.Data["kind"])
	}
	if resp.Data["name"] != "Kamar Rawat 2 Renamed" {
		t.Fatalf("name = %v, want Kamar Rawat 2 Renamed", resp.Data["name"])
	}
}
