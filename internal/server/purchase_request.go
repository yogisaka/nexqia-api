// internal/server/purchase_request.go
// Purchase request lifecycle API (plan 2026-10-20-inventory-17b-p1-procurement-core
// Task 2, spec 2026-10-20-inventory-17b-procurement-special-cases §4.2–4.3,
// migration 000080_procurement_core): PR drafts, submit/approve/reject/cancel,
// convert to a purchase order, contract prices, procurement settings and the
// central-item list — all inside RunRequestTx transactions.
package server

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Purchase permissions (migration 000080): usulan (request), persetujuan
// 1 tingkat (approve), dan kelola PO/kontrak/pengaturan (manage) — spec §3.
const (
	PermPurchaseRequest = "inventory.purchase.request"
	PermPurchaseApprove = "inventory.purchase.approve"
	PermPurchaseManage  = "inventory.purchase.manage"
)

// RegisterProcurementRoutes registers the procurement endpoints under the
// authenticated tenant group (wired in server.go right after
// RegisterStockRoutes). Purchase-order routes are added here by Task 3.
func RegisterProcurementRoutes(rg *gin.RouterGroup) {
	rg.POST("/merchants/:id/purchase-requests", CreatePurchaseRequestHandler)
	rg.GET("/merchants/:id/purchase-requests", ListPurchaseRequestsHandler)
	rg.PATCH("/purchase-requests/:id", PatchPurchaseRequestHandler)
	rg.POST("/purchase-requests/:id/submit", SubmitPurchaseRequestHandler)
	rg.POST("/purchase-requests/:id/approve", ApprovePurchaseRequestHandler)
	rg.POST("/purchase-requests/:id/reject", RejectPurchaseRequestHandler)
	rg.POST("/purchase-requests/:id/cancel", CancelPurchaseRequestHandler)
	rg.POST("/purchase-requests/:id/convert", ConvertPurchaseRequestHandler)

	rg.GET("/contract-prices", ListContractPricesHandler)
	rg.PUT("/contract-prices", PutContractPriceHandler)
	rg.DELETE("/contract-prices/:id", DeleteContractPriceHandler)
	rg.GET("/merchants/:id/contract-prices", ListEffectiveContractPricesHandler)

	rg.GET("/companies/:id/procurement-settings", GetProcurementSettingsHandler)
	rg.PATCH("/companies/:id/procurement-settings", PatchProcurementSettingsHandler)
	rg.GET("/companies/:id/procurement-central-items", ListProcurementCentralItemsHandler)
	rg.PUT("/companies/:id/procurement-central-items", PutProcurementCentralItemsHandler)
}

// nextProcurementNo derives the next daily document number for a merchant
// (spec §4.1, pola admission.go): lock the merchant+scope+day advisory lock,
// then count rows created since the merchant-local day start. prefix carries
// the trailing dash ("PR-"/"PO-"); seqScope stays dash-free ("pr:"/"po:").
// It also returns the day start so callers can pass the same business day to
// effective-price lookups.
func nextProcurementNo(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, prefix, seqScope string, countFn func(time.Time) (int64, error)) (string, time.Time, error) {
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		return "", time.Time{}, err
	}
	dayStart, err := txDayStart(ctx, q, loc)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := lockDailySequence(ctx, q, merchantID, seqScope, dayStart); err != nil {
		return "", time.Time{}, err
	}
	n, err := countFn(dayStart)
	if err != nil {
		return "", time.Time{}, err
	}
	return fmt.Sprintf("%s%s-%04d", prefix, dayStart.Format("20060102"), n+1), dayStart, nil
}

// §4.1 JSON renderers — snake_case, numerics as exact decimal strings.

func purchaseRequestLineJSON(l sqlcgen.InventoryPurchaseRequestLine) gin.H {
	return gin.H{
		"id":           l.ID.String(),
		"item_id":      l.ItemID.String(),
		"item_unit_id": l.ItemUnitID.String(),
		"pack_qty":     numericExact(l.PackQty),
		"note":         textOrNil(l.Note),
	}
}

func purchaseRequestJSON(r sqlcgen.InventoryPurchaseRequest, lines []sqlcgen.InventoryPurchaseRequestLine) gin.H {
	out := gin.H{
		"id":               r.ID.String(),
		"merchant_id":      r.MerchantID.String(),
		"location_id":      r.LocationID.String(),
		"pr_no":            r.PrNo,
		"status":           r.Status,
		"needed_on":        dateOrNil(r.NeededOn),
		"note":             textOrNil(r.Note),
		"submitted_at":     tsOrNil(r.SubmittedAt),
		"submitted_by":     uuidOrNil(r.SubmittedBy),
		"decided_at":       tsOrNil(r.DecidedAt),
		"decided_by":       uuidOrNil(r.DecidedBy),
		"rejection_reason": textOrNil(r.RejectionReason),
		"created_at":       tsOrNil(r.CreatedAt),
		"row_version":      r.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, purchaseRequestLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

func purchaseOrderLineJSON(l sqlcgen.InventoryPurchaseOrderLine) gin.H {
	return gin.H{
		"id":                l.ID.String(),
		"item_id":           l.ItemID.String(),
		"item_unit_id":      l.ItemUnitID.String(),
		"pack_qty":          numericExact(l.PackQty),
		"pack_content":      numericExact(l.PackContent),
		"pack_price":        numericExact(l.PackPrice),
		"received_base_qty": numericExact(l.ReceivedBaseQty),
		"note":              textOrNil(l.Note),
	}
}

func purchaseOrderJSON(r sqlcgen.InventoryPurchaseOrder, lines []sqlcgen.InventoryPurchaseOrderLine) gin.H {
	out := gin.H{
		"id":            r.ID.String(),
		"merchant_id":   r.MerchantID.String(),
		"supplier_id":   r.SupplierID.String(),
		"location_id":   r.LocationID.String(),
		"request_id":    uuidOrNil(r.RequestID),
		"po_no":         r.PoNo,
		"status":        r.Status,
		"note":          textOrNil(r.Note),
		"sent_at":       tsOrNil(r.SentAt),
		"sent_by":       uuidOrNil(r.SentBy),
		"closed_at":     tsOrNil(r.ClosedAt),
		"cancel_reason": textOrNil(r.CancelReason),
		"created_at":    tsOrNil(r.CreatedAt),
		"row_version":   r.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, purchaseOrderLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

// §4.2 purchase requests.

type purchaseRequestLineRequest struct {
	ItemID     string `json:"item_id" binding:"required"`
	ItemUnitID string `json:"item_unit_id" binding:"required"`
	PackQty    string `json:"pack_qty" binding:"required"`
	Note       string `json:"note"`
}

type purchaseRequestPayload struct {
	LocationID string                       `json:"location_id" binding:"required"`
	NeededOn   string                       `json:"needed_on"`
	Note       string                       `json:"note"`
	Lines      []purchaseRequestLineRequest `json:"lines" binding:"required,min=1,dive"`
	RowVersion int64                        `json:"row_version"`
}

type prLine struct {
	ItemID     pgtype.UUID
	ItemUnitID pgtype.UUID
	PackQty    pgtype.Numeric
	Note       pgtype.Text
}

// parsePurchaseRequestPayload validates the shared draft body and returns the
// parsed lines. ok=false when the response is already written.
func parsePurchaseRequestPayload(c *gin.Context, q *sqlcgen.Queries, req purchaseRequestPayload, merchantID pgtype.UUID) (locID pgtype.UUID, neededOn pgtype.Date, lines []prLine, ok bool) {
	locID, ok = parseUUID(req.LocationID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	loc, err := q.GetLocationByID(c.Request.Context(), locID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// needed_on is optional (NULL); anything set must parse as a date.
	if d, okD := parseDateStr(req.NeededOn); !okD {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid needed_on"})
		return
	} else {
		neededOn = d
	}
	for _, lr := range req.Lines {
		var line prLine
		itemID, ok1 := parseUUID(lr.ItemID)
		unitID, ok2 := parseUUID(lr.ItemUnitID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return
		}
		unit, err := q.GetInventoryItemUnitByID(c.Request.Context(), unitID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!unit.IsActive || unit.ItemID != itemID)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid packaging unit"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		qty, errN := numericOrZero(lr.PackQty)
		if errN != nil || !qty.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_qty"})
			return
		}
		line.ItemID = itemID
		line.ItemUnitID = unitID
		line.PackQty = qty
		line.Note = pgText(lr.Note)
		lines = append(lines, line)
	}
	ok = true
	return
}

// loadPurchaseRequest fetches a live purchase request; rows of other
// merchants are hidden by RLS, so a cross-merchant read is 404
// "purchase request not found" (pola loadGoodsReceipt).
func loadPurchaseRequest(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryPurchaseRequest, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryPurchaseRequest{}, false
	}
	r, err := q.GetPurchaseRequestByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "purchase request not found"})
		return sqlcgen.InventoryPurchaseRequest{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryPurchaseRequest{}, false
	}
	return r, true
}

func insertPurchaseRequestLines(c *gin.Context, q *sqlcgen.Queries, pr sqlcgen.InventoryPurchaseRequest, lines []prLine) bool {
	for _, l := range lines {
		if _, err := q.InsertPurchaseRequestLine(c.Request.Context(), sqlcgen.InsertPurchaseRequestLineParams{
			CompanyID: pr.CompanyID, MerchantID: pr.MerchantID,
			RequestID: pr.ID, ItemID: l.ItemID, ItemUnitID: l.ItemUnitID,
			PackQty: l.PackQty, Note: l.Note, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// CreatePurchaseRequestHandler godoc
// @Summary Create a purchase-request draft with lines
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body purchaseRequestPayload true "PR draft"
// @Success 201 {object} apiResponse
// @Router /merchants/{id}/purchase-requests [post]
func CreatePurchaseRequestHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseRequest, merchantID) {
		return
	}
	var req purchaseRequestPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	locID, neededOn, lines, ok := parsePurchaseRequestPayload(c, q, req, merchantID)
	if !ok {
		return
	}
	prNo, _, err := nextProcurementNo(c.Request.Context(), q, merchantID, "PR-", "pr:",
		func(dayStart time.Time) (int64, error) {
			return q.CountPurchaseRequestsSince(c.Request.Context(), sqlcgen.CountPurchaseRequestsSinceParams{
				MerchantID: merchantID, CreatedAt: pgtype.Timestamptz{Time: dayStart, Valid: true},
			})
		})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	pr, err := q.InsertPurchaseRequest(c.Request.Context(), sqlcgen.InsertPurchaseRequestParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		LocationID: locID, PrNo: prNo, NeededOn: neededOn,
		Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertPurchaseRequestLines(c, q, pr, lines) {
		return
	}
	saved, err := q.ListPurchaseRequestLines(c.Request.Context(), pr.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": purchaseRequestJSON(pr, saved), "meta": gin.H{}})
}

// ListPurchaseRequestsHandler godoc
// @Summary List a merchant's purchase requests (newest first)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/purchase-requests [get]
func ListPurchaseRequestsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseRequest, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListPurchaseRequests(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, purchaseRequestJSON(r, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// PatchPurchaseRequestHandler godoc
// @Summary Replace a draft purchase request (header + full line replace)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Param request body purchaseRequestPayload true "PR draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id} [patch]
func PatchPurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseRequest, pr.MerchantID) {
		return
	}
	if pr.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "request is not editable"})
		return
	}
	var req purchaseRequestPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(pr.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "purchase request changed, reload"})
		return
	}
	locID, neededOn, lines, ok := parsePurchaseRequestPayload(c, q, req, pr.MerchantID)
	if !ok {
		return
	}
	updated, err := q.UpdatePurchaseRequestDraft(c.Request.Context(), sqlcgen.UpdatePurchaseRequestDraftParams{
		LocationID: locID, NeededOn: neededOn, Note: pgText(req.Note),
		UpdatedBy: AuthUserID(c), ID: pr.ID, RowVersion: pr.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "purchase request changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if err := q.DeletePurchaseRequestLines(c.Request.Context(), pr.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertPurchaseRequestLines(c, q, updated, lines) {
		return
	}
	saved, err := q.ListPurchaseRequestLines(c.Request.Context(), pr.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseRequestJSON(updated, saved), "meta": gin.H{}})
}

// markPurchaseRequest transitions a PR through one status step; ErrNoRows
// from the guarded UPDATE becomes 409 with the given message.
func markPurchaseRequest(c *gin.Context, q *sqlcgen.Queries, prID pgtype.UUID, current, target string, rejection pgtype.Text) (sqlcgen.InventoryPurchaseRequest, bool) {
	r, err := q.MarkPurchaseRequestDecided(c.Request.Context(), sqlcgen.MarkPurchaseRequestDecidedParams{
		RequestID: prID, NewStatus: target, DecidedBy: AuthUserID(c),
		RejectionReason: rejection, FromStatus: current,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request is not " + current})
		return sqlcgen.InventoryPurchaseRequest{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryPurchaseRequest{}, false
	}
	return r, true
}

// SubmitPurchaseRequestHandler godoc
// @Summary Submit a draft purchase request (locks it from edits)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id}/submit [post]
func SubmitPurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseRequest, pr.MerchantID) {
		return
	}
	r, err := q.MarkPurchaseRequestSubmitted(c.Request.Context(), sqlcgen.MarkPurchaseRequestSubmittedParams{
		ID: pr.ID, SubmittedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request is not draft"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseRequestJSON(r, nil), "meta": gin.H{}})
}

// ApprovePurchaseRequestHandler godoc
// @Summary Approve a submitted purchase request
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id}/approve [post]
func ApprovePurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseApprove, pr.MerchantID) {
		return
	}
	r, okR := markPurchaseRequest(c, q, pr.ID, "submitted", "approved", pgtype.Text{})
	if !okR {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseRequestJSON(r, nil), "meta": gin.H{}})
}

type rejectPurchaseRequestRequest struct {
	RejectionReason string `json:"rejection_reason" binding:"required"`
}

// RejectPurchaseRequestHandler godoc
// @Summary Reject a submitted purchase request (reason required)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Param request body rejectPurchaseRequestRequest true "Rejection reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id}/reject [post]
func RejectPurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseApprove, pr.MerchantID) {
		return
	}
	var req rejectPurchaseRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.RejectionReason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rejection reason required"})
		return
	}
	r, okR := markPurchaseRequest(c, q, pr.ID, "submitted", "rejected", pgText(req.RejectionReason))
	if !okR {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseRequestJSON(r, nil), "meta": gin.H{}})
}

// CancelPurchaseRequestHandler godoc
// @Summary Cancel a draft or submitted purchase request
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id}/cancel [post]
func CancelPurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseRequest, pr.MerchantID) {
		return
	}
	r, err := q.MarkPurchaseRequestCancelled(c.Request.Context(), sqlcgen.MarkPurchaseRequestCancelledParams{
		ID: pr.ID, DecidedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request is not cancellable"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseRequestJSON(r, nil), "meta": gin.H{}})
}

type convertPurchaseRequestRequest struct {
	// The PO's supplier — purchase_request carries none and
	// purchase_order.supplier_id is NOT NULL (KEPUTUSAN, spec §4.2 #1).
	SupplierID string `json:"supplier_id" binding:"required"`
	// Optional destination location; defaults to the PR location when the PO
	// stays on the PR's merchant, required when it moves to headquarters.
	LocationID string `json:"location_id"`
	Note       string `json:"note"`
}

// ConvertPurchaseRequestHandler godoc
// @Summary Convert an approved purchase request into a draft purchase order
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase request UUID"
// @Param request body convertPurchaseRequestRequest true "Convert options"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-requests/{id}/convert [post]
func ConvertPurchaseRequestHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	pr, ok := loadPurchaseRequest(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, pr.MerchantID) {
		return
	}
	var req convertPurchaseRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	lines, err := q.ListPurchaseRequestLines(c.Request.Context(), pr.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(lines) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request needs lines"})
		return
	}
	supplierID, okS := parseUUID(req.SupplierID)
	if !okS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		return
	}
	// PO target merchant (spec §4.2 #1): sentral/hibrid → headquarters,
	// desentral → the PR's own merchant.
	mode, hq, err := procurementMode(c.Request.Context(), q, pr.CompanyID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	target := pr.MerchantID
	if (mode == "sentral" || mode == "hibrid") && hq.Valid {
		target = hq
	}
	sup, err := q.GetSupplierByID(c.Request.Context(), supplierID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sup.MerchantID.Valid && sup.MerchantID != target) {
		c.JSON(http.StatusNotFound, gin.H{"error": "supplier not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	locationID := pr.LocationID
	if req.LocationID != "" {
		locID, okL := parseUUID(req.LocationID)
		if !okL {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
			return
		}
		loc, err := q.GetLocationByID(c.Request.Context(), locID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != target) {
			c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		locationID = locID
	} else if target != pr.MerchantID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "location required"})
		return
	}
	poNo, dayStart, err := nextProcurementNo(c.Request.Context(), q, target, "PO-", "po:",
		func(ds time.Time) (int64, error) {
			return q.CountPurchaseOrdersSince(c.Request.Context(), sqlcgen.CountPurchaseOrdersSinceParams{
				MerchantID: target, CreatedAt: pgtype.Timestamptz{Time: ds, Valid: true},
			})
		})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	po, err := q.InsertPurchaseOrder(c.Request.Context(), sqlcgen.InsertPurchaseOrderParams{
		CompanyID: pr.CompanyID, MerchantID: target,
		SupplierID: supplierID, LocationID: locationID, RequestID: pr.ID,
		PoNo: poNo, Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "supplier mismatch"})
			return
		}
		respondInternalError(c, err)
		return
	}
	// Copy PR lines: pack_content from the packaging factor; pack_price from
	// the company contract (kind purchase, in force today) — 0 without one.
	for _, l := range lines {
		unit, err := q.GetInventoryItemUnitByID(c.Request.Context(), l.ItemUnitID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		price := pgtype.Numeric{Int: new(big.Int), Exp: -2, Valid: true}
		cp, err := q.GetEffectiveContractPrice(c.Request.Context(), sqlcgen.GetEffectiveContractPriceParams{
			CompanyID: pr.CompanyID, SupplierID: supplierID, ItemID: l.ItemID,
			ItemUnitID: l.ItemUnitID, Kind: "purchase",
			Column6: pgtype.Date{Time: dayStart, Valid: true},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			respondInternalError(c, err)
			return
		}
		if err == nil {
			price = cp.PackPrice
		}
		if _, err := q.InsertPurchaseOrderLine(c.Request.Context(), sqlcgen.InsertPurchaseOrderLineParams{
			CompanyID: pr.CompanyID, MerchantID: target,
			OrderID: po.ID, ItemID: l.ItemID, ItemUnitID: l.ItemUnitID,
			PackQty: l.PackQty, PackContent: unit.Factor, PackPrice: price,
			Note: l.Note, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if _, okM := markPurchaseRequest(c, q, pr.ID, "approved", "converted", pgtype.Text{}); !okM {
		return
	}
	saved, err := q.ListPurchaseOrderLines(c.Request.Context(), po.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": purchaseOrderJSON(po, saved), "meta": gin.H{}})
}
