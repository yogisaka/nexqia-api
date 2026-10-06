package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/platformsession"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

const (
	platformAdminUserIDContextKey   = "platform_admin_user_id"
	platformAdminDeviceIDContextKey = "platform_admin_device_id"

	platformRefreshCookieName = "platform_refresh_token"
	// Must match the mounted prefix of every route that reads this cookie
	// (/api/v1/platform/refresh, /logout) — deliberately its OWN name+path,
	// distinct from the tenant refresh_token cookie (/api/v1/auth), even though
	// the path alone would already prevent the browser from ever sending the
	// wrong cookie to the wrong endpoint — avoids any confusion debugging via
	// devtools. See 2026-09-24-platform-admin-foundation-design.md §4.
	platformRefreshCookiePath = "/api/v1/platform"
)

// Permission codes checked via RequirePlatformPermission — must exist in
// platform.permission (bootstrapped by cmd/bootstrap-platform-admin) and be
// granted through platform.role_permission.
const (
	PermPlatformCompanyView           = "platform.company.view"
	PermPlatformCompanyManage         = "platform.company.manage"
	PermPlatformAdminManage           = "platform.admin.manage"
	PermPlatformTenantUserImpersonate = "platform.tenant_user.impersonate"
)

// PlatformTxMiddleware opens one transaction per request for the /platform route
// group — unlike TenantMiddleware/CompanyOnlyMiddleware, it sets NO session GUCs
// at all (no app.current_company_id/app.current_merchant_id), because no table
// under platform.* has RLS (spec §3) and every cross-tenant read goes through an
// explicitly-scoped SECURITY DEFINER function instead (migration 000039).
func PlatformTxMiddleware(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}
		RunRequestTx(c, tx)
	}
}

// PlatformAdminAuthMiddleware validates the Bearer JWT issued by
// PlatformAdminLoginHandler and binds the authenticated admin_user_id into the
// request context. Must run after PlatformTxMiddleware (shares TxFromContext,
// though this middleware itself doesn't need it).
func PlatformAdminAuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
			return
		}
		claims, err := auth.ParsePlatformAdminToken(secret, token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		adminUserID, ok := parseUUID(claims.AdminUserID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token subject"})
			return
		}
		c.Set(platformAdminUserIDContextKey, adminUserID)
		c.Set(platformAdminDeviceIDContextKey, claims.DeviceID)
		// Audit trail: changes made through platform endpoints (e.g. POST /platform/companies
		// creating the owner person) are attributed to this admin.
		if txVal, ok := c.Get(txContextKey); ok {
			if _, err := txVal.(pgx.Tx).Exec(c.Request.Context(),
				"SELECT set_config('app.platform_admin_id', $1, true)", adminUserID.String()); err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set audit context"})
				return
			}
		}
		c.Next()
	}
}

// PlatformAdminUserID returns the authenticated caller's platform.admin_user id.
// Only valid on routes behind PlatformAdminAuthMiddleware.
func PlatformAdminUserID(c *gin.Context) pgtype.UUID {
	return c.MustGet(platformAdminUserIDContextKey).(pgtype.UUID)
}

// PlatformAdminDeviceID returns the authenticated caller's device id (mirrors
// AuthDeviceID for tenant sessions) — not consumed by any 0d-1 handler yet, but
// kept accessible rather than write-only, since 0d-4's audit trail (spec §10)
// will need it. Only valid on routes behind PlatformAdminAuthMiddleware.
func PlatformAdminDeviceID(c *gin.Context) string {
	return c.MustGet(platformAdminDeviceIDContextKey).(string)
}

// RequirePlatformPermission checks the caller holds `code` via
// platform.admin_user_role -> platform.role_permission — no merchant/company
// header involved at all, unlike RequirePermission (tenant). On failure it
// writes the response and returns false — callers must `return` immediately.
func RequirePlatformPermission(c *gin.Context, code string) bool {
	q := sqlcgen.New(TxFromContext(c))
	has, err := q.AdminUserHasPlatformPermission(c.Request.Context(), sqlcgen.AdminUserHasPlatformPermissionParams{
		AdminUserID: PlatformAdminUserID(c), Code: code,
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if !has {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + code})
		return false
	}
	return true
}

// checkPlatformLoginRateLimit mirrors checkLoginRateLimit (internal/server/ratelimit.go)
// exactly, with its own key prefix — a shared prefix with tenant login would mean
// a burst of tenant login attempts from one IP could incidentally rate-limit a
// platform admin logging in from the same IP (e.g. an office NAT), which is
// confusing even though not a security hole. Same config values reused (no new
// config field), spec §4 "TTL sesi" note.
func checkPlatformLoginRateLimit(c *gin.Context, limiter *ratelimit.Limiter, cfg config.Config, username string) bool {
	keys := []string{
		"platform_login:user:" + username,
		"platform_login:ip:" + c.ClientIP(),
	}
	for _, key := range keys {
		result, err := limiter.AllowSlidingWindow(c.Request.Context(), key, cfg.RateLimitLoginMaxAttempts, cfg.RateLimitLoginWindowSeconds)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return false
		}
		if !result.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts, try again later"})
			return false
		}
	}
	return true
}

// platformSessionConfig adapts config.Config's env-driven tunables into
// platformsession.Config — reuses the SAME TTL values as tenant sessions
// (spec §4 "TTL sesi": deliberate, not an oversight — shorter TTLs for this
// high-privilege account type is a future hardening step, not 0d-1 scope).
func platformSessionConfig(cfg config.Config) platformsession.Config {
	return platformsession.Config{
		JWTSecret:      cfg.JWTSecret,
		AccessTokenTTL: time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute,
		IdleTimeout:    time.Duration(cfg.RefreshTokenIdleTimeoutHours) * time.Hour,
		AbsoluteTTL:    time.Duration(cfg.RefreshTokenAbsoluteTTLDays) * 24 * time.Hour,
	}
}

func setPlatformRefreshCookie(c *gin.Context, cfg config.Config, token string, expiresAt time.Time) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(platformRefreshCookieName, token, int(time.Until(expiresAt).Seconds()), platformRefreshCookiePath, "", cfg.CookieSecure, true)
}

func clearPlatformRefreshCookie(c *gin.Context, cfg config.Config) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(platformRefreshCookieName, "", -1, platformRefreshCookiePath, "", cfg.CookieSecure, true)
}
