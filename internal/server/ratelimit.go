// internal/server/ratelimit.go
package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

// RateLimitAPIMiddleware enforces a per-user_id token bucket on every protected
// route. Must run after AuthMiddleware (needs AuthUserID). Fails closed: if Redis
// is unreachable, the request is rejected with 503, not allowed through — see
// docs/design/specs/2026-09-15-ratelimit-hardening-design.md §6.
func RateLimitAPIMiddleware(limiter *ratelimit.Limiter, cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := "api:user:" + AuthUserID(c).String()
		result, err := limiter.AllowTokenBucket(c.Request.Context(), key, cfg.RateLimitAPIBurst, cfg.RateLimitAPITokensPerMinute)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return
		}
		if !result.Allowed {
			c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests, try again later"})
			return
		}
		c.Next()
	}
}

// checkLoginRateLimit enforces sliding-window limits on both username and IP.
// Called from LoginHandler after parsing the request body (username is not known
// before that point) and before any password hash comparison — see spec §9 for
// why the cheap check must gate the expensive one. Writes the response itself
// and returns false on rejection; caller must return immediately in that case.
func checkLoginRateLimit(c *gin.Context, limiter *ratelimit.Limiter, cfg config.Config, username string) bool {
	keys := []string{
		"login:user:" + username,
		"login:ip:" + c.ClientIP(),
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

// checkRegisterRateLimit enforces a sliding-window limit per-IP only — no
// username exists yet before the request is processed (registration creates a
// brand-new account, spec §6). Stricter defaults than login since registering
// creates a company + account. Writes the response itself and returns false on
// rejection or Redis outage (fail-closed, same as checkLoginRateLimit).
func checkRegisterRateLimit(c *gin.Context, limiter *ratelimit.Limiter, cfg config.Config) bool {
	result, err := limiter.AllowSlidingWindow(c.Request.Context(), "register:ip:"+c.ClientIP(), cfg.RateLimitRegisterMaxAttempts, cfg.RateLimitRegisterWindowSeconds)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
		return false
	}
	if !result.Allowed {
		c.Header("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many registration attempts, try again later"})
		return false
	}
	return true
}
