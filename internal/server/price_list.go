// internal/server/price_list.go
// Versioned price lists, category adjustments and the price grid/resolve
// endpoints (spec 2026-10-07-tariff-price-lists §4, §5).
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// parsePercentBP converts a signed percent string with up to 2 fraction
// digits ("30.00", "-10.50") into basis points (30.00% = 3000 bp). It parses
// the absolute value with parseCents and re-applies the sign: parseCents
// rejects signs itself.
func parsePercentBP(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	bp, err := parseCents(strings.TrimPrefix(s, "-"))
	if err != nil {
		return 0, err
	}
	if neg {
		bp = -bp
	}
	return bp, nil
}

// parseAPIDate parses an "YYYY-MM-DD" query/body date.
func parseAPIDate(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// priceListJSON renders a price list with snake_case fields; percents and
// rounding are decimal strings with 2 digits. `kind` is "base" when the list
// has no base but other lists derive from it, "standalone" when it has no
// base and no children, "derived" otherwise.
func priceListJSON(c *gin.Context, list sqlcgen.CorePriceList) gin.H {
	adjustment := "0.00"
	if v, err := numericToCents(list.AdjustmentPercent); err == nil {
		adjustment = formatCents(v)
	}
	rounding := "0.00"
	if v, err := numericToCents(list.RoundingUnit); err == nil {
		rounding = formatCents(v)
	}
	kind := "derived"
	if !list.BasePriceListID.Valid {
		kind = "standalone"
		q := sqlcgen.New(TxFromContext(c))
		if n, err := q.CountDerivedPriceLists(c.Request.Context(), list.ID); err == nil && n > 0 {
			kind = "base"
		}
	}
	var base any
	if list.BasePriceListID.Valid {
		base = list.BasePriceListID.String()
	}
	return gin.H{
		"id":                 list.ID.String(),
		"code":               list.Code,
		"name":               list.Name,
		"base_price_list_id": base,
		"kind":               kind,
		"adjustment_percent": adjustment,
		"rounding_unit":      rounding,
		"is_active":          list.IsActive,
		"row_version":        list.RowVersion,
	}
}

// loadPriceListAuthorized fetches the price list (404 when missing) and
// checks PermTariffManage against its actual merchant.
func loadPriceListAuthorized(c *gin.Context, rawID string) (sqlcgen.CorePriceList, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CorePriceList{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	list, err := q.GetPriceListByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "price list not found"})
		return sqlcgen.CorePriceList{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePriceList{}, false
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, list.MerchantID) {
		return sqlcgen.CorePriceList{}, false
	}
	return list, true
}

// loadPriceResolveContext loads the list's rule and (when derived) its base
// list and base rate rows for onDate. Inactive list/base → 409 per contract.
func loadPriceResolveContext(c *gin.Context, q *sqlcgen.Queries, list sqlcgen.CorePriceList, onDate pgtype.Date) (priceListRule, []sqlcgen.ListEffectiveRatesForListRow, bool) {
	rule := priceListRule{}
	if !list.IsActive {
		c.JSON(http.StatusConflict, gin.H{"error": "price list is inactive"})
		return rule, nil, false
	}
	if !list.BasePriceListID.Valid {
		return rule, nil, true
	}
	base, err := q.GetPriceListByID(c.Request.Context(), list.BasePriceListID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "base price list is inactive"})
		return rule, nil, false
	}
	if err != nil {
		respondInternalError(c, err)
		return rule, nil, false
	}
	if !base.IsActive {
		c.JSON(http.StatusConflict, gin.H{"error": "base price list is inactive"})
		return rule, nil, false
	}
	rule.Derived = true
	adjustmentBP, err := numericToCents(list.AdjustmentPercent)
	if err != nil {
		respondInternalError(c, err)
		return rule, nil, false
	}
	rule.AdjustmentBP = adjustmentBP
	rounding, err := numericToCents(list.RoundingUnit)
	if err != nil {
		respondInternalError(c, err)
		return rule, nil, false
	}
	rule.RoundingCents = rounding
	adjustments, err := q.ListPriceListAdjustments(c.Request.Context(), list.ID)
	if err != nil {
		respondInternalError(c, err)
		return rule, nil, false
	}
	rule.CategoryAdjustmentBP = make(map[string]int64, len(adjustments))
	for _, a := range adjustments {
		bp, err := numericToCents(a.AdjustmentPercent)
		if err != nil {
			respondInternalError(c, err)
			return rule, nil, false
		}
		rule.CategoryAdjustmentBP[a.ItemType] = bp
	}
	rows, err := q.ListEffectiveRatesForList(c.Request.Context(), sqlcgen.ListEffectiveRatesForListParams{
		PriceListID: base.ID, OnDate: onDate,
	})
	if err != nil {
		respondInternalError(c, err)
		return rule, nil, false
	}
	return rule, rows, true
}

// priceResultJSON renders one resolve/grid result; totals and amounts are
// decimal strings with 2 digits.
func priceResultJSON(item sqlcgen.CoreServiceItem, res resolvedPrice) gin.H {
	comps := make([]gin.H, 0, len(res.Components))
	for _, comp := range res.Components {
		comps = append(comps, gin.H{
			"rate_component_id": comp.ComponentID,
			"code":              comp.Code,
			"name":              comp.Name,
			"amount":            formatCents(comp.Cents),
		})
	}
	return gin.H{
		"item_id":                    item.ID.String(),
		"code":                       item.Code,
		"name":                       item.Name,
		"item_type":                  item.ItemType,
		"source":                     res.Source,
		"total":                      formatCents(res.TotalCents),
		"components":                 comps,
		"adjustment_percent_applied": formatCents(res.AppliedBP),
	}
}

// missingPriceResultJSON renders a grid row for an item with no resolvable
// price: source "missing", total null.
func missingPriceResultJSON(item sqlcgen.CoreServiceItem) gin.H {
	return gin.H{
		"item_id":                    item.ID.String(),
		"code":                       item.Code,
		"name":                       item.Name,
		"item_type":                  item.ItemType,
		"source":                     "missing",
		"total":                      nil,
		"components":                 []gin.H{},
		"adjustment_percent_applied": nil,
	}
}

// requestDate resolves the ?date= param (default: today in the merchant's
// timezone) as a pgtype.Date.
func requestDate(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, raw string) (pgtype.Date, bool) {
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return pgtype.Date{}, false
	}
	day := localDayStart(time.Now(), loc)
	if raw != "" {
		t, ok := parseAPIDate(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date"})
			return pgtype.Date{}, false
		}
		day = t
	}
	return pgtype.Date{Time: day, Valid: true}, true
}

// respondPriceListError maps insert/update Postgres errors: unique → 409
// "price list code already exists"; check_violation (23514, the one-level
// trigger and the table checks) → 409 one-level message.
func respondPriceListError(c *gin.Context, err error) bool {
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "price list code already exists"})
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		c.JSON(http.StatusConflict, gin.H{"error": "price list base must be a base list (one level of derivation)"})
		return true
	}
	return false
}

type createPriceListRequest struct {
	MerchantID        string `json:"merchant_id" binding:"required"`
	Code              string `json:"code" binding:"required"`
	Name              string `json:"name" binding:"required"`
	BasePriceListID   string `json:"base_price_list_id"`
	AdjustmentPercent string `json:"adjustment_percent"`
	RoundingUnit      string `json:"rounding_unit"`
}

// validatePriceListFields parses the optional base/percent/rounding fields
// shared by create and update; a base-less list must not carry an adjustment
// or rounding unit, and a base must belong to the same merchant.
func validatePriceListFields(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, req createPriceListRequest) (pgtype.UUID, int64, int64, bool) {
	var baseID pgtype.UUID
	if req.BasePriceListID != "" {
		id, ok := parseUUID(req.BasePriceListID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_price_list_id"})
			return baseID, 0, 0, false
		}
		base, err := q.GetPriceListByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && base.MerchantID != merchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "base price list not found"})
			return baseID, 0, 0, false
		}
		if err != nil {
			respondInternalError(c, err)
			return baseID, 0, 0, false
		}
		baseID = id
	}
	adjustment := req.AdjustmentPercent
	if adjustment == "" {
		adjustment = "0.00"
	}
	adjustBP, err := parsePercentBP(adjustment)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid adjustment_percent"})
		return baseID, 0, 0, false
	}
	rounding := req.RoundingUnit
	if rounding == "" {
		rounding = "0.00"
	}
	roundCents, err := parseCents(rounding)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rounding_unit"})
		return baseID, 0, 0, false
	}
	if !baseID.Valid && (adjustBP != 0 || roundCents != 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only derived lists take an adjustment or rounding"})
		return baseID, 0, 0, false
	}
	return baseID, adjustBP, roundCents, true
}

// CreatePriceListHandler godoc
// @Summary Create a price list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPriceListRequest true "Price list data"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-lists [post]
func CreatePriceListHandler(c *gin.Context) {
	var req createPriceListRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	baseID, adjustBP, roundCents, ok := validatePriceListFields(c, q, merchantID, req)
	if !ok {
		return
	}
	list, err := q.CreatePriceList(c.Request.Context(), sqlcgen.CreatePriceListParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: req.Code, Name: req.Name,
		BasePriceListID: baseID, AdjustmentPercent: centsToNumeric(adjustBP), RoundingUnit: centsToNumeric(roundCents),
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondPriceListError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": priceListJSON(c, list), "meta": gin.H{}})
}

// ListPriceListsHandler godoc
// @Summary List price lists under a merchant
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/price-lists [get]
func ListPriceListsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	lists, err := q.ListPriceListsByMerchant(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(lists))
	for _, list := range lists {
		data = append(data, priceListJSON(c, list))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetPriceListHandler godoc
// @Summary Get a price list by id
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /price-lists/{id} [get]
func GetPriceListHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": priceListJSON(c, list), "meta": gin.H{}})
}

type updatePriceListRequest struct {
	Name              string `json:"name" binding:"required"`
	BasePriceListID   string `json:"base_price_list_id"`
	AdjustmentPercent string `json:"adjustment_percent"`
	RoundingUnit      string `json:"rounding_unit"`
	IsActive          bool   `json:"is_active"`
}

// UpdatePriceListHandler godoc
// @Summary Update a price list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param request body updatePriceListRequest true "Price list data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /price-lists/{id} [patch]
func UpdatePriceListHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req updatePriceListRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	createReq := createPriceListRequest{
		BasePriceListID:   req.BasePriceListID,
		AdjustmentPercent: req.AdjustmentPercent,
		RoundingUnit:      req.RoundingUnit,
	}
	baseID, adjustBP, roundCents, ok := validatePriceListFields(c, q, list.MerchantID, createReq)
	if !ok {
		return
	}
	updated, err := q.UpdatePriceList(c.Request.Context(), sqlcgen.UpdatePriceListParams{
		ID: list.ID, Name: req.Name, BasePriceListID: baseID,
		AdjustmentPercent: centsToNumeric(adjustBP), RoundingUnit: centsToNumeric(roundCents),
		IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondPriceListError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": priceListJSON(c, updated), "meta": gin.H{}})
}

// DeletePriceListHandler godoc
// @Summary Soft-delete a price list
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /price-lists/{id} [delete]
func DeletePriceListHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	derived, err := q.CountDerivedPriceLists(c.Request.Context(), list.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if derived > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "price list is the base of other lists"})
		return
	}
	loc, err := merchantLocation(c.Request.Context(), q, list.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	today := pgtype.Date{Time: localDayStart(time.Now(), loc), Valid: true}
	live, err := q.CountLivePricesInList(c.Request.Context(), sqlcgen.CountLivePricesInListParams{
		PriceListID: list.ID, OnDate: today,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if live > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "price list still has live prices"})
		return
	}
	if err := q.SoftDeletePriceList(c.Request.Context(), sqlcgen.SoftDeletePriceListParams{ID: list.ID, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": list.ID.String(), "deleted": true}, "meta": gin.H{}})
}

type priceListAdjustmentInput struct {
	ItemType          string `json:"item_type" binding:"required"`
	AdjustmentPercent string `json:"adjustment_percent" binding:"required"`
}

type putPriceListAdjustmentsRequest struct {
	Adjustments []priceListAdjustmentInput `json:"adjustments" binding:"required"`
}

// PutPriceListAdjustmentsHandler godoc
// @Summary Replace the category adjustments of a derived price list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param request body putPriceListAdjustmentsRequest true "Category adjustments"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-lists/{id}/adjustments [put]
func PutPriceListAdjustmentsHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !list.BasePriceListID.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "only derived lists take category adjustments"})
		return
	}
	var req putPriceListAdjustmentsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	seen := make(map[string]bool, len(req.Adjustments))
	parsed := make([]sqlcgen.AddPriceListAdjustmentParams, 0, len(req.Adjustments))
	for _, a := range req.Adjustments {
		if !serviceItemTypes[a.ItemType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_type"})
			return
		}
		if seen[a.ItemType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate item_type"})
			return
		}
		seen[a.ItemType] = true
		bp, err := parsePercentBP(a.AdjustmentPercent)
		if err != nil || bp <= -10000 || bp > 100000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "adjustment_percent must be greater than -100 and at most 1000"})
			return
		}
		parsed = append(parsed, sqlcgen.AddPriceListAdjustmentParams{
			CompanyID: AuthCompanyID(c), MerchantID: list.MerchantID, PriceListID: list.ID,
			ItemType: a.ItemType, AdjustmentPercent: centsToNumeric(bp), CreatedBy: AuthUserID(c),
		})
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.DeletePriceListAdjustments(c.Request.Context(), list.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, p := range parsed {
		if err := q.AddPriceListAdjustment(c.Request.Context(), p); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	data := make([]gin.H, 0, len(parsed))
	for _, p := range parsed {
		data = append(data, gin.H{"item_type": p.ItemType, "adjustment_percent": formatCents(numericBP(p.AdjustmentPercent))})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"adjustments": data}, "meta": gin.H{}})
}

// numericBP reads back the basis points stored in an adjustment numeric.
func numericBP(n pgtype.Numeric) int64 {
	bp, err := numericToCents(n)
	if err != nil {
		return 0
	}
	return bp
}

type priceComponentInput struct {
	RateComponentID string `json:"rate_component_id" binding:"required"`
	Amount          string `json:"amount" binding:"required"`
}

type setPriceListPriceRequest struct {
	EffectiveFrom string                `json:"effective_from" binding:"required"`
	Components    []priceComponentInput `json:"components" binding:"required,min=1,dive"`
}

type pricedItem struct {
	list sqlcgen.CorePriceList
	item sqlcgen.CoreServiceItem
}

// loadPricedItem resolves the path ids of the set/delete price endpoints,
// checking that the item belongs to the list's merchant.
func loadPricedItem(c *gin.Context, list sqlcgen.CorePriceList, rawItem string) (sqlcgen.CoreServiceItem, bool) {
	itemID, ok := parseUUID(rawItem)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_id"})
		return sqlcgen.CoreServiceItem{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.GetServiceItemByID(c.Request.Context(), itemID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && item.MerchantID != list.MerchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
		return sqlcgen.CoreServiceItem{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CoreServiceItem{}, false
	}
	return item, true
}

// lockItemPrice takes the advisory lock serializing price versions of one
// (list, item); it is held until the request transaction ends.
func lockItemPrice(c *gin.Context, q *sqlcgen.Queries, merchantID, listID, itemID pgtype.UUID) bool {
	if err := q.LockSequence(c.Request.Context(), sqlcgen.LockSequenceParams{
		Column1: merchantID.String(),
		Column2: "price:" + listID.String() + ":" + itemID.String(),
	}); err != nil {
		respondInternalError(c, err)
		return false
	}
	return true
}

// SetPriceListPriceHandler godoc
// @Summary Set (or supersede) the manual price versions of an item in a list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param item_id path string true "Service item UUID"
// @Param request body setPriceListPriceRequest true "Price components"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-lists/{id}/items/{item_id}/price [put]
func SetPriceListPriceHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	item, ok := loadPricedItem(c, list, c.Param("item_id"))
	if !ok {
		return
	}
	var req setPriceListPriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Components) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one component is required"})
		return
	}
	effFrom, okDate := parseAPIDate(req.EffectiveFrom)
	if !okDate {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from"})
		return
	}
	effDate := pgtype.Date{Time: effFrom, Valid: true}
	q := sqlcgen.New(TxFromContext(c))
	seen := make(map[string]bool, len(req.Components))
	rows := make([]sqlcgen.InsertPriceRowParams, 0, len(req.Components))
	resolved := make([]rateRow, 0, len(req.Components))
	for _, comp := range req.Components {
		compID, okID := parseUUID(comp.RateComponentID)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rate_component_id"})
			return
		}
		if seen[comp.RateComponentID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate rate component"})
			return
		}
		seen[comp.RateComponentID] = true
		component, err := q.GetRateComponentByID(c.Request.Context(), compID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && component.MerchantID != list.MerchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "rate component not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		cents, err := parseCents(comp.Amount)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		rows = append(rows, sqlcgen.InsertPriceRowParams{
			CompanyID: AuthCompanyID(c), MerchantID: list.MerchantID,
			ServiceItemID: item.ID, RateComponentID: compID, PriceListID: list.ID,
			Amount: centsToNumeric(cents), EffectiveFrom: effDate, CreatedBy: AuthUserID(c),
		})
		resolved = append(resolved, rateRow{ComponentID: compID.String(), Code: component.Code, Name: component.Name, Cents: cents})
	}
	if !lockItemPrice(c, q, list.MerchantID, list.ID, item.ID) {
		return
	}
	latest, err := q.LatestPriceVersion(c.Request.Context(), sqlcgen.LatestPriceVersionParams{
		PriceListID: list.ID, ServiceItemID: item.ID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if latest.Valid && !effDate.Time.After(latest.Time) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("effective_from must be after the latest price version %s", latest.Time.Format("2006-01-02"))})
		return
	}
	if err := q.ClosePriceVersion(c.Request.Context(), sqlcgen.ClosePriceVersionParams{
		CloseOn:   pgtype.Date{Time: effFrom.AddDate(0, 0, -1), Valid: true},
		UpdatedBy: AuthUserID(c), PriceListID: list.ID, ServiceItemID: item.ID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, row := range rows {
		if err := q.InsertPriceRow(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	res, err := resolvePrice(item.ItemType, priceListRule{}, resolved, nil)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": priceResultJSON(item, res), "meta": gin.H{}})
}

// DeletePriceListPriceHandler godoc
// @Summary Delete a future price version of an item in a list
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param item_id path string true "Service item UUID"
// @Param effective_from query string true "Version start date (YYYY-MM-DD)"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /price-lists/{id}/items/{item_id}/price [delete]
func DeletePriceListPriceHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	item, ok := loadPricedItem(c, list, c.Param("item_id"))
	if !ok {
		return
	}
	effFrom, okDate := parseAPIDate(c.Query("effective_from"))
	if !okDate {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c.Request.Context(), q, list.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Compare as calendar dates: parseAPIDate yields UTC midnight while
	// localDayStart is midnight in the merchant zone.
	today := localDayStart(time.Now(), loc)
	if effFrom.Format("2006-01-02") <= today.Format("2006-01-02") {
		c.JSON(http.StatusConflict, gin.H{"error": "only future price versions can be deleted"})
		return
	}
	if !lockItemPrice(c, q, list.MerchantID, list.ID, item.ID) {
		return
	}
	deleted, err := q.DeletePriceVersion(c.Request.Context(), sqlcgen.DeletePriceVersionParams{
		DeletedBy: AuthUserID(c), PriceListID: list.ID, ServiceItemID: item.ID,
		EffectiveFrom: pgtype.Date{Time: effFrom, Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if deleted == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "price version not found"})
		return
	}
	if err := q.ReopenPreviousPriceVersion(c.Request.Context(), sqlcgen.ReopenPreviousPriceVersionParams{
		UpdatedBy: AuthUserID(c), PriceListID: list.ID, ServiceItemID: item.ID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}, "meta": gin.H{}})
}

// ResolvePriceListHandler godoc
// @Summary Resolve the price of one item on a date
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param item_id query string true "Service item UUID"
// @Param date query string false "Date (YYYY-MM-DD), default today"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /price-lists/{id}/resolve [get]
func ResolvePriceListHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadPricedItem(c, list, c.Query("item_id"))
	if !ok {
		return
	}
	onDate, okDate := requestDate(c, q, list.MerchantID, c.Query("date"))
	if !okDate {
		return
	}
	rule, baseListRows, okCtx := loadPriceResolveContext(c, q, list, onDate)
	if !okCtx {
		return
	}
	own, err := q.ListEffectiveRatesForItem(c.Request.Context(), sqlcgen.ListEffectiveRatesForItemParams{
		PriceListID: list.ID, ServiceItemID: item.ID, OnDate: onDate,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ownRows := make([]rateRow, 0, len(own))
	for _, r := range own {
		cents, err := numericToCents(r.Amount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		ownRows = append(ownRows, rateRow{ComponentID: r.RateComponentID.String(), Code: r.Code, Name: r.Name, Cents: cents})
	}
	baseRows := make([]rateRow, 0, len(baseListRows))
	for _, r := range baseListRows {
		if r.ServiceItemID != item.ID {
			continue
		}
		cents, err := numericToCents(r.Amount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		baseRows = append(baseRows, rateRow{ComponentID: r.RateComponentID.String(), Code: r.Code, Name: r.Name, Cents: cents})
	}
	res, err := resolvePrice(item.ItemType, rule, ownRows, baseRows)
	if errors.Is(err, errPriceMissing) {
		c.JSON(http.StatusNotFound, gin.H{"error": "price_missing"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": priceResultJSON(item, res), "meta": gin.H{}})
}

// ListPriceListItemsHandler godoc
// @Summary Price grid: resolve every item of a list on a date
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param date query string false "Date (YYYY-MM-DD), default today"
// @Param item_type query string false "Filter by item type"
// @Param q query string false "Filter by code or name"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /price-lists/{id}/items [get]
func ListPriceListItemsHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	onDate, okDate := requestDate(c, q, list.MerchantID, c.Query("date"))
	if !okDate {
		return
	}
	rule, baseListRows, okCtx := loadPriceResolveContext(c, q, list, onDate)
	if !okCtx {
		return
	}
	ownRowsAll, err := q.ListEffectiveRatesForList(c.Request.Context(), sqlcgen.ListEffectiveRatesForListParams{
		PriceListID: list.ID, OnDate: onDate,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Convert sqlc rows to resolver rows, grouped per item.
	toRows := func(rows []sqlcgen.ListEffectiveRatesForListRow) (map[string][]rateRow, bool) {
		byItem := map[string][]rateRow{}
		for _, r := range rows {
			cents, err := numericToCents(r.Amount)
			if err != nil {
				respondInternalError(c, err)
				return nil, false
			}
			id := r.ServiceItemID.String()
			byItem[id] = append(byItem[id], rateRow{ComponentID: r.RateComponentID.String(), Code: r.Code, Name: r.Name, Cents: cents})
		}
		return byItem, true
	}
	ownByItem, okRows := toRows(ownRowsAll)
	if !okRows {
		return
	}
	baseByItem, okRows := toRows(baseListRows)
	if !okRows {
		return
	}
	limit, offset := paginationParams(c)
	var itemType, search pgtype.Text
	if v := c.Query("item_type"); v != "" {
		itemType = pgtype.Text{String: v, Valid: true}
	}
	if v := c.Query("q"); v != "" {
		search = pgtype.Text{String: v, Valid: true}
	}
	items, err := q.ListServiceItemsForGrid(c.Request.Context(), sqlcgen.ListServiceItemsForGridParams{
		MerchantID: list.MerchantID, Limit: limit, Offset: offset, ItemType: itemType, Q: search,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(items))
	for _, item := range items {
		res, err := resolvePrice(item.ItemType, rule, ownByItem[item.ID.String()], baseByItem[item.ID.String()])
		if errors.Is(err, errPriceMissing) {
			data = append(data, missingPriceResultJSON(item))
			continue
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		data = append(data, priceResultJSON(item, res))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

var defaultRateComponents = []struct {
	Code string
	Name string
}{
	{"SARANA", "Jasa Sarana"},
	{"MEDIS", "Jasa Medis"},
	{"BHP", "Bahan Habis Pakai"},
	{"FARMASI", "Jasa Kefarmasian"},
	{"KLAIM", "Tarif Klaim"},
}

// CreateRateComponentDefaultsHandler godoc
// @Summary Create the standard rate components for a merchant if missing
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/rate-components/defaults [post]
func CreateRateComponentDefaultsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	var created int64
	for _, def := range defaultRateComponents {
		n, err := q.CreateRateComponentIfMissing(c.Request.Context(), sqlcgen.CreateRateComponentIfMissingParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: def.Code, Name: def.Name, CreatedBy: AuthUserID(c),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		created += n
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"created": created}, "meta": gin.H{}})
}
