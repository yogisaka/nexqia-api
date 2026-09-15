// internal/server/server.go
package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/ratelimit"
	"github.com/yogisaka/nexqia-api/internal/version"
)

func NewRouter(pool *pgxpool.Pool, redisClient *redis.Client, cfg config.Config) *gin.Engine {
	router := gin.Default()
	router.GET("/healthz", HealthzHandler)
	router.GET("/version", VersionHandler)

	limiter := ratelimit.NewLimiter(redisClient)
	hasher := auth.NewPasswordHasher(
		cfg.PasswordHashMaxConcurrent,
		time.Duration(cfg.PasswordHashQueueTimeoutMS)*time.Millisecond,
		auth.Argon2Params{
			MemoryKiB:   cfg.Argon2MemoryKiB,
			Iterations:  cfg.Argon2Iterations,
			Parallelism: cfg.Argon2Parallelism,
		},
	)

	v1 := router.Group("/api/v1")
	v1.Use(TenantMiddleware(pool))
	v1.POST("/auth/login", LoginHandler(cfg.JWTSecret, limiter, cfg, hasher))

	protected := v1.Group("")
	protected.Use(AuthMiddleware(cfg.JWTSecret))
	protected.Use(RateLimitAPIMiddleware(limiter, cfg))
	protected.GET("/ping", PingHandler)
	RegisterTenancyRoutes(protected)
	RegisterRBACRoutes(protected, hasher)

	return router
}

func HealthzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func VersionHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"version": version.Version})
}

func PingHandler(c *gin.Context) {
	tx := TxFromContext(c)
	var companyID, merchantID string
	err := tx.QueryRow(c.Request.Context(),
		"SELECT current_setting('app.current_company_id'), current_setting('app.current_merchant_id')",
	).Scan(&companyID, &merchantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{"company_id": companyID, "merchant_id": merchantID},
		"meta": gin.H{},
	})
}
