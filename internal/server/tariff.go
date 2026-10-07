// internal/server/tariff.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermTariffManage guards core.service_item, core.rate_component and
// core.service_rate CRUD (docs/07-core-ddl.md §7 "Tarif engine") — one manage
// permission across all three, matching PermPersonManage/PermDepartmentManage.
const PermTariffManage = "core.tariff.manage"

// RegisterTariffRoutes wires the tariff engine's 3 tables. Same permission-scoping
// convention as department.go: create/list use RequirePermission (X-Merchant-ID
// header), get/update/delete on an existing resource use RequirePermissionForMerchant
// scoped to that resource's actual merchant_id.
func RegisterTariffRoutes(rg *gin.RouterGroup) {
	rg.POST("/service-items", CreateServiceItemHandler)
	rg.GET("/merchants/:id/service-items", ListServiceItemsHandler)
	rg.GET("/service-items/:id", GetServiceItemHandler)
	rg.PATCH("/service-items/:id", UpdateServiceItemHandler)
	rg.DELETE("/service-items/:id", DeleteServiceItemHandler)

	rg.POST("/rate-components", CreateRateComponentHandler)
	rg.GET("/merchants/:id/rate-components", ListRateComponentsHandler)
	rg.GET("/rate-components/:id", GetRateComponentHandler)
	rg.PATCH("/rate-components/:id", UpdateRateComponentHandler)
	rg.DELETE("/rate-components/:id", DeleteRateComponentHandler)

	rg.GET("/service-items/:id/rates", ListServiceRatesHandler)
}

// --- service_item ---

// serviceItemTypes mirrors the 9 values of the service_item_item_type_check
// constraint (migration 000063, spec 2026-10-07-tariff-price-lists §3.1).
var serviceItemTypes = map[string]bool{
	"consultation": true, "procedure": true, "administration": true, "room": true,
	"package": true, "pharmacy_fee": true, "drug": true, "medical_device": true,
	"material": true,
}

// icd9cmSystemURI is the FHIR system URI for ICD-9-CM procedures
// (spec 2026-10-07-tariff-price-lists §4.2 / terminology spec 2026-10-01).
const icd9cmSystemURI = "http://hl7.org/fhir/sid/icd-9-cm"

// validateProcedureConceptID parses an optional UUID string and checks the
// concept is active, selectable, and belongs to the ICD-9-CM code system.
func validateProcedureConceptID(c *gin.Context, raw string) (pgtype.UUID, bool) {
	var conceptID pgtype.UUID
	if raw == "" {
		return conceptID, true
	}
	id, ok := parseUUID(raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid procedure_concept_id"})
		return conceptID, false
	}
	var exists bool
	err := TxFromContext(c).QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM terminology.concept ct
		JOIN terminology.code_system cs ON cs.id = ct.code_system_id
		WHERE ct.id = $1 AND ct.is_active AND ct.is_selectable AND cs.system_uri = $2)`,
		id, icd9cmSystemURI).Scan(&exists)
	if err != nil {
		respondInternalError(c, err)
		return conceptID, false
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "procedure_concept_id must be an active ICD-9-CM concept"})
		return conceptID, false
	}
	return id, true
}

type createServiceItemRequest struct {
	MerchantID         string `json:"merchant_id" binding:"required"`
	Code               string `json:"code" binding:"required"`
	Name               string `json:"name" binding:"required"`
	ItemType           string `json:"item_type" binding:"required"`
	ProcedureConceptID string `json:"procedure_concept_id"`
}

// CreateServiceItemHandler godoc
// @Summary Create a service item
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createServiceItemRequest true "Service item data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /service-items [post]
func CreateServiceItemHandler(c *gin.Context) {
	var req createServiceItemRequest
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
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	if !serviceItemTypes[req.ItemType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_type"})
		return
	}
	procedureConceptID, ok := validateProcedureConceptID(c, req.ProcedureConceptID)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.CreateServiceItem(c.Request.Context(), sqlcgen.CreateServiceItemParams{
		CompanyID:          AuthCompanyID(c),
		MerchantID:         merchantID,
		Code:               req.Code,
		Name:               req.Name,
		ItemType:           req.ItemType,
		CreatedBy:          AuthUserID(c),
		ProcedureConceptID: procedureConceptID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": item, "meta": gin.H{}})
}

// ListServiceItemsHandler godoc
// @Summary List service items under a merchant
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/service-items [get]
func ListServiceItemsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	items, err := q.ListServiceItemsByMerchant(c.Request.Context(), sqlcgen.ListServiceItemsByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetServiceItemHandler godoc
// @Summary Get a service item by id
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service item UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /service-items/{id} [get]
func GetServiceItemHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.GetServiceItemByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, item.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item, "meta": gin.H{}})
}

type updateServiceItemRequest struct {
	Name               string `json:"name" binding:"required"`
	ItemType           string `json:"item_type" binding:"required"`
	IsActive           bool   `json:"is_active"`
	ProcedureConceptID string `json:"procedure_concept_id"`
}

// UpdateServiceItemHandler godoc
// @Summary Update a service item
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service item UUID"
// @Param request body updateServiceItemRequest true "Service item data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /service-items/{id} [patch]
func UpdateServiceItemHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetServiceItemByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	var req updateServiceItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !serviceItemTypes[req.ItemType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_type"})
		return
	}
	procedureConceptID, ok := validateProcedureConceptID(c, req.ProcedureConceptID)
	if !ok {
		return
	}
	item, err := q.UpdateServiceItem(c.Request.Context(), sqlcgen.UpdateServiceItemParams{
		ID: id, Name: req.Name, ItemType: req.ItemType, ProcedureConceptID: procedureConceptID,
		IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item, "meta": gin.H{}})
}

// DeleteServiceItemHandler godoc
// @Summary Soft-delete a service item
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service item UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /service-items/{id} [delete]
func DeleteServiceItemHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetServiceItemByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeleteServiceItem(c.Request.Context(), sqlcgen.SoftDeleteServiceItemParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- rate_component ---

type createRateComponentRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
	Code       string `json:"code" binding:"required"`
	Name       string `json:"name" binding:"required"`
}

// CreateRateComponentHandler godoc
// @Summary Create a rate component
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createRateComponentRequest true "Rate component data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /rate-components [post]
func CreateRateComponentHandler(c *gin.Context) {
	var req createRateComponentRequest
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
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	component, err := q.CreateRateComponent(c.Request.Context(), sqlcgen.CreateRateComponentParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: req.Code, Name: req.Name, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": component, "meta": gin.H{}})
}

// ListRateComponentsHandler godoc
// @Summary List rate components under a merchant
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/rate-components [get]
func ListRateComponentsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	components, err := q.ListRateComponentsByMerchant(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": components, "meta": gin.H{}})
}

// GetRateComponentHandler godoc
// @Summary Get a rate component by id
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Rate component UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /rate-components/{id} [get]
func GetRateComponentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	component, err := q.GetRateComponentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "rate component not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, component.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": component, "meta": gin.H{}})
}

type updateRateComponentRequest struct {
	Name string `json:"name" binding:"required"`
}

// UpdateRateComponentHandler godoc
// @Summary Update a rate component
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Rate component UUID"
// @Param request body updateRateComponentRequest true "Rate component data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /rate-components/{id} [patch]
func UpdateRateComponentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetRateComponentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "rate component not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	var req updateRateComponentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	component, err := q.UpdateRateComponent(c.Request.Context(), sqlcgen.UpdateRateComponentParams{
		ID: id, Name: req.Name, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "rate component not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": component, "meta": gin.H{}})
}

// DeleteRateComponentHandler godoc
// @Summary Soft-delete a rate component
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Rate component UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /rate-components/{id} [delete]
func DeleteRateComponentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetRateComponentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "rate component not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeleteRateComponent(c.Request.Context(), sqlcgen.SoftDeleteRateComponentParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- service_rate ---

// ListServiceRatesHandler godoc
// @Summary List rates for a service item
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service item UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /service-items/{id}/rates [get]
func ListServiceRatesHandler(c *gin.Context) {
	serviceItemID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.GetServiceItemByID(c.Request.Context(), serviceItemID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, item.MerchantID) {
		return
	}
	rates, err := q.ListServiceRatesByServiceItem(c.Request.Context(), sqlcgen.ListServiceRatesByServiceItemParams{
		MerchantID: item.MerchantID, ServiceItemID: serviceItemID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rates, "meta": gin.H{}})
}
