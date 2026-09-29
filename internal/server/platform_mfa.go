// internal/server/platform_mfa.go
// Platform admin TOTP MFA — grace period, enrollment, setup/confirm, reset.
// See docs/design/specs/2026-09-25-mfa-grace-enforcement-design.md.
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
)

const (
	platformMFATOTPIssuer = "Nexqia Platform"
	// Protocol timeout for finishing enrollment after a password check — not a
	// tunable (same reasoning as merchantSelectionTokenTTL).
	mfaEnrollmentTokenTTL = 10 * time.Minute
)

type mfaGraceDecision int

const (
	mfaGraceNotApplicable mfaGraceDecision = iota // has TOTP, or policy off
	mfaGraceActive                                // may log in, remind until graceUntil
	mfaGraceExpired                               // must enroll before a session is issued
)

// graceDecision turns a started grace window into a decision (spec §3).
func graceDecision(graceUntil time.Time) mfaGraceDecision {
	if time.Now().Before(graceUntil) {
		return mfaGraceActive
	}
	return mfaGraceExpired
}

// mfaSetupRequiredPayload is the no-session login response once grace is over
// (spec §3 step 3): a short-lived enrollment token plus a fresh secret/QR.
func mfaSetupRequiredPayload(jwtSecret, subjectID, purpose, issuer, accountName string) (gin.H, error) {
	token, err := auth.GenerateMFAEnrollmentToken(jwtSecret, subjectID, purpose, mfaEnrollmentTokenTTL)
	if err != nil {
		return nil, err
	}
	secret, uri, err := mfa.GenerateSecret(issuer, accountName)
	if err != nil {
		return nil, err
	}
	qr, err := mfa.QRDataURI(uri)
	if err != nil {
		return nil, err
	}
	return gin.H{
		"mfa_setup_required": true,
		"enrollment_token":   token,
		"secret":             secret,
		"otpauth_uri":        uri,
		"qr_png":             qr,
	}, nil
}

// platformMFAGrace evaluates the always-on platform policy for an admin
// without TOTP, starting the grace window on first sight.
func platformMFAGrace(c *gin.Context, q *sqlcgen.Queries, cfg config.Config, admin sqlcgen.PlatformAdminUser) (mfaGraceDecision, time.Time, error) {
	if admin.MfaSecret.Valid {
		return mfaGraceNotApplicable, time.Time{}, nil
	}
	until := admin.MfaGraceUntil
	if !until.Valid {
		ctx := c.Request.Context()
		started, err := q.StartPlatformAdminMFAGrace(ctx, sqlcgen.StartPlatformAdminMFAGraceParams{
			ID: admin.ID, GraceDays: int32(cfg.PlatformMFAGraceDays),
		})
		if errors.Is(err, pgx.ErrNoRows) { // a concurrent login started it first
			fresh, getErr := q.GetPlatformAdminUserByID(ctx, admin.ID)
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

// verifyPlatformLoginTOTP is verifyLoginTOTP (internal/server/auth.go) for
// platform admins — checks the code against the decrypted TOTP secret, then
// falls back to the admin's one-time recovery codes (matched codes are marked
// used so they can't be replayed).
func verifyPlatformLoginTOTP(c *gin.Context, q *sqlcgen.Queries, enc *mfa.Encryptor, hasher *auth.PasswordHasher, adminID pgtype.UUID, encryptedSecret, code string) bool {
	ctx := c.Request.Context()
	secret, err := enc.Decrypt(encryptedSecret)
	if err == nil && mfa.Validate(code, secret) {
		return true
	}
	recoveryCodes, err := q.ListActivePlatformAdminMFARecoveryCodes(ctx, adminID)
	if err != nil {
		return false
	}
	for _, rc := range recoveryCodes {
		ok, err := hasher.Verify(ctx, rc.CodeHash, code)
		if err == nil && ok {
			_ = q.MarkPlatformAdminMFARecoveryCodeUsed(ctx, rc.ID)
			return true
		}
	}
	return false
}

// persistPlatformTOTP stores an already-verified secret and replaces the
// admin's recovery codes. All hashing happens before the first write; every
// failure after it aborts so the tx rolls back (global constraint). Returns
// nil after writing the error response.
func persistPlatformTOTP(c *gin.Context, q *sqlcgen.Queries, enc *mfa.Encryptor, hasher *auth.PasswordHasher, adminID pgtype.UUID, secret string) []string {
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
	if err := q.SetPlatformAdminMFASecret(ctx, sqlcgen.SetPlatformAdminMFASecretParams{
		ID: adminID, MfaSecret: pgtype.Text{String: encrypted, Valid: true}, UpdatedBy: adminID,
	}); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to save MFA secret"})
		return nil
	}
	if err := q.DeletePlatformAdminMFARecoveryCodes(ctx, adminID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
		return nil
	}
	for _, h := range hashes {
		if err := q.CreatePlatformAdminMFARecoveryCode(ctx, sqlcgen.CreatePlatformAdminMFARecoveryCodeParams{
			AdminUserID: adminID, CodeHash: h,
		}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
			return nil
		}
	}
	return codes
}

// checkPlatformMFAEnrollRateLimit mirrors checkPlatformLoginRateLimit
// (internal/server/platform_auth.go)'s 503/429 handling, keyed per admin —
// enrollment tokens are short-lived, so brute-forcing the TOTP code against
// one admin must be bounded per admin, not per IP.
func checkPlatformMFAEnrollRateLimit(c *gin.Context, limiter *ratelimit.Limiter, cfg config.Config, adminID string) bool {
	result, err := limiter.AllowSlidingWindow(c.Request.Context(), "platform_mfa_enroll:"+adminID, cfg.RateLimitLoginMaxAttempts, cfg.RateLimitLoginWindowSeconds)
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

type platformMFAEnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token" binding:"required"`
	Secret          string `json:"secret" binding:"required"`
	Code            string `json:"code" binding:"required"`
}

// PlatformMFAEnrollHandler godoc
// @Summary Finish platform admin TOTP enrollment with a login-time enrollment token
// @Description Public. Completes the mandatory setup started by a login that
// @Description returned mfa_setup_required: verifies the enrollment token and
// @Description TOTP code, stores the secret, and returns one-time recovery codes.
// @Tags platform
// @Accept json
// @Produce json
// @Param request body platformMFAEnrollRequest true "Enrollment token, secret, code"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /platform/mfa/enroll [post]
func PlatformMFAEnrollHandler(limiter *ratelimit.Limiter, cfg config.Config, enc *mfa.Encryptor, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req platformMFAEnrollRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		adminID, err := auth.ParseMFAEnrollmentToken(cfg.JWTSecret, req.EnrollmentToken, auth.PlatformMFAEnrollmentPurpose)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		if !checkPlatformMFAEnrollRateLimit(c, limiter, cfg, adminID) {
			return
		}
		adminUUID, ok := parseUUID(adminID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		admin, err := q.GetPlatformAdminUserByID(c.Request.Context(), adminUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired enrollment token"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load admin user"})
			return
		}
		if admin.MfaSecret.Valid {
			c.JSON(http.StatusConflict, gin.H{"error": "mfa already enabled"})
			return
		}
		if !mfa.Validate(req.Code, req.Secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
			return
		}
		codes := persistPlatformTOTP(c, q, enc, hasher, adminUUID, req.Secret)
		if codes == nil {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"recovery_codes": codes}, "meta": gin.H{}})
	}
}

// PlatformMFASetupHandler godoc
// @Summary Generate a new platform admin TOTP secret
// @Description Writes nothing to the database — the secret only persists once
// @Description PlatformMFAConfirmHandler verifies it.
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /platform/mfa/setup [post]
func PlatformMFASetupHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		q := sqlcgen.New(TxFromContext(c))
		admin, err := q.GetPlatformAdminUserByID(c.Request.Context(), PlatformAdminUserID(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load admin user"})
			return
		}
		secret, otpauthURI, err := mfa.GenerateSecret(platformMFATOTPIssuer, admin.Username)
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

type platformMFAConfirmRequest struct {
	Secret   string `json:"secret" binding:"required"`
	Code     string `json:"code" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// PlatformMFAConfirmHandler godoc
// @Summary Confirm platform admin TOTP enrollment
// @Description Verifies the code against the secret from PlatformMFASetupHandler,
// @Description requires password re-confirmation, persists the secret, and
// @Description returns one-time recovery codes. Re-enrollment goes through an
// @Description MFA reset by another admin instead.
// @Tags platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body platformMFAConfirmRequest true "Secret, code, password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /platform/mfa/confirm [post]
func PlatformMFAConfirmHandler(enc *mfa.Encryptor, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req platformMFAConfirmRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		adminID := PlatformAdminUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		admin, err := q.GetPlatformAdminUserByID(c.Request.Context(), adminID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load admin user"})
			return
		}
		valid, err := hasher.Verify(c.Request.Context(), admin.PasswordHash, req.Password)
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
		if admin.MfaSecret.Valid {
			// Re-enroll goes through another admin's MFA reset — overwriting an
			// existing secret from confirm would silently change the account's
			// second factor.
			c.JSON(http.StatusConflict, gin.H{"error": "mfa already enabled"})
			return
		}
		if !mfa.Validate(req.Code, req.Secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
			return
		}
		codes := persistPlatformTOTP(c, q, enc, hasher, adminID, req.Secret)
		if codes == nil {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"recovery_codes": codes}, "meta": gin.H{}})
	}
}

// ResetPlatformAdminMFAHandler godoc
// @Summary Reset another platform admin's MFA
// @Description Clears the target's TOTP secret and recovery codes — the target
// @Description must enroll again at next login and loses every active session.
// @Tags platform
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admin user UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /platform/admin-users/{id}/mfa/reset [post]
func ResetPlatformAdminMFAHandler(c *gin.Context) {
	if !RequirePlatformPermission(c, PermPlatformAdminManage) {
		return
	}
	targetID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	callerID := PlatformAdminUserID(c)
	if targetID == callerID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot reset your own MFA"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	n, err := q.ResetPlatformAdminMFA(ctx, sqlcgen.ResetPlatformAdminMFAParams{ID: targetID, UpdatedBy: callerID})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reset MFA"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "admin user not found"})
		return
	}
	if err := q.DeletePlatformAdminMFARecoveryCodes(ctx, targetID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to reset MFA"})
		return
	}
	if err := q.RevokeAllPlatformAdminRefreshTokensForUser(ctx, targetID); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to reset MFA"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
}
