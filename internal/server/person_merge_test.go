//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// mergeCreatePerson creates a person through POST /persons (createPersonViaAPI
// path) with an arbitrary field map (NIK, birth_date, phone, …) and resolves
// the generated id + MRN from the DB.
func mergeCreatePerson(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID string, fields map[string]string) (id, mrn string) {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal person: %v", err)
	}
	rec := accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create person %v expected 2xx, got %d: %s", fields["full_name"], rec.Code, rec.Body.String())
	}
	return latestPerson(t, ctx, pool, companyID, fields["full_name"])
}

// mergeTestRouter builds the router with NIK encryption/search keys set —
// testConfig() leaves them empty and pgcrypto rejects an empty passphrase.
func mergeTestRouter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	cfg := testConfig()
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.NIKSearchKey = "test-search-key"
	return server.NewRouter(pool, newTestRedisClient(t, ctx), cfg)
}

func mergePreview(t *testing.T, router *gin.Engine, token, companyID, merchantID, survivorID, mergedID string) map[string]any {
	t.Helper()
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/merge-preview?survivor_id="+survivorID+"&merged_id="+mergedID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge preview expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return mergeRespData(t, rec)
}

func mergeExec(t *testing.T, router *gin.Engine, token, companyID, merchantID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal merge body: %v", err)
	}
	return accountRequest(router, http.MethodPost, "/api/v1/persons/merge", token, companyID, merchantID, raw)
}

func mergeUnmerge(t *testing.T, router *gin.Engine, token, companyID, merchantID, logID, reason string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		t.Fatalf("marshal unmerge body: %v", err)
	}
	return accountRequest(router, http.MethodPost, "/api/v1/persons/merge-logs/"+logID+"/unmerge", token, companyID, merchantID, raw)
}

func mergeRespData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	return out.Data
}

func mergeRespList(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var out struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list response %s: %v", rec.Body.String(), err)
	}
	return out.Data
}

// mergeRowVersions reads both row_versions straight from the DB (keeps the
// number of API calls under the test rate-limit burst).
func mergeRowVersions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, survivorID, mergedID string) map[string]any {
	t.Helper()
	read := func(id string) float64 {
		t.Helper()
		var v int32
		if err := pool.QueryRow(ctx, "SELECT row_version FROM core.person WHERE id = $1", id).Scan(&v); err != nil {
			t.Fatalf("row_version %s: %v", id, err)
		}
		return float64(v)
	}
	return map[string]any{"survivor": read(survivorID), "merged": read(mergedID)}
}

// mergeSide is a thin accessor over the preview data map.
func mergeSide(t *testing.T, data map[string]any, key string) map[string]any {
	t.Helper()
	side, ok := data[key].(map[string]any)
	if !ok {
		t.Fatalf("preview data.%s missing", key)
	}
	return side
}

// mergeUserSeq keeps generated uuid hex suffixes unique within a test.
var mergeUserSeq int

// mergeUserToken seeds an app_user with a role holding exactly permCodes and
// returns a bearer token for it (pattern from TestDepartmentTemplate_RequiresPermission).
func mergeUserToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, username string, permCodes []string) string {
	t.Helper()
	mergeUserSeq++
	suffix := fmt.Sprintf("%012x", mergeUserSeq)
	userID := "7c7c7c7c-7c7c-7c7c-7c7c-" + suffix
	roleID := "7d7d7d7d-7d7d-7d7d-7d7d-" + suffix
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, companyID, "Role "+username); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	for _, code := range permCodes {
		if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, p.id FROM core.permission p WHERE p.code = $2", roleID, code); err != nil {
			t.Fatalf("seed role_permission %s: %v", code, err)
		}
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, companyID, username, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", username, username+"-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return token
}

// mergeSeedAdmission inserts one active (registered) admission for personID.
func mergeSeedAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, personID, label string) {
	t.Helper()
	var deptID, payerID string
	if err := pool.QueryRow(ctx, "INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $4) RETURNING id::text",
		companyID, merchantID, "MRGT-"+label, "Poli "+label).Scan(&deptID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $4, 'self_pay') RETURNING id::text",
		companyID, merchantID, "MRGT-PAY-"+label, "Payer "+label).Scan(&payerID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, primary_payer_id, status) VALUES ($1, $2, $3, $4, 'outpatient', $5, $6, 'registered')",
		companyID, merchantID, "MRGT-"+label+"-1", personID, deptID, payerID); err != nil {
		t.Fatalf("seed admission: %v", err)
	}
}

// TestPersonMerge_FullFlow — spec §11: preview shows identity ok, a phone
// conflict and identity autofill; the merge needs the phone choice; afterwards
// the merged record is gone (404) and one merge log row exists.
func TestPersonMerge_FullFlow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mergeflow.owner", "081234601001")

	// A has a NIK, B does not; same birth date & gender, phones differ.
	// Survivor = B (no NIK) so the identity group lands in autofill.
	aID, aMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Flow Alpha", "gender": "female", "birth_date": "1980-01-01",
		"nik": "3201010101800001", "phone": "081111110001",
	})
	bID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Flow Beta", "gender": "female", "birth_date": "1980-01-01",
		"phone": "082222220001",
	})

	preview := mergePreview(t, router, token, companyID, merchantID, bID, aID)
	check := mergeSide(t, preview, "identity_check")
	if check["result"] != "ok" {
		t.Fatalf("identity_check.result = %v, want ok", check["result"])
	}
	conflictFields := map[string]bool{}
	for _, f := range preview["conflicts"].([]any) {
		conflictFields[f.(map[string]any)["field"].(string)] = true
	}
	if !conflictFields["phone"] {
		t.Fatalf("conflicts must contain phone, got %v", preview["conflicts"])
	}
	autofill := map[string]bool{}
	for _, f := range preview["autofill"].([]any) {
		autofill[f.(string)] = true
	}
	if !autofill["identity"] {
		t.Fatalf("autofill must contain identity, got %v", preview["autofill"])
	}
	versions := mergeSide(t, preview, "survivor")["row_version"].(float64)
	versionM := mergeSide(t, preview, "merged")["row_version"].(float64)

	// Without a phone choice the merge is rejected naming the field.
	base := map[string]any{
		"survivor_id": bID, "merged_id": aID, "reason": "duplikat",
		"expected_row_versions": map[string]any{"survivor": versions, "merged": versionM},
		"confirm_mrn":           aMRN,
	}
	noChoice := map[string]any{}
	for k, v := range base {
		noChoice[k] = v
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, noChoice); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "phone") {
		t.Fatalf("merge without phone choice expected 400 naming phone, got %d: %s", rec.Code, rec.Body.String())
	}

	// With the choice → 200.
	base["field_choices"] = map[string]string{"phone": "merged", "full_name": "survivor"}
	rec := mergeExec(t, router, token, companyID, merchantID, base)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := mergeRespData(t, rec)
	if data["survivor_id"] == nil || data["log_id"] == nil || data["moved_counts"] == nil {
		t.Fatalf("merge response missing keys: %s", rec.Body.String())
	}

	// The merged record is gone.
	if rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+aID, token, companyID, merchantID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET merged person expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Exactly one merge log row, visible in the list and per-person log.
	var actions []string
	rows, err := pool.Query(ctx, "SELECT action FROM core.person_merge_log WHERE surviving_person_id = $1 ORDER BY merged_at", bID)
	if err != nil {
		t.Fatalf("query merge log: %v", err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		actions = append(actions, a)
	}
	rows.Close()
	if len(actions) != 1 || actions[0] != "merge" {
		t.Fatalf("merge log actions = %v, want [merge]", actions)
	}
	if rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+bID+"/merge-logs", token, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET person merge-logs expected 200, got %d", rec.Code)
	} else if logs := mergeRespList(t, rec); len(logs) != 1 {
		t.Fatalf("person merge-logs expected 1 row, got %s", rec.Body.String())
	}
}

// mergeGuardPair creates a compatible pair (one NIK, one without, same
// demographics) and returns ids + MRN of the merged side + preview row versions.
func mergeGuardPair(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID, label, nik string) (survivorID, mergedID, mergedMRN string, versions map[string]any) {
	t.Helper()
	mergedID, mergedMRN = mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Guard " + label + " Merged", "gender": "female", "birth_date": "1980-01-01",
		"nik": nik,
	})
	survivorID, _ = mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Guard " + label + " Survivor", "gender": "female", "birth_date": "1980-01-01",
	})
	return survivorID, mergedID, mergedMRN, mergeRowVersions(t, ctx, pool, survivorID, mergedID)
}

// TestPersonMerge_Guards — every §7 guard: wrong MRN confirmation, stale row
// versions, differing NIKs, missing documents, active visit and a linked
// app_user on the merged record.
func TestPersonMerge_Guards(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mergeguard.owner", "081234601002")

	// confirm_mrn wrong → 400.
	sID, mID, mMRN, ver := mergeGuardPair(t, ctx, pool, router, token, companyID, merchantID, "MRN", "3201010101800011")
	body := map[string]any{
		"survivor_id": sID, "merged_id": mID, "reason": "x",
		"expected_row_versions": ver, "confirm_mrn": "WRONG",
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "confirmation does not match the medical record number") {
		t.Fatalf("wrong confirm_mrn expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Stale row_version → 409.
	body["confirm_mrn"] = mMRN
	stale := map[string]any{"survivor": ver["survivor"], "merged": float64(ver["merged"].(float64) + 1)}
	body["expected_row_versions"] = stale
	if rec := mergeExec(t, router, token, companyID, merchantID, body); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "patient data changed, reload the preview") {
		t.Fatalf("stale row_version expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Differing NIKs (same default identity type) → 409 identity_mismatch.
	s2, m2, mrn2, ver2 := mergeGuardPair(t, ctx, pool, router, token, companyID, merchantID, "NIK", "3201010101800012")
	if _, err := pool.Exec(ctx, `UPDATE core.person SET
		nik = encode(pgp_sym_encrypt('3201010101800099', 'test-nik-key'), 'base64'),
		nik_search_hash = encode(hmac('3201010101800099'::bytea, 'test-search-key'::bytea, 'sha256'), 'hex')
		WHERE id = $1`, s2); err != nil {
		t.Fatalf("give survivor a different NIK: %v", err)
	}
	// The update bumped the survivor's row_version — re-read.
	ver2 = mergeRowVersions(t, ctx, pool, s2, m2)
	body2 := map[string]any{
		"survivor_id": s2, "merged_id": m2, "reason": "x",
		"expected_row_versions": ver2, "confirm_mrn": mrn2,
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, body2); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "identity check failed") || !strings.Contains(rec.Body.String(), "identity_mismatch") {
		t.Fatalf("differing NIKs expected 409 identity_mismatch, got %d: %s", rec.Code, rec.Body.String())
	}

	// Both without NIK, no documents → 400.
	aID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Guard Docs A", "gender": "female", "birth_date": "1980-01-01",
	})
	bID, bMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Guard Docs B", "gender": "female", "birth_date": "1980-01-01",
	})
	preview := mergePreview(t, router, token, companyID, merchantID, aID, bID)
	body3 := map[string]any{
		"survivor_id": aID, "merged_id": bID, "reason": "x",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"],
			"merged":   mergeSide(t, preview, "merged")["row_version"],
		},
		"confirm_mrn": bMRN,
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, body3); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "verified documents are required") {
		t.Fatalf("no documents expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Active visit on the merged record → 409.
	s4, m4, mrn4, ver4 := mergeGuardPair(t, ctx, pool, router, token, companyID, merchantID, "VIS", "3201010101800013")
	mergeSeedAdmission(t, ctx, pool, companyID, merchantID, m4, "VIS")
	body4 := map[string]any{
		"survivor_id": s4, "merged_id": m4, "reason": "x",
		"expected_row_versions": ver4, "confirm_mrn": mrn4,
		"field_choices": map[string]string{"full_name": "survivor"},
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, body4); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "merged record has an active visit") {
		t.Fatalf("active visit expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Merged record linked to core.app_user.person_id → 409.
	s5, m5, mrn5, ver5 := mergeGuardPair(t, ctx, pool, router, token, companyID, merchantID, "USR", "3201010101800014")
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.app_user (id, company_id, username, password_hash, is_active, person_id) VALUES (gen_random_uuid(), $1, $2, 'x', true, $3)",
		companyID, "mergeguard.linked", m5); err != nil {
		t.Fatalf("seed linked app_user: %v", err)
	}
	body5 := map[string]any{
		"survivor_id": s5, "merged_id": m5, "reason": "x",
		"expected_row_versions": ver5, "confirm_mrn": mrn5,
		"field_choices": map[string]string{"full_name": "survivor"},
	}
	if rec := mergeExec(t, router, token, companyID, merchantID, body5); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "merged record is linked to a user account or physician; keep it as the surviving record") {
		t.Fatalf("linked account expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonMerge_Unmerge — unmerge restores the merged record with its
// snapshot NIK, resets the survivor's identity, writes both log rows, refuses
// a second unmerge and enforces last-in-first-out across a merge chain.
func TestPersonMerge_Unmerge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mergeun.owner", "081234601003")

	// A without NIK (survivor), B with NIK (merged) → identity autofill to A.
	bID, bMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Un Bravo", "gender": "female", "birth_date": "1980-01-01",
		"nik": "3201010101800021", "phone": "083333330001",
	})
	aID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Un Alpha", "gender": "female", "birth_date": "1980-01-01",
		"phone": "084444440001",
	})
	preview := mergePreview(t, router, token, companyID, merchantID, aID, bID)
	mergeRec := mergeExec(t, router, token, companyID, merchantID, map[string]any{
		"survivor_id": aID, "merged_id": bID, "reason": "duplikat",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"],
			"merged":   mergeSide(t, preview, "merged")["row_version"],
		},
		"field_choices": map[string]string{"phone": "survivor", "full_name": "survivor"},
		"confirm_mrn":   bMRN,
	})
	if mergeRec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", mergeRec.Code, mergeRec.Body.String())
	}
	mergeLogID := mergeRespData(t, mergeRec)["log_id"].(string)

	// Unmerge.
	rec := mergeUnmerge(t, router, token, companyID, merchantID, mergeLogID, "salah gabung")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if data := mergeRespData(t, rec); data["identity_cleared"] != false {
		t.Fatalf("identity_cleared = %v, want false", data["identity_cleared"])
	}
	// survivor_snapshot is projected to the compared fields only — the raw
	// snapshot (encrypted NIK ciphertext) must never come back.
	snap := mergeRespData(t, rec)["survivor_snapshot"].(map[string]any)
	if snap["full_name"] != "Merge Un Alpha" {
		t.Fatalf("survivor_snapshot.full_name = %v, want Merge Un Alpha", snap["full_name"])
	}
	if _, has := snap["nik"]; has {
		t.Fatalf("survivor_snapshot must not expose the raw snapshot: %v", snap)
	}

	// B alive again with its own NIK; A back without NIK.
	getNIK := func(personID string) string {
		t.Helper()
		rec := accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET person %s expected 200, got %d: %s", personID, rec.Code, rec.Body.String())
		}
		var out struct {
			Data struct {
				Nik string `json:"nik"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode person: %v", err)
		}
		return out.Data.Nik
	}
	if got := getNIK(bID); got != "3201010101800021" {
		t.Fatalf("B nik after unmerge = %q, want original", got)
	}
	if got := getNIK(aID); got != "" {
		t.Fatalf("A nik after unmerge = %q, want empty", got)
	}

	// Logs: one merge (reverted) + one unmerge.
	var status string
	if err := pool.QueryRow(ctx, "SELECT CASE WHEN reverted_at IS NULL THEN 'open' ELSE 'reverted' END FROM core.person_merge_log WHERE id = $1", mergeLogID).Scan(&status); err != nil || status != "reverted" {
		t.Fatalf("merge log reverted status = %q (%v), want reverted", status, err)
	}
	logs := mergeRespList(t, accountRequest(router, http.MethodGet, "/api/v1/persons/"+aID+"/merge-logs", token, companyID, merchantID, nil))
	actions := map[string]int{}
	for _, r := range logs {
		actions[r.(map[string]any)["action"].(string)]++
	}
	if actions["merge"] != 1 || actions["unmerge"] != 1 {
		t.Fatalf("log actions = %v, want one merge + one unmerge", actions)
	}

	// Second unmerge → 409.
	if rec := mergeUnmerge(t, router, token, companyID, merchantID, mergeLogID, "lagi"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "merge already reverted or not a merge") {
		t.Fatalf("second unmerge expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Chain A2←B2 then A2←C2: unmerging B2 first must be refused.
	c2ID, c2MRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Un Charlie", "gender": "female", "birth_date": "1985-05-05",
	})
	a2ID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Un Chain A", "gender": "female", "birth_date": "1985-05-05",
	})
	b2ID, b2MRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Un Chain B", "gender": "female", "birth_date": "1985-05-05",
	})
	docs := []string{"ktp"}
	doMerge := func(survivor, merged, mrn string, rowVer map[string]any) string {
		t.Helper()
		rec := mergeExec(t, router, token, companyID, merchantID, map[string]any{
			"survivor_id": survivor, "merged_id": merged, "reason": "rantai",
			"expected_row_versions": rowVer, "verified_documents": docs, "confirm_mrn": mrn,
			"field_choices": map[string]string{"full_name": "survivor"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chain merge expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		return mergeRespData(t, rec)["log_id"].(string)
	}
	previewB := mergePreview(t, router, token, companyID, merchantID, a2ID, b2ID)
	logB := doMerge(a2ID, b2ID, b2MRN, map[string]any{
		"survivor": mergeSide(t, previewB, "survivor")["row_version"],
		"merged":   mergeSide(t, previewB, "merged")["row_version"],
	})
	previewC := mergePreview(t, router, token, companyID, merchantID, a2ID, c2ID)
	doMerge(a2ID, c2ID, c2MRN, map[string]any{
		"survivor": mergeSide(t, previewC, "survivor")["row_version"],
		"merged":   mergeSide(t, previewC, "merged")["row_version"],
	})
	if rec := mergeUnmerge(t, router, token, companyID, merchantID, logB, "b dulu"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "undo the later merge first") {
		t.Fatalf("chain unmerge expected 409 undo-later, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonMerge_UnmergeSameIdentity — a same-NIK merge (both rows carry the
// same hash, e.g. legacy data) unmerges with identity_cleared: the merged
// record comes back WITHOUT a NIK, the survivor keeps its own.
func TestPersonMerge_UnmergeSameIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mergesame.owner", "081234601004")

	aID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Same Alpha", "gender": "male", "birth_date": "1990-02-02",
		"nik": "3201010101900001",
	})
	bID, bMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Same Beta", "gender": "male", "birth_date": "1990-02-02",
	})
	// Legacy duplicate: copy the identity columns of A onto B (drop both NIK
	// uniqueness indexes: the pre-000066 uq_person_nik and 000066's
	// uq_person_nik_hash, so the test is stable before and after Task 4).
	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS core.uq_person_nik_hash"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS core.uq_person_nik"); err != nil {
		t.Fatalf("drop old index: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.person SET nik = a.nik, nik_search_hash = a.nik_search_hash FROM core.person a WHERE a.id = $1 AND core.person.id = $2", aID, bID); err != nil {
		t.Fatalf("copy NIK: %v", err)
	}

	// Survivor = A (holds the NIK), merged = B (same hash copied on).
	preview := mergePreview(t, router, token, companyID, merchantID, aID, bID)
	check := mergeSide(t, preview, "identity_check")
	if check["result"] != "same_identity" {
		t.Fatalf("identity_check.result = %v, want same_identity", check["result"])
	}
	mergeRec := mergeExec(t, router, token, companyID, merchantID, map[string]any{
		"survivor_id": aID, "merged_id": bID, "reason": "nik sama",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"],
			"merged":   mergeSide(t, preview, "merged")["row_version"],
		},
		"field_choices": map[string]string{"full_name": "survivor"},
		"confirm_mrn":   bMRN,
	})
	if mergeRec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", mergeRec.Code, mergeRec.Body.String())
	}
	logID := mergeRespData(t, mergeRec)["log_id"].(string)

	rec := mergeUnmerge(t, router, token, companyID, merchantID, logID, "salah nik")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmerge expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if data := mergeRespData(t, rec); data["identity_cleared"] != true {
		t.Fatalf("identity_cleared = %v, want true", data["identity_cleared"])
	}
	getNIK := func(personID string) string {
		t.Helper()
		got := accountRequest(router, http.MethodGet, "/api/v1/persons/"+personID, token, companyID, merchantID, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("GET person %s expected 200, got %d: %s", personID, got.Code, got.Body.String())
		}
		var out struct {
			Data struct {
				Nik string `json:"nik"`
			} `json:"data"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Data.Nik
	}
	if got := getNIK(bID); got != "" {
		t.Fatalf("survivor B nik = %q, want empty", got)
	}
	if got := getNIK(aID); got != "3201010101900001" {
		t.Fatalf("restored A nik = %q, want original", got)
	}
}

// TestPersonMerge_RequiresPermission — 403 without core.person.merge for
// preview/merge and without core.person.unmerge for unmerge.
func TestPersonMerge_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mergeperm.owner", "081234601005")
	xID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Perm X", "gender": "female", "birth_date": "1970-07-07",
	})
	yID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Perm Y", "gender": "female", "birth_date": "1970-07-07",
	})

	noPerm := mergeUserToken(t, ctx, pool, companyID, merchantID, "mergeperm.nope", nil)
	rec := accountRequest(router, http.MethodGet, "/api/v1/persons/merge-preview?survivor_id="+xID+"&merged_id="+yID, noPerm, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preview without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = mergeExec(t, router, noPerm, companyID, merchantID, map[string]any{
		"survivor_id": xID, "merged_id": yID, "reason": "x",
		"expected_row_versions": map[string]any{"survivor": 1, "merged": 1}, "confirm_mrn": "X",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("merge without permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	mergeOnly := mergeUserToken(t, ctx, pool, companyID, merchantID, "mergeperm.only", []string{server.PermPersonMerge})
	rec = mergeUnmerge(t, router, mergeOnly, companyID, merchantID, "00000000-0000-0000-0000-000000000000", "x")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unmerge without unmerge permission expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	// The merge-only user CAN preview (sanity: the 403 above is the missing
	// unmerge permission, not a broken token).
	if rec := accountRequest(router, http.MethodGet, "/api/v1/persons/merge-preview?survivor_id="+xID+"&merged_id="+yID, mergeOnly, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("preview with merge permission expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonMerge_PreviewNotLive — merge-preview refuses a soft-deleted
// or already-merged side (spec 2026-10-10-merge-review-fixes §1): a
// stale URL must 404 instead of rendering a full preview.
func TestPersonMerge_PreviewNotLive(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := mergeTestRouter(t, ctx, pool)

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "mprevlive.owner", "081234601006")

	// Soft-deleted side (stale merge URL).
	aID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Prev Alpha", "gender": "female", "birth_date": "1980-01-01",
	})
	bID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Prev Beta", "gender": "female", "birth_date": "1980-01-01",
	})
	if _, err := pool.Exec(ctx,
		"UPDATE core.person SET deleted_at = now(), deleted_by = gen_random_uuid() WHERE id = $1", bID); err != nil {
		t.Fatalf("soft-delete person: %v", err)
	}
	for _, pair := range [][2]string{{aID, bID}, {bID, aID}} {
		rec := accountRequest(router, http.MethodGet,
			"/api/v1/persons/merge-preview?survivor_id="+pair[0]+"&merged_id="+pair[1], token, companyID, merchantID, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "person not found") {
			t.Fatalf("preview with soft-deleted side (%s,%s) expected 404 person not found, got %d: %s",
				pair[0], pair[1], rec.Code, rec.Body.String())
		}
	}

	// Already-merged side: a real merge first, then the old pair 404s.
	cID, cMRN := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Prev Ceta", "gender": "female", "birth_date": "1980-01-01",
		"nik": "3201010101800031", "phone": "081111110031",
	})
	dID, _ := mergeCreatePerson(t, ctx, pool, router, token, companyID, merchantID, map[string]string{
		"full_name": "Merge Prev Delta", "gender": "female", "birth_date": "1980-01-01",
		"phone": "082222220031",
	})
	preview := mergePreview(t, router, token, companyID, merchantID, dID, cID)
	mergeRec := mergeExec(t, router, token, companyID, merchantID, map[string]any{
		"survivor_id": dID, "merged_id": cID, "reason": "duplikat",
		"expected_row_versions": map[string]any{
			"survivor": mergeSide(t, preview, "survivor")["row_version"],
			"merged":   mergeSide(t, preview, "merged")["row_version"],
		},
		"field_choices": map[string]string{"phone": "merged", "full_name": "survivor"},
		"confirm_mrn":   cMRN,
	})
	if mergeRec.Code != http.StatusOK {
		t.Fatalf("merge expected 200, got %d: %s", mergeRec.Code, mergeRec.Body.String())
	}
	for _, pair := range [][2]string{{dID, cID}, {cID, dID}} {
		rec := accountRequest(router, http.MethodGet,
			"/api/v1/persons/merge-preview?survivor_id="+pair[0]+"&merged_id="+pair[1], token, companyID, merchantID, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("preview with merged side (%s,%s) expected 404, got %d: %s",
				pair[0], pair[1], rec.Code, rec.Body.String())
		}
	}
}
