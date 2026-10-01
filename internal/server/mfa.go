// internal/server/mfa.go
// TOTP 2FA enrollment/disable — see 2026-09-15-totp-2fa-design.md.
package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mfa"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
	"github.com/yogisaka/nexqia-api/internal/session"
)

const mfaTOTPIssuer = "Nexqia"

// RegisterMFARoutes wires TOTP enrollment/disable (spec §5, §8). Runs behind
// AuthMiddleware (see server.go) — every handler here acts on the caller's own
// account only, there is no cross-user MFA management endpoint.
func RegisterMFARoutes(rg *gin.RouterGroup, enc *mfa.Encryptor, hasher *auth.PasswordHasher) {
	rg.GET("/auth/mfa", MFAStatusHandler())
	rg.POST("/auth/mfa/setup", MFASetupHandler())
	rg.POST("/auth/mfa/confirm", MFAConfirmHandler(enc, hasher))
	rg.POST("/auth/mfa/disable", MFADisableHandler(hasher))
}

// MFASetupHandler generates a new TOTP secret + otpauth:// URI for QR rendering
// (spec §5). Deliberately writes nothing to the database — the secret only
// persists once MFAConfirmHandler verifies the user actually enrolled it.
// MFASetupHandler godoc
// @Summary Generate a new TOTP secret
// @Description Writes nothing to the database — the secret only persists once MFAConfirmHandler verifies it.
// @Tags mfa
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/mfa/setup [post]
func MFASetupHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), AuthUserID(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		secret, otpauthURI, err := mfa.GenerateSecret(mfaTOTPIssuer, user.Username)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate MFA secret"})
			return
		}
		qr, err := mfa.QRDataURI(otpauthURI)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate MFA secret"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"secret":      secret,
			"otpauth_uri": otpauthURI,
			"qr_png":      qr,
		}, "meta": gin.H{}})
	}
}

type mfaConfirmRequest struct {
	Secret   string `json:"secret" binding:"required"`
	Code     string `json:"code" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// MFAConfirmHandler verifies the code the user entered against the secret they
// were shown by MFASetupHandler, and only then persists it (spec §5). Success
// also (re-)generates recovery codes (spec §7), returned once in the response.
// Requires password re-confirmation (same as MFADisableHandler) — a stolen
// short-lived access token alone must not be able to silently enroll TOTP.
// MFAConfirmHandler godoc
// @Summary Confirm TOTP enrollment
// @Description Verifies the code against the secret from MFASetupHandler, persists it, and returns one-time recovery codes. Requires password re-confirmation.
// @Tags mfa
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body mfaConfirmRequest true "Secret, code, password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/mfa/confirm [post]
func MFAConfirmHandler(enc *mfa.Encryptor, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		var req mfaConfirmRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), user.PasswordHash, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify password"})
			return
		}
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
		if !mfa.Validate(req.Code, req.Secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
			return
		}
		// Re-enrollment invalidates any recovery codes tied to the previous secret
		// (spec §7) — persistTenantTOTP always starts from a clean slate.
		codes := persistTenantTOTP(c, q, enc, hasher, userID, AuthCompanyID(c), req.Secret)
		if codes == nil {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"recovery_codes": codes,
		}, "meta": gin.H{}})
	}
}

type mfaDisableRequest struct {
	Password string `json:"password" binding:"required"`
}

// MFADisableHandler requires password re-confirmation (security-sensitive action,
// spec §8) and, as defense-in-depth, revokes every other active session for the
// user — in case the request came from a stolen live session rather than the
// account owner.
// MFADisableHandler godoc
// @Summary Disable TOTP
// @Description Requires password re-confirmation and revokes every other active session for the user as defense-in-depth.
// @Tags mfa
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body mfaDisableRequest true "Password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/mfa/disable [post]
func MFADisableHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		var req mfaDisableRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), user.PasswordHash, req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify password"})
			return
		}
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
		// Admin-permission holders must keep TOTP (spec §2) — refuse before any write.
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID")) // same source AppLockMiddleware uses
		required, err := mfaRequired(c, q, user, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate MFA policy"})
			return
		}
		if required {
			c.JSON(http.StatusForbidden, gin.H{"error": "mfa is required for this account"})
			return
		}
		if err := q.ClearAppUserMFASecret(c.Request.Context(), sqlcgen.ClearAppUserMFASecretParams{
			ID: userID, UpdatedBy: userID,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disable MFA"})
			return
		}
		if err := q.DeleteMFARecoveryCodesForUser(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disable MFA"})
			return
		}
		currentRawToken, _ := c.Cookie(refreshCookieName)
		if err := session.RevokeAllExceptCurrent(c.Request.Context(), q, userID, currentRawToken); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke sessions"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}

// mfaAdminPermissions: holding any of these at the active merchant makes TOTP
// mandatory regardless of the merchant's auth.require_totp flag (spec
// 2026-09-29-small-security-fixes §2). Owner holds all of them.
var mfaAdminPermissions = []string{
	PermUserManage, PermRoleManage, PermMerchantManage, PermCompanyManageOwn, PermAuditLogView,
}

// mfaRequired reports whether TOTP is mandatory for user at merchantID: the
// merchant's auth.require_totp flag, or an admin permission held ANYWHERE in the
// user's company (company role or a merchant role at any of its merchants). The
// admin check must not depend on X-Merchant-ID: that header is client-supplied,
// so a per-merchant check let an admin opt out by naming another merchant.
// checkMFANudge runs first because it also sets app.current_merchant_id for this
// transaction.
func mfaRequired(c *gin.Context, q *sqlcgen.Queries, user sqlcgen.CoreAppUser, merchantID pgtype.UUID) (bool, error) {
	if !merchantID.Valid {
		return false, nil // merchant-less session (Owner before the first merchant)
	}
	if checkMFANudge(c, q, merchantID) {
		return true, nil
	}
	return q.UserHasCompanyLevelPermission(c.Request.Context(), sqlcgen.UserHasCompanyLevelPermissionParams{
		UserID: user.ID, CompanyID: user.CompanyID, Codes: mfaAdminPermissions,
	})
}

// tenantMFAGrace evaluates the merchant's auth.require_totp policy (spec §5)
// for a user without TOTP, starting the grace window on first sight.
// Merchant-less sessions (Owner before the first merchant) are exempt.
func tenantMFAGrace(c *gin.Context, q *sqlcgen.Queries, cfg config.Config, user sqlcgen.CoreAppUser, merchantID pgtype.UUID) (mfaGraceDecision, time.Time, error) {
	if user.MfaSecret.Valid {
		return mfaGraceNotApplicable, time.Time{}, nil
	}
	required, err := mfaRequired(c, q, user, merchantID)
	if err != nil {
		return 0, time.Time{}, err
	}
	if !required {
		return mfaGraceNotApplicable, time.Time{}, nil
	}
	until := user.MfaGraceUntil
	if !until.Valid {
		ctx := c.Request.Context()
		started, err := q.StartAppUserMFAGrace(ctx, sqlcgen.StartAppUserMFAGraceParams{
			ID: user.ID, GraceDays: int32(cfg.TenantMFAGraceDays),
		})
		if errors.Is(err, pgx.ErrNoRows) { // a concurrent login started it first
			fresh, getErr := q.GetAppUserByID(ctx, user.ID)
			if getErr != nil {
				return 0, time.Time{}, getErr
			}
			started, err = fresh.MfaGraceUntil, nil
		}
		if err != nil {
			return 0, time.Time{}, err
		}
		until = started
	}
	return graceDecision(until.Time), until.Time, nil
}

// persistTenantTOTP is persistPlatformTOTP for core.app_user — shared by
// MFAConfirmHandler and MFAEnrollHandler. Fixes the old confirm path, which
// used plain c.JSON after SetAppUserMFASecret and so committed a secret
// without recovery codes on failure.
func persistTenantTOTP(c *gin.Context, q *sqlcgen.Queries, enc *mfa.Encryptor, hasher *auth.PasswordHasher, userID, companyID pgtype.UUID, secret string) []string {
	ctx := c.Request.Context()
	encrypted, err := enc.Encrypt(secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MFA secret"})
		return nil
	}
	codes, err := mfa.GenerateRecoveryCodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate recovery codes"})
		return nil
	}
	hashes := make([]string, len(codes))
	for i, code := range codes {
		if hashes[i], err = hasher.Hash(ctx, code); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
			return nil
		}
	}
	if err := q.SetAppUserMFASecret(ctx, sqlcgen.SetAppUserMFASecretParams{
		ID: userID, MfaSecret: pgtype.Text{String: encrypted, Valid: true}, UpdatedBy: userID,
	}); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to save MFA secret"})
		return nil
	}
	if err := q.DeleteMFARecoveryCodesForUser(ctx, userID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
		return nil
	}
	for _, h := range hashes {
		if err := q.CreateMFARecoveryCode(ctx, sqlcgen.CreateMFARecoveryCodeParams{
			UserID: userID, CompanyID: companyID, CodeHash: h,
		}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
			return nil
		}
	}
	return codes
}

// checkTenantMFAEnrollRateLimit mirrors checkPlatformMFAEnrollRateLimit —
// enrollment tokens are short-lived, so brute-forcing the TOTP code against
// one user must be bounded per user, not per IP.
func checkTenantMFAEnrollRateLimit(c *gin.Context, limiter *ratelimit.Limiter, cfg config.Config, userID string) bool {
	result, err := limiter.AllowSlidingWindow(c.Request.Context(), "mfa_enroll:"+userID, cfg.RateLimitLoginMaxAttempts, cfg.RateLimitLoginWindowSeconds)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
		return false
	}
	if !result.Allowed {
		c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts, try again later"})
		return false
	}
	return true
}

type mfaEnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token" binding:"required"`
	Secret          string `json:"secret" binding:"required"`
	Code            string `json:"code" binding:"required"`
}

// MFAEnrollHandler godoc
// @Summary Finish tenant TOTP enrollment with a login-time enrollment token
// @Description Public. Completes the mandatory setup started by a login that
// @Description returned mfa_setup_required: verifies the enrollment token and
// @Description TOTP code, stores the secret, and returns one-time recovery codes.
// @Tags mfa
// @Accept json
// @Produce json
// @Param X-Company-ID header string true "Company UUID"
// @Param request body mfaEnrollRequest true "Enrollment token, secret, code"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /auth/mfa/enroll [post]
func MFAEnrollHandler(cfg config.Config, limiter *ratelimit.Limiter, enc *mfa.Encryptor, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req mfaEnrollRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID, err := auth.ParseMFAEnrollmentToken(cfg.JWTSecret, req.EnrollmentToken, auth.MFAEnrollmentPurpose)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		if !checkTenantMFAEnrollRateLimit(c, limiter, cfg, userID) {
			return
		}
		userUUID, ok := parseUUID(userID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		if !user.IsActive {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		if user.MfaSecret.Valid {
			c.JSON(http.StatusConflict, gin.H{"error": "mfa already enabled"})
			return
		}
		if !mfa.Validate(req.Code, req.Secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
			return
		}
		codes := persistTenantTOTP(c, q, enc, hasher, userUUID, user.CompanyID, req.Secret)
		if codes == nil {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"recovery_codes": codes}, "meta": gin.H{}})
	}
}

// MFAStatusHandler godoc
// @Summary Report the caller's TOTP enrollment status
// @Description Read-only snapshot for the frontend: whether TOTP is enabled on
// @Description the account, whether the active merchant's auth.require_totp
// @Description policy demands it, and when the caller's grace window ends.
// @Tags mfa
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Router /auth/mfa [get]
func MFAStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), AuthUserID(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID")) // same source AppLockMiddleware uses; TenantMiddleware already rejected a missing header
		required, err := mfaRequired(c, q, user, merchantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate MFA policy"})
			return
		}
		var graceUntil any
		if user.MfaGraceUntil.Valid {
			graceUntil = user.MfaGraceUntil.Time.Format(time.RFC3339)
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"enabled":     user.MfaSecret.Valid,
			"required":    required,
			"grace_until": graceUntil,
		}, "meta": gin.H{}})
	}
}
