// NEXQIA Platform Admin — foundation (0d-1). Genuinely separate auth/access
// system from tenant staff, see docs/design/specs/2026-09-24-platform-admin-foundation-design.md.
package server

import (
	"errors"
	"net/http"
	"net/netip"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/platformsession"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

func RegisterPlatformRoutes(rg *gin.RouterGroup, limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher) {
	rg.POST("/login", PlatformAdminLoginHandler(limiter, cfg, hasher))
	rg.POST("/refresh", PlatformAdminRefreshHandler(cfg))

	protected := rg.Group("", PlatformAdminAuthMiddleware(cfg.JWTSecret))
	protected.POST("/logout", PlatformAdminLogoutHandler(cfg))
	protected.GET("/companies", ListPlatformCompaniesHandler)
	protected.GET("/companies/:id", GetPlatformCompanyDetailHandler)
	protected.POST("/companies/:id/suspend", SuspendPlatformCompanyHandler)
	protected.POST("/companies/:id/activate", ActivatePlatformCompanyHandler)
	protected.GET("/roles", ListPlatformRolesHandler)
	protected.POST("/admin-users", CreatePlatformAdminUserHandler(hasher))
}

type platformAdminLoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// PlatformAdminLoginHandler godoc
// @Summary Platform admin login
// @Tags platform
// @Accept json
// @Produce json
// @Param request body platformAdminLoginRequest true "Credentials"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /platform/login [post]
func PlatformAdminLoginHandler(limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req platformAdminLoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !checkPlatformLoginRateLimit(c, limiter, cfg, req.Username) {
			return
		}
		deviceID, deviceLabel, ip, err := deviceHeaders(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		adminUser, err := q.GetPlatformAdminUserByUsername(c.Request.Context(), req.Username)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !adminUser.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), adminUser.PasswordHash, req.Password)
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
		_ = q.TouchPlatformAdminUserLastLogin(c.Request.Context(), adminUser.ID)

		issued, err := platformsession.Issue(c.Request.Context(), q, platformSessionConfig(cfg), platformsession.IssueParams{
			AdminUserID: adminUser.ID, Username: adminUser.Username,
			DeviceID: deviceID, DeviceLabel: deviceLabel, IP: ip,
		})
		if err != nil {
			// AbortWithStatusJSON, NOT plain JSON — TouchPlatformAdminUserLastLogin
			// above already wrote in this SAME tx; a plain c.JSON would let it
			// commit despite the 500, mutating last_login_at for a failed login
			// (PlatformTxMiddleware rolls back only when c.IsAborted()).
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setPlatformRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"token": issued.AccessToken,
			"admin_user": gin.H{
				"id": adminUser.ID, "username": adminUser.Username, "full_name": adminUser.FullName,
			},
		}, "meta": gin.H{}})
	}
}

// PlatformAdminRefreshHandler godoc
// @Summary Refresh a platform admin access token
// @Tags platform
// @Produce json
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /platform/refresh [post]
func PlatformAdminRefreshHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(platformRefreshCookieName)
		if err != nil || rawToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		ip, parseErr := netip.ParseAddr(c.ClientIP())
		if parseErr != nil {
			ip = netip.IPv4Unspecified()
		}
		q := sqlcgen.New(TxFromContext(c))
		issued, err := platformsession.Refresh(c.Request.Context(), q, platformSessionConfig(cfg), rawToken, ip)
		if err != nil {
			clearPlatformRefreshCookie(c, cfg)
			switch {
			case errors.Is(err, platformsession.ErrNoSession), errors.Is(err, platformsession.ErrSessionExpired), errors.Is(err, platformsession.ErrSessionRevoked):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				// AbortWithStatusJSON, NOT plain JSON — this branch means
				// platformsession.Refresh failed after revoking the old refresh
				// token in this SAME tx (e.g. Issue inside it errored); a plain
				// c.JSON would commit that revocation and leave the device with
				// no successor token. Aborting rolls the whole rotation back so
				// the caller can retry with the still-valid old token. The 401
				// branches above MUST stay plain c.JSON: their revocations
				// (reuse-detection device-chain revoke, expired-token revoke)
				// are security effects meant to persist.
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}
		setPlatformRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"token": issued.AccessToken}, "meta": gin.H{}})
	}
}

// PlatformAdminLogoutHandler godoc
// @Summary Platform admin logout
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /platform/logout [post]
func PlatformAdminLogoutHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(platformRefreshCookieName)
		if err == nil && rawToken != "" {
			q := sqlcgen.New(TxFromContext(c))
			_ = platformsession.RevokeByRawToken(c.Request.Context(), q, rawToken)
		}
		clearPlatformRefreshCookie(c, cfg)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}

// ListPlatformCompaniesHandler godoc
// @Summary List companies across ALL tenants
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /platform/companies [get]
func ListPlatformCompaniesHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformCompanyView) {
		return
	}
	limit, offset := paginationParams(c)
	tx := TxFromContext(c)
	rows, err := tx.Query(c.Request.Context(), "SELECT * FROM platform.list_companies($1, $2)", limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	companies := []sqlcgen.CoreCompany{}
	for rows.Next() {
		var comp sqlcgen.CoreCompany
		if err := rows.Scan(
			&comp.ID, &comp.Code, &comp.Name, &comp.IsActive,
			&comp.CreatedAt, &comp.CreatedBy, &comp.UpdatedAt, &comp.UpdatedBy,
			&comp.DeletedAt, &comp.DeletedBy, &comp.RowVersion, &comp.MaxConcurrentSessions,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		companies = append(companies, comp)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var total int64
	if err := tx.QueryRow(c.Request.Context(), "SELECT platform.count_companies()").Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": companies, "meta": gin.H{"limit": limit, "offset": offset, "total": total}})
}

// platformCompanyDetail is get_company_detail's flat row shape (migration
// 000039) — deliberately flat columns, not a nested core.company composite
// (spec §5: simpler to Scan by hand than decoding a composite row type).
type platformCompanyDetail struct {
	ID            pgtype.UUID        `json:"id"`
	Code          string             `json:"code"`
	Name          string             `json:"name"`
	IsActive      bool               `json:"is_active"`
	CreatedAt     pgtype.Timestamptz `json:"created_at"`
	UpdatedAt     pgtype.Timestamptz `json:"updated_at"`
	MerchantCount int64              `json:"merchant_count"`
	UserCount     int64              `json:"user_count"`
}

// GetPlatformCompanyDetailHandler godoc
// @Summary Get a company's detail across tenants, with merchant/user counts
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /platform/companies/{id} [get]
func GetPlatformCompanyDetailHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformCompanyView) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	tx := TxFromContext(c)
	var detail platformCompanyDetail
	err := tx.QueryRow(c.Request.Context(), "SELECT * FROM platform.get_company_detail($1)", id).Scan(
		&detail.ID, &detail.Code, &detail.Name, &detail.IsActive,
		&detail.CreatedAt, &detail.UpdatedAt, &detail.MerchantCount, &detail.UserCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail, "meta": gin.H{}})
}

// ListPlatformRolesHandler godoc
// @Summary List platform-admin roles
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /platform/roles [get]
func ListPlatformRolesHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformAdminManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	roles, err := q.ListPlatformRoles(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": roles, "meta": gin.H{}})
}

type createPlatformAdminUserRequest struct {
	Username string `json:"username" binding:"required"`
	Email    string `json:"email" binding:"required"`
	FullName string `json:"full_name" binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
	RoleID   string `json:"role_id" binding:"required"`
}

// CreatePlatformAdminUserHandler godoc
// @Summary Create a new platform admin
// @Tags platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPlatformAdminUserRequest true "New admin data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /platform/admin-users [post]
func CreatePlatformAdminUserHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePlatformPermission(c, PermPlatformAdminManage) {
			return
		}
		var req createPlatformAdminUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		roleID, ok := parseUUID(req.RoleID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role_id"})
			return
		}
		passwordHash, err := hasher.Hash(c.Request.Context(), req.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		adminUser, err := q.CreatePlatformAdminUser(c.Request.Context(), sqlcgen.CreatePlatformAdminUserParams{
			Username: req.Username, Email: req.Email, FullName: req.FullName,
			PasswordHash: passwordHash, CreatedBy: PlatformAdminUserID(c),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := q.CreatePlatformAdminUserRole(c.Request.Context(), sqlcgen.CreatePlatformAdminUserRoleParams{
			AdminUserID: adminUser.ID, RoleID: roleID,
		}); err != nil {
			// AbortWithStatusJSON, NOT plain JSON — PlatformTxMiddleware (Task 6)
			// only rolls back when c.IsAborted() (or c.Errors is non-empty); a
			// plain c.JSON here would let the tx commit anyway despite the 500,
			// since CreatePlatformAdminUser (above) already succeeded in the SAME
			// tx — silently leaving an admin_user row with no role/permissions at
			// all (e.g. if role_id parses as a UUID but doesn't exist in
			// platform.role, an FK violation here).
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": adminUser, "meta": gin.H{}})
	}
}

// SuspendPlatformCompanyHandler godoc
// @Summary Suspend a company — revokes all its active sessions
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /platform/companies/{id}/suspend [post]
func SuspendPlatformCompanyHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	tx := TxFromContext(c)
	var found bool
	if err := tx.QueryRow(c.Request.Context(), "SELECT platform.suspend_company($1, $2)", id, PlatformAdminUserID(c)).Scan(&found); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}

// ActivatePlatformCompanyHandler godoc
// @Summary Reactivate a suspended company
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /platform/companies/{id}/activate [post]
func ActivatePlatformCompanyHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	tx := TxFromContext(c)
	var found bool
	if err := tx.QueryRow(c.Request.Context(), "SELECT platform.activate_company($1, $2)", id, PlatformAdminUserID(c)).Scan(&found); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}
