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

	rg.POST("/service-rates", CreateServiceRateHandler)
	rg.GET("/service-items/:id/rates", ListServiceRatesHandler)
	rg.GET("/service-rates/:id", GetServiceRateHandler)
	rg.PATCH("/service-rates/:id", UpdateServiceRateHandler)
	rg.DELETE("/service-rates/:id", DeleteServiceRateHandler)
}

// --- service_item ---

type createServiceItemRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
	Code       string `json:"code" binding:"required"`
	Name       string `json:"name" binding:"required"`
	ItemType   string `json:"item_type" binding:"required"`
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
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.CreateServiceItem(c.Request.Context(), sqlcgen.CreateServiceItemParams{
		CompanyID:  AuthCompanyID(c),
		MerchantID: merchantID,
		Code:       req.Code,
		Name:       req.Name,
		ItemType:   req.ItemType,
		CreatedBy:  AuthUserID(c),
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
	Name     string `json:"name" binding:"required"`
	ItemType string `json:"item_type" binding:"required"`
	IsActive bool   `json:"is_active"`
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
	item, err := q.UpdateServiceItem(c.Request.Context(), sqlcgen.UpdateServiceItemParams{
		ID: id, Name: req.Name, ItemType: req.ItemType, IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
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

type createServiceRateRequest struct {
	MerchantID      string `json:"merchant_id" binding:"required"`
	ServiceItemID   string `json:"service_item_id" binding:"required"`
	RateComponentID string `json:"rate_component_id" binding:"required"`
	PayerClass      string `json:"payer_class" binding:"required"`
	Amount          string `json:"amount" binding:"required"`
	EffectiveFrom   string `json:"effective_from" binding:"required"`
	EffectiveTo     string `json:"effective_to"`
}

// CreateServiceRateHandler godoc
// @Summary Create a service rate
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createServiceRateRequest true "Service rate data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /service-rates [post]
func CreateServiceRateHandler(c *gin.Context) {
	var req createServiceRateRequest
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
	serviceItemID, ok := parseUUID(req.ServiceItemID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_item_id"})
		return
	}
	rateComponentID, ok := parseUUID(req.RateComponentID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rate_component_id"})
		return
	}
	var amount pgtype.Numeric
	if err := amount.Scan(req.Amount); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	effectiveFrom, err := optDate(req.EffectiveFrom)
	if err != nil || !effectiveFrom.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from, expected YYYY-MM-DD"})
		return
	}
	effectiveTo, err := optDate(req.EffectiveTo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_to, expected YYYY-MM-DD"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rate, err := q.CreateServiceRate(c.Request.Context(), sqlcgen.CreateServiceRateParams{
		CompanyID:       AuthCompanyID(c),
		MerchantID:      merchantID,
		ServiceItemID:   serviceItemID,
		RateComponentID: rateComponentID,
		PayerClass:      req.PayerClass,
		Amount:          amount,
		EffectiveFrom:   effectiveFrom,
		EffectiveTo:     effectiveTo,
		CreatedBy:       AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": rate, "meta": gin.H{}})
}

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

// GetServiceRateHandler godoc
// @Summary Get a service rate by id
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service rate UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /service-rates/{id} [get]
func GetServiceRateHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rate, err := q.GetServiceRateByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service rate not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, rate.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rate, "meta": gin.H{}})
}

type updateServiceRateRequest struct {
	Amount      string `json:"amount" binding:"required"`
	EffectiveTo string `json:"effective_to"`
}

// UpdateServiceRateHandler godoc
// @Summary Update a service rate
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service rate UUID"
// @Param request body updateServiceRateRequest true "Service rate data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /service-rates/{id} [patch]
func UpdateServiceRateHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetServiceRateByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service rate not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	var req updateServiceRateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var amount pgtype.Numeric
	if err := amount.Scan(req.Amount); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	effectiveTo, err := optDate(req.EffectiveTo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_to, expected YYYY-MM-DD"})
		return
	}
	rate, err := q.UpdateServiceRate(c.Request.Context(), sqlcgen.UpdateServiceRateParams{
		ID: id, Amount: amount, EffectiveTo: effectiveTo, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service rate not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rate, "meta": gin.H{}})
}

// DeleteServiceRateHandler godoc
// @Summary Soft-delete a service rate
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Service rate UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /service-rates/{id} [delete]
func DeleteServiceRateHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetServiceRateByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service rate not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeleteServiceRate(c.Request.Context(), sqlcgen.SoftDeleteServiceRateParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
