// internal/server/mfa.go
// TOTP 2FA enrollment/disable — see docs/design/specs/2026-09-15-totp-2fa-design.md.
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mfa"
	"github.com/yogisaka/nexqia-api/internal/session"
)

const mfaTOTPIssuer = "Nexqia"

// RegisterMFARoutes wires TOTP enrollment/disable (spec §5, §8). Runs behind
// AuthMiddleware (see server.go) — every handler here acts on the caller's own
// account only, there is no cross-user MFA management endpoint.
func RegisterMFARoutes(rg *gin.RouterGroup, enc *mfa.Encryptor, hasher *auth.PasswordHasher) {
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
// @Router /auth/mfa/setup [post]
func MFASetupHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), AuthUserID(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		secret, otpauthURI, err := mfa.GenerateSecret(mfaTOTPIssuer, user.Username)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate MFA secret"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"secret":      secret,
			"otpauth_uri": otpauthURI,
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
// @Router /auth/mfa/confirm [post]
func MFAConfirmHandler(enc *mfa.Encryptor, hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req mfaConfirmRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
		if !mfa.Validate(req.Code, req.Secret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
			return
		}
		encryptedSecret, err := enc.Encrypt(req.Secret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MFA secret"})
			return
		}
		if err := q.SetAppUserMFASecret(c.Request.Context(), sqlcgen.SetAppUserMFASecretParams{
			ID: userID, MfaSecret: pgtype.Text{String: encryptedSecret, Valid: true}, UpdatedBy: userID,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Re-enrollment invalidates any recovery codes tied to the previous secret
		// (spec §7) — always start from a clean slate.
		if err := q.DeleteMFARecoveryCodesForUser(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		codes, err := mfa.GenerateRecoveryCodes()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate recovery codes"})
			return
		}
		companyID := AuthCompanyID(c)
		for _, code := range codes {
			hash, err := hasher.Hash(c.Request.Context(), code)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store recovery codes"})
				return
			}
			if err := q.CreateMFARecoveryCode(c.Request.Context(), sqlcgen.CreateMFARecoveryCodeParams{
				UserID: userID, CompanyID: companyID, CodeHash: hash,
			}); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
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
// @Router /auth/mfa/disable [post]
func MFADisableHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
		if err := q.ClearAppUserMFASecret(c.Request.Context(), sqlcgen.ClearAppUserMFASecretParams{
			ID: userID, UpdatedBy: userID,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := q.DeleteMFARecoveryCodesForUser(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		currentRawToken, _ := c.Cookie(refreshCookieName)
		if err := session.RevokeAllExceptCurrent(c.Request.Context(), q, userID, currentRawToken); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}
