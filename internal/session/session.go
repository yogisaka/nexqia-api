// internal/session/session.go
// Session/refresh-token model — see 2026-09-15-session-refresh-token-design.md.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Config carries the tunables from config.Config needed to issue/validate sessions.
type Config struct {
	JWTSecret      string
	AccessTokenTTL time.Duration
	IdleTimeout    time.Duration
	AbsoluteTTL    time.Duration
}

var (
	// ErrNoSession means the presented refresh token doesn't match any known row
	// (missing cookie, garbage value, or never issued).
	ErrNoSession = errors.New("no session")
	// ErrSessionRevoked means the refresh token was already rotated/revoked —
	// reusing it indicates possible theft (see spec §7-§8). The whole device's
	// session chain is revoked as a side effect of detecting this.
	ErrSessionRevoked = errors.New("session revoked")
	// ErrSessionExpired means idle-timeout or the absolute TTL was exceeded.
	ErrSessionExpired = errors.New("session expired")
	// ErrSessionMismatch means the presented refresh token belongs to a different user.
	ErrSessionMismatch = errors.New("session does not belong to caller")
)

// Issued is what a caller (login/refresh/select-merchant handlers) hands back to
// the client: the access token for the response body and the raw refresh token
// for the Set-Cookie header. RefreshExpiresAt sets the cookie's Max-Age.
type Issued struct {
	AccessToken      string
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func generateRawToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IssueParams identifies the principal and device a new session is issued for.
type IssueParams struct {
	UserID      pgtype.UUID
	CompanyID   pgtype.UUID
	MerchantID  pgtype.UUID
	Username    string
	DeviceID    string
	DeviceLabel string
	IP          netip.Addr
}

// Issue creates a new refresh-token row and access token — called by LoginHandler
// (and /auth/select-merchant) once the active merchant is known (spec §3a).
// Caller is responsible for the device-limit check (spec §6) before calling this.
func Issue(ctx context.Context, q *sqlcgen.Queries, cfg Config, p IssueParams) (Issued, error) {
	raw, err := generateRawToken()
	if err != nil {
		return Issued{}, err
	}
	expiresAt := time.Now().Add(cfg.AbsoluteTTL)
	row, err := q.CreateRefreshToken(ctx, sqlcgen.CreateRefreshTokenParams{
		UserID:      p.UserID,
		CompanyID:   p.CompanyID,
		MerchantID:  p.MerchantID,
		DeviceID:    p.DeviceID,
		DeviceLabel: p.DeviceLabel,
		TokenHash:   hashToken(raw),
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
		CreatedIp:   p.IP,
	})
	if err != nil {
		return Issued{}, err
	}
	accessToken, err := auth.GenerateToken(cfg.JWTSecret, p.UserID.String(), p.CompanyID.String(), p.MerchantID.String(), p.Username, p.DeviceID, cfg.AccessTokenTTL)
	if err != nil {
		return Issued{}, err
	}
	return Issued{AccessToken: accessToken, RefreshToken: raw, RefreshExpiresAt: row.ExpiresAt.Time}, nil
}

// Refresh validates the presented raw refresh token and, if valid, rotates it:
// the old row is revoked and a new one (same device_id/merchant_id) replaces it.
// See spec §7 for the full decision table this function implements. Looks up the
// principal's username itself (from the row's user_id) — the caller only has an
// opaque token at this point, not an already-known identity.
func Refresh(ctx context.Context, q *sqlcgen.Queries, cfg Config, rawToken string, ip netip.Addr) (Issued, error) {
	row, err := q.GetRefreshTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrNoSession
	}
	if err != nil {
		return Issued{}, err
	}
	if row.RevokedAt.Valid {
		// Reuse of an already-rotated token — revoke the whole device chain,
		// not just this row (spec §8): the successor row (different hash) must
		// die too, cutting off both a legitimate user and a possible thief.
		_ = q.RevokeAllRefreshTokensForDevice(ctx, sqlcgen.RevokeAllRefreshTokensForDeviceParams{
			UserID: row.UserID, DeviceID: row.DeviceID,
		})
		return Issued{}, ErrSessionRevoked
	}
	now := time.Now()
	if now.Sub(row.LastUsedAt.Time) > cfg.IdleTimeout || now.After(row.ExpiresAt.Time) {
		_ = q.RevokeRefreshToken(ctx, row.ID)
		return Issued{}, ErrSessionExpired
	}
	user, err := q.GetAppUserByID(ctx, row.UserID)
	if err != nil {
		return Issued{}, err
	}
	if err := q.RevokeRefreshToken(ctx, row.ID); err != nil {
		return Issued{}, err
	}
	return Issue(ctx, q, cfg, IssueParams{
		UserID: row.UserID, CompanyID: row.CompanyID, MerchantID: row.MerchantID,
		Username: user.Username, DeviceID: row.DeviceID, DeviceLabel: row.DeviceLabel, IP: ip,
	})
}

// SwitchMerchant changes the active merchant on the session identified by rawToken
// (the refresh-token cookie) without rotating the refresh token itself — only the
// access token is reissued. Caller (POST /auth/switch-merchant handler) must
// validate newMerchantID against core.user_merchant_role before calling this
// (spec §3a); this function only checks that the session belongs to userID.
func SwitchMerchant(ctx context.Context, q *sqlcgen.Queries, cfg Config, rawToken string, userID, newMerchantID pgtype.UUID) (string, error) {
	row, err := q.GetRefreshTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSession
	}
	if err != nil {
		return "", err
	}
	if row.RevokedAt.Valid {
		return "", ErrSessionRevoked
	}
	if row.UserID != userID {
		return "", ErrSessionMismatch
	}
	if err := q.UpdateRefreshTokenMerchant(ctx, sqlcgen.UpdateRefreshTokenMerchantParams{
		ID: row.ID, MerchantID: newMerchantID,
	}); err != nil {
		return "", err
	}
	user, err := q.GetAppUserByID(ctx, userID)
	if err != nil {
		return "", err
	}
	return auth.GenerateToken(cfg.JWTSecret, userID.String(), row.CompanyID.String(), newMerchantID.String(), user.Username, row.DeviceID, cfg.AccessTokenTTL)
}

// MerchantOption is one entry of the merchant list shown at login when a user
// belongs to more than one merchant (spec §3a).
type MerchantOption struct {
	ID   pgtype.UUID `json:"id"`
	Name string      `json:"name"`
}

// ListUserMerchants calls core.list_user_merchants (migrations/000015), a
// SECURITY DEFINER function that deliberately bypasses user_merchant_role's
// merchant-scoped RLS to enumerate every merchant a user belongs to — sqlc
// can't type a RETURNS TABLE function call reliably, so this is a raw query.
func ListUserMerchants(ctx context.Context, tx pgx.Tx, userID pgtype.UUID) ([]MerchantOption, error) {
	rows, err := tx.Query(ctx, "SELECT id, name FROM core.list_user_merchants($1) ORDER BY name", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MerchantOption
	for rows.Next() {
		var m MerchantOption
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MerchantIsAssigned reports whether merchantID is one of userID's assigned merchants.
// Deliberately goes through ListUserMerchants (RLS-bypassing) rather than a direct
// user_merchant_role query — such a query is scoped to whichever merchant is
// currently active in the session and would wrongly return false for every other
// merchant, including the one a user is trying to select or switch to (spec §3a).
func MerchantIsAssigned(ctx context.Context, tx pgx.Tx, userID, merchantID pgtype.UUID) (bool, error) {
	merchants, err := ListUserMerchants(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	for _, m := range merchants {
		if m.ID == merchantID {
			return true, nil
		}
	}
	return false, nil
}

// RevokeAllExceptCurrent revokes every active refresh-token row for userID except
// the one identified by currentRawToken (if any) — used by the MFA-disable flow
// (2026-09-15-totp-2fa-design.md §8) to cut off other
// devices/sessions as a defense-in-depth measure without logging the caller out
// of the request they're currently making. An empty currentRawToken (no refresh
// cookie on this request) revokes every active session.
func RevokeAllExceptCurrent(ctx context.Context, q *sqlcgen.Queries, userID pgtype.UUID, currentRawToken string) error {
	var exceptHash string
	if currentRawToken != "" {
		exceptHash = hashToken(currentRawToken)
	}
	return q.RevokeAllRefreshTokensForUserExcept(ctx, sqlcgen.RevokeAllRefreshTokensForUserExceptParams{
		UserID:    userID,
		TokenHash: exceptHash,
	})
}

// RevokeByRawToken logs out the device that presented rawToken — used by LogoutHandler.
func RevokeByRawToken(ctx context.Context, q *sqlcgen.Queries, rawToken string) error {
	row, err := q.GetRefreshTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return q.RevokeRefreshToken(ctx, row.ID)
}
