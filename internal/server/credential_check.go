// internal/server/credential_check.go
// Periodic STR/SIP/SIPA expiry reminder worker — spec
// 2026-10-20-staff-credential-expiry-design §4.3.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mail"
)

// RunCredentialCheckWorker scans credential expiry once per
// CredentialCheckIntervalHours until ctx is done; a failed scan is logged and
// never stops the worker (pattern mail.RunWorker).
func RunCredentialCheckWorker(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.CredentialCheckIntervalHours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := processCredentialChecks(ctx, pool, cfg); err != nil {
			slog.Warn("credential worker: check failed", "error", err)
		}
	}
}

// credentialExpiryRow is one core.scan_credential_expiry row (RETURNS TABLE,
// raw query like mail.ClaimEmails — sqlc can't type its result columns).
// user_id/user_email are nullable: a physician row joins its app_user
// laterally and may have none; staff rows always carry both.
type credentialExpiryRow struct {
	MerchantID     pgtype.UUID
	CompanyID      pgtype.UUID
	StaffKind      string
	UserID         pgtype.UUID
	UserEmail      pgtype.Text
	StaffName      string
	CredentialKind string
	LicenseNumber  string
	ValidUntil     pgtype.Date
}

// processCredentialChecks scans every credential at/below the padded deadline,
// groups the expired/expiring rows per merchant and enqueues one reminder per
// emailed license owner plus one admin digest per merchant. Writes go only
// through core.enqueue_credential_email (SECURITY DEFINER).
func processCredentialChecks(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
	// Global deadline padded +2 days so a merchant west of UTC cannot miss a
	// row that only becomes "expiring" inside its own timezone.
	deadline := time.Now().UTC().AddDate(0, 0, cfg.CredentialWarnDays+2)
	rows, err := pool.Query(ctx, "SELECT * FROM core.scan_credential_expiry($1)", pgtype.Date{Time: deadline, Valid: true})
	if err != nil {
		return err
	}
	defer rows.Close()
	byMerchant := map[pgtype.UUID][]credentialExpiryRow{}
	for rows.Next() {
		var r credentialExpiryRow
		if err := rows.Scan(&r.MerchantID, &r.CompanyID, &r.StaffKind, &r.UserID, &r.UserEmail,
			&r.StaffName, &r.CredentialKind, &r.LicenseNumber, &r.ValidUntil); err != nil {
			return err
		}
		byMerchant[r.MerchantID] = append(byMerchant[r.MerchantID], r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for merchantID, merchantRows := range byMerchant {
		if err := processCredentialMerchant(ctx, pool, cfg, merchantID, merchantRows); err != nil {
			if ctx.Err() != nil {
				return err
			}
			slog.Warn("credential worker: merchant scan failed", "merchant_id", merchantID.String(), "error", err)
		}
	}
	return nil
}

// processCredentialMerchant enqueues the per-user reminders and the admin
// digest for one merchant's expired/expiring credential rows.
func processCredentialMerchant(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, merchantID pgtype.UUID, rows []credentialExpiryRow) error {
	var tz pgtype.Text
	if err := pool.QueryRow(ctx, "SELECT core.credential_merchant_timezone($1)", merchantID).Scan(&tz); err != nil || !tz.Valid || tz.String == "" {
		tz.String = defaultMerchantTimezone
	}
	loc, err := time.LoadLocation(tz.String)
	if err != nil {
		slog.Warn("credential worker: invalid merchant timezone, skipping merchant",
			"merchant_id", merchantID.String(), "timezone", tz.String, "error", err)
		return nil
	}
	today := time.Now().In(loc)
	day := today.Format("2006-01-02")
	companyID := rows[0].CompanyID
	merchantName := merchantDisplayName(ctx, pool, companyID, merchantID)

	type userItems struct {
		email string
		items []mail.CredentialExpiryItem
	}
	perUser := map[pgtype.UUID]userItems{}
	adminItems := make([]mail.CredentialExpiryItem, 0, len(rows))
	for _, r := range rows {
		days, status := credentialStatusInfo(r.ValidUntil, today, cfg.CredentialWarnDays)
		if status != "expired" && status != "expiring" {
			continue
		}
		item := mail.CredentialExpiryItem{
			StaffName:      r.StaffName,
			CredentialKind: r.CredentialKind,
			LicenseNumber:  r.LicenseNumber,
			ValidUntil:     r.ValidUntil.Time.Format("2006-01-02"),
			DaysRemaining:  *days,
		}
		adminItems = append(adminItems, item)
		if r.UserID.Valid && r.UserEmail.Valid && r.UserEmail.String != "" {
			u := perUser[r.UserID]
			u.email, u.items = r.UserEmail.String, append(u.items, item)
			perUser[r.UserID] = u
		}
	}

	q := sqlcgen.New(pool)
	enqueue := func(to, dedup string, d mail.CredentialExpiryData) {
		msg, err := mail.RenderCredentialExpiry(d)
		if err != nil {
			slog.Warn("credential worker: render failed, skipping item", "dedup_key", dedup, "error", err)
			return
		}
		msg.To = to
		if err := q.EnqueueCredentialEmail(ctx, sqlcgen.EnqueueCredentialEmailParams{
			CompanyID: companyID, ToAddress: msg.To, Subject: msg.Subject,
			BodyText: msg.Text, BodyHtml: msg.HTML, DedupKey: dedup,
		}); err != nil {
			slog.Warn("credential worker: enqueue failed", "dedup_key", dedup, "error", err)
		}
	}
	// One summary per license owner with an email; recipients without an
	// email never join (their items still feed the admin digest).
	for userID, u := range perUser {
		dedup := fmt.Sprintf("staff_credential_expiry:%s:u:%s:%s", merchantID.String(), userID.String(), day)
		enqueue(u.email, dedup, mail.CredentialExpiryData{
			RecipientName: u.items[0].StaffName, MerchantName: merchantName, IsAdmin: false, Items: u.items,
		})
	}
	// One digest per admin (core.user.manage) carrying every item.
	for _, a := range credentialAdminEmails(ctx, pool, merchantID) {
		dedup := fmt.Sprintf("staff_credential_expiry:%s:a:%s:%s", merchantID.String(), a.id.String(), day)
		enqueue(a.email, dedup, mail.CredentialExpiryData{
			RecipientName: a.email, MerchantName: merchantName, IsAdmin: true, Items: adminItems,
		})
	}
	return nil
}

// credentialAdminEmail lists the merchant's active, emailed core.user.manage
// holders via core.credential_admin_emails.
func credentialAdminEmails(ctx context.Context, pool *pgxpool.Pool, merchantID pgtype.UUID) []struct {
	id    pgtype.UUID
	email string
} {
	rows, err := pool.Query(ctx, "SELECT * FROM core.credential_admin_emails($1)", merchantID)
	if err != nil {
		slog.Warn("credential worker: admin query failed", "merchant_id", merchantID.String(), "error", err)
		return nil
	}
	defer rows.Close()
	var out []struct {
		id    pgtype.UUID
		email string
	}
	for rows.Next() {
		var a struct {
			id    pgtype.UUID
			email string
		}
		if err := rows.Scan(&a.id, &a.email); err != nil {
			slog.Warn("credential worker: admin scan failed", "merchant_id", merchantID.String(), "error", err)
			return out
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("credential worker: admin rows failed", "merchant_id", merchantID.String(), "error", err)
	}
	return out
}

// merchantDisplayName reads core.merchant.name through a company-scoped
// transaction (RLS needs app.current_company_id); a read error falls back to
// an empty name rather than failing the whole merchant scan.
func merchantDisplayName(ctx context.Context, pool *pgxpool.Pool, companyID, merchantID pgtype.UUID) string {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ""
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID.String()); err != nil {
		return ""
	}
	m, err := sqlcgen.New(tx).GetMerchantByID(ctx, merchantID)
	if err != nil {
		return ""
	}
	if err := tx.Commit(ctx); err != nil {
		return ""
	}
	return m.Name
}
