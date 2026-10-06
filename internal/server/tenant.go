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
// Commit/rollback and response buffering are handled by RunRequestTx (txfinish.go).
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
			_ = tx.Rollback(ctx)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
			_ = tx.Rollback(ctx)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}

		RunRequestTx(c, tx)
	}
}

// TxFromContext retrieves the per-request transaction opened by TenantMiddleware.
func TxFromContext(c *gin.Context) pgx.Tx {
	return c.MustGet(txContextKey).(pgx.Tx)
}

// CompanyOnlyMiddleware is TenantMiddleware without the X-Merchant-ID requirement —
// for /auth/login, /auth/select-merchant and /auth/refresh, where the active merchant
// isn't known yet (spec §3a: a user can belong to >1 merchant, chosen at login).
// app.current_merchant_id is left unset; anything scoped only by company_id (app_user,
// core.list_user_merchants) still works, anything RLS-scoped by merchant does not —
// those tables must not be queried on this path.
func CompanyOnlyMiddleware(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		companyID := c.GetHeader("X-Company-ID")
		if companyID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "X-Company-ID header is required"})
			return
		}

		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to open transaction"})
			return
		}

		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
			_ = tx.Rollback(ctx)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set tenant context"})
			return
		}

		RunRequestTx(c, tx)
	}
}
