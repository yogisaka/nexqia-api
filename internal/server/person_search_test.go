//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestPersonsSearch_ExcludesPersonWithoutMRN — SearchPersonsByName must only
// surface persons that carry a medical record number (plan 2026-10-22
// user-with-person, task 1): bare person rows created for app_user accounts
// must never appear in /persons/search results.
func TestPersonsSearch_ExcludesPersonWithoutMRN(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "srch.mrm", "081234570233")

	// Person WITH a medical record number — searchable.
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender, medical_record_no) VALUES ('a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', $1, 'Search Dengan Mrn', 'female', 'SRCH-MRN-001')", companyID); err != nil {
		t.Fatalf("seed person with MRN: %v", err)
	}
	// Person WITHOUT an MRN — must be invisible to the search.
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ('b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2', $1, 'Search Tanpa Mrn', 'male')", companyID); err != nil {
		t.Fatalf("seed person without MRN: %v", err)
	}

	// q=Search: only the MRN-bearing person (the owner's account person has no
	// MRN either, so it must not appear).
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/search?q=Search", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search q=Search expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var byName struct {
		Data []struct {
			ID       string `json:"ID"`
			FullName string `json:"FullName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &byName); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	if len(byName.Data) != 1 || byName.Data[0].FullName != "Search Dengan Mrn" {
		t.Fatalf("q=Search expected only 'Search Dengan Mrn', got: %s", rec.Body.String())
	}

	// q=SRCH-MRN-001: exactly one item, the MRN-bearing person.
	rec = accountRequest(router, http.MethodGet, "/api/v1/persons/search?q=SRCH-MRN-001", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search q=SRCH-MRN-001 expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &byName); err != nil {
		t.Fatalf("decode MRN search response: %v", err)
	}
	if len(byName.Data) != 1 || byName.Data[0].FullName != "Search Dengan Mrn" {
		t.Fatalf("q=SRCH-MRN-001 expected 1 item 'Search Dengan Mrn', got: %s", rec.Body.String())
	}

	// q=Search Tanpa: the MRN-less person must not be found at all.
	rec = accountRequest(router, http.MethodGet, "/api/v1/persons/search?q=Search%20Tanpa", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search q=Search Tanpa expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &byName); err != nil {
		t.Fatalf("decode no-MRN search response: %v", err)
	}
	if len(byName.Data) != 0 {
		t.Fatalf("q=Search Tanpa expected 0 items, got: %s", rec.Body.String())
	}
}
