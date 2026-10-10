//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestInventoryItem_* covers the company-level item master API (spec
// 2026-10-07-tariff-c-drug-master-pricing §3.3-§3.5): create with ingredients,
// validation via CHECK constraints, packaging defaults and guards, optimistic
// locking and the inventory.item.manage permission.

// invSeed registers a fresh Owner and returns company/merchant/token.
func invSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, suffix, phone string) (companyID, merchantID, token string) {
	t.Helper()
	companyID, merchantID, _, token, _ = seedAccountOwner(t, ctx, pool, router, "inv."+suffix, phone)
	return companyID, merchantID, token
}

// invUnitID resolves a platform unit id by code.
func invUnitID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, "SELECT id FROM inventory.unit WHERE code = $1", code).Scan(&id); err != nil {
		t.Fatalf("resolve unit %q: %v", code, err)
	}
	return id
}

// invCreateItem posts a valid drug item and returns (status, body map, id).
func invCreateItem(t *testing.T, router *gin.Engine, token, companyID, merchantID, code string, extra map[string]any) (int, map[string]any, string) {
	t.Helper()
	body := map[string]any{
		"code": code, "name": code + " name", "item_kind": "drug",
		"pricing_category": "generic", "drug_class": "keras",
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	rec := accountRequest(router, http.MethodPost, "/api/v1/inventory/items", token, companyID, merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return rec.Code, out, id
}

// invGet fetches the item detail.
func invGet(t *testing.T, router *gin.Engine, token, companyID, merchantID, itemID string) (int, map[string]any) {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/inventory/items/"+itemID, token, companyID, merchantID, nil)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// invDetail extracts the data object of a 200/201 item response.
func invDetail(t *testing.T, status int, out map[string]any, want int) map[string]any {
	t.Helper()
	if status != want {
		t.Fatalf("status = %d, want %d: %v", status, want, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data: %v", out)
	}
	return data
}

// invErr extracts the error message.
func invErr(out map[string]any) string {
	msg, _ := out["error"].(string)
	return msg
}

// TestInventoryItem_CreateAndGet — a drug with KFA code and 2 active
// ingredients is created; the detail carries the base unit packaging
// (factor "1.000", default purchase+issue) and track_batch true.
func TestInventoryItem_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "create", "081234610001")
	tablet := invUnitID(t, ctx, pool, "tablet")

	status, out, itemID := invCreateItem(t, router, token, companyID, merchantID, "INV-CR-1", map[string]any{
		"base_unit_id": tablet,
		"kfa_code":     "930000001",
		"het_amount":   "15000.00",
		// chk_item_het_pair: het_amount never travels alone.
		"het_effective_from": "2026-01-01",
		"ingredients": []map[string]any{
			{"ingredient_code": "910000001", "ingredient_name": "Paracetamol", "strength": "500", "strength_unit": "mg"},
			{"ingredient_name": "Caffeine", "strength": "50", "strength_unit": "mg", "per_quantity": "2"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create expected 201, got %d: %v", status, out)
	}
	if itemID == "" {
		t.Fatalf("missing item id: %v", out)
	}

	status, out = invGet(t, router, token, companyID, merchantID, itemID)
	data := invDetail(t, status, out, http.StatusOK)
	if data["code"] != "INV-CR-1" || data["track_batch"] != true || data["track_expiry"] != true {
		t.Fatalf("detail basics = %v", data)
	}
	if data["het_amount"] != "15000.00" {
		t.Fatalf("het_amount = %v, want 15000.00", data["het_amount"])
	}
	units, _ := data["units"].([]any)
	if len(units) != 1 {
		t.Fatalf("units = %v, want 1", units)
	}
	unit := units[0].(map[string]any)
	// Base unit packaging: factor 1 rendered with 3 fraction digits.
	if unit["factor"] != "1.000" || unit["is_default_purchase"] != true || unit["is_default_issue"] != true {
		t.Fatalf("base unit row = %v", unit)
	}
	baseUnit, _ := unit["unit"].(map[string]any)
	if baseUnit["code"] != "tablet" {
		t.Fatalf("base unit = %v, want tablet", baseUnit)
	}
	ings, _ := data["ingredients"].([]any)
	if len(ings) != 2 {
		t.Fatalf("ingredients = %v, want 2", ings)
	}
	first := ings[0].(map[string]any)
	if first["ingredient_name"] != "Paracetamol" || first["strength"] != "500.0000" {
		t.Fatalf("ingredient[0] = %v", first)
	}
	if data["row_version"].(float64) != 1 {
		t.Fatalf("row_version = %v, want 1", data["row_version"])
	}

	// List shows the ringkas shape.
	rec := accountRequest(router, http.MethodGet, "/api/v1/inventory/items", token, companyID, merchantID, nil)
	list := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %v", rec.Code, list)
	}
	rows, _ := list["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list rows = %v, want 1", rows)
	}
	row := rows[0].(map[string]any)
	if row["base_unit_code"] != "tablet" || row["het_amount"] != "15000.00" || row["item_kind"] != "drug" {
		t.Fatalf("list row = %v", row)
	}
}

// TestInventoryItem_Validation — CHECK constraint mapping: drug without KFA
// and without non-KFA reason → 400 exact message; drug_class on material →
// 400; duplicate code → 409.
func TestInventoryItem_Validation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "valid", "081234610002")
	tablet := invUnitID(t, ctx, pool, "tablet")

	// Drug, no KFA code, non_kfa false → chk_item_kfa.
	status, out, _ := invCreateItem(t, router, token, companyID, merchantID, "INV-VAL-1", map[string]any{
		"base_unit_id": tablet,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("no-KFA create expected 400, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "drugs, vaccines and medical devices need a KFA code or a non-KFA reason" {
		t.Fatalf("error = %q", msg)
	}

	// drug_class set on a material kind → chk_item_drug_class (KFA not needed).
	status, out, _ = invCreateItem(t, router, token, companyID, merchantID, "INV-VAL-2", map[string]any{
		"base_unit_id": tablet, "item_kind": "material", "drug_class": "keras",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("material+drug_class expected 400, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "drug_class is required for drugs and vaccines only" {
		t.Fatalf("error = %q", msg)
	}

	// Valid item, then the same code again → unique violation.
	status, out, _ = invCreateItem(t, router, token, companyID, merchantID, "INV-VAL-3", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000002",
	})
	if status != http.StatusCreated {
		t.Fatalf("valid create expected 201, got %d: %v", status, out)
	}
	status, out, _ = invCreateItem(t, router, token, companyID, merchantID, "INV-VAL-3", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000003",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate create expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "item code already exists" {
		t.Fatalf("error = %q", msg)
	}
}

// TestInventoryItem_Units — packaging: adding a box with factor 100 as the
// purchase default moves the default off the base unit; a second active box
// row collides; deactivating the base unit packaging is rejected by the
// trigger; factors with more than 3 fraction digits are invalid.
func TestInventoryItem_Units(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "units", "081234610003")
	tablet := invUnitID(t, ctx, pool, "tablet")
	box := invUnitID(t, ctx, pool, "box")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "INV-UN-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000004",
	})
	status, out := invGet(t, router, token, companyID, merchantID, itemID)
	data := invDetail(t, status, out, http.StatusOK)
	units, _ := data["units"].([]any)
	baseRow := units[0].(map[string]any)
	baseRowID := baseRow["id"].(string)

	// Add box factor 100 as purchase default.
	raw, _ := json.Marshal(map[string]any{"unit_id": box, "factor": "100", "is_default_purchase": true})
	rec := accountRequest(router, http.MethodPost, "/api/v1/inventory/items/"+itemID+"/units", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add box expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	boxOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &boxOut)
	boxData := invDetail(t, http.StatusCreated, boxOut, http.StatusCreated)
	boxUnits, _ := boxData["units"].([]any)
	if len(boxUnits) != 2 {
		t.Fatalf("units = %v, want 2", boxUnits)
	}
	var boxRow map[string]any
	for _, u := range boxUnits {
		row := u.(map[string]any)
		unitRef := row["unit"].(map[string]any)
		if unitRef["code"] == "box" {
			boxRow = row
		}
	}
	if boxRow == nil || boxRow["factor"] != "100.000" || boxRow["is_default_purchase"] != true {
		t.Fatalf("box row = %v", boxRow)
	}
	// The base unit row inside the SAME response lost the purchase default.
	var baseAfter map[string]any
	for _, u := range boxUnits {
		row := u.(map[string]any)
		if row["id"] == baseRowID {
			baseAfter = row
		}
	}
	if baseAfter == nil || baseAfter["is_default_purchase"] != false {
		t.Fatalf("base unit after box add = %v, want is_default_purchase false", baseAfter)
	}

	// Same active unit again → unique.
	raw, _ = json.Marshal(map[string]any{"unit_id": box, "factor": "100"})
	rec = accountRequest(router, http.MethodPost, "/api/v1/inventory/items/"+itemID+"/units", token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate box expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	dup := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &dup)
	if msg := invErr(dup); msg != "packaging unit already exists" {
		t.Fatalf("error = %q", msg)
	}

	// Deactivate the base unit packaging → trigger.
	raw, _ = json.Marshal(map[string]any{"is_active": false})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/inventory/item-units/"+baseRowID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deactivate base unit expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	deact := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &deact)
	if msg := invErr(deact); msg != "base unit packaging cannot be deactivated" {
		t.Fatalf("error = %q", msg)
	}

	// factor with 4 fraction digits → 400 invalid factor.
	raw, _ = json.Marshal(map[string]any{"unit_id": box, "factor": "1.2345"})
	rec = accountRequest(router, http.MethodPost, "/api/v1/inventory/items/"+itemID+"/units", token, companyID, merchantID, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("factor 1.2345 expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	bad := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if msg := invErr(bad); msg != "invalid factor" {
		t.Fatalf("error = %q", msg)
	}
}

// TestInventoryItem_PatchVersion — optimistic locking: a stale row_version →
// 409; changing base_unit_id → 400.
func TestInventoryItem_PatchVersion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "patch", "081234610004")
	tablet := invUnitID(t, ctx, pool, "tablet")
	box := invUnitID(t, ctx, pool, "box")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "INV-PV-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000005",
	})

	// Stale row_version.
	raw, _ := json.Marshal(map[string]any{"name": "Nama Baru", "row_version": 99})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/inventory/items/"+itemID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale patch expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	stale := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &stale)
	if msg := invErr(stale); msg != "item changed, reload" {
		t.Fatalf("error = %q", msg)
	}

	// Current row_version but a base_unit_id change.
	raw, _ = json.Marshal(map[string]any{"row_version": 1, "base_unit_id": box})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/inventory/items/"+itemID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("base unit change expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	base := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &base)
	if msg := invErr(base); msg != "base unit cannot be changed" {
		t.Fatalf("error = %q", msg)
	}

	// A good patch succeeds and bumps row_version.
	raw, _ = json.Marshal(map[string]any{"name": "Nama Baru", "row_version": 1})
	rec = accountRequest(router, http.MethodPatch, "/api/v1/inventory/items/"+itemID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid patch expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	okOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &okOut)
	data := invDetail(t, http.StatusOK, okOut, http.StatusOK)
	if data["name"] != "Nama Baru" || data["row_version"].(float64) != 2 {
		t.Fatalf("patched detail = %v", data)
	}
}

// TestInventoryItem_RequiresPermission — a user without inventory.item.manage
// gets 403 on the list endpoint.
func TestInventoryItem_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "perm", "081234610005")

	// Revoke the Owner role's inventory.item.manage grant for this company's
	// system Owner role.
	if _, err := pool.Exec(ctx, `DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'inventory.item.manage'`); err != nil {
		t.Fatalf("revoke permission: %v", err)
	}

	rec := accountRequest(router, http.MethodGet, "/api/v1/inventory/items", token, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}
