//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestInventoryPricing_* covers the drug sell price proposal engine (spec
// 2026-10-07-tariff-c-drug-master-pricing §4.2): margin rules on base lists,
// proposals from the reference price (100000 cents ÷ box 100 × 1.20 =
// 1200 cents = "1200.00"), approval writing a BARANG price version,
// same-day re-approval moving to tomorrow, supersede/no-op, auto approve,
// the HET cap, derived list rejection and permissions.

// ipEnv is one pricing test environment: a merchant with a base price list
// "umum", a generic drug item with a box-100 packaging in the formulary.
type ipEnv struct {
	pool          *pgxpool.Pool
	router        *gin.Engine
	companyID     string
	merchantID    string
	token         string
	listID        string
	entryID       string
	boxRowID      string
	serviceItemID string
	itemID        string
}

// ipToday is today (offsetDays applied) in Jakarta as YYYY-MM-DD.
func ipToday(offsetDays int) string {
	y, m, d := time.Now().In(mustLoc(plTZ)).AddDate(0, 0, offsetDays).Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

var ipKfaSeq int64 = 930001000

// ipSetup seeds company/merchant, a generic drug with a box-100 packaging,
// formulary activation and the base price list "umum".
func ipSetup(t *testing.T, label, phone string, itemExtra map[string]any) *ipEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, token := invSeed(t, ctx, pool, router, label, phone)
	tablet := invUnitID(t, ctx, pool, "tablet")
	box := invUnitID(t, ctx, pool, "box")

	ipKfaSeq++
	code := "IP-" + strings.ToUpper(label)
	extra := map[string]any{"base_unit_id": tablet, "kfa_code": fmt.Sprintf("%d", ipKfaSeq)}
	for k, v := range itemExtra {
		extra[k] = v
	}
	_, out, itemID := invCreateItem(t, router, token, companyID, merchantID, code, extra)
	if itemID == "" {
		t.Fatalf("missing item id: %v", out)
	}

	// Box-100 packaging.
	raw, _ := json.Marshal(map[string]any{"unit_id": box, "factor": "100"})
	rec := accountRequest(router, http.MethodPost, "/api/v1/inventory/items/"+itemID+"/units", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add box expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	status, outGet := invGet(t, router, token, companyID, merchantID, itemID)
	data := invDetail(t, status, outGet, http.StatusOK)
	var boxRowID string
	for _, u := range data["units"].([]any) {
		row := u.(map[string]any)
		if row["unit"].(map[string]any)["code"] == "box" {
			boxRowID = row["id"].(string)
		}
	}
	if boxRowID == "" {
		t.Fatalf("box packaging row missing: %v", data)
	}

	_, outForm, entryID := formAdd(t, router, token, companyID, merchantID, itemID)
	if entryID == "" {
		t.Fatalf("missing formulary entry id: %v", outForm)
	}
	serviceItemID, _ := outForm["data"].(map[string]any)["service_item_id"].(string)

	// Reference price "100000" on the box-100 packaging (before any margin
	// rule exists, so no proposal is generated yet).
	raw, _ = json.Marshal(map[string]any{"amount": "100000", "item_unit_id": boxRowID})
	rec = accountRequest(router, http.MethodPut, "/api/v1/formulary/"+entryID+"/reference-price", token, companyID, merchantID, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("set reference price expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Base price list "umum" via the tariff-a API.
	raw, _ = json.Marshal(map[string]any{"merchant_id": merchantID, "code": "umum", "name": "Daftar umum"})
	rec = accountRequest(router, http.MethodPost, "/api/v1/price-lists", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create list expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	listOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &listOut)
	listID := plDataID(t, http.StatusCreated, listOut, http.StatusCreated)

	return &ipEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID,
		token: token, listID: listID, entryID: entryID, boxRowID: boxRowID,
		serviceItemID: serviceItemID, itemID: itemID}
}

// ipRequest sends a tenant request as the owner and decodes the JSON body.
func ipRequest(t *testing.T, e *ipEnv, method, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := accountRequest(e.router, method, path, e.token, e.companyID, e.merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ipPutMargin replaces the margin rules of the list.
func ipPutMargin(t *testing.T, e *ipEnv, rules []map[string]any) (int, map[string]any) {
	t.Helper()
	return ipRequest(t, e, http.MethodPut, "/api/v1/price-lists/"+e.listID+"/margin-rules", map[string]any{"rules": rules})
}

// ipSetReference sets the reference price on the box packaging.
func ipSetReference(t *testing.T, e *ipEnv, amount string) (int, map[string]any) {
	t.Helper()
	return ipRequest(t, e, http.MethodPut, "/api/v1/formulary/"+e.entryID+"/reference-price",
		map[string]any{"amount": amount, "item_unit_id": e.boxRowID})
}

// ipProposals lists proposals with the given query string.
func ipProposals(t *testing.T, e *ipEnv, query string) []any {
	t.Helper()
	status, out := ipRequest(t, e, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/price-proposals"+query, nil)
	if status != http.StatusOK {
		t.Fatalf("list proposals expected 200, got %d: %v", status, out)
	}
	data, _ := out["data"].([]any)
	return data
}

// ipProposalRow asserts the pending list has n entries and returns the first.
func ipProposalRow(t *testing.T, e *ipEnv, query string, n int) map[string]any {
	t.Helper()
	data := ipProposals(t, e, query)
	if len(data) != n {
		t.Fatalf("proposals = %d rows, want %d: %v", len(data), n, data)
	}
	if n == 0 {
		return nil
	}
	return data[0].(map[string]any)
}

// ipResolve resolves the item price on a date and returns the data object.
func ipResolve(t *testing.T, e *ipEnv, date string, want int) map[string]any {
	t.Helper()
	status, out := ipRequest(t, e, http.MethodGet,
		"/api/v1/price-lists/"+e.listID+"/resolve?item_id="+e.serviceItemID+"&date="+date, nil)
	if status != want {
		t.Fatalf("resolve expected %d, got %d: %v", want, status, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

// TestInventoryPricing_ProposalFromReference — PUT margin generic 20% on the
// reference 100000/box100 creates a pending proposal "1200.00" with no
// current price; approval writes a BARANG version effective today.
func TestInventoryPricing_ProposalFromReference(t *testing.T) {
	e := ipSetup(t, "prop", "081234620001", nil)
	status, out := ipPutMargin(t, e, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}})
	if status != http.StatusOK {
		t.Fatalf("put margin expected 200, got %d: %v", status, out)
	}
	row := ipProposalRow(t, e, "", 1)
	if row["proposed_amount"] != "1200.00" || row["current_amount"] != nil || row["status"] != "pending" {
		t.Fatalf("proposal = %v", row)
	}
	id := row["id"].(string)

	status, out = ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+id+"/approve", nil)
	if status != http.StatusOK {
		t.Fatalf("approve expected 200, got %d: %v", status, out)
	}
	decided, _ := out["data"].(map[string]any)
	if decided["status"] != "approved" || decided["effective_from"] != ipToday(0) {
		t.Fatalf("approved proposal = %v", decided)
	}
	res := ipResolve(t, e, ipToday(0), http.StatusOK)
	if res["total"] != "1200.00" {
		t.Fatalf("resolve total = %v, want 1200.00", res["total"])
	}
	comps := res["components"].([]any)
	if len(comps) != 1 || comps[0].(map[string]any)["code"] != "BARANG" {
		t.Fatalf("components = %v", comps)
	}
}

// TestInventoryPricing_SecondApprovalSameDay — after approving today, a new
// reference creates a second proposal; its approval is effective tomorrow
// while today still resolves the old price.
func TestInventoryPricing_SecondApprovalSameDay(t *testing.T) {
	e := ipSetup(t, "second", "081234620002", nil)
	if status, out := ipPutMargin(t, e, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}}); status != http.StatusOK {
		t.Fatalf("put margin: %d %v", status, out)
	}
	first := ipProposalRow(t, e, "", 1)
	if status, out := ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+first["id"].(string)+"/approve", nil); status != http.StatusOK {
		t.Fatalf("first approve: %d %v", status, out)
	}

	// 110000 ÷ 100 × 1.2 = 1320.
	if status, out := ipSetReference(t, e, "110000"); status != http.StatusOK {
		t.Fatalf("set reference: %d %v", status, out)
	}
	row := ipProposalRow(t, e, "", 1)
	if row["proposed_amount"] != "1320.00" || row["current_amount"] != "1200.00" {
		t.Fatalf("second proposal = %v", row)
	}
	status, out := ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+row["id"].(string)+"/approve", nil)
	if status != http.StatusOK {
		t.Fatalf("second approve: %d %v", status, out)
	}
	decided, _ := out["data"].(map[string]any)
	if decided["effective_from"] != ipToday(1) {
		t.Fatalf("effective_from = %v, want tomorrow %s", decided["effective_from"], ipToday(1))
	}
	if res := ipResolve(t, e, ipToday(0), http.StatusOK); res["total"] != "1200.00" {
		t.Fatalf("today total = %v, want 1200.00", res["total"])
	}
	if res := ipResolve(t, e, ipToday(1), http.StatusOK); res["total"] != "1320.00" {
		t.Fatalf("tomorrow total = %v, want 1320.00", res["total"])
	}
}

// TestInventoryPricing_SupersedeAndNoop — regenerating without approval
// supersedes the old pending proposal; when the proposed price equals the
// current price no new proposal is created.
func TestInventoryPricing_SupersedeAndNoop(t *testing.T) {
	e := ipSetup(t, "supers", "081234620003", nil)
	if status, out := ipPutMargin(t, e, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}}); status != http.StatusOK {
		t.Fatalf("put margin: %d %v", status, out)
	}
	old := ipProposalRow(t, e, "", 1)

	// Second regeneration: the first pending becomes superseded.
	if status, out := ipSetReference(t, e, "105000"); status != http.StatusOK {
		t.Fatalf("set reference 2: %d %v", status, out)
	}
	row := ipProposalRow(t, e, "", 1)
	if row["id"] == old["id"] {
		t.Fatalf("pending proposal was not replaced")
	}
	superseded := ipProposalRow(t, e, "?status=superseded", 1)
	if superseded["id"] != old["id"] {
		t.Fatalf("superseded = %v, want %v", superseded["id"], old["id"])
	}

	// Approve, then regenerate with the SAME reference → no new pending
	// (105000 ÷ 100 × 1.2 = 1260 = current price).
	if status, out := ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+row["id"].(string)+"/approve", nil); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	if status, out := ipSetReference(t, e, "105000"); status != http.StatusOK {
		t.Fatalf("set reference 3: %d %v", status, out)
	}
	ipProposalRow(t, e, "", 0)
}

// TestInventoryPricing_AutoApprove — an auto_approve rule applies the price
// immediately with status auto_applied; nothing stays pending.
func TestInventoryPricing_AutoApprove(t *testing.T) {
	e := ipSetup(t, "auto", "081234620004", nil)
	status, out := ipPutMargin(t, e, []map[string]any{{
		"pricing_category": "generic", "margin_percent": "20.00", "auto_approve": true,
	}})
	if status != http.StatusOK {
		t.Fatalf("put margin: %d %v", status, out)
	}
	ipProposalRow(t, e, "", 0)
	applied := ipProposalRow(t, e, "?status=auto_applied", 1)
	if applied["proposed_amount"] != "1200.00" {
		t.Fatalf("auto applied = %v", applied)
	}
	if res := ipResolve(t, e, ipToday(0), http.StatusOK); res["total"] != "1200.00" {
		t.Fatalf("resolve total = %v, want 1200.00", res["total"])
	}
}

// TestInventoryPricing_HETCap — a HET of 1000 effective since yesterday caps
// the 1200 proposal at "1000.00" with het_capped true.
func TestInventoryPricing_HETCap(t *testing.T) {
	e := ipSetup(t, "het", "081234620005", map[string]any{
		"het_amount": "1000", "het_effective_from": ipToday(-1),
	})
	if status, out := ipPutMargin(t, e, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}}); status != http.StatusOK {
		t.Fatalf("put margin: %d %v", status, out)
	}
	row := ipProposalRow(t, e, "", 1)
	if row["proposed_amount"] != "1000.00" || row["het_capped"] != true {
		t.Fatalf("capped proposal = %v", row)
	}
}

// TestInventoryPricing_DerivedListRejected — margin rules are rejected on a
// derived list; the defaults fill on the base list yields 20% (oncology 2%).
func TestInventoryPricing_DerivedListRejected(t *testing.T) {
	e := ipSetup(t, "derived", "081234620006", nil)
	// Derived list on top of "umum".
	raw, _ := json.Marshal(map[string]any{
		"merchant_id": e.merchantID, "code": "turunan", "name": "Daftar turunan",
		"base_price_list_id": e.listID, "adjustment_percent": "30.00", "rounding_unit": "0.00",
	})
	rec := accountRequest(e.router, http.MethodPost, "/api/v1/price-lists", e.token, e.companyID, e.merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create derived list expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	derivedOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &derivedOut)
	derivedID := plDataID(t, http.StatusCreated, derivedOut, http.StatusCreated)

	derivEnv := &ipEnv{router: e.router, companyID: e.companyID, merchantID: e.merchantID, token: e.token, listID: derivedID}
	status, out := ipPutMargin(t, derivEnv, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}})
	if status != http.StatusConflict {
		t.Fatalf("derived put margin expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "margin rules apply to base or standalone lists only" {
		t.Fatalf("error = %q", msg)
	}
	status, out = ipRequest(t, derivEnv, http.MethodPost, "/api/v1/price-lists/"+derivedID+"/margin-rules/defaults", nil)
	if status != http.StatusConflict {
		t.Fatalf("derived defaults expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "margin rules apply to base or standalone lists only" {
		t.Fatalf("error = %q", msg)
	}

	// Defaults on the base list: 8 rules, oncology "2.00", others "20.00".
	status, out = ipRequest(t, e, http.MethodPost, "/api/v1/price-lists/"+e.listID+"/margin-rules/defaults", nil)
	if status != http.StatusOK {
		t.Fatalf("defaults expected 200, got %d: %v", status, out)
	}
	rules, _ := out["data"].([]any)
	if len(rules) != 8 {
		t.Fatalf("defaults = %d rules, want 8: %v", len(rules), out)
	}
	for _, r := range rules {
		rule := r.(map[string]any)
		want := "20.00"
		if rule["pricing_category"] == "oncology" {
			want = "2.00"
		}
		if rule["margin_percent"] != want {
			t.Fatalf("rule %v margin = %v, want %s", rule["pricing_category"], rule["margin_percent"], want)
		}
	}
}

// TestInventoryPricing_RejectAndPermission — rejection leaves the price
// unchanged, approving a rejected proposal 409s, and a user without
// core.tariff.manage gets 403 on the proposal list.
func TestInventoryPricing_RejectAndPermission(t *testing.T) {
	e := ipSetup(t, "reject", "081234620007", nil)
	if status, out := ipPutMargin(t, e, []map[string]any{{"pricing_category": "generic", "margin_percent": "20.00"}}); status != http.StatusOK {
		t.Fatalf("put margin: %d %v", status, out)
	}
	row := ipProposalRow(t, e, "", 1)
	status, out := ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+row["id"].(string)+"/reject", nil)
	if status != http.StatusOK {
		t.Fatalf("reject expected 200, got %d: %v", status, out)
	}
	decided, _ := out["data"].(map[string]any)
	if decided["status"] != "rejected" {
		t.Fatalf("rejected proposal = %v", decided)
	}
	// No price version was written: resolve has nothing to find.
	ipResolve(t, e, ipToday(0), http.StatusNotFound)

	status, out = ipRequest(t, e, http.MethodPost, "/api/v1/price-proposals/"+row["id"].(string)+"/approve", nil)
	if status != http.StatusConflict {
		t.Fatalf("approve rejected expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "proposal is not pending" {
		t.Fatalf("error = %q", msg)
	}

	// A user with a role lacking core.tariff.manage → 403 on the list.
	ctx := context.Background()
	restrictedUserID := "6a6a6a6a-6a6a-6a6a-6a6a-6a6a6a6a6a6a"
	roleID := "6b6b6b6b-6b6b-6b6b-6b6b-6b6b6b6b6b6b"
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Tariff Pricing')", roleID, e.companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'ip.noperm', $3, true)", restrictedUserID, e.companyID, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, e.merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, e.companyID, e.merchantID, "", "ip.noperm", "ip-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	rec := accountRequest(e.router, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/price-proposals", restrictedToken, e.companyID, e.merchantID, nil)
	if rec.Code != http.StatusForbidden {
		out403 := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out403)
		t.Fatalf("list without permission = %d, want 403: %v", rec.Code, out403)
	}
}
