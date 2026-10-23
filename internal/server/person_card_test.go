//go:build integration

package server_test

// Integration tests for the patient card API (spec
// 2026-10-20-qr-visit-media-design.md §4.2, §4.4, §4.5): issue/re-issue on
// POST /persons/:id/card, resolve on POST /persons/resolve-card (incl. merge
// chain), invalid code, and permission isolation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// pcCard mirrors the card object of POST /persons/:id/card.
type pcCard struct {
	Token string  `json:"token"`
	QrPNG *string `json:"qr_png"`
}

type pcIssueResp struct {
	Data struct {
		Card pcCard `json:"card"`
	} `json:"data"`
}

// pcIssue calls POST /persons/:id/card as the fixture owner.
func pcIssue(f *vmFixture, personID string) (int, pcIssueResp) {
	f.t.Helper()
	var resp pcIssueResp
	code := f.vmPost(http.MethodPost, "/api/v1/persons/"+personID+"/card", nil, &resp)
	return code, resp
}

// pcResolve calls POST /persons/resolve-card as the fixture owner and returns
// (status, data object; nil on non-200, non-201).
func pcResolve(f *vmFixture, code string) (int, map[string]any) {
	f.t.Helper()
	var env struct {
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	raw, _ := json.Marshal(map[string]any{"code": code})
	rec := accountRequest(f.router, http.MethodPost, "/api/v1/persons/resolve-card",
		f.token, f.companyID, f.merchantID, raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		f.t.Fatalf("decode resolve-card response %q: %v", rec.Body.String(), err)
	}
	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		return rec.Code, env.Data
	}
	return rec.Code, map[string]any{"error": env.Error}
}

// TestPersonCardIssueAndResolve — issue returns an NQP1 token + QR; resolving
// that code returns the same person_id (spec §4.2, §4.4).
func TestPersonCardIssueAndResolve(t *testing.T) {
	f := vmNewFixture(t, "card1")
	person := f.vmSeedPerson("Siti Aminah")

	code, resp := pcIssue(f, person)
	if code != http.StatusCreated {
		t.Fatalf("POST card expected 201, got %d", code)
	}
	tokenRe := regexp.MustCompile(`^NQP1-[0-9A-HJKMNP-TV-Z]{16}$`)
	if !tokenRe.MatchString(resp.Data.Card.Token) {
		t.Errorf("token %q does not match NQP1 pattern", resp.Data.Card.Token)
	}
	if resp.Data.Card.QrPNG == nil || !strings.HasPrefix(*resp.Data.Card.QrPNG, "data:image/png;base64,") {
		t.Errorf("qr_png = %v, want data:image/png;base64 prefix", resp.Data.Card.QrPNG)
	}

	status, out := pcResolve(f, resp.Data.Card.Token)
	if status != http.StatusOK {
		t.Fatalf("resolve expected 200, got %d: %v", status, out)
	}
	if out["person_id"] != person {
		t.Errorf("resolve person_id = %v, want %s", out["person_id"], person)
	}
	if out["full_name"] != "Siti Aminah" {
		t.Errorf("resolve full_name = %v, want Siti Aminah", out["full_name"])
	}
}

// TestPersonCardReplaced — a second issue overwrites the hash: the first card
// stops resolving (404 card not recognized), the second resolves (spec §4.2).
func TestPersonCardReplaced(t *testing.T) {
	f := vmNewFixture(t, "card2")
	person := f.vmSeedPerson("Pasien Kartu Ganda")

	_, first := pcIssue(f, person)
	code, second := pcIssue(f, person)
	if code != http.StatusCreated {
		t.Fatalf("second issue expected 201, got %d", code)
	}
	status1, _ := pcResolve(f, first.Data.Card.Token)
	if status1 != http.StatusNotFound {
		t.Fatalf("resolve replaced card expected 404, got %d", status1)
	}
	status2, out2 := pcResolve(f, second.Data.Card.Token)
	if status2 != http.StatusOK {
		t.Fatalf("resolve new card expected 200, got %d: %v", status2, out2)
	}
	if out2["person_id"] != person {
		t.Errorf("resolve person_id = %v, want %s", out2["person_id"], person)
	}
}

// TestPersonCardMergeFollows — a card issued to person B still resolves after
// B is merged into A, pointing at the survivor A (spec §4.4). Merge is seeded
// directly via SQL (queue_engine_test.go pattern).
func TestPersonCardMergeFollows(t *testing.T) {
	f := vmNewFixture(t, "card3")
	survivor := f.vmSeedPerson("Pasien A Survivor")
	merged := f.vmSeedPerson("Pasien B Merged")

	code, resp := pcIssue(f, merged)
	if code != http.StatusCreated {
		t.Fatalf("issue card for B expected 201, got %d", code)
	}
	if _, err := f.pool.Exec(f.ctx,
		"UPDATE core.person SET merged_into_person_id = $1, deleted_at = now(), deleted_by = $2 WHERE id = $3",
		survivor, "11111111-1111-1111-1111-111111111111", merged); err != nil {
		t.Fatalf("seed merge: %v", err)
	}

	status, out := pcResolve(f, resp.Data.Card.Token)
	if status != http.StatusOK {
		t.Fatalf("resolve merged card expected 200, got %d: %v", status, out)
	}
	if out["person_id"] != survivor {
		t.Errorf("resolve person_id = %v, want survivor %s", out["person_id"], survivor)
	}
}

// TestPersonCardInvalidCode — a code with the wrong prefix (NQV1 = visit
// token) is rejected 400 invalid token (spec §4.4).
func TestPersonCardInvalidCode(t *testing.T) {
	f := vmNewFixture(t, "card4")

	status, out := pcResolve(f, "NQV1-0123456789ABCDEF")
	if status != http.StatusBadRequest {
		t.Fatalf("resolve NQV1 code expected 400, got %d: %v", status, out)
	}
	if out["error"] != "invalid token" {
		t.Errorf("error = %v, want invalid token", out["error"])
	}
}

// TestPersonCardPermissionDenied — a user without core.person.manage gets 403
// (mfa_test.go seedTenantNonAdminUser pattern with an empty role).
func TestPersonCardPermissionDenied(t *testing.T) {
	f := vmNewFixture(t, "card5")
	person := f.vmSeedPerson("Pasien Tanpa Izin")

	roleID := "7d7d7d7d-7d7d-7d7d-7d7d-7d7d7d7d7d7d"
	userID := "7c7c7c7c-7c7c-7c7c-7c7c-7c7c7c7c7c7c"
	if _, err := f.pool.Exec(f.ctx,
		"INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Person')", roleID, f.companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		"INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)",
		userID, f.companyID, "card5.staff", mustHashPassword(t, f.ctx)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		"INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)",
		userID, f.merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		"UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", f.companyID); err != nil {
		t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"username": "card5.staff", "password": "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", f.companyID)
	req.Header.Set("X-Device-Id", "pcard-staff")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var login struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}

	staffRec := accountRequest(f.router, http.MethodPost,
		"/api/v1/persons/"+person+"/card", login.Data.Token, f.companyID, f.merchantID, nil)
	if staffRec.Code != http.StatusForbidden {
		t.Fatalf("issue card without core.person.manage expected 403, got %d: %s", staffRec.Code, staffRec.Body.String())
	}
}

// mustHashPassword hashes "correct-horse" with the test hasher.
func mustHashPassword(t *testing.T, ctx context.Context) string {
	t.Helper()
	hash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return hash
}
