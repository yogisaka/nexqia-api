//go:build integration

package server_test

import (
	"context"
	"testing"
)

// TestMasterDataRefreshMigration — spec 2026-11-04-master-data-terminologi-refresh
// §2: migration 000093 menghapus code_system legacy option/citizenship dan
// men-anchor ISO 3166-1 sebagai satu-satunya sistem kewarganegaraan.
// CI memakai DB fresh (migrations tanpa seed), sehingga bagian remap/hapus
// legacy migration berjalan no-op dan tetap harus sukses.
func TestMasterDataRefreshMigration(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM terminology.code_system WHERE system_uri = 'http://nexqia.local/option/citizenship'",
	).Scan(&n); err != nil {
		t.Fatalf("count legacy option/citizenship: %v", err)
	}
	if n != 0 {
		t.Fatalf("legacy option/citizenship code_system count = %d, want 0", n)
	}

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM terminology.code_system WHERE name = 'citizenship'",
	).Scan(&n); err != nil {
		t.Fatalf("count code_system name=citizenship: %v", err)
	}
	if n != 1 {
		t.Fatalf("code_system name=citizenship count = %d, want 1 (ISO anchor)", n)
	}

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM terminology.concept c
		JOIN terminology.code_system cs ON cs.id = c.code_system_id
		WHERE cs.system_uri = 'urn:iso:std:iso:3166:-1'`).Scan(&n); err != nil {
		t.Fatalf("count ISO 3166-1 concepts: %v", err)
	}
	if n != 1 {
		t.Fatalf("ISO 3166-1 concept count = %d, want 1", n)
	}

	var code, display string
	if err := pool.QueryRow(ctx, `SELECT c.code, c.display FROM terminology.concept c
		JOIN terminology.code_system cs ON cs.id = c.code_system_id
		WHERE cs.system_uri = 'urn:iso:std:iso:3166:-1'`).Scan(&code, &display); err != nil {
		t.Fatalf("read ISO 3166-1 anchor concept: %v", err)
	}
	if code != "ID" || display != "Indonesia" {
		t.Fatalf("ISO anchor concept = (%q, %q), want (ID, Indonesia)", code, display)
	}
}
