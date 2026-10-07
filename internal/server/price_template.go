// internal/server/price_template.go
// Platform price templates: list the BPJS non-capitation template and apply it
// into a merchant as a standalone price list (spec
// 2026-10-07-tariff-price-lists §6).
package server

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// templatePriceScopes are the two care settings of core.template_price_item.
var templatePriceScopes = map[string]bool{"fktp": true, "bidan_jejaring": true}

// amountOrNil renders a template numeric bound as a 2-decimal string, or nil.
func amountOrNil(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	cents, err := numericToCents(n)
	if err != nil {
		return nil
	}
	return formatCents(cents)
}

// ListPriceTemplatesHandler godoc
// @Summary List active price-list templates with their items
// @Tags tariff
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /price-list-templates [get]
func ListPriceTemplatesHandler(c *gin.Context) {
	if !RequirePermission(c, PermTariffManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	templates, err := q.ListPriceTemplates(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(templates))
	for _, t := range templates {
		items, err := q.ListTemplatePriceItems(ctx, t.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		itemViews := make([]gin.H, 0, len(items))
		for _, it := range items {
			scopes := it.Scopes
			if scopes == nil {
				scopes = []string{}
			}
			itemViews = append(itemViews, gin.H{
				"code":       it.Code,
				"name":       it.Name,
				"item_type":  it.ItemType,
				"value_kind": it.ValueKind,
				"amount_min": amountOrNil(it.AmountMin),
				"amount_max": amountOrNil(it.AmountMax),
				"scopes":     scopes,
				"legal_ref":  it.LegalRef,
				"note":       textOrNil(it.Note),
			})
		}
		data = append(data, gin.H{
			"id":          uuidOrNil(t.ID),
			"code":        t.Code,
			"name":        t.Name,
			"description": textOrNil(t.Description),
			"version":     t.Version,
			"items":       itemViews,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

type applyPriceTemplateRequest struct {
	MerchantID    string            `json:"merchant_id" binding:"required"`
	PriceListCode string            `json:"price_list_code" binding:"required"`
	PriceListName string            `json:"price_list_name" binding:"required"`
	Scope         string            `json:"scope" binding:"required"`
	EffectiveFrom string            `json:"effective_from" binding:"required"`
	Values        map[string]string `json:"values"`
}

// scopedPriceItem pairs a template row with the value chosen for the scope.
type scopedPriceItem struct {
	item  sqlcgen.CoreTemplatePriceItem
	cents int64
}

// validateTemplateValues checks every scoped row against the request values
// BEFORE any write (spec §6 step 2). Unknown keys are rejected as well.
func validateTemplateValues(c *gin.Context, items []sqlcgen.CoreTemplatePriceItem, values map[string]string, scope string) ([]scopedPriceItem, bool) {
	byCode := make(map[string]sqlcgen.CoreTemplatePriceItem, len(items))
	scoped := make([]scopedPriceItem, 0, len(items))
	for _, it := range items {
		if !slices.Contains(it.Scopes, scope) {
			continue
		}
		byCode[it.Code] = it
	}
	for code := range values {
		if _, ok := byCode[code]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown template item " + code})
			return nil, false
		}
	}
	for _, it := range items {
		if !slices.Contains(it.Scopes, scope) {
			continue
		}
		raw, provided := values[it.Code]
		// NULL bounds (max/regional rows) read as "not available".
		bound := func(n pgtype.Numeric) (int64, bool) {
			if !n.Valid {
				return 0, false
			}
			cents, err := numericToCents(n)
			if err != nil {
				return 0, false
			}
			return cents, true
		}
		minCents, hasMin := bound(it.AmountMin)
		maxCents, hasMax := bound(it.AmountMax)
		switch it.ValueKind {
		case "fixed":
			// Absent means the fixed tariff; a present value must match it.
			if provided {
				v, err := parseCents(raw)
				if err != nil || !hasMin || v != minCents {
					c.JSON(http.StatusBadRequest, gin.H{"error": "fixed tariff " + it.Code + " cannot be changed"})
					return nil, false
				}
			}
			scoped = append(scoped, scopedPriceItem{item: it, cents: minCents})
		case "range":
			v, err := parseCents(raw)
			if err != nil || !hasMin || !hasMax || v < minCents || v > maxCents {
				c.JSON(http.StatusBadRequest, gin.H{"error": it.Code + " must be between " + formatCents(minCents) + " and " + formatCents(maxCents)})
				return nil, false
			}
			scoped = append(scoped, scopedPriceItem{item: it, cents: v})
		case "max":
			if !provided || raw == "" {
				scoped = append(scoped, scopedPriceItem{item: it, cents: maxCents})
				continue
			}
			v, err := parseCents(raw)
			if err != nil || !hasMax || v <= 0 || v > maxCents {
				c.JSON(http.StatusBadRequest, gin.H{"error": it.Code + " must be greater than 0 and at most " + formatCents(maxCents)})
				return nil, false
			}
			scoped = append(scoped, scopedPriceItem{item: it, cents: v})
		case "regional":
			v, err := parseCents(raw)
			if err != nil || v <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": it.Code + " needs the regional tariff"})
				return nil, false
			}
			scoped = append(scoped, scopedPriceItem{item: it, cents: v})
		}
	}
	return scoped, true
}

// ApplyPriceListTemplateHandler godoc
// @Summary Apply a price-list template into a standalone merchant price list
// @Tags tariff
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Template ID"
// @Param request body applyPriceTemplateRequest true "Apply parameters"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /price-list-templates/{id}/apply [post]
func ApplyPriceListTemplateHandler(c *gin.Context) {
	templateID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req applyPriceTemplateRequest
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
	if !templatePriceScopes[req.Scope] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be fktp or bidan_jejaring"})
		return
	}
	effFrom, okDate := parseAPIDate(req.EffectiveFrom)
	if !okDate {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	// No price version may start in the merchant's past (review fix, Task 4).
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	today := localDayStart(time.Now(), loc)
	if effFrom.Format("2006-01-02") < today.Format("2006-01-02") {
		c.JSON(http.StatusConflict, gin.H{"error": "effective_from cannot be in the past"})
		return
	}
	// The template must be an active price_list kind (404 otherwise).
	templates, err := q.ListPriceTemplates(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var template *sqlcgen.CoreTemplate
	for i := range templates {
		if templates[i].ID == templateID {
			template = &templates[i]
			break
		}
	}
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
		return
	}
	items, err := q.ListTemplatePriceItems(ctx, template.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	scoped, okVals := validateTemplateValues(c, items, req.Values, req.Scope)
	if !okVals {
		return
	}

	// Standard claim component (created if missing).
	if _, err := q.CreateRateComponentIfMissing(ctx, sqlcgen.CreateRateComponentIfMissingParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: "KLAIM", Name: "Tarif Klaim", CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	components, err := q.ListRateComponentsByMerchant(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var klaimID pgtype.UUID
	for _, comp := range components {
		if comp.Code == "KLAIM" {
			klaimID = comp.ID
			break
		}
	}

	list, err := q.CreatePriceList(ctx, sqlcgen.CreatePriceListParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		Code: req.PriceListCode, Name: req.PriceListName,
		AdjustmentPercent: centsToNumeric(0), RoundingUnit: centsToNumeric(0),
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondPriceListError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	effDate := pgtype.Date{Time: effFrom, Valid: true}
	created := []string{}
	reused := []string{}
	for _, sc := range scoped {
		item, err := q.GetServiceItemByMerchantCode(ctx, sqlcgen.GetServiceItemByMerchantCodeParams{
			MerchantID: merchantID, Code: sc.item.Code,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			item, err = q.CreateServiceItem(ctx, sqlcgen.CreateServiceItemParams{
				CompanyID: AuthCompanyID(c), MerchantID: merchantID,
				Code: sc.item.Code, Name: sc.item.Name, ItemType: sc.item.ItemType,
				CreatedBy: AuthUserID(c),
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			created = append(created, sc.item.Code)
		case err != nil:
			respondInternalError(c, err)
			return
		case item.ItemType != sc.item.ItemType:
			c.JSON(http.StatusConflict, gin.H{"error": "item " + sc.item.Code + " exists with type " + item.ItemType})
			return
		default:
			reused = append(reused, sc.item.Code)
		}
		if err := q.InsertPriceRow(ctx, sqlcgen.InsertPriceRowParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID,
			ServiceItemID: item.ID, RateComponentID: klaimID, PriceListID: list.ID,
			Amount: centsToNumeric(sc.cents), EffectiveFrom: effDate, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if err := q.InsertTemplateApplication(ctx, sqlcgen.InsertTemplateApplicationParams{
		CompanyID: AuthCompanyID(c), TemplateID: template.ID, TemplateVersion: template.Version,
		AppliedBy: AuthUserID(c), RolesCreated: created, RolesSkipped: reused,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"price_list": priceListJSON(c, list),
		"created":    created,
		"reused":     reused,
	}})
}
