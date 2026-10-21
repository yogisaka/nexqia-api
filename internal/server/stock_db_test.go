//go:build integration

package server_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// seedStockRefs inserts a warehouse/pharmacy room, a drug item with its base
// packaging, and a lot — the reference rows every stock constraint test needs.
// The tenant GUCs must be set on tx.
func seedStockRefs(t *testing.T, ctx context.Context, tx pgx.Tx, companyID, merchantID, tag string) (locationID, itemID, lotID string) {
	t.Helper()
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room',$3,'Gudang Utama',ARRAY['warehouse','pharmacy']::text[])
		RETURNING id::text`, companyID, merchantID, "LOC-"+tag).Scan(&locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	tablet := unitID(t, ctx, tx, "tablet")
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,$2,'Obat Uji Stok','drug','generic',$3,$4,'keras') RETURNING id::text`,
		companyID, "ITM-"+tag, tablet, "93100099").Scan(&itemID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.item_unit (company_id, item_id, unit_id, factor, is_default_issue)
		VALUES ($1,$2,$3,1,true)`, companyID, itemID, tablet); err != nil {
		t.Fatalf("insert item_unit: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date)
		VALUES ($1,$2,$3,DATE '2028-06-30') RETURNING id::text`,
		companyID, itemID, "BATCH-"+tag).Scan(&lotID); err != nil {
		t.Fatalf("insert lot: %v", err)
	}
	return locationID, itemID, lotID
}

// TestStockDB_Constraints — CHECK/UNIQUE guards of the stock core tables:
// negative balances, zero-quantity moves, recalled lots without a note,
// purchase receipts without a supplier, void receipts without a reason,
// min>max stock levels and a second draft opname per location all fail.
func TestStockDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "stockdb.constraint", "081234572001")

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	locationID, itemID, lotID := seedStockRefs(t, ctx, tx, companyID, merchantID, "C1")

	savepointExec(t, ctx, tx, "negative balance", "stock_balance_quantity_check",
		`INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity)
			VALUES ($1,$2,$3,$4,$5,'purchase','available',-1)`,
		companyID, merchantID, locationID, itemID, lotID)
	savepointExec(t, ctx, tx, "zero-quantity move", "stock_move_quantity_check",
		`INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
			VALUES ($1,$2,CURRENT_DATE,'receipt',$3,$4,$5,'purchase','available',0)`,
		companyID, merchantID, locationID, itemID, lotID)
	savepointExec(t, ctx, tx, "recalled lot without note", "chk_lot_recall",
		`INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date, is_recalled)
			VALUES ($1,$2,'BATCH-RECALL',DATE '2028-06-30',true)`, companyID, itemID)
	savepointExec(t, ctx, tx, "purchase receipt without supplier", "chk_goods_receipt_supplier",
		`INSERT INTO inventory.goods_receipt (company_id, merchant_id, supplier_id, location_id, source, received_on)
			VALUES ($1,$2,NULL,$3,'purchase',CURRENT_DATE)`,
		companyID, merchantID, locationID)
	savepointExec(t, ctx, tx, "void receipt without reason", "chk_goods_receipt_void",
		`INSERT INTO inventory.goods_receipt (company_id, merchant_id, supplier_id, location_id, source, received_on, status, posted_at, void_reason)
			VALUES ($1,$2,NULL,$3,'grant',CURRENT_DATE,'void',now(),NULL)`,
		companyID, merchantID, locationID)
	savepointExec(t, ctx, tx, "stock level max below min", "chk_stock_level_range",
		`INSERT INTO inventory.stock_level (company_id, merchant_id, location_id, item_id, min_qty, max_qty)
			VALUES ($1,$2,$3,$4,10,5)`,
		companyID, merchantID, locationID, itemID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.stock_count (company_id, merchant_id, location_id, count_date)
		VALUES ($1,$2,$3,CURRENT_DATE)`, companyID, merchantID, locationID); err != nil {
		t.Fatalf("first draft opname must insert, got %v", err)
	}
	savepointExec(t, ctx, tx, "second draft opname same location", "uq_stock_count_draft",
		`INSERT INTO inventory.stock_count (company_id, merchant_id, location_id, count_date)
			VALUES ($1,$2,$3,CURRENT_DATE)`, companyID, merchantID, locationID)
}

// TestStockDB_MoveImmutable — the stock card is append-only: app_runtime may
// neither UPDATE nor DELETE inventory.stock_move rows.
func TestStockDB_MoveImmutable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "stockdb.immutable", "081234572002")

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	locationID, itemID, lotID := seedStockRefs(t, ctx, tx, companyID, merchantID, "M1")

	var moveID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
		VALUES ($1,$2,CURRENT_DATE,'receipt',$3,$4,$5,'purchase','available',10) RETURNING id::text`,
		companyID, merchantID, locationID, itemID, lotID).Scan(&moveID); err != nil {
		t.Fatalf("insert stock_move: %v", err)
	}
	savepointExec(t, ctx, tx, "update stock_move", "permission denied",
		"UPDATE inventory.stock_move SET quantity = 99 WHERE id = $1", moveID)
	savepointExec(t, ctx, tx, "delete stock_move", "permission denied",
		"DELETE FROM inventory.stock_move WHERE id = $1", moveID)
}

// TestStockDB_SupplierCompanyVisibility — company-level suppliers
// (merchant_id NULL) are visible from every merchant of the company, while a
// merchant-only supplier stays invisible from sibling merchants.
func TestStockDB_SupplierCompanyVisibility(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "stockdb.supplier", "081234572003")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Stok")

	tx1 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	if _, err := tx1.Exec(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,NULL,'SUP-CO','Supplier Pusat Perusahaan')`, companyID); err != nil {
		t.Fatalf("insert company supplier: %v", err)
	}
	if _, err := tx1.Exec(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,'SUP-A','Supplier Khusus Merchant A')`, companyID, merchantA); err != nil {
		t.Fatalf("insert merchant A supplier: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit suppliers: %v", err)
	}

	tx2 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	var seenA int
	if err := tx2.QueryRow(ctx, "SELECT count(*) FROM core.supplier").Scan(&seenA); err != nil {
		t.Fatalf("count suppliers as merchant A: %v", err)
	}
	if seenA != 2 {
		t.Fatalf("merchant A must see company + own supplier, got %d", seenA)
	}

	tx3 := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	var seenB, companyLevel int
	if err := tx3.QueryRow(ctx,
		"SELECT count(*), count(*) FILTER (WHERE merchant_id IS NULL) FROM core.supplier").Scan(&seenB, &companyLevel); err != nil {
		t.Fatalf("count suppliers as merchant B: %v", err)
	}
	if seenB != 1 || companyLevel != 1 {
		t.Fatalf("merchant B must see only the company supplier, got %d (company-level %d)", seenB, companyLevel)
	}
}

// TestStockDB_LocationFunctions — after migration 000071 a room may carry the
// new warehouse/pharmacy functions required by stock locations.
func TestStockDB_LocationFunctions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "stockdb.location", "081234572004")

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	var funcs []string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room','LOC-RX','Ruang Obat',ARRAY['warehouse','pharmacy']::text[])
		RETURNING functions`, companyID, merchantID).Scan(&funcs); err != nil {
		t.Fatalf("room with warehouse+pharmacy must insert, got %v", err)
	}
	if len(funcs) != 2 {
		t.Fatalf("expected 2 functions stored, got %v", funcs)
	}
}

// TestStockDB_Procurement — migration 000080 procurement core: the central
// procurement mode requires a headquarters merchant, PR numbers are unique per
// merchant (same number across sibling merchants is fine), rejected PRs need a
// reason, PO lines default received_base_qty/pack_price and reject negative
// prices, goods receipts stay insertable without a PO link, and PRs are
// invisible across merchants under RLS.
func TestStockDB_Procurement(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "stockdb.procurement", "081234572005")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Pengadaan")

	txA := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	locationID, itemID, _ := seedStockRefs(t, ctx, txA, companyID, merchantA, "P1")
	var tablet string // item_unit id of the seeded item's base packaging
	if err := txA.QueryRow(ctx,
		"SELECT id::text FROM inventory.item_unit WHERE item_id = $1", itemID).Scan(&tablet); err != nil {
		t.Fatalf("resolve item_unit: %v", err)
	}

	var supplierID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,'SUP-PR','Supplier Pengadaan Uji') RETURNING id::text`,
		companyID, merchantA).Scan(&supplierID); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	// (a) mode 'sentral' without headquarters_merchant_id is rejected.
	savepointExec(t, ctx, txA, "sentral without headquarters", "chk_procurement_setting_mode",
		`INSERT INTO inventory.procurement_setting (company_id, mode) VALUES ($1,'sentral')`, companyID)

	// (b) pr_no unique per merchant; the same number on another merchant is fine.
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.purchase_request (company_id, merchant_id, location_id, pr_no)
		VALUES ($1,$2,$3,'PR-001')`, companyID, merchantA, locationID); err != nil {
		t.Fatalf("insert merchant A PR: %v", err)
	}
	savepointExec(t, ctx, txA, "duplicate pr_no same merchant", "uq_purchase_request_no",
		`INSERT INTO inventory.purchase_request (company_id, merchant_id, location_id, pr_no)
			VALUES ($1,$2,$3,'PR-001')`, companyID, merchantA, locationID)

	// (c) rejected PR without a reason is rejected.
	savepointExec(t, ctx, txA, "rejected PR without reason", "chk_pr_rejection",
		`INSERT INTO inventory.purchase_request (company_id, merchant_id, location_id, pr_no, status)
			VALUES ($1,$2,$3,'PR-002','rejected')`, companyID, merchantA, locationID)

	// (d) PO line defaults and non-negative pack_price.
	var poID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.purchase_order (company_id, merchant_id, supplier_id, location_id, po_no)
		VALUES ($1,$2,$3,$4,'PO-001') RETURNING id::text`,
		companyID, merchantA, supplierID, locationID).Scan(&poID); err != nil {
		t.Fatalf("insert PO: %v", err)
	}
	var receivedZero, priceZero bool
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.purchase_order_line (company_id, merchant_id, order_id, item_id, item_unit_id, pack_qty, pack_content)
		VALUES ($1,$2,$3,$4,$5,10,100) RETURNING received_base_qty = 0, pack_price = 0`,
		companyID, merchantA, poID, itemID, tablet).Scan(&receivedZero, &priceZero); err != nil {
		t.Fatalf("insert PO line: %v", err)
	}
	if !receivedZero || !priceZero {
		t.Fatalf("PO line defaults: received_base_qty zero = %v, pack_price zero = %v, want true/true", receivedZero, priceZero)
	}
	savepointExec(t, ctx, txA, "negative pack_price", "pack_price_check",
		`INSERT INTO inventory.purchase_order_line (company_id, merchant_id, order_id, item_id, item_unit_id, pack_qty, pack_content, pack_price)
			VALUES ($1,$2,$3,$4,$5,10,100,-1)`, companyID, merchantA, poID, itemID, tablet)

	// (e) goods_receipt.purchase_order_id is nullable — insert without it passes.
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.goods_receipt (company_id, merchant_id, supplier_id, location_id, source, received_on)
		VALUES ($1,$2,$3,$4,'purchase',CURRENT_DATE)`,
		companyID, merchantA, supplierID, locationID); err != nil {
		t.Fatalf("insert goods_receipt without PO link: %v", err)
	}

	// (f) RLS: the same pr_no on another merchant is allowed, and each
	// merchant session only sees its own PR.
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit merchant A rows: %v", err)
	}
	txB := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	var locationB string
	if err := txB.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room','LOC-PB','Gudang Cabang',ARRAY['warehouse']::text[])
		RETURNING id::text`, companyID, merchantB).Scan(&locationB); err != nil {
		t.Fatalf("insert branch location: %v", err)
	}
	if _, err := txB.Exec(ctx, `
		INSERT INTO inventory.purchase_request (company_id, merchant_id, location_id, pr_no)
		VALUES ($1,$2,$3,'PR-001')`, companyID, merchantB, locationB); err != nil {
		t.Fatalf("same pr_no on another merchant must insert: %v", err)
	}
	var seenB int
	if err := txB.QueryRow(ctx, "SELECT count(*) FROM inventory.purchase_request").Scan(&seenB); err != nil {
		t.Fatalf("count PRs as merchant B: %v", err)
	}
	if seenB != 1 {
		t.Fatalf("merchant B must see only its own PR, got %d", seenB)
	}
	txA2 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	var seenA int
	if err := txA2.QueryRow(ctx, "SELECT count(*) FROM inventory.purchase_request").Scan(&seenA); err != nil {
		t.Fatalf("count PRs as merchant A: %v", err)
	}
	if seenA != 1 {
		t.Fatalf("merchant A must see only its own PR, got %d", seenA)
	}
}
