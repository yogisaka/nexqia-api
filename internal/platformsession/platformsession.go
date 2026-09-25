// Session/refresh-token model for platform.admin_user — deliberately NOT part of
// internal/session (that package is hardcoded to core.refresh_token, which has
// NOT NULL company_id/merchant_id + RLS + a tenant-consistency trigger, none of
// which apply here). Same algorithm, separate table. See
// docs/design/specs/2026-09-24-platform-admin-foundation-design.md §4.
package platformsession

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
	// ErrNoSession means the presented refresh token doesn't match any known row.
	ErrNoSession = errors.New("no session")
	// ErrSessionRevoked means the refresh token was already rotated/revoked —
	// reusing it indicates possible theft. The whole device's session chain is
	// revoked as a side effect of detecting this.
	ErrSessionRevoked = errors.New("session revoked")
	// ErrSessionExpired means idle-timeout or the absolute TTL was exceeded.
	ErrSessionExpired = errors.New("session expired")
)

// Issued is what a caller (login/refresh handlers) hands back to the client: the
// access token for the response body and the raw refresh token for the
// Set-Cookie header. RefreshExpiresAt sets the cookie's Max-Age.
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
	AdminUserID pgtype.UUID
	Username    string
	DeviceID    string
	DeviceLabel string
	IP          netip.Addr
}

// Issue creates a new refresh-token row and access token.
func Issue(ctx context.Context, q *sqlcgen.Queries, cfg Config, p IssueParams) (Issued, error) {
	raw, err := generateRawToken()
	if err != nil {
		return Issued{}, err
	}
	expiresAt := time.Now().Add(cfg.AbsoluteTTL)
	row, err := q.CreatePlatformAdminRefreshToken(ctx, sqlcgen.CreatePlatformAdminRefreshTokenParams{
		AdminUserID: p.AdminUserID,
		DeviceID:    p.DeviceID,
		DeviceLabel: p.DeviceLabel,
		TokenHash:   hashToken(raw),
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
		CreatedIp:   p.IP,
	})
	if err != nil {
		return Issued{}, err
	}
	accessToken, err := auth.GeneratePlatformAdminToken(cfg.JWTSecret, p.AdminUserID.String(), p.Username, p.DeviceID, cfg.AccessTokenTTL)
	if err != nil {
		return Issued{}, err
	}
	return Issued{AccessToken: accessToken, RefreshToken: raw, RefreshExpiresAt: row.ExpiresAt.Time}, nil
}

// Refresh validates the presented raw refresh token and, if valid, rotates it:
// the old row is revoked and a new one (same device_id) replaces it.
func Refresh(ctx context.Context, q *sqlcgen.Queries, cfg Config, rawToken string, ip netip.Addr) (Issued, error) {
	row, err := q.GetPlatformAdminRefreshTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrNoSession
	}
	if err != nil {
		return Issued{}, err
	}
	if row.RevokedAt.Valid {
		// Reuse of an already-rotated token — revoke the whole device chain,
		// not just this row: the successor row (different hash) must die too.
		_ = q.RevokeAllPlatformAdminRefreshTokensForDevice(ctx, sqlcgen.RevokeAllPlatformAdminRefreshTokensForDeviceParams{
			AdminUserID: row.AdminUserID, DeviceID: row.DeviceID,
		})
		return Issued{}, ErrSessionRevoked
	}
	now := time.Now()
	if now.Sub(row.LastUsedAt.Time) > cfg.IdleTimeout || now.After(row.ExpiresAt.Time) {
		_ = q.RevokePlatformAdminRefreshToken(ctx, row.ID)
		return Issued{}, ErrSessionExpired
	}
	adminUser, err := q.GetPlatformAdminUserByID(ctx, row.AdminUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Admin deactivated or soft-deleted since login: kill this device's
		// whole chain. Returned as ErrSessionRevoked so the handler answers 401
		// with plain c.JSON and this revocation commits.
		_ = q.RevokeAllPlatformAdminRefreshTokensForDevice(ctx, sqlcgen.RevokeAllPlatformAdminRefreshTokensForDeviceParams{
			AdminUserID: row.AdminUserID, DeviceID: row.DeviceID,
		})
		return Issued{}, ErrSessionRevoked
	}
	if err != nil {
		return Issued{}, err
	}
	if err := q.RevokePlatformAdminRefreshToken(ctx, row.ID); err != nil {
		return Issued{}, err
	}
	return Issue(ctx, q, cfg, IssueParams{
		AdminUserID: row.AdminUserID, Username: adminUser.Username,
		DeviceID: row.DeviceID, DeviceLabel: row.DeviceLabel, IP: ip,
	})
}

// RevokeByRawToken logs out the device that presented rawToken.
func RevokeByRawToken(ctx context.Context, q *sqlcgen.Queries, rawToken string) error {
	row, err := q.GetPlatformAdminRefreshTokenByHash(ctx, hashToken(rawToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return q.RevokePlatformAdminRefreshToken(ctx, row.ID)
}
