//go:build integration

package server_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/connect"
)

// stubAdapter is a fake IntegrationAdapter counting its calls; the sendFn hook
// lets each test script the outcome.
type stubAdapter struct {
	mu     sync.Mutex
	calls  int
	sendFn func(req connect.SendRequest) (connect.SendResponse, error)
}

func (s *stubAdapter) Send(_ context.Context, req connect.SendRequest) (connect.SendResponse, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.sendFn(req)
}

func (s *stubAdapter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// seedConnectTenant seeds a company + merchant + active satusehat
// integration_config (fresh DB container per test, so fixed UUIDs are safe)
// and returns the ids.
func seedConnectTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (companyID, merchantID string) {
	t.Helper()
	companyID = "aaaaaab1-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	merchantID = "bbbbbbb1-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, max_concurrent_sessions) VALUES ($1, 'connect-co', 'Connect Co', 10)", companyID); err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'connect-merchant', 'Connect Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}
	_, err := pool.Exec(ctx,
		"INSERT INTO connect.integration_config (company_id, merchant_id, provider_id, base_url) SELECT $1, $2, id, 'http://stub.test' FROM connect.integration_provider WHERE code = 'satusehat'",
		companyID, merchantID,
	)
	if err != nil {
		t.Fatalf("failed to seed integration_config: %v", err)
	}
	return companyID, merchantID
}

// seedConnectMap inserts one pending satusehat_resource_map row and returns its id.
func seedConnectMap(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, localTable, localID, resourceType string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"resourceType": %q}`, resourceType)
	var id string
	err := pool.QueryRow(ctx,
		"INSERT INTO connect.satusehat_resource_map (company_id, merchant_id, local_table, local_id, fhir_resource_type, last_payload) VALUES ($1, $2, $3, $4, $5, $6::jsonb) RETURNING id",
		companyID, merchantID, localTable, localID, resourceType, payload,
	).Scan(&id)
	if err != nil {
		t.Fatalf("failed to seed satusehat_resource_map: %v", err)
	}
	return id
}

// seedConnectLocation inserts one core.location row (kind room per plan) and returns its id.
func seedConnectLocation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		"INSERT INTO core.location (company_id, merchant_id, kind, code, name) VALUES ($1, $2, 'room', 'R1', 'Ruang 1') RETURNING id",
		companyID, merchantID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("failed to seed location: %v", err)
	}
	return id
}

// mapState reads sync_status, fhir_resource_id, locked_until, last_error for one map row.
func mapState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, mapID string) (status, fhirResourceID string, lockedUntil pgtype.Timestamptz, lastError *string) {
	t.Helper()
	var fhirID *string
	var locked pgtype.Timestamptz
	var lastErr *string
	err := pool.QueryRow(ctx,
		"SELECT sync_status, fhir_resource_id, locked_until, last_error FROM connect.satusehat_resource_map WHERE id = $1", mapID,
	).Scan(&status, &fhirID, &locked, &lastErr)
	if err != nil {
		t.Fatalf("failed to read map state: %v", err)
	}
	if fhirID != nil {
		fhirResourceID = *fhirID
	}
	if lastErr != nil {
		lastError = lastErr
	}
	return status, fhirResourceID, locked, lastError
}

func workerCfg() config.Config {
	return config.Config{ConnectBatchSize: 10, IntegrationCredentialKey: ""}
}

func TestConnectWorkerSyncsPendingRow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyID, merchantID := seedConnectTenant(t, ctx, pool)
	locationID := seedConnectLocation(t, ctx, pool, companyID, merchantID)
	orgMapID := seedConnectMap(t, ctx, pool, companyID, merchantID, "core.merchant", merchantID, "Organization")
	locMapID := seedConnectMap(t, ctx, pool, companyID, merchantID, "core.location", locationID, "Location")

	stub := &stubAdapter{sendFn: func(req connect.SendRequest) (connect.SendResponse, error) {
		ids := map[string]string{"Organization": "org-123", "Location": "loc-456"}
		return connect.SendResponse{ResourceID: ids[req.ResourceType], HTTPStatus: 200, Body: []byte(`{"ok":true}`)}, nil
	}}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, stub)

	n, err := connect.RunOnce(ctx, pool, registry, workerCfg())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("RunOnce processed %d rows, want 2", n)
	}

	for _, tc := range []struct{ mapID, wantID string }{{orgMapID, "org-123"}, {locMapID, "loc-456"}} {
		status, fhirID, locked, lastErr := mapState(t, ctx, pool, tc.mapID)
		if status != "synced" {
			t.Errorf("map %s status = %q, want synced", tc.mapID, status)
		}
		if fhirID != tc.wantID {
			t.Errorf("map %s fhir_resource_id = %q, want %q", tc.mapID, fhirID, tc.wantID)
		}
		if locked.Valid {
			t.Errorf("map %s locked_until = %v, want NULL", tc.mapID, locked)
		}
		if lastErr != nil {
			t.Errorf("map %s last_error = %v, want NULL", tc.mapID, lastErr)
		}
	}

	var satusehatLocID *string
	if err := pool.QueryRow(ctx, "SELECT satusehat_location_id FROM core.location WHERE id = $1", locationID).Scan(&satusehatLocID); err != nil {
		t.Fatalf("failed to read location: %v", err)
	}
	if satusehatLocID == nil || *satusehatLocID != "loc-456" {
		t.Errorf("satusehat_location_id = %v, want loc-456", satusehatLocID)
	}

	var logCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM connect.integration_request_log WHERE status = 'success' AND endpoint IN ('satusehat/Organization', 'satusehat/Location')",
	).Scan(&logCount); err != nil {
		t.Fatalf("failed to read request log: %v", err)
	}
	if logCount != 2 {
		t.Errorf("request log success rows = %d, want 2", logCount)
	}
}

func TestConnectWorkerMarksFailed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyID, merchantID := seedConnectTenant(t, ctx, pool)
	locationID := seedConnectLocation(t, ctx, pool, companyID, merchantID)
	mapID := seedConnectMap(t, ctx, pool, companyID, merchantID, "core.location", locationID, "Location")

	stub := &stubAdapter{sendFn: func(connect.SendRequest) (connect.SendResponse, error) {
		return connect.SendResponse{}, errors.New("connection refused")
	}}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, stub)

	if _, err := connect.RunOnce(ctx, pool, registry, workerCfg()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	status, _, locked, lastErr := mapState(t, ctx, pool, mapID)
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if lastErr == nil {
		t.Error("last_error is NULL, want populated")
	}
	if locked.Valid {
		t.Errorf("locked_until = %v, want NULL", locked)
	}

	var httpStatus, logCount int
	if err := pool.QueryRow(ctx,
		"SELECT http_status, count(*) OVER () FROM connect.integration_request_log WHERE status = 'failed'",
	).Scan(&httpStatus, &logCount); err != nil {
		t.Fatalf("failed to read request log: %v", err)
	}
	if httpStatus != 0 {
		t.Errorf("http_status = %d, want 0", httpStatus)
	}
	if logCount != 1 {
		t.Errorf("failed log rows = %d, want 1", logCount)
	}

	var satusehatLocID *string
	if err := pool.QueryRow(ctx, "SELECT satusehat_location_id FROM core.location WHERE id = $1", locationID).Scan(&satusehatLocID); err != nil {
		t.Fatalf("failed to read location: %v", err)
	}
	if satusehatLocID != nil {
		t.Errorf("satusehat_location_id = %q, want NULL", *satusehatLocID)
	}
}

func TestConnectWorkerReclaimsStaleSending(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyID, merchantID := seedConnectTenant(t, ctx, pool)
	mapID := seedConnectMap(t, ctx, pool, companyID, merchantID, "core.merchant", merchantID, "Organization")
	if _, err := pool.Exec(ctx,
		"UPDATE connect.satusehat_resource_map SET sync_status = 'sending', locked_until = now() - interval '1 minute' WHERE id = $1",
		mapID,
	); err != nil {
		t.Fatalf("failed to stale-lock map: %v", err)
	}

	stub := &stubAdapter{sendFn: func(connect.SendRequest) (connect.SendResponse, error) {
		return connect.SendResponse{ResourceID: "org-reclaimed", HTTPStatus: 200, Body: []byte(`{}`)}, nil
	}}
	registry := connect.NewRegistry()
	registry.Register(connect.ProviderSatusehat, stub)

	n, err := connect.RunOnce(ctx, pool, registry, workerCfg())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("RunOnce processed %d rows, want 1", n)
	}
	status, fhirID, locked, _ := mapState(t, ctx, pool, mapID)
	if status != "synced" || fhirID != "org-reclaimed" || locked.Valid {
		t.Errorf("map state after reclaim: status=%q fhir=%q locked=%v", status, fhirID, locked)
	}
	if calls := stub.callCount(); calls != 1 {
		t.Errorf("adapter called %d times, want 1", calls)
	}
}

func TestConnectWorkerSkipsWithoutAdapter(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyID, merchantID := seedConnectTenant(t, ctx, pool)
	mapID := seedConnectMap(t, ctx, pool, companyID, merchantID, "core.merchant", merchantID, "Organization")

	n, err := connect.RunOnce(ctx, pool, connect.NewRegistry(), workerCfg())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("RunOnce processed %d rows, want 0", n)
	}
	status, _, locked, _ := mapState(t, ctx, pool, mapID)
	if status != "pending" {
		t.Errorf("status = %q, want pending (untouched)", status)
	}
	if locked.Valid {
		t.Errorf("locked_until = %v, want NULL", locked)
	}
	var logCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM connect.integration_request_log").Scan(&logCount); err != nil {
		t.Fatalf("failed to count request log: %v", err)
	}
	if logCount != 0 {
		t.Errorf("request log rows = %d, want 0", logCount)
	}
}
