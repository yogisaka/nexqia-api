// internal/server/auth.go
package server

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
	"github.com/yogisaka/nexqia-api/internal/session"
)

const (
	authUserIDContextKey    = "auth_user_id"
	authCompanyIDContextKey = "auth_company_id"

	refreshCookieName = "refresh_token"
	refreshCookiePath = "/auth"

	merchantSelectionTokenTTL = 5 * time.Minute
)

// Permission codes checked via RequirePermission — must exist in core.permission
// (seeded/migrated, see docs/07-core-ddl.md §2) and be granted through core.role_permission.
const (
	PermCompanyManage  = "core.company.manage"
	PermMerchantManage = "core.merchant.manage"
	PermUserManage     = "core.user.manage"
	PermRoleManage     = "core.role.manage"
)

// AuthMiddleware validates the Bearer JWT issued by LoginHandler and binds the
// authenticated user_id/company_id into the request context. It must run after
// TenantMiddleware (needs TxFromContext for nothing here, but shares the request tx).
func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
			return
		}
		claims, err := auth.ParseToken(secret, token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		userID, ok := parseUUID(claims.UserID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token subject"})
			return
		}
		companyID, ok := parseUUID(claims.CompanyID)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token company"})
			return
		}
		c.Set(authUserIDContextKey, userID)
		c.Set(authCompanyIDContextKey, companyID)
		c.Next()
	}
}

// AuthUserID returns the authenticated caller's user id. Only valid on routes behind AuthMiddleware.
func AuthUserID(c *gin.Context) pgtype.UUID {
	return c.MustGet(authUserIDContextKey).(pgtype.UUID)
}

// AuthCompanyID returns the authenticated caller's company id. Only valid on routes behind AuthMiddleware.
func AuthCompanyID(c *gin.Context) pgtype.UUID {
	return c.MustGet(authCompanyIDContextKey).(pgtype.UUID)
}

// RequirePermission checks the caller holds `code` in the merchant from X-Merchant-ID
// (already required by TenantMiddleware). On failure it writes the response and returns false —
// callers must `return` immediately when this returns false.
//
// Only use this when the request has no single mutation target that could differ from
// the header (e.g. listing, or creating a brand-new resource). When a handler mutates
// an existing resource scoped to a specific merchant, check RequirePermissionForMerchant
// against that resource's actual merchant instead — otherwise a caller with permission
// at merchant A (declared via header) could mutate merchant B's data merely by sharing
// a company, which breaks the per-merchant role isolation merger scenarios require.
func RequirePermission(c *gin.Context, code string) bool {
	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return false
	}
	return RequirePermissionForMerchant(c, code, merchantID)
}

// RequirePermissionForMerchant checks the caller holds `code` at merchantID specifically —
// use this for mutations on an existing resource so the permission check is scoped to the
// resource being mutated, not whatever merchant the caller happened to declare in X-Merchant-ID.
// On failure it writes the response and returns false — callers must `return` immediately.
func RequirePermissionForMerchant(c *gin.Context, code string, merchantID pgtype.UUID) bool {
	q := sqlcgen.New(TxFromContext(c))
	has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: code,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	if !has {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + code})
		return false
	}
	return true
}

// sessionConfig adapts config.Config's env-driven tunables into session.Config.
func sessionConfig(cfg config.Config) session.Config {
	return session.Config{
		JWTSecret:      cfg.JWTSecret,
		AccessTokenTTL: time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute,
		IdleTimeout:    time.Duration(cfg.RefreshTokenIdleTimeoutHours) * time.Hour,
		AbsoluteTTL:    time.Duration(cfg.RefreshTokenAbsoluteTTLDays) * 24 * time.Hour,
	}
}

func setRefreshCookie(c *gin.Context, cfg config.Config, token string, expiresAt time.Time) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, token, int(time.Until(expiresAt).Seconds()), refreshCookiePath, "", cfg.CookieSecure, true)
}

func clearRefreshCookie(c *gin.Context, cfg config.Config) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, "", -1, refreshCookiePath, "", cfg.CookieSecure, true)
}

// deviceHeaders reads the client-generated device identity (spec §6): X-Device-Id
// is required (client persists it in localStorage across sessions), device_label
// is derived from User-Agent since parsing it further needs no extra dependency.
func deviceHeaders(c *gin.Context) (deviceID, deviceLabel string, ip netip.Addr, err error) {
	deviceID = c.GetHeader("X-Device-Id")
	if deviceID == "" {
		return "", "", netip.Addr{}, errors.New("X-Device-Id header is required")
	}
	deviceLabel = c.GetHeader("User-Agent")
	if deviceLabel == "" {
		deviceLabel = "Unknown device"
	} else if len(deviceLabel) > 255 {
		deviceLabel = deviceLabel[:255]
	}
	if parsed, parseErr := netip.ParseAddr(c.ClientIP()); parseErr == nil {
		ip = parsed
	} else {
		ip = netip.IPv4Unspecified()
	}
	return deviceID, deviceLabel, ip, nil
}

// checkDeviceLimit enforces core.company.max_concurrent_sessions (spec §6/§8):
// exceeding it is an explicit 409, not a silent auto-kick of the oldest device.
// On failure it writes the response and returns false.
func checkDeviceLimit(c *gin.Context, q *sqlcgen.Queries, userID, companyID pgtype.UUID) bool {
	ctx := c.Request.Context()
	max, err := q.GetCompanyMaxConcurrentSessions(ctx, companyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	count, err := q.CountActiveRefreshTokens(ctx, sqlcgen.CountActiveRefreshTokensParams{UserID: userID, CompanyID: companyID})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	if count >= int64(max) {
		active, _ := q.ListActiveRefreshTokens(ctx, userID)
		c.JSON(http.StatusConflict, gin.H{"error": "device limit reached", "data": gin.H{"active_sessions": active}})
		return false
	}
	return true
}

// issueLoginSession finishes login once the active merchant is known (either the
// user's only merchant, or one chosen via SelectMerchantHandler) — shared by both.
func issueLoginSession(c *gin.Context, cfg config.Config, q *sqlcgen.Queries, userID, companyID, merchantID pgtype.UUID, username string) {
	deviceID, deviceLabel, ip, err := deviceHeaders(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !checkDeviceLimit(c, q, userID, companyID) {
		return
	}
	issued, err := session.Issue(c.Request.Context(), q, sessionConfig(cfg), session.IssueParams{
		UserID: userID, CompanyID: companyID, MerchantID: merchantID,
		Username: username, DeviceID: deviceID, DeviceLabel: deviceLabel, IP: ip,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue session"})
		return
	}
	setRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token":       issued.AccessToken,
		"expires_in":  int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
		"merchant_id": merchantID.String(),
	}, "meta": gin.H{}})
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginHandler authenticates against core.app_user scoped to the X-Company-ID header
// (RLS-enforced, must run behind CompanyOnlyMiddleware — the active merchant isn't
// known yet), enforces per-username/IP sliding-window rate limiting before any
// password comparison, and — once the active merchant is resolved — issues a JWT
// access token plus an HttpOnly refresh-token cookie (spec §3a, §6).
// On successful login with a legacy bcrypt hash, transparently rehashes to Argon2id
// (see docs/design/specs/2026-09-15-ratelimit-hardening-design.md §11).
func LoginHandler(secret string, limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		companyID, ok := parseUUID(c.GetHeader("X-Company-ID"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Company-ID"})
			return
		}
		var req loginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !checkLoginRateLimit(c, limiter, cfg, req.Username) {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByUsername(c.Request.Context(), sqlcgen.GetAppUserByUsernameParams{
			CompanyID: companyID, Username: req.Username,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), user.PasswordHash, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		if hasher.NeedsRehash(user.PasswordHash) {
			if newHash, rehashErr := hasher.Hash(c.Request.Context(), req.Password); rehashErr == nil {
				_ = q.UpdateAppUserPassword(c.Request.Context(), sqlcgen.UpdateAppUserPasswordParams{
					ID: user.ID, PasswordHash: newHash, UpdatedBy: user.ID,
				})
			}
		}
		_ = q.TouchAppUserLastLogin(c.Request.Context(), user.ID)

		merchants, err := session.ListUserMerchants(c.Request.Context(), TxFromContext(c), user.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if len(merchants) == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "user has no merchant assignment"})
			return
		}
		if len(merchants) > 1 {
			selToken, err := auth.GenerateMerchantSelectionToken(secret, user.ID.String(), merchantSelectionTokenTTL)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue selection token"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": gin.H{
				"requires_merchant_selection": true,
				"selection_token":             selToken,
				"merchants":                   merchants,
			}, "meta": gin.H{}})
			return
		}

		issueLoginSession(c, cfg, q, user.ID, user.CompanyID, merchants[0].ID, user.Username)
	}
}

type selectMerchantRequest struct {
	SelectionToken string `json:"selection_token" binding:"required"`
	MerchantID     string `json:"merchant_id" binding:"required"`
}

// SelectMerchantHandler completes a login that LoginHandler paused for merchant
// selection (spec §3a) — consumes the short-lived selection token instead of
// asking for the password again, validates the chosen merchant against
// core.user_merchant_role, then issues the session exactly like a single-merchant
// login would. Must run behind CompanyOnlyMiddleware (same X-Company-ID as login).
func SelectMerchantHandler(secret string, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req selectMerchantRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		claims, err := auth.ParseMerchantSelectionToken(secret, req.SelectionToken)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired selection token"})
			return
		}
		userID, ok := parseUUID(claims.UserID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid selection token subject"})
			return
		}
		merchantID, ok := parseUUID(req.MerchantID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
			return
		}
		has, err := session.MerchantIsAssigned(c.Request.Context(), TxFromContext(c), userID, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "user is not assigned to this merchant"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		issueLoginSession(c, cfg, q, user.ID, user.CompanyID, merchantID, user.Username)
	}
}

// RefreshHandler rotates the refresh-token cookie and reissues an access token
// (spec §7). Must run behind CompanyOnlyMiddleware (needs X-Company-ID for
// core.refresh_token's RLS; the active merchant is read back off the stored row).
func RefreshHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		ip, parseErr := netip.ParseAddr(c.ClientIP())
		if parseErr != nil {
			ip = netip.IPv4Unspecified()
		}
		q := sqlcgen.New(TxFromContext(c))
		issued, err := session.Refresh(c.Request.Context(), q, sessionConfig(cfg), rawToken, ip)
		if err != nil {
			clearRefreshCookie(c, cfg)
			switch {
			case errors.Is(err, session.ErrNoSession), errors.Is(err, session.ErrSessionExpired), errors.Is(err, session.ErrSessionRevoked):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}
		setRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"token":      issued.AccessToken,
			"expires_in": int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
		}, "meta": gin.H{}})
	}
}

// LogoutHandler revokes the session tied to the refresh-token cookie. Deliberately
// does not require a valid access token (a user with an expired access token but
// still-valid refresh token must still be able to log out) — runs behind
// CompanyOnlyMiddleware only.
func LogoutHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(refreshCookieName)
		if err == nil && rawToken != "" {
			q := sqlcgen.New(TxFromContext(c))
			_ = session.RevokeByRawToken(c.Request.Context(), q, rawToken)
		}
		clearRefreshCookie(c, cfg)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}

type switchMerchantRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
}

// SwitchMerchantHandler lets an already-logged-in user change their session's
// active merchant without re-entering a password (spec §3a) — only the access
// token is reissued, the refresh token is not rotated. Runs behind AuthMiddleware.
func SwitchMerchantHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req switchMerchantRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		merchantID, ok := parseUUID(req.MerchantID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
			return
		}
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		userID := AuthUserID(c)
		has, err := session.MerchantIsAssigned(c.Request.Context(), TxFromContext(c), userID, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !has {
			c.JSON(http.StatusForbidden, gin.H{"error": "user is not assigned to this merchant"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		token, err := session.SwitchMerchant(c.Request.Context(), q, sessionConfig(cfg), rawToken, userID, merchantID)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrNoSession), errors.Is(err, session.ErrSessionRevoked), errors.Is(err, session.ErrSessionMismatch):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"token":       token,
			"expires_in":  int((time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute).Seconds()),
			"merchant_id": merchantID.String(),
		}, "meta": gin.H{}})
	}
}

// ListSessionsHandler lists the caller's active devices (spec §6, for a
// "log out this device" UI). Runs behind AuthMiddleware.
func ListSessionsHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	sessions, err := q.ListActiveRefreshTokens(c.Request.Context(), AuthUserID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sessions, "meta": gin.H{}})
}

// RevokeSessionHandler revokes one of the caller's own devices by refresh-token
// row id — ownership-checked so a user can only revoke their own sessions.
// Runs behind AuthMiddleware.
func RevokeSessionHandler(c *gin.Context) {
	sessionID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	row, err := q.GetRefreshTokenByID(c.Request.Context(), sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if row.UserID != AuthUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err := q.RevokeRefreshToken(c.Request.Context(), sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}
