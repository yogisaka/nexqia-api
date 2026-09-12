// internal/server/tenant.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const txContextKey = "db_tx"

// TenantMiddleware opens one transaction per request and binds Postgres RLS session
// variables via set_config(..., true) (transaction-scoped, equivalent to SET LOCAL) —
// required under PgBouncer transaction pooling, see docs/15-repo-infra-plan.md §6.
func TenantMiddleware(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		companyID := c.GetHeader("X-Company-ID")
		merchantID := c.GetHeader("X-Merchant-ID")
		if companyID == "" || merchantID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "X-Company-ID and X-Merchant-ID headers are required"})
			return
		}

		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}

		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
			tx.Rollback(ctx)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
			tx.Rollback(ctx)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}

		c.Set(txContextKey, tx)
		c.Next()

		if c.IsAborted() || len(c.Errors) > 0 {
			tx.Rollback(ctx)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to commit transaction"})
		}
	}
}

// TxFromContext retrieves the per-request transaction opened by TenantMiddleware.
func TxFromContext(c *gin.Context) pgx.Tx {
	return c.MustGet(txContextKey).(pgx.Tx)
}
