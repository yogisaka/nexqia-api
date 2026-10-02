// internal/server/security_settings.go
// Merchant security settings — PIN-lock toggle + idle window and MFA-nudge
// policy, with password re-confirmation and an immutable change history
// (2026-09-29-merchant-security-settings-design.md §3–§5, §7, §8).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

// RegisterSecuritySettingsRoutes wires GET/PUT /settings/security and
// GET /settings/security/history under the locked group (spec §7): reading the
// page while locked is harmless, PUT is gated by a password re-confirmation.
func RegisterSecuritySettingsRoutes(rg *gin.RouterGroup, hasher *auth.PasswordHasher, limiter *ratelimit.Limiter, redisClient *redis.Client, cfg config.Config) {
	rg.GET("/settings/security", GetSecuritySettingsHandler(cfg))
	rg.PUT("/settings/security", PutSecuritySettingsHandler(hasher, limiter, redisClient, cfg))
	rg.GET("/settings/security/history", ListSecuritySettingHistoryHandler)
}

// canViewSecuritySettings reports whether the caller may open the security
// settings page: holders of core.merchant.manage OR audit.log.view (the viewer
// is read-only, plan review focus 5). canEdit additionally requires an
// un-impersonated session. Writes the response and returns ok=false on any
// failure — callers must return immediately.
func canViewSecuritySettings(c *gin.Context) (canEdit bool, ok bool) {
	merchantID, parsed := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !parsed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return false, false
	}
	q := sqlcgen.New(TxFromContext(c))
	// Both checks scope to the session's ACTIVE role, not the union across the
	// caller's roles (spec 2026-10-01-active-role-scope-design §3.4). Tokens
	// without a "rid" claim (registration auto-login issued before the merchant
	// existed, legacy tokens) fall back to the caller's default role at the
	// merchant — same fallback as RequirePermissionForMerchant.
	roleID := AuthRoleID(c)
	if !roleID.Valid {
		def, err := q.DefaultUserRole(c.Request.Context(), sqlcgen.DefaultUserRoleParams{
			UserID: AuthUserID(c), MerchantID: merchantID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false, false
		}
		roleID = def // invalid ⇒ no assignment ⇒ both checks below fail → 403
	}
	manage, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: PermMerchantManage, RoleID: roleID,
	})
	if err != nil {
		respondInternalError(c, err)
		return false, false
	}
	if !manage {
		has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
			UserID: AuthUserID(c), MerchantID: merchantID, Code: PermAuditLogView, RoleID: roleID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false, false
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "missing permission"})
			return false, false
		}
	}
	return manage && AuthImpersonatedBy(c) == "", true
}

// securitySettingsData builds the shared GET/PUT response body: the current
// PIN-lock flag (with its effective idle window), the allowed idle range, the
// MFA-nudge policy, per-user adoption stats, and whether the caller may edit.
func securitySettingsData(c *gin.Context, q *sqlcgen.Queries, cfg config.Config, merchantID pgtype.UUID, canEdit bool) (gin.H, error) {
	flag := getPinLockFlag(c, q, merchantID)
	stats, err := q.SecurityUserStats(c.Request.Context(), sqlcgen.SecurityUserStatsParams{
		MerchantID: merchantID, CompanyID: AuthCompanyID(c),
	})
	if err != nil {
		return nil, err
	}
	return gin.H{
		"pin_lock": gin.H{
			"enabled":      flag.Enabled,
			"idle_minutes": pinLockIdleMinutes(cfg, flag),
		},
		"limits": gin.H{
			"min_idle_minutes": cfg.AppLockMinIdleMinutes,
			"max_idle_minutes": cfg.AppLockMaxIdleMinutes,
		},
		"require_totp":      checkMFANudge(c, q, merchantID),
		"users_total":       stats.UsersTotal,
		"users_without_pin": stats.UsersWithoutPin,
		"users_without_mfa": stats.UsersWithoutMfa,
		"can_edit":          canEdit,
	}, nil
}

// GetSecuritySettingsHandler godoc
// @Summary Show the merchant's security settings
// @Description Returns the PIN-lock state (enabled + effective idle window), the
// @Description allowed idle range, the MFA-nudge policy, per-user PIN/MFA
// @Description adoption stats, and can_edit (core.merchant.manage holders outside
// @Description impersonation; audit.log.view holders get a read-only view).
// @Tags settings
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /settings/security [get]
func GetSecuritySettingsHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		canEdit, ok := canViewSecuritySettings(c)
		if !ok {
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		q := sqlcgen.New(TxFromContext(c))
		data, err := securitySettingsData(c, q, cfg, merchantID, canEdit)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
	}
}

type securitySettingsRequest struct {
	Password string `json:"password" binding:"required"`
	PinLock  *struct {
		Enabled     *bool `json:"enabled" binding:"required"`
		IdleMinutes *int  `json:"idle_minutes" binding:"required"`
	} `json:"pin_lock" binding:"required"`
	RequireTOTP *bool `json:"require_totp" binding:"required"`
}

// PutSecuritySettingsHandler godoc
// @Summary Update the merchant's security settings
// @Description Re-confirms the caller's password (rate-limited per user), then
// @Description saves the PIN-lock flag (enabled + idle minutes inside the
// @Description configured range) and the MFA-nudge policy. Every change lands in
// @Description the immutable security setting log. Enabling the lock seeds the
// @Description caller's own device so the saver is not locked out mid-session.
// @Tags settings
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body securitySettingsRequest true "Password + PIN-lock + MFA policy"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 429 {object} apiErrorResponse
// @Router /settings/security [put]
func PutSecuritySettingsHandler(hasher *auth.PasswordHasher, limiter *ratelimit.Limiter, redisClient *redis.Client, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		if !RequirePermission(c, PermMerchantManage) {
			return
		}
		var req securitySettingsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		result, err := limiter.AllowSlidingWindow(c.Request.Context(), "security_settings:"+userID.String(), cfg.RateLimitLoginMaxAttempts, cfg.RateLimitLoginWindowSeconds)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return
		}
		if !result.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts, try again later"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		if !verifyOwnPassword(c, q, hasher, userID, req.Password) {
			return
		}
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		idle := *req.PinLock.IdleMinutes
		if idle < cfg.AppLockMinIdleMinutes || idle > cfg.AppLockMaxIdleMinutes {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("idle_minutes must be between %d and %d", cfg.AppLockMinIdleMinutes, cfg.AppLockMaxIdleMinutes)})
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		pinValue, err := json.Marshal(pinLockFlag{Enabled: *req.PinLock.Enabled, IdleMinutes: idle})
		if err != nil {
			abortInternalError(c, err)
			return
		}
		if _, err := q.UpsertFeatureFlag(c.Request.Context(), sqlcgen.UpsertFeatureFlagParams{
			MerchantID: merchantID, FlagKey: pinLockFlagKey, FlagValue: pinValue, CreatedBy: userID,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
		totpValue, err := json.Marshal(*req.RequireTOTP)
		if err != nil {
			abortInternalError(c, err)
			return
		}
		if _, err := q.UpsertFeatureFlag(c.Request.Context(), sqlcgen.UpsertFeatureFlagParams{
			MerchantID: merchantID, FlagKey: mfaNudgeFlagKey, FlagValue: totpValue, CreatedBy: userID,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
		data, err := securitySettingsData(c, q, cfg, merchantID, true)
		if err != nil {
			abortInternalError(c, err)
			return
		}
		// Turning the lock ON must not lock the admin who just saved it (plan
		// review focus 1) — seed their device's applock key exactly like
		// PinSetHandler seeds it for users who set a PIN while the flag is on.
		// Other PIN-enrolled users (e.g. staff) lock on their next request.
		if *req.PinLock.Enabled && user.PinHash.Valid {
			if err := redisClient.Set(c.Request.Context(), applockKey(userID, AuthDeviceID(c)), "1", time.Duration(idle)*time.Minute).Err(); err != nil {
				log.Printf("failed to seed applock key after enabling pin lock: %v", err)
			}
			data["pin_lock_idle_minutes"] = idle
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
	}
}

// ListSecuritySettingHistoryHandler godoc
// @Summary List the merchant's security setting changes
// @Description Returns core.security_setting_log rows (who changed which flag,
// @Description from what to what), newest first. old_value/new_value are raw
// @Description JSON (null when the flag had no previous value) — the log is
// @Description written only by the DB trigger and never by this endpoint.
// @Tags settings
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /settings/security/history [get]
func ListSecuritySettingHistoryHandler(c *gin.Context) {
	if _, ok := canViewSecuritySettings(c); !ok {
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	limit, offset := auditLimitOffset(c)
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListSecuritySettingLog(c.Request.Context(), sqlcgen.ListSecuritySettingLogParams{
		MerchantID: merchantID, RowOffset: offset, RowLimit: limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load history"})
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":                uuidOrNil(r.ID),
			"flag_key":          r.FlagKey,
			"old_value":         rawJSONOrNil(r.OldValue),
			"new_value":         rawJSONOrNil(r.NewValue),
			"changed_by":        uuidOrNil(r.ChangedBy),
			"changed_by_name":   textOrNil(r.ChangedByName),
			"platform_admin_id": uuidOrNil(r.PlatformAdminID),
			"changed_at":        rfc3339OrNil(r.ChangedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// rawJSONOrNil renders a jsonb []byte column as raw JSON (json.RawMessage, so
// gin doesn't base64-encode it) or nil (JSON null) when the column was NULL.
func rawJSONOrNil(v []byte) any {
	if v == nil {
		return nil
	}
	return json.RawMessage(v)
}

// textOrNil renders a nullable text column as string or nil (no repo helper existed).
func textOrNil(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}
