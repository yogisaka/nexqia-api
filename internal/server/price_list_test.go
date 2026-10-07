//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestPriceList_* covers the price list API (spec 2026-10-07-tariff-price-lists
// §4-§5): manual prices, derived lists with category adjustments and rounding,
// price versioning under the advisory lock, missing prices, one-level
// derivation and delete guards, permissions, and the removed service-rate
// routes.

// plTZ is the merchant timezone used throughout (Jakarta).
const plTZ = "Asia/Jakarta"

// plNewEnv is dnNewEnv (high rate limit, explicit timezone) with Jakarta.
func plNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	return dnNewEnv(t, "pl."+label, plTZ)
}

// plToday is today's date in the env merchant timezone as YYYY-MM-DD.
func plToday(env *qeEnv, offsetDays int) string {
	y, m, d := time.Now().In(mustLoc(plTZ)).AddDate(0, 0, offsetDays).Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// plDataID asserts the status and returns data.id (snake_case helpers) or
// data.ID (raw sqlc structs like POST /service-items).
func plDataID(t *testing.T, status int, out map[string]any, want int) string {
	t.Helper()
	if status != want {
		t.Fatalf("status = %d, want %d: %v", status, want, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data: %v", out)
	}
	if id, _ := data["id"].(string); id != "" {
		return id
	}
	id, _ := data["ID"].(string)
	if id == "" {
		t.Fatalf("missing data.id: %v", out)
	}
	return id
}

// plErrorMsg extracts out["error"] (or "" when absent).
func plErrorMsg(out map[string]any) string {
	msg, _ := out["error"].(string)
	return msg
}

func plCreateItem(t *testing.T, env *qeEnv, code, itemType string) string {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/service-items", map[string]any{
		"merchant_id": env.merchantID, "code": code, "name": code + " name", "item_type": itemType,
	})
	return plDataID(t, status, out, http.StatusCreated)
}

func plCreateComponent(t *testing.T, env *qeEnv, code string) string {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/rate-components", map[string]any{
		"merchant_id": env.merchantID, "code": code, "name": code + " name",
	})
	return plDataID(t, status, out, http.StatusCreated)
}

func plCreateList(t *testing.T, env *qeEnv, code, baseID, adjustment, rounding string) string {
	t.Helper()
	body := map[string]any{"merchant_id": env.merchantID, "code": code, "name": code + " list"}
	if baseID != "" {
		body["base_price_list_id"] = baseID
		body["adjustment_percent"] = adjustment
		body["rounding_unit"] = rounding
	}
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/price-lists", body)
	return plDataID(t, status, out, http.StatusCreated)
}

func plSetPrice(t *testing.T, env *qeEnv, listID, itemID, from string, comps map[string]string) (int, map[string]any) {
	t.Helper()
	parts := make([]map[string]string, 0, len(comps))
	for id, amount := range comps {
		parts = append(parts, map[string]string{"rate_component_id": id, "amount": amount})
	}
	return qeRequest(t, env, http.MethodPut, "/api/v1/price-lists/"+listID+"/items/"+itemID+"/price", map[string]any{
		"effective_from": from, "components": parts,
	})
}

func plResolve(t *testing.T, env *qeEnv, listID, itemID, date string) (int, map[string]any) {
	t.Helper()
	return qeRequest(t, env, http.MethodGet,
		"/api/v1/price-lists/"+listID+"/resolve?item_id="+itemID+"&date="+date, nil)
}

func plResolveData(t *testing.T, env *qeEnv, listID, itemID, date string) map[string]any {
	t.Helper()
	status, out := plResolve(t, env, listID, itemID, date)
	if status != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("resolve missing data: %v", out)
	}
	return data
}

// plPut is dnPost for PUT (safe outside the test goroutine).
func plPut(env *qeEnv, path string, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("X-Company-ID", env.companyID)
	req.Header.Set("X-Merchant-ID", env.merchantID)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// plSeedPricedBase creates an item priced SARANA 60000 + MEDIS 40000 in a
// standalone list (total 100000.00), returning (listID, itemID, saranaID, medisID).
func plSeedPricedBase(t *testing.T, env *qeEnv, label string) (string, string, string, string) {
	t.Helper()
	itemID := plCreateItem(t, env, label+"-item", "procedure")
	saranaID := plCreateComponent(t, env, label+"-SAR")
	medisID := plCreateComponent(t, env, label+"-MED")
	listID := plCreateList(t, env, label, "", "", "")
	status, out := plSetPrice(t, env, listID, itemID, plToday(env, 0), map[string]string{saranaID: "60000.00", medisID: "40000.00"})
	if status != http.StatusOK {
		t.Fatalf("set base price: %d %v", status, out)
	}
	return listID, itemID, saranaID, medisID
}

// TestPriceList_ManualPrice — a standalone list with manually set components
// resolves source=manual with exact 2-decimal strings.
func TestPriceList_ManualPrice(t *testing.T) {
	env := plNewEnv(t, "manual")
	itemID := plCreateItem(t, env, "KNSUL-M", "consultation")
	saranaID := plCreateComponent(t, env, "SAR-M")
	medisID := plCreateComponent(t, env, "MED-M")
	listID := plCreateList(t, env, "umum-manual", "", "", "")

	status, out := plSetPrice(t, env, listID, itemID, plToday(env, 0),
		map[string]string{saranaID: "40000.00", medisID: "35000.00"})
	if status != http.StatusOK {
		t.Fatalf("set price status = %d: %v", status, out)
	}
	// 40000.00 + 35000.00 = 75000.00, manual source.
	res := plResolveData(t, env, listID, itemID, plToday(env, 0))
	if res["source"] != "manual" {
		t.Fatalf("source = %v, want manual", res["source"])
	}
	if res["total"] != "75000.00" {
		t.Fatalf("total = %v, want 75000.00", res["total"])
	}
	comps, _ := res["components"].([]any)
	if len(comps) != 2 {
		t.Fatalf("components = %v, want 2", res["components"])
	}
	amounts := map[string]bool{}
	for _, raw := range comps {
		comp := raw.(map[string]any)
		amounts[comp["amount"].(string)] = true
	}
	if !amounts["40000.00"] || !amounts["35000.00"] {
		t.Fatalf("component amounts = %v, want 40000.00 and 35000.00", amounts)
	}
}

// TestPriceList_DerivedAndCategory — derived +30% on a 100000 base = 130000;
// procedure 0% = 100000; 12.5% with 1000 rounding = 113000 (diff on the
// largest component).
func TestPriceList_DerivedAndCategory(t *testing.T) {
	env := plNewEnv(t, "derived")
	baseList, itemID, saranaID, medisID := plSeedPricedBase(t, env, "dbase")
	derived := plCreateList(t, env, "vip30", baseList, "30.00", "")

	res := plResolveData(t, env, derived, itemID, plToday(env, 0))
	// 100000 * 1.30 = 130000.00.
	if res["total"] != "130000.00" || res["source"] != "derived" || res["adjustment_percent_applied"] != "30.00" {
		t.Fatalf("derived resolve = %v", res)
	}

	status, out := qeRequest(t, env, http.MethodPut, "/api/v1/price-lists/"+derived+"/adjustments", map[string]any{
		"adjustments": []map[string]string{{"item_type": "procedure", "adjustment_percent": "0.00"}},
	})
	if status != http.StatusOK {
		t.Fatalf("put adjustments status = %d: %v", status, out)
	}
	res = plResolveData(t, env, derived, itemID, plToday(env, 0))
	// Category 0% overrides list 30% → 100000.00.
	if res["total"] != "100000.00" || res["adjustment_percent_applied"] != "0.00" {
		t.Fatalf("category resolve = %v", res)
	}

	rounded := plCreateList(t, env, "vip-rnd", baseList, "12.50", "1000.00")
	res = plResolveData(t, env, rounded, itemID, plToday(env, 0))
	// 100000 * 1.125 = 112500 → round to 113000; diff +500 on SARANA (largest):
	// 67500 + 500 = 68000, MEDIS 45000.
	if res["total"] != "113000.00" {
		t.Fatalf("rounded total = %v, want 113000.00", res["total"])
	}
	comps, _ := res["components"].([]any)
	for _, raw := range comps {
		comp := raw.(map[string]any)
		want := "45000.00"
		if comp["rate_component_id"] == saranaID {
			want = "68000.00"
		}
		if comp["rate_component_id"] != saranaID && comp["rate_component_id"] != medisID {
			t.Fatalf("unexpected component %v", comp)
		}
		if comp["amount"] != want {
			t.Fatalf("component amount = %v, want %s: %v", comp["amount"], want, comp)
		}
	}
}

// TestPriceList_SetPriceVersioning — versions supersede each other; overlapping
// sets conflict; future versions can be deleted, running ones not.
func TestPriceList_SetPriceVersioning(t *testing.T) {
	env := plNewEnv(t, "versioning")
	itemID := plCreateItem(t, env, "VER-ITEM", "consultation")
	compID := plCreateComponent(t, env, "VER-COMP")
	listID := plCreateList(t, env, "umum-ver", "", "", "")

	d0, d3, d5, d10 := plToday(env, 0), plToday(env, 3), plToday(env, 5), plToday(env, 10)
	if status, out := plSetPrice(t, env, listID, itemID, d0, map[string]string{compID: "100000.00"}); status != http.StatusOK {
		t.Fatalf("set v1: %d %v", status, out)
	}
	if status, out := plSetPrice(t, env, listID, itemID, d10, map[string]string{compID: "120000.00"}); status != http.StatusOK {
		t.Fatalf("set v2: %d %v", status, out)
	}
	// d5 falls inside v1 → 100000; d10 is v2 → 120000.
	if res := plResolveData(t, env, listID, itemID, d5); res["total"] != "100000.00" {
		t.Fatalf("resolve d5 = %v, want 100000.00", res["total"])
	}
	if res := plResolveData(t, env, listID, itemID, d10); res["total"] != "120000.00" {
		t.Fatalf("resolve d10 = %v, want 120000.00", res["total"])
	}
	// A version starting inside the latest one conflicts.
	status, out := plSetPrice(t, env, listID, itemID, d3, map[string]string{compID: "110000.00"})
	if status != http.StatusConflict || plErrorMsg(out) != "effective_from must be after the latest price version "+d10 {
		t.Fatalf("set d3 = %d %v, want 409 after %s", status, out, d10)
	}
	// Delete the future version; v1 reopens and covers d10 again.
	status, out = qeRequest(t, env, http.MethodDelete,
		"/api/v1/price-lists/"+listID+"/items/"+itemID+"/price?effective_from="+d10, nil)
	if status != http.StatusOK {
		t.Fatalf("delete future version = %d %v", status, out)
	}
	if res := plResolveData(t, env, listID, itemID, d10); res["total"] != "100000.00" {
		t.Fatalf("resolve d10 after delete = %v, want 100000.00", res["total"])
	}
	// The running version (today) cannot be deleted.
	status, out = qeRequest(t, env, http.MethodDelete,
		"/api/v1/price-lists/"+listID+"/items/"+itemID+"/price?effective_from="+d0, nil)
	if status != http.StatusConflict || plErrorMsg(out) != "only future price versions can be deleted" {
		t.Fatalf("delete running version = %d %v", status, out)
	}
}

// TestPriceList_RejectsPastEffectiveFrom — setting a price that starts in the
// merchant's past is a 409 with no new service_rate row; today is allowed.
func TestPriceList_RejectsPastEffectiveFrom(t *testing.T) {
	env := plNewEnv(t, "pastdate")
	ctx := context.Background()
	itemID := plCreateItem(t, env, "PAST-ITEM", "consultation")
	compID := plCreateComponent(t, env, "PAST-COMP")
	listID := plCreateList(t, env, "umum-past", "", "", "")

	countRates := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(ctx,
			"SELECT count(*) FROM core.service_rate WHERE price_list_id=$1 AND service_item_id=$2 AND deleted_at IS NULL",
			listID, itemID).Scan(&n); err != nil {
			t.Fatalf("count rates: %v", err)
		}
		return n
	}

	before := countRates()
	status, out := plSetPrice(t, env, listID, itemID, plToday(env, -1), map[string]string{compID: "50000.00"})
	if status != http.StatusConflict || plErrorMsg(out) != "effective_from cannot be in the past" {
		t.Fatalf("past effective_from = %d %v, want 409 'effective_from cannot be in the past'", status, out)
	}
	if n := countRates(); n != before {
		t.Fatalf("service_rate count = %d after 409, want %d", n, before)
	}
	status, out = plSetPrice(t, env, listID, itemID, plToday(env, 0), map[string]string{compID: "50000.00"})
	if status != http.StatusOK {
		t.Fatalf("today effective_from = %d %v, want 200", status, out)
	}
}

// TestPriceList_ConcurrentSetPrice — concurrent sets of the same item/list
// serialize on the advisory lock: exactly one open version, no overlaps.
func TestPriceList_ConcurrentSetPrice(t *testing.T) {
	env := plNewEnv(t, "concurrent")
	itemID := plCreateItem(t, env, "CONC-ITEM", "consultation")
	compID := plCreateComponent(t, env, "CONC-COMP")
	listID := plCreateList(t, env, "umum-conc", "", "", "")

	var wg sync.WaitGroup
	for i := 1; i <= 4; i++ {
		wg.Add(1)
		go func(day int) {
			defer wg.Done()
			plPut(env, "/api/v1/price-lists/"+listID+"/items/"+itemID+"/price", map[string]any{
				"effective_from": plToday(env, day),
				"components":     []map[string]string{{"rate_component_id": compID, "amount": "50000.00"}},
			})
		}(i)
	}
	wg.Wait()

	ctx := context.Background()
	var openCount, totalCount, overlapCount int
	if err := env.pool.QueryRow(ctx,
		"SELECT count(*) FROM core.service_rate WHERE price_list_id=$1 AND service_item_id=$2 AND deleted_at IS NULL AND effective_to IS NULL",
		listID, itemID).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		"SELECT count(*) FROM core.service_rate WHERE price_list_id=$1 AND service_item_id=$2 AND deleted_at IS NULL",
		listID, itemID).Scan(&totalCount); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM core.service_rate a JOIN core.service_rate b
		 ON a.price_list_id=b.price_list_id AND a.service_item_id=b.service_item_id AND a.id <> b.id
		 WHERE a.price_list_id=$1 AND a.service_item_id=$2
		   AND a.deleted_at IS NULL AND b.deleted_at IS NULL
		   AND a.effective_from < b.effective_from
		   AND (a.effective_to IS NULL OR a.effective_to > b.effective_from)`,
		listID, itemID).Scan(&overlapCount); err != nil {
		t.Fatalf("count overlaps: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("open versions = %d, want 1", openCount)
	}
	// How many versions land depends on serialization order (a later date
	// committing first makes earlier dates conflict); the invariants that
	// must hold are one open version, several rows, and no overlaps.
	if totalCount < 1 || totalCount > 4 {
		t.Fatalf("versions = %d, want between 1 and 4", totalCount)
	}
	if overlapCount != 0 {
		t.Fatalf("overlapping version pairs = %d, want 0", overlapCount)
	}
}

// TestPriceList_Missing — an unpriced item 404s price_missing on resolve and
// surfaces as source=missing with null total on the grid.
func TestPriceList_Missing(t *testing.T) {
	env := plNewEnv(t, "missing")
	itemID := plCreateItem(t, env, "MISS-ITEM", "consultation")
	listID := plCreateList(t, env, "umum-miss", "", "", "")

	status, out := plResolve(t, env, listID, itemID, plToday(env, 0))
	if status != http.StatusNotFound || plErrorMsg(out) != "price_missing" {
		t.Fatalf("resolve = %d %v, want 404 price_missing", status, out)
	}
	status, out = qeRequest(t, env, http.MethodGet,
		"/api/v1/price-lists/"+listID+"/items?date="+plToday(env, 0), nil)
	if status != http.StatusOK {
		t.Fatalf("items = %d %v", status, out)
	}
	rows, _ := out["data"].([]any)
	var found bool
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["item_id"] == itemID {
			found = true
			if row["source"] != "missing" || row["total"] != nil {
				t.Fatalf("missing row = %v, want source missing + null total", row)
			}
		}
	}
	if !found {
		t.Fatalf("item %s not in grid: %v", itemID, out)
	}
}

// TestPriceList_OneLevelAndDelete — no derivation chains, delete guards.
func TestPriceList_OneLevelAndDelete(t *testing.T) {
	env := plNewEnv(t, "onelevel")
	baseList, _, _, _ := plSeedPricedBase(t, env, "ol")
	derived := plCreateList(t, env, "ol-vip", baseList, "10.00", "")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/price-lists", map[string]any{
		"merchant_id": env.merchantID, "code": "ol-vip2", "name": "two levels",
		"base_price_list_id": derived, "adjustment_percent": "5.00",
	})
	if status != http.StatusConflict || plErrorMsg(out) != "price list base must be a base list (one level of derivation)" {
		t.Fatalf("derived-of-derived = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/price-lists/"+baseList, nil)
	if status != http.StatusConflict || plErrorMsg(out) != "price list is the base of other lists" {
		t.Fatalf("delete base with children = %d %v", status, out)
	}
	empty := plCreateList(t, env, "ol-empty", "", "", "")
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/price-lists/"+empty, nil)
	if status != http.StatusOK {
		t.Fatalf("delete empty list = %d %v", status, out)
	}
}

// TestPriceList_RequiresPermission — a user without core.tariff.manage gets
// 403 on create and list.
func TestPriceList_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "pl.perm", "081234571101")

	restrictedUserID := "6e6e6e6e-6e6e-6e6e-6e6e-6e6e6e6e6e6e"
	roleID := "6f6f6f6f-6f6f-6f6f-6f6f-6f6f6f6f6f6f"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Tariff')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'pl.noperm', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "pl.noperm", "pl-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	status, out := qeRequestToken(t, router, http.MethodPost, "/api/v1/price-lists", restrictedToken, companyID, merchantID,
		map[string]any{"merchant_id": merchantID, "code": "noperm", "name": "no perm"})
	if status != http.StatusForbidden {
		t.Fatalf("POST without permission = %d, want 403: %v", status, out)
	}
	status, out = qeRequestToken(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/price-lists", restrictedToken, companyID, merchantID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("GET without permission = %d, want 403: %v", status, out)
	}
}

// TestPriceList_OldServiceRateRoutesGone — the legacy flat service-rate route
// is retired in favor of per-list versioned prices.
func TestPriceList_OldServiceRateRoutesGone(t *testing.T) {
	env := plNewEnv(t, "oldroute")
	status, _ := qeRequest(t, env, http.MethodPost, "/api/v1/service-rates", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("POST /service-rates = %d, want 404", status)
	}
}

// TestPriceList_ItemICD9Validation — procedure_concept_id must be an active
// concept of the ICD-9-CM code system.
func TestPriceList_ItemICD9Validation(t *testing.T) {
	env := plNewEnv(t, "icd9")
	ctx := context.Background()

	// Seed the ICD-9-CM code system (idempotent) and one concept per test run.
	var systemID string
	if err := env.pool.QueryRow(ctx,
		`INSERT INTO terminology.code_system (system_uri, name, version)
		 VALUES ('http://hl7.org/fhir/sid/icd-9-cm', 'ICD-9-CM', 'test')
		 ON CONFLICT (system_uri) DO UPDATE SET name = EXCLUDED.name
		 RETURNING id::text`).Scan(&systemID); err != nil {
		t.Fatalf("seed icd-9-cm system: %v", err)
	}
	conceptCode := fmt.Sprintf("PL9-%06d", qePhoneSeq.Add(1))
	var conceptID string
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, $2, 'Test procedure') RETURNING id::text",
		systemID, conceptCode).Scan(&conceptID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/service-items", map[string]any{
		"merchant_id": env.merchantID, "code": "ICD9-OK", "name": "with icd9", "item_type": "procedure",
		"procedure_concept_id": conceptID,
	})
	plDataID(t, status, out, http.StatusCreated)

	// A concept from another code system must be rejected.
	var otherSystemID string
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test:pl-other', 'Other', '1') RETURNING id::text").Scan(&otherSystemID); err != nil {
		t.Fatalf("seed other system: %v", err)
	}
	var otherConceptID string
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, 'OTHER-1', 'Not ICD-9') RETURNING id::text",
		otherSystemID).Scan(&otherConceptID); err != nil {
		t.Fatalf("seed other concept: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/service-items", map[string]any{
		"merchant_id": env.merchantID, "code": "ICD9-BAD", "name": "wrong system", "item_type": "procedure",
		"procedure_concept_id": otherConceptID,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("other-system concept = %d %v, want 400", status, out)
	}
}
