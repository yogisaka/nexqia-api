// internal/server/inventory_pricing.go
// Margin rules per price list × pricing category and the drug sell price
// proposal engine (spec 2026-10-07-tariff-c-drug-master-pricing §4.2).
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// pricingCategories is the full set of inventory.item.pricing_category
// values (CHECK on inventory.margin_rule, migration 000067).
var pricingCategories = []string{
	"generic", "branded", "otc", "bmhp", "medical_device", "oncology", "vaccine", "other",
}

// defaultMarginBP is the default margin (20%) of the fill-defaults endpoint
// (pattern PMK 73/PMK.05/2013); oncology takes oncologyMarginBP (2%).
const (
	defaultMarginBP  = 2000
	oncologyMarginBP = 200
)

// barangComponent is the "Harga Barang" rate component proposals write.
const (
	barangComponentCode = "BARANG"
	barangComponentName = "Harga Barang"
)

// errItemPriceLocked signals that lockItemPrice has already written the error
// response, so the caller must not respond again.
var errItemPriceLocked = errors.New("item price lock failed")

// pricingCategorySet reports whether s is a valid pricing_category.
func pricingCategorySet(s string) bool {
	for _, c := range pricingCategories {
		if c == s {
			return true
		}
	}
	return false
}

// ruleCents renders a rule numeric (margin percent or rounding unit, both
// numeric(*,2)) as a 2-digit decimal string; the columns' scale makes the
// error path unreachable.
func ruleCents(n pgtype.Numeric) string {
	c, err := numericToCents(n)
	if err != nil {
		c = 0
	}
	return formatCents(c)
}

// marginRuleJSON renders one margin rule.
func marginRuleJSON(r sqlcgen.InventoryMarginRule) gin.H {
	return gin.H{
		"pricing_category": r.PricingCategory,
		"margin_percent":   ruleCents(r.MarginPercent),
		"rounding_unit":    ruleCents(r.RoundingUnit),
		"auto_approve":     r.AutoApprove,
	}
}

// proposalJSON renders one price proposal with item and list names (the
// Detail query's joined columns).
func proposalJSON(r sqlcgen.GetPriceProposalDetailRow) gin.H {
	var current any
	if c, err := numericToCents(r.CurrentAmount); err == nil {
		current = formatCents(c)
	}
	proposed := ruleCents(r.ProposedAmount)
	return gin.H{
		"id":               r.ID.String(),
		"merchant_item_id": r.MerchantItemID.String(),
		"item": gin.H{
			"code": r.ItemCode,
			"name": r.ItemName,
		},
		"price_list": gin.H{
			"id":   r.PriceListID.String(),
			"code": r.ListCode,
			"name": r.ListName,
		},
		"reason":          r.Reason,
		"current_amount":  current,
		"proposed_amount": proposed,
		"het_capped":      r.HetCapped,
		"status":          r.Status,
		"effective_from":  dateOrNil(r.EffectiveFrom),
		"created_at":      tsOrNil(r.CreatedAt),
	}
}

// ListMarginRulesHandler godoc
// @Summary List the margin rules of a price list
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Success 200 {object} apiResponse
// @Router /price-lists/{id}/margin-rules [get]
func ListMarginRulesHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rules, err := q.ListMarginRules(c.Request.Context(), list.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		data = append(data, marginRuleJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type marginRuleInput struct {
	PricingCategory string `json:"pricing_category" binding:"required"`
	MarginPercent   string `json:"margin_percent" binding:"required"`
	RoundingUnit    string `json:"rounding_unit"`
	AutoApprove     bool   `json:"auto_approve"`
}

type putMarginRulesRequest struct {
	Rules []marginRuleInput `json:"rules" binding:"required,dive"`
}

// requireBaseOrStandaloneList 409s when the list is derived: margin rules
// apply to base or standalone lists only.
func requireBaseOrStandaloneList(c *gin.Context, list sqlcgen.CorePriceList) bool {
	if list.BasePriceListID.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "margin rules apply to base or standalone lists only"})
		return false
	}
	return true
}

// parseMarginRules validates the request rules: invalid category, duplicates
// and out-of-range percents are rejected with the contract messages.
func parseMarginRules(c *gin.Context, req putMarginRulesRequest) ([]sqlcgen.InsertMarginRuleParams, bool) {
	seen := make(map[string]bool, len(req.Rules))
	parsed := make([]sqlcgen.InsertMarginRuleParams, 0, len(req.Rules))
	for _, r := range req.Rules {
		if !pricingCategorySet(r.PricingCategory) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pricing_category"})
			return nil, false
		}
		if seen[r.PricingCategory] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate pricing_category"})
			return nil, false
		}
		seen[r.PricingCategory] = true
		bp, err := parsePercentBP(r.MarginPercent)
		if err != nil || bp <= -10000 || bp > 100000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "margin_percent must be greater than -100 and at most 1000"})
			return nil, false
		}
		rounding := r.RoundingUnit
		if rounding == "" {
			rounding = "0.00"
		}
		roundCents, err := parseCents(rounding)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rounding_unit"})
			return nil, false
		}
		parsed = append(parsed, sqlcgen.InsertMarginRuleParams{
			CompanyID: AuthCompanyID(c), MerchantID: pgtype.UUID{}, PriceListID: pgtype.UUID{},
			PricingCategory: r.PricingCategory, MarginPercent: centsToNumeric(bp),
			RoundingUnit: centsToNumeric(roundCents), AutoApprove: r.AutoApprove,
			CreatedBy: AuthUserID(c),
		})
	}
	return parsed, true
}

// regenerateAllProposals recomputes proposals for every live, active
// merchant item of the merchant that has a reference price (spec §4.2.3).
func regenerateAllProposals(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, reason string) error {
	items, err := q.ListActiveMerchantItemsWithReference(c.Request.Context(), merchantID)
	if err != nil {
		return err
	}
	for _, mi := range items {
		if err := regenerateProposals(c, q, mi, reason); err != nil {
			return err
		}
	}
	return nil
}

// listMarginRules renders the rules of the list after a replace/fill.
func listMarginRules(c *gin.Context, listID pgtype.UUID) ([]gin.H, error) {
	q := sqlcgen.New(TxFromContext(c))
	rules, err := q.ListMarginRules(c.Request.Context(), listID)
	if err != nil {
		return nil, err
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		data = append(data, marginRuleJSON(r))
	}
	return data, nil
}

// respondRegenerateError maps regenerate errors; a lock failure already
// wrote its own response.
func respondRegenerateError(c *gin.Context, err error) {
	if !errors.Is(err, errItemPriceLocked) {
		respondInternalError(c, err)
	}
}

// PutMarginRulesHandler godoc
// @Summary Replace the margin rules of a base/standalone price list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Param request body putMarginRulesRequest true "Margin rules"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-lists/{id}/margin-rules [put]
func PutMarginRulesHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requireBaseOrStandaloneList(c, list) {
		return
	}
	var req putMarginRulesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	parsed, okRules := parseMarginRules(c, req)
	if !okRules {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.DeleteMarginRules(c.Request.Context(), list.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, p := range parsed {
		row := p
		row.MerchantID = list.MerchantID
		row.PriceListID = list.ID
		if err := q.InsertMarginRule(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if err := regenerateAllProposals(c, q, list.MerchantID, "margin_changed"); err != nil {
		respondRegenerateError(c, err)
		return
	}
	data, err := listMarginRules(c, list.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// PutDefaultMarginRulesHandler godoc
// @Summary Fill the default margin rules (20%, oncology 2%) of a list
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Price list UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-lists/{id}/margin-rules/defaults [post]
func PutDefaultMarginRulesHandler(c *gin.Context) {
	list, ok := loadPriceListAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requireBaseOrStandaloneList(c, list) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.ListMarginRules(c.Request.Context(), list.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	have := make(map[string]bool, len(existing))
	for _, r := range existing {
		have[r.PricingCategory] = true
	}
	for _, cat := range pricingCategories {
		if have[cat] {
			continue
		}
		bp := int64(defaultMarginBP)
		if cat == "oncology" {
			bp = oncologyMarginBP
		}
		if err := q.InsertMarginRule(c.Request.Context(), sqlcgen.InsertMarginRuleParams{
			CompanyID: AuthCompanyID(c), MerchantID: list.MerchantID, PriceListID: list.ID,
			PricingCategory: cat, MarginPercent: centsToNumeric(bp),
			RoundingUnit: centsToNumeric(0), AutoApprove: false, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if err := regenerateAllProposals(c, q, list.MerchantID, "margin_changed"); err != nil {
		respondRegenerateError(c, err)
		return
	}
	data, err := listMarginRules(c, list.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// regenerateProposals recomputes sell-price proposals for ONE merchant item
// after a change (spec §4.2.3): for every margin rule matching the item
// category on a live, active, non-derived list, compare the computed price
// with the current effective total and insert/auto-apply a pending proposal.
func regenerateProposals(c *gin.Context, q *sqlcgen.Queries, mi sqlcgen.InventoryMerchantItem, reason string) error {
	if !mi.ReferenceAmount.Valid {
		return nil
	}
	info, err := q.GetItemPricingInfo(c.Request.Context(), mi.ItemID)
	if err != nil {
		return err
	}
	loc, err := merchantLocation(c.Request.Context(), q, mi.MerchantID)
	if err != nil {
		return err
	}
	today := pgtype.Date{Time: localDayStart(time.Now(), loc), Valid: true}
	packCents, err := numericToCents(mi.ReferenceAmount)
	if err != nil {
		return err
	}
	factorMilli, err := numericToMilli(mi.ReferenceFactor)
	if err != nil {
		return err
	}
	var het int64
	if info.HetAmount.Valid && info.HetEffectiveFrom.Valid && !info.HetEffectiveFrom.Time.After(today.Time) {
		if het, err = numericToCents(info.HetAmount); err != nil {
			return err
		}
	}
	rules, err := q.ListMarginRulesForItem(c.Request.Context(), sqlcgen.ListMarginRulesForItemParams{
		MerchantID: mi.MerchantID, PricingCategory: info.PricingCategory,
	})
	if err != nil {
		return err
	}
	for _, rule := range rules {
		proposed, capped := drugSellPriceCents(packCents, factorMilli,
			numericBP(rule.MarginPercent), ruleCentsValue(rule.RoundingUnit), het)
		rates, err := q.ListEffectiveRatesForItem(c.Request.Context(), sqlcgen.ListEffectiveRatesForItemParams{
			PriceListID: rule.PriceListID, ServiceItemID: mi.ServiceItemID, OnDate: today,
		})
		if err != nil {
			return err
		}
		var current int64
		for _, r := range rates {
			c, err := numericToCents(r.Amount)
			if err != nil {
				return err
			}
			current += c
		}
		if err := q.SupersedePendingProposals(c.Request.Context(), sqlcgen.SupersedePendingProposalsParams{
			MerchantItemID: mi.ID, PriceListID: rule.PriceListID,
		}); err != nil {
			return err
		}
		if len(rates) > 0 && current == proposed {
			continue
		}
		proposalReason := reason
		if capped && reason == "margin_changed" {
			proposalReason = "het_changed"
		}
		var currentNum pgtype.Numeric
		if len(rates) > 0 {
			currentNum = centsToNumeric(current)
		}
		proposal, err := q.InsertPriceProposal(c.Request.Context(), sqlcgen.InsertPriceProposalParams{
			CompanyID: mi.CompanyID, MerchantID: mi.MerchantID,
			MerchantItemID: mi.ID, PriceListID: rule.PriceListID, Reason: proposalReason,
			CurrentAmount: currentNum, ProposedAmount: centsToNumeric(proposed),
			HetCapped: capped, Status: "pending", CreatedBy: AuthUserID(c),
		})
		if err != nil {
			return err
		}
		if rule.AutoApprove {
			if _, err := applyProposal(c, q, proposal, "auto_applied"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ruleCentsValue is ruleCents returning the raw cents for the price formula.
func ruleCentsValue(n pgtype.Numeric) int64 {
	c, err := numericToCents(n)
	if err != nil {
		return 0
	}
	return c
}

// applyProposal applies an approved/auto proposal under the item price lock
// (spec §4.2.4): effective today (merchant zone), or the day after the
// latest version when that starts today or later; the running version is
// closed, one BARANG rate row is inserted, and the proposal is decided.
func applyProposal(c *gin.Context, q *sqlcgen.Queries, proposal sqlcgen.InventoryPriceProposal, status string) (sqlcgen.InventoryPriceProposal, error) {
	mi, err := q.GetMerchantItemByID(c.Request.Context(), proposal.MerchantItemID)
	if err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	if !lockItemPrice(c, q, proposal.MerchantID, proposal.PriceListID, mi.ServiceItemID) {
		return sqlcgen.InventoryPriceProposal{}, errItemPriceLocked
	}
	loc, err := merchantLocation(c.Request.Context(), q, proposal.MerchantID)
	if err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	today := localDayStart(time.Now(), loc)
	eff := today
	latest, err := q.LatestPriceVersion(c.Request.Context(), sqlcgen.LatestPriceVersionParams{
		PriceListID: proposal.PriceListID, ServiceItemID: mi.ServiceItemID,
	})
	if err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	if latest.Valid && !today.After(latest.Time) {
		eff = latest.Time.AddDate(0, 0, 1)
	}
	if err := q.ClosePriceVersion(c.Request.Context(), sqlcgen.ClosePriceVersionParams{
		CloseOn:     pgtype.Date{Time: eff.AddDate(0, 0, -1), Valid: true},
		UpdatedBy:   AuthUserID(c),
		PriceListID: proposal.PriceListID, ServiceItemID: mi.ServiceItemID,
	}); err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	if _, err := q.CreateRateComponentIfMissing(c.Request.Context(), sqlcgen.CreateRateComponentIfMissingParams{
		CompanyID: proposal.CompanyID, MerchantID: proposal.MerchantID,
		Code: barangComponentCode, Name: barangComponentName, CreatedBy: AuthUserID(c),
	}); err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	comp, err := q.GetRateComponentByMerchantCode(c.Request.Context(), sqlcgen.GetRateComponentByMerchantCodeParams{
		MerchantID: proposal.MerchantID, Code: barangComponentCode,
	})
	if err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	proposedCents, err := numericToCents(proposal.ProposedAmount)
	if err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	if err := q.InsertPriceRow(c.Request.Context(), sqlcgen.InsertPriceRowParams{
		CompanyID: proposal.CompanyID, MerchantID: proposal.MerchantID,
		ServiceItemID: mi.ServiceItemID, RateComponentID: comp.ID, PriceListID: proposal.PriceListID,
		Amount: centsToNumeric(proposedCents), EffectiveFrom: pgtype.Date{Time: eff, Valid: true},
		CreatedBy: AuthUserID(c),
	}); err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	if err := q.DecidePriceProposal(c.Request.Context(), sqlcgen.DecidePriceProposalParams{
		Status: status, EffectiveFrom: pgtype.Date{Time: eff, Valid: true},
		DecidedBy: AuthUserID(c), ID: proposal.ID,
	}); err != nil {
		return sqlcgen.InventoryPriceProposal{}, err
	}
	return q.GetPriceProposalByID(c.Request.Context(), proposal.ID)
}

// loadPriceProposal fetches a proposal by id (404 when missing) and checks
// PermTariffManage on its merchant.
func loadPriceProposal(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryPriceProposal, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryPriceProposal{}, false
	}
	p, err := q.GetPriceProposalByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "proposal not found"})
		return sqlcgen.InventoryPriceProposal{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryPriceProposal{}, false
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, p.MerchantID) {
		return sqlcgen.InventoryPriceProposal{}, false
	}
	return p, true
}

// loadProposalDetail re-fetches the joined proposal row after deciding.
func loadProposalDetail(c *gin.Context, id pgtype.UUID) {
	q := sqlcgen.New(TxFromContext(c))
	detail, err := q.GetPriceProposalDetail(c.Request.Context(), id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": proposalJSON(detail), "meta": gin.H{}})
}

// ApprovePriceProposalHandler godoc
// @Summary Approve a pending price proposal
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Proposal UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-proposals/{id}/approve [post]
func ApprovePriceProposalHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	p, ok := loadPriceProposal(c, q, c.Param("id"))
	if !ok {
		return
	}
	if p.Status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "proposal is not pending"})
		return
	}
	if _, err := applyProposal(c, q, p, "approved"); err != nil {
		respondRegenerateError(c, err)
		return
	}
	loadProposalDetail(c, p.ID)
}

// RejectPriceProposalHandler godoc
// @Summary Reject a pending price proposal
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Proposal UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-proposals/{id}/reject [post]
func RejectPriceProposalHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	p, ok := loadPriceProposal(c, q, c.Param("id"))
	if !ok {
		return
	}
	if p.Status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "proposal is not pending"})
		return
	}
	if err := q.DecidePriceProposal(c.Request.Context(), sqlcgen.DecidePriceProposalParams{
		Status: "rejected", EffectiveFrom: pgtype.Date{}, DecidedBy: AuthUserID(c), ID: p.ID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	loadProposalDetail(c, p.ID)
}

type listPriceProposalsParams struct {
	MerchantID  pgtype.UUID
	Status      pgtype.Text
	PriceListID pgtype.UUID
}

// ListPriceProposalsHandler godoc
// @Summary List the price proposals of a merchant
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param status query string false "Filter by status (default pending)"
// @Param price_list_id query string false "Filter by price list"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/price-proposals [get]
func ListPriceProposalsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	params := listPriceProposalsParams{MerchantID: merchantID, Status: pgtype.Text{String: "pending", Valid: true}}
	if v := c.Query("status"); v != "" {
		params.Status = pgtype.Text{String: v, Valid: true}
	}
	if v := c.Query("price_list_id"); v != "" {
		id, okID := parseUUID(v)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid price_list_id"})
			return
		}
		params.PriceListID = id
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListPriceProposals(c.Request.Context(), sqlcgen.ListPriceProposalsParams{
		MerchantID: params.MerchantID, Status: params.Status, PriceListID: params.PriceListID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, proposalJSON(sqlcgen.GetPriceProposalDetailRow(r)))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// RefreshPriceProposalsHandler godoc
// @Summary Regenerate price proposals for all formulary items of a merchant
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/price-proposals/refresh [post]
func RefreshPriceProposalsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := regenerateAllProposals(c, q, merchantID, "margin_changed"); err != nil {
		respondRegenerateError(c, err)
		return
	}
	pending, err := q.CountPendingPriceProposals(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pending": pending}, "meta": gin.H{}})
}
