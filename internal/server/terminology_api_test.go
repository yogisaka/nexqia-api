//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
	"github.com/yogisaka/nexqia-api/internal/termimport"
)

// TestTerminologyAPI — spec §6: profile is a JSON object, search hides
// non-selectable concepts, and conversion candidates follow the auto rule.
func TestTerminologyAPI(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "term.api", "081234080193")

	src := termimport.CodeSystem{URI: "urn:test:src", Name: "SRC", Version: "1", Tags: []string{"diagnosis"}}
	dst := termimport.CodeSystem{URI: "urn:test:dst", Name: "DST", Version: "1"}
	if _, err := termimport.LoadConcepts(ctx, pool, src, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "S1", Display: "One target", Selectable: true},
		{Code: "S2", Display: "Two targets", Selectable: true},
		{Code: "S3", Display: "Cluster target", Selectable: true},
		{Code: "S4", Display: "Unselectable target", Selectable: true},
		{Code: "CH", Display: "Chapter", Selectable: false},
	}}, false); err != nil {
		t.Fatalf("load SRC: %v", err)
	}
	if _, err := termimport.LoadConcepts(ctx, pool, dst, termimport.ParseResult{Concepts: []termimport.Concept{
		{Code: "D1", Display: "Dest one", Selectable: true, Properties: map[string]any{"linearization_uri": "http://x/D1"}},
		{Code: "D2", Display: "Dest two", Selectable: true},
		{Code: "DX", Display: "Extension", Selectable: false},
	}}, false); err != nil {
		t.Fatalf("load DST: %v", err)
	}
	if _, err := termimport.LoadMap(ctx, pool, "test-src-dst", src.URI, dst.URI, []termimport.MapRow{
		{SourceCode: "S1", TargetCode: "D1", Preferred: true},
		{SourceCode: "S2", TargetCode: "D1"}, {SourceCode: "S2", TargetCode: "D2", Preferred: true},
		{SourceCode: "S3", TargetCode: "D1&DX"},
		{SourceCode: "S4", TargetCode: "DX"},
	}, false); err != nil {
		t.Fatalf("load map: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE terminology.code_system SET profile = '{"status":"optional"}' WHERE system_uri = 'urn:test:src'`); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	get := func(path string) (int, map[string]any) {
		rec := accountRequest(router, http.MethodGet, path, token, companyID, merchantID, nil)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, resp := get("/api/v1/terminology/code-systems?tag=diagnosis")
	if code != http.StatusOK {
		t.Fatalf("code-systems = %d %v", code, resp)
	}
	found := false
	for _, it := range resp["data"].([]any) {
		item := it.(map[string]any)
		if item["Name"] != "SRC" {
			continue
		}
		found = true
		profile, ok := item["Profile"].(map[string]any)
		if !ok || profile["status"] != "optional" || item["SystemURI"] != "urn:test:src" {
			t.Fatalf("SRC item = %v, want Profile as an object and SystemURI", item)
		}
	}
	if !found {
		t.Fatal("SRC missing from code-systems?tag=diagnosis")
	}

	code, resp = get("/api/v1/terminology/search?system=SRC&q=Chapter")
	if code != http.StatusOK || len(resp["data"].([]any)) != 0 {
		t.Fatalf("search must hide non-selectable concepts: %d %v", code, resp)
	}

	for _, tc := range []struct {
		src       string
		auto      bool
		n         int
		firstCode string
	}{
		{"S1", true, 1, "D1"},
		{"S2", false, 2, "D2"},
		{"S3", false, 1, "D1&DX"},
		{"S4", false, 1, "DX"},
	} {
		code, resp = get("/api/v1/terminology/map?system=SRC&code=" + tc.src + "&target=DST")
		if code != http.StatusOK {
			t.Fatalf("map %s = %d %v", tc.src, code, resp)
		}
		data := resp["data"].(map[string]any)
		cands := data["candidates"].([]any)
		if data["auto"] != tc.auto || len(cands) != tc.n || cands[0].(map[string]any)["code"] != tc.firstCode {
			t.Fatalf("map %s = %v, want auto=%v n=%d first=%s", tc.src, data, tc.auto, tc.n, tc.firstCode)
		}
	}
	first := func(src string) map[string]any {
		_, r := get("/api/v1/terminology/map?system=SRC&code=" + src + "&target=DST")
		return r["data"].(map[string]any)["candidates"].([]any)[0].(map[string]any)
	}
	if c := first("S1"); c["uri"] != "http://x/D1" || c["is_cluster"] != false {
		t.Fatalf("S1 candidate = %v", c)
	}
	if c := first("S3"); c["is_cluster"] != true {
		t.Fatalf("S3 candidate must be a cluster: %v", c)
	}

	if code, _ := get("/api/v1/terminology/map?system=SRC&code=NOPE&target=DST"); code != http.StatusNotFound {
		t.Fatalf("unknown code = %d, want 404", code)
	}
	if code, _ := get("/api/v1/terminology/map?system=SRC&code=S1"); code != http.StatusBadRequest {
		t.Fatalf("missing target = %d, want 400", code)
	}
}
