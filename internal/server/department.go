// internal/server/department.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermDepartmentManage guards core.department (poli) CRUD. ward/bed are
// explicitly out of v1 scope (ranap-only) and have no routes here — see
// docs/design/specs/2026-09-15-v1-rajal-poliumum-scope-design.md.
const PermDepartmentManage = "core.department.manage"

// RegisterDepartmentRoutes wires core.department CRUD (docs/07-core-ddl.md §6
// "Master Data fasilitas"). List/create use RequirePermission (checked against
// X-Merchant-ID header); get/update/delete on an existing department use
// RequirePermissionForMerchant scoped to that department's actual merchant_id,
// same isolation rule as tenancy.go's merchant handlers.
func RegisterDepartmentRoutes(rg *gin.RouterGroup) {
	rg.POST("/departments", CreateDepartmentHandler)
	rg.GET("/merchants/:id/departments", ListDepartmentsHandler)
	rg.GET("/departments/:id", GetDepartmentHandler)
	rg.PATCH("/departments/:id", UpdateDepartmentHandler)
	rg.DELETE("/departments/:id", DeleteDepartmentHandler)
}

type createDepartmentRequest struct {
	MerchantID         string `json:"merchant_id" binding:"required"`
	Code               string `json:"code" binding:"required"`
	Name               string `json:"name" binding:"required"`
	SpecialtyConceptID string `json:"specialty_concept_id"`
}

// CreateDepartmentHandler godoc
// @Summary Create a department (poli)
// @Tags department
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createDepartmentRequest true "Department data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /departments [post]
func CreateDepartmentHandler(c *gin.Context) {
	var req createDepartmentRequest
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
	// IDOR.
	if !RequirePermissionForMerchant(c, PermDepartmentManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	department, err := q.CreateDepartment(c.Request.Context(), sqlcgen.CreateDepartmentParams{
		CompanyID:          AuthCompanyID(c),
		MerchantID:         merchantID,
		Code:               req.Code,
		Name:               req.Name,
		SpecialtyConceptID: optUUID(req.SpecialtyConceptID),
		CreatedBy:          AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": department, "meta": gin.H{}})
}

// ListDepartmentsHandler godoc
// @Summary List departments under a merchant
// @Tags department
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/departments [get]
func ListDepartmentsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermDepartmentManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	departments, err := q.ListDepartmentsByMerchant(c.Request.Context(), sqlcgen.ListDepartmentsByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": departments, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetDepartmentHandler godoc
// @Summary Get a department by id
// @Tags department
// @Produce json
// @Security BearerAuth
// @Param id path string true "Department UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /departments/{id} [get]
func GetDepartmentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	department, err := q.GetDepartmentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermDepartmentManage, department.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": department, "meta": gin.H{}})
}

type updateDepartmentRequest struct {
	Name               string `json:"name" binding:"required"`
	SpecialtyConceptID string `json:"specialty_concept_id"`
	IsActive           bool   `json:"is_active"`
}

// UpdateDepartmentHandler godoc
// @Summary Update a department
// @Tags department
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Department UUID"
// @Param request body updateDepartmentRequest true "Department data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /departments/{id} [patch]
func UpdateDepartmentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetDepartmentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermDepartmentManage, existing.MerchantID) {
		return
	}
	var req updateDepartmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	department, err := q.UpdateDepartment(c.Request.Context(), sqlcgen.UpdateDepartmentParams{
		ID:                 id,
		Name:               req.Name,
		SpecialtyConceptID: optUUID(req.SpecialtyConceptID),
		IsActive:           req.IsActive,
		UpdatedBy:          AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": department, "meta": gin.H{}})
}

// DeleteDepartmentHandler godoc
// @Summary Soft-delete a department
// @Tags department
// @Produce json
// @Security BearerAuth
// @Param id path string true "Department UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /departments/{id} [delete]
func DeleteDepartmentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetDepartmentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermDepartmentManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeleteDepartment(c.Request.Context(), sqlcgen.SoftDeleteDepartmentParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
