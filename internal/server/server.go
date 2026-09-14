// internal/server/server.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/version"
)

func NewRouter(pool *pgxpool.Pool) *gin.Engine {
	router := gin.Default()
	router.GET("/healthz", HealthzHandler)
	router.GET("/version", VersionHandler)

	v1 := router.Group("/api/v1")
	v1.Use(TenantMiddleware(pool))
	v1.GET("/ping", PingHandler)

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
