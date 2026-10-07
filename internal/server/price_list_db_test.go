//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

func serverRouterForPriceList(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	return server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
}

// price list fixture: one company/merchant pair from the account flow.
func priceListFixture(t *testing.T) (context.Context, *pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := serverRouterForPriceList(t, ctx, pool)
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "price.db", "081234080301")
	return ctx, pool, companyID, merchantID
}

func insertPriceList(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, code string, baseID *string) (string, error) {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		"INSERT INTO core.price_list (company_id, merchant_id, code, name, base_price_list_id, adjustment_percent) VALUES ($1, $2, $3, $3, $4, CASE WHEN $4::uuid IS NULL THEN 0 ELSE 10 END) RETURNING id::text",
		companyID, merchantID, code, baseID).Scan(&id)
	return id, err
}

// TestPriceListDB_OneLevel — spec §3.2: a derived list cannot be a base, and
// a base of other lists cannot become derived.
func TestPriceListDB_OneLevel(t *testing.T) {
	ctx, pool, companyID, merchantID := priceListFixture(t)
	base, err := insertPriceList(t, ctx, pool, companyID, merchantID, "umum", nil)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	derived, err := insertPriceList(t, ctx, pool, companyID, merchantID, "eksekutif", &base)
	if err != nil {
		t.Fatalf("derived: %v", err)
	}
	if _, err := insertPriceList(t, ctx, pool, companyID, merchantID, "turunan2", &derived); err == nil || !strings.Contains(err.Error(), "23514") {
		t.Fatalf("derived-of-derived: want check_violation 23514, got %v", err)
	}
	other, err := insertPriceList(t, ctx, pool, companyID, merchantID, "lain", nil)
	if err != nil {
		t.Fatalf("other base: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.price_list SET base_price_list_id = $1, adjustment_percent = 5 WHERE id = $2", other, base); err == nil || !strings.Contains(err.Error(), "23514") {
		t.Fatalf("base-of-others becoming derived: want 23514, got %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.price_list SET adjustment_percent = 5 WHERE id = $1", base); err == nil {
		t.Fatal("a base list must not carry an adjustment (chk_price_list_base_no_adjustment)")
	}
}

// TestPriceListDB_TemplateSeeded — 34 Permenkes 3/2023 rows, spot-checked values.
func TestPriceListDB_TemplateSeeded(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.template_price_item i JOIN core.template t ON t.id = i.template_id
		WHERE t.code = 'bpjs_nonkapitasi_2023'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 34 {
		t.Fatalf("template items = %d, want 34", n)
	}
	for code, want := range map[string]string{"ANC-BIDAN": "70000.00", "PERSALINAN-TIM-DR": "1200000.00", "KB-SUNTIK": "20000.00", "PRB-MICROALBUMIN": "120000.00"} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT i.amount_max::text FROM core.template_price_item i JOIN core.template t ON t.id = i.template_id
			WHERE t.code = 'bpjs_nonkapitasi_2023' AND i.code = $1`, code).Scan(&got); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if got != want {
			t.Fatalf("%s amount = %s, want %s", code, got, want)
		}
	}
}
