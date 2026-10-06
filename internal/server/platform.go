// NEXQIA Platform Admin — foundation (0d-1). Genuinely separate auth/access
// system from tenant staff, see 2026-09-24-platform-admin-foundation-design.md.
package server

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mfa"
	"github.com/yogisaka/nexqia-api/internal/platformsession"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

func RegisterPlatformRoutes(rg *gin.RouterGroup, pool *pgxpool.Pool, limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher, enc *mfa.Encryptor) {
	rg.POST("/login", PlatformAdminLoginHandler(limiter, cfg, hasher, enc))
	rg.POST("/refresh", PlatformAdminRefreshHandler(cfg))
	rg.POST("/mfa/enroll", PlatformMFAEnrollHandler(limiter, cfg, enc, hasher))

	protected := rg.Group("", PlatformAdminAuthMiddleware(cfg.JWTSecret))
	protected.POST("/logout", PlatformAdminLogoutHandler(cfg))
	protected.GET("/companies", ListPlatformCompaniesHandler)
	protected.GET("/companies/:id", GetPlatformCompanyDetailHandler)
	protected.POST("/companies", CreatePlatformCompanyHandler(pool, hasher))
	protected.POST("/companies/:id/suspend", SuspendPlatformCompanyHandler)
	protected.POST("/companies/:id/activate", ActivatePlatformCompanyHandler)
	protected.GET("/roles", ListPlatformRolesHandler)
	protected.POST("/admin-users", CreatePlatformAdminUserHandler(hasher))
	protected.POST("/admin-users/:id/mfa/reset", ResetPlatformAdminMFAHandler)
	protected.POST("/impersonate", ImpersonateHandler(cfg))
	protected.POST("/mfa/setup", PlatformMFASetupHandler())
	protected.POST("/mfa/confirm", PlatformMFAConfirmHandler(enc, hasher))
}

type platformAdminLoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	TotpCode string `json:"totp_code"`
}

// PlatformAdminLoginHandler godoc
// @Summary Platform admin login
// @Description Returns totp_required when the admin has TOTP enabled and no/invalid
// @Description code was given, or mfa_setup_required once the enrollment grace
// @Description period has ended (no session is issued in that case).
// @Tags platform
// @Accept json
// @Produce json
// @Param request body platformAdminLoginRequest true "Credentials"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /platform/login [post]
func PlatformAdminLoginHandler(limiter *ratelimit.Limiter, cfg config.Config, hasher *auth.PasswordHasher, enc *mfa.Encryptor) gin.HandlerFunc {
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
			respondInternalError(c, err)
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
			respondInternalError(c, err)
			return
		}
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		// TOTP check happens once here (spec §4 row 1) — password is already
		// confirmed above, so it's safe to reveal 2FA-enrollment status now
		// (same reasoning as LoginHandler's tenant check).
		if adminUser.MfaSecret.Valid {
			if req.TotpCode == "" {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{"totp_required": true}, "meta": gin.H{}})
				return
			}
			if !verifyPlatformLoginTOTP(c, q, enc, hasher, adminUser.ID, adminUser.MfaSecret.String, req.TotpCode) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
				return
			}
		}
		decision, graceUntil, err := platformMFAGrace(c, q, cfg, adminUser)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate MFA policy"})
			return
		}
		if decision == mfaGraceExpired {
			// No session (and no refresh cookie) is issued until enrollment is
			// finished — and this response comes deliberately BEFORE
			// TouchPlatformAdminUserLastLogin, so last_login_at stays NULL for
			// the setup-only response (spec §3 step 3).
			payload, err := mfaSetupRequiredPayload(cfg.JWTSecret, adminUser.ID.String(), auth.PlatformMFAEnrollmentPurpose, platformMFATOTPIssuer, adminUser.Username)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare MFA enrollment"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
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
			abortInternalError(c, err)
			return
		}
		setPlatformRefreshCookie(c, cfg, issued.RefreshToken, issued.RefreshExpiresAt)
		data := gin.H{
			"token": issued.AccessToken,
			"admin_user": gin.H{
				"id": adminUser.ID, "username": adminUser.Username, "full_name": adminUser.FullName,
			},
		}
		if decision == mfaGraceActive {
			data["mfa_setup_due_at"] = graceUntil.Format(time.RFC3339)
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
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
				KeepTxOnError(c)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired or revoked, please log in again"})
			default:
				// abortInternalError rolls the whole rotation back (the old
				// refresh token was already revoked in this SAME tx), so the
				// caller can retry with the still-valid old token. The 401
				// branch above calls KeepTxOnError: its revocations
				// (reuse-detection device-chain revoke, expired-token revoke)
				// are security effects meant to persist (spec
				// 2026-10-06-request-tx-finalization §4 #3).
				abortInternalError(c, err)
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
		respondInternalError(c, err)
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
			respondInternalError(c, err)
			return
		}
		companies = append(companies, comp)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(c, err)
		return
	}
	var total int64
	if err := tx.QueryRow(c.Request.Context(), "SELECT platform.count_companies()").Scan(&total); err != nil {
		respondInternalError(c, err)
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
		respondInternalError(c, err)
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
		respondInternalError(c, err)
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
			respondInternalError(c, err)
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		adminUser, err := q.CreatePlatformAdminUser(c.Request.Context(), sqlcgen.CreatePlatformAdminUserParams{
			Username: req.Username, Email: req.Email, FullName: req.FullName,
			PasswordHash: passwordHash, CreatedBy: PlatformAdminUserID(c),
		})
		if err != nil {
			respondInternalError(c, err)
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
			abortInternalError(c, err)
			return
		}
		// Explicit whitelist, NOT the raw struct — sqlcgen.PlatformAdminUser
		// marshals password_hash, leaking it in the response body.
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"id": adminUser.ID.String(), "username": adminUser.Username,
			"email": adminUser.Email, "full_name": adminUser.FullName,
		}, "meta": gin.H{}})
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
		abortInternalError(c, err)
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
		abortInternalError(c, err)
		return
	}
	if !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}

type createPlatformCompanyRequest struct {
	CompanyName string `json:"company_name" binding:"required,min=3"`
	FullName    string `json:"full_name" binding:"required"`
	Username    string `json:"username" binding:"required"`
	Email       string `json:"email" binding:"required"`
	Phone       string `json:"phone" binding:"required"`
	Password    string `json:"password" binding:"required,min=8"`
}

func (r createPlatformCompanyRequest) validate() string {
	switch {
	case !registerUsernameFormat.MatchString(r.Username):
		return "username must contain only lowercase letters, digits, and dots"
	case !registerEmailFormat.MatchString(r.Email):
		return "invalid email format"
	case !registerPhoneFormat.MatchString(r.Phone):
		return "invalid phone number format"
	default:
		return ""
	}
}

// CreatePlatformCompanyHandler godoc
// @Summary Create a company and bootstrap its Owner — platform-admin-initiated
// @Description No session is issued for the new Owner — they log in themselves
// @Description afterward via the normal /auth/login. See
// @Description 2026-09-24-platform-admin-create-company-design.md.
// @Tags platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPlatformCompanyRequest true "New company + Owner data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /platform/companies [post]
func CreatePlatformCompanyHandler(pool *pgxpool.Pool, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePlatformPermission(c, PermPlatformCompanyManage) {
			return
		}
		var req createPlatformCompanyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if msg := req.validate(); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		adminID := PlatformAdminUserID(c)

		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}
		defer tx.Rollback(ctx)
		q := sqlcgen.New(tx)

		// No email/phone pre-check here — same reasoning as RegisterHandler
		// (0a, commit 09c057c): the pre-check queries were RLS-protected plain
		// SELECTs run BEFORE set_config (crash SQLSTATE 42704) and useless under
		// RLS anyway; the UNIQUE constraints + isUniqueViolation at CreateAppUser
		// below enforce duplicates correctly, and defer tx.Rollback undoes any
		// earlier writes in this local tx on rejection.
		var companyID pgtype.UUID
		if err := tx.QueryRow(ctx, "SELECT uuid_generate_v7()").Scan(&companyID); err != nil {
			respondInternalError(c, err)
			return
		}

		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID.String()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}
		// Audit trail: this handler's own tx never sees the middleware GUC, so the
		// platform admin attribution for the owner person created below is set here.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_admin_id', $1, true)", PlatformAdminUserID(c).String()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}
		company, err := InsertWithUniqueCode(ctx, tx, generateCode, func(q *sqlcgen.Queries, code string) (sqlcgen.CoreCompany, error) {
			return q.CreateCompanyWithID(ctx, sqlcgen.CreateCompanyWithIDParams{
				ID: companyID, Code: code, Name: req.CompanyName, CreatedBy: adminID,
			})
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		// Gender sengaja gak diisi (zero-value pgtype.Text{} = NULL) — request
		// admin platform gak ngumpulin data ini, core.person.gender nullable
		// sejak migration 000038 (pola sama kayak RegisterHandler 0a).
		person, err := q.CreatePerson(ctx, sqlcgen.CreatePersonParams{
			CompanyID: companyID,
			FullName:  req.FullName,
			CreatedBy: adminID,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		hash, err := hasher.Hash(ctx, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}

		user, err := q.CreateAppUser(ctx, sqlcgen.CreateAppUserParams{
			CompanyID: companyID, PersonID: person.ID, Username: req.Username,
			Email:        pgtype.Text{String: req.Email, Valid: true},
			Phone:        pgtype.Text{String: req.Phone, Valid: true},
			PasswordHash: hash, CreatedBy: adminID,
		})
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "email or phone number already registered"})
				return
			}
			respondInternalError(c, err)
			return
		}

		role, err := q.CreateRole(ctx, sqlcgen.CreateRoleParams{
			CompanyID: companyID, Name: "Owner", Description: pgtype.Text{String: "Company owner — full access", Valid: true}, IsSystem: true, CreatedBy: adminID,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		permissions, err := q.ListPermissions(ctx)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for _, p := range permissions {
			if p.Code == PermCompanyManage {
				continue // core.company.manage — platform-wide, never in the Owner bundle
			}
			if err := q.AddRolePermission(ctx, sqlcgen.AddRolePermissionParams{RoleID: role.ID, PermissionID: p.ID}); err != nil {
				respondInternalError(c, err)
				return
			}
		}

		if _, err := q.CreateUserCompanyRole(ctx, sqlcgen.CreateUserCompanyRoleParams{
			UserID: user.ID, CompanyID: companyID, RoleID: role.ID, CreatedBy: adminID,
		}); err != nil {
			respondInternalError(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"company_id":   companyID.String(),
			"company_code": company.Code,
			"company_name": company.Name,
			"user_id":      user.ID.String(),
			"username":     user.Username,
			"full_name":    req.FullName,
		}, "meta": gin.H{}})
	}
}

type impersonateRequest struct {
	CompanyID string `json:"company_id" binding:"required"`
	Username  string `json:"username" binding:"required"`
	Reason    string `json:"reason" binding:"required,min=10"`
}

// platformImpersonateCompany / platformImpersonateUser are the flat rows of
// platform.get_company and platform.get_app_user_by_username (migration
// 000041) — the RLS-bypass DEFINER reads backing ImpersonateHandler, same
// flat-Scan pattern as 000039's get_company_detail.
type platformImpersonateCompany struct {
	ID       pgtype.UUID `json:"id"`
	Name     string      `json:"name"`
	Code     string      `json:"code"`
	IsActive bool        `json:"is_active"`
}

type platformImpersonateUser struct {
	ID       pgtype.UUID `json:"id"`
	PersonID pgtype.UUID `json:"person_id"`
	Username string      `json:"username"`
	IsActive bool        `json:"is_active"`
}

const impersonationTokenTTL = time.Hour

// ImpersonateHandler godoc
// @Summary Get a short-lived tenant access token for a specific user
// @Description No refresh token is issued — the access token expires after
// @Description 1 hour with no way to renew; call this endpoint again for a
// @Description fresh, separately-audited session. See
// @Description 2026-09-24-platform-admin-impersonate-design.md.
// @Tags platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body impersonateRequest true "Target user + reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /platform/impersonate [post]
func ImpersonateHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePlatformPermission(c, PermPlatformTenantUserImpersonate) {
			return
		}
		var req impersonateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		companyID, ok := parseUUID(req.CompanyID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid company_id"})
			return
		}

		q := sqlcgen.New(TxFromContext(c))

		// Read the tenant + target user through SECURITY DEFINER functions
		// (migration 000041) — core.company/core.app_user are RLS-protected on
		// app.current_company_id and PlatformTxMiddleware sets no GUC, so direct
		// SELECTs would return 0 rows in production (app_runtime is NOBYPASSRLS).
		var company platformImpersonateCompany
		err := TxFromContext(c).QueryRow(c.Request.Context(),
			"SELECT * FROM platform.get_company($1)", companyID).Scan(
			&company.ID, &company.Name, &company.Code, &company.IsActive,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}

		var targetUser platformImpersonateUser
		err = TxFromContext(c).QueryRow(c.Request.Context(),
			"SELECT * FROM platform.get_app_user_by_username($1, $2)", companyID, req.Username).Scan(
			&targetUser.ID, &targetUser.PersonID, &targetUser.Username, &targetUser.IsActive,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if !targetUser.IsActive {
			c.JSON(http.StatusForbidden, gin.H{"error": "target user is inactive"})
			return
		}

		var ip netip.Addr
		if parsed, parseErr := netip.ParseAddr(c.ClientIP()); parseErr == nil {
			ip = parsed
		} else {
			ip = netip.IPv4Unspecified()
		}

		adminID := PlatformAdminUserID(c)
		expiresAt := time.Now().Add(impersonationTokenTTL)
		if _, err := q.CreatePlatformImpersonationSession(c.Request.Context(), sqlcgen.CreatePlatformImpersonationSessionParams{
			AdminUserID: adminID, TargetUserID: targetUser.ID, TargetCompanyID: companyID,
			Reason: req.Reason, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true}, StartedIp: ip,
		}); err != nil {
			abortInternalError(c, err)
			return
		}

		token, err := auth.GenerateImpersonationToken(
			cfg.JWTSecret, targetUser.ID.String(), companyID.String(), "",
			targetUser.Username, "impersonation", adminID.String(), impersonationTokenTTL,
		)
		if err != nil {
			abortInternalError(c, err)
			return
		}

		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"token":      token,
			"expires_in": int(impersonationTokenTTL.Seconds()),
			"impersonating": gin.H{
				"user_id":      targetUser.ID.String(),
				"username":     targetUser.Username,
				"company_id":   companyID.String(),
				"company_name": company.Name,
			},
		}, "meta": gin.H{}})
	}
}
