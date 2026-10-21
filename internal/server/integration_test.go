//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/connect"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestSatusehat* covers the SATUSEHAT API (Permenkes 6/2026 Fase C2, spec
// 2026-10-20-permenkes-6-2026-compliance-design §5): config upsert with
// pgp_sym_encrypt-ed credentials, explicit Organization/Location re-enqueue,
// worker push against a deterministic stub endpoint, the resource status list
// and the integration.status.view / integration.config.manage permissions.

// satRequest is one captured stub request.
type satRequest struct {
	Method string
	Path   string
	Body   string
}

// satStub is a deterministic SATUSEHAT endpoint: token endpoint plus FHIR
// create (POST → generated id) / update (PUT → id from URL).
type satStub struct {
	mu       sync.Mutex
	requests []satRequest
	seq      int
	srv      *httptest.Server
}

func newSatusehatStub(t *testing.T) *satStub {
	t.Helper()
	s := &satStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v1/accesstoken", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stub-token", "expires_in": 3600})
	})
	mux.HandleFunc("/fhir-r4/v1/", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		s.mu.Lock()
		s.requests = append(s.requests, satRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/fhir-r4/v1/"), "/")
		resourceType := parts[0]
		id := ""
		if len(parts) > 1 {
			id = parts[1]
		} else {
			s.seq++
			id = fmt.Sprintf("stub-%s-%d", resourceType, s.seq)
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resourceType": resourceType, "id": id})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// snapshot returns the captured requests.
func (s *satStub) snapshot() []satRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]satRequest(nil), s.requests...)
}

// satNewEnv is qeNewEnv with a router whose IntegrationCredentialKey is set.
func satNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.IntegrationCredentialKey = "test-key"
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.ConnectBatchSize = 10
	router := server.NewRouter(pool, redisClient, cfg)
	phone := fmt.Sprintf("0812398%05d", qePhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "sat."+label, phone)
	return &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}
}

func satConfigPath(merchantID string) string {
	return "/api/v1/merchants/" + merchantID + "/integration/satusehat"
}

// satPutConfig PUTs the satusehat config with the given base_url.
func satPutConfig(t *testing.T, env *qeEnv, baseURL string) (int, map[string]any) {
	t.Helper()
	return qeRequest(t, env, http.MethodPut, satConfigPath(env.merchantID), map[string]any{
		"base_url": baseURL, "client_id": "cid-1", "client_secret": "cs-1",
	})
}

// satSeedLocations inserts room R1 (no coordinates) and site S1 (coordinates
// required by chk_location_site_position, migrations/000055:30) and returns
// their ids.
func satSeedLocations(t *testing.T, ctx context.Context, env *qeEnv) (roomID, siteID string) {
	t.Helper()
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'room', 'R1', 'Ruang 1') RETURNING id::text",
		env.companyID, env.merchantID).Scan(&roomID); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name, latitude, longitude) VALUES ($1, $2, 'site', 'S1', 'Site 1', 55.123456, 110.654321) RETURNING id::text",
		env.companyID, env.merchantID).Scan(&siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return roomID, siteID
}

// satMapState reads sync_status + fhir_resource_id for one map row.
func satMapState(t *testing.T, ctx context.Context, env *qeEnv, localTable, localID, resourceType string) (string, string) {
	t.Helper()
	var status string
	var fhirID *string
	if err := env.pool.QueryRow(ctx,
		"SELECT sync_status, fhir_resource_id FROM connect.satusehat_resource_map WHERE local_table = $1 AND local_id = $2 AND fhir_resource_type = $3",
		localTable, localID, resourceType).Scan(&status, &fhirID); err != nil {
		t.Fatalf("read map state %s/%s: %v", localTable, resourceType, err)
	}
	if fhirID == nil {
		return status, ""
	}
	return status, *fhirID
}

// TestSatusehatConfigPutGet — PUT stores pgp_sym_encrypt-ed credentials and
// the GET/read shapes never leak client_id/client_secret.
func TestSatusehatConfigPutGet(t *testing.T) {
	ctx := context.Background()
	env := satNewEnv(t, "cfg")

	status, out := satPutConfig(t, env, "https://satu.test")
	if status != http.StatusOK {
		t.Fatalf("PUT config = %d %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["configured"] != true || data["credential_set"] != true || data["is_active"] != true {
		t.Fatalf("PUT response = %v", out)
	}
	if data["base_url"] != "https://satu.test" {
		t.Fatalf("base_url = %v", data["base_url"])
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "cs-1") || strings.Contains(string(raw), "client_id") || strings.Contains(string(raw), "client_secret") {
		t.Fatalf("credentials leaked in response: %s", raw)
	}

	// GET shape matches the PUT response.
	status, out = qeRequest(t, env, http.MethodGet, satConfigPath(env.merchantID), nil)
	data, _ = out["data"].(map[string]any)
	if status != http.StatusOK || data["credential_set"] != true || data["configured"] != true {
		t.Fatalf("GET config = %d %v", status, out)
	}

	// Via tx: the stored credential decrypts to the original JSON.
	tx := runtimeTenantTx(t, ctx, env.pool, env.companyID, env.merchantID)
	var decrypted string
	if err := tx.QueryRow(ctx,
		"SELECT pgp_sym_decrypt(credential_encrypted, 'test-key') FROM connect.integration_config WHERE merchant_id = $1",
		env.merchantID).Scan(&decrypted); err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	want := `{"client_id":"cid-1","client_secret":"cs-1"}`
	if decrypted != want {
		t.Fatalf("decrypted credential = %s, want %s", decrypted, want)
	}
}

// TestSatusehatConfigRequiresKey — without an integration credential key the
// PUT answers 503 with the literal message.
func TestSatusehatConfigRequiresKey(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "sat.nokey", "081234571301")
	env := &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}

	status, out := satPutConfig(t, env, "https://satu.test")
	if status != http.StatusServiceUnavailable || out["error"] != "integration credential key not configured" {
		t.Fatalf("PUT without key = %d %v", status, out)
	}
}

// TestSatusehatSyncOrgLocation — sync enqueues 1 Organization + N Locations,
// RunOnce pushes them through the stub adapter (create), locations get their
// satusehat_location_id written back, the status list carries all rows, a
// failed row is re-enqueued pending by an explicit sync, and a second run
// updates (PUT) with the same FHIR ids.
func TestSatusehatSyncOrgLocation(t *testing.T) {
	ctx := context.Background()
	env := satNewEnv(t, "sync")
	stub := newSatusehatStub(t)
	roomID, siteID := satSeedLocations(t, ctx, env)

	status, out := satPutConfig(t, env, stub.srv.URL)
	if status != http.StatusOK {
		t.Fatalf("PUT config = %d %v", status, out)
	}

	status, out = qeRequest(t, env, http.MethodPost, satConfigPath(env.merchantID)+"/sync", nil)
	data, _ := out["data"].(map[string]any)
	if status != http.StatusOK || data["enqueued"] != float64(3) || data["organization"] != float64(1) || data["locations"] != float64(2) {
		t.Fatalf("POST sync = %d %v", status, out)
	}

	wCfg := config.Config{ConnectBatchSize: 10, IntegrationCredentialKey: "test-key"}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, connect.NewSatusehatAdapter())
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// All maps synced.
	for _, tc := range []struct{ table, id, rtype string }{
		{"core.merchant", env.merchantID, "Organization"},
		{"core.location", roomID, "Location"},
		{"core.location", siteID, "Location"},
	} {
		if st, _ := satMapState(t, ctx, env, tc.table, tc.id, tc.rtype); st != "synced" {
			t.Fatalf("map %s/%s status = %s, want synced", tc.table, tc.rtype, st)
		}
	}

	// Stub received 1 POST Organization + 2 POST Location; S1 carries position.
	reqs := stub.snapshot()
	var orgPosts, locPosts int
	var siteBody string
	for _, r := range reqs {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s %s in first run", r.Method, r.Path)
		}
		switch {
		case r.Path == "/fhir-r4/v1/Organization":
			orgPosts++
		case r.Path == "/fhir-r4/v1/Location":
			locPosts++
			if strings.Contains(r.Body, `"position"`) {
				siteBody = r.Body
			}
		default:
			t.Fatalf("unexpected stub path %s", r.Path)
		}
	}
	if orgPosts != 1 || locPosts != 2 {
		t.Fatalf("stub posts = org %d loc %d, want 1/2", orgPosts, locPosts)
	}
	if !strings.Contains(siteBody, `"position"`) {
		t.Fatalf("no Location payload carries position: %v", reqs)
	}

	// Writeback: both locations have satusehat_location_id.
	for _, id := range []string{roomID, siteID} {
		var ssID *string
		if err := env.pool.QueryRow(ctx,
			"SELECT satusehat_location_id FROM core.location WHERE id = $1", id).Scan(&ssID); err != nil || ssID == nil || *ssID == "" {
			t.Fatalf("location %s satusehat_location_id = %v (err %v)", id, ssID, err)
		}
	}

	// Status endpoint lists 3 resources.
	status, out = qeRequest(t, env, http.MethodGet, satConfigPath(env.merchantID)+"/status", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d %v", status, out)
	}
	data, _ = out["data"].(map[string]any)
	resources, _ := data["resources"].([]any)
	if len(resources) != 3 {
		t.Fatalf("status resources = %d, want 3: %v", len(resources), out)
	}

	// (a) Re-enqueue resets a failed Location row to pending with no error.
	if _, err := env.pool.Exec(ctx,
		"UPDATE connect.satusehat_resource_map SET sync_status = 'failed', last_error = 'boom' WHERE local_table = 'core.location' AND local_id = $1 AND fhir_resource_type = 'Location'",
		siteID); err != nil {
		t.Fatalf("fail map row: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, satConfigPath(env.merchantID)+"/sync", nil)
	data, _ = out["data"].(map[string]any)
	if status != http.StatusOK || data["enqueued"] != float64(3) {
		t.Fatalf("POST sync (a) = %d %v", status, out)
	}
	st, _ := satMapState(t, ctx, env, "core.location", siteID, "Location")
	if st != "pending" {
		t.Fatalf("(a) site map status = %s, want pending", st)
	}
	var lastErr *string
	if err := env.pool.QueryRow(ctx,
		"SELECT last_error FROM connect.satusehat_resource_map WHERE local_table = 'core.location' AND local_id = $1 AND fhir_resource_type = 'Location'",
		siteID).Scan(&lastErr); err != nil || lastErr != nil {
		t.Fatalf("(a) last_error = %v (err %v)", lastErr, err)
	}

	// (b) Second sync + run → PUT updates with the SAME FHIR ids.
	status, out = qeRequest(t, env, http.MethodPost, satConfigPath(env.merchantID)+"/sync", nil)
	if status != http.StatusOK {
		t.Fatalf("POST sync (b) = %d %v", status, out)
	}
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("RunOnce (b): %v", err)
	}
	// (b) Second sync + run → PUT updates with the SAME FHIR ids (as recorded
	// by the first create run).
	_, orgID := satMapState(t, ctx, env, "core.merchant", env.merchantID, "Organization")
	_, roomFHIR := satMapState(t, ctx, env, "core.location", roomID, "Location")
	_, siteFHIR := satMapState(t, ctx, env, "core.location", siteID, "Location")
	if orgID == "" || roomFHIR == "" || siteFHIR == "" {
		t.Fatalf("(b) missing FHIR ids after first run: org=%q room=%q site=%q", orgID, roomFHIR, siteFHIR)
	}
	newReqs := stub.snapshot()[len(reqs):]
	wantPUT := map[string]bool{
		"/fhir-r4/v1/Organization/" + orgID: false,
		"/fhir-r4/v1/Location/" + roomFHIR:  false,
		"/fhir-r4/v1/Location/" + siteFHIR:  false,
	}
	for _, r := range newReqs {
		if r.Method != http.MethodPut {
			t.Fatalf("(b) expected PUT, got %s %s", r.Method, r.Path)
		}
		seen, ok := wantPUT[r.Path]
		if !ok || seen {
			t.Fatalf("(b) unexpected PUT path %s", r.Path)
		}
		wantPUT[r.Path] = true
		wantID := strings.TrimPrefix(r.Path, "/fhir-r4/v1/Organization/")
		wantID = strings.TrimPrefix(wantID, "/fhir-r4/v1/Location/")
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(r.Body), &payload); err != nil || payload.ID != wantID {
			t.Fatalf("(b) PUT body id = %q (err %v), want %s: %s", payload.ID, err, wantID, r.Body)
		}
	}
	for path, seen := range wantPUT {
		if !seen {
			t.Fatalf("(b) missing PUT %s", path)
		}
	}
}

// TestSatusehatRequiresPermission — a user without the integration
// permissions gets 403 (pattern TestPayer_RequiresPermission).
func TestSatusehatRequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.IntegrationCredentialKey = "test-key"
	router := server.NewRouter(pool, redisClient, cfg)
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "sat.perm", "081234571401")

	restrictedUserID := "7d7d7d7d-7d7d-7d7d-7d7d-7d7d7d7d7d7d"
	roleID := "7c7c7c7c-7c7c-7c7c-7c7c-7c7c7c7c7c7c"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Integration')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'sat.noperm', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(cfg.JWTSecret, restrictedUserID, companyID, merchantID, "", "sat.noperm", "sat-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	env := &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: restrictedToken}

	status, out := satPutConfig(t, env, "https://satu.test")
	if status != http.StatusForbidden {
		t.Fatalf("PUT config without permission = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, satConfigPath(env.merchantID)+"/sync", nil)
	if status != http.StatusForbidden {
		t.Fatalf("POST sync without permission = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodGet, satConfigPath(env.merchantID)+"/status", nil)
	if status != http.StatusForbidden {
		t.Fatalf("GET status without permission = %d %v", status, out)
	}
}

// satSeedPerson inserts a person for the Patient sync tests; nikPlain ""
// seeds a NIK-less person (NULL nik). The NIK is stored ENCRYPTED following
// seed/001_core_seed.sql:70 — the handler must decrypt, never pass the
// ciphertext through.
func satSeedPerson(t *testing.T, ctx context.Context, env *qeEnv, nikPlain string) string {
	t.Helper()
	var id string
	var err error
	if nikPlain == "" {
		err = env.pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, 'male', '1990-01-01') RETURNING id::text",
			env.companyID, "Pasien Satusehat").Scan(&id)
	} else {
		err = env.pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, nik, full_name, gender, birth_date) VALUES ($1, encode(pgp_sym_encrypt($2, 'test-nik-key'), 'base64'), $3, 'male', '1990-01-01') RETURNING id::text",
			env.companyID, nikPlain, "Pasien Satusehat").Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed person: %v", err)
	}
	return id
}

// satPersonSyncPath is the patient sync endpoint path for one person.
func satPersonSyncPath(personID string) string {
	return "/api/v1/persons/" + personID + "/satusehat/sync"
}

// satRetryPath is the resource-map retry endpoint path.
func satRetryPath(mapID string) string {
	return "/api/v1/integration/satusehat/resources/" + mapID + "/retry"
}

// satMerchantMapID returns the id of the merchant Organization map row.
func satMerchantMapID(t *testing.T, ctx context.Context, env *qeEnv) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(ctx,
		"SELECT id::text FROM connect.satusehat_resource_map WHERE merchant_id = $1 AND local_table = 'core.merchant' AND fhir_resource_type = 'Organization'",
		env.merchantID).Scan(&id); err != nil {
		t.Fatalf("read merchant map id: %v", err)
	}
	return id
}

// TestSatusehatPatientSync — patient sync enqueues a pending Patient map with
// a payload carrying the DECRYPTED NIK; the worker finds the IHS via search
// (1 GET, 0 POST) when the Bundle has an entry (a), a NIK-less person answers
// 400 (b), and an empty Bundle falls through to POST create (c).
func TestSatusehatPatientSync(t *testing.T) {
	ctx := context.Background()
	env := satNewEnv(t, "patient")
	nikPlain := "3174020190880001"
	personID := satSeedPerson(t, ctx, env, nikPlain)

	// (a) Search stub: GET Patient → Bundle with one entry (ihs-123); any POST
	// is recorded so the test can assert it never happens.
	var getSearches int64
	var posts int64
	var gotQuery string
	searchMux := http.NewServeMux()
	searchMux.HandleFunc("/oauth2/v1/accesstoken", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-p", "expires_in": 3600})
	})
	searchMux.HandleFunc("/fhir-r4/v1/Patient", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt64(&getSearches, 1)
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"ihs-123"}}]}`))
			return
		}
		atomic.AddInt64(&posts, 1)
		_, _ = w.Write([]byte(`{"id":"should-not-post"}`))
	})
	searchSrv := httptest.NewServer(searchMux)
	defer searchSrv.Close()

	if status, out := satPutConfig(t, env, searchSrv.URL); status != http.StatusOK {
		t.Fatalf("PUT config = %d %v", status, out)
	}
	status, out := qeRequest(t, env, http.MethodPost, satPersonSyncPath(personID), nil)
	data, _ := out["data"].(map[string]any)
	if status != http.StatusOK || data["sync_status"] != "pending" || data["map_id"] == "" {
		t.Fatalf("(a) POST sync = %d %v", status, out)
	}
	if st, _ := satMapState(t, ctx, env, "core.person", personID, "Patient"); st != "pending" {
		t.Fatalf("(a) map status after sync = %s, want pending", st)
	}

	wCfg := config.Config{ConnectBatchSize: 10, IntegrationCredentialKey: "test-key"}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, connect.NewSatusehatAdapter())
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("(a) RunOnce: %v", err)
	}
	st, fhirID := satMapState(t, ctx, env, "core.person", personID, "Patient")
	if st != "synced" || fhirID != "ihs-123" {
		t.Fatalf("(a) map after run = %s/%s, want synced/ihs-123", st, fhirID)
	}
	if n := atomic.LoadInt64(&getSearches); n != 1 {
		t.Fatalf("(a) search GETs = %d, want 1", n)
	}
	if n := atomic.LoadInt64(&posts); n != 0 {
		t.Fatalf("(a) POSTs = %d, want 0 (IHS found via search)", n)
	}
	// The search URL must carry the PLAINTEXT NIK after the system|value
	// separator (%7C), never the ciphertext/base64.
	if !strings.Contains(gotQuery, url.QueryEscape("https://fhir.kemkes.go.id/id/nik|"+nikPlain)) {
		t.Fatalf("(a) search query = %s, want it to contain %q", gotQuery, "|"+nikPlain)
	}
	var payloadNIK string
	if err := env.pool.QueryRow(ctx,
		"SELECT last_payload->'identifier'->0->>'value' FROM connect.satusehat_resource_map WHERE local_table = 'core.person' AND local_id = $1",
		personID).Scan(&payloadNIK); err != nil || payloadNIK != nikPlain {
		t.Fatalf("(a) last_payload NIK = %q (err %v), want plaintext %s", payloadNIK, err, nikPlain)
	}

	// (b) Person WITHOUT a NIK answers 400 with the literal message.
	noNikID := satSeedPerson(t, ctx, env, "")
	status, out = qeRequest(t, env, http.MethodPost, satPersonSyncPath(noNikID), nil)
	if status != http.StatusBadRequest || out["error"] != "person has no NIK" {
		t.Fatalf("(b) POST sync (no NIK) = %d %v", status, out)
	}

	// (c) Empty Bundle search → fall through to POST create (ihs-new).
	createMux := http.NewServeMux()
	createMux.HandleFunc("/oauth2/v1/accesstoken", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-c", "expires_in": 3600})
	})
	createMux.HandleFunc("/fhir-r4/v1/Patient", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ihs-new"}`))
	})
	createSrv := httptest.NewServer(createMux)
	defer createSrv.Close()
	if status, out := satPutConfig(t, env, createSrv.URL); status != http.StatusOK {
		t.Fatalf("(c) PUT config = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, satPersonSyncPath(personID), nil)
	if status != http.StatusOK {
		t.Fatalf("(c) POST sync = %d %v", status, out)
	}
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("(c) RunOnce: %v", err)
	}
	st, fhirID = satMapState(t, ctx, env, "core.person", personID, "Patient")
	if st != "synced" || fhirID != "ihs-new" {
		t.Fatalf("(c) map after run = %s/%s, want synced/ihs-new", st, fhirID)
	}
}

// TestSatusehatRetry — a failed map row (worker + stub error) resets to
// pending with last_error NULL via the retry endpoint; a synced row answers
// 409 and another merchant's row answers 404.
func TestSatusehatRetry(t *testing.T) {
	ctx := context.Background()
	env := satNewEnv(t, "retry")

	// Erroring stub: every FHIR call answers 500 so the worker fails the row.
	errMux := http.NewServeMux()
	errMux.HandleFunc("/oauth2/v1/accesstoken", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-e", "expires_in": 3600})
	})
	errMux.HandleFunc("/fhir-r4/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	errSrv := httptest.NewServer(errMux)
	defer errSrv.Close()

	if status, out := satPutConfig(t, env, errSrv.URL); status != http.StatusOK {
		t.Fatalf("PUT config = %d %v", status, out)
	}
	if status, out := qeRequest(t, env, http.MethodPost, satConfigPath(env.merchantID)+"/sync", nil); status != http.StatusOK {
		t.Fatalf("POST sync = %d %v", status, out)
	}
	wCfg := config.Config{ConnectBatchSize: 10, IntegrationCredentialKey: "test-key"}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, connect.NewSatusehatAdapter())
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	mapID := satMerchantMapID(t, ctx, env)
	if st, _ := satMapState(t, ctx, env, "core.merchant", env.merchantID, "Organization"); st != "failed" {
		t.Fatalf("map status after failing run = %s, want failed", st)
	}

	// Retry: failed → pending, last_error cleared.
	status, out := qeRequest(t, env, http.MethodPost, satRetryPath(mapID), nil)
	data, _ := out["data"].(map[string]any)
	if status != http.StatusOK || data["sync_status"] != "pending" {
		t.Fatalf("POST retry = %d %v", status, out)
	}
	var lastErr *string
	var st string
	if err := env.pool.QueryRow(ctx,
		"SELECT sync_status, last_error FROM connect.satusehat_resource_map WHERE id = $1", mapID).Scan(&st, &lastErr); err != nil {
		t.Fatalf("read map after retry: %v", err)
	}
	if st != "pending" || lastErr != nil {
		t.Fatalf("map after retry = %s/%v, want pending/<nil>", st, lastErr)
	}

	// Synced row: retry answers 409 with the literal message.
	if _, err := env.pool.Exec(ctx,
		"UPDATE connect.satusehat_resource_map SET sync_status = 'synced', fhir_resource_id = 'ihs-x' WHERE id = $1", mapID); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, satRetryPath(mapID), nil)
	if status != http.StatusConflict || out["error"] != "only failed resources can be retried" {
		t.Fatalf("POST retry (synced) = %d %v", status, out)
	}

	// Another merchant's map: retry answers 404.
	env2 := satNewEnv(t, "retry2")
	if status, out := satPutConfig(t, env2, errSrv.URL); status != http.StatusOK {
		t.Fatalf("env2 PUT config = %d %v", status, out)
	}
	if status, out := qeRequest(t, env2, http.MethodPost, satConfigPath(env2.merchantID)+"/sync", nil); status != http.StatusOK {
		t.Fatalf("env2 POST sync = %d %v", status, out)
	}
	otherMapID := satMerchantMapID(t, ctx, env2)
	status, out = qeRequest(t, env, http.MethodPost, satRetryPath(otherMapID), nil)
	if status != http.StatusNotFound || out["error"] != "resource map not found" {
		t.Fatalf("POST retry (other merchant) = %d %v", status, out)
	}
}

// satEncounterSyncPath is the admission encounter sync endpoint path.
func satEncounterSyncPath(admissionID string) string {
	return "/api/v1/admissions/" + admissionID + "/satusehat/sync"
}

// satSeedAdmission inserts one operations.admission row (pola kolom
// migrations/000020:8-23) and returns its id.
func satSeedAdmission(t *testing.T, ctx context.Context, env *qeEnv, personID, admissionType, status, visitNo, departmentID string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, status) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text",
		env.companyID, env.merchantID, visitNo, personID, admissionType, departmentID, status).Scan(&id); err != nil {
		t.Fatalf("seed admission %s: %v", visitNo, err)
	}
	return id
}

// TestSatusehatEncounterSync — encounter sync answers 409 while the Patient
// is unsynced (a); once the Patient carries an IHS the encounter map enqueues
// pending with an in-progress payload whose subject is the IHS and the worker
// pushes it to synced (b); inpatient (c) and cancelled (d) admissions answer
// 400; a discharged admission maps to finished with a filled period.end (e).
func TestSatusehatEncounterSync(t *testing.T) {
	ctx := context.Background()
	env := satNewEnv(t, "enc")
	stub := newSatusehatStub(t)

	var departmentID string
	if err := env.pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, 'ENC01', 'Poli Encounter') RETURNING id::text",
		env.companyID, env.merchantID).Scan(&departmentID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	personID := satSeedPerson(t, ctx, env, "3174020190880002")
	admissionID := satSeedAdmission(t, ctx, env, personID, "outpatient", "registered", "RJ-ENC-0001", departmentID)

	if status, out := satPutConfig(t, env, stub.srv.URL); status != http.StatusOK {
		t.Fatalf("PUT config = %d %v", status, out)
	}

	// (a) Patient belum synced ⇒ 409 literal.
	status, out := qeRequest(t, env, http.MethodPost, satEncounterSyncPath(admissionID), nil)
	if status != http.StatusConflict || out["error"] != "patient not synced to satusehat" {
		t.Fatalf("(a) POST encounter sync = %d %v", status, out)
	}

	// Patient synced: jalankan person sync + worker terhadap stub.
	status, out = qeRequest(t, env, http.MethodPost, satPersonSyncPath(personID), nil)
	if status != http.StatusOK {
		t.Fatalf("POST person sync = %d %v", status, out)
	}
	wCfg := config.Config{ConnectBatchSize: 10, IntegrationCredentialKey: "test-key"}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, connect.NewSatusehatAdapter())
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("(patient) RunOnce: %v", err)
	}
	patientStatus, patientIHS := satMapState(t, ctx, env, "core.person", personID, "Patient")
	if patientStatus != "synced" || patientIHS == "" {
		t.Fatalf("(patient) map = %s/%q, want synced/<ihs>", patientStatus, patientIHS)
	}

	// (b) Encounter sync ⇒ map pending, payload in-progress + subject IHS.
	status, out = qeRequest(t, env, http.MethodPost, satEncounterSyncPath(admissionID), nil)
	data, _ := out["data"].(map[string]any)
	if status != http.StatusOK || data["sync_status"] != "pending" || data["map_id"] == "" {
		t.Fatalf("(b) POST encounter sync = %d %v", status, out)
	}
	if st, _ := satMapState(t, ctx, env, "operations.admission", admissionID, "Encounter"); st != "pending" {
		t.Fatalf("(b) encounter map status = %s, want pending", st)
	}
	var encounterPayload string
	if err := env.pool.QueryRow(ctx,
		"SELECT last_payload::text FROM connect.satusehat_resource_map WHERE local_table = 'operations.admission' AND local_id = $1 AND fhir_resource_type = 'Encounter'",
		admissionID).Scan(&encounterPayload); err != nil {
		t.Fatalf("(b) read encounter payload: %v", err)
	}
	var payloadMap map[string]any
	if err := json.Unmarshal([]byte(encounterPayload), &payloadMap); err != nil {
		t.Fatalf("(b) unmarshal encounter payload: %v", err)
	}
	if payloadMap["status"] != "in-progress" {
		t.Fatalf("(b) payload status = %v, want in-progress", payloadMap["status"])
	}
	subject, _ := payloadMap["subject"].(map[string]any)
	if subject["reference"] != "Patient/"+patientIHS {
		t.Fatalf("(b) payload subject = %v, want Patient/%s", subject, patientIHS)
	}
	if _, err := connect.RunOnce(ctx, env.pool, registry, wCfg); err != nil {
		t.Fatalf("(b) RunOnce: %v", err)
	}
	if st, fhirID := satMapState(t, ctx, env, "operations.admission", admissionID, "Encounter"); st != "synced" || fhirID == "" {
		t.Fatalf("(b) encounter map after run = %s/%q, want synced/<id>", st, fhirID)
	}
	var encounterPosted bool
	for _, r := range stub.snapshot() {
		var body map[string]any
		if r.Method != http.MethodPost || r.Path != "/fhir-r4/v1/Encounter" || json.Unmarshal([]byte(r.Body), &body) != nil {
			continue
		}
		if body["status"] == "in-progress" {
			encounterPosted = true
		}
	}
	if !encounterPosted {
		t.Fatalf("(b) stub never received the Encounter POST: %v", stub.snapshot())
	}

	// (c) Inpatient ⇒ 400 literal.
	inpatientID := satSeedAdmission(t, ctx, env, personID, "inpatient", "registered", "RI-ENC-0001", departmentID)
	status, out = qeRequest(t, env, http.MethodPost, satEncounterSyncPath(inpatientID), nil)
	if status != http.StatusBadRequest || out["error"] != "only outpatient admissions are supported" {
		t.Fatalf("(c) POST encounter sync (inpatient) = %d %v", status, out)
	}

	// (d) Cancelled ⇒ 400 literal.
	cancelledID := satSeedAdmission(t, ctx, env, personID, "outpatient", "cancelled", "RJ-ENC-0002", departmentID)
	status, out = qeRequest(t, env, http.MethodPost, satEncounterSyncPath(cancelledID), nil)
	if status != http.StatusBadRequest || out["error"] != "admission is cancelled" {
		t.Fatalf("(d) POST encounter sync (cancelled) = %d %v", status, out)
	}

	// (e) Discharged ⇒ payload finished + period.end terisi.
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.admission SET status = 'discharged', discharge_at = now() WHERE id = $1", admissionID); err != nil {
		t.Fatalf("(e) discharge admission: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, satEncounterSyncPath(admissionID), nil)
	if status != http.StatusOK {
		t.Fatalf("(e) POST encounter sync = %d %v", status, out)
	}
	if err := env.pool.QueryRow(ctx,
		"SELECT last_payload::text FROM connect.satusehat_resource_map WHERE local_table = 'operations.admission' AND local_id = $1 AND fhir_resource_type = 'Encounter'",
		admissionID).Scan(&encounterPayload); err != nil {
		t.Fatalf("(e) read encounter payload: %v", err)
	}
	payloadMap = nil
	if err := json.Unmarshal([]byte(encounterPayload), &payloadMap); err != nil {
		t.Fatalf("(e) unmarshal encounter payload: %v", err)
	}
	if payloadMap["status"] != "finished" {
		t.Fatalf("(e) payload status = %v, want finished", payloadMap["status"])
	}
	period, _ := payloadMap["period"].(map[string]any)
	if period["end"] == nil || period["end"] == "" {
		t.Fatalf("(e) payload period.end empty: %s", encounterPayload)
	}
}
