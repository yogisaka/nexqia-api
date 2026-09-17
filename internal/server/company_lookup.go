// internal/server/company_lookup.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterCompanyLookupRoute wires the one deliberately public, un-tenant-scoped
// route in the API — the login screen doesn't know X-Company-ID yet, so it can't
// go through CompanyOnlyMiddleware/TenantMiddleware. See
// docs/design/specs/2026-09-16-his-auth-wiring-design.md §3.
func RegisterCompanyLookupRoute(rg *gin.RouterGroup, pool *pgxpool.Pool) {
	rg.GET("/companies/lookup", CompanyLookupHandler(pool))
}

type companyLookupResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CompanyLookupHandler godoc
// @Summary Resolve a company by its 6-character code
// @Description Public, unauthenticated. Used by the login screen to resolve X-Company-ID before calling /auth/login. Returns only id and name.
// @Tags auth
// @Produce json
// @Param code query string true "6-character alphanumeric company code"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /companies/lookup [get]
func CompanyLookupHandler(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Query("code")
		if !companyCodeFormat.MatchString(code) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "code must be exactly 6 uppercase alphanumeric characters"})
			return
		}
		q := sqlcgen.New(pool)
		company, err := q.LookupCompanyByCode(c.Request.Context(), code)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"data": companyLookupResponse{ID: company.ID.String(), Name: company.Name},
			"meta": gin.H{},
		})
	}
}
