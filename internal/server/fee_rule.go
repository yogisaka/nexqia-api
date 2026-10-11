// internal/server/fee_rule.go
// Practitioner fee rule CRUD (core.practitioner_fee_rule, migration 000070;
// spec 2026-10-07-tariff-d-practitioner-fees-tax §3/§4.1).
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermFeeManage guards practitioner fee rules, fee payouts and PPh 21
// (permission billing.fee.manage, migration 000070).
const PermFeeManage = "billing.fee.manage"

// RegisterFeeRoutes registers the fee rule endpoints under the authenticated
// router group (wired in server.go right after RegisterReceivableRoutes).
func RegisterFeeRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/fee-rules", ListFeeRulesHandler)
	rg.POST("/merchants/:id/fee-rules", CreateFeeRuleHandler)
	rg.PATCH("/fee-rules/:id", UpdateFeeRuleHandler)
	rg.DELETE("/fee-rules/:id", DeleteFeeRuleHandler)
}

// feeRuleSpecificity scores a rule per spec §4.1 WITHOUT the payer/performer
// bonus — display only. Actual rule selection (with the bonus) lives in
// fee_compute.go.
func feeRuleSpecificity(r sqlcgen.CorePractitionerFeeRule) int {
	switch {
	case r.PhysicianID.Valid && r.ServiceItemID.Valid:
		return 50
	case r.PhysicianID.Valid && r.ItemType.Valid:
		return 40
	case r.PhysicianID.Valid:
		return 30
	case r.ServiceItemID.Valid:
		return 20
	case r.ItemType.Valid:
		return 10
	default:
		return 0
	}
}

// numericJSON renders an optional numeric as "12.34" (nil when NULL).
func numericJSON(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	return numStr2(n)
}

// dateText renders an optional date as YYYY-MM-DD (nil when NULL).
func dateText(d pgtype.Date) any {
	if !d.Valid {
		return nil
	}
	return d.Time.Format("2006-01-02")
}

// feeRuleJSON maps a raw sqlcgen.CorePractitionerFeeRule to a snake_case JSON
// object plus the §4.1 specificity score.
func feeRuleJSON(r sqlcgen.CorePractitionerFeeRule) gin.H {
	return gin.H{
		"id":              uuidJSON(r.ID),
		"company_id":      uuidJSON(r.CompanyID),
		"merchant_id":     uuidJSON(r.MerchantID),
		"physician_id":    uuidJSON(r.PhysicianID),
		"service_item_id": uuidJSON(r.ServiceItemID),
		"item_type":       textJSON(r.ItemType),
		"payer_id":        uuidJSON(r.PayerID),
		"performer_role":  textJSON(r.PerformerRole),
		"basis":           r.Basis,
		"component_code":  textJSON(r.ComponentCode),
		"percent":         numericJSON(r.Percent),
		"amount":          numericJSON(r.Amount),
		"effective_from":  dateText(r.EffectiveFrom),
		"effective_to":    dateText(r.EffectiveTo),
		"created_at":      tsJSON(r.CreatedAt),
		"created_by":      uuidJSON(r.CreatedBy),
		"updated_at":      tsJSON(r.UpdatedAt),
		"updated_by":      uuidJSON(r.UpdatedBy),
		"row_version":     r.RowVersion,
		"specificity":     feeRuleSpecificity(r),
	}
}

// feeRuleFields is the shared rule body of create and update. Reference
// fields are optional pointers: nil/empty means "keep existing" on PATCH and
// "NULL" on POST.
type feeRuleFields struct {
	PhysicianID   *string `json:"physician_id"`
	ServiceItemID *string `json:"service_item_id"`
	ItemType      *string `json:"item_type"`
	PayerID       *string `json:"payer_id"`
	PerformerRole *string `json:"performer_role"`
	Basis         string  `json:"basis"`
	ComponentCode *string `json:"component_code"`
	Percent       *string `json:"percent"`
	Amount        *string `json:"amount"`
	EffectiveFrom string  `json:"effective_from"`
}

// optUUIDField parses an optional UUID reference (empty/nil → zero UUID).
func optUUIDField(s *string) (pgtype.UUID, bool) {
	if s == nil || *s == "" {
		return pgtype.UUID{}, true
	}
	id, ok := parseUUID(*s)
	if !ok {
		return pgtype.UUID{}, false
	}
	return id, true
}

// optTextField returns the referenced string, or "" when absent.
func optTextField(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// optNumericField parses an optional numeric string ("50.00").
func optNumericField(s *string) (pgtype.Numeric, bool) {
	if s == nil || *s == "" {
		return pgtype.Numeric{}, true
	}
	var n pgtype.Numeric
	if err := n.Scan(*s); err != nil {
		return pgtype.Numeric{}, false
	}
	return n, true
}

// respondFeeRuleError maps fee rule insert/update Postgres errors to the
// contract 400 message "invalid fee rule: <constraint>"; returns false when
// err is not a constraint violation.
func respondFeeRuleError(c *gin.Context, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "23514", "23505", "23503":
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid fee rule: " + pgErr.ConstraintName})
		return true
	}
	return false
}

// checkFeeRuleRef404 validates that an optional reference (physician, service
// item, payer) exists AND belongs to the merchant — anything else is a 404.
func checkFeeRuleRef404(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, kind string, raw *string) bool {
	if raw == nil || *raw == "" {
		return true
	}
	id, ok := parseUUID(*raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + kind})
		return false
	}
	var owner pgtype.UUID
	switch kind {
	case "physician_id":
		row, err := q.GetPhysicianByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "physician not found"})
			return false
		}
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		owner = row.MerchantID
	case "service_item_id":
		row, err := q.GetServiceItemByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "service item not found"})
			return false
		}
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		owner = row.MerchantID
	case "payer_id":
		payerOwner, err := q.GetFeeRulePayerMerchant(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
			return false
		}
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		owner = payerOwner
	}
	if owner != merchantID {
		c.JSON(http.StatusNotFound, gin.H{"error": kind + " not found"})
		return false
	}
	return true
}

// loadFeeRuleAuthorized loads a non-deleted fee rule and enforces
// PermFeeManage against its merchant; writes the error response itself.
func loadFeeRuleAuthorized(c *gin.Context, rawID string) (sqlcgen.CorePractitionerFeeRule, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CorePractitionerFeeRule{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	rule, err := q.GetFeeRuleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fee rule not found"})
		return sqlcgen.CorePractitionerFeeRule{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePractitionerFeeRule{}, false
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, rule.MerchantID) {
		return sqlcgen.CorePractitionerFeeRule{}, false
	}
	return rule, true
}

// ListFeeRulesHandler godoc
// @Summary List fee rules of a merchant
// @Tags fee-rule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/fee-rules [get]
func ListFeeRulesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rules, err := q.ListFeeRulesByMerchant(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rules))
	for _, r := range rules {
		data = append(data, feeRuleJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// CreateFeeRuleHandler godoc
// @Summary Create a practitioner fee rule
// @Tags fee-rule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body object true "Fee rule data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id}/fee-rules [post]
func CreateFeeRuleHandler(c *gin.Context) {
	var req feeRuleFields
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if req.Basis == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "basis is required"})
		return
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	physicianID, ok := optUUIDField(req.PhysicianID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid physician_id"})
		return
	}
	itemID, ok := optUUIDField(req.ServiceItemID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service_item_id"})
		return
	}
	payerID, ok := optUUIDField(req.PayerID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payer_id"})
		return
	}
	if !checkFeeRuleRef404(c, q, merchantID, "physician_id", req.PhysicianID) {
		return
	}
	if !checkFeeRuleRef404(c, q, merchantID, "service_item_id", req.ServiceItemID) {
		return
	}
	if !checkFeeRuleRef404(c, q, merchantID, "payer_id", req.PayerID) {
		return
	}
	var effectiveFrom pgtype.Date
	if req.EffectiveFrom != "" {
		t, err := time.Parse("2006-01-02", req.EffectiveFrom)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from"})
			return
		}
		effectiveFrom = pgtype.Date{Time: t, Valid: true}
	} else {
		effectiveFrom = pgtype.Date{Time: time.Now(), Valid: true}
	}
	if _, ok := optNumericField(req.Percent); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid percent"})
		return
	}
	if _, ok := optNumericField(req.Amount); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	percent, _ := optNumericField(req.Percent)
	amount, _ := optNumericField(req.Amount)
	rule, err := q.CreateFeeRule(c.Request.Context(), sqlcgen.CreateFeeRuleParams{
		CompanyID:     AuthCompanyID(c),
		MerchantID:    merchantID,
		PhysicianID:   physicianID,
		ServiceItemID: itemID,
		ItemType:      optTextPtr(req.ItemType),
		PayerID:       payerID,
		PerformerRole: optTextPtr(req.PerformerRole),
		Basis:         req.Basis,
		ComponentCode: optTextPtr(req.ComponentCode),
		Percent:       percent,
		Amount:        amount,
		EffectiveFrom: effectiveFrom,
		CreatedBy:     AuthUserID(c),
	})
	if err != nil {
		if respondFeeRuleError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": feeRuleJSON(rule), "meta": gin.H{}})
}

// feeRuleUpdateParams merges PATCH fields onto the current rule values and
// parses percent/amount/effective_from. Caller has validated references.
func feeRuleUpdateParams(merchantID pgtype.UUID, existing sqlcgen.CorePractitionerFeeRule, req feeRuleFields, updatedBy pgtype.UUID) sqlcgen.UpdateFeeRuleParams {
	physicianID := existing.PhysicianID
	if v, ok := optUUIDField(req.PhysicianID); ok && req.PhysicianID != nil && *req.PhysicianID != "" {
		physicianID = v
	}
	itemID := existing.ServiceItemID
	if v, ok := optUUIDField(req.ServiceItemID); ok && req.ServiceItemID != nil && *req.ServiceItemID != "" {
		itemID = v
	}
	payerID := existing.PayerID
	if v, ok := optUUIDField(req.PayerID); ok && req.PayerID != nil && *req.PayerID != "" {
		payerID = v
	}
	itemType := existing.ItemType
	if v := optTextField(req.ItemType); v != "" {
		itemType = optTextPtr(req.ItemType)
	}
	performerRole := existing.PerformerRole
	if v := optTextField(req.PerformerRole); v != "" {
		performerRole = optTextPtr(req.PerformerRole)
	}
	componentCode := existing.ComponentCode
	if v := optTextField(req.ComponentCode); v != "" {
		componentCode = optTextPtr(req.ComponentCode)
	}
	percent := existing.Percent
	if v, ok := optNumericField(req.Percent); ok && req.Percent != nil && *req.Percent != "" {
		percent = v
	}
	amount := existing.Amount
	if v, ok := optNumericField(req.Amount); ok && req.Amount != nil && *req.Amount != "" {
		amount = v
	}
	effectiveFrom := existing.EffectiveFrom
	if req.EffectiveFrom != "" {
		if t, err := time.Parse("2006-01-02", req.EffectiveFrom); err == nil {
			effectiveFrom = pgtype.Date{Time: t, Valid: true}
		}
	}
	basis := existing.Basis
	if req.Basis != "" {
		basis = req.Basis
	}
	return sqlcgen.UpdateFeeRuleParams{
		ID:            existing.ID,
		PhysicianID:   physicianID,
		ServiceItemID: itemID,
		ItemType:      itemType,
		PayerID:       payerID,
		PerformerRole: performerRole,
		Basis:         basis,
		ComponentCode: componentCode,
		Percent:       percent,
		Amount:        amount,
		EffectiveFrom: effectiveFrom,
		UpdatedBy:     updatedBy,
	}
}

// UpdateFeeRuleHandler godoc
// @Summary Update a fee rule (optimistic locking on row_version)
// @Tags fee-rule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee rule UUID"
// @Param request body object true "Fee rule data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-rules/{id} [patch]
func UpdateFeeRuleHandler(c *gin.Context) {
	existing, ok := loadFeeRuleAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		RowVersion *int64 `json:"row_version" binding:"required"`
		feeRuleFields
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion == nil || *req.RowVersion != int64(existing.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "row version mismatch"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if !checkFeeRuleRef404(c, q, existing.MerchantID, "physician_id", req.PhysicianID) {
		return
	}
	if !checkFeeRuleRef404(c, q, existing.MerchantID, "service_item_id", req.ServiceItemID) {
		return
	}
	if !checkFeeRuleRef404(c, q, existing.MerchantID, "payer_id", req.PayerID) {
		return
	}
	if _, ok := optNumericField(req.Percent); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid percent"})
		return
	}
	if _, ok := optNumericField(req.Amount); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	rule, err := q.UpdateFeeRule(c.Request.Context(), feeRuleUpdateParams(existing.MerchantID, existing, req.feeRuleFields, AuthUserID(c)))
	if err != nil {
		if respondFeeRuleError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": feeRuleJSON(rule), "meta": gin.H{}})
}

// DeleteFeeRuleHandler godoc
// @Summary Soft-delete a fee rule
// @Tags fee-rule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee rule UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /fee-rules/{id} [delete]
func DeleteFeeRuleHandler(c *gin.Context) {
	existing, ok := loadFeeRuleAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.SoftDeleteFeeRule(c.Request.Context(), sqlcgen.SoftDeleteFeeRuleParams{ID: existing.ID, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
