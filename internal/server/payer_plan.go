// internal/server/payer_plan.go
// Payer service rules (blacklist/contract discount), payer plans with care
// rules and per-visit limits, BPJS item coverage (with the non-capitation
// template import) and the merchant billing/facility profile — spec
// 2026-10-07-tariff-b1-billing-core §3.3–3.6, §6. Routes are wired from
// RegisterPayerRoutes (payer.go).
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

var planCareTypes = map[string]bool{"outpatient": true, "inpatient": true, "emergency": true}

var coverageValues = map[string]bool{"capitation": true, "claim": true, "excluded": true, "package": true}

var claimTypes = map[string]bool{
	"anc": true, "pnc": true, "delivery": true, "kb": true, "prb": true,
	"ambulance": true, "screening": true, "first_inpatient": true, "pre_referral": true, "other": true,
}

var facilityTypes = map[string]bool{
	"klinik_pratama": true, "klinik_utama": true, "praktik_dokter": true,
	"praktik_dokter_gigi": true, "praktik_bidan": true, "puskesmas": true, "rumah_sakit": true,
}

var bpjsCooperationTypes = map[string]bool{"none": true, "fktp_capitation": true, "fktp_network": true, "fkrtl": true}

// numStr2 renders a numeric column as a 2-decimal decimal string.
func numStr2(n pgtype.Numeric) string {
	return formatCents(numericBP(n))
}

// requirePlanTypeVisible rejects payers that cannot own plans (bpjs/self_pay).
func requirePlanCapablePayer(c *gin.Context, payer sqlcgen.CorePayer) bool {
	if payer.PayerType == "bpjs" || payer.PayerType == "self_pay" {
		c.JSON(http.StatusConflict, gin.H{"error": "this payer type has no plans"})
		return false
	}
	return true
}

// requireBPJSPayer rejects non-BPJS payers on the item coverage endpoints.
func requireBPJSPayer(c *gin.Context, payer sqlcgen.CorePayer) bool {
	if payer.PayerType != "bpjs" {
		c.JSON(http.StatusConflict, gin.H{"error": "item coverage applies to the BPJS payer only"})
		return false
	}
	return true
}

// loadPlanAuthorized fetches the plan (404 when missing) and checks
// PermTariffManage against its actual merchant.
func loadPlanAuthorized(c *gin.Context, rawID string) (sqlcgen.CorePayerPlan, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CorePayerPlan{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	plan, err := q.GetPayerPlanByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "payer plan not found"})
		return sqlcgen.CorePayerPlan{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePayerPlan{}, false
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, plan.MerchantID) {
		return sqlcgen.CorePayerPlan{}, false
	}
	return plan, true
}

// planJSON renders a plan with its rules and limits; amounts and percents are
// 2-decimal decimal strings.
func planJSON(c *gin.Context, q *sqlcgen.Queries, plan sqlcgen.CorePayerPlan) (gin.H, bool) {
	rules, err := q.ListPlanRules(c.Request.Context(), plan.ID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	limits, err := q.ListPlanLimits(c.Request.Context(), plan.ID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	var annual any
	if plan.AnnualLimit.Valid {
		annual = numStr2(plan.AnnualLimit)
	}
	rulesOut := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		var copayMax, visitLimit any
		if r.CopayMaxPerClaim.Valid {
			copayMax = numStr2(r.CopayMaxPerClaim)
		}
		if r.VisitLimit.Valid {
			visitLimit = numStr2(r.VisitLimit)
		}
		rulesOut = append(rulesOut, gin.H{
			"care_type":            r.CareType,
			"copay_percent":        numStr2(r.CopayPercent),
			"copay_max_per_claim":  copayMax,
			"deductible_per_claim": numStr2(r.DeductiblePerClaim),
			"visit_limit":          visitLimit,
		})
	}
	limitsOut := make([]gin.H, 0, len(limits))
	for _, l := range limits {
		limitsOut = append(limitsOut, gin.H{"item_type": l.ItemType, "limit_per_visit": numStr2(l.LimitPerVisit)})
	}
	return gin.H{
		"id":                    plan.ID.String(),
		"payer_id":              plan.PayerID.String(),
		"code":                  plan.Code,
		"name":                  plan.Name,
		"requires_gl":           plan.RequiresGl,
		"annual_limit":          annual,
		"plan_year_start_month": plan.PlanYearStartMonth,
		"is_active":             plan.IsActive,
		"row_version":           plan.RowVersion,
		"rules":                 rulesOut,
		"limits":                limitsOut,
	}, true
}

// Payer service rules ---------------------------------------------------------

type serviceRuleInput struct {
	ServiceItemID   string `json:"service_item_id" binding:"required"`
	RuleType        string `json:"rule_type" binding:"required"`
	DiscountPercent string `json:"discount_percent"`
	DiscountAmount  string `json:"discount_amount"`
}

type putServiceRulesRequest struct {
	Rules []serviceRuleInput `json:"rules"`
}

// GetPayerServiceRulesHandler godoc
// @Summary List the blacklist/discount rules of a payer
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Success 200 {object} apiResponse
// @Router /payers/{id}/service-rules [get]
func GetPayerServiceRulesHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rules, err := q.ListPayerServiceRules(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		var percent, amount any
		if r.DiscountPercent.Valid {
			percent = numStr2(r.DiscountPercent)
		}
		if r.DiscountAmount.Valid {
			amount = numStr2(r.DiscountAmount)
		}
		data = append(data, gin.H{
			"service_item_id":  r.ServiceItemID.String(),
			"rule_type":        r.RuleType,
			"discount_percent": percent,
			"discount_amount":  amount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rules": data}, "meta": gin.H{}})
}

// validateServiceRule parses one rule input; a discount needs exactly one of
// percent (0–100) or amount (> 0), a blacklist carries no value.
func validateServiceRule(c *gin.Context, rule serviceRuleInput) (pgtype.Numeric, pgtype.Numeric, bool) {
	var percent, amount pgtype.Numeric
	if rule.RuleType == "blacklist" {
		if rule.DiscountPercent != "" || rule.DiscountAmount != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "blacklist takes no discount value"})
			return percent, amount, false
		}
		return percent, amount, true
	}
	if rule.RuleType != "discount" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rule_type"})
		return percent, amount, false
	}
	hasPercent := rule.DiscountPercent != ""
	hasAmount := rule.DiscountAmount != ""
	if hasPercent == hasAmount { // both or neither
		c.JSON(http.StatusBadRequest, gin.H{"error": "discount needs a percent or an amount"})
		return percent, amount, false
	}
	if hasPercent {
		bp, err := parsePercentBP(rule.DiscountPercent)
		if err != nil || bp < 0 || bp > 10000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "discount needs a percent or an amount"})
			return percent, amount, false
		}
		percent = centsToNumeric(bp)
		return percent, amount, true
	}
	cents, err := parseCents(rule.DiscountAmount)
	if err != nil || cents <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "discount needs a percent or an amount"})
		return percent, amount, false
	}
	amount = centsToNumeric(cents)
	return percent, amount, true
}

// PutPayerServiceRulesHandler godoc
// @Summary Replace the full service-rule set of a payer
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param request body putServiceRulesRequest true "Service rules"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /payers/{id}/service-rules [put]
func PutPayerServiceRulesHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req putServiceRulesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	seen := make(map[string]bool, len(req.Rules))
	type parsedRule struct {
		itemID pgtype.UUID
		rule   sqlcgen.InsertPayerServiceRuleParams
	}
	parsed := make([]parsedRule, 0, len(req.Rules))
	for _, r := range req.Rules {
		itemID, okItem := parseUUID(r.ServiceItemID)
		if !okItem {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_item_id"})
			return
		}
		if seen[r.ServiceItemID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate service item"})
			return
		}
		seen[r.ServiceItemID] = true
		item, err := q.GetServiceItemByID(c.Request.Context(), itemID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && item.MerchantID != payer.MerchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		percent, amount, okRule := validateServiceRule(c, r)
		if !okRule {
			return
		}
		parsed = append(parsed, parsedRule{itemID: itemID, rule: sqlcgen.InsertPayerServiceRuleParams{
			CompanyID: AuthCompanyID(c), MerchantID: payer.MerchantID, PayerID: payer.ID,
			ServiceItemID: itemID, RuleType: r.RuleType,
			DiscountPercent: percent, DiscountAmount: amount, CreatedBy: AuthUserID(c),
		}})
	}
	if err := q.DeletePayerServiceRules(c.Request.Context(), payer.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, p := range parsed {
		if err := q.InsertPayerServiceRule(c.Request.Context(), p.rule); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	rules, err := q.ListPayerServiceRules(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		var percent, amount any
		if r.DiscountPercent.Valid {
			percent = numStr2(r.DiscountPercent)
		}
		if r.DiscountAmount.Valid {
			amount = numStr2(r.DiscountAmount)
		}
		data = append(data, gin.H{
			"service_item_id":  r.ServiceItemID.String(),
			"rule_type":        r.RuleType,
			"discount_percent": percent,
			"discount_amount":  amount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rules": data}, "meta": gin.H{}})
}

// Payer plans -----------------------------------------------------------------

type planInput struct {
	Code               string `json:"code" binding:"required"`
	Name               string `json:"name" binding:"required"`
	RequiresGl         bool   `json:"requires_gl"`
	AnnualLimit        string `json:"annual_limit"`
	PlanYearStartMonth *int16 `json:"plan_year_start_month"`
	IsActive           *bool  `json:"is_active"`
}

// parseAnnualLimit parses an optional annual-limit money string (> 0).
func parseAnnualLimit(c *gin.Context, raw string) (pgtype.Numeric, bool) {
	var n pgtype.Numeric
	if raw == "" {
		return n, true
	}
	cents, err := parseCents(raw)
	if err != nil || cents <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid annual_limit"})
		return n, false
	}
	return centsToNumeric(cents), true
}

// ListPayerPlansHandler godoc
// @Summary List the plans of a payer (with rules and limits)
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/plans [get]
func ListPayerPlansHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requirePlanCapablePayer(c, payer) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	plans, err := q.ListPayerPlans(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(plans))
	for _, plan := range plans {
		out, okPlan := planJSON(c, q, plan)
		if !okPlan {
			return
		}
		data = append(data, out)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// CreatePayerPlanHandler godoc
// @Summary Create a plan under a payer
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param request body planInput true "Plan data"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/plans [post]
func CreatePayerPlanHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requirePlanCapablePayer(c, payer) {
		return
	}
	var req planInput
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	annual, okLimit := parseAnnualLimit(c, req.AnnualLimit)
	if !okLimit {
		return
	}
	month := int16(1)
	if req.PlanYearStartMonth != nil {
		month = *req.PlanYearStartMonth
	}
	if month < 1 || month > 12 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_year_start_month"})
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	q := sqlcgen.New(TxFromContext(c))
	plan, err := q.CreatePayerPlan(c.Request.Context(), sqlcgen.CreatePayerPlanParams{
		CompanyID: AuthCompanyID(c), MerchantID: payer.MerchantID, PayerID: payer.ID,
		Code: req.Code, Name: req.Name, RequiresGl: req.RequiresGl, AnnualLimit: annual,
		PlanYearStartMonth: month, IsActive: isActive, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "plan code already exists"})
			return
		}
		respondInternalError(c, err)
		return
	}
	out, okPlan := planJSON(c, q, plan)
	if !okPlan {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": out, "meta": gin.H{}})
}

type updatePayerPlanRequest struct {
	Code               *string `json:"code"`
	Name               *string `json:"name"`
	RequiresGl         *bool   `json:"requires_gl"`
	AnnualLimit        *string `json:"annual_limit"`
	PlanYearStartMonth *int16  `json:"plan_year_start_month"`
	IsActive           *bool   `json:"is_active"`
	RowVersion         *int64  `json:"row_version" binding:"required"`
}

// UpdatePayerPlanHandler godoc
// @Summary Update a payer plan
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer plan UUID"
// @Param request body updatePayerPlanRequest true "Plan data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payer-plans/{id} [patch]
func UpdatePayerPlanHandler(c *gin.Context) {
	plan, ok := loadPlanAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req updatePayerPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion == nil || *req.RowVersion != int64(plan.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "row version mismatch"})
		return
	}
	code, name := plan.Code, plan.Name
	if req.Code != nil {
		code = *req.Code
	}
	if req.Name != nil {
		name = *req.Name
	}
	requiresGl := plan.RequiresGl
	if req.RequiresGl != nil {
		requiresGl = *req.RequiresGl
	}
	annual := plan.AnnualLimit
	if req.AnnualLimit != nil {
		parsed, okLimit := parseAnnualLimit(c, *req.AnnualLimit)
		if !okLimit {
			return
		}
		annual = parsed
	}
	month := plan.PlanYearStartMonth
	if req.PlanYearStartMonth != nil {
		month = *req.PlanYearStartMonth
	}
	if month < 1 || month > 12 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_year_start_month"})
		return
	}
	isActive := plan.IsActive
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	q := sqlcgen.New(TxFromContext(c))
	updated, err := q.UpdatePayerPlan(c.Request.Context(), sqlcgen.UpdatePayerPlanParams{
		ID: plan.ID, Code: code, Name: name, RequiresGl: requiresGl, AnnualLimit: annual,
		PlanYearStartMonth: month, IsActive: isActive, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "plan code already exists"})
			return
		}
		respondInternalError(c, err)
		return
	}
	out, okPlan := planJSON(c, q, updated)
	if !okPlan {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}

// DeletePayerPlanHandler godoc
// @Summary Soft-delete a payer plan
// @Description Rejected with 409 while active patient coverages point at the plan.
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer plan UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payer-plans/{id} [delete]
func DeletePayerPlanHandler(c *gin.Context) {
	plan, ok := loadPlanAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	coverages, err := q.CountPlanCoverages(c.Request.Context(), plan.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if coverages > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "plan is used by patient coverages"})
		return
	}
	if err := q.SoftDeletePayerPlan(c.Request.Context(), sqlcgen.SoftDeletePayerPlanParams{
		ID: plan.ID, DeletedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": plan.ID.String(), "deleted": true}, "meta": gin.H{}})
}

// Plan care rules and per-visit limits (full-set replace) ----------------------

type planRuleInput struct {
	CareType           string `json:"care_type" binding:"required"`
	CopayPercent       string `json:"copay_percent" binding:"required"`
	CopayMaxPerClaim   string `json:"copay_max_per_claim"`
	DeductiblePerClaim string `json:"deductible_per_claim"`
	VisitLimit         string `json:"visit_limit"`
}

type putPlanRulesRequest struct {
	Rules []planRuleInput `json:"rules"`
}

type planLimitInput struct {
	ItemType      string `json:"item_type" binding:"required"`
	LimitPerVisit string `json:"limit_per_visit" binding:"required"`
}

type putPlanLimitsRequest struct {
	Limits []planLimitInput `json:"limits"`
}

// respondPlanChange renders the updated plan (with rules + limits) after a
// full-set replace; returns false when a DB error was already responded.
func respondPlanChange(c *gin.Context, q *sqlcgen.Queries, planID pgtype.UUID) bool {
	plan, err := q.GetPayerPlanByID(c.Request.Context(), planID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	out, ok := planJSON(c, q, plan)
	if !ok {
		return false
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
	return true
}

// PutPayerPlanRulesHandler godoc
// @Summary Replace the care-type rules of a payer plan
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer plan UUID"
// @Param request body putPlanRulesRequest true "Care rules"
// @Success 200 {object} apiResponse
// @Router /payer-plans/{id}/rules [put]
func PutPayerPlanRulesHandler(c *gin.Context) {
	plan, ok := loadPlanAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req putPlanRulesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	seen := make(map[string]bool, len(req.Rules))
	rows := make([]sqlcgen.InsertPlanRuleParams, 0, len(req.Rules))
	for _, r := range req.Rules {
		if !planCareTypes[r.CareType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid care_type"})
			return
		}
		if seen[r.CareType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate care_type"})
			return
		}
		seen[r.CareType] = true
		bp, err := parsePercentBP(r.CopayPercent)
		if err != nil || bp < 0 || bp > 10000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "copay_percent must be between 0 and 100"})
			return
		}
		var copayMax, deductible, visitLimit pgtype.Numeric
		if r.CopayMaxPerClaim != "" {
			cents, errC := parseCents(r.CopayMaxPerClaim)
			if errC != nil || cents < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid copay_max_per_claim"})
				return
			}
			copayMax = centsToNumeric(cents)
		}
		if r.DeductiblePerClaim != "" {
			cents, errC := parseCents(r.DeductiblePerClaim)
			if errC != nil || cents < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deductible_per_claim"})
				return
			}
			deductible = centsToNumeric(cents)
		}
		if r.VisitLimit != "" {
			cents, errC := parseCents(r.VisitLimit)
			if errC != nil || cents <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "visit_limit must be greater than 0"})
				return
			}
			visitLimit = centsToNumeric(cents)
		}
		rows = append(rows, sqlcgen.InsertPlanRuleParams{
			CompanyID: AuthCompanyID(c), MerchantID: plan.MerchantID, PlanID: plan.ID,
			CareType: r.CareType, CopayPercent: centsToNumeric(bp),
			CopayMaxPerClaim: copayMax, DeductiblePerClaim: deductible, VisitLimit: visitLimit,
		})
	}
	if err := q.DeletePlanRules(c.Request.Context(), plan.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, row := range rows {
		if err := q.InsertPlanRule(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	respondPlanChange(c, q, plan.ID)
}

// PutPayerPlanLimitsHandler godoc
// @Summary Replace the per-visit item limits of a payer plan
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer plan UUID"
// @Param request body putPlanLimitsRequest true "Inner limits"
// @Success 200 {object} apiResponse
// @Router /payer-plans/{id}/limits [put]
func PutPayerPlanLimitsHandler(c *gin.Context) {
	plan, ok := loadPlanAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req putPlanLimitsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	seen := make(map[string]bool, len(req.Limits))
	rows := make([]sqlcgen.InsertPlanLimitParams, 0, len(req.Limits))
	for _, l := range req.Limits {
		if !serviceItemTypes[l.ItemType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_type"})
			return
		}
		if seen[l.ItemType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate item_type"})
			return
		}
		seen[l.ItemType] = true
		cents, err := parseCents(l.LimitPerVisit)
		if err != nil || cents <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit_per_visit must be greater than 0"})
			return
		}
		rows = append(rows, sqlcgen.InsertPlanLimitParams{
			CompanyID: AuthCompanyID(c), MerchantID: plan.MerchantID, PlanID: plan.ID,
			ItemType: l.ItemType, LimitPerVisit: centsToNumeric(cents),
		})
	}
	if err := q.DeletePlanLimits(c.Request.Context(), plan.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, row := range rows {
		if err := q.InsertPlanLimit(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	respondPlanChange(c, q, plan.ID)
}

// BPJS item coverage ------------------------------------------------------------

type coverageRuleInput struct {
	ServiceItemID string `json:"service_item_id"`
	ItemType      string `json:"item_type"`
	Coverage      string `json:"coverage" binding:"required"`
	ClaimType     string `json:"claim_type"`
}

type putItemCoverageRequest struct {
	Rules []coverageRuleInput `json:"rules"`
}

// coverageRuleJSON renders one item-coverage row (snake_case, nullable refs).
func coverageRuleJSON(r sqlcgen.CorePayerItemCoverage) gin.H {
	var itemID, itemType, claimType any
	if r.ServiceItemID.Valid {
		itemID = r.ServiceItemID.String()
	}
	if r.ItemType.Valid {
		itemType = r.ItemType.String
	}
	if r.ClaimType.Valid {
		claimType = r.ClaimType.String
	}
	return gin.H{
		"service_item_id": itemID,
		"item_type":       itemType,
		"coverage":        r.Coverage,
		"claim_type":      claimType,
	}
}

// GetPayerItemCoverageHandler godoc
// @Summary List the BPJS item coverage of a payer
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/item-coverage [get]
func GetPayerItemCoverageHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requireBPJSPayer(c, payer) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rules, err := q.ListPayerItemCoverage(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		data = append(data, coverageRuleJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rules": data}, "meta": gin.H{}})
}

// PutPayerItemCoverageHandler godoc
// @Summary Replace the full BPJS item-coverage set of a payer
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param request body putItemCoverageRequest true "Coverage rules"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/item-coverage [put]
func PutPayerItemCoverageHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requireBPJSPayer(c, payer) {
		return
	}
	var req putItemCoverageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	seenItem := make(map[string]bool, len(req.Rules))
	seenType := make(map[string]bool, len(req.Rules))
	rows := make([]sqlcgen.InsertPayerItemCoverageParams, 0, len(req.Rules))
	for _, r := range req.Rules {
		hasItem := r.ServiceItemID != ""
		hasType := r.ItemType != ""
		if hasItem == hasType { // both or neither
			c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one of service_item_id or item_type is required"})
			return
		}
		if !coverageValues[r.Coverage] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid coverage"})
			return
		}
		if (r.Coverage == "claim") != (r.ClaimType != "") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "claim_type is required for claim coverage"})
			return
		}
		if r.ClaimType != "" && !claimTypes[r.ClaimType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid claim_type"})
			return
		}
		row := sqlcgen.InsertPayerItemCoverageParams{
			CompanyID: AuthCompanyID(c), MerchantID: payer.MerchantID, PayerID: payer.ID,
			Coverage: r.Coverage,
		}
		if hasItem {
			itemID, okItem := parseUUID(r.ServiceItemID)
			if !okItem {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_item_id"})
				return
			}
			if seenItem[r.ServiceItemID] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate coverage target"})
				return
			}
			seenItem[r.ServiceItemID] = true
			item, err := q.GetServiceItemByID(c.Request.Context(), itemID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && item.MerchantID != payer.MerchantID) {
				c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
				return
			}
			if err != nil {
				respondInternalError(c, err)
				return
			}
			row.ServiceItemID = pgtype.UUID{Bytes: itemID.Bytes, Valid: true}
		} else {
			if !serviceItemTypes[r.ItemType] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_type"})
				return
			}
			if seenType[r.ItemType] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate coverage target"})
				return
			}
			seenType[r.ItemType] = true
			row.ItemType = pgtype.Text{String: r.ItemType, Valid: true}
		}
		if r.ClaimType != "" {
			row.ClaimType = pgtype.Text{String: r.ClaimType, Valid: true}
		}
		rows = append(rows, row)
	}
	if err := q.DeletePayerItemCoverage(c.Request.Context(), payer.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, row := range rows {
		if err := q.InsertPayerItemCoverage(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	rules, err := q.ListPayerItemCoverage(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		data = append(data, coverageRuleJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"rules": data}, "meta": gin.H{}})
}

// claimTypeForTemplateCode maps a template item code to its BPJS claim type;
// the first matching literal prefix wins.
func claimTypeForTemplateCode(code string) string {
	prefixes := []struct{ prefix, claimType string }{
		{"ANC-", "anc"},
		{"PASCA-PERSALINAN", "pnc"},
		{"PNC", "pnc"},
		{"PERSALINAN-", "delivery"},
		{"KB-", "kb"},
		{"PRARUJUK", "pre_referral"},
		{"PRB-", "prb"},
		{"AMBULANS", "ambulance"},
		{"SKR-", "screening"},
		{"RANAP-", "first_inpatient"},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(code, p.prefix) {
			return p.claimType
		}
	}
	return "other"
}

// PostPayerItemCoverageFromTemplateHandler godoc
// @Summary Seed BPJS claim coverage rules from the non-capitation template
// @Description For every merchant service item whose code exists in the
// @Description bpjs_nonkapitasi_2023 template, insert a claim rule with the
// @Description claim type derived from the code prefix; items that already
// @Description have a coverage rule are skipped.
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/item-coverage/from-template [post]
func PostPayerItemCoverageFromTemplateHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if !requireBPJSPayer(c, payer) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	items, err := q.ListMerchantItemsInBPJSTemplate(c.Request.Context(), payer.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	existing, err := q.ListPayerItemCoverage(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	hasRule := make(map[string]bool, len(existing))
	for _, r := range existing {
		if r.ServiceItemID.Valid {
			hasRule[r.ServiceItemID.String()] = true
		}
	}
	var created, skipped int64
	for _, item := range items {
		if hasRule[item.ID.String()] {
			skipped++
			continue
		}
		if err := q.InsertPayerItemCoverage(c.Request.Context(), sqlcgen.InsertPayerItemCoverageParams{
			CompanyID: AuthCompanyID(c), MerchantID: payer.MerchantID, PayerID: payer.ID,
			ServiceItemID: item.ID, Coverage: "claim",
			ClaimType: pgtype.Text{String: claimTypeForTemplateCode(item.Code), Valid: true},
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		created++
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"created": created, "skipped": skipped}, "meta": gin.H{}})
}

// Merchant billing (facility) profile -------------------------------------------

type updateBillingProfileRequest struct {
	FacilityType       *string `json:"facility_type"`
	BpjsCooperation    *string `json:"bpjs_cooperation"`
	BpjsNetworkParent  *string `json:"bpjs_network_parent"`
	DefaultPriceListID *string `json:"default_price_list_id"`
}

// billingProfileJSON renders the facility/BPJS profile subset of a merchant.
func billingProfileJSON(m sqlcgen.CoreMerchant) gin.H {
	var facilityType, networkParent, priceListID any
	if m.FacilityType.Valid {
		facilityType = m.FacilityType.String
	}
	if m.BpjsNetworkParent.Valid {
		networkParent = m.BpjsNetworkParent.String
	}
	if m.DefaultPriceListID.Valid {
		priceListID = m.DefaultPriceListID.String()
	}
	return gin.H{
		"facility_type":         facilityType,
		"bpjs_cooperation":      m.BpjsCooperation,
		"bpjs_network_parent":   networkParent,
		"default_price_list_id": priceListID,
	}
}

// UpdateMerchantBillingProfileHandler godoc
// @Summary Update the facility / BPJS billing profile of a merchant
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body updateBillingProfileRequest true "Billing profile"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id}/billing-profile [patch]
func UpdateMerchantBillingProfileHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	var req updateBillingProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.GetMerchantByID(c.Request.Context(), merchantID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	facilityType := merchant.FacilityType
	if req.FacilityType != nil {
		if !facilityTypes[*req.FacilityType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid facility_type"})
			return
		}
		if *req.FacilityType == "" {
			facilityType = pgtype.Text{}
		} else {
			facilityType = pgtype.Text{String: *req.FacilityType, Valid: true}
		}
	}
	cooperation := merchant.BpjsCooperation
	if req.BpjsCooperation != nil {
		if !bpjsCooperationTypes[*req.BpjsCooperation] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bpjs_cooperation"})
			return
		}
		cooperation = *req.BpjsCooperation
	}
	networkParent := merchant.BpjsNetworkParent
	if req.BpjsNetworkParent != nil {
		if *req.BpjsNetworkParent == "" {
			networkParent = pgtype.Text{}
		} else {
			networkParent = pgtype.Text{String: *req.BpjsNetworkParent, Valid: true}
		}
	}
	if cooperation == "fktp_network" && !networkParent.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "network FKTP needs the parent FKTP"})
		return
	}
	priceListID := merchant.DefaultPriceListID
	if req.DefaultPriceListID != nil {
		parsed, okList := resolvePayerPriceList(c, q, merchantID, *req.DefaultPriceListID)
		if !okList {
			return
		}
		priceListID = parsed
	}
	updated, err := q.UpdateMerchantBillingProfile(c.Request.Context(), sqlcgen.UpdateMerchantBillingProfileParams{
		ID: merchantID, FacilityType: facilityType, BpjsCooperation: cooperation,
		BpjsNetworkParent: networkParent, DefaultPriceListID: priceListID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			c.JSON(http.StatusConflict, gin.H{"error": pgErr.Message})
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": billingProfileJSON(updated), "meta": gin.H{}})
}
