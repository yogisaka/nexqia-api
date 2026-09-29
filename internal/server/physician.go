// internal/server/physician.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermPhysicianManage guards core.physician CRUD (docs/07-core-ddl.md §5).
// Prerequisite for the operations pillar — physician_schedule/admission/queue
// all FK to core.physician, see
// docs/design/specs/2026-09-16-v1-operations-antrian-jadwal-design.md §2.
const PermPhysicianManage = "core.physician.manage"

func RegisterPhysicianRoutes(rg *gin.RouterGroup) {
	rg.POST("/physicians", CreatePhysicianHandler)
	rg.GET("/merchants/:id/physicians", ListPhysiciansHandler)
	rg.GET("/physicians/:id", GetPhysicianHandler)
	rg.PATCH("/physicians/:id", UpdatePhysicianHandler)
	rg.DELETE("/physicians/:id", DeletePhysicianHandler)
}

type createPhysicianRequest struct {
	MerchantID         string `json:"merchant_id" binding:"required"`
	PersonID           string `json:"person_id" binding:"required"`
	StrNumber          string `json:"str_number"`
	SipNumber          string `json:"sip_number"`
	SpecialtyConceptID string `json:"specialty_concept_id"`
}

// CreatePhysicianHandler godoc
// @Summary Create a physician
// @Tags physician
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPhysicianRequest true "Physician data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /physicians [post]
func CreatePhysicianHandler(c *gin.Context) {
	var req createPhysicianRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	// Check permission against the body's merchant_id, not X-Merchant-ID —
	// they can differ, and creating under a merchant the caller only declared
	// in a header (without holding the permission there) is a cross-tenant
	// IDOR. See docs/design/plans/2026-09-16-v1-operations-antrian-jadwal-plan.md.
	if !RequirePermissionForMerchant(c, PermPhysicianManage, merchantID) {
		return
	}
	personID, ok := parseUUID(req.PersonID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	physician, err := q.CreatePhysician(c.Request.Context(), sqlcgen.CreatePhysicianParams{
		CompanyID:          AuthCompanyID(c),
		MerchantID:         merchantID,
		PersonID:           personID,
		StrNumber:          optText(req.StrNumber),
		SipNumber:          optText(req.SipNumber),
		SpecialtyConceptID: optUUID(req.SpecialtyConceptID),
		CreatedBy:          AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": physician, "meta": gin.H{}})
}

// ListPhysiciansHandler godoc
// @Summary List physicians under a merchant
// @Tags physician
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/physicians [get]
func ListPhysiciansHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermPhysicianManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	physicians, err := q.ListPhysiciansByMerchant(c.Request.Context(), sqlcgen.ListPhysiciansByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": physicians, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetPhysicianHandler godoc
// @Summary Get a physician by id
// @Tags physician
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physicians/{id} [get]
func GetPhysicianHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	physician, err := q.GetPhysicianByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermPhysicianManage, physician.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": physician, "meta": gin.H{}})
}

type updatePhysicianRequest struct {
	StrNumber          string `json:"str_number"`
	SipNumber          string `json:"sip_number"`
	SpecialtyConceptID string `json:"specialty_concept_id"`
	IsActive           bool   `json:"is_active"`
}

// UpdatePhysicianHandler godoc
// @Summary Update a physician
// @Tags physician
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician UUID"
// @Param request body updatePhysicianRequest true "Physician data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physicians/{id} [patch]
func UpdatePhysicianHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPhysicianByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermPhysicianManage, existing.MerchantID) {
		return
	}
	var req updatePhysicianRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	physician, err := q.UpdatePhysician(c.Request.Context(), sqlcgen.UpdatePhysicianParams{
		ID:                 id,
		StrNumber:          optText(req.StrNumber),
		SipNumber:          optText(req.SipNumber),
		SpecialtyConceptID: optUUID(req.SpecialtyConceptID),
		IsActive:           req.IsActive,
		UpdatedBy:          AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": physician, "meta": gin.H{}})
}

// DeletePhysicianHandler godoc
// @Summary Soft-delete a physician
// @Tags physician
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /physicians/{id} [delete]
func DeletePhysicianHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPhysicianByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermPhysicianManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeletePhysician(c.Request.Context(), sqlcgen.SoftDeletePhysicianParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
