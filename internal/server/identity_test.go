//go:build integration

package server

// Integration tests for P3 identity & packaging (plan
// 2026-10-20-inventory-17b-p3-identity-packaging Task 3, spec
// 2026-10-20-inventory-17b-procurement-special-cases §6): consignment usage
// pricing/balances, serial issue/transfer enforcement and repack posting/void
// with carried cost. The functions under test are unexported and called
// directly inside a runtime transaction (pola stock_ledger_test.go); the
// container/tenant helpers live in stock_ledger_test.go and are reused.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// identitySeedSupplier inserts a supplier for the tenant.
func identitySeedSupplier(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, code, name string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		w.companyStr, w.merchantStr, code, name).Scan(&id); err != nil {
		t.Fatalf("insert supplier %s: %v", code, err)
	}
	return id
}

// identitySeedUnit resolves a catalog unit code (inventory.unit is a seeded
// global catalog, read-only for app_runtime).
func identitySeedUnit(t *testing.T, ctx context.Context, tx pgx.Tx, code string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM inventory.unit WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("resolve unit %s: %v", code, err)
	}
	return id
}

// identitySeedItem inserts an item with an explicit track_serial flag.
func identitySeedItem(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, code, name string, trackSerial bool, baseUnitCode string) pgtype.UUID {
	t.Helper()
	baseUnit := identitySeedUnit(t, ctx, tx, baseUnitCode)
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class, track_serial)
		VALUES ($1,$2,$3,'drug','generic',$4,'93100090','keras',$5) RETURNING id`,
		w.companyStr, code, name, baseUnit, trackSerial).Scan(&id); err != nil {
		t.Fatalf("insert item %s: %v", code, err)
	}
	return id
}

// identitySeedItemUnit links an item to a unit with the given factor.
func identitySeedItemUnit(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID, unitID pgtype.UUID, factor string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item_unit (company_id, item_id, unit_id, factor)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		w.companyStr, itemID, unitID, factor).Scan(&id); err != nil {
		t.Fatalf("insert item_unit: %v", err)
	}
	return id
}

// identitySeedContract inserts a consignment-kind contract price valid from
// long before the test day.
func identitySeedContract(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, supplierID, itemID, itemUnitID pgtype.UUID, packPrice string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.contract_price
			(company_id, supplier_id, item_id, item_unit_id, kind, pack_price, valid_from)
		VALUES ($1,$2,$3,$4,'consignment',$5,'2026-01-01')`,
		w.companyStr, supplierID, itemID, itemUnitID, packPrice); err != nil {
		t.Fatalf("insert contract price: %v", err)
	}
}

// identitySeedLot inserts a lot for an arbitrary item.
func identitySeedLot(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID pgtype.UUID, batch string, expiry *time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		w.companyStr, itemID, batch, expiry).Scan(&id); err != nil {
		t.Fatalf("insert lot %s: %v", batch, err)
	}
	return id
}

// identitySeedBalance adds qty (milli-units) to one balance via
// applyStockMove, with the consignment supplier dimension when given.
func identitySeedBalance(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID, lotID pgtype.UUID, source, status string, qtyMilli int64, supplier pgtype.UUID) {
	t.Helper()
	q := sqlcgen.New(tx)
	if _, err := applyStockMove(ctx, q, w.merchant, w.company, w.user, stockMoveInput{
		Key:        stockKey{LocationID: w.location, LotID: lotID, Source: source, Status: status, SupplierID: supplier},
		ItemID:     itemID,
		Qty:        numericFromMilli(qtyMilli),
		UnitCost:   pgtype.Numeric{Valid: true},
		MoveType:   "receipt",
		MoveDate:   w.day,
		ReceivedAt: w.day,
	}); err != nil {
		t.Fatalf("seed balance %s/%s: %v", source, status, err)
	}
}

// identitySetAvgCost pins an item's average cost (inventory.item_cost).
func identitySetAvgCost(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID pgtype.UUID, cost string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.item_cost (company_id, merchant_id, item_id, avg_cost)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (merchant_id, item_id) DO UPDATE SET avg_cost = EXCLUDED.avg_cost`,
		w.companyStr, w.merchantStr, itemID, cost); err != nil {
		t.Fatalf("set avg cost: %v", err)
	}
}

// identitySeedLocation inserts an extra location (repack needs two).
func identitySeedLocation(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, code, name string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room',$3,$4, ARRAY['warehouse']::text[]) RETURNING id`,
		w.companyStr, w.merchantStr, code, name).Scan(&id); err != nil {
		t.Fatalf("insert location %s: %v", code, err)
	}
	return id
}

// identitySerialsAt counts serial rows of an item by status at a location
// (zero location = issued/destroyed/returned rows).
func identitySerialsAt(t *testing.T, ctx context.Context, tx pgx.Tx, itemID pgtype.UUID, status string, locationID pgtype.UUID) int64 {
	t.Helper()
	var n int64
	var err error
	if locationID.Valid {
		err = tx.QueryRow(ctx, `
			SELECT count(*) FROM inventory.serial
			WHERE item_id = $1 AND status = $2 AND location_id = $3`,
			itemID, status, locationID).Scan(&n)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT count(*) FROM inventory.serial
			WHERE item_id = $1 AND status = $2 AND location_id IS NULL`,
			itemID, status).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count serials: %v", err)
	}
	return n
}

// TestConsignmentUsageRecorded: a consignment issue writes consignment_usage
// priced from the contract (pack_price ÷ pack_content, 6 decimals).
func TestConsignmentUsageRecorded(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identcons1", "081234573101", "Klinik Konsinyasi 1")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	supplier := identitySeedSupplier(t, ctx, tx, w, "SUP-1", "PT Konsinyasi Satu")
	packUnit := identitySeedUnit(t, ctx, tx, "tablet")
	// issueStock keys the contract on the item's base unit; pack_content is
	// that unit's factor (stock_ledger.go phase 2).
	packUnitID := identitySeedItemUnit(t, ctx, tx, w, w.item, packUnit, "10")
	// Contract: "9000.00" per pack of 10 → unit_price "900.000000".
	identitySeedContract(t, ctx, tx, w, supplier, w.item, packUnitID, "9000.00")
	ed := w.day.AddDate(0, 1, 0)
	lot := identitySeedLot(t, ctx, tx, w, w.item, "B-CONS", &ed)
	identitySeedBalance(t, ctx, tx, w, w.item, lot, "consignment", "available", 10_000, supplier)

	// Expected (contract example): issue 4 of 10 → usage qty "4.000",
	// unit_price "900.000000", value 4 × 900 = "3600.000000".
	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(4_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil {
		t.Fatalf("issueStock: %v", err)
	}
	if len(parts) != 1 || parts[0].Source != "consignment" {
		t.Fatalf("expected one consignment part, got %+v", parts)
	}
	var qty, unitPrice string
	if err := tx.QueryRow(ctx, `
		SELECT base_qty::text, unit_price::text FROM inventory.consignment_usage`).Scan(&qty, &unitPrice); err != nil {
		t.Fatalf("read consignment usage: %v", err)
	}
	if qty != "4.000" || unitPrice != "900.000000" {
		t.Fatalf("usage qty/price = %s/%s, want 4.000/900.000000", qty, unitPrice)
	}
	var value bool
	if err := tx.QueryRow(ctx, `
		SELECT SUM(base_qty * unit_price) = 3600::numeric FROM inventory.consignment_usage`).Scan(&value); err != nil {
		t.Fatalf("sum usage value: %v", err)
	}
	if !value {
		t.Fatalf("usage value must be 3600")
	}
}

// TestConsignmentPriceMissing: an issue touching a consignment balance
// without a valid contract is rejected before anything is written.
func TestConsignmentPriceMissing(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identcons2", "081234573102", "Klinik Konsinyasi 2")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	supplier := identitySeedSupplier(t, ctx, tx, w, "SUP-NF", "PT Tanpa Kontrak")
	ed := w.day.AddDate(0, 1, 0)
	lot := identitySeedLot(t, ctx, tx, w, w.item, "B-NOPRICE", &ed)
	identitySeedBalance(t, ctx, tx, w, w.item, lot, "consignment", "available", 10_000, supplier)

	var movesBefore, usageBefore int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory.stock_move WHERE move_type = 'issue'`).Scan(&movesBefore); err != nil {
		t.Fatalf("count issue moves: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory.consignment_usage`).Scan(&usageBefore); err != nil {
		t.Fatalf("count usage: %v", err)
	}

	if _, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(4_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, ""); !errors.Is(err, errConsignmentPriceMissing) {
		t.Fatalf("expected errConsignmentPriceMissing, got %v", err)
	}
	// 409 mapping with the fixed message.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if !respondStockError(c, errConsignmentPriceMissing) ||
		rec.Code != http.StatusConflict ||
		rec.Body.String() != `{"error":"consignment price missing"}` {
		t.Fatalf("consignment price missing response = %d %s", rec.Code, rec.Body.String())
	}

	var movesAfter, usageAfter int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory.stock_move WHERE move_type = 'issue'`).Scan(&movesAfter); err != nil {
		t.Fatalf("count issue moves after: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory.consignment_usage`).Scan(&usageAfter); err != nil {
		t.Fatalf("count usage after: %v", err)
	}
	if movesAfter != movesBefore || usageAfter != usageBefore {
		t.Fatalf("rejected issue must write nothing: moves %d→%d usage %d→%d",
			movesBefore, movesAfter, usageBefore, usageAfter)
	}
}

// TestConsignmentBalanceBySupplier: two suppliers on the same item+lot are
// separate balance rows, and FEFO draws non-consignment before consignment.
func TestConsignmentBalanceBySupplier(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identcons3", "081234573103", "Klinik Konsinyasi 3")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	s1 := identitySeedSupplier(t, ctx, tx, w, "SUP-S1", "PT S1")
	s2 := identitySeedSupplier(t, ctx, tx, w, "SUP-S2", "PT S2")
	packUnit := identitySeedUnit(t, ctx, tx, "tablet")
	packUnitID := identitySeedItemUnit(t, ctx, tx, w, w.item, packUnit, "10")
	identitySeedContract(t, ctx, tx, w, s1, w.item, packUnitID, "9000.00")
	identitySeedContract(t, ctx, tx, w, s2, w.item, packUnitID, "9000.00")
	ed := w.day.AddDate(0, 1, 0)
	lot := identitySeedLot(t, ctx, tx, w, w.item, "B-SHARED", &ed)
	identitySeedBalance(t, ctx, tx, w, w.item, lot, "purchase", "available", 5_000, pgtype.UUID{})
	identitySeedBalance(t, ctx, tx, w, w.item, lot, "consignment", "available", 3_000, s1)
	identitySeedBalance(t, ctx, tx, w, w.item, lot, "consignment", "available", 3_000, s2)

	var consignmentRows int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM inventory.stock_balance
		WHERE lot_id = $1 AND source = 'consignment' AND status = 'available'`, lot).Scan(&consignmentRows); err != nil {
		t.Fatalf("count consignment balances: %v", err)
	}
	if consignmentRows != 2 {
		t.Fatalf("same item+lot with two suppliers must be 2 balance rows, got %d", consignmentRows)
	}

	// Issue 8: 5 from the purchase balance, 3 from the consignment rows
	// (drawn last per spec §4.4).
	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(8_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil {
		t.Fatalf("issueStock: %v", err)
	}
	var purchaseMilli, consignmentMilli int64
	for _, p := range parts {
		m, _ := numericToMilli(p.Qty)
		switch p.Source {
		case "purchase":
			purchaseMilli += m
		case "consignment":
			consignmentMilli += m
		}
	}
	if purchaseMilli != 5_000 || consignmentMilli != 3_000 {
		t.Fatalf("expected purchase 5000 + consignment 3000 milli, got %d + %d", purchaseMilli, consignmentMilli)
	}
	var consignmentLeft int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint), 0) FROM inventory.stock_balance
		WHERE lot_id = $1 AND source = 'consignment'`, lot).Scan(&consignmentLeft); err != nil {
		t.Fatalf("sum consignment left: %v", err)
	}
	if consignmentLeft != 3_000 {
		t.Fatalf("consignment left must be 3000 milli, got %d", consignmentLeft)
	}
}

// TestSerialReceiveAndEnforce: receive-registered serials flip to issued with
// their ref; wrong location and wrong quantity are rejected.
func TestSerialReceiveAndEnforce(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identsn1", "081234573104", "Klinik Serial 1")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	item := identitySeedItem(t, ctx, tx, w, "ITM-SN", "Implan Seri", true, "tablet")
	// Penerimaan: 3 serial numbers registered in_stock at the location (pola
	// posting GR Task 2).
	serialNos := []string{"SN-001", "SN-002", "SN-003"}
	for _, sn := range serialNos {
		if _, err := q.InsertSerial(ctx, sqlcgen.InsertSerialParams{
			CompanyID: w.company, MerchantID: w.merchant,
			ItemID: item, SerialNo: sn, LocationID: w.location, CreatedBy: w.user,
		}); err != nil {
			t.Fatalf("insert serial %s: %v", sn, err)
		}
	}
	if n := identitySerialsAt(t, ctx, tx, item, "in_stock", w.location); n != 3 {
		t.Fatalf("expected 3 in_stock serials, got %d", n)
	}

	// enforce issue of 2 → issued with the usage ref, location cleared.
	ref := stockRef{RefType: "usage"}
	if err := enforceSerialIssue(ctx, q, w.merchant, w.location, item,
		[]string{"SN-001", "SN-002"}, "issued", ref); err != nil {
		t.Fatalf("enforceSerialIssue: %v", err)
	}
	if n := identitySerialsAt(t, ctx, tx, item, "issued", pgtype.UUID{}); n != 2 {
		t.Fatalf("expected 2 issued serials, got %d", n)
	}
	var refType string
	var refID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		SELECT issued_ref_type, issued_ref_id FROM inventory.serial
		WHERE item_id = $1 AND serial_no = 'SN-001'`, item).Scan(&refType, &refID); err != nil {
		t.Fatalf("read issued serial: %v", err)
	}
	if refType != "usage" {
		t.Fatalf("issued_ref_type = %s, want usage", refType)
	}

	// Wrong location: the remaining in-stock serial sits at w.location.
	otherLoc := identitySeedLocation(t, ctx, tx, w, "GUD-2", "Gudang Kedua")
	if err := enforceSerialIssue(ctx, q, w.merchant, otherLoc, item,
		[]string{"SN-003"}, "issued", ref); !errors.Is(err, errSerialNotInStock) {
		t.Fatalf("expected errSerialNotInStock for wrong location, got %v", err)
	}
	// Unknown serial in the request → fetched count ≠ requested count.
	if err := enforceSerialIssue(ctx, q, w.merchant, w.location, item,
		[]string{"SN-003", "SN-GONE"}, "issued", ref); !errors.Is(err, errSerialQuantityMismat) {
		t.Fatalf("expected errSerialQuantityMismat, got %v", err)
	}
}

// TestSerialMovesWithTransfer: a location transfer moves the serials with the
// stock; the status stays in_stock.
func TestSerialMovesWithTransfer(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identsn2", "081234573105", "Klinik Serial 2")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	item := identitySeedItem(t, ctx, tx, w, "ITM-SN2", "Implan Seri 2", true, "tablet")
	toLocation := identitySeedLocation(t, ctx, tx, w, "GUD-3", "Gudang Tujuan")
	for _, sn := range []string{"SN-T1", "SN-T2"} {
		if _, err := q.InsertSerial(ctx, sqlcgen.InsertSerialParams{
			CompanyID: w.company, MerchantID: w.merchant,
			ItemID: item, SerialNo: sn, LocationID: w.location, CreatedBy: w.user,
		}); err != nil {
			t.Fatalf("insert serial %s: %v", sn, err)
		}
	}
	if err := moveSerialsForTransfer(ctx, q, w.merchant, w.merchant, toLocation, item,
		[]string{"SN-T1", "SN-T2"}); err != nil {
		t.Fatalf("moveSerialsForTransfer: %v", err)
	}
	if n := identitySerialsAt(t, ctx, tx, item, "in_stock", toLocation); n != 2 {
		t.Fatalf("expected 2 in_stock serials at the target location, got %d", n)
	}
	if n := identitySerialsAt(t, ctx, tx, item, "in_stock", w.location); n != 0 {
		t.Fatalf("source location must be empty, got %d", n)
	}
}

// identityRepackFixture seeds the contract repack example: item A avg cost 50
// with 10 units, item B avg cost 80 with 5 units, item C as the repack
// output; returns the draft repack, the items and the second location.
func identityRepackFixture(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld) (sqlcgen.InventoryRepack, pgtype.UUID, pgtype.UUID, pgtype.UUID, pgtype.UUID, *sqlcgen.Queries) {
	t.Helper()
	q := sqlcgen.New(tx)
	itemA := identitySeedItem(t, ctx, tx, w, "ITM-RPA", "Repack A", false, "tablet")
	itemB := identitySeedItem(t, ctx, tx, w, "ITM-RPB", "Repack B", false, "tablet")
	itemC := identitySeedItem(t, ctx, tx, w, "ITM-RPC", "Repack C", false, "tablet")
	identitySetAvgCost(t, ctx, tx, w, itemA, "50.000000")
	identitySetAvgCost(t, ctx, tx, w, itemB, "80.000000")
	inLocation := identitySeedLocation(t, ctx, tx, w, "PACK-1", "Ruang Kemas")
	ed := w.day.AddDate(0, 2, 0)
	lotA := identitySeedLot(t, ctx, tx, w, itemA, "B-RA", &ed)
	lotB := identitySeedLot(t, ctx, tx, w, itemB, "B-RB", &ed)
	identitySeedBalance(t, ctx, tx, w, itemA, lotA, "purchase", "available", 10_000, pgtype.UUID{})
	identitySeedBalance(t, ctx, tx, w, itemB, lotB, "purchase", "available", 5_000, pgtype.UUID{})

	r, err := q.InsertRepack(ctx, sqlcgen.InsertRepackParams{
		CompanyID: w.company, MerchantID: w.merchant,
		OutLocationID: w.location, InLocationID: inLocation,
		Note: pgText("repack kontrak"), CreatedBy: w.user,
	})
	if err != nil {
		t.Fatalf("insert repack: %v", err)
	}
	// Out: 10 of A + 5 of B → value 10×50 + 5×80 = 900.
	if _, err := q.InsertRepackOutLine(ctx, sqlcgen.InsertRepackOutLineParams{
		CompanyID: w.company, MerchantID: w.merchant, RepackID: r.ID,
		ItemID: itemA, LotID: lotA, Source: "purchase",
		BaseQty: numericFromMilli(10_000), CreatedBy: w.user,
	}); err != nil {
		t.Fatalf("insert out line A: %v", err)
	}
	if _, err := q.InsertRepackOutLine(ctx, sqlcgen.InsertRepackOutLineParams{
		CompanyID: w.company, MerchantID: w.merchant, RepackID: r.ID,
		ItemID: itemB, LotID: lotB, Source: "purchase",
		BaseQty: numericFromMilli(5_000), CreatedBy: w.user,
	}); err != nil {
		t.Fatalf("insert out line B: %v", err)
	}
	// In: 15 base units of C (one in line; pack fields 15×1). The line's
	// item_unit row for C's base unit carries factor 1.
	cBaseUnitID := identitySeedItemUnit(t, ctx, tx, w, itemC,
		identitySeedUnit(t, ctx, tx, "tablet"), "1")
	rcBatch := "B-RC"
	rcExpiry := w.day.AddDate(0, 3, 0)
	if _, err := q.InsertRepackInLine(ctx, sqlcgen.InsertRepackInLineParams{
		CompanyID: w.company, MerchantID: w.merchant, RepackID: r.ID,
		ItemID: itemC, ItemUnitID: cBaseUnitID,
		PackQty: numericFromMilli(15_000), PackContent: numericFromMilli(1_000),
		BatchNo:    pgtype.Text{String: rcBatch, Valid: true},
		ExpiryDate: pgtype.Date{Time: rcExpiry, Valid: true},
		BaseQty:    numericFromMilli(15_000), CreatedBy: w.user,
	}); err != nil {
		t.Fatalf("insert in line C: %v", err)
	}
	return r, itemA, itemB, itemC, inLocation, q
}

// TestRepackCostCarried: the repack_in unit cost carries the full out value
// (900 ÷ 15 = "60.000000") and the HPP (item_cost) stays untouched.
func TestRepackCostCarried(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identrp1", "081234573106", "Klinik Repack 1")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	r, itemA, itemB, itemC, _, q := identityRepackFixture(t, ctx, tx, w)

	if err := postRepack(ctx, tx, q, w.merchant, w.company, w.user, r, w.day); err != nil {
		t.Fatalf("postRepack: %v", err)
	}
	posted, err := q.GetRepackByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("read repack: %v", err)
	}
	if posted.Status != "posted" || !posted.PostedAt.Valid {
		t.Fatalf("repack must be posted with posted_at set, got %+v", posted)
	}
	// The repack_in move carries unit_cost "60.000000".
	var unitCost string
	if err := tx.QueryRow(ctx, `
		SELECT unit_cost::text FROM inventory.stock_move
		WHERE ref_id = $1 AND move_type = 'repack_in'`, r.ID).Scan(&unitCost); err != nil {
		t.Fatalf("read repack_in move: %v", err)
	}
	if unitCost != "60.000000" {
		t.Fatalf("repack_in unit_cost = %s, want 60.000000", unitCost)
	}
	// HPP (item_cost) is unchanged: A 50, B 80, C untouched 0.
	for _, tc := range []struct {
		item pgtype.UUID
		want string
	}{{itemA, "50.000000"}, {itemB, "80.000000"}} {
		var cost string
		if err := tx.QueryRow(ctx, `
			SELECT avg_cost::text FROM inventory.item_cost
			WHERE merchant_id = $1 AND item_id = $2`, w.merchant, tc.item).Scan(&cost); err != nil {
			t.Fatalf("read item_cost: %v", err)
		}
		if cost != tc.want {
			t.Fatalf("item_cost = %s, want %s", cost, tc.want)
		}
	}
	// Item C never had an item_cost row and postRepack must not create one.
	var costRows int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM inventory.item_cost
		WHERE merchant_id = $1 AND item_id = $2`, w.merchant, itemC).Scan(&costRows); err != nil {
		t.Fatalf("count item_cost C: %v", err)
	}
	if costRows != 0 {
		t.Fatalf("item C must have no item_cost row, got %d", costRows)
	}
	// Balances: out lots drained, in item 15 at the in location.
	var balA, balB, balC int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemA).Scan(&balA); err != nil {
		t.Fatalf("read balance A: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemB).Scan(&balB); err != nil {
		t.Fatalf("read balance B: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemC).Scan(&balC); err != nil {
		t.Fatalf("read balance C: %v", err)
	}
	if balA != 0 || balB != 0 || balC != 15_000 {
		t.Fatalf("balances after repack = A %d B %d C %d, want 0/0/15000", balA, balB, balC)
	}
}

// TestRepackVoid: voiding a posted repack writes the full reversal pair set
// and keeps posted_at (chk_repack_posted requires it NOT NULL once posted).
func TestRepackVoid(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "identrp2", "081234573107", "Klinik Repack 2")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	r, itemA, itemB, itemC, _, q := identityRepackFixture(t, ctx, tx, w)

	if err := postRepack(ctx, tx, q, w.merchant, w.company, w.user, r, w.day); err != nil {
		t.Fatalf("postRepack: %v", err)
	}
	if err := voidRepack(ctx, tx, q, w.merchant, w.company, w.user, r, "salah kemasan"); err != nil {
		t.Fatalf("voidRepack: %v", err)
	}
	voided, err := q.GetRepackByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("read repack: %v", err)
	}
	if voided.Status != "void" || voided.VoidReason.String != "salah kemasan" {
		t.Fatalf("repack must be void with its reason, got %+v", voided)
	}
	if !voided.PostedAt.Valid {
		t.Fatal("posted_at must be preserved after void (chk_repack_posted)")
	}
	// Reversal pairs: 1 negative per repack_in, 1 positive per repack_out,
	// each naming its source move.
	var negIn, posOut, missingRef int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE quantity < 0), count(*) FILTER (WHERE quantity > 0),
		       count(*) FILTER (WHERE reversal_of IS NULL)
		FROM inventory.stock_move
		WHERE ref_type = 'repack' AND ref_id = $1 AND move_type = 'reversal'`, r.ID).Scan(&negIn, &posOut, &missingRef); err != nil {
		t.Fatalf("read reversal moves: %v", err)
	}
	if negIn != 1 || posOut != 2 || missingRef != 0 {
		t.Fatalf("reversal pairs = neg %d pos %d missing-ref %d, want 1/2/0", negIn, posOut, missingRef)
	}
	// Balances restored: A 10, B 5, C 0.
	var balA, balB, balC int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemA).Scan(&balA); err != nil {
		t.Fatalf("read balance A: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemB).Scan(&balB); err != nil {
		t.Fatalf("read balance B: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM((quantity*1000)::bigint),0) FROM inventory.stock_balance WHERE item_id = $1`, itemC).Scan(&balC); err != nil {
		t.Fatalf("read balance C: %v", err)
	}
	if balA != 10_000 || balB != 5_000 || balC != 0 {
		t.Fatalf("balances after void = A %d B %d C %d, want 10000/5000/0", balA, balB, balC)
	}
}

// §6 HTTP round-trips through the full router (plan Task 4, pola
// goods_receipt_test.go): the routes are exercised as the seeded owner with
// the standard tenant/auth headers.

// identityHTTPEnv is one full-router test environment: the seeded stock world
// plus the owner's bearer token.
type identityHTTPEnv struct {
	w      *stockWorld
	router *gin.Engine
	token  string
}

// identityHTTPSetup registers a fresh tenant through the real endpoints,
// seeds one warehouse room, and returns the router + owner token.
func identityHTTPSetup(t *testing.T, label, phone string) *identityHTTPEnv {
	t.Helper()
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	redisClient := newStockTestRedis(t, ctx)
	router := NewRouter(pool, redisClient, stockTestConfig())
	companyStr, userStr, token, _ := stockRegisterOwner(t, ctx, pool, router, "idh."+label, phone)
	merchantStr := stockMerchantID(t, ctx, pool, companyStr)
	tx := stockRuntimeTx(t, ctx, pool, companyStr, merchantStr)
	var locationID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room','GUD-1','Gudang Utama', ARRAY['warehouse']::text[]) RETURNING id`,
		companyStr, merchantStr).Scan(&locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	w := &stockWorld{
		pool:       pool,
		company:    stockMustUUID(t, ctx, tx, companyStr),
		merchant:   stockMustUUID(t, ctx, tx, merchantStr),
		user:       stockMustUUID(t, ctx, tx, userStr),
		companyStr: companyStr, merchantStr: merchantStr, userStr: userStr,
		location: locationID,
		day:      time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	// One default drug item (like seedStockWorld) for tests that seed
	// balances against it.
	tablet := stockUnitID(t, ctx, tx, "tablet")
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-STK','Obat Uji Stok','drug','generic',$2,'93100090','keras') RETURNING id`,
		companyStr, tablet).Scan(&w.item); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return &identityHTTPEnv{w: w, router: router, token: token}
}

// identityAPI sends an authenticated tenant request and decodes the JSON body.
func identityAPI(t *testing.T, e *identityHTTPEnv, method, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.w.companyStr)
	req.Header.Set("X-Merchant-ID", e.w.merchantStr)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// identityHTTPUnit resolves a catalog unit code to its id string.
func identityHTTPUnit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM inventory.unit WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("resolve unit %s: %v", code, err)
	}
	return id
}

var identityKfaSeq int64 = 931002000

// identityHTTPItem creates an item via the API and returns its id.
func identityHTTPItem(t *testing.T, e *identityHTTPEnv, code string, trackSerial bool) string {
	t.Helper()
	identityKfaSeq++
	status, out := identityAPI(t, e, http.MethodPost, "/api/v1/inventory/items", map[string]any{
		"code": code, "name": code + " name", "item_kind": "drug",
		"pricing_category": "generic", "drug_class": "keras",
		"base_unit_id": identityHTTPUnit(t, context.Background(), e.w.pool, "tablet"),
		"kfa_code":     fmt.Sprintf("%d", identityKfaSeq),
		"track_serial": trackSerial,
	})
	if status != http.StatusCreated {
		t.Fatalf("create item %s expected 201, got %d: %v", code, status, out)
	}
	data, _ := out["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("missing item id: %v", out)
	}
	return id
}

// TestSerialItemField: track_serial round-trips through create, get and patch
// (plain flag — no item_kind restriction).
func TestSerialItemField(t *testing.T) {
	e := identityHTTPSetup(t, "item", "081234573110")
	itemID := identityHTTPItem(t, e, "ITM-HTTP-SN", true)

	status, out := identityAPI(t, e, http.MethodGet, "/api/v1/inventory/items/"+itemID, nil)
	if status != http.StatusOK {
		t.Fatalf("get expected 200, got %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["track_serial"] != true {
		t.Fatalf("created track_serial = %v, want true", data["track_serial"])
	}

	status, out = identityAPI(t, e, http.MethodPatch, "/api/v1/inventory/items/"+itemID,
		map[string]any{"row_version": 1, "track_serial": false})
	if status != http.StatusOK {
		t.Fatalf("patch expected 200, got %d: %v", status, out)
	}
	data, _ = out["data"].(map[string]any)
	if data["track_serial"] != false {
		t.Fatalf("patched track_serial = %v, want false", data["track_serial"])
	}
}

// TestSerialAPI: bulk registration via POST /merchants/:id/serials and the
// filtered listing (?item_id=&status=).
func TestSerialAPI(t *testing.T) {
	e := identityHTTPSetup(t, "serial", "081234573111")
	itemID := identityHTTPItem(t, e, "ITM-HTTP-SE", true)
	serialPath := "/api/v1/merchants/" + e.w.merchantStr + "/serials"

	status, out := identityAPI(t, e, http.MethodPost, serialPath, map[string]any{
		"item_id": itemID, "location_id": e.w.location.String(),
		"serials": []string{"SN-H1", "SN-H2"},
	})
	if status != http.StatusCreated {
		t.Fatalf("register serials expected 201, got %d: %v", status, out)
	}
	rows, _ := out["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("registered serials = %v, want 2", rows)
	}

	status, out = identityAPI(t, e, http.MethodGet,
		serialPath+"?item_id="+itemID+"&status=in_stock", nil)
	if status != http.StatusOK {
		t.Fatalf("list serials expected 200, got %d: %v", status, out)
	}
	rows, _ = out["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("listed serials = %v, want 2", rows)
	}
	for _, r := range rows {
		if r.(map[string]any)["status"] != "in_stock" {
			t.Fatalf("serial status = %v, want in_stock", r)
		}
	}
}

// TestRepackAPI: create draft → post (status posted) → void (status void)
// over the full router, on the contract fixture shape (out 10 @ 50, in 10).
func TestRepackAPI(t *testing.T) {
	ctx := context.Background()
	e := identityHTTPSetup(t, "repack", "081234573112")
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	itemA := identitySeedItem(t, ctx, tx, e.w, "ITM-HRA", "Repack HTTP A", false, "tablet")
	itemC := identitySeedItem(t, ctx, tx, e.w, "ITM-HRC", "Repack HTTP C", false, "tablet")
	identitySetAvgCost(t, ctx, tx, e.w, itemA, "50.000000")
	ed := e.w.day.AddDate(0, 2, 0)
	lotA := identitySeedLot(t, ctx, tx, e.w, itemA, "B-HRA", &ed)
	identitySeedBalance(t, ctx, tx, e.w, itemA, lotA, "purchase", "available", 10_000, pgtype.UUID{})
	unitC := identitySeedItemUnit(t, ctx, tx, e.w, itemC,
		identitySeedUnit(t, ctx, tx, "tablet"), "1")
	inLocation := identitySeedLocation(t, ctx, tx, e.w, "PACK-H", "Ruang Kemas HTTP")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit repack seed: %v", err)
	}

	status, out := identityAPI(t, e, http.MethodPost,
		"/api/v1/merchants/"+e.w.merchantStr+"/repacks", map[string]any{
			"out_location_id": e.w.location.String(), "in_location_id": inLocation.String(),
			"out_lines": []map[string]any{
				{"item_id": itemA.String(), "lot_id": lotA.String(), "source": "purchase", "base_qty": "10"},
			},
			"in_lines": []map[string]any{
				{"item_id": itemC.String(), "item_unit_id": unitC.String(),
					"pack_qty": "10", "pack_content": "1", "base_qty": "10",
					"batch_no": "B-HRC", "expiry_date": "2026-12-01"},
			},
		})
	if status != http.StatusCreated {
		t.Fatalf("create repack expected 201, got %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["status"] != "draft" {
		t.Fatalf("repack status = %v, want draft", data["status"])
	}
	repackID, _ := data["id"].(string)

	status, out = identityAPI(t, e, http.MethodPost, "/api/v1/repacks/"+repackID+"/post", nil)
	if status != http.StatusOK {
		t.Fatalf("post repack expected 200, got %d: %v", status, out)
	}
	if out["data"].(map[string]any)["status"] != "posted" {
		t.Fatalf("posted status = %v, want posted", out["data"])
	}

	status, out = identityAPI(t, e, http.MethodPost, "/api/v1/repacks/"+repackID+"/void",
		map[string]any{"reason": "salah kemasan"})
	if status != http.StatusOK {
		t.Fatalf("void repack expected 200, got %d: %v", status, out)
	}
	if out["data"].(map[string]any)["status"] != "void" {
		t.Fatalf("voided status = %v, want void", out["data"])
	}
}

// TestConsignmentReportAPI: two consignment issues on different dates price at
// 900/unit (contract 9000.00 per pack of 10) — the report aggregates
// qty "6.000" / value "5400.000000" with one detail per date, and the
// supplier_id filter excludes other suppliers.
func TestConsignmentReportAPI(t *testing.T) {
	ctx := context.Background()
	e := identityHTTPSetup(t, "consign", "081234573113")
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	s1 := identitySeedSupplier(t, ctx, tx, e.w, "SUP-H1", "PT HTTP Satu")
	s2 := identitySeedSupplier(t, ctx, tx, e.w, "SUP-H2", "PT HTTP Dua")
	packUnitID := identitySeedItemUnit(t, ctx, tx, e.w, e.w.item,
		identitySeedUnit(t, ctx, tx, "tablet"), "10")
	identitySeedContract(t, ctx, tx, e.w, s1, e.w.item, packUnitID, "9000.00")
	ed := e.w.day.AddDate(0, 1, 0)
	lot := identitySeedLot(t, ctx, tx, e.w, e.w.item, "B-HCONS", &ed)
	identitySeedBalance(t, ctx, tx, e.w, e.w.item, lot, "consignment", "available", 10_000, s1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit consignment seed: %v", err)
	}

	// Issues priced from the contract: 4 × 900 = 3600 on 2026-10-01 and
	// 2 × 900 = 1800 on 2026-10-05 (w.day = 2026-10-10).
	tx1 := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := issueStock(ctx, sqlcgen.New(tx1), e.w.merchant, e.w.company, e.w.user,
		e.w.location, e.w.item, numericFromMilli(4_000), e.w.day.AddDate(0, 0, -9),
		stockRef{RefType: "usage"}, pgtype.UUID{}, ""); err != nil {
		t.Fatalf("issue 1: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit issue 1: %v", err)
	}
	tx2 := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := issueStock(ctx, sqlcgen.New(tx2), e.w.merchant, e.w.company, e.w.user,
		e.w.location, e.w.item, numericFromMilli(2_000), e.w.day.AddDate(0, 0, -5),
		stockRef{RefType: "usage"}, pgtype.UUID{}, ""); err != nil {
		t.Fatalf("issue 2: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit issue 2: %v", err)
	}

	reportPath := "/api/v1/merchants/" + e.w.merchantStr +
		"/consignment/report?from=2026-10-01&to=2026-10-31"
	status, out := identityAPI(t, e, http.MethodGet, reportPath, nil)
	if status != http.StatusOK {
		t.Fatalf("report expected 200, got %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	suppliers, _ := data["suppliers"].([]any)
	if len(suppliers) != 1 {
		t.Fatalf("suppliers = %v, want 1", suppliers)
	}
	sup := suppliers[0].(map[string]any)
	if sup["supplier_id"] != s1.String() {
		t.Fatalf("supplier_id = %v, want %s", sup["supplier_id"], s1)
	}
	items, _ := sup["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1", items)
	}
	entry := items[0].(map[string]any)
	// Expected: qty 4+2 = "6.000"; value ROUND(3600+1800, 6) = "5400.000000".
	if entry["qty"] != "6.000" || entry["value"] != "5400.000000" {
		t.Fatalf("aggregate = %v/%v, want 6.000/5400.000000", entry["qty"], entry["value"])
	}
	details, _ := entry["details"].([]any)
	if len(details) != 2 {
		t.Fatalf("details = %v, want 2", details)
	}
	first := details[0].(map[string]any)
	second := details[1].(map[string]any)
	if first["used_on"] != "2026-10-01" || first["qty"] != "4.000" || first["unit_price"] != "900.000000" {
		t.Fatalf("detail[0] = %v", first)
	}
	if second["used_on"] != "2026-10-05" || second["qty"] != "2.000" || second["unit_price"] != "900.000000" {
		t.Fatalf("detail[1] = %v", second)
	}

	// The supplier_id filter excludes suppliers without usage.
	status, out = identityAPI(t, e, http.MethodGet,
		reportPath+"&supplier_id="+s2.String(), nil)
	if status != http.StatusOK {
		t.Fatalf("filtered report expected 200, got %d: %v", status, out)
	}
	suppliers, _ = out["data"].(map[string]any)["suppliers"].([]any)
	if len(suppliers) != 0 {
		t.Fatalf("filtered suppliers = %v, want empty", suppliers)
	}
}
