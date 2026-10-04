//go:build integration

package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestDisplayBoard_* covers display board CRUD, token issue/revoke and the
// token-protected, rate-limited device feed (plan #23 task 3 kontrak §6).

var dbPhoneSeq atomic.Int64

// dbNewEnv is qeNewEnv with a non-zero display rate limit — testConfig()
// leaves RateLimitDisplayPerMinute at 0, which would 429 the very first feed
// request.
func dbNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.RateLimitDisplayPerMinute = 60
	router := server.NewRouter(pool, redisClient, cfg)
	phone := fmt.Sprintf("0812397%05d", dbPhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dbfeed."+label, phone)
	return &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}
}

func dbFeedRequest(t *testing.T, router *gin.Engine, boardID, token, companyID, merchantID string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/display/boards/"+boardID+"/feed", nil)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	if token != "" {
		req.Header.Set("X-Display-Token", token)
	}
	// No Authorization header on purpose: the feed route has no AuthMiddleware.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func TestDisplayBoard_CRUDAndTokenLifecycle(t *testing.T) {
	env := dbNewEnv(t, "board")

	// Create.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/display-boards", map[string]any{
		"merchant_id": env.merchantID, "name": "Layar Utama", "layout": "single",
		"name_display": "initials", "poll_seconds": 5, "show_next_n": 3,
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /display-boards expected 201, got %d: %v", code, resp)
	}
	board := resp["data"].(map[string]any)
	boardID := board["id"].(string)
	if _, leaked := board["token_hash"]; leaked {
		t.Fatalf("board response must never include token_hash: %v", board)
	}
	if board["name_display"] != "initials" {
		t.Fatalf("board defaults/payload unexpected: %v", board)
	}
	if _, ok := board["layout"]; ok {
		t.Fatalf("board response must not include layout: %v", board)
	}

	// Create without name_display → default "initials" (migration 000058).
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards", map[string]any{
		"merchant_id": env.merchantID, "name": "Layar Default",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /display-boards (no name_display) expected 201, got %d: %v", code, resp)
	}
	if got := resp["data"].(map[string]any)["name_display"]; got != "initials" {
		t.Fatalf("name_display default: expected \"initials\", got %v", got)
	}

	// Invalid enum → 400.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards", map[string]any{
		"merchant_id": env.merchantID, "name": "Layar Rusak", "layout": "hologram",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid layout expected 400, got %d: %v", code, resp)
	}

	// Issue token.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards/"+boardID+"/token", nil)
	if code != http.StatusOK {
		t.Fatalf("POST token expected 200, got %d: %v", code, resp)
	}
	token := resp["data"].(map[string]any)["token"].(string)
	if len(token) != 43 {
		t.Fatalf("token must be 32 bytes base64url (43 chars), got %d: %q", len(token), token)
	}

	// Only the sha256 hex is stored — never the raw token.
	sum := sha256.Sum256([]byte(token))
	wantHash := hex.EncodeToString(sum[:])
	var storedHash string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT token_hash FROM operations.display_board WHERE id = $1", boardID).Scan(&storedHash); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if storedHash != wantHash {
		t.Fatalf("stored hash mismatch: expected sha256 hex %q, got %q", wantHash, storedHash)
	}
	if strings.Contains(storedHash, token) {
		t.Fatal("raw token must never be stored")
	}

	// Feed with correct token → 200.
	code, resp, _ = dbFeedRequest(t, env.router, boardID, token, env.companyID, env.merchantID)
	if code != http.StatusOK {
		t.Fatalf("feed with valid token expected 200, got %d", code)
	}
	cfg := resp["data"].(map[string]any)["board"].(map[string]any)
	if _, ok := cfg["layout"]; ok {
		t.Fatalf("feed board config must not include layout: %v", cfg)
	}
	if cfg["poll_seconds"] != float64(5) || cfg["show_next_n"] != float64(3) {
		t.Fatalf("feed board config unexpected: %v", cfg)
	}

	// Wrong token / missing token → 401.
	code, _, _ = dbFeedRequest(t, env.router, boardID, "wrong-token-aaaaaaaaaaaaaaaaaaaa", env.companyID, env.merchantID)
	if code != http.StatusUnauthorized {
		t.Fatalf("feed with wrong token expected 401, got %d", code)
	}
	code, _, _ = dbFeedRequest(t, env.router, boardID, "", env.companyID, env.merchantID)
	if code != http.StatusUnauthorized {
		t.Fatalf("feed without token expected 401, got %d", code)
	}

	// Revoke → feed 401 again.
	code, resp = qeRequest(t, env, http.MethodDelete, "/api/v1/display-boards/"+boardID+"/token", nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE token expected 204, got %d: %v", code, resp)
	}
	code, _, _ = dbFeedRequest(t, env.router, boardID, token, env.companyID, env.merchantID)
	if code != http.StatusUnauthorized {
		t.Fatalf("feed with revoked token expected 401, got %d", code)
	}

	// Delete board.
	code, resp = qeRequest(t, env, http.MethodDelete, "/api/v1/display-boards/"+boardID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE board expected 204, got %d: %v", code, resp)
	}
}

func TestDisplayBoard_FeedPayload(t *testing.T) {
	env := dbNewEnv(t, "feed")

	// Flow with admission + physician stages.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Feed",
		"stages": []map[string]any{
			{"seq": 1, "name": "Admisi", "kind": "admission", "served_by_permission": "operations.counter.manage"},
			{"seq": 2, "name": "Dokter", "kind": "physician", "served_by_permission": "operations.visit.manage"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("seed flow expected 201, got %d: %v", code, resp)
	}
	flowData := resp["data"].(map[string]any)
	flowID := flowData["id"].(string)
	stages := flowData["stages"].([]any)
	admissionStage := stages[0].(map[string]any)["id"].(string)

	deptID := env.seedDepartment(t, "FEED")
	payerID := env.seedPayer(t, "FEED", "self_pay")

	// Patient A gets called; patient B waits.
	personA := env.seedPerson(t, "Budi Santoso")
	admissionA := env.qeAdmit(t, deptID, personA, payerID, flowID, http.StatusCreated)
	personB := env.seedPerson(t, "Siti Aminah")
	env.qeAdmit(t, deptID, personB, payerID, flowID, http.StatusCreated)

	ticketA := env.qeFirstWaitingTicket(t, admissionA)
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue/"+ticketA, map[string]string{"status": "called"})
	if code != http.StatusOK {
		t.Fatalf("call ticket A expected 200, got %d: %v", code, resp)
	}

	// Board over the admission stage, initials mode.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards", map[string]any{
		"merchant_id": env.merchantID, "name": "Layar Feed", "name_display": "initials",
		"stage_ids": []string{admissionStage},
	})
	if code != http.StatusCreated {
		t.Fatalf("seed board expected 201, got %d: %v", code, resp)
	}
	boardID := resp["data"].(map[string]any)["id"].(string)
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards/"+boardID+"/token", nil)
	if code != http.StatusOK {
		t.Fatalf("token expected 200, got %d: %v", code, resp)
	}
	token := resp["data"].(map[string]any)["token"].(string)

	code, resp, raw := dbFeedRequest(t, env.router, boardID, token, env.companyID, env.merchantID)
	if code != http.StatusOK {
		t.Fatalf("feed expected 200, got %d", code)
	}
	data := resp["data"].(map[string]any)
	current := data["current"].([]any)
	if len(current) != 1 {
		t.Fatalf("expected 1 current ticket, got %d: %s", len(current), raw)
	}
	cur := current[0].(map[string]any)
	if cur["patient_label"] != "B. S." {
		t.Fatalf("initials label: expected \"B. S.\", got %v", cur["patient_label"])
	}
	if cur["stage_name"] != "Admisi" {
		t.Fatalf("expected stage_name Admisi, got %v", cur["stage_name"])
	}
	if cur["queue_number"] == nil || cur["queue_number"] == "" {
		t.Fatalf("expected queue_number, got %v", cur["queue_number"])
	}
	waiting := data["waiting"].([]any)
	if len(waiting) != 1 {
		t.Fatalf("expected 1 waiting ticket, got %d: %s", len(waiting), raw)
	}

	// merchant_name = name of the seeded merchant.
	var merchantName string
	if err := env.pool.QueryRow(t.Context(),
		"SELECT name FROM core.merchant WHERE id = $1", env.merchantID).Scan(&merchantName); err != nil {
		t.Fatalf("read merchant name: %v", err)
	}
	if data["board"].(map[string]any)["merchant_name"] != merchantName {
		t.Fatalf("merchant_name: expected %q, got %v", merchantName, data["board"].(map[string]any)["merchant_name"])
	}

	// summary counts the FULL ticket slice, not the truncated waiting list.
	// Seed: 1 called (A) + 1 waiting (B), both in stage "Admisi" →
	// waiting:1, called:1, in_progress:0, active_total = called+in_progress = 1;
	// one stage group "Admisi" {waiting:1, called:1, in_progress:0}.
	summary := data["summary"].(map[string]any)
	if summary["waiting"] != float64(1) || summary["called"] != float64(1) ||
		summary["in_progress"] != float64(0) || summary["active_total"] != float64(1) {
		t.Fatalf("summary top-level counts unexpected: %v", summary)
	}
	stagesSum := summary["stages"].([]any)
	if len(stagesSum) != 1 {
		t.Fatalf("expected 1 summary stage, got %d: %s", len(stagesSum), raw)
	}
	sg := stagesSum[0].(map[string]any)
	if sg["name"] != "Admisi" || sg["waiting"] != float64(1) ||
		sg["called"] != float64(1) || sg["in_progress"] != float64(0) {
		t.Fatalf("summary stage unexpected: %v", sg)
	}

	// No patient identifiers anywhere in the payload. Fields are checked as
	// quoted JSON keys so substrings inside values (e.g. "Klinik" contains
	// "nik") don't false-positive.
	for _, forbidden := range []string{"person_id", "full_name", "medical_record", "birth_date", "photo", "nik", "patient_id"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("feed leaks forbidden field %q: %s", forbidden, raw)
		}
	}

	// masked mode: "Siti Aminah" → "S*** A***".
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/display-boards/"+boardID, map[string]any{
		"name": "Layar Feed", "name_display": "masked", "stage_ids": []string{admissionStage},
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH board expected 200, got %d: %v", code, resp)
	}
	code, resp, raw = dbFeedRequest(t, env.router, boardID, token, env.companyID, env.merchantID)
	if code != http.StatusOK {
		t.Fatalf("feed after patch expected 200, got %d", code)
	}
	waiting = resp["data"].(map[string]any)["waiting"].([]any)
	if got := waiting[0].(map[string]any)["patient_label"]; got != "S*** A***" {
		t.Fatalf("masked label: expected \"S*** A***\", got %v", got)
	}
}

func TestDisplayBoard_FeedRateLimit(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	// Bucket of exactly 1 request per minute per IP+board → 2nd request is 429.
	cfg := testConfig()
	cfg.RateLimitDisplayPerMinute = 1
	router := server.NewRouter(pool, redisClient, cfg)
	phone := fmt.Sprintf("0812398%05d", dbPhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "dbfeed.ratelimit", phone)
	env := &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}

	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/display-boards", map[string]any{
		"merchant_id": merchantID, "name": "Layar Limit",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed board expected 201, got %d: %v", code, resp)
	}
	boardID := resp["data"].(map[string]any)["id"].(string)
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/display-boards/"+boardID+"/token", nil)
	if code != http.StatusOK {
		t.Fatalf("token expected 200, got %d: %v", code, resp)
	}
	deviceToken := resp["data"].(map[string]any)["token"].(string)

	code, _, _ = dbFeedRequest(t, router, boardID, deviceToken, companyID, merchantID)
	if code != http.StatusOK {
		t.Fatalf("first feed request expected 200, got %d", code)
	}
	code, _, raw := dbFeedRequest(t, router, boardID, deviceToken, companyID, merchantID)
	if code != http.StatusTooManyRequests {
		t.Fatalf("second feed request expected 429, got %d: %s", code, raw)
	}
}
