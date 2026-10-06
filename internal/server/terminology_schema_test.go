//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
)

// TestTerminologySchema_RuntimeReadOnly — spec 2026-10-01-terminology-import
// §3.4/§12 R5: app_runtime may read but never write terminology tables,
// including concept_map created after the default privileges.
func TestTerminologySchema_RuntimeReadOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	for _, stmt := range []string{
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test', 'T', '1')",
		"UPDATE terminology.concept SET display = display",
		"DELETE FROM terminology.concept_hierarchy",
		"DELETE FROM terminology.concept_map",
	} {
		tx := beginAsRuntime(t, ctx, pool)
		_, err := tx.Exec(ctx, stmt)
		// Free the connection now: the test pool holds only max(4, NumCPU)
		// connections and an open tx per statement would exhaust it (hang).
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "42501") {
			t.Fatalf("%q as app_runtime: want permission denied (42501), got %v", stmt, err)
		}
	}
	tx := beginAsRuntime(t, ctx, pool)
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM terminology.concept_map"); err != nil {
		t.Fatalf("app_runtime must read concept_map: %v", err)
	}
}

// TestTerminologySchema_Constraints — profile/properties must be JSON
// objects; one row per (map_set, source, target_code).
func TestTerminologySchema_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	var csID, conceptID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.code_system (system_uri, name, version) VALUES ('urn:test:schema', 'T', '1') RETURNING id::text").Scan(&csID); err != nil {
		t.Fatalf("seed code_system: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, 'A', 'A') RETURNING id::text", csID).Scan(&conceptID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}
	var selectable bool
	var props string
	if err := pool.QueryRow(ctx, "SELECT is_selectable, properties::text FROM terminology.concept WHERE id = $1", conceptID).Scan(&selectable, &props); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if !selectable || props != "{}" {
		t.Fatalf("concept defaults = %v %s, want true {}", selectable, props)
	}
	if _, err := pool.Exec(ctx, "UPDATE terminology.code_system SET profile = '[]'::jsonb WHERE id = $1", csID); err == nil {
		t.Fatal("profile array must violate chk_code_system_profile_object")
	}
	if _, err := pool.Exec(ctx, "UPDATE terminology.concept SET properties = '1'::jsonb WHERE id = $1", conceptID); err == nil {
		t.Fatal("properties scalar must violate chk_concept_properties_object")
	}
	insert := "INSERT INTO terminology.concept_map (map_set, source_concept_id, target_concept_id, target_code) VALUES ('s', $1, $1, 'A')"
	if _, err := pool.Exec(ctx, insert, conceptID); err != nil {
		t.Fatalf("first concept_map row: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, conceptID); err == nil {
		t.Fatal("duplicate (map_set, source, target_code) must violate uq_concept_map")
	}
}
