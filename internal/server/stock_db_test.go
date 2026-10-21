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

// TestStockDB_Outbound — migration 000081 outbound value: supplier_return and
// destruction are valid stock_move types (bogus ones still rejected), the
// supplier_return posted/draft/void invariants hold (posted_at survives void),
// destruction witnesses must be a JSON array, inter-merchant transfers reject
// identical merchants and duplicate per-company numbers (other companies are
// fine), supplier returns are invisible across merchants, and a company
// session sees its inter-merchant transfers across all of its merchants.
func TestStockDB_Outbound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "stockdb.outbound", "081234572006")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Outbound")
	company2ID, merchantC, _, token2, refreshCookie2 := seedAccountOwner(t, ctx, pool, router, "stockdb.outbound2", "081234572007")
	merchantD := createTenantMerchant(t, ctx, pool, router, token2, refreshCookie2, company2ID, "Cabang Outbound Dua")

	txA := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	locationA, itemID, lotID := seedStockRefs(t, ctx, txA, companyID, merchantA, "O1")
	var supplierID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,'SUP-RET','Supplier Retur Uji') RETURNING id::text`,
		companyID, merchantA).Scan(&supplierID); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	// (a) new move_type values pass the widened CHECK, bogus ones do not.
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
		VALUES ($1,$2,CURRENT_DATE,'supplier_return',$3,$4,$5,'purchase','available',-3)`,
		companyID, merchantA, locationA, itemID, lotID); err != nil {
		t.Fatalf("insert supplier_return move: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
		VALUES ($1,$2,CURRENT_DATE,'destruction',$3,$4,$5,'purchase','available',-2)`,
		companyID, merchantA, locationA, itemID, lotID); err != nil {
		t.Fatalf("insert destruction move: %v", err)
	}
	savepointExec(t, ctx, txA, "bogus move_type", "stock_move_move_type_check",
		`INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
			VALUES ($1,$2,CURRENT_DATE,'bogus',$3,$4,$5,'purchase','available',-1)`,
		companyID, merchantA, locationA, itemID, lotID)

	// (b) supplier_return status invariants.
	savepointExec(t, ctx, txA, "posted return without posted_at", "chk_supplier_return_posted",
		`INSERT INTO inventory.supplier_return (company_id, merchant_id, supplier_id, return_no, status, reason)
			VALUES ($1,$2,$3,'SRET-1','posted',' Salah kirim ')`,
		companyID, merchantA, supplierID)
	savepointExec(t, ctx, txA, "draft return with posted_at", "chk_supplier_return_posted",
		`INSERT INTO inventory.supplier_return (company_id, merchant_id, supplier_id, return_no, status, reason, posted_at)
			VALUES ($1,$2,$3,'SRET-1','draft','Salah kirim',now())`,
		companyID, merchantA, supplierID)
	savepointExec(t, ctx, txA, "void return without reason", "chk_supplier_return_void",
		`INSERT INTO inventory.supplier_return (company_id, merchant_id, supplier_id, return_no, status, reason, posted_at)
			VALUES ($1,$2,$3,'SRET-1','void','Salah kirim',now())`,
		companyID, merchantA, supplierID)
	var returnID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.supplier_return (company_id, merchant_id, supplier_id, return_no, reason)
		VALUES ($1,$2,$3,'SRET-1','Salah kirim') RETURNING id::text`,
		companyID, merchantA, supplierID).Scan(&returnID); err != nil {
		t.Fatalf("insert draft supplier_return: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		UPDATE inventory.supplier_return SET status = 'posted', posted_at = now(), posted_by = NULL
		WHERE id = $1`, returnID); err != nil {
		t.Fatalf("post supplier_return: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		UPDATE inventory.supplier_return SET status = 'void', void_reason = 'Salah posting'
		WHERE id = $1`, returnID); err != nil {
		t.Fatalf("void supplier_return keeping posted_at: %v", err)
	}
	var voided, hasPostedAt bool
	if err := txA.QueryRow(ctx,
		"SELECT status = 'void', posted_at IS NOT NULL FROM inventory.supplier_return WHERE id = $1", returnID).Scan(&voided, &hasPostedAt); err != nil {
		t.Fatalf("reread supplier_return: %v", err)
	}
	if !voided || !hasPostedAt {
		t.Fatalf("void return must keep posted_at, got voided=%v posted_at=%v", voided, hasPostedAt)
	}

	// (c) destruction witnesses must be a JSON array.
	savepointExec(t, ctx, txA, "destruction witnesses non-array", "chk_destruction_witnesses",
		`INSERT INTO inventory.destruction (company_id, merchant_id, location_id, occurred_on, destruction_no, method, witnesses)
			VALUES ($1,$2,$3,CURRENT_DATE,'DSTR-1','Dibakar','"x"'::jsonb)`,
		companyID, merchantA, locationA)

	// (d) inter-merchant transfer: distinct merchants, unique no per company.
	savepointExec(t, ctx, txA, "imt identical merchants", "chk_imt_distinct",
		`INSERT INTO inventory.inter_merchant_transfer
			(company_id, from_merchant_id, to_merchant_id, from_location_id, to_location_id, imt_no)
			VALUES ($1,$2,$2,$3,$3,'IMT-0001')`,
		companyID, merchantA, locationA)
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.inter_merchant_transfer
			(company_id, from_merchant_id, to_merchant_id, from_location_id, to_location_id, imt_no)
			VALUES ($1,$2,$3,$4,$5,'IMT-0001')`,
		companyID, merchantA, merchantB, locationA, locationA); err != nil {
		t.Fatalf("insert valid IMT: %v", err)
	}
	savepointExec(t, ctx, txA, "duplicate imt_no same company", "uq_inter_merchant_transfer_no",
		`INSERT INTO inventory.inter_merchant_transfer
			(company_id, from_merchant_id, to_merchant_id, from_location_id, to_location_id, imt_no)
			VALUES ($1,$2,$3,$4,$5,'IMT-0001')`,
		companyID, merchantA, merchantB, locationA, locationA)

	// (e) RLS: merchant B cannot read merchant A's returns; a company session
	// sees its IMT across merchants; the same imt_no in another company is fine.
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit merchant A rows: %v", err)
	}
	txB := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	var seenReturnsB int
	if err := txB.QueryRow(ctx, "SELECT count(*) FROM inventory.supplier_return").Scan(&seenReturnsB); err != nil {
		t.Fatalf("count supplier_return as merchant B: %v", err)
	}
	if seenReturnsB != 0 {
		t.Fatalf("merchant B must not see merchant A returns, got %d", seenReturnsB)
	}
	txA2 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	var seenReturnsA int
	if err := txA2.QueryRow(ctx, "SELECT count(*) FROM inventory.supplier_return").Scan(&seenReturnsA); err != nil {
		t.Fatalf("count supplier_return as merchant A: %v", err)
	}
	if seenReturnsA != 1 {
		t.Fatalf("merchant A must see its own return, got %d", seenReturnsA)
	}

	txCompany := runtimeTx(t, ctx, pool, companyID)
	var seenIMT int
	if err := txCompany.QueryRow(ctx, "SELECT count(*) FROM inventory.inter_merchant_transfer").Scan(&seenIMT); err != nil {
		t.Fatalf("count IMT as company session: %v", err)
	}
	if seenIMT != 1 {
		t.Fatalf("company session must see its IMT across merchants, got %d", seenIMT)
	}
	if err := txCompany.Rollback(ctx); err != nil {
		t.Fatalf("rollback company session: %v", err)
	}

	txC := runtimeTenantTx(t, ctx, pool, company2ID, merchantC)
	var locationC string
	if err := txC.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room','LOC-OC','Gudang Perusahaan Dua',ARRAY['warehouse']::text[])
		RETURNING id::text`, company2ID, merchantC).Scan(&locationC); err != nil {
		t.Fatalf("insert company-2 location: %v", err)
	}
	if _, err := txC.Exec(ctx, `
		INSERT INTO inventory.inter_merchant_transfer
			(company_id, from_merchant_id, to_merchant_id, from_location_id, to_location_id, imt_no)
		VALUES ($1,$2,$3,$4,$5,'IMT-0001')`, company2ID, merchantC, merchantD, locationC, locationC); err != nil {
		t.Fatalf("same imt_no in another company must insert: %v", err)
	}
	var seenIMTC int
	if err := txC.QueryRow(ctx, "SELECT count(*) FROM inventory.inter_merchant_transfer").Scan(&seenIMTC); err != nil {
		t.Fatalf("count IMT as company 2: %v", err)
	}
	if seenIMTC != 1 {
		t.Fatalf("company 2 must see only its own IMT, got %d", seenIMTC)
	}
}

// TestStockDB_Identity — migration 000082 identity & packaging: supplier_id on
// stock_balance is consignment-only (NOT VALID check still rejects new rows),
// the widened unique slot separates consignment balances per supplier,
// repack_out/repack_in move types pass the widened CHECK, serial numbers are
// unique per merchant+item but reusable across merchants, consignment_usage is
// append-only for app_runtime (UPDATE revoked), repack rejects identical
// out/in locations, and track_serial defaults to false.
func TestStockDB_Identity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "stockdb.identity", "081234572008")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Identitas")

	txA := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	locationID, itemID, lotID := seedStockRefs(t, ctx, txA, companyID, merchantA, "I1")
	var supplier1ID, supplier2ID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,'SUP-ID1','Supplier Konsinyasi Satu') RETURNING id::text`,
		companyID, merchantA).Scan(&supplier1ID); err != nil {
		t.Fatalf("insert supplier 1: %v", err)
	}
	if err := txA.QueryRow(ctx, `
		INSERT INTO core.supplier (company_id, merchant_id, code, name)
		VALUES ($1,$2,'SUP-ID2','Supplier Konsinyasi Dua') RETURNING id::text`,
		companyID, merchantA).Scan(&supplier2ID); err != nil {
		t.Fatalf("insert supplier 2: %v", err)
	}

	// (f) track_serial defaults to false on existing-style items.
	var trackSerial bool
	if err := txA.QueryRow(ctx,
		"SELECT track_serial FROM inventory.item WHERE id = $1", itemID).Scan(&trackSerial); err != nil {
		t.Fatalf("read track_serial: %v", err)
	}
	if trackSerial {
		t.Fatalf("track_serial must default to false")
	}

	// (a) a purchase balance carrying supplier_id is rejected even though the
	// check is NOT VALID — NOT VALID only skips existing-row validation.
	savepointExec(t, ctx, txA, "purchase balance with supplier", "chk_stock_balance_supplier",
		`INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity, supplier_id)
			VALUES ($1,$2,$3,$4,$5,'purchase','available',10,$6)`,
		companyID, merchantA, locationID, itemID, lotID, supplier1ID)

	// (b) consignment balances on the same slot with different suppliers both
	// insert; the same supplier again hits the widened unique index.
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity, supplier_id)
		VALUES ($1,$2,$3,$4,$5,'consignment','available',5,$6)`,
		companyID, merchantA, locationID, itemID, lotID, supplier1ID); err != nil {
		t.Fatalf("insert consignment balance supplier 1: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity, supplier_id)
		VALUES ($1,$2,$3,$4,$5,'consignment','available',7,$6)`,
		companyID, merchantA, locationID, itemID, lotID, supplier2ID); err != nil {
		t.Fatalf("insert consignment balance supplier 2 (same slot) must pass: %v", err)
	}
	savepointExec(t, ctx, txA, "duplicate consignment slot same supplier", "uq_stock_balance_slot",
		`INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity, supplier_id)
			VALUES ($1,$2,$3,$4,$5,'consignment','available',1,$6)`,
		companyID, merchantA, locationID, itemID, lotID, supplier1ID)

	// (c) repack_out/repack_in move types pass the widened CHECK; bogus fails.
	for _, mt := range []string{"repack_out", "repack_in"} {
		if _, err := txA.Exec(ctx, `
			INSERT INTO inventory.stock_move
				(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
			VALUES ($1,$2,CURRENT_DATE,$3,$4,$5,$6,'purchase','available',-1)`,
			companyID, merchantA, mt, locationID, itemID, lotID); err != nil {
			t.Fatalf("insert %s move: %v", mt, err)
		}
	}
	savepointExec(t, ctx, txA, "bogus move_type", "stock_move_move_type_check",
		`INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id, source, status, quantity)
			VALUES ($1,$2,CURRENT_DATE,'bogus',$3,$4,$5,'purchase','available',-1)`,
		companyID, merchantA, locationID, itemID, lotID)

	// (d) serial_no is unique per (merchant, item); duplicates are rejected.
	var tablet string
	if err := txA.QueryRow(ctx,
		"SELECT id::text FROM inventory.item_unit WHERE item_id = $1", itemID).Scan(&tablet); err != nil {
		t.Fatalf("resolve item_unit: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.serial (company_id, merchant_id, item_id, serial_no, lot_id, location_id)
		VALUES ($1,$2,$3,'SN-0001',$4,$5)`,
		companyID, merchantA, itemID, lotID, locationID); err != nil {
		t.Fatalf("insert serial: %v", err)
	}
	savepointExec(t, ctx, txA, "duplicate serial same merchant", "uq_serial",
		`INSERT INTO inventory.serial (company_id, merchant_id, item_id, serial_no, lot_id, location_id)
			VALUES ($1,$2,$3,'SN-0001',$4,$5)`,
		companyID, merchantA, itemID, lotID, locationID)

	// (e) consignment_usage is append-only for app_runtime (UPDATE revoked).
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.consignment_usage
			(company_id, merchant_id, supplier_id, item_id, lot_id, base_qty, unit_price, used_on)
		VALUES ($1,$2,$3,$4,$5,2,15000,CURRENT_DATE)`,
		companyID, merchantA, supplier1ID, itemID, lotID); err != nil {
		t.Fatalf("insert consignment_usage: %v", err)
	}
	savepointExec(t, ctx, txA, "update consignment_usage as app_runtime", "permission denied",
		"UPDATE inventory.consignment_usage SET base_qty = 99")

	// (f) repack with identical out/in locations is rejected.
	savepointExec(t, ctx, txA, "repack identical locations", "chk_repack_locations",
		`INSERT INTO inventory.repack (company_id, merchant_id, out_location_id, in_location_id)
			VALUES ($1,$2,$3,$3)`, companyID, merchantA, locationID)

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit merchant A rows: %v", err)
	}

	// (d, cont.) the same serial_no on a sibling merchant of the same company
	// is allowed — uniqueness is per merchant.
	txB := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	var locationB string
	if err := txB.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room','LOC-IB','Gudang Identitas',ARRAY['warehouse']::text[])
		RETURNING id::text`, companyID, merchantB).Scan(&locationB); err != nil {
		t.Fatalf("insert merchant B location: %v", err)
	}
	if _, err := txB.Exec(ctx, `
		INSERT INTO inventory.serial (company_id, merchant_id, item_id, serial_no, location_id)
		VALUES ($1,$2,$3,'SN-0001',$4)`,
		companyID, merchantB, itemID, locationB); err != nil {
		t.Fatalf("same serial_no on another merchant must insert: %v", err)
	}
}

// TestStockDB_ColdChain — migration 000083 cold chain & emergency kits: VVM
// stages are bounded to 1..4, only one active seal per emergency kit is allowed
// (a broken seal frees the slot), opened containers demand a BUD timestamp and
// a non-negative remaining quantity that may reach zero, vvm_log and
// fridge_temp_log are append-only for app_runtime, the new tables are
// merchant-isolated under RLS, and kit items are unique per (kit, item).
func TestStockDB_ColdChain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "stockdb.coldchain", "081234572009")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Cold Chain")

	txA := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	locationID, itemID, lotID := seedStockRefs(t, ctx, txA, companyID, merchantA, "CC")

	// (c) opened_container: BUD timestamp is mandatory and remaining qty >= 0.
	savepointExec(t, ctx, txA, "opened container without BUD", "bud_at",
		`INSERT INTO inventory.opened_container
			(company_id, merchant_id, item_id, lot_id, location_id, opened_at, bud_at, remaining_base_qty)
			VALUES ($1,$2,$3,$4,$5,now(),NULL,5)`,
		companyID, merchantA, itemID, lotID, locationID)
	savepointExec(t, ctx, txA, "negative remaining quantity", "opened_container_remaining_base_qty_check",
		`INSERT INTO inventory.opened_container
			(company_id, merchant_id, item_id, lot_id, location_id, opened_at, bud_at, remaining_base_qty)
			VALUES ($1,$2,$3,$4,$5,now(),now() + interval '28 days',-1)`,
		companyID, merchantA, itemID, lotID, locationID)
	var containerID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.opened_container
			(company_id, merchant_id, item_id, lot_id, location_id, opened_at, bud_at, remaining_base_qty)
		VALUES ($1,$2,$3,$4,$5,now(),now() + interval '28 days',5) RETURNING id::text`,
		companyID, merchantA, itemID, lotID, locationID).Scan(&containerID); err != nil {
		t.Fatalf("insert opened_container: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		UPDATE inventory.opened_container SET remaining_base_qty = 0, status = 'finished'
		WHERE id = $1`, containerID); err != nil {
		t.Fatalf("finish container at zero remaining must pass: %v", err)
	}

	// (a) vvm_log stage is bounded to 1..4.
	savepointExec(t, ctx, txA, "vvm stage 0", "vvm_log_stage_check",
		`INSERT INTO inventory.vvm_log (company_id, merchant_id, opened_container_id, checked_at, stage)
			VALUES ($1,$2,$3,now(),0)`, companyID, merchantA, containerID)
	savepointExec(t, ctx, txA, "vvm stage 5", "vvm_log_stage_check",
		`INSERT INTO inventory.vvm_log (company_id, merchant_id, opened_container_id, checked_at, stage)
			VALUES ($1,$2,$3,now(),5)`, companyID, merchantA, containerID)
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.vvm_log (company_id, merchant_id, opened_container_id, checked_at, stage)
		VALUES ($1,$2,$3,now(),2)`, companyID, merchantA, containerID); err != nil {
		t.Fatalf("insert valid vvm_log: %v", err)
	}

	// (b) only one active seal per kit; breaking it frees the slot.
	var kitID, seal1ID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.emergency_kit (company_id, merchant_id, name, location_id)
		VALUES ($1,$2,'Troli Emergensi Uji',$3) RETURNING id::text`,
		companyID, merchantA, locationID).Scan(&kitID); err != nil {
		t.Fatalf("insert emergency_kit: %v", err)
	}
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.emergency_kit_seal (company_id, merchant_id, kit_id, seal_no)
		VALUES ($1,$2,$3,'SEGEL-001') RETURNING id::text`,
		companyID, merchantA, kitID).Scan(&seal1ID); err != nil {
		t.Fatalf("insert first seal: %v", err)
	}
	savepointExec(t, ctx, txA, "second active seal same kit", "uq_emergency_kit_seal_active",
		`INSERT INTO inventory.emergency_kit_seal (company_id, merchant_id, kit_id, seal_no)
			VALUES ($1,$2,$3,'SEGEL-002')`, companyID, merchantA, kitID)
	if _, err := txA.Exec(ctx, `
		UPDATE inventory.emergency_kit_seal SET broken_at = now() WHERE id = $1`, seal1ID); err != nil {
		t.Fatalf("break first seal: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.emergency_kit_seal (company_id, merchant_id, kit_id, seal_no)
		VALUES ($1,$2,$3,'SEGEL-003')`, companyID, merchantA, kitID); err != nil {
		t.Fatalf("seal after breaking the old one must insert: %v", err)
	}

	// (f) kit items are unique per (kit, item).
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.emergency_kit_item (company_id, merchant_id, kit_id, item_id, base_qty)
		VALUES ($1,$2,$3,$4,2)`, companyID, merchantA, kitID, itemID); err != nil {
		t.Fatalf("insert kit item: %v", err)
	}
	savepointExec(t, ctx, txA, "duplicate kit item", "uq_emergency_kit_item",
		`INSERT INTO inventory.emergency_kit_item (company_id, merchant_id, kit_id, item_id, base_qty)
			VALUES ($1,$2,$3,$4,1)`, companyID, merchantA, kitID, itemID)

	// (d) vvm_log and fridge_temp_log are append-only for app_runtime.
	var fridgeID string
	if err := txA.QueryRow(ctx, `
		INSERT INTO inventory.fridge (company_id, merchant_id, name, location_id)
		VALUES ($1,$2,'Kulkas Vaksin Uji',$3) RETURNING id::text`,
		companyID, merchantA, locationID).Scan(&fridgeID); err != nil {
		t.Fatalf("insert fridge: %v", err)
	}
	if _, err := txA.Exec(ctx, `
		INSERT INTO inventory.fridge_temp_log (company_id, merchant_id, fridge_id, logged_at, temperature)
		VALUES ($1,$2,$3,now(),4.2)`, companyID, merchantA, fridgeID); err != nil {
		t.Fatalf("insert fridge_temp_log: %v", err)
	}
	savepointExec(t, ctx, txA, "update vvm_log as app_runtime", "permission denied",
		"UPDATE inventory.vvm_log SET stage = 3")
	savepointExec(t, ctx, txA, "update fridge_temp_log as app_runtime", "permission denied",
		"UPDATE inventory.fridge_temp_log SET temperature = 9.9")

	// (e) RLS: merchant B sees none of merchant A's cold chain rows.
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit merchant A cold chain rows: %v", err)
	}
	txB := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	var seenContainers, seenKits int
	if err := txB.QueryRow(ctx,
		"SELECT count(*), (SELECT count(*) FROM inventory.emergency_kit) FROM inventory.opened_container").Scan(&seenContainers, &seenKits); err != nil {
		t.Fatalf("count cold chain rows as merchant B: %v", err)
	}
	if seenContainers != 0 || seenKits != 0 {
		t.Fatalf("merchant B must not see merchant A rows, got containers=%d kits=%d", seenContainers, seenKits)
	}
	if err := txB.Rollback(ctx); err != nil {
		t.Fatalf("rollback merchant B session: %v", err)
	}
	txA2 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	var ownContainers, ownKits int
	if err := txA2.QueryRow(ctx,
		"SELECT count(*), (SELECT count(*) FROM inventory.emergency_kit) FROM inventory.opened_container").Scan(&ownContainers, &ownKits); err != nil {
		t.Fatalf("count cold chain rows as merchant A: %v", err)
	}
	if ownContainers != 1 || ownKits != 1 {
		t.Fatalf("merchant A must see its own rows, got containers=%d kits=%d", ownContainers, ownKits)
	}
}
