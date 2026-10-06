//go:build integration

package server_test

import (
	"context"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/termimport"
)

var termTestSystem = termimport.CodeSystem{
	URI: "urn:test:icd", Name: "TESTICD", Version: "1", Tags: []string{"diagnosis"},
	License: "test", SourceURL: "https://example.test/r.zip", SourceSHA256: "00",
}

func termTestRelease(codes ...string) termimport.ParseResult {
	res := termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "CH", Display: "Chapter", Selectable: false, Properties: map[string]any{"class_kind": "chapter"}},
	}}
	for _, code := range codes {
		res.Concepts = append(res.Concepts, termimport.Concept{Code: code, Display: "Display " + code, Selectable: true,
			Properties: map[string]any{"class_kind": "category"}})
		res.Edges = append(res.Edges, termimport.Edge{Parent: "CH", Child: code})
	}
	return res
}

// TestTermImport_IdempotentAndDeactivates — spec §4: re-import changes
// nothing; a code missing from the next release is deactivated, not deleted.
func TestTermImport_IdempotentAndDeactivates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	first, err := termimport.LoadConcepts(ctx, pool, termTestSystem, termTestRelease("A1", "A2"), false)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.New != 3 || first.Updated != 0 || first.Edges != 2 {
		t.Fatalf("first load = %+v, want 3 new, 2 edges", first)
	}
	again, err := termimport.LoadConcepts(ctx, pool, termTestSystem, termTestRelease("A1", "A2"), false)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if again.New != 0 || again.Updated != 3 || again.Deactivated != 0 || again.Edges != 2 {
		t.Fatalf("re-import = %+v, want 0 new, 3 updated, 0 deactivated, 2 edges", again)
	}
	next, err := termimport.LoadConcepts(ctx, pool, termTestSystem, termTestRelease("A1"), false)
	if err != nil {
		t.Fatalf("release without A2: %v", err)
	}
	if next.Deactivated != 1 {
		t.Fatalf("release without A2 = %+v, want 1 deactivated", next)
	}
	var active bool
	if err := pool.QueryRow(ctx, `SELECT c.is_active FROM terminology.concept c
		JOIN terminology.code_system cs ON cs.id = c.code_system_id
		WHERE cs.system_uri = 'urn:test:icd' AND c.code = 'A2'`).Scan(&active); err != nil {
		t.Fatalf("A2 must still exist: %v", err)
	}
	if active {
		t.Fatal("A2 must be inactive")
	}
	var edges int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM terminology.concept_hierarchy h
		JOIN terminology.concept c ON c.id = h.child_concept_id
		JOIN terminology.code_system cs ON cs.id = c.code_system_id
		WHERE cs.system_uri = 'urn:test:icd'`).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 1 {
		t.Fatalf("hierarchy edges = %d, want 1 (rebuilt from the latest release)", edges)
	}
}

// TestTermImport_DryRunAndTags — dry-run writes nothing; tags merge.
func TestTermImport_DryRunAndTags(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	if _, err := termimport.LoadConcepts(ctx, pool, termTestSystem, termTestRelease("B1"), true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM terminology.code_system WHERE system_uri = 'urn:test:icd'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("dry run must not write")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO terminology.code_system (system_uri, name, version, tags) VALUES ('urn:test:icd', 'TESTICD', '0', '{legacy}')"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := termimport.LoadConcepts(ctx, pool, termTestSystem, termTestRelease("B1"), false); err != nil {
		t.Fatalf("load: %v", err)
	}
	var hasLegacy, hasDiagnosis bool
	if err := pool.QueryRow(ctx, "SELECT 'legacy' = ANY(tags), 'diagnosis' = ANY(tags) FROM terminology.code_system WHERE system_uri = 'urn:test:icd'").Scan(&hasLegacy, &hasDiagnosis); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if !hasLegacy || !hasDiagnosis {
		t.Fatalf("tags must merge: legacy=%v diagnosis=%v", hasLegacy, hasDiagnosis)
	}
}
