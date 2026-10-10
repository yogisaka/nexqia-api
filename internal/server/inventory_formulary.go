// internal/server/inventory_formulary.go
// Per-merchant formulary API (spec 2026-10-07-tariff-c-drug-master-pricing
// §4.1, §4.2.1): activate company items as merchant service items, toggle
// them, and record the reference price (HNA+PPN per package).
package server

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// formularyItemType maps a company item kind to the core.service_item
// item_type created on activation (spec §4.1.3).
func formularyItemType(itemKind string) string {
	switch itemKind {
	case "drug", "vaccine":
		return "drug"
	case "medical_device":
		return "medical_device"
	default: // material, reagent, general
		return "material"
	}
}

// loadFormularyEntry fetches a live merchant_item, enforcing
// inventory.item.manage on ITS merchant (path-less routes take the merchant
// from the row). Writes 400/403/404 and returns false on failure.
func loadFormularyEntry(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryMerchantItem, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryMerchantItem{}, false
	}
	mi, err := q.GetMerchantItemByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "formulary entry not found"})
		return sqlcgen.InventoryMerchantItem{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryMerchantItem{}, false
	}
	if !RequirePermissionForMerchant(c, PermInventoryItemManage, mi.MerchantID) {
		return sqlcgen.InventoryMerchantItem{}, false
	}
	return mi, true
}

// referenceJSON renders the reference-price block: amount per package,
// packaging snapshot and the per-base-unit amount (amount ÷ factor, 2
// decimals half-up — informational only).
func referenceJSON(amount pgtype.Numeric, unitID pgtype.UUID, factor pgtype.Numeric, updatedAt pgtype.Timestamptz, unitCode pgtype.Text) any {
	if !amount.Valid {
		return nil
	}
	cents, err := numericToCents(amount)
	if err != nil {
		cents = 0
	}
	factorMilli := mustNumericScaled(factor, 3)
	// Half-up: base = floor((2·cents·1000 + factorMilli) / (2·factorMilli)).
	num := new(big.Int).Mul(big.NewInt(cents), big.NewInt(1000))
	den := big.NewInt(factorMilli)
	num.Mul(num, big.NewInt(2)).Add(num, den)
	den2 := new(big.Int).Mul(den, big.NewInt(2))
	baseCents := new(big.Int).Quo(num, den2).Int64()
	return gin.H{
		"amount":           formatCents(cents),
		"item_unit_id":     uuidOrNil(unitID),
		"unit_code":        textOrNil(unitCode),
		"factor":           formatScaled(factorMilli, 3),
		"base_unit_amount": formatCents(baseCents),
		"updated_at":       tsOrNil(updatedAt),
	}
}

// formularyRowJSON renders one formulary row (list, create, patch and
// reference-price responses all share this shape).
func formularyRowJSON(row sqlcgen.GetMerchantFormularyRowRow) gin.H {
	return gin.H{
		"id": row.ID.String(),
		"item": gin.H{
			"id":               row.ItemID.String(),
			"code":             row.ItemCode,
			"name":             row.ItemName,
			"item_kind":        row.ItemKind,
			"pricing_category": row.ItemPricingCategory,
			"drug_class":       textOrNil(row.ItemDrugClass),
			"base_unit_code":   row.BaseUnitCode,
			"het_amount":       hetJSON(row.ItemHetAmount),
		},
		"service_item_id":     row.ServiceItemID.String(),
		"default_location_id": uuidOrNil(row.DefaultLocationID),
		"is_active":           row.IsActive,
		"reference": referenceJSON(row.ReferenceAmount, row.ReferenceItemUnitID,
			row.ReferenceFactor, row.ReferenceUpdatedAt, row.ReferenceUnitCode),
		"row_version": row.RowVersion,
	}
}

// ListMerchantFormularyHandler godoc
// @Summary List a merchant's formulary with reference prices
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param q query string false "Filter by item code or name"
// @Param active query bool false "Filter by is_active"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/formulary [get]
func ListMerchantFormularyHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermInventoryItemManage, merchantID) {
		return
	}
	var qFilter pgtype.Text
	if v := c.Query("q"); v != "" {
		qFilter = pgtype.Text{String: v, Valid: true}
	}
	var active pgtype.Bool
	switch c.Query("active") {
	case "true":
		active = pgtype.Bool{Bool: true, Valid: true}
	case "false":
		active = pgtype.Bool{Bool: false, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListMerchantFormulary(c.Request.Context(), sqlcgen.ListMerchantFormularyParams{
		MerchantID: merchantID, Q: qFilter, IsActive: active,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, formularyRowJSON(sqlcgen.GetMerchantFormularyRowRow(r)))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type addMerchantFormularyRequest struct {
	ItemID            string `json:"item_id" binding:"required"`
	DefaultLocationID string `json:"default_location_id"`
}

// AddMerchantFormularyHandler godoc
// @Summary Add a company item to a merchant's formulary
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body addMerchantFormularyRequest true "Formulary entry"
// @Success 201 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/formulary [post]
func AddMerchantFormularyHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermInventoryItemManage, merchantID) {
		return
	}
	var req addMerchantFormularyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// 1. Item must be live AND active under the company RLS.
	item, ok := loadInventoryItem(c, q, req.ItemID)
	if !ok {
		return
	}
	if !item.IsActive {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	// 2. Default location (when sent) must belong to this merchant.
	var locationID pgtype.UUID
	if req.DefaultLocationID != "" {
		id, okID := parseUUID(req.DefaultLocationID)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid default_location_id"})
			return
		}
		loc, err := q.GetLocationByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != merchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		locationID = id
	}
	// 3. Already in the formulary (live row) → 409.
	if _, err := q.GetMerchantItemByMerchantItem(c.Request.Context(), sqlcgen.GetMerchantItemByMerchantItemParams{
		MerchantID: merchantID, ItemID: item.ID,
	}); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "item already in formulary"})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	// 4. Create the tariff-a service item so the drug can be priced/billed.
	si, err := q.CreateServiceItem(c.Request.Context(), sqlcgen.CreateServiceItemParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		Code: item.Code, Name: item.Name, ItemType: formularyItemType(item.ItemKind),
		CreatedBy: AuthUserID(c), ProcedureConceptID: pgtype.UUID{},
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("service item code %s already exists in this merchant", item.Code)})
			return
		}
		respondInternalError(c, err)
		return
	}
	// 5. Link the formulary row.
	mi, err := q.InsertMerchantItem(c.Request.Context(), sqlcgen.InsertMerchantItemParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		ItemID: item.ID, ServiceItemID: si.ID, DefaultLocationID: locationID,
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "item already in formulary"})
			return
		}
		respondInternalError(c, err)
		return
	}
	row, err := q.GetMerchantFormularyRow(c.Request.Context(), mi.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": formularyRowJSON(row), "meta": gin.H{}})
}

type updateFormularyRequest struct {
	IsActive          *bool   `json:"is_active"`
	DefaultLocationID *string `json:"default_location_id"`
	RowVersion        int64   `json:"row_version"`
}

// UpdateFormularyHandler godoc
// @Summary Update a formulary entry (activation and default location)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant item UUID"
// @Param request body updateFormularyRequest true "Entry fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /formulary/{id} [patch]
func UpdateFormularyHandler(c *gin.Context) {
	mi, ok := loadFormularyEntry(c, sqlcgen.New(TxFromContext(c)), c.Param("id"))
	if !ok {
		return
	}
	var req updateFormularyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(mi.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "formulary entry changed, reload"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Activating requires the company item to still be active.
	if req.IsActive != nil && *req.IsActive {
		item, err := q.GetInventoryItemByID(c.Request.Context(), mi.ItemID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !item.IsActive) {
			c.JSON(http.StatusConflict, gin.H{"error": "item is inactive"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
	}
	var locationID pgtype.UUID
	if req.DefaultLocationID != nil && *req.DefaultLocationID != "" {
		id, okID := parseUUID(*req.DefaultLocationID)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid default_location_id"})
			return
		}
		loc, err := q.GetLocationByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != mi.MerchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		locationID = id
	}
	updated, err := q.UpdateMerchantItem(c.Request.Context(), sqlcgen.UpdateMerchantItemParams{
		IsActive: nargBool(req.IsActive), DefaultLocationID: locationID,
		UpdatedBy: AuthUserID(c), MerchantItemID: mi.ID,
		ExpectedRowVersion: mi.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "formulary entry changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	// Activation state mirrors onto the tariff service item in the same tx.
	if req.IsActive != nil {
		if _, err := q.SetServiceItemActive(c.Request.Context(), sqlcgen.SetServiceItemActiveParams{
			IsActive: *req.IsActive, UpdatedBy: AuthUserID(c), ServiceItemID: mi.ServiceItemID,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	row, err := q.GetMerchantFormularyRow(c.Request.Context(), updated.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": formularyRowJSON(row), "meta": gin.H{}})
}

type setReferencePriceRequest struct {
	Amount     string `json:"amount" binding:"required"`
	ItemUnitID string `json:"item_unit_id" binding:"required"`
}

// SetFormularyReferencePriceHandler godoc
// @Summary Set the reference price (HNA+PPN) of a formulary entry
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant item UUID"
// @Param request body setReferencePriceRequest true "Reference price"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /formulary/{id}/reference-price [put]
func SetFormularyReferencePriceHandler(c *gin.Context) {
	mi, ok := loadFormularyEntry(c, sqlcgen.New(TxFromContext(c)), c.Param("id"))
	if !ok {
		return
	}
	var req setReferencePriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// The packaging must be an ACTIVE unit of THIS entry's item.
	unitID, okID := parseUUID(req.ItemUnitID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid packaging unit"})
		return
	}
	iu, err := q.GetInventoryItemUnitByID(c.Request.Context(), unitID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!iu.IsActive || iu.ItemID != mi.ItemID)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid packaging unit"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	cents, err := parseCents(req.Amount)
	if err != nil || cents <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	factorMilli := mustNumericScaled(iu.Factor, 3)
	updated, err := q.SetMerchantItemReference(c.Request.Context(), sqlcgen.SetMerchantItemReferenceParams{
		MerchantItemID: mi.ID, ReferenceAmount: centsToNumeric(cents),
		ReferenceItemUnitID: unitID, ReferenceFactor: scaledToNumeric(factorMilli, 3),
		ReferenceUpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := regenerateProposals(c, q, updated, "reference_changed"); err != nil {
		respondRegenerateError(c, err)
		return
	}
	row, err := q.GetMerchantFormularyRow(c.Request.Context(), mi.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": formularyRowJSON(row), "meta": gin.H{}})
}
