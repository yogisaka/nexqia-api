// internal/server/pin.go
// PIN enrollment/disable/verify — see docs/design/specs/2026-09-15-pin-unlock-design.md §5, §7.
package server

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

var pinFormat = regexp.MustCompile(`^\d{6}$`)

// RegisterPinRoutes wires PIN enrollment/disable/verify (spec §5, §7). Runs behind
// AuthMiddleware but deliberately OUTSIDE AppLockMiddleware (see server.go) — a
// locked user must still be able to call these to unlock or turn the lock off.
func RegisterPinRoutes(rg *gin.RouterGroup, hasher *auth.PasswordHasher, redisClient *redis.Client, cfg config.Config) {
	rg.POST("/auth/pin/set", PinSetHandler(hasher, redisClient, cfg))
	rg.POST("/auth/pin/disable", PinDisableHandler(hasher))
	rg.POST("/auth/pin/verify", PinVerifyHandler(hasher, redisClient, cfg))
}

// verifyOwnPassword re-confirms the caller's password — the same security-sensitive
// re-authentication pattern as MFAConfirmHandler/MFADisableHandler (internal/server/mfa.go).
// Writes the response and returns false on any failure; callers must return immediately.
func verifyOwnPassword(c *gin.Context, q *sqlcgen.Queries, hasher *auth.PasswordHasher, userID pgtype.UUID, password string) bool {
	user, err := q.GetAppUserByID(c.Request.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		return false
	}
	valid, err := hasher.Verify(c.Request.Context(), user.PasswordHash, password)
	if err != nil {
		if errors.Is(err, auth.ErrHashQueueTimeout) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify password"})
		return false
	}
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return false
	}
	return true
}

type pinSetRequest struct {
	Password string `json:"password" binding:"required"`
	Pin      string `json:"pin" binding:"required"`
}

// PinSetHandler sets or replaces the caller's PIN (spec §5) — same endpoint handles
// both first enrollment and change (overwrite). Hashed with the existing Argon2id
// PasswordHasher — PIN is compare-only like a password, no new crypto needed.
// PinSetHandler godoc
// @Summary Set or replace the caller's app-lock PIN
// @Tags pin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body pinSetRequest true "Password + new 6-digit PIN"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/pin/set [post]
func PinSetHandler(hasher *auth.PasswordHasher, redisClient *redis.Client, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		var req pinSetRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !pinFormat.MatchString(req.Pin) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pin must be exactly 6 digits"})
			return
		}
		userID := AuthUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		if !verifyOwnPassword(c, q, hasher, userID, req.Password) {
			return
		}
		hash, err := hasher.Hash(c.Request.Context(), req.Pin)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store pin"})
			return
		}
		if err := q.SetAppUserPin(c.Request.Context(), sqlcgen.SetAppUserPinParams{
			ID: userID, PinHash: pgtype.Text{String: hash, Valid: true}, UpdatedBy: userID,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save pin"})
			return
		}
		data := gin.H{}
		// Setting a PIN mid-session while the merchant's auth.pin_lock is on used to
		// leave no applock key, so AppLockMiddleware answered the very next request
		// with 423. Seed it exactly like issueLoginSession does.
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		if pinFlag := getPinLockFlag(c, q, merchantID); pinFlag.Enabled {
			idleMinutes := pinLockIdleMinutes(cfg, pinFlag)
			if err := redisClient.Set(c.Request.Context(), applockKey(userID, AuthDeviceID(c)), "1", time.Duration(idleMinutes)*time.Minute).Err(); err != nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			data["pin_lock_idle_minutes"] = idleMinutes
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
	}
}

type pinDisableRequest struct {
	Password string `json:"password" binding:"required"`
}

// PinDisableHandler clears the caller's PIN (spec §5) — enforcement stops
// immediately for them (AppLockMiddleware re-reads pin_hash every request, no
// cache), until they set a new one.
// PinDisableHandler godoc
// @Summary Clear the caller's app-lock PIN
// @Tags pin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body pinDisableRequest true "Password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /auth/pin/disable [post]
func PinDisableHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rejectDuringImpersonation(c) {
			return
		}
		var req pinDisableRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		userID := AuthUserID(c)
		q := sqlcgen.New(TxFromContext(c))
		if !verifyOwnPassword(c, q, hasher, userID, req.Password) {
			return
		}
		if err := q.ClearAppUserPin(c.Request.Context(), sqlcgen.ClearAppUserPinParams{
			ID: userID, UpdatedBy: userID,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disable pin"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
	}
}

type pinVerifyRequest struct {
	Pin string `json:"pin" binding:"required"`
}

// PinVerifyHandler re-proves the caller's PIN to unlock their device (spec §7).
// Tracks consecutive failures in Redis (pinfail:<user_id>:<device_id>) and force-
// revokes the device's session via the existing reuse-detection primitive
// (session.Refresh already calls the same q.RevokeAllRefreshTokensForDevice for
// stolen-token reuse) once APP_LOCK_MAX_PIN_ATTEMPTS is reached.
// PinVerifyHandler godoc
// @Summary Verify PIN to unlock the device
// @Description Tracks consecutive failures in Redis; force-revokes the device session after APP_LOCK_MAX_PIN_ATTEMPTS.
// @Tags pin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body pinVerifyRequest true "6-digit PIN"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse "too_many_attempts, requires re-login"
// @Router /auth/pin/verify [post]
func PinVerifyHandler(hasher *auth.PasswordHasher, redisClient *redis.Client, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req pinVerifyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
			return
		}
		ctx := c.Request.Context()
		userID := AuthUserID(c)
		deviceID := AuthDeviceID(c)

		failKey := pinfailKey(userID, deviceID)
		attempts, err := redisClient.Incr(ctx, failKey).Result()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return
		}
		if attempts == 1 {
			if err := redisClient.Expire(ctx, failKey, time.Duration(cfg.AppLockAttemptWindowMinutes)*time.Minute).Err(); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
		}

		q := sqlcgen.New(TxFromContext(c))
		user, err := q.GetAppUserByID(ctx, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify pin"})
			return
		}
		valid := false
		if user.PinHash.Valid {
			valid, err = hasher.Verify(ctx, user.PinHash.String, req.Pin)
			if err != nil {
				if errors.Is(err, auth.ErrHashQueueTimeout) {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify pin"})
				return
			}
		}

		if valid {
			redisClient.Del(ctx, failKey)
			flag := getPinLockFlag(c, q, merchantID)
			idleMinutes := pinLockIdleMinutes(cfg, flag)
			if err := redisClient.Set(ctx, applockKey(userID, deviceID), "1", time.Duration(idleMinutes)*time.Minute).Err(); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": gin.H{}, "meta": gin.H{}})
			return
		}

		if int(attempts) >= cfg.AppLockMaxPinAttempts {
			_ = q.RevokeAllRefreshTokensForDevice(ctx, sqlcgen.RevokeAllRefreshTokensForDeviceParams{
				UserID: userID, DeviceID: deviceID,
			})
			redisClient.Del(ctx, applockKey(userID, deviceID), failKey)
			c.JSON(http.StatusForbidden, gin.H{"error": "too_many_attempts", "requires_login": true})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid pin"})
	}
}
