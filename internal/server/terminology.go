// internal/server/terminology.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
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
	rg.GET("/terminology/map", TerminologyMapHandler)
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

// TerminologyMapHandler godoc
// @Summary List conversion candidates from one code system to another
// @Description Returns every active map-set candidate source→target (spec §6.2); auto is true only when exactly one selectable non-cluster candidate exists (§3.3).
// @Tags terminology
// @Produce json
// @Security BearerAuth
// @Param system query string true "Source code system name, e.g. ICD-10"
// @Param code query string true "Source concept code, e.g. A00.0"
// @Param target query string true "Target code system name, e.g. ICD-11"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 404 {object} apiResponse
// @Router /terminology/map [get]
func TerminologyMapHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	system, code, target := c.Query("system"), c.Query("code"), c.Query("target")
	if system == "" || code == "" || target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "system, code and target query params are required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	source, err := q.GetActiveConceptBySystemName(c.Request.Context(), sqlcgen.GetActiveConceptBySystemNameParams{
		System: system, Code: code,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "concept not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListMapCandidates(c.Request.Context(), sqlcgen.ListMapCandidatesParams{
		SourceID: source.ID, Target: target,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	candidates := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		candidates = append(candidates, gin.H{
			"code": r.TargetCode, "display": r.Display, "uri": r.Uri,
			"is_selectable": r.IsSelectable, "is_preferred": r.IsPreferred,
			"relationship": r.Relationship, "map_set": r.MapSet,
			"is_cluster": r.TargetCode != r.StemCode,
		})
	}
	auto := len(rows) == 1 && rows[0].IsSelectable && rows[0].TargetCode == rows[0].StemCode
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"source":     gin.H{"code": source.Code, "display": source.Display},
		"auto":       auto,
		"candidates": candidates,
	}, "meta": gin.H{}})
}
