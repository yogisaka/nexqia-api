// internal/server/procurement.go
// Procurement policies API (plan 2026-10-20-inventory-17b-p1-procurement-core
// Task 2, spec 2026-10-20-inventory-17b-procurement-special-cases §4.2–4.3,
// migration 000080_procurement_core): company contract prices, procurement
// mode settings and the central-item list — all company-level under
// inventory.purchase.manage except the merchant effective-price view.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// procurementMode resolves the company's procurement mode and headquarters
// merchant; no settings row yet = desentral with no headquarters (spec §4.1).
func procurementMode(ctx context.Context, q *sqlcgen.Queries, companyID pgtype.UUID) (mode string, hq pgtype.UUID, err error) {
	s, err := q.GetProcurementSetting(ctx, companyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "desentral", pgtype.UUID{}, nil
	}
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	return s.Mode, s.HeadquartersMerchantID, nil
}

// §4.1 JSON renderers.

func contractPriceJSON(p sqlcgen.InventoryContractPrice) gin.H {
	return gin.H{
		"id":           p.ID.String(),
		"supplier_id":  p.SupplierID.String(),
		"item_id":      p.ItemID.String(),
		"item_unit_id": p.ItemUnitID.String(),
		"kind":         p.Kind,
		"pack_price":   numericExact(p.PackPrice),
		"valid_from":   dateText(p.ValidFrom),
		"valid_until":  dateOrNil(p.ValidUntil),
		"note":         textOrNil(p.Note),
		"created_at":   tsOrNil(p.CreatedAt),
	}
}

func procurementSettingJSON(s sqlcgen.InventoryProcurementSetting) gin.H {
	return gin.H{
		"mode":                     s.Mode,
		"headquarters_merchant_id": uuidOrNil(s.HeadquartersMerchantID),
		"updated_at":               tsOrNil(s.UpdatedAt),
		"updated_by":               uuidOrNil(s.UpdatedBy),
	}
}

func procurementCentralItemJSON(r sqlcgen.InventoryProcurementCentralItem) gin.H {
	return gin.H{
		"id":         r.ID.String(),
		"item_id":    r.ItemID.String(),
		"created_at": tsOrNil(r.CreatedAt),
	}
}

// requireCompanyPathID checks the :id path company matches the session
// company (company-level resources are pinned by RLS to the session).
func requireCompanyPathID(c *gin.Context) (pgtype.UUID, bool) {
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return pgtype.UUID{}, false
	}
	return id, true
}

// §4.3 contract prices (company-level, PermPurchaseManage).

// ListContractPricesHandler godoc
// @Summary List the company's contract price rows
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /contract-prices [get]
func ListContractPricesHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListContractPrices(c.Request.Context(), AuthCompanyID(c))
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, p := range rows {
		data = append(data, contractPriceJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type contractPriceRequest struct {
	SupplierID string `json:"supplier_id" binding:"required"`
	ItemID     string `json:"item_id" binding:"required"`
	ItemUnitID string `json:"item_unit_id" binding:"required"`
	Kind       string `json:"kind" binding:"required"`
	PackPrice  string `json:"pack_price" binding:"required"`
	ValidFrom  string `json:"valid_from" binding:"required"`
	ValidUntil string `json:"valid_until"`
	Note       string `json:"note"`
}

// PutContractPriceHandler godoc
// @Summary Add a contract price row (revisi = baris valid_from baru)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body contractPriceRequest true "Contract price"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /contract-prices [put]
func PutContractPriceHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	var req contractPriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Kind != "purchase" && req.Kind != "consignment" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid kind"})
		return
	}
	supplierID, okS := parseUUID(req.SupplierID)
	itemID, okI := parseUUID(req.ItemID)
	unitID, okU := parseUUID(req.ItemUnitID)
	if !okS || !okI || !okU {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	price, err := parseNumeric(req.PackPrice)
	if err != nil || !price.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_price"})
		return
	}
	validFrom, okF := parseDateStr(req.ValidFrom)
	if !okF || !validFrom.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_from"})
		return
	}
	validUntil, _ := parseDateStr(req.ValidUntil)
	q := sqlcgen.New(TxFromContext(c))
	p, err := q.InsertContractPrice(c.Request.Context(), sqlcgen.InsertContractPriceParams{
		CompanyID: AuthCompanyID(c), SupplierID: supplierID, ItemID: itemID,
		ItemUnitID: unitID, Kind: req.Kind, PackPrice: price,
		ValidFrom: validFrom, ValidUntil: validUntil,
		Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		// uq_contract_price: same supplier+item+unit+kind+valid_from —
		// revisions are a NEW valid_from row; no overwrite (spec §4.1).
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "contract price already exists"})
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": contractPriceJSON(p), "meta": gin.H{}})
}

// DeleteContractPriceHandler godoc
// @Summary Delete a contract price row
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Contract price UUID"
// @Success 200 {object} apiResponse
// @Router /contract-prices/{id} [delete]
func DeleteContractPriceHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.DeleteContractPrice(c.Request.Context(), sqlcgen.DeleteContractPriceParams{
		ID: id, CompanyID: AuthCompanyID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id.String()}, "meta": gin.H{}})
}

// ListEffectiveContractPricesHandler godoc
// @Summary List contract prices in force today for a merchant (optional item_id)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param item_id query string false "Filter by item UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/contract-prices [get]
func ListEffectiveContractPricesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	var itemID pgtype.UUID
	if v := c.Query("item_id"); v != "" {
		id, okI := parseUUID(v)
		if !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_id"})
			return
		}
		itemID = id
	}
	loc, err := merchantLocation(c.Request.Context(), sqlcgen.New(TxFromContext(c)), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	today := pgtype.Date{Time: time.Now().In(loc), Valid: true}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListContractPrices(c.Request.Context(), AuthCompanyID(c))
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, p := range rows {
		if itemID.Valid && p.ItemID != itemID {
			continue
		}
		if p.ValidFrom.Time.After(today.Time) {
			continue
		}
		if p.ValidUntil.Valid && p.ValidUntil.Time.Before(today.Time) {
			continue
		}
		data = append(data, contractPriceJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// §4.3 procurement settings (company-level, PermPurchaseManage).

// GetProcurementSettingsHandler godoc
// @Summary Get the company's procurement mode settings
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/procurement-settings [get]
func GetProcurementSettingsHandler(c *gin.Context) {
	companyID, ok := requireCompanyPathID(c)
	if !ok {
		return
	}
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	s, err := q.GetProcurementSetting(c.Request.Context(), companyID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row yet = the migration defaults (desentral, no HQ).
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"mode": "desentral", "headquarters_merchant_id": nil,
		}, "meta": gin.H{}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": procurementSettingJSON(s), "meta": gin.H{}})
}

type patchProcurementSettingsRequest struct {
	Mode                   string `json:"mode" binding:"required"`
	HeadquartersMerchantID string `json:"headquarters_merchant_id"`
}

// PatchProcurementSettingsHandler godoc
// @Summary Set the procurement mode and headquarters merchant
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Param request body patchProcurementSettingsRequest true "Procurement settings"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /companies/{id}/procurement-settings [patch]
func PatchProcurementSettingsHandler(c *gin.Context) {
	companyID, ok := requireCompanyPathID(c)
	if !ok {
		return
	}
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	var req patchProcurementSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	switch req.Mode {
	case "desentral", "sentral", "hibrid":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
		return
	}
	var hqID pgtype.UUID
	if req.HeadquartersMerchantID != "" {
		id, okH := parseUUID(req.HeadquartersMerchantID)
		if !okH {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid headquarters_merchant_id"})
			return
		}
		hqID = id
	}
	if req.Mode != "desentral" && !hqID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "headquarters required"})
		return
	}
	if hqID.Valid {
		// The headquarters must be one of this company's merchants.
		m, err := sqlcgen.New(TxFromContext(c)).GetMerchantByID(c.Request.Context(), hqID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.CompanyID != companyID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "headquarters outside company"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	s, err := q.UpsertProcurementSetting(c.Request.Context(), sqlcgen.UpsertProcurementSettingParams{
		CompanyID: companyID, Mode: req.Mode, HeadquartersMerchantID: hqID,
		UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": procurementSettingJSON(s), "meta": gin.H{}})
}

// §4.3 central items (company-level, PermPurchaseManage).

// ListProcurementCentralItemsHandler godoc
// @Summary List items that must be procured through the headquarters
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/procurement-central-items [get]
func ListProcurementCentralItemsHandler(c *gin.Context) {
	companyID, ok := requireCompanyPathID(c)
	if !ok {
		return
	}
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListProcurementCentralItems(c.Request.Context(), companyID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, procurementCentralItemJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type putProcurementCentralItemsRequest struct {
	ItemIDs []string `json:"item_ids" binding:"required,dive,uuid"`
}

// PutProcurementCentralItemsHandler godoc
// @Summary Replace the whole central-item list in one transaction
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Param request body putProcurementCentralItemsRequest true "Item id list"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/procurement-central-items [put]
func PutProcurementCentralItemsHandler(c *gin.Context) {
	companyID, ok := requireCompanyPathID(c)
	if !ok {
		return
	}
	if !RequireCompanyLevelPermission(c, PermPurchaseManage) {
		return
	}
	var req putProcurementCentralItemsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ids := make([]pgtype.UUID, 0, len(req.ItemIDs))
	for _, raw := range req.ItemIDs {
		id, okI := parseUUID(raw)
		if !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_ids"})
			return
		}
		ids = append(ids, id)
	}
	q := sqlcgen.New(TxFromContext(c))
	// Replace-all in ONE transaction (delete all rows, then insert the new
	// list) — a failure rolls back to the previous list.
	if err := q.DeleteProcurementCentralItem(c.Request.Context(), companyID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, id := range ids {
		if _, err := q.InsertProcurementCentralItem(c.Request.Context(), sqlcgen.InsertProcurementCentralItemParams{
			CompanyID: companyID, ItemID: id, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	rows, err := q.ListProcurementCentralItems(c.Request.Context(), companyID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, procurementCentralItemJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
