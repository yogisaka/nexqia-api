// internal/server/auth.go
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

const (
	authUserIDContextKey    = "auth_user_id"
	authCompanyIDContextKey = "auth_company_id"
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
func RequirePermission(c *gin.Context, code string) bool {
	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return false
	}
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

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginHandler authenticates against core.app_user scoped to the X-Company-ID header
// (RLS-enforced, see TenantMiddleware), enforces per-username/IP sliding-window rate
// limiting before any password comparison, and issues a JWT carrying user_id + company_id.
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
		token, err := auth.GenerateToken(secret, user.ID.String(), user.CompanyID.String(), user.Username)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue token"})
			return
		}
		_ = q.TouchAppUserLastLogin(c.Request.Context(), user.ID)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"token": token, "expires_in": auth.TokenTTL.Seconds()}, "meta": gin.H{}})
	}
}
