// internal/server/payer.go
// Payer (penjamin) master CRUD (spec 2026-10-07-tariff-b1-billing-core §6);
// the pre-existing read-only list stays untouched (PascalCase sqlc structs
// consumed by PendaftaranPage/masterDataLoader). Service rules, plans and
// item coverage live in payer_plan.go.
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterPayerRoutes wires core.payer listing (pendaftaran form "Jaminan
// Pembayaran" section). No dedicated payer permission exists in the RBAC seed;
// the closest master-data manage permission is core.tariff.manage (tariff.go),
// which already covers payer-linked service rates — reused here rather than
// minting a new permission for a read-only list.
func RegisterPayerRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/payers", ListPayersHandler)
	rg.POST("/merchants/:id/payers", CreatePayerHandler)
	rg.PATCH("/payers/:id", UpdatePayerHandler)
	rg.DELETE("/payers/:id", DeletePayerHandler)
	rg.GET("/payers/:id/service-rules", GetPayerServiceRulesHandler)
	rg.PUT("/payers/:id/service-rules", PutPayerServiceRulesHandler)
	rg.GET("/payers/:id/plans", ListPayerPlansHandler)
	rg.POST("/payers/:id/plans", CreatePayerPlanHandler)
	rg.PATCH("/payer-plans/:id", UpdatePayerPlanHandler)
	rg.DELETE("/payer-plans/:id", DeletePayerPlanHandler)
	rg.PUT("/payer-plans/:id/rules", PutPayerPlanRulesHandler)
	rg.PUT("/payer-plans/:id/limits", PutPayerPlanLimitsHandler)
	rg.GET("/payers/:id/item-coverage", GetPayerItemCoverageHandler)
	rg.PUT("/payers/:id/item-coverage", PutPayerItemCoverageHandler)
	rg.POST("/payers/:id/item-coverage/from-template", PostPayerItemCoverageFromTemplateHandler)
	rg.PATCH("/merchants/:id/billing-profile", UpdateMerchantBillingProfileHandler)
	// Kepesertaan pasien (core.person.manage), visit guarantors and guarantee
	// letters (operations.visit.manage or billing.invoice.create) — handlers
	// live in patient_coverage.go.
	rg.GET("/persons/:id/coverages", AccessLog("patient_coverage", "list", "id"), ListPatientCoveragesHandler)
	rg.POST("/persons/:id/coverages", CreatePatientCoverageHandler)
	rg.PATCH("/patient-coverages/:id", UpdatePatientCoverageHandler)
	rg.PUT("/admissions/:id/guarantors", PutAdmissionGuarantorsHandler)
	rg.PUT("/admissions/:id/guarantee-letters", PutAdmissionGuaranteeLettersHandler)
}

// ListPayersHandler godoc
// @Summary List payers (penjamin) under a merchant
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/payers [get]
func ListPayersHandler(c *gin.Context) {
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
	payers, err := q.ListPayersByMerchant(c.Request.Context(), sqlcgen.ListPayersByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payers, "meta": gin.H{"limit": limit, "offset": offset}})
}

// payerJSON renders a payer with snake_case fields; money/percent values are
// not part of the payer row itself.
func payerJSON(payer sqlcgen.CorePayer) gin.H {
	var priceListID any
	if payer.PriceListID.Valid {
		priceListID = payer.PriceListID.String()
	}
	return gin.H{
		"id":            payer.ID.String(),
		"code":          payer.Code,
		"name":          payer.Name,
		"payer_type":    payer.PayerType,
		"price_list_id": priceListID,
		"is_active":     payer.IsActive,
		"row_version":   payer.RowVersion,
	}
}

// loadPayerAuthorized fetches the payer (404 when missing) and checks
// PermTariffManage against its actual merchant.
func loadPayerAuthorized(c *gin.Context, rawID string) (sqlcgen.CorePayer, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CorePayer{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	payer, err := q.GetPayerByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
		return sqlcgen.CorePayer{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePayer{}, false
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, payer.MerchantID) {
		return sqlcgen.CorePayer{}, false
	}
	return payer, true
}

// resolvePayerPriceList validates an optional price list reference: it must be
// a live list of the same merchant (404 otherwise).
func resolvePayerPriceList(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, raw string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if raw == "" {
		return id, true
	}
	parsed, ok := parseUUID(raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid price_list_id"})
		return id, false
	}
	list, err := q.GetPriceListByID(c.Request.Context(), parsed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && list.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "price list not found"})
		return id, false
	}
	if err != nil {
		respondInternalError(c, err)
		return id, false
	}
	return parsed, true
}

// respondPayerError maps insert/update Postgres errors: unique → 409
// "payer code already exists".
func respondPayerError(c *gin.Context, err error) bool {
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "payer code already exists"})
		return true
	}
	return false
}

var payerTypes = map[string]bool{
	"bpjs":              true,
	"private_insurance": true,
	"company_guarantor": true,
	"self_pay":          true,
}

type createPayerRequest struct {
	Code        string `json:"code" binding:"required"`
	Name        string `json:"name" binding:"required"`
	PayerType   string `json:"payer_type" binding:"required"`
	PriceListID string `json:"price_list_id"`
}

// CreatePayerHandler godoc
// @Summary Create a payer (penjamin) under a merchant
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body createPayerRequest true "Payer data"
// @Success 201 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/payers [post]
func CreatePayerHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	var req createPayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !payerTypes[req.PayerType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payer_type"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	priceListID, ok := resolvePayerPriceList(c, q, merchantID, req.PriceListID)
	if !ok {
		return
	}
	if req.PayerType == "bpjs" {
		n, err := q.CountBpjsPayers(c.Request.Context(), sqlcgen.CountBpjsPayersParams{
			MerchantID: merchantID, ExcludeID: pgtype.UUID{},
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if n > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "merchant already has a BPJS payer"})
			return
		}
	}
	payer, err := q.CreatePayer(c.Request.Context(), sqlcgen.CreatePayerParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: req.Code, Name: req.Name,
		PayerType: req.PayerType, PriceListID: priceListID, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondPayerError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": payerJSON(payer), "meta": gin.H{}})
}

type updatePayerRequest struct {
	Name        *string `json:"name"`
	IsActive    *bool   `json:"is_active"`
	PriceListID *string `json:"price_list_id"`
	RowVersion  *int64  `json:"row_version" binding:"required"`
}

// UpdatePayerHandler godoc
// @Summary Update a payer (name, active flag, price list)
// @Tags payer
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param request body updatePayerRequest true "Payer data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id} [patch]
func UpdatePayerHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	var req updatePayerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion == nil || *req.RowVersion != int64(payer.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "row version mismatch"})
		return
	}
	name := payer.Name
	if req.Name != nil {
		name = *req.Name
	}
	isActive := payer.IsActive
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	priceListID := payer.PriceListID
	if req.PriceListID != nil {
		q := sqlcgen.New(TxFromContext(c))
		parsed, ok := resolvePayerPriceList(c, q, payer.MerchantID, *req.PriceListID)
		if !ok {
			return
		}
		priceListID = parsed
	}
	q := sqlcgen.New(TxFromContext(c))
	updated, err := q.UpdatePayer(c.Request.Context(), sqlcgen.UpdatePayerParams{
		ID: payer.ID, Name: name, IsActive: isActive, PriceListID: priceListID, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondPayerError(c, err) {
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payerJSON(updated), "meta": gin.H{}})
}

// DeletePayerHandler godoc
// @Summary Soft-delete a payer
// @Description Rejected with 409 while the payer guarantees a visit that is not discharged/cancelled.
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id} [delete]
func DeletePayerHandler(c *gin.Context) {
	payer, ok := loadPayerAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	visits, err := q.CountPayerActiveVisits(c.Request.Context(), payer.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if visits > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "payer is used by an active visit"})
		return
	}
	if err := q.SoftDeletePayer(c.Request.Context(), sqlcgen.SoftDeletePayerParams{
		ID: payer.ID, DeletedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": payer.ID.String(), "deleted": true}, "meta": gin.H{}})
}
