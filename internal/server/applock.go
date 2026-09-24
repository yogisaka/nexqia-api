// internal/server/applock.go
// PIN-unlock enforcement — see docs/design/specs/2026-09-15-pin-unlock-design.md §6.
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// pinLockFlagKey is the core.feature_flag key a merchant opts into (spec §3).
const pinLockFlagKey = "auth.pin_lock"

// pinLockFlag mirrors the flag_value jsonb shape {"enabled": bool, "idle_minutes": int}.
type pinLockFlag struct {
	Enabled     bool `json:"enabled"`
	IdleMinutes int  `json:"idle_minutes"`
}

// getPinLockFlag reads merchantID's auth.pin_lock flag. A missing flag or one that
// fails to parse is treated as disabled (zero value) — never as an error, since an
// unset flag is the normal/default state for a merchant that hasn't opted in.
// Callers must already be running under a transaction with app.current_merchant_id
// set for merchantID (TenantMiddleware does this for every route in the protected
// group) — see getPinLockFlagAtLogin in auth.go for the one path that isn't.
func getPinLockFlag(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) pinLockFlag {
	flag, err := q.GetFeatureFlag(c.Request.Context(), sqlcgen.GetFeatureFlagParams{MerchantID: merchantID, FlagKey: pinLockFlagKey})
	if err != nil {
		return pinLockFlag{}
	}
	var out pinLockFlag
	if err := json.Unmarshal(flag.FlagValue, &out); err != nil {
		return pinLockFlag{}
	}
	return out
}

// pinLockIdleMinutes resolves the effective idle window: the merchant's own
// idle_minutes always wins when set (spec §3), falling back to
// APP_LOCK_DEFAULT_IDLE_MINUTES only if unset/invalid (<= 0).
func pinLockIdleMinutes(cfg config.Config, flag pinLockFlag) int {
	if flag.IdleMinutes > 0 {
		return flag.IdleMinutes
	}
	return cfg.AppLockDefaultIdleMinutes
}

func applockKey(userID pgtype.UUID, deviceID string) string {
	return "applock:" + userID.String() + ":" + deviceID
}

func pinfailKey(userID pgtype.UUID, deviceID string) string {
	return "pinfail:" + userID.String() + ":" + deviceID
}

// AppLockMiddleware enforces PIN re-verification after idle timeout, per merchant
// opt-in and per user enrollment (spec §6). Must run after TenantMiddleware (needs
// X-Merchant-ID's RLS context already set for the feature-flag read) and
// AuthMiddleware (needs AuthUserID/AuthDeviceID). Deliberately NOT applied to
// /auth/pin/set, /auth/pin/disable, /auth/pin/verify — see server.go.
func AppLockMiddleware(redisClient *redis.Client, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if AuthImpersonatedBy(c) != "" {
			// Impersonation sessions skip idle-lock entirely — the admin has
			// no way to unlock with the target user's own PIN. See
			// docs/design/specs/2026-09-24-platform-admin-impersonate-design.md §4.
			c.Next()
			return
		}
		merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
		if !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		flag := getPinLockFlag(c, q, merchantID)
		if !flag.Enabled {
			c.Next()
			return
		}
		userID := AuthUserID(c)
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !user.PinHash.Valid {
			// Soft opt-in: flag on, user hasn't enrolled a PIN yet — no enforcement (spec §3).
			c.Next()
			return
		}
		key := applockKey(userID, AuthDeviceID(c))
		idleMinutes := pinLockIdleMinutes(cfg, flag)
		// EXPIRE returns true only if the key exists — this is check-and-refresh
		// (sliding window) in one atomic round trip, no separate EXISTS call needed.
		refreshed, err := redisClient.Expire(c.Request.Context(), key, time.Duration(idleMinutes)*time.Minute).Result()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return
		}
		if !refreshed {
			c.AbortWithStatusJSON(http.StatusLocked, gin.H{"error": "locked", "requires_pin": true})
			return
		}
		c.Next()
	}
}
