//go:build integration

package server_test

import (
	"context"
	"testing"
)

// TestPermissionCatalog_NoDuplicates verifies migration 000044: the 4 legacy
// duplicates of core.* codes and the unused procurement.po.create are gone,
// while the 5 reserved codes for planned features and the 4 core.* equivalents
// that received the moved grants each remain exactly once.
func TestPermissionCatalog_NoDuplicates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	removed := []string{
		"master.person.manage",
		"master.service_item.manage",
		"rbac.user.manage",
		"rbac.role.manage",
		"procurement.po.create",
	}
	var removedCount int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.permission WHERE code = ANY($1::text[])", removed,
	).Scan(&removedCount); err != nil {
		t.Fatalf("failed to count removed codes: %v", err)
	}
	if removedCount != 0 {
		t.Errorf("expected the 5 removed codes to be gone, found %d rows", removedCount)
	}

	expected := []string{
		"billing.invoice.create",
		"billing.invoice.view",
		"clinical.order.write",
		"audit.log.view",
		"notification.manage",
		"core.person.manage",
		"core.tariff.manage",
		"core.user.manage",
		"core.role.manage",
	}
	rows, err := pool.Query(ctx,
		"SELECT code, count(*) FROM core.permission WHERE code = ANY($1::text[]) GROUP BY code", expected)
	if err != nil {
		t.Fatalf("failed to count expected codes: %v", err)
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var code string
		var n int64
		if err := rows.Scan(&code, &n); err != nil {
			t.Fatalf("failed to scan row: %v", err)
		}
		counts[code] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("failed to iterate rows: %v", err)
	}
	for _, code := range expected {
		if counts[code] != 1 {
			t.Errorf("code %s: expected exactly 1 row, got %d", code, counts[code])
		}
	}
}
