// internal/server/account.go
// Self-service account management — profile (GET/PATCH /auth/me) and password
// change (POST /auth/password), see docs/design/specs/
// 2026-09-25-account-menu-design.md §3. All routes run on the `locked` group
// (TenantMiddleware + AuthMiddleware + AppLockMiddleware), same as MFA routes.
package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
	"github.com/yogisaka/nexqia-api/internal/session"
)

// RegisterAccountRoutes wires the self-service account endpoints (spec §3).
func RegisterAccountRoutes(rg *gin.RouterGroup, hasher *auth.PasswordHasher, limiter *ratelimit.Limiter, cfg config.Config) {
	rg.GET("/auth/me", GetMeHandler)
	rg.PATCH("/auth/me", UpdateMeHandler)
	rg.POST("/auth/password", ChangePasswordHandler(hasher, limiter, cfg))
}

// meData builds the shared GET/PATCH /auth/me response shape (spec §3).
// full_name/photo_url stay null when no person is linked. pin_lock_enabled is
// the current merchant's auth.pin_lock flag, read the same way
// AppLockMiddleware reads it so the value matches what enforcement uses.
// pgtype.Text/pgtype.UUID marshal as JSON string-or-null on their own.
func meData(c *gin.Context, q *sqlcgen.Queries, user sqlcgen.CoreAppUser) gin.H {
	data := gin.H{
		"user_id":   user.ID.String(),
		"username":  user.Username,
		"email":     user.Email,
		"phone":     user.Phone,
		"person_id": user.PersonID,
		"full_name": nil,
		"photo_url": nil,
		"has_pin":   user.PinHash.Valid,
	}
	if user.PersonID.Valid {
		if person, err := q.GetPersonByID(c.Request.Context(), user.PersonID); err == nil {
			data["full_name"] = person.FullName
			data["photo_url"] = person.PhotoUrl
		}
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	data["pin_lock_enabled"] = getPinLockFlag(c, q, merchantID).Enabled
	return data
}

// GetMeHandler returns the caller's own profile (spec §3). Runs behind
// AuthMiddleware/AppLockMiddleware.
// GetMeHandler godoc
// @Summary Get the caller's own profile
// @Tags account
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Router /auth/me [get]
func GetMeHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	user, err := q.GetAppUserByID(c.Request.Context(), AuthUserID(c))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meData(c, q, user), "meta": gin.H{}})
}

type updateMeRequest struct {
	FullName *string `json:"full_name"`
	Email    *string `json:"email"`
	Phone    *string `json:"phone"`
}

// UpdateMeHandler edits the caller's own profile (spec §3): full_name on the
// linked core.person, email/phone on core.app_user (globally UNIQUE since
// migration 000034). Pointer fields — absent = unchanged; values are trimmed;
// empty email/phone become NULL (never "", the UNIQUE constraints would make a
// second empty value collide). Write order: contact first, then full name.
// UpdateMeHandler godoc
// @Summary Update the caller's own profile
// @Description Only full_name, email, phone. Absent fields unchanged; empty email/phone clears them (NULL). Returns the same shape as GET /auth/me.
// @Tags account
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body updateMeRequest true "Profile fields"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /auth/me [patch]
func UpdateMeHandler(c *gin.Context) {
	var req updateMeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	user, err := q.GetAppUserByID(c.Request.Context(), AuthUserID(c))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var newFullName string
	if req.FullName != nil {
		newFullName = strings.TrimSpace(*req.FullName)
		if newFullName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "full_name cannot be empty"})
			return
		}
		if !user.PersonID.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "account has no person record"})
			return
		}
	}
	// Absent field = unchanged → pass the stored value back: UpdateOwnAppUserContact
	// has no COALESCE, a NULL param would clear the column.
	params := sqlcgen.UpdateOwnAppUserContactParams{Email: user.Email, Phone: user.Phone, ID: user.ID}
	if req.Email != nil {
		trimmed := strings.TrimSpace(*req.Email)
		if trimmed != "" && !strings.Contains(trimmed, "@") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid email format"})
			return
		}
		params.Email = pgtype.Text{String: trimmed, Valid: trimmed != ""}
	}
	if req.Phone != nil {
		trimmed := strings.TrimSpace(*req.Phone)
		params.Phone = pgtype.Text{String: trimmed, Valid: trimmed != ""}
	}
	if _, err := q.UpdateOwnAppUserContact(c.Request.Context(), params); err != nil {
		// Abort, not plain c.JSON: the failed UPDATE leaves the request tx in
		// Postgres' aborted state, so TenantMiddleware's commit would fail and
		// append a second JSON body after this response.
		if isUniqueViolation(err) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "email or phone already used"})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if req.FullName != nil {
		if err := q.UpdatePersonFullName(c.Request.Context(), sqlcgen.UpdatePersonFullNameParams{
			ID: user.PersonID, FullName: newFullName, UpdatedBy: user.ID,
		}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	// Response = GET shape — reload after the writes.
	user, err = q.GetAppUserByID(c.Request.Context(), user.ID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meData(c, q, user), "meta": gin.H{}})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required,min=8"`
}

// ChangePasswordHandler changes the caller's own password (spec §3): re-auth
// via verifyOwnPassword (same pattern as PIN set/disable and MFA disable),
// rate-limited with the login sliding-window limits keyed per user, then
// revokes every other session via session.RevokeAllExceptCurrent — the current
// device stays logged in (its refresh cookie identifies the kept session).
// ChangePasswordHandler godoc
// @Summary Change the caller's own password
// @Description Rate-limited per user. Revokes every other device's session; the current device stays logged in.
// @Tags account
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body changePasswordRequest true "Current + new password"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 429 {object} apiErrorResponse
// @Router /auth/password [post]
func ChangePasswordHandler(hasher *auth.PasswordHasher, limiter *ratelimit.Limiter, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req changePasswordRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		result, err := limiter.AllowSlidingWindow(c.Request.Context(), "password_change:"+userID.String(), cfg.RateLimitLoginMaxAttempts, cfg.RateLimitLoginWindowSeconds)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return
		}
		if !result.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts, try again later"})
			return
		}
		if req.NewPassword == req.CurrentPassword {
			c.JSON(http.StatusBadRequest, gin.H{"error": "new password must differ from the current password"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		if !verifyOwnPassword(c, q, hasher, userID, req.CurrentPassword) {
			return
		}
		hash, err := hasher.Hash(c.Request.Context(), req.NewPassword)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}
		if err := q.UpdateAppUserPassword(c.Request.Context(), sqlcgen.UpdateAppUserPasswordParams{
			ID: userID, PasswordHash: hash, UpdatedBy: userID,
		}); err != nil {
			// Abort: the failed write poisons the request tx (see UpdateMeHandler).
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		currentRawToken, _ := c.Cookie(refreshCookieName)
		if err := session.RevokeAllExceptCurrent(c.Request.Context(), q, userID, currentRawToken); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}
