// internal/server/stock_ops.go
// Stock operations API (plan 2026-10-07-inventory-17a Task 5, spec
// 2026-10-07-inventory-17-stock-procurement §4.3–4.5): usage issues, transfers
// between locations, signed adjustments, status changes, lot recall, stock
// counts (opname), min/max levels and merchant inventory settings including
// the stock period lock. Every balance mutation goes through the locked
// ledger in stock_ledger.go — this file never writes stock_balance or
// stock_move directly.
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermStockManage guards stock mutations: usage, transfers, adjustments,
// status changes, counts, min/max and warehouse settings (permission
// inventory.stock.manage, migration 000071).
const PermStockManage = "inventory.stock.manage"

// validStockStatus reports whether s is one of the stock balance statuses.
func validStockStatus(s string) bool {
	switch s {
	case "available", "quarantine", "damaged":
		return true
	}
	return false
}

// lotJSON renders a lot for stock operation responses.
func lotJSON(l sqlcgen.InventoryLot) gin.H {
	return gin.H{
		"id":          l.ID.String(),
		"batch_no":    l.BatchNo,
		"expiry_date": dateOrNil(l.ExpiryDate),
		"is_recalled": l.IsRecalled,
		"recall_note": textOrNil(l.RecallNote),
	}
}

// stockMoveJSON renders one ledger move for operation responses.
func stockMoveJSON(m sqlcgen.InventoryStockMove) gin.H {
	return gin.H{
		"id":          m.ID.String(),
		"move_type":   m.MoveType,
		"move_date":   dateText(m.MoveDate),
		"location_id": m.LocationID.String(),
		"item_id":     m.ItemID.String(),
		"lot_id":      m.LotID.String(),
		"source":      m.Source,
		"status":      m.Status,
		"quantity":    numericExact(m.Quantity),
		"unit_cost":   numericExact(m.UnitCost),
		"ref_type":    textOrNil(m.RefType),
		"ref_id":      uuidOrNil(m.RefID),
		"reason":      textOrNil(m.Reason),
	}
}

// issuedPartJSON renders one allocation of a usage issue (spec §4.4).
func issuedPartJSON(p issuedPart, lot sqlcgen.InventoryLot) gin.H {
	return gin.H{
		"lot":       lotJSON(lot),
		"source":    p.Source,
		"quantity":  numericExact(p.Qty),
		"unit_cost": numericExact(p.UnitCost),
	}
}

// loadMerchantRoom validates that a location exists and belongs to the
// merchant; writes 404 and returns false otherwise.
func loadMerchantRoom(c *gin.Context, q *sqlcgen.Queries, locID, merchantID pgtype.UUID) (sqlcgen.CoreLocation, bool) {
	loc, err := q.GetLocationByID(c.Request.Context(), locID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && loc.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return sqlcgen.CoreLocation{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CoreLocation{}, false
	}
	return loc, true
}

// §4.4 usage.

type stockUsageRequest struct {
	LocationID     string `json:"location_id" binding:"required"`
	ItemID         string `json:"item_id" binding:"required"`
	Quantity       string `json:"quantity" binding:"required"`
	Reason         string `json:"reason"`
	AdmissionID    string `json:"admission_id"`
	OverrideLotID  string `json:"override_lot_id"`
	OverrideReason string `json:"override_reason"`
}

// StockUsageHandler godoc
// @Summary Issue stock for usage (FEFO) or an admission
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body stockUsageRequest true "Usage"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/stock/usage [post]
func StockUsageHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req stockUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	locID, okL := parseUUID(req.LocationID)
	itemID, okI := parseUUID(req.ItemID)
	if !okL || !okI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	qty, err := parseNumeric(req.Quantity)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
		return
	}
	var overrideLot pgtype.UUID
	if req.OverrideLotID != "" {
		if overrideLot, ok = parseUUID(req.OverrideLotID); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid override_lot_id"})
			return
		}
	}
	// ref_type is admission when an admission is given, else usage.
	ref := stockRef{RefType: "usage"}
	if req.AdmissionID != "" {
		admissionID, okA := parseUUID(req.AdmissionID)
		if !okA {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
			return
		}
		ref = stockRef{RefType: "admission", RefID: admissionID}
	}
	parts, err := issueStock(c.Request.Context(), q, merchantID, AuthCompanyID(c), AuthUserID(c),
		locID, itemID, qty, time.Now().UTC(), ref, overrideLot, req.OverrideReason)
	if err != nil {
		if !respondStockError(c, err) {
			respondInternalError(c, err)
		}
		return
	}
	data := make([]gin.H, 0, len(parts))
	for _, p := range parts {
		lot, err := q.GetLotByID(c.Request.Context(), p.LotID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		data = append(data, issuedPartJSON(p, lot))
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"parts": data}, "meta": gin.H{}})
}

// §4.5 transfers.

type stockTransferLineRequest struct {
	ItemID   string `json:"item_id" binding:"required"`
	LotID    string `json:"lot_id" binding:"required"`
	Source   string `json:"source" binding:"required"`
	Quantity string `json:"quantity" binding:"required"`
}

type stockTransferRequest struct {
	FromLocationID string                     `json:"from_location_id" binding:"required"`
	ToLocationID   string                     `json:"to_location_id" binding:"required"`
	TransferDate   string                     `json:"transfer_date"`
	Lines          []stockTransferLineRequest `json:"lines" binding:"required,min=1,dive"`
}

// StockTransferHandler godoc
// @Summary Transfer stock between two locations of the merchant
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body stockTransferRequest true "Transfer lines"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/stock/transfers [post]
func StockTransferHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req stockTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fromID, okF := parseUUID(req.FromLocationID)
	toID, okT := parseUUID(req.ToLocationID)
	if !okF || !okT {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location"})
		return
	}
	if fromID == toID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "locations must differ"})
		return
	}
	moveDate := time.Now().UTC()
	if req.TransferDate != "" {
		d, okD := parseDateStr(req.TransferDate)
		if !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid transfer_date"})
			return
		}
		moveDate = d.Time
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, fromID, merchantID); !ok {
		return
	}
	if _, ok := loadMerchantRoom(c, q, toID, merchantID); !ok {
		return
	}
	// One shared ref_id for every move of this transfer (no document table
	// in #17a — moves are grouped by ref).
	refID, err := q.NewStockRefID(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	moves := make([]gin.H, 0, 2*len(req.Lines))
	for _, lr := range req.Lines {
		itemID, ok1 := parseUUID(lr.ItemID)
		lotID, ok2 := parseUUID(lr.LotID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return
		}
		if !validReceiptSource(lr.Source) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
			return
		}
		lot, err := q.GetLotByID(c.Request.Context(), lotID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && lot.ItemID != itemID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "lot does not belong to item"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		qty, errN := parseNumeric(lr.Quantity)
		if errN != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
			return
		}
		avg, errC := q.GetItemAvgCost(c.Request.Context(), sqlcgen.GetItemAvgCostParams{
			MerchantID: merchantID, ItemID: itemID,
		})
		if errC != nil {
			respondInternalError(c, errC)
			return
		}
		base := stockMoveInput{
			ItemID: itemID, UnitCost: avg, MoveType: "transfer_out",
			RefType: "transfer", RefID: refID, MoveDate: moveDate,
		}
		out := base
		out.Key = stockKey{LocationID: fromID, LotID: lotID, Source: lr.Source, Status: "available"}
		out.Qty = negateNumeric(qty)
		moveID, errM := applyStockMove(c.Request.Context(), q, merchantID, AuthCompanyID(c), AuthUserID(c), out)
		if errM != nil {
			if !respondStockError(c, errM) {
				respondInternalError(c, errM)
			}
			return
		}
		if m, err := q.GetStockMoveByID(c.Request.Context(), moveID); err == nil {
			moves = append(moves, stockMoveJSON(m))
		} else {
			respondInternalError(c, err)
			return
		}
		in := base
		in.Key = stockKey{LocationID: toID, LotID: lotID, Source: lr.Source, Status: "available"}
		in.Qty = qty
		in.MoveType = "transfer_in"
		moveID, errM = applyStockMove(c.Request.Context(), q, merchantID, AuthCompanyID(c), AuthUserID(c), in)
		if errM != nil {
			if !respondStockError(c, errM) {
				respondInternalError(c, errM)
			}
			return
		}
		if m, err := q.GetStockMoveByID(c.Request.Context(), moveID); err == nil {
			moves = append(moves, stockMoveJSON(m))
		} else {
			respondInternalError(c, err)
			return
		}
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"transfer_id": refID.String(), "moves": moves}, "meta": gin.H{}})
}

// §4.5 adjustments.

type stockAdjustmentRequest struct {
	LocationID string `json:"location_id" binding:"required"`
	ItemID     string `json:"item_id" binding:"required"`
	LotID      string `json:"lot_id" binding:"required"`
	Source     string `json:"source" binding:"required"`
	Status     string `json:"status" binding:"required"`
	Quantity   string `json:"quantity" binding:"required"`
	Reason     string `json:"reason"`
}

// StockAdjustmentHandler godoc
// @Summary Signed stock adjustment per lot/source/status (reason required)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body stockAdjustmentRequest true "Adjustment"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/stock/adjustments [post]
func StockAdjustmentHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req stockAdjustmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	if !validReceiptSource(req.Source) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}
	if !validStockStatus(req.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	qty, err := parseNumeric(req.Quantity)
	if err != nil || !qty.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
		return
	}
	if m, errM := numericToMilli(qty); errM != nil || m == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quantity must not be zero"})
		return
	}
	locID, okL := parseUUID(req.LocationID)
	itemID, okI := parseUUID(req.ItemID)
	lotID, okR := parseUUID(req.LotID)
	if !okL || !okI || !okR {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	lot, errL := q.GetLotByID(c.Request.Context(), lotID)
	if errors.Is(errL, pgx.ErrNoRows) || (errL == nil && lot.ItemID != itemID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "lot does not belong to item"})
		return
	}
	if errL != nil {
		respondInternalError(c, errL)
		return
	}
	// Valuation = current average cost; a positive purchase adjustment does
	// not touch the HPP (spec §4.5).
	avg, errC := q.GetItemAvgCost(c.Request.Context(), sqlcgen.GetItemAvgCostParams{
		MerchantID: merchantID, ItemID: itemID,
	})
	if errC != nil {
		respondInternalError(c, errC)
		return
	}
	moveID, errM := applyStockMove(c.Request.Context(), q, merchantID, AuthCompanyID(c), AuthUserID(c), stockMoveInput{
		Key:      stockKey{LocationID: locID, LotID: lotID, Source: req.Source, Status: req.Status},
		ItemID:   itemID,
		Qty:      qty,
		UnitCost: avg,
		MoveType: "adjustment",
		RefType:  "adjustment",
		Reason:   req.Reason,
		MoveDate: time.Now().UTC(),
	})
	if errM != nil {
		if !respondStockError(c, errM) {
			respondInternalError(c, errM)
		}
		return
	}
	move, err := q.GetStockMoveByID(c.Request.Context(), moveID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": stockMoveJSON(move), "meta": gin.H{}})
}

// §4.3 status change.

type stockStatusChangeRequest struct {
	LocationID string `json:"location_id" binding:"required"`
	LotID      string `json:"lot_id" binding:"required"`
	Source     string `json:"source" binding:"required"`
	FromStatus string `json:"from_status" binding:"required"`
	ToStatus   string `json:"to_status" binding:"required"`
	Quantity   string `json:"quantity" binding:"required"`
	Reason     string `json:"reason"`
}

// StockStatusChangeHandler godoc
// @Summary Move quantity from one balance status to another (two moves)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body stockStatusChangeRequest true "Status change"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/stock/status-change [post]
func StockStatusChangeHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req stockStatusChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validStockStatus(req.FromStatus) || !validStockStatus(req.ToStatus) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if req.FromStatus == req.ToStatus {
		c.JSON(http.StatusBadRequest, gin.H{"error": "statuses must differ"})
		return
	}
	if !validReceiptSource(req.Source) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}
	qty, err := parseNumeric(req.Quantity)
	if err != nil || !qty.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
		return
	}
	if m, errM := numericToMilli(qty); errM != nil || m <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quantity must be positive"})
		return
	}
	locID, okL := parseUUID(req.LocationID)
	lotID, okR := parseUUID(req.LotID)
	if !okL || !okR {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	// The item id comes from the lot (the request carries only the lot).
	lot, errL := q.GetLotByID(c.Request.Context(), lotID)
	if errors.Is(errL, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "lot not found"})
		return
	}
	if errL != nil {
		respondInternalError(c, errL)
		return
	}
	avg, errC := q.GetItemAvgCost(c.Request.Context(), sqlcgen.GetItemAvgCostParams{
		MerchantID: merchantID, ItemID: lot.ItemID,
	})
	if errC != nil {
		respondInternalError(c, errC)
		return
	}
	base := stockMoveInput{
		ItemID: lot.ItemID, Qty: qty, UnitCost: avg, MoveType: "status_change",
		RefType: "status_change", Reason: req.Reason, MoveDate: time.Now().UTC(),
	}
	moves := make([]gin.H, 0, 2)
	out := base
	out.Key = stockKey{LocationID: locID, LotID: lotID, Source: req.Source, Status: req.FromStatus}
	out.Qty = negateNumeric(qty)
	for _, in := range []stockMoveInput{out, func() stockMoveInput {
		v := base
		v.Key = stockKey{LocationID: locID, LotID: lotID, Source: req.Source, Status: req.ToStatus}
		return v
	}()} {
		moveID, errM := applyStockMove(c.Request.Context(), q, merchantID, AuthCompanyID(c), AuthUserID(c), in)
		if errM != nil {
			if !respondStockError(c, errM) {
				respondInternalError(c, errM)
			}
			return
		}
		move, err := q.GetStockMoveByID(c.Request.Context(), moveID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		moves = append(moves, stockMoveJSON(move))
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"moves": moves}, "meta": gin.H{}})
}

// §4.3 lot recall (company level).

type recallLotRequest struct {
	IsRecalled *bool  `json:"is_recalled" binding:"required"`
	RecallNote string `json:"recall_note"`
}

// RecallLotHandler godoc
// @Summary Recall (or release) a lot company-wide
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Lot UUID"
// @Param request body recallLotRequest true "Recall flag and note"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /lots/{id}/recall [patch]
func RecallLotHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermStockManage) {
		return
	}
	lotID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req recallLotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if *req.IsRecalled && strings.TrimSpace(req.RecallNote) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recall note is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	lot, err := q.UpdateLotRecall(c.Request.Context(), sqlcgen.UpdateLotRecallParams{
		ID: lotID, IsRecalled: *req.IsRecalled, RecallNote: pgText(req.RecallNote),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "lot not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": lotJSON(lot), "meta": gin.H{}})
}

// §4.5 opname (stock counts).

func stockCountJSON(r sqlcgen.InventoryStockCount, lines []sqlcgen.ListStockCountLinesRow) gin.H {
	out := gin.H{
		"id":          r.ID.String(),
		"merchant_id": r.MerchantID.String(),
		"location_id": r.LocationID.String(),
		"count_date":  dateText(r.CountDate),
		"status":      r.Status,
		"note":        textOrNil(r.Note),
		"posted_at":   tsOrNil(r.PostedAt),
		"created_at":  tsOrNil(r.CreatedAt),
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, stockCountLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

func stockCountLineJSON(l sqlcgen.ListStockCountLinesRow) gin.H {
	return gin.H{
		"id":          l.ID.String(),
		"balance_id":  l.BalanceID.String(),
		"item_code":   l.ItemCode,
		"item_name":   l.ItemName,
		"batch_no":    l.BatchNo,
		"expiry_date": dateOrNil(l.ExpiryDate),
		"source":      l.Source,
		"status":      l.Status,
		"system_qty":  numericExact(l.SystemQty),
		"counted_qty": numericExactOrNil(l.CountedQty),
		"reason":      textOrNil(l.Reason),
	}
}

type createStockCountRequest struct {
	LocationID string `json:"location_id" binding:"required"`
	CountDate  string `json:"count_date"`
}

// CreateStockCountHandler godoc
// @Summary Start a stock count: snapshot every balance with qty > 0
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body createStockCountRequest true "Count"
// @Success 201 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/stock-counts [post]
func CreateStockCountHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req createStockCountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	locID, okL := parseUUID(req.LocationID)
	if !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	countDate := time.Now().UTC()
	if req.CountDate != "" {
		d, okD := parseDateStr(req.CountDate)
		if !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid count_date"})
			return
		}
		countDate = d.Time
	}
	count, err := q.InsertStockCount(c.Request.Context(), sqlcgen.InsertStockCountParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		LocationID: locID, CountDate: pgDate(countDate), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "a draft count already exists for this location"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if _, err := q.SnapshotStockCountLines(c.Request.Context(), sqlcgen.SnapshotStockCountLinesParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, CountID: count.ID, LocationID: locID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	lines, err := q.ListStockCountLines(c.Request.Context(), count.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": stockCountJSON(count, lines), "meta": gin.H{}})
}

// loadStockCount fetches a count and checks PermStockManage at its merchant.
func loadStockCount(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryStockCount, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryStockCount{}, false
	}
	r, err := q.GetStockCountByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "stock count not found"})
		return sqlcgen.InventoryStockCount{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryStockCount{}, false
	}
	if !RequirePermissionForMerchant(c, PermStockManage, r.MerchantID) {
		return sqlcgen.InventoryStockCount{}, false
	}
	return r, true
}

type stockCountLineInput struct {
	BalanceID  string `json:"balance_id" binding:"required"`
	CountedQty string `json:"counted_qty" binding:"required"`
	Reason     string `json:"reason"`
}

type stockCountLinesRequest struct {
	Lines []stockCountLineInput `json:"lines" binding:"required,min=1,dive"`
}

// UpdateStockCountLinesHandler godoc
// @Summary Replace counted quantities on draft count lines
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Stock count UUID"
// @Param request body stockCountLinesRequest true "Counted lines"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /stock-counts/{id}/lines [put]
func UpdateStockCountLinesHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	count, ok := loadStockCount(c, q, c.Param("id"))
	if !ok {
		return
	}
	if count.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "stock count is not draft"})
		return
	}
	var req stockCountLinesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for _, lr := range req.Lines {
		balanceID, okB := parseUUID(lr.BalanceID)
		if !okB {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid balance_id"})
			return
		}
		counted, err := parseNumeric(lr.CountedQty)
		if err != nil || !counted.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counted_qty"})
			return
		}
		if m, errM := numericToMilli(counted); errM != nil || m < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counted_qty"})
			return
		}
		rows, err := q.SetStockCountLine(c.Request.Context(), sqlcgen.SetStockCountLineParams{
			CountID: count.ID, BalanceID: balanceID,
			CountedQty: counted, Reason: nargText(&lr.Reason),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if rows == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown balance in this count"})
			return
		}
	}
	lines, err := q.ListStockCountLines(c.Request.Context(), count.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": stockCountJSON(count, lines), "meta": gin.H{}})
}

// strPtr returns a pointer to s (empty string keeps nargText NULL).
func strPtr(s string) *string { return &s }

// PostStockCountHandler godoc
// @Summary Post a draft stock count: one count move per differing line
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Stock count UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /stock-counts/{id}/post [post]
func PostStockCountHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	count, ok := loadStockCount(c, q, c.Param("id"))
	if !ok {
		return
	}
	if count.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "stock count is not draft"})
		return
	}
	lines, err := q.ListStockCountLines(c.Request.Context(), count.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Lines are ordered by their snapshot (balance id) order, so the FOR
	// UPDATE locks follow a stable order.
	for _, l := range lines {
		bal, err := q.LockStockCountBalance(c.Request.Context(), l.BalanceID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		sysMilli, errS := numericToMilli(l.SystemQty)
		curMilli, errC := numericToMilli(bal.Quantity)
		if errS != nil || errC != nil {
			respondInternalError(c, errS)
			return
		}
		if curMilli != sysMilli {
			c.JSON(http.StatusConflict, gin.H{"error": "stock changed since the count started"})
			return
		}
		if !l.CountedQty.Valid {
			continue
		}
		diff := mustNumericScaled(l.CountedQty, 3) - sysMilli
		if diff == 0 {
			continue
		}
		// A difference needs a reason (spec §4.5).
		if !l.Reason.Valid || strings.TrimSpace(l.Reason.String) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "count difference needs a reason"})
			return
		}
		if _, err := applyStockMove(c.Request.Context(), q, count.MerchantID, count.CompanyID, AuthUserID(c), stockMoveInput{
			Key:      stockKey{LocationID: count.LocationID, LotID: bal.LotID, Source: bal.Source, Status: bal.Status},
			ItemID:   bal.ItemID,
			Qty:      numericFromMilli(diff),
			UnitCost: pgtype.Numeric{Valid: true},
			MoveType: "count",
			RefType:  "stock_count",
			RefID:    count.ID,
			Reason:   l.Reason.String,
			MoveDate: count.CountDate.Time,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	posted, err := q.MarkStockCountPosted(c.Request.Context(), sqlcgen.MarkStockCountPostedParams{
		CountID: count.ID, PostedBy: AuthUserID(c),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "stock count is not draft"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListStockCountLines(c.Request.Context(), count.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": stockCountJSON(posted, saved), "meta": gin.H{}})
}

// DeleteStockCountHandler godoc
// @Summary Delete a draft stock count
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Stock count UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /stock-counts/{id} [delete]
func DeleteStockCountHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	count, ok := loadStockCount(c, q, c.Param("id"))
	if !ok {
		return
	}
	rows, err := q.DeleteStockCount(c.Request.Context(), count.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if rows == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "stock count is not draft"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": count.ID.String()}, "meta": gin.H{}})
}

// §4.5 min/max stock levels.

type stockLevelInput struct {
	LocationID string `json:"location_id" binding:"required"`
	ItemID     string `json:"item_id" binding:"required"`
	MinQty     string `json:"min_qty" binding:"required"`
	MaxQty     string `json:"max_qty" binding:"required"`
}

type stockLevelsRequest struct {
	Levels []stockLevelInput `json:"levels" binding:"required,min=1,dive"`
}

func stockLevelJSON(l sqlcgen.InventoryStockLevel) gin.H {
	return gin.H{
		"id":          l.ID.String(),
		"location_id": l.LocationID.String(),
		"item_id":     l.ItemID.String(),
		"min_qty":     numericExact(l.MinQty),
		"max_qty":     numericExact(l.MaxQty),
	}
}

// PutStockLevelsHandler godoc
// @Summary Replace the min/max set for each location sent
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body stockLevelsRequest true "Levels"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/stock-levels [put]
func PutStockLevelsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req stockLevelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Group the request by location: each location sent gets its whole set
	// replaced; other locations are untouched.
	order := make([]pgtype.UUID, 0, len(req.Levels))
	perLoc := map[pgtype.UUID][]stockLevelInput{}
	for _, lr := range req.Levels {
		locID, okL := parseUUID(lr.LocationID)
		itemID, okI := parseUUID(lr.ItemID)
		if !okL || !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid level reference"})
			return
		}
		minQty, errMin := parseNumeric(lr.MinQty)
		maxQty, errMax := parseNumeric(lr.MaxQty)
		if errMin != nil || errMax != nil || !minQty.Valid || !maxQty.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid min_qty/max_qty"})
			return
		}
		minMilli, err1 := numericToMilli(minQty)
		maxMilli, err2 := numericToMilli(maxQty)
		if err1 != nil || err2 != nil || minMilli < 0 || maxMilli < minMilli {
			c.JSON(http.StatusBadRequest, gin.H{"error": "max_qty must be >= min_qty"})
			return
		}
		if _, seen := perLoc[locID]; !seen {
			order = append(order, locID)
		}
		perLoc[locID] = append(perLoc[locID], lr)
		_ = itemID
	}
	saved := make([]gin.H, 0, len(req.Levels))
	for _, locID := range order {
		if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
			return
		}
		if err := q.DeleteStockLevelsByLocation(c.Request.Context(), sqlcgen.DeleteStockLevelsByLocationParams{
			MerchantID: merchantID, LocationID: locID,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		for _, lr := range perLoc[locID] {
			itemID, _ := parseUUID(lr.ItemID)
			if _, err := q.GetInventoryItemByID(c.Request.Context(), itemID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "unknown item"})
					return
				}
				respondInternalError(c, err)
				return
			}
			minQty, _ := parseNumeric(lr.MinQty)
			maxQty, _ := parseNumeric(lr.MaxQty)
			level, err := q.InsertStockLevel(c.Request.Context(), sqlcgen.InsertStockLevelParams{
				CompanyID: AuthCompanyID(c), MerchantID: merchantID,
				LocationID: locID, ItemID: itemID, MinQty: minQty, MaxQty: maxQty,
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			saved = append(saved, stockLevelJSON(level))
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": saved, "meta": gin.H{}})
}

// §4.2/§4.5 merchant inventory settings.

func inventorySettingJSON(s sqlcgen.InventoryMerchantSetting) gin.H {
	return gin.H{
		"merchant_id":                   s.MerchantID.String(),
		"near_expiry_months_1":          s.NearExpiryMonths1,
		"near_expiry_months_2":          s.NearExpiryMonths2,
		"min_shelf_life_months":         s.MinShelfLifeMonths,
		"min_shelf_life_vaccine_months": s.MinShelfLifeVaccineMonths,
		"input_vat_creditable":          s.InputVatCreditable,
		"default_vat_percent":           numericJSON(s.DefaultVatPercent),
		"stock_locked_until":            dateOrNil(s.StockLockedUntil),
		"updated_at":                    tsOrNil(s.UpdatedAt),
	}
}

// GetInventorySettingsHandler godoc
// @Summary Read the merchant's inventory settings (defaults created on demand)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/inventory-settings [get]
func GetInventorySettingsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	s, err := q.GetOrCreateMerchantSettingFull(c.Request.Context(), sqlcgen.GetOrCreateMerchantSettingFullParams{
		MerchantID: merchantID, CompanyID: AuthCompanyID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": inventorySettingJSON(s), "meta": gin.H{}})
}

type patchInventorySettingsRequest struct {
	NearExpiryMonths1         *int16  `json:"near_expiry_months_1"`
	NearExpiryMonths2         *int16  `json:"near_expiry_months_2"`
	MinShelfLifeMonths        *int16  `json:"min_shelf_life_months"`
	MinShelfLifeVaccineMonths *int16  `json:"min_shelf_life_vaccine_months"`
	InputVatCreditable        *bool   `json:"input_vat_creditable"`
	DefaultVatPercent         *string `json:"default_vat_percent"`
	StockLockedUntil          *string `json:"stock_locked_until"`
}

// PatchInventorySettingsHandler godoc
// @Summary Update the merchant's inventory settings (lock date may not move backwards)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body patchInventorySettingsRequest true "Settings"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/inventory-settings [patch]
func PatchInventorySettingsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req patchInventorySettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetOrCreateMerchantSettingFull(c.Request.Context(), sqlcgen.GetOrCreateMerchantSettingFullParams{
		MerchantID: merchantID, CompanyID: AuthCompanyID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var lockedUntil pgtype.Date
	if req.StockLockedUntil != nil {
		d, okD := parseDateStr(*req.StockLockedUntil)
		if !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stock_locked_until"})
			return
		}
		if existing.StockLockedUntil.Valid && d.Valid && d.Time.Before(existing.StockLockedUntil.Time) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "lock date cannot move backwards"})
			return
		}
		lockedUntil = d
	}
	var vatPercent pgtype.Numeric
	if req.DefaultVatPercent != nil {
		if vatPercent, err = parseNumeric(*req.DefaultVatPercent); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid default_vat_percent"})
			return
		}
	}
	updated, err := q.UpdateMerchantSetting(c.Request.Context(), sqlcgen.UpdateMerchantSettingParams{
		NearExpiryMonths1:         nargInt2(req.NearExpiryMonths1),
		NearExpiryMonths2:         nargInt2(req.NearExpiryMonths2),
		MinShelfLifeMonths:        nargInt2(req.MinShelfLifeMonths),
		MinShelfLifeVaccineMonths: nargInt2(req.MinShelfLifeVaccineMonths),
		InputVatCreditable:        nargBool(req.InputVatCreditable),
		DefaultVatPercent:         vatPercent,
		StockLockedUntil:          lockedUntil,
		UpdatedBy:                 AuthUserID(c),
		MerchantID:                merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": inventorySettingJSON(updated), "meta": gin.H{}})
}

// nargInt2 maps an optional JSON integer to a nullable smallint.
func nargInt2(p *int16) pgtype.Int2 {
	if p == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: *p, Valid: true}
}
