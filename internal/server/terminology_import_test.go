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

// TestTermImport_MapClustersAndDots — spec §12 R3/R4: a cluster target is
// stored on its stem concept with the full code; ICD-10 codes match with or
// without the dot; unknown codes are skipped, not fatal.
func TestTermImport_MapClustersAndDots(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	icd10 := termimport.CodeSystem{URI: "http://hl7.org/fhir/sid/icd-10", Name: "ICD-10", Version: "legacy"}
	icd11 := termimport.CodeSystem{URI: "http://id.who.int/icd/release/11/mms", Name: "ICD-11", Version: "2026-01"}
	if _, err := termimport.LoadConcepts(ctx, pool, icd10, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "A000", Display: "Cholera O1 (undotted legacy code)", Selectable: true},
		{Code: "A00.9", Display: "Cholera", Selectable: true},
	}}, false); err != nil {
		t.Fatalf("load ICD-10: %v", err)
	}
	if _, err := termimport.LoadConcepts(ctx, pool, icd11, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "1A00", Display: "Cholera", Selectable: true},
		{Code: "1A01", Display: "Other", Selectable: true},
	}}, false); err != nil {
		t.Fatalf("load ICD-11: %v", err)
	}
	sum, err := termimport.LoadMap(ctx, pool, "who-icd10-to-icd11-test", icd10.URI, icd11.URI, []termimport.MapRow{
		{SourceCode: "A00.0", TargetCode: "1A00&XN8P1"},
		{SourceCode: "A00.9", TargetCode: "1A00", Preferred: true},
		{SourceCode: "A00.9", TargetCode: "1A01"},
		{SourceCode: "Z99.9", TargetCode: "1A00"},
	}, false)
	if err != nil {
		t.Fatalf("LoadMap: %v", err)
	}
	if sum.New != 3 || len(sum.Skipped) != 1 {
		t.Fatalf("map summary = %+v, want 3 new and 1 skipped (Z99.9)", sum)
	}
	var targetCode, stem string
	if err := pool.QueryRow(ctx, `SELECT cm.target_code, t.code FROM terminology.concept_map cm
		JOIN terminology.concept s ON s.id = cm.source_concept_id
		JOIN terminology.concept t ON t.id = cm.target_concept_id
		WHERE cm.map_set = 'who-icd10-to-icd11-test' AND s.code = 'A000'`).Scan(&targetCode, &stem); err != nil {
		t.Fatalf("cluster row for undotted A000: %v", err)
	}
	if targetCode != "1A00&XN8P1" || stem != "1A00" {
		t.Fatalf("cluster row = %s on %s, want 1A00&XN8P1 on stem 1A00", targetCode, stem)
	}
	again, err := termimport.LoadMap(ctx, pool, "who-icd10-to-icd11-test", icd10.URI, icd11.URI, []termimport.MapRow{
		{SourceCode: "A00.9", TargetCode: "1A00", Preferred: true},
	}, false)
	if err != nil {
		t.Fatalf("LoadMap again: %v", err)
	}
	if again.Updated != 1 || again.Deactivated != 2 {
		t.Fatalf("re-map summary = %+v, want 1 updated and 2 deactivated", again)
	}
}

// TestTermImport_ReverseMapDotlessTarget — R4 on the target side: an
// ICD-11→ICD-10 pair whose ICD-10 code is stored without the dot resolves.
func TestTermImport_ReverseMapDotlessTarget(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	icd10 := termimport.CodeSystem{URI: "http://hl7.org/fhir/sid/icd-10", Name: "ICD-10", Version: "legacy"}
	icd11 := termimport.CodeSystem{URI: "http://id.who.int/icd/release/11/mms", Name: "ICD-11", Version: "2026-01"}
	if _, err := termimport.LoadConcepts(ctx, pool, icd10, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "A000", Display: "Cholera O1 (undotted legacy code)", Selectable: true},
	}}, false); err != nil {
		t.Fatalf("load ICD-10: %v", err)
	}
	if _, err := termimport.LoadConcepts(ctx, pool, icd11, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "1A00", Display: "Cholera", Selectable: true},
	}}, false); err != nil {
		t.Fatalf("load ICD-11: %v", err)
	}
	sum, err := termimport.LoadMap(ctx, pool, "who-icd11-to-icd10-test", icd11.URI, icd10.URI, []termimport.MapRow{
		{SourceCode: "1A00", TargetCode: "A00.0", Preferred: true},
	}, false)
	if err != nil {
		t.Fatalf("LoadMap: %v", err)
	}
	if sum.New != 1 || len(sum.Skipped) != 0 {
		t.Fatalf("reverse map summary = %+v, want 1 new and none skipped", sum)
	}
	var targetCode string
	if err := pool.QueryRow(ctx, `SELECT target_code FROM terminology.concept_map WHERE map_set = 'who-icd11-to-icd10-test'`).Scan(&targetCode); err != nil {
		t.Fatalf("read map: %v", err)
	}
	if targetCode != "A00.0" {
		t.Fatalf("target_code = %q, want the code as written in the WHO file", targetCode)
	}
}
