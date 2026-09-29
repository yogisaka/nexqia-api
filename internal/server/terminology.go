// internal/server/terminology.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterTerminologyRoutes wires the read-only concept search used by the
// pendaftaran form's searchable comboboxes (wilayah, option, diagnosis).
// Guarded by PermPersonManage: the only consumers are person/pendaftaran
// screens, and the RBAC convention here has no separate read permission
// (see person.go).
func RegisterTerminologyRoutes(rg *gin.RouterGroup) {
	rg.GET("/terminology/search", SearchTerminologyHandler)
}

// SearchTerminologyHandler godoc
// @Summary Search terminology concepts by code_system name
// @Tags terminology
// @Produce json
// @Security BearerAuth
// @Param system query string true "Code system name, e.g. Kemendagri Region"
// @Param q query string false "Code/display search query, required unless full=true"
// @Param limit query int false "Page size"
// @Param full query bool false "Bypass paging, dump the whole code_system ordered by code (client-side bulk preload)"
// @Success 200 {object} apiResponse
// @Router /terminology/search [get]
func SearchTerminologyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	system := c.Query("system")
	if system == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing query param system"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))

	if c.Query("full") == "true" {
		concepts, err := q.ListConceptsBySystem(c.Request.Context(), system)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": concepts})
		return
	}

	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing query param q"})
		return
	}
	limit, _ := paginationParams(c)
	concepts, err := q.SearchConceptsBySystem(c.Request.Context(), sqlcgen.SearchConceptsBySystemParams{
		Name: system, Column2: pgtype.Text{String: query, Valid: true}, Limit: limit,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": concepts, "meta": gin.H{"limit": limit}})
}
