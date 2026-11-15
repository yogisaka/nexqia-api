//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// Nutrition order + diet catalog tests (Task 1 plan 2026-10-21-cppt-rajal-b,
// spec 2026-10-21-cppt-rajal-fase1-design §4, §9). Fixtures reuse the shared
// clinical helpers; each test gets its own Postgres container so fixed UUIDs
// are safe. Expectations come from the contract/DDL, not from the handlers.

var noCounter int64

func noNext() int64 { return atomic.AddInt64(&noCounter, 1) }

// noSeedDiet inserts a diet type directly (catalog rows only exist in seed for
// demo merchants); returns id + code.
func noSeedDiet(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, code string, active bool) string {
	t.Helper()
	var id string
	q := "INSERT INTO core.diet_type (company_id, merchant_id, code, name, is_active) VALUES ($1, $2, $3, $3, $4) RETURNING id::text"
	if err := pool.QueryRow(ctx, q, companyID, merchantID, code, active).Scan(&id); err != nil {
		t.Fatalf("seed diet type %s: %v", code, err)
	}
	return id
}

// noSeedEnv wraps cfSeedEnv with a nutrition-specific label.
func noSeedEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, label string) (companyID, merchantID, userID, token string) {
	t.Helper()
	n := noNext()
	companyID, merchantID, userID, token, _ = seedAccountOwner(t, ctx, pool, router, "no."+label, fmt.Sprintf("08123482%04d", n))
	return companyID, merchantID, userID, token
}

// TestDietTypeCreateAndList — POST creates (201, echo), GET lists only active
// rows of the merchant (contract: {"data":[{id,code,name,default_calorie,is_active}]}).
func TestDietTypeCreateAndList(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "dt")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", token, companyID, merchantID, map[string]any{
		"code": "  DM1700 ", "name": "Diet DM 1700 kkal", "default_calorie": 1700.0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	item := resp["data"].(map[string]any)
	if item["code"] != "DM1700" || item["name"] != "Diet DM 1700 kkal" || item["is_active"] != true {
		t.Fatalf("created = %v (code must be trimmed)", item)
	}

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/diet-types", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("list got %d items, want 1", len(data))
	}
	if data[0].(map[string]any)["code"] != "DM1700" {
		t.Fatalf("list[0] = %v", data[0])
	}

	// Empty code / name → 400 with the contract message.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", token, companyID, merchantID, map[string]any{"code": "  ", "name": "X"})
	if rec.Code != http.StatusBadRequest || resp["error"] != "kode dan nama diet wajib" {
		t.Fatalf("blank code = %d %v", rec.Code, resp)
	}
}

// TestDietTypeDuplicate409 — same (merchant, code) → 409 "kode diet sudah ada"
// (UNIQUE (merchant_id, code), migration 000010); another merchant may reuse it.
func TestDietTypeDuplicate409(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "dup")
	body := map[string]any{"code": "RGS", "name": "Rendah Garam"}
	if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", token, companyID, merchantID, body); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", token, companyID, merchantID, body)
	if rec.Code != http.StatusConflict || resp["error"] != "kode diet sudah ada" {
		t.Fatalf("duplicate = %d %v", rec.Code, resp)
	}
	// Another merchant with the same code is fine.
	companyB, merchantB, _, tokenB := noSeedEnv(t, ctx, pool, router, "dupb")
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", tokenB, companyB, merchantB, body); rec.Code != http.StatusCreated {
		t.Fatalf("cross-merchant create = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNutritionOrderCreateValidate — diet inactive → 400, ranap admission →
// 400, success → 201 active with ordered_by = current user (contract §9).
func TestNutritionOrderCreateValidate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, userID, token := noSeedEnv(t, ctx, pool, router, "ord")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "ord")
	inactiveID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-DIET-OFF", false)
	activeID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-DIET-ON", true)
	personID, _ := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "ord2")
	inpatientID := cfSeedInpatientAdmission(t, ctx, pool, companyID, merchantID, personID, "ord")

	post := func(admission, diet string, route any) (*int, map[string]any) {
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
			"admission_id": admission, "diet_type_id": diet, "route": route,
		})
		return &rec.Code, resp
	}

	// Inactive diet type → 400.
	if code, resp := post(admissionID, inactiveID, "oral"); *code != http.StatusBadRequest || resp["error"] != "jenis diet tidak aktif" {
		t.Fatalf("inactive diet = %d %v", *code, resp)
	}
	// Inpatient admission → 400.
	if code, resp := post(inpatientID, activeID, "oral"); *code != http.StatusBadRequest || resp["error"] != "cppt fase 1 hanya rawat jalan" {
		t.Fatalf("inpatient = %d %v", *code, resp)
	}
	// Invalid route → 400.
	if code, resp := post(admissionID, activeID, "iv"); *code != http.StatusBadRequest || resp["error"] != "invalid value for route" {
		t.Fatalf("bad route = %d %v", *code, resp)
	}
	// Invalid texture → 400.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": activeID, "route": "oral", "texture": "cair_bening",
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for texture" {
		t.Fatalf("bad texture = %d %v", rec.Code, resp)
	}
	// Success → 201, status active, ordered_by = the calling user.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": activeID, "route": "ngt",
		"texture": "cair", "calorie_target": 1500.5, "restriction_note": "tanpa garam",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	order := resp["data"].(map[string]any)
	if order["status"] != "active" || order["route"] != "ngt" || order["texture"] != "cair" {
		t.Fatalf("order = %v", order)
	}
	if order["ordered_by"] != userID {
		t.Fatalf("ordered_by = %v, want current user %s", order["ordered_by"], userID)
	}
	if order["diet_type_code"] != "NO-DIET-ON" {
		t.Fatalf("diet join = %v", order)
	}
	// List per admission joins the diet type.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-orders?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 1 {
		t.Fatalf("list = %d %v", rec.Code, resp)
	}
}

// TestNutritionOrderReviseConflict — PATCH without / stale row_version → 409
// "data sudah berubah"; correct row_version bumps it (optimistic locking).
func TestNutritionOrderReviseConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "rev")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "rev")
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-REV-DIET", true)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": dietID, "route": "oral",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	order := resp["data"].(map[string]any)
	id := order["id"].(string)
	// DDL default row_version = 1 (migration 000089 nutrition_order).
	if rv, _ := order["row_version"].(float64); rv != 1 {
		t.Fatalf("initial row_version = %v, want 1", order["row_version"])
	}

	patch := func(body map[string]any) (*int, map[string]any) {
		rec, resp := cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, body)
		return &rec.Code, resp
	}
	// Missing row_version → 409.
	if code, resp := patch(map[string]any{"route": "ngt"}); *code != http.StatusConflict || resp["error"] != "data sudah berubah" {
		t.Fatalf("no row_version = %d %v", *code, resp)
	}
	// Stale row_version → 409.
	if code, resp := patch(map[string]any{"route": "ngt", "row_version": 99}); *code != http.StatusConflict || resp["error"] != "data sudah berubah" {
		t.Fatalf("stale row_version = %d %v", *code, resp)
	}
	// Correct row_version → 200, row_version bumped to 2.
	code, resp := patch(map[string]any{"route": "ngt", "row_version": 1, "restriction_note": "perbanyak cairan"})
	if *code != http.StatusOK {
		t.Fatalf("patch ok = %d", *code)
	}
	got := resp["data"].(map[string]any)
	if got["route"] != "ngt" || got["restriction_note"] != "perbanyak cairan" {
		t.Fatalf("patched = %v", got)
	}
	if rv, _ := got["row_version"].(float64); rv != 2 {
		t.Fatalf("row_version after patch = %v, want 2", got["row_version"])
	}
	// Foreign merchant → 404.
	companyB, merchantB, _, tokenB := noSeedEnv(t, ctx, pool, router, "revb")
	if rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, tokenB, companyB, merchantB, map[string]any{"row_version": 2}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-merchant patch = %d %v", rec.Code, resp)
	}
}

// TestNutritionOrderStatusTransition — active → completed (ended_at set),
// then completed → anything → 409 "status order tidak bisa diubah".
func TestNutritionOrderStatusTransition(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "st")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "st")
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-ST-DIET", true)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": dietID, "route": "oral",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["id"].(string)

	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"status": "completed", "row_version": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	done := resp["data"].(map[string]any)
	if done["status"] != "completed" || done["ended_at"] == nil {
		t.Fatalf("completed = %v (ended_at must be set)", done)
	}
	if rv, _ := done["row_version"].(float64); rv != 2 {
		t.Fatalf("row_version = %v, want 2", done["row_version"])
	}

	// completed → cancelled / completed → active / re-completed all → 409.
	for _, next := range []string{"cancelled", "active", "completed"} {
		rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
			"status": next, "row_version": done["row_version"].(float64),
		})
		if rec.Code != http.StatusConflict || resp["error"] != "status order tidak bisa diubah" {
			t.Fatalf("completed → %s = %d %v", next, rec.Code, resp)
		}
	}
}

// TestNutritionOrderTerminalRevision — PATCH tanpa field status pada order
// completed → 409 "order diet sudah selesai dan tidak bisa direvisi"
// (review B1); revisi field biasa pada order active tetap 200 dan ended_at
// tidak di-set. Juga: list admission merchant lain → 404 (review B2).
func TestNutritionOrderTerminalRevision(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "term")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "term")
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-TERM-DIET", true)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": dietID, "route": "oral",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["id"].(string)

	// Field-only revision on the active order → 200, ended_at stays empty.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"restriction_note": "revisi biasa", "row_version": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("active revision = %d: %s", rec.Code, rec.Body.String())
	}
	got := resp["data"].(map[string]any)
	if got["ended_at"] != nil {
		t.Fatalf("ended_at = %v, want nil on field-only revision", got["ended_at"])
	}

	// Cross-merchant admission list → 404 (review B2).
	companyB, merchantB, _, tokenB := noSeedEnv(t, ctx, pool, router, "termb")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-orders?admission_id="+admissionID, tokenB, companyB, merchantB, nil); rec.Code != http.StatusNotFound || resp["error"] != "admission tidak ditemukan" {
		t.Fatalf("cross-merchant list = %d %v", rec.Code, resp)
	}

	// Complete the order, then a field-only PATCH (no status) → 409 terminal.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"status": "completed", "row_version": 2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"restriction_note": "coba revisi setelah selesai", "row_version": 3,
	})
	if rec.Code != http.StatusConflict || resp["error"] != "order diet sudah selesai dan tidak bisa direvisi" {
		t.Fatalf("terminal revision = %d %v", rec.Code, resp)
	}
}

// TestNutritionOrderPermissionDenied — a user without clinical.nutrition.write
// gets 403 on the write endpoints; view-only stays possible (clinical.record.view).
func TestNutritionOrderPermissionDenied(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := noSeedEnv(t, ctx, pool, router, "perm")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "perm")
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-PERM-DIET", true)

	viewer := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "no.viewer", server.PermClinicalRecordView)
	body := map[string]any{"admission_id": admissionID, "diet_type_id": dietID, "route": "oral"}
	if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", viewer, companyID, merchantID, body); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/diet-types", viewer, companyID, merchantID, map[string]any{"code": "X", "name": "Y"}); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer diet create = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Owner creates; viewer can still list (view permission only).
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-orders?admission_id="+admissionID, viewer, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("viewer list = %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, viewer, companyID, merchantID, map[string]any{"row_version": 1}); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestNutritionOrderRevisionHistory — create/revise/stop each append one row to
// clinical.nutrition_order_revision (migration 000101) inside the request tx;
// GET /clinical/nutrition-order-revisions returns them changed_at DESC with the
// contract shape {row_version, action, note, changed_by_name, changed_at}.
func TestNutritionOrderRevisionHistory(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	// Username fixture = "no." + label (seedAccountOwner arg).
	const username = "no.rvhist"
	companyID, merchantID, _, token := noSeedEnvWithUsername(t, ctx, pool, router, username)
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "rvhist")
	dietID := noSeedDiet(t, ctx, pool, companyID, merchantID, "NO-RV-DIET", true)

	// 1) POST order without note → create row, note nil, row_version 1.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-orders", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "diet_type_id": dietID, "route": "oral",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["id"].(string)

	// 2) PATCH revise with note → revise row (row_version 2).
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"calorie_target": 1600.0, "note": "Kalori 1800 → 1600 kcal/hari", "row_version": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("revise = %d: %s", rec.Code, rec.Body.String())
	}
	if rv, _ := resp["data"].(map[string]any)["row_version"].(float64); rv != 2 {
		t.Fatalf("revise row_version = %v, want 2", resp["data"].(map[string]any)["row_version"])
	}

	// 3) PATCH stop (status cancelled) → stop row (row_version 3).
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"status": "cancelled", "note": "pasien pulang", "row_version": 2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", rec.Code, rec.Body.String())
	}

	// 4) GET revisions → 3 rows, changed_at DESC (stop, revise, create).
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-order-revisions?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("revisions got %d rows, want 3: %v", len(data), data)
	}
	// Expectations from the contract, not the handlers.
	actions := []string{"stop", "revise", "create"}
	for i, want := range actions {
		row := data[i].(map[string]any)
		if row["action"] != want {
			t.Fatalf("row[%d].action = %v, want %s", i, row["action"], want)
		}
		if row["changed_by_name"] != username {
			t.Fatalf("row[%d].changed_by_name = %v, want %s", i, row["changed_by_name"], username)
		}
		if _, okTS := row["changed_at"].(string); !okTS {
			t.Fatalf("row[%d].changed_at = %v, want RFC3339 string", i, row["changed_at"])
		}
	}
	stopRow := data[0].(map[string]any)
	if stopRow["row_version"].(float64) != 3 {
		t.Fatalf("stop row_version = %v, want 3", stopRow["row_version"])
	}
	if stopRow["note"] != "pasien pulang" {
		t.Fatalf("stop note = %v", stopRow["note"])
	}
	revRow := data[1].(map[string]any)
	if revRow["note"] != "Kalori 1800 → 1600 kcal/hari" {
		t.Fatalf("revise note = %v", revRow["note"])
	}
	if revRow["row_version"].(float64) != 2 {
		t.Fatalf("revise row_version = %v, want 2", revRow["row_version"])
	}
	createRow := data[2].(map[string]any)
	if createRow["note"] != nil {
		t.Fatalf("create note = %v, want nil", createRow["note"])
	}
	if createRow["row_version"].(float64) != 1 {
		t.Fatalf("create row_version = %v, want 1", createRow["row_version"])
	}

	// note 201 rune → 400 with the exact contract message.
	long := strings.Repeat("a", 201)
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-orders/"+id, token, companyID, merchantID, map[string]any{
		"note": long, "row_version": 3,
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "note maksimal 200 karakter" {
		t.Fatalf("long note = %d %v", rec.Code, resp)
	}
	// GET without admission_id → 400.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-order-revisions", token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest || resp["error"] != "admission_id wajib diisi" {
		t.Fatalf("missing admission_id = %d %v", rec.Code, resp)
	}
}

// noSeedEnvWithUsername is noSeedEnv with an explicit username (revision
// history asserts changed_by_name against it).
func noSeedEnvWithUsername(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, username string) (companyID, merchantID, userID, token string) {
	t.Helper()
	companyID, merchantID, userID, token, _ = seedAccountOwner(t, ctx, pool, router, username, fmt.Sprintf("08123493%04d", noNext()))
	return companyID, merchantID, userID, token
}
