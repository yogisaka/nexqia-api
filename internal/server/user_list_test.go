//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// userListResp decodes GET /api/v1/users (ListAppUsersHandler).
type userListResp struct {
	Data []map[string]any `json:"data"`
	Meta struct {
		Limit  int   `json:"limit"`
		Offset int   `json:"offset"`
		Total  int64 `json:"total"`
	} `json:"meta"`
	Error string `json:"error"`
}

// getUserList calls GET /api/v1/users with the given raw query string
// (e.g. "q=budi&limit=1") using the owner token headers.
func getUserList(t *testing.T, router http.Handler, token, companyID, merchantID, rawQuery string) (*httptest.ResponseRecorder, userListResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users?"+rawQuery, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var resp userListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list response %q: %v", rec.Body.String(), err)
	}
	return rec, resp
}

// usernames extracts the username column of each row.
func usernames(rows []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if u, ok := r["username"].(string); ok {
			out[u] = true
		}
	}
	return out
}

// TestAppUsersListFilterPagination — filters (q per column, role_id,
// merchant_id, combination, status), pagination meta, and row shape of
// GET /api/v1/users (plan 2026-11-02-users-management Task 1).
func TestAppUsersListFilterPagination(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool) // NIK keys needed by with-person

	companyID, merchantID, ownerID, token, _ := seedAccountOwner(t, ctx, pool, router, "ulist.owner", "081234590001")

	// Seed role of the owner (register creates it; has core.user.manage).
	var seedRoleID, seedRoleName string
	if err := pool.QueryRow(ctx,
		"SELECT r.id, r.name FROM core.user_merchant_role umr JOIN core.role r ON r.id = umr.role_id WHERE umr.user_id = $1 ORDER BY r.name LIMIT 1", ownerID,
	).Scan(&seedRoleID, &seedRoleName); err != nil {
		t.Fatalf("fetch seed role: %v", err)
	}

	// Second merchant M2 and second role R2 in the same company.
	m2 := "6a6a6a6a-6a6a-6a6a-6a6a-6a6a6a6a6a6a"
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'ulist-m2', 'Merchant Dua')", m2, companyID); err != nil {
		t.Fatalf("seed merchant M2: %v", err)
	}
	r2 := "6b6b6b6b-6b6b-6b6b-6b6b-6b6b6b6b6b6b"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Direktur Klinik')", r2, companyID); err != nil {
		t.Fatalf("seed role R2: %v", err)
	}

	// U1 budi — with email + person; assignments (seed role, M1) + (R2, M2).
	budi := map[string]string{
		"company_id": companyID, "full_name": "Budi Santoso", "gender": "male",
		"nik": "3201010101900001", "birth_date": "1990-01-01", "gender_": "",
		"email": "budi@klinik.example", "username": "budi", "password": "Passw0rd123!",
	}
	delete(budi, "gender_")
	if rec := postWithPersonUser(t, router, token, companyID, merchantID, budi); rec.Code != http.StatusCreated {
		t.Fatalf("create budi expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var budiID string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.app_user WHERE company_id = $1 AND username = 'budi'", companyID).Scan(&budiID); err != nil {
		t.Fatalf("fetch budi id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", budiID, merchantID, seedRoleID); err != nil {
		t.Fatalf("assign budi (seed role, M1): %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", budiID, m2, r2); err != nil {
		t.Fatalf("assign budi (R2, M2): %v", err)
	}

	// U2 siti — full_name only, no email; assignment (R2, M1).
	siti := map[string]string{
		"company_id": companyID, "full_name": "Siti Aminah", "gender": "female",
		"nik": "3201010102900002", "birth_date": "1992-02-02",
		"username": "siti", "password": "Passw0rd123!",
	}
	if rec := postWithPersonUser(t, router, token, companyID, merchantID, siti); rec.Code != http.StatusCreated {
		t.Fatalf("create siti expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sitiID string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.app_user WHERE company_id = $1 AND username = 'siti'", companyID).Scan(&sitiID); err != nil {
		t.Fatalf("fetch siti id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", sitiID, merchantID, r2); err != nil {
		t.Fatalf("assign siti (R2, M1): %v", err)
	}

	// U3 ops1 — created with person, then person detached + deactivated; no assignment.
	ops1 := map[string]string{
		"company_id": companyID, "full_name": "Ops Satu", "gender": "female",
		"nik": "3201010103900003", "birth_date": "1993-03-03",
		"username": "ops1", "password": "Passw0rd123!",
	}
	if rec := postWithPersonUser(t, router, token, companyID, merchantID, ops1); rec.Code != http.StatusCreated {
		t.Fatalf("create ops1 expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := pool.Exec(ctx, "UPDATE core.app_user SET person_id = NULL, is_active = false WHERE username = 'ops1' AND company_id = $1", companyID); err != nil {
		t.Fatalf("detach ops1 person: %v", err)
	}

	// T = users created by this test (budi, siti, ops1) + owner = 4.
	const totalUsers = 4

	// q per column.
	if _, resp := getUserList(t, router, token, companyID, merchantID, "q=budi"); !usernames(resp.Data)["budi"] || resp.Meta.Total == 0 {
		t.Fatalf("q=budi should contain budi, got %v", resp.Data)
	}
	if _, resp := getUserList(t, router, token, companyID, merchantID, "q=klinik.example"); !usernames(resp.Data)["budi"] {
		t.Fatalf("q=klinik.example (email) should contain budi, got %v", resp.Data)
	}
	if _, resp := getUserList(t, router, token, companyID, merchantID, "q=Aminah"); !usernames(resp.Data)["siti"] {
		t.Fatalf("q=Aminah (full_name) should contain siti, got %v", resp.Data)
	}
	if _, resp := getUserList(t, router, token, companyID, merchantID, "q=Santoso"); !usernames(resp.Data)["budi"] {
		t.Fatalf("q=Santoso (full_name) should contain budi, got %v", resp.Data)
	}
	if _, resp := getUserList(t, router, token, companyID, merchantID, "q=tidakada"); resp.Meta.Total != 0 || len(resp.Data) != 0 {
		t.Fatalf("q=tidakada expected total 0, got %d rows total %d", len(resp.Data), resp.Meta.Total)
	}

	// role_id filter.
	if _, resp := getUserList(t, router, token, companyID, merchantID, "role_id="+r2); !usernames(resp.Data)["budi"] || !usernames(resp.Data)["siti"] {
		t.Fatalf("role_id=R2 should contain budi and siti, got %v", resp.Data)
	}
	if _, resp := getUserList(t, router, token, companyID, merchantID, "role_id="+r2); usernames(resp.Data)["ops1"] {
		t.Fatalf("role_id=R2 must not contain ops1, got %v", resp.Data)
	}

	// role_id = seed role (owner's role): only owner and budi.
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "role_id="+seedRoleID)
		names := usernames(resp.Data)
		if !names["budi"] || names["siti"] || names["ops1"] {
			t.Fatalf("role_id=seed should contain budi but not siti/ops1, got %v", resp.Data)
		}
	}

	// Combination role_id + merchant_id.
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "role_id="+r2+"&merchant_id="+merchantID)
		names := usernames(resp.Data)
		if !names["siti"] || names["budi"] {
			t.Fatalf("role_id=R2&merchant_id=M1 should contain only siti, got %v", resp.Data)
		}
	}
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "role_id="+r2+"&merchant_id="+m2)
		names := usernames(resp.Data)
		if !names["budi"] || names["siti"] {
			t.Fatalf("role_id=R2&merchant_id=M2 should contain only budi, got %v", resp.Data)
		}
	}

	// status filter.
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "status=nonaktif")
		names := usernames(resp.Data)
		if !names["ops1"] || names["budi"] || names["siti"] {
			t.Fatalf("status=nonaktif should contain only ops1, got %v", resp.Data)
		}
	}
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "status=aktif")
		names := usernames(resp.Data)
		if !names["budi"] || !names["siti"] || names["ops1"] {
			t.Fatalf("status=aktif should contain budi and siti but not ops1, got %v", resp.Data)
		}
	}

	// pagination.
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "limit=1")
		if len(resp.Data) != 1 || resp.Meta.Total != int64(totalUsers) {
			t.Fatalf("limit=1 expected 1 row total %d, got %d rows total %d", totalUsers, len(resp.Data), resp.Meta.Total)
		}
		if resp.Meta.Limit != 1 {
			t.Fatalf("meta.limit should echo 1, got %d", resp.Meta.Limit)
		}
	}
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, fmt.Sprintf("limit=200&offset=%d", totalUsers))
		if len(resp.Data) != 0 || resp.Meta.Total != int64(totalUsers) {
			t.Fatalf("offset=T expected empty data total %d, got %d rows total %d", totalUsers, len(resp.Data), resp.Meta.Total)
		}
		if resp.Meta.Offset != totalUsers {
			t.Fatalf("meta.offset should echo %d, got %d", totalUsers, resp.Meta.Offset)
		}
	}
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "limit=200")
		if len(resp.Data) != totalUsers || resp.Meta.Total != int64(totalUsers) {
			t.Fatalf("limit=200 expected %d rows total %d, got %d rows total %d", totalUsers, totalUsers, len(resp.Data), resp.Meta.Total)
		}
		if resp.Meta.Limit != 200 {
			t.Fatalf("meta.limit should echo 200, got %d", resp.Meta.Limit)
		}
		// Row shape checks on the full page.
		for _, row := range resp.Data {
			for _, forbidden := range []string{"password_hash", "mfa_secret", "PasswordHash", "MfaSecret"} {
				if _, bad := row[forbidden]; bad {
					t.Fatalf("row %v must not expose %s", row["username"], forbidden)
				}
			}
		}
		var opsRow map[string]any
		var budiRow map[string]any
		for _, row := range resp.Data {
			switch row["username"] {
			case "ops1":
				opsRow = row
			case "budi":
				budiRow = row
			}
		}
		if opsRow == nil || budiRow == nil {
			t.Fatalf("full page must contain ops1 and budi rows, got %v", resp.Data)
		}
		if _, ok := opsRow["full_name"]; ok && opsRow["full_name"] != nil {
			t.Fatalf("ops1 full_name expected nil, got %v", opsRow["full_name"])
		}
		opsRoles, ok := opsRow["roles"].([]any)
		if !ok || len(opsRoles) != 0 {
			t.Fatalf("ops1 roles expected [], got %v", opsRow["roles"])
		}
		if budiRow["is_active"] != true {
			t.Fatalf("budi is_active expected true, got %v", budiRow["is_active"])
		}
		rolesRaw, ok := budiRow["roles"].([]any)
		if !ok || len(rolesRaw) != 2 {
			t.Fatalf("budi roles expected 2 entries, got %v", budiRow["roles"])
		}
		hasSeed, hasR2 := false, false
		sorted := true
		var prev string
		for i, rr := range rolesRaw {
			name, _ := rr.(string)
			if name == seedRoleName {
				hasSeed = true
			}
			if name == "Direktur Klinik" {
				hasR2 = true
			}
			if i > 0 && prev > name {
				sorted = false
			}
			prev = name
		}
		if !hasSeed || !hasR2 {
			t.Fatalf("budi roles expected seed role %q and Direktur Klinik, got %v", seedRoleName, rolesRaw)
		}
		if !sorted {
			t.Fatalf("budi roles expected sorted, got %v", rolesRaw)
		}
	}

	// Unfiltered default: owner sees T users including itself.
	{
		_, resp := getUserList(t, router, token, companyID, merchantID, "")
		if resp.Meta.Total != int64(totalUsers) {
			t.Fatalf("unfiltered total expected %d, got %d", totalUsers, resp.Meta.Total)
		}
		if resp.Meta.Limit != 50 || resp.Meta.Offset != 0 {
			t.Fatalf("unfiltered meta expected limit 50 offset 0, got %d/%d", resp.Meta.Limit, resp.Meta.Offset)
		}
	}
}

// postWithPersonUser posts a with-person payload map to the endpoint.
func postWithPersonUser(t *testing.T, router http.Handler, token, companyID, merchantID string, payload map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := payload["phone"]; !ok {
		payload["phone"] = "081234590099"
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/with-person", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestAppUsersListInvalidParams — malformed role_id/merchant_id/status return
// 400 with the exact error message.
func TestAppUsersListInvalidParams(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "ulist.bad", "081234590101")

	cases := []struct{ query, msg string }{
		{"status=bogus", "invalid status"},
		{"role_id=xyz", "invalid role_id"},
		{"merchant_id=xyz", "invalid merchant_id"},
	}
	for _, tc := range cases {
		rec, resp := getUserList(t, router, token, companyID, merchantID, tc.query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s expected 400, got %d: %s", tc.query, rec.Code, rec.Body.String())
		}
		if resp.Error != tc.msg {
			t.Fatalf("%s expected error %q, got %q", tc.query, tc.msg, resp.Error)
		}
	}
}
