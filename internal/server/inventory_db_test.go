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

// runtimeTenantTx opens a transaction acting as app_runtime with both tenant
// GUCs set, so inventory RLS (company on items, merchant on formulary rows) and
// the tenant-consistency triggers behave like production. Rolled back on test
// cleanup unless the test commits it first.
func runtimeTenantTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
		t.Fatalf("set merchant GUC: %v", err)
	}
	return tx
}

// savepointExec runs one statement inside a savepoint so a failing statement
// does not abort the surrounding transaction; it asserts the error mentions want.
func savepointExec(t *testing.T, ctx context.Context, tx pgx.Tx, name, want, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: expected error containing %q, got %v", name, want, err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}

// unitID resolves a platform unit code from the unit dictionary.
func unitID(t *testing.T, ctx context.Context, tx pgx.Tx, code string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM inventory.unit WHERE code = $1", code).Scan(&id); err != nil {
		t.Fatalf("resolve unit %s: %v", code, err)
	}
	return id
}

// TestInventoryDB_ItemConstraints — CHECK constraints of inventory.item: KFA or
// non-KFA reason, drug class pairing, HET pair, LASA note, KFA code format, and
// the per-company unique code (soft-delete aware index).
func TestInventoryDB_ItemConstraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "invdb.constraint", "081234571001")

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	tablet := unitID(t, ctx, tx, "tablet")

	const base = `INSERT INTO inventory.item
		(company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, non_kfa, non_kfa_reason,
		 drug_class, het_amount, het_effective_from, is_lasa, lasa_note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

	savepointExec(t, ctx, tx, "drug without kfa", "chk_item_kfa", base,
		companyID, "INV-1", "Paracetamol 500mg", "drug", "generic", tablet,
		nil, false, nil, "keras", nil, nil, false, nil)
	savepointExec(t, ctx, tx, "drug without class", "chk_item_drug_class", base,
		companyID, "INV-2", "Obat Tanpa Golongan", "drug", "generic", tablet,
		"93100015", false, nil, nil, nil, nil, false, nil)
	savepointExec(t, ctx, tx, "material with class", "chk_item_drug_class", base,
		companyID, "INV-3", "Kapas Medis", "material", "medical_device", tablet,
		nil, false, nil, "bebas", nil, nil, false, nil)
	savepointExec(t, ctx, tx, "het without date", "chk_item_het_pair", base,
		companyID, "INV-4", "Obat HET", "drug", "branded", tablet,
		"93100016", false, nil, "keras", 15000.00, nil, false, nil)
	savepointExec(t, ctx, tx, "lasa without note", "chk_item_lasa", base,
		companyID, "INV-5", "Obat LASA", "drug", "generic", tablet,
		"93100017", false, nil, "keras", nil, nil, true, nil)
	savepointExec(t, ctx, tx, "bad kfa format", "kfa_code", base,
		companyID, "INV-6", "KFA Salah Format", "drug", "generic", tablet,
		"12345678", false, nil, "keras", nil, nil, false, nil)

	valid := `INSERT INTO inventory.item
		(company_id, code, name, item_kind, pricing_category, base_unit_id, non_kfa, non_kfa_reason, drug_class)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	if _, err := tx.Exec(ctx, valid,
		companyID, "INV-OK", "Racikan Non-KFA", "drug", "generic", tablet, true, "preparasi racikan apotek", "keras"); err != nil {
		t.Fatalf("non_kfa=true with reason must insert, got %v", err)
	}
	savepointExec(t, ctx, tx, "duplicate code", "duplicate key", valid,
		companyID, "INV-OK", "Duplikat Kode", "drug", "generic", tablet, true, "preparasi racikan apotek", "keras")
}

// TestInventoryDB_UnitGuards — packaging factor and base-unit links are
// immutable, and the platform unit dictionary is read-only for app_runtime.
func TestInventoryDB_UnitGuards(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "invdb.unitguard", "081234571002")

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	tablet, box := unitID(t, ctx, tx, "tablet"), unitID(t, ctx, tx, "box")

	var itemID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'INV-UG','Amoksilin 500mg','drug','generic',$2,'93100020','keras') RETURNING id::text`,
		companyID, tablet).Scan(&itemID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	var baseRowID, boxRowID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item_unit (company_id, item_id, unit_id, factor, is_default_issue)
		VALUES ($1,$2,$3,1,true) RETURNING id::text`, companyID, itemID, tablet).Scan(&baseRowID); err != nil {
		t.Fatalf("insert base packaging: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item_unit (company_id, item_id, unit_id, factor, is_default_purchase)
		VALUES ($1,$2,$3,100,true) RETURNING id::text`, companyID, itemID, box).Scan(&boxRowID); err != nil {
		t.Fatalf("insert box packaging: %v", err)
	}

	savepointExec(t, ctx, tx, "change factor", "packaging factor cannot be changed",
		"UPDATE inventory.item_unit SET factor = 200 WHERE id = $1", boxRowID)
	savepointExec(t, ctx, tx, "deactivate base unit", "base unit packaging cannot be deactivated",
		"UPDATE inventory.item_unit SET is_active = false WHERE id = $1", baseRowID)
	savepointExec(t, ctx, tx, "change base unit", "base unit cannot be changed",
		"UPDATE inventory.item SET base_unit_id = $2 WHERE id = $1", itemID, box)
	savepointExec(t, ctx, tx, "runtime writes unit dictionary", "permission denied",
		"INSERT INTO inventory.unit (code, name, sort_order) VALUES ('tetes', 'Tetes', 31)")
}

// TestInventoryDB_CrossMerchantGuards — the item-level guard trigger runs as
// definer: an item active in merchant B's formulary cannot be deactivated from
// merchant A's session, and a rename propagates to merchant B's service item
// even though that row is invisible under RLS.
func TestInventoryDB_CrossMerchantGuards(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "invdb.crossm", "081234571003")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang Formularium")

	// Seed the item and merchant B's formulary row + billing service item.
	tx1 := runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	tablet := unitID(t, ctx, tx1, "tablet")
	var itemID string
	if err := tx1.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'INV-CM','Obat Lintas Merchant','drug','generic',$2,'93100030','keras') RETURNING id::text`,
		companyID, tablet).Scan(&itemID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	var serviceItemID string
	if err := tx1.QueryRow(ctx, `
		INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type)
		VALUES ($1,$2,'INV-CM','Obat Lintas Merchant','drug') RETURNING id::text`,
		companyID, merchantB).Scan(&serviceItemID); err != nil {
		t.Fatalf("insert service_item: %v", err)
	}
	if _, err := tx1.Exec(ctx, `
		INSERT INTO inventory.merchant_item (company_id, merchant_id, item_id, service_item_id)
		VALUES ($1,$2,$3,$4)`, companyID, merchantB, itemID, serviceItemID); err != nil {
		t.Fatalf("insert merchant_item: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// From merchant A's runtime session the formulary row of merchant B is
	// invisible, yet the guard still blocks deactivation and propagates renames.
	tx2 := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	var visible int
	if err := tx2.QueryRow(ctx, "SELECT count(*) FROM inventory.merchant_item").Scan(&visible); err != nil {
		t.Fatalf("count merchant_item as merchant A: %v", err)
	}
	if visible != 0 {
		t.Fatalf("merchant A must not see merchant B's formulary row, got %d", visible)
	}
	savepointExec(t, ctx, tx2, "deactivate item in formulary", "item is active in a formulary",
		"UPDATE inventory.item SET is_active = false WHERE id = $1", itemID)
	if _, err := tx2.Exec(ctx, "UPDATE inventory.item SET name = 'Obat Nama Baru' WHERE id = $1", itemID); err != nil {
		t.Fatalf("rename item as merchant A: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit rename: %v", err)
	}

	var newName string
	if err := pool.QueryRow(ctx, "SELECT name FROM core.service_item WHERE id = $1", serviceItemID).Scan(&newName); err != nil {
		t.Fatalf("read service_item: %v", err)
	}
	if newName != "Obat Nama Baru" {
		t.Fatalf("service_item of merchant B must follow the rename, got %q", newName)
	}
}

// TestInventoryDB_RLS — items are company-isolated and formulary rows are
// merchant-isolated under app_runtime.
func TestInventoryDB_RLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, merchantA, _, _, _ := seedAccountOwner(t, ctx, pool, router, "invdb.rls.a", "081234571004")
	companyB, merchantB, _, _, _ := seedAccountOwner(t, ctx, pool, router, "invdb.rls.b", "081234571005")

	seed := func(companyID, merchantID, code string) {
		t.Helper()
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		tablet := unitID(t, ctx, tx, "tablet")
		var itemID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
			VALUES ($1,$2,'Obat RLS','drug','generic',$3,'93100040','keras') RETURNING id::text`,
			companyID, code, tablet).Scan(&itemID); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		var serviceItemID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type)
			VALUES ($1,$2,$3,'Obat RLS','drug') RETURNING id::text`,
			companyID, merchantID, code).Scan(&serviceItemID); err != nil {
			t.Fatalf("insert service_item: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory.merchant_item (company_id, merchant_id, item_id, service_item_id)
			VALUES ($1,$2,$3,$4)`, companyID, merchantID, itemID, serviceItemID); err != nil {
			t.Fatalf("insert merchant_item: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit seed %s: %v", code, err)
		}
	}
	seed(companyA, merchantA, "INV-RLS-A")
	seed(companyB, merchantB, "INV-RLS-B")

	tx := runtimeTenantTx(t, ctx, pool, companyA, merchantA)
	var items, formulary int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM inventory.item").Scan(&items); err != nil {
		t.Fatalf("count items as company A: %v", err)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM inventory.merchant_item").Scan(&formulary); err != nil {
		t.Fatalf("count merchant_item as merchant A: %v", err)
	}
	if items != 1 {
		t.Fatalf("company A must see only its own item, got %d", items)
	}
	if formulary != 1 {
		t.Fatalf("merchant A must see only its own formulary row, got %d", formulary)
	}
}
