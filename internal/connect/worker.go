package connect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// claimLockSeconds must outlive one send (httpClient timeout 30s + margin): a
// lock never expires while its row is still being sent.
const claimLockSeconds = 120

const claimSQL = "SELECT * FROM connect.claim_satusehat_maps($1, $2, $3)"

// claimedRow mirrors one row of connect.claim_satusehat_maps' RETURNS TABLE
// (migration 000077), scanned explicitly per position. The function is
// SECURITY DEFINER, so the plain pool connection (app_runtime, no tenant GUC)
// is legitimate here; nullable columns follow the DDL.
type claimedRow struct {
	ID               pgtype.UUID
	CompanyID        pgtype.UUID
	MerchantID       pgtype.UUID
	LocalTable       string
	LocalID          pgtype.UUID
	FhirResourceType pgtype.Text
	FhirResourceID   pgtype.Text
	LastPayload      []byte
	BaseURL          string
	Config           []byte
	CredentialJSON   pgtype.Text
}

// RunWorker claims due satusehat_resource_map rows every
// ConnectPollIntervalSeconds and pushes each one through its adapter, until
// ctx is done.
func RunWorker(ctx context.Context, pool *pgxpool.Pool, registry *Registry, cfg config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.ConnectPollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := RunOnce(ctx, pool, registry, cfg); err != nil && ctx.Err() == nil {
			slog.Warn("connect worker: claim failed", "error", err)
		}
	}
}

// RunOnce claims up to ConnectBatchSize rows through the SECURITY DEFINER
// claim function and processes each one; it returns how many rows were
// processed (sent and failed alike). Without a satusehat adapter nothing is
// claimed, so queued rows stay pending instead of churning locks.
func RunOnce(ctx context.Context, pool *pgxpool.Pool, registry *Registry, cfg config.Config) (int, error) {
	adapter := registry.Adapter(ProviderSatusehat)
	if adapter == nil {
		return 0, nil
	}
	rows, err := pool.Query(ctx, claimSQL, cfg.ConnectBatchSize, cfg.IntegrationCredentialKey, claimLockSeconds)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	claimed := make([]claimedRow, 0, cfg.ConnectBatchSize)
	for rows.Next() {
		var r claimedRow
		if err := rows.Scan(
			&r.ID, &r.CompanyID, &r.MerchantID, &r.LocalTable, &r.LocalID,
			&r.FhirResourceType, &r.FhirResourceID, &r.LastPayload,
			&r.BaseURL, &r.Config, &r.CredentialJSON,
		); err != nil {
			return 0, err
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range claimed {
		if err := processRow(ctx, pool, adapter, r); err != nil {
			slog.Warn("connect worker: row failed", "id", r.ID.String(), "error", truncate(err.Error(), 500))
		}
	}
	return len(claimed), nil
}

// processRow pushes one claimed row and records the outcome: the map row
// (synced/failed), the integration request log, and — for locations — the
// writeback of the satusehat location id all happen in ONE transaction.
func processRow(ctx context.Context, pool *pgxpool.Pool, adapter IntegrationAdapter, row claimedRow) error {
	start := time.Now()
	req := SendRequest{
		CompanyID:       row.CompanyID.String(),
		MerchantID:      row.MerchantID.String(),
		BaseURL:         row.BaseURL,
		Config:          row.Config,
		Credential:      row.CredentialJSON.String,
		ResourceType:    row.FhirResourceType.String,
		KnownResourceID: row.FhirResourceID.String,
		Payload:         row.LastPayload,
	}
	resp, sendErr := safeSend(ctx, adapter, req)
	duration := time.Since(start).Milliseconds()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.current_merchant_id', $1, true)", row.MerchantID.String(),
	); err != nil {
		return err
	}
	q := sqlcgen.New(tx)

	if sendErr == nil {
		resourceID := row.FhirResourceID // keep the known id when the send returned none
		if resp.ResourceID != "" {
			resourceID = pgtype.Text{String: resp.ResourceID, Valid: true}
		}
		if err := q.MarkSatusehatMapSynced(ctx, sqlcgen.MarkSatusehatMapSyncedParams{
			ID: row.ID, FhirResourceID: resourceID,
		}); err != nil {
			return err
		}
		if err := q.InsertIntegrationRequestLog(ctx, sqlcgen.InsertIntegrationRequestLogParams{
			CompanyID: row.CompanyID, MerchantID: row.MerchantID,
			Endpoint:        "satusehat/" + row.FhirResourceType.String,
			RequestPayload:  row.LastPayload,
			ResponsePayload: []byte(truncate(string(resp.Body), 32000)),
			HttpStatus:      pgtype.Int4{Int32: int32(resp.HTTPStatus), Valid: true},
			Status:          "success",
			DurationMs:      pgtype.Int4{Int32: int32(duration), Valid: true},
		}); err != nil {
			return err
		}
		if row.LocalTable == "core.location" && row.FhirResourceType.String == "Location" {
			if err := q.UpdateLocationSatusehatID(ctx, sqlcgen.UpdateLocationSatusehatIDParams{
				ID: row.LocalID, SatusehatLocationID: resourceID,
			}); err != nil {
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		slog.Info("connect worker: row synced",
			"id", row.ID.String(), "resource_type", row.FhirResourceType.String,
			"status", "success", "duration_ms", duration)
		return nil
	}

	status := "failed"
	var netErr net.Error
	if errors.Is(sendErr, context.DeadlineExceeded) || errors.As(sendErr, &netErr) && netErr.Timeout() {
		status = "timeout"
	}
	if err := q.MarkSatusehatMapFailed(ctx, sqlcgen.MarkSatusehatMapFailedParams{
		ID: row.ID, LastError: pgtype.Text{String: truncate(sendErr.Error(), 500), Valid: true},
	}); err != nil {
		return err
	}
	if err := q.InsertIntegrationRequestLog(ctx, sqlcgen.InsertIntegrationRequestLogParams{
		CompanyID: row.CompanyID, MerchantID: row.MerchantID,
		Endpoint:       "satusehat/" + row.FhirResourceType.String,
		RequestPayload: row.LastPayload,
		HttpStatus:     pgtype.Int4{Int32: int32(resp.HTTPStatus), Valid: true},
		Status:         status,
		DurationMs:     pgtype.Int4{Int32: int32(duration), Valid: true},
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	slog.Warn("connect worker: row failed",
		"id", row.ID.String(), "resource_type", row.FhirResourceType.String,
		"status", status, "duration_ms", duration,
		"error", truncate(sendErr.Error(), 500))
	return nil
}

// safeSend turns a panicking adapter into an ordinary failed attempt so one
// bad row cannot kill the worker goroutine.
func safeSend(ctx context.Context, adapter IntegrationAdapter, req SendRequest) (resp SendResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp = SendResponse{}
			err = fmt.Errorf("send panicked: %v", r)
		}
	}()
	return adapter.Send(ctx, req)
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
