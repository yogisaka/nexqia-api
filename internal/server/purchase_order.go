// internal/server/purchase_order.go
// Purchase order lifecycle API (plan 2026-10-20-inventory-17b-p1-procurement-core
// Task 3, spec 2026-10-20-inventory-17b-procurement-special-cases §4.2 #2–#3,
// migration 000080_procurement_core): PO drafts with central-mode gating,
// contract-price prefill, send/close/cancel guards and list/get with lines —
// all inside RunRequestTx transactions.
package server

import (
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// loadPurchaseOrder fetches a live purchase order; rows of other merchants
// are hidden by RLS, so a cross-merchant read is 404
// "purchase order not found" (pola loadGoodsReceipt).
func loadPurchaseOrder(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryPurchaseOrder, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryPurchaseOrder{}, false
	}
	r, err := q.GetPurchaseOrderByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "purchase order not found"})
		return sqlcgen.InventoryPurchaseOrder{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryPurchaseOrder{}, false
	}
	return r, true
}

// purchaseOrderLineIn is one draft line of the create/patch body.
type purchaseOrderLineIn struct {
	ItemID      string `json:"item_id" binding:"required"`
	ItemUnitID  string `json:"item_unit_id" binding:"required"`
	PackQty     string `json:"pack_qty" binding:"required"`
	PackContent string `json:"pack_content"`
	PackPrice   string `json:"pack_price"`
	Note        string `json:"note"`
}

type purchaseOrderRequest struct {
	SupplierID string                `json:"supplier_id" binding:"required"`
	LocationID string                `json:"location_id" binding:"required"`
	RequestID  string                `json:"request_id"`
	Note       string                `json:"note"`
	Lines      []purchaseOrderLineIn `json:"lines" binding:"required,min=1,dive"`
	RowVersion int64                 `json:"row_version"`
}

type poLine struct {
	ItemID      pgtype.UUID
	ItemUnitID  pgtype.UUID
	PackQty     pgtype.Numeric
	PackContent pgtype.Numeric
	PackPrice   pgtype.Numeric
	PriceFrom   string // "contract" (prefill at insert time) or "manual"
	Note        pgtype.Text
}

// parsePurchaseOrderLines validates the line list: packaging unit must belong
// to the item, pack_qty > 0, pack_content defaults to the packaging factor,
// and pack_price is either empty (contract prefill at insert) or >= 0.
func parsePurchaseOrderLines(c *gin.Context, q *sqlcgen.Queries, req purchaseOrderRequest) ([]poLine, bool) {
	lines := make([]poLine, 0, len(req.Lines))
	for _, lr := range req.Lines {
		var line poLine
		itemID, ok1 := parseUUID(lr.ItemID)
		unitID, ok2 := parseUUID(lr.ItemUnitID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return nil, false
		}
		unit, err := q.GetInventoryItemUnitByID(c.Request.Context(), unitID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!unit.IsActive || unit.ItemID != itemID)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid packaging unit"})
			return nil, false
		}
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		qty, errN := numericOrZero(lr.PackQty)
		if errN != nil || !qty.Valid || qty.Int == nil || qty.Int.Sign() <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_qty"})
			return nil, false
		}
		// pack_content defaults to the packaging factor when 0/empty.
		if strings.TrimSpace(lr.PackContent) == "" || lr.PackContent == "0" {
			line.PackContent = unit.Factor
			line.PackContent.Valid = true
		} else if line.PackContent, errN = numericOrZero(lr.PackContent); errN != nil || !line.PackContent.Valid ||
			line.PackContent.Int == nil || line.PackContent.Int.Sign() <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_content"})
			return nil, false
		}
		if strings.TrimSpace(lr.PackPrice) == "" {
			line.PriceFrom = "contract"
		} else {
			price, errP := numericOrZero(lr.PackPrice)
			if errP != nil || !price.Valid || (price.Int != nil && price.Int.Sign() < 0) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_price"})
				return nil, false
			}
			line.PackPrice = price
			line.PriceFrom = "manual"
		}
		line.ItemID = itemID
		line.ItemUnitID = unitID
		line.PackQty = qty
		line.Note = pgText(lr.Note)
		lines = append(lines, line)
	}
	return lines, true
}

// enforceCentralMode applies the procurement-mode gating on PO creates
// (spec §4.2 #2): sentral means only the headquarters merchant may issue POs;
// hibrid forces centrally-listed items to the headquarters merchant. PR
// converts bypass this check (handled at convert time by the target choice).
func enforceCentralMode(c *gin.Context, q *sqlcgen.Queries, companyID, merchantID pgtype.UUID, lines []poLine) bool {
	mode, hq, err := procurementMode(c.Request.Context(), q, companyID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if mode == "sentral" && merchantID != hq {
		c.JSON(http.StatusConflict, gin.H{"error": "central procurement required"})
		return false
	}
	if mode == "hibrid" {
		for _, l := range lines {
			central, errC := q.IsProcurementCentralItem(c.Request.Context(), sqlcgen.IsProcurementCentralItemParams{
				CompanyID: companyID, ItemID: l.ItemID,
			})
			if errC != nil {
				respondInternalError(c, errC)
				return false
			}
			if central {
				c.JSON(http.StatusConflict, gin.H{"error": "central procurement required"})
				return false
			}
		}
	}
	return true
}

// insertPurchaseOrderLines writes parsed lines, prefilling pack_price from
// the company contract (kind purchase, in force on the PO day) when the
// request omitted it — spec §4.2 #3.
func insertPurchaseOrderLines(c *gin.Context, q *sqlcgen.Queries, po sqlcgen.InventoryPurchaseOrder, lines []poLine, dayStart time.Time) bool {
	for _, l := range lines {
		price := l.PackPrice
		if l.PriceFrom == "contract" {
			cp, err := q.GetEffectiveContractPrice(c.Request.Context(), sqlcgen.GetEffectiveContractPriceParams{
				CompanyID: po.CompanyID, SupplierID: po.SupplierID, ItemID: l.ItemID,
				ItemUnitID: l.ItemUnitID, Kind: "purchase",
				Column6: pgtype.Date{Time: dayStart, Valid: true},
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return false
			}
			if err == nil {
				price = cp.PackPrice
			} else {
				price = pgtype.Numeric{Int: new(big.Int), Exp: -2, Valid: true}
			}
		}
		if _, err := q.InsertPurchaseOrderLine(c.Request.Context(), sqlcgen.InsertPurchaseOrderLineParams{
			CompanyID: po.CompanyID, MerchantID: po.MerchantID,
			OrderID: po.ID, ItemID: l.ItemID, ItemUnitID: l.ItemUnitID,
			PackQty: l.PackQty, PackContent: l.PackContent, PackPrice: price,
			Note: l.Note, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// CreatePurchaseOrderHandler godoc
// @Summary Create a purchase-order draft with lines
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body purchaseOrderRequest true "PO draft"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/purchase-orders [post]
func CreatePurchaseOrderHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, merchantID) {
		return
	}
	var req purchaseOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	supplierID, okS := parseUUID(req.SupplierID)
	if !okS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		return
	}
	sup, err := q.GetSupplierByID(c.Request.Context(), supplierID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sup.MerchantID.Valid && sup.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "supplier not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	locID, okL := parseUUID(req.LocationID)
	if !okL {
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
	var requestID pgtype.UUID
	if req.RequestID != "" {
		id, okR := parseUUID(req.RequestID)
		if !okR {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request_id"})
			return
		}
		pr, err := q.GetPurchaseRequestByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "purchase request not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if pr.Status != "approved" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request is not approved"})
			return
		}
		requestID = id
	}
	lines, ok := parsePurchaseOrderLines(c, q, req)
	if !ok {
		return
	}
	if !enforceCentralMode(c, q, AuthCompanyID(c), merchantID, lines) {
		return
	}
	poNo, dayStart, err := nextProcurementNo(c.Request.Context(), q, merchantID, "PO-", "po:",
		func(ds time.Time) (int64, error) {
			return q.CountPurchaseOrdersSince(c.Request.Context(), sqlcgen.CountPurchaseOrdersSinceParams{
				MerchantID: merchantID, CreatedAt: pgtype.Timestamptz{Time: ds, Valid: true},
			})
		})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	po, err := q.InsertPurchaseOrder(c.Request.Context(), sqlcgen.InsertPurchaseOrderParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		SupplierID: supplierID, LocationID: locID, RequestID: requestID,
		PoNo: poNo, Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertPurchaseOrderLines(c, q, po, lines, dayStart) {
		return
	}
	saved, err := q.ListPurchaseOrderLines(c.Request.Context(), po.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": purchaseOrderJSON(po, saved), "meta": gin.H{}})
}

// ListPurchaseOrdersHandler godoc
// @Summary List a merchant's purchase orders (newest first, with lines)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/purchase-orders [get]
func ListPurchaseOrdersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListPurchaseOrders(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		lines, errL := q.ListPurchaseOrderLines(c.Request.Context(), r.ID)
		if errL != nil {
			respondInternalError(c, errL)
			return
		}
		data = append(data, purchaseOrderJSON(r, lines))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetPurchaseOrderHandler godoc
// @Summary Fetch one purchase order with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase order UUID"
// @Success 200 {object} apiResponse
// @Router /purchase-orders/{id} [get]
func GetPurchaseOrderHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	po, ok := loadPurchaseOrder(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, po.MerchantID) {
		return
	}
	lines, err := q.ListPurchaseOrderLines(c.Request.Context(), po.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseOrderJSON(po, lines), "meta": gin.H{}})
}

// PatchPurchaseOrderHandler godoc
// @Summary Replace a draft purchase order (header + full line replace)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase order UUID"
// @Param request body purchaseOrderRequest true "PO draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-orders/{id} [patch]
func PatchPurchaseOrderHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	po, ok := loadPurchaseOrder(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, po.MerchantID) {
		return
	}
	if po.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "order is not editable"})
		return
	}
	var req purchaseOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(po.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "purchase order changed, reload"})
		return
	}
	supplierID, okS := parseUUID(req.SupplierID)
	if !okS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		return
	}
	sup, err := q.GetSupplierByID(c.Request.Context(), supplierID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sup.MerchantID.Valid && sup.MerchantID != po.MerchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "supplier not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	locID, okL := parseUUID(req.LocationID)
	if !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	loc, err := q.GetLocationByID(c.Request.Context(), locID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != po.MerchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	lines, ok := parsePurchaseOrderLines(c, q, req)
	if !ok {
		return
	}
	updated, err := q.UpdatePurchaseOrderDraft(c.Request.Context(), sqlcgen.UpdatePurchaseOrderDraftParams{
		SupplierID: supplierID, LocationID: locID, Note: pgText(req.Note),
		UpdatedBy: AuthUserID(c), ID: po.ID, RowVersion: po.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "purchase order changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if err := q.DeletePurchaseOrderLines(c.Request.Context(), po.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	locT, errD := merchantLocation(c.Request.Context(), q, po.MerchantID)
	if errD != nil {
		respondInternalError(c, errD)
		return
	}
	dayStart, errD := txDayStart(c.Request.Context(), q, locT)
	if errD != nil {
		respondInternalError(c, errD)
		return
	}
	if !insertPurchaseOrderLines(c, q, updated, lines, dayStart) {
		return
	}
	saved, err := q.ListPurchaseOrderLines(c.Request.Context(), po.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseOrderJSON(updated, saved), "meta": gin.H{}})
}

// SendPurchaseOrderHandler godoc
// @Summary Send a draft purchase order (locks its lines from edits)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase order UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-orders/{id}/send [post]
func SendPurchaseOrderHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	po, ok := loadPurchaseOrder(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, po.MerchantID) {
		return
	}
	r, err := q.MarkPurchaseOrderSent(c.Request.Context(), sqlcgen.MarkPurchaseOrderSentParams{
		ID: po.ID, SentBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "order is not draft"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseOrderJSON(r, nil), "meta": gin.H{}})
}

// ClosePurchaseOrderHandler godoc
// @Summary Close a sent or partially received purchase order
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase order UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-orders/{id}/close [post]
func ClosePurchaseOrderHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	po, ok := loadPurchaseOrder(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, po.MerchantID) {
		return
	}
	if po.Status != "sent" && po.Status != "partial" {
		c.JSON(http.StatusConflict, gin.H{"error": "order is not closable"})
		return
	}
	// No draft goods receipt may still reference the order (spec §4.2 #3).
	drafts, err := q.ListGoodsReceipts(c.Request.Context(), sqlcgen.ListGoodsReceiptsParams{
		MerchantID: po.MerchantID,
		Status:     pgtype.Text{String: "draft", Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, r := range drafts {
		if r.PurchaseOrderID == po.ID {
			c.JSON(http.StatusConflict, gin.H{"error": "order has open receipts"})
			return
		}
	}
	r, err := q.MarkPurchaseOrderClosed(c.Request.Context(), sqlcgen.MarkPurchaseOrderClosedParams{
		ID: po.ID, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "order is not closable"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseOrderJSON(r, nil), "meta": gin.H{}})
}

type cancelPurchaseOrderRequest struct {
	CancelReason string `json:"cancel_reason" binding:"required"`
}

// CancelPurchaseOrderHandler godoc
// @Summary Cancel a draft purchase order, or a sent one with nothing received
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Purchase order UUID"
// @Param request body cancelPurchaseOrderRequest true "Cancel reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /purchase-orders/{id}/cancel [post]
func CancelPurchaseOrderHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	po, ok := loadPurchaseOrder(c, q, c.Param("id"))
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPurchaseManage, po.MerchantID) {
		return
	}
	var req cancelPurchaseOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.CancelReason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cancel reason required"})
		return
	}
	r, err := q.MarkPurchaseOrderCancelled(c.Request.Context(), sqlcgen.MarkPurchaseOrderCancelledParams{
		ID: po.ID, CancelReason: pgText(req.CancelReason), UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "order cannot be cancelled"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": purchaseOrderJSON(r, nil), "meta": gin.H{}})
}
