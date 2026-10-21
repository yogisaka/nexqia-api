// internal/server/stock_out.go
// Stock outbound value flows (plan 2026-10-20-inventory-17b-p2 Task 2, spec
// 2026-10-20-inventory-17b-procurement-special-cases §5, migration 000081):
// supplier returns (HPP dihitung mundur), destructions (berita acara
// narkotika) and inter-merchant transfers (HPP pengirim, dalam perjalanan).
// All balance mutations go through applyStockMove; daily numbers use
// lockDailySequence on the txDayStart business day.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermStockReturn guards supplier returns (inventory.stock.return,
// migration 000081 — HPP reversal and potential credit notes).
const PermStockReturn = "inventory.stock.return"

// PermStockDestroy guards destructions (inventory.stock.destroy,
// migration 000081 — narcotics four-eyes companion permission).
const PermStockDestroy = "inventory.stock.destroy"

// RegisterStockOutRoutes registers the outbound endpoints under the
// authenticated router group (wired in server.go right after
// RegisterProcurementRoutes).
func RegisterStockOutRoutes(rg *gin.RouterGroup) {
	rg.POST("/merchants/:id/supplier-returns", CreateSupplierReturnHandler)
	rg.GET("/merchants/:id/supplier-returns", ListSupplierReturnsHandler)
	rg.GET("/supplier-returns/:id", GetSupplierReturnHandler)
	rg.PATCH("/supplier-returns/:id", UpdateSupplierReturnHandler)
	rg.POST("/supplier-returns/:id/post", PostSupplierReturnHandler)
	rg.POST("/supplier-returns/:id/void", VoidSupplierReturnHandler)

	rg.POST("/merchants/:id/destructions", CreateDestructionHandler)
	rg.GET("/merchants/:id/destructions", ListDestructionsHandler)
	rg.GET("/destructions/:id", GetDestructionHandler)
	rg.PATCH("/destructions/:id", UpdateDestructionHandler)
	rg.POST("/destructions/:id/post", PostDestructionHandler)
	rg.POST("/destructions/:id/void", VoidDestructionHandler)

	rg.POST("/merchants/:id/inter-merchant-transfers", CreateInterMerchantTransferHandler)
	rg.GET("/merchants/:id/inter-merchant-transfers", ListInterMerchantTransfersHandler)
	rg.GET("/inter-merchant-transfers/:id", GetInterMerchantTransferHandler)
	rg.POST("/inter-merchant-transfers/:id/receive", ReceiveInterMerchantTransferHandler)
	rg.POST("/inter-merchant-transfers/:id/cancel", CancelInterMerchantTransferHandler)

	// §5.2 #3 lot recall report — company-level (no merchant in the path).
	rg.GET("/lots/:id/recall-report", RecallReportHandler)
}

// nextOutboundNo derives the next daily document number (pola
// nextProcurementNo): lock the advisory lock for seqScope and lockKey on the
// tzMerchant-local business day, then count rows created since that day
// start. prefix carries the trailing dash; seqScope stays dash-free.
func nextOutboundNo(ctx context.Context, q *sqlcgen.Queries, tzMerchant, lockKey pgtype.UUID, prefix, seqScope string, countFn func(time.Time) (int64, error)) (string, error) {
	loc, err := merchantLocation(ctx, q, tzMerchant)
	if err != nil {
		return "", err
	}
	dayStart, err := txDayStart(ctx, q, loc)
	if err != nil {
		return "", err
	}
	if err := lockDailySequence(ctx, q, lockKey, seqScope, dayStart); err != nil {
		return "", err
	}
	n, err := countFn(dayStart)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s-%04d", prefix, dayStart.Format("20060102"), n+1), nil
}

// outboundLine is one parsed draft line, shared by the three document kinds
// (status doubles as destruction from_status).
type outboundLine struct {
	ItemID   pgtype.UUID
	LotID    pgtype.UUID
	Source   string
	Status   string
	BaseQty  pgtype.Numeric
	UnitCost pgtype.Numeric
}

// validateOutboundLot loads the lot and pins it to the item (400 otherwise).
func validateOutboundLot(c *gin.Context, q *sqlcgen.Queries, itemID, lotID pgtype.UUID) (sqlcgen.InventoryLot, bool) {
	lot, err := q.GetLotByID(c.Request.Context(), lotID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lot.ItemID != itemID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid lot_id"})
		return sqlcgen.InventoryLot{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryLot{}, false
	}
	return lot, true
}

// loadPostedMoves reads the positive stock moves a posted document wrote, in
// write order — the reversal source for void (the sqlc queries predate this
// read-back need). moveType filters to the document's own move type.
type postedMoveRow struct {
	ID         pgtype.UUID
	LocationID pgtype.UUID
	ItemID     pgtype.UUID
	LotID      pgtype.UUID
	Source     string
	Status     string
	Quantity   pgtype.Numeric
	UnitCost   pgtype.Numeric
}

func loadPostedMoves(c *gin.Context, moveType string, refID pgtype.UUID) ([]postedMoveRow, bool) {
	rows, err := TxFromContext(c).Query(c.Request.Context(), `
		SELECT id, location_id, item_id, lot_id, source, status, quantity, unit_cost
		FROM inventory.stock_move
		WHERE ref_type = $1 AND ref_id = $2 AND move_type = $3
		ORDER BY created_at, id`, moveType, refID, moveType)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	defer rows.Close()
	out := []postedMoveRow{}
	for rows.Next() {
		var m postedMoveRow
		if err := rows.Scan(&m.ID, &m.LocationID, &m.ItemID, &m.LotID,
			&m.Source, &m.Status, &m.Quantity, &m.UnitCost); err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	return out, true
}

// §5.2 #1 supplier returns.

func supplierReturnLineJSON(l sqlcgen.InventorySupplierReturnLine) gin.H {
	return gin.H{
		"id":        l.ID.String(),
		"item_id":   l.ItemID.String(),
		"lot_id":    l.LotID.String(),
		"source":    l.Source,
		"base_qty":  numericExact(l.BaseQty),
		"unit_cost": numericExact(l.UnitCost),
	}
}

func supplierReturnJSON(r sqlcgen.InventorySupplierReturn, lines []sqlcgen.InventorySupplierReturnLine) gin.H {
	out := gin.H{
		"id":          r.ID.String(),
		"merchant_id": r.MerchantID.String(),
		"supplier_id": r.SupplierID.String(),
		"return_no":   r.ReturnNo,
		"status":      r.Status,
		"reason":      r.Reason,
		"note":        textOrNil(r.Note),
		"posted_at":   tsOrNil(r.PostedAt),
		"posted_by":   uuidOrNil(r.PostedBy),
		"void_reason": textOrNil(r.VoidReason),
		"created_at":  tsOrNil(r.CreatedAt),
		"row_version": r.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, supplierReturnLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

type supplierReturnLineRequest struct {
	ItemID   string  `json:"item_id" binding:"required"`
	LotID    string  `json:"lot_id" binding:"required"`
	Source   string  `json:"source" binding:"required"`
	BaseQty  string  `json:"base_qty" binding:"required"`
	UnitCost *string `json:"unit_cost"`
}

type supplierReturnRequest struct {
	SupplierID string                      `json:"supplier_id" binding:"required"`
	Reason     string                      `json:"reason" binding:"required"`
	Note       string                      `json:"note"`
	Lines      []supplierReturnLineRequest `json:"lines" binding:"required,min=1,dive"`
	RowVersion int64                       `json:"row_version"`
}

// parseSupplierReturnLines validates the draft lines and prefills each
// unit_cost from the lot's last receipt move when not supplied (spec §5.2 #1;
// editable — an explicit unit_cost wins). ok=false when the response is
// already written.
func parseSupplierReturnLines(c *gin.Context, q *sqlcgen.Queries, reqLines []supplierReturnLineRequest) ([]outboundLine, bool) {
	lines := make([]outboundLine, 0, len(reqLines))
	for _, lr := range reqLines {
		itemID, ok1 := parseUUID(lr.ItemID)
		lotID, ok2 := parseUUID(lr.LotID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return nil, false
		}
		if lr.Source != "purchase" && lr.Source != "consignment" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
			return nil, false
		}
		if _, okL := validateOutboundLot(c, q, itemID, lotID); !okL {
			return nil, false
		}
		baseQty, err := numericOrZero(lr.BaseQty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		if milli, errM := numericToMilli(baseQty); errM != nil || milli <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		var unitCost pgtype.Numeric
		if lr.UnitCost != nil && strings.TrimSpace(*lr.UnitCost) != "" {
			var errN error
			if unitCost, errN = numericOrZero(*lr.UnitCost); errN != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unit_cost"})
				return nil, false
			}
		} else {
			last, errC := q.LastReceiptUnitCost(c.Request.Context(), lotID)
			if errors.Is(errC, pgx.ErrNoRows) {
				unitCost = pgtype.Numeric{Int: big.NewInt(0), Valid: true}
			} else if errC != nil {
				respondInternalError(c, errC)
				return nil, false
			} else {
				unitCost = last
			}
		}
		lines = append(lines, outboundLine{
			ItemID: itemID, LotID: lotID, Source: lr.Source,
			BaseQty: baseQty, UnitCost: unitCost,
		})
	}
	return lines, true
}

// insertSupplierReturnLines replaces the draft's lines (delete + insert).
func insertSupplierReturnLines(c *gin.Context, q *sqlcgen.Queries, r sqlcgen.InventorySupplierReturn, lines []outboundLine) bool {
	if err := q.DeleteSupplierReturnLines(c.Request.Context(), r.ID); err != nil {
		respondInternalError(c, err)
		return false
	}
	for _, l := range lines {
		if _, err := q.InsertSupplierReturnLine(c.Request.Context(), sqlcgen.InsertSupplierReturnLineParams{
			CompanyID: r.CompanyID, MerchantID: r.MerchantID, ReturnID: r.ID,
			ItemID: l.ItemID, LotID: l.LotID, Source: l.Source,
			BaseQty: l.BaseQty, UnitCost: l.UnitCost, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// loadSupplierReturn fetches a live return and checks PermStockReturn at its
// merchant. Writes 400/403/404 and returns false on failure.
func loadSupplierReturn(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventorySupplierReturn, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventorySupplierReturn{}, false
	}
	r, err := q.GetSupplierReturnByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "supplier return not found"})
		return sqlcgen.InventorySupplierReturn{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventorySupplierReturn{}, false
	}
	if !RequirePermissionForMerchant(c, PermStockReturn, r.MerchantID) {
		return sqlcgen.InventorySupplierReturn{}, false
	}
	return r, true
}

// CreateSupplierReturnHandler godoc
// @Summary Create a supplier-return draft (unit_cost prefilled from the lot's last receipt)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body supplierReturnRequest true "Return draft"
// @Success 201 {object} apiResponse
// @Router /merchants/{id}/supplier-returns [post]
func CreateSupplierReturnHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockReturn, merchantID) {
		return
	}
	var req supplierReturnRequest
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
	if _, err := q.GetSupplierByID(c.Request.Context(), supplierID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		} else {
			respondInternalError(c, err)
		}
		return
	}
	lines, okP := parseSupplierReturnLines(c, q, req.Lines)
	if !okP {
		return
	}
	returnNo, errN := nextOutboundNo(c.Request.Context(), q, merchantID, merchantID, "SRET-", "sret:",
		func(day time.Time) (int64, error) {
			return q.CountSupplierReturnsSince(c.Request.Context(), sqlcgen.CountSupplierReturnsSinceParams{
				MerchantID: merchantID, CreatedAt: pgtype.Timestamptz{Time: day, Valid: true},
			})
		})
	if errN != nil {
		respondInternalError(c, errN)
		return
	}
	r, err := q.InsertSupplierReturn(c.Request.Context(), sqlcgen.InsertSupplierReturnParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		SupplierID: supplierID, ReturnNo: returnNo,
		Reason: req.Reason, Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertSupplierReturnLines(c, q, r, lines) {
		return
	}
	saved, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": supplierReturnJSON(r, saved), "meta": gin.H{}})
}

// ListSupplierReturnsHandler godoc
// @Summary List a merchant's supplier returns
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/supplier-returns [get]
func ListSupplierReturnsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockReturn, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListSupplierReturns(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, supplierReturnJSON(r, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetSupplierReturnHandler godoc
// @Summary Fetch one supplier return with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Return UUID"
// @Success 200 {object} apiResponse
// @Router /supplier-returns/{id} [get]
func GetSupplierReturnHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadSupplierReturn(c, q, c.Param("id"))
	if !ok {
		return
	}
	lines, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": supplierReturnJSON(r, lines), "meta": gin.H{}})
}

// UpdateSupplierReturnHandler godoc
// @Summary Replace a draft supplier return (header + full line replace)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Return UUID"
// @Param request body supplierReturnRequest true "Return draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /supplier-returns/{id} [patch]
func UpdateSupplierReturnHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadSupplierReturn(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "return is not editable"})
		return
	}
	var req supplierReturnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(r.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "return changed, reload"})
		return
	}
	supplierID, okS := parseUUID(req.SupplierID)
	if !okS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		return
	}
	if _, err := q.GetSupplierByID(c.Request.Context(), supplierID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
		} else {
			respondInternalError(c, err)
		}
		return
	}
	lines, okP := parseSupplierReturnLines(c, q, req.Lines)
	if !okP {
		return
	}
	updated, err := q.UpdateSupplierReturnDraft(c.Request.Context(), sqlcgen.UpdateSupplierReturnDraftParams{
		SupplierID: supplierID, Reason: req.Reason, Note: pgText(req.Note),
		UpdatedBy: AuthUserID(c), ReturnID: r.ID, ExpectedRowVersion: r.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "return changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if !insertSupplierReturnLines(c, q, updated, lines) {
		return
	}
	saved, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": supplierReturnJSON(updated, saved), "meta": gin.H{}})
}

// allocation is one locked draw from a balance row.
type allocation struct {
	LocationID pgtype.UUID
	Milli      int64
}

// lockReturnAllocation locks every available balance row of lot+source and
// splits baseQty across them oldest-receipt-first (spec §5.2 #1). 409
// insufficient stock when the total falls short; ok=false then.
func lockReturnAllocation(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, l outboundLine) ([]allocation, bool) {
	rows, err := q.LockAvailableByLot(c.Request.Context(), sqlcgen.LockAvailableByLotParams{
		MerchantID: merchantID, LotID: l.LotID, Source: l.Source,
	})
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	needMilli, errN := numericToMilli(l.BaseQty)
	if errN != nil {
		respondInternalError(c, errN)
		return nil, false
	}
	var total int64
	allocs := make([]allocation, 0, len(rows))
	for _, row := range rows {
		m, errM := numericToMilli(row.Quantity)
		if errM != nil {
			respondInternalError(c, errM)
			return nil, false
		}
		allocs = append(allocs, allocation{LocationID: row.LocationID, Milli: m})
		total += m
	}
	if total < needMilli {
		c.JSON(http.StatusConflict, gin.H{"error": "insufficient stock"})
		return nil, false
	}
	return allocs, true
}

// PostSupplierReturnHandler godoc
// @Summary Post a draft supplier return (supplier_return moves + HPP reversal)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Return UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /supplier-returns/{id}/post [post]
func PostSupplierReturnHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadSupplierReturn(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "return is not draft"})
		return
	}
	lines, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Guard every line first: allocate across the locked available balances.
	type returnPlan struct {
		Line      sqlcgen.InventorySupplierReturnLine
		Allocs    []allocation
		NeedMilli int64
	}
	plans := make([]returnPlan, 0, len(lines))
	for _, l := range lines {
		allocs, okA := lockReturnAllocation(c, q, r.MerchantID, outboundLine{
			LotID: l.LotID, Source: l.Source, BaseQty: l.BaseQty,
		})
		if !okA {
			return
		}
		milli, _ := numericToMilli(l.BaseQty)
		plans = append(plans, returnPlan{Line: l, Allocs: allocs, NeedMilli: milli})
	}
	moveDate := time.Now().UTC()
	for _, p := range plans {
		// HPP reversal is once per line with the line total (not per move).
		if p.Line.Source == "purchase" {
			if _, err := q.ReverseReceiptMovingAverage(c.Request.Context(), sqlcgen.ReverseReceiptMovingAverageParams{
				CompanyID: r.CompanyID, MerchantID: r.MerchantID, ItemID: p.Line.ItemID,
				UnitCost: p.Line.UnitCost, BaseQty: p.Line.BaseQty,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		remaining := p.NeedMilli
		for _, a := range p.Allocs {
			if remaining == 0 {
				break
			}
			take := a.Milli
			if take > remaining {
				take = remaining
			}
			remaining -= take
			if _, err := applyStockMove(c.Request.Context(), q, r.MerchantID, r.CompanyID, AuthUserID(c), stockMoveInput{
				Key:      stockKey{LocationID: a.LocationID, LotID: p.Line.LotID, Source: p.Line.Source, Status: "available"},
				ItemID:   p.Line.ItemID,
				Qty:      numericFromMilli(-take),
				UnitCost: p.Line.UnitCost,
				MoveType: "supplier_return",
				RefType:  "supplier_return",
				RefID:    r.ID,
				Reason:   r.Reason,
				MoveDate: moveDate,
			}); err != nil {
				if !respondStockError(c, err) {
					respondInternalError(c, err)
				}
				return
			}
		}
	}
	posted, err := q.MarkSupplierReturnPosted(c.Request.Context(), sqlcgen.MarkSupplierReturnPostedParams{
		PostedBy: AuthUserID(c), ReturnID: r.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "return is not draft"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": supplierReturnJSON(posted, saved), "meta": gin.H{}})
}

type voidOutboundRequest struct {
	Reason string `json:"reason"`
}

// VoidSupplierReturnHandler godoc
// @Summary Void a posted supplier return (reversal moves + HPP re-applied)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Return UUID"
// @Param request body voidOutboundRequest true "Void reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /supplier-returns/{id}/void [post]
func VoidSupplierReturnHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadSupplierReturn(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "posted" {
		c.JSON(http.StatusConflict, gin.H{"error": "return is not posted"})
		return
	}
	var req voidOutboundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "void reason required"})
		return
	}
	moves, okM := loadPostedMoves(c, "supplier_return", r.ID)
	if !okM {
		return
	}
	// Every allocated portion must still be on hand (pola void GR).
	for _, m := range moves {
		row, err := q.LockStockBalance(c.Request.Context(), sqlcgen.LockStockBalanceParams{
			MerchantID: r.MerchantID, LocationID: m.LocationID,
			LotID: m.LotID, Source: m.Source, Status: "available",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "returned stock was already used"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		balMilli, errB := numericToMilli(row.Quantity)
		moveMilli, errM := numericToMilli(m.Quantity)
		if errB != nil || errM != nil {
			respondInternalError(c, errors.New("balance quantity outside numeric(14,3) scale"))
			return
		}
		if balMilli < moveMilli {
			c.JSON(http.StatusConflict, gin.H{"error": "returned stock was already used"})
			return
		}
	}
	// Re-apply the HPP the posting reversed, once per line.
	lines, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, l := range lines {
		if l.Source != "purchase" {
			continue
		}
		if _, err := q.ApplyReceiptMovingAverage(c.Request.Context(), sqlcgen.ApplyReceiptMovingAverageParams{
			CompanyID: r.CompanyID, MerchantID: r.MerchantID, ItemID: l.ItemID,
			UnitCost: l.UnitCost, BaseQty: l.BaseQty,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	moveDate := time.Now().UTC()
	for _, m := range moves {
		if _, err := applyStockMove(c.Request.Context(), q, r.MerchantID, r.CompanyID, AuthUserID(c), stockMoveInput{
			Key:        stockKey{LocationID: m.LocationID, LotID: m.LotID, Source: m.Source, Status: "available"},
			ItemID:     m.ItemID,
			Qty:        negateNumeric(m.Quantity), // the posted move was negative
			UnitCost:   m.UnitCost,
			MoveType:   "reversal",
			RefType:    "supplier_return",
			RefID:      r.ID,
			ReversalOf: m.ID,
			Reason:     req.Reason,
			MoveDate:   moveDate,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	voided, err := q.MarkSupplierReturnVoid(c.Request.Context(), sqlcgen.MarkSupplierReturnVoidParams{
		VoidReason: req.Reason, UpdatedBy: AuthUserID(c), ReturnID: r.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "return is not posted"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListSupplierReturnLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": supplierReturnJSON(voided, saved), "meta": gin.H{}})
}

// §5.2 #2 destructions.

func destructionLineJSON(l sqlcgen.InventoryDestructionLine) gin.H {
	return gin.H{
		"id":          l.ID.String(),
		"item_id":     l.ItemID.String(),
		"lot_id":      l.LotID.String(),
		"source":      l.Source,
		"from_status": l.FromStatus,
		"base_qty":    numericExact(l.BaseQty),
	}
}

// destructionWitnesses decodes the jsonb name array.
func destructionWitnesses(raw []byte) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func destructionJSON(d sqlcgen.InventoryDestruction, lines []sqlcgen.InventoryDestructionLine) gin.H {
	out := gin.H{
		"id":             d.ID.String(),
		"merchant_id":    d.MerchantID.String(),
		"location_id":    d.LocationID.String(),
		"occurred_on":    dateText(d.OccurredOn),
		"destruction_no": d.DestructionNo,
		"method":         d.Method,
		"witnesses":      destructionWitnesses(d.Witnesses),
		"status":         d.Status,
		"note":           textOrNil(d.Note),
		"posted_at":      tsOrNil(d.PostedAt),
		"posted_by":      uuidOrNil(d.PostedBy),
		"void_reason":    textOrNil(d.VoidReason),
		"created_at":     tsOrNil(d.CreatedAt),
		"row_version":    d.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, destructionLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

type destructionLineRequest struct {
	ItemID     string `json:"item_id" binding:"required"`
	LotID      string `json:"lot_id" binding:"required"`
	Source     string `json:"source" binding:"required"`
	FromStatus string `json:"from_status" binding:"required"`
	BaseQty    string `json:"base_qty" binding:"required"`
}

type destructionRequest struct {
	LocationID string                   `json:"location_id" binding:"required"`
	OccurredOn string                   `json:"occurred_on" binding:"required"`
	Method     string                   `json:"method" binding:"required"`
	Witnesses  []string                 `json:"witnesses"`
	Note       string                   `json:"note"`
	Lines      []destructionLineRequest `json:"lines" binding:"required,min=1,dive"`
	RowVersion int64                    `json:"row_version"`
}

// parseDestructionLines validates the draft lines (source and from_status
// follow the migration CHECKs, 000081). ok=false when the response is
// already written.
func parseDestructionLines(c *gin.Context, q *sqlcgen.Queries, reqLines []destructionLineRequest) ([]outboundLine, bool) {
	lines := make([]outboundLine, 0, len(reqLines))
	for _, lr := range reqLines {
		itemID, ok1 := parseUUID(lr.ItemID)
		lotID, ok2 := parseUUID(lr.LotID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return nil, false
		}
		switch lr.Source {
		case "purchase", "program", "grant", "consignment":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
			return nil, false
		}
		switch lr.FromStatus {
		case "available", "quarantine", "damaged":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from_status"})
			return nil, false
		}
		if _, okL := validateOutboundLot(c, q, itemID, lotID); !okL {
			return nil, false
		}
		baseQty, err := numericOrZero(lr.BaseQty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		if milli, errM := numericToMilli(baseQty); errM != nil || milli <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		lines = append(lines, outboundLine{
			ItemID: itemID, LotID: lotID, Source: lr.Source,
			Status: lr.FromStatus, BaseQty: baseQty,
		})
	}
	return lines, true
}

// insertDestructionLines replaces the draft's lines (delete + insert).
func insertDestructionLines(c *gin.Context, q *sqlcgen.Queries, d sqlcgen.InventoryDestruction, lines []outboundLine) bool {
	if err := q.DeleteDestructionLines(c.Request.Context(), d.ID); err != nil {
		respondInternalError(c, err)
		return false
	}
	for _, l := range lines {
		if _, err := q.InsertDestructionLine(c.Request.Context(), sqlcgen.InsertDestructionLineParams{
			CompanyID: d.CompanyID, MerchantID: d.MerchantID, DestructionID: d.ID,
			ItemID: l.ItemID, LotID: l.LotID, Source: l.Source,
			FromStatus: l.Status, BaseQty: l.BaseQty, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// destructionBodyWitnesses renders the request's witness list for storage.
func destructionBodyWitnesses(names []string) []byte {
	if names == nil {
		names = []string{}
	}
	raw, err := json.Marshal(names)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

// loadDestruction fetches a live destruction and checks PermStockDestroy at
// its merchant. Writes 400/403/404 and returns false on failure.
func loadDestruction(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryDestruction, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryDestruction{}, false
	}
	d, err := q.GetDestructionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "destruction not found"})
		return sqlcgen.InventoryDestruction{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryDestruction{}, false
	}
	if !RequirePermissionForMerchant(c, PermStockDestroy, d.MerchantID) {
		return sqlcgen.InventoryDestruction{}, false
	}
	return d, true
}

// CreateDestructionHandler godoc
// @Summary Create a destruction draft (berita acara fields included)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body destructionRequest true "Destruction draft"
// @Success 201 {object} apiResponse
// @Router /merchants/{id}/destructions [post]
func CreateDestructionHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockDestroy, merchantID) {
		return
	}
	var req destructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	locationID, okL := parseUUID(req.LocationID)
	if !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	occurredOn, okD := parseDateStr(req.OccurredOn)
	if !okD {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid occurred_on"})
		return
	}
	lines, okP := parseDestructionLines(c, q, req.Lines)
	if !okP {
		return
	}
	destructionNo, errN := nextOutboundNo(c.Request.Context(), q, merchantID, merchantID, "DSTR-", "dstr:",
		func(day time.Time) (int64, error) {
			return q.CountDestructionsSince(c.Request.Context(), sqlcgen.CountDestructionsSinceParams{
				MerchantID: merchantID, CreatedAt: pgtype.Timestamptz{Time: day, Valid: true},
			})
		})
	if errN != nil {
		respondInternalError(c, errN)
		return
	}
	d, err := q.InsertDestruction(c.Request.Context(), sqlcgen.InsertDestructionParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		LocationID: locationID, OccurredOn: occurredOn,
		DestructionNo: destructionNo, Method: req.Method,
		Witnesses: destructionBodyWitnesses(req.Witnesses),
		Note:      pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertDestructionLines(c, q, d, lines) {
		return
	}
	saved, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": destructionJSON(d, saved), "meta": gin.H{}})
}

// ListDestructionsHandler godoc
// @Summary List a merchant's destructions
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/destructions [get]
func ListDestructionsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockDestroy, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListDestructions(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, d := range rows {
		data = append(data, destructionJSON(d, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetDestructionHandler godoc
// @Summary Fetch one destruction with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Destruction UUID"
// @Success 200 {object} apiResponse
// @Router /destructions/{id} [get]
func GetDestructionHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	d, ok := loadDestruction(c, q, c.Param("id"))
	if !ok {
		return
	}
	lines, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": destructionJSON(d, lines), "meta": gin.H{}})
}

// UpdateDestructionHandler godoc
// @Summary Replace a draft destruction (header + full line replace)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Destruction UUID"
// @Param request body destructionRequest true "Destruction draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /destructions/{id} [patch]
func UpdateDestructionHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	d, ok := loadDestruction(c, q, c.Param("id"))
	if !ok {
		return
	}
	if d.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "destruction is not editable"})
		return
	}
	var req destructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(d.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "destruction changed, reload"})
		return
	}
	locationID, okL := parseUUID(req.LocationID)
	if !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	occurredOn, okD := parseDateStr(req.OccurredOn)
	if !okD {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid occurred_on"})
		return
	}
	lines, okP := parseDestructionLines(c, q, req.Lines)
	if !okP {
		return
	}
	updated, err := q.UpdateDestructionDraft(c.Request.Context(), sqlcgen.UpdateDestructionDraftParams{
		LocationID: locationID, OccurredOn: occurredOn, Method: req.Method,
		Witnesses: destructionBodyWitnesses(req.Witnesses), Note: pgText(req.Note),
		UpdatedBy: AuthUserID(c), DestructionID: d.ID, ExpectedRowVersion: d.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "destruction changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if !insertDestructionLines(c, q, updated, lines) {
		return
	}
	saved, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": destructionJSON(updated, saved), "meta": gin.H{}})
}

// destructionReportRequired reports whether the narcotics four-eyes guard
// fails: controlled drug classes need ≥2 witnesses and a method (spec §5.2 #2).
func destructionReportRequired(drugClass pgtype.Text, witnesses []string, method string) bool {
	switch drugClass.String {
	case "narkotika", "psikotropika", "prekursor":
	default:
		return false
	}
	count := 0
	for _, w := range witnesses {
		if strings.TrimSpace(w) != "" {
			count++
		}
	}
	return count < 2 || strings.TrimSpace(method) == ""
}

// PostDestructionHandler godoc
// @Summary Post a draft destruction (destruction moves; narcotics report guard)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Destruction UUID"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /destructions/{id}/post [post]
func PostDestructionHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	d, ok := loadDestruction(c, q, c.Param("id"))
	if !ok {
		return
	}
	if d.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "destruction is not draft"})
		return
	}
	lines, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Guard every line BEFORE the first write: balance availability and the
	// narcotics four-eyes report (spec §5.2 #2).
	type destructionPlan struct {
		Line      sqlcgen.InventoryDestructionLine
		Item      sqlcgen.InventoryItem
		UnitCost  pgtype.Numeric
		BalID     pgtype.UUID
		NeedMilli int64
	}
	plans := make([]destructionPlan, 0, len(lines))
	for _, l := range lines {
		row, err := q.LockStockBalance(c.Request.Context(), sqlcgen.LockStockBalanceParams{
			MerchantID: d.MerchantID, LocationID: d.LocationID,
			LotID: l.LotID, Source: l.Source, Status: l.FromStatus,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "insufficient stock"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		balMilli, errB := numericToMilli(row.Quantity)
		needMilli, errN := numericToMilli(l.BaseQty)
		if errB != nil || errN != nil {
			respondInternalError(c, errors.New("balance quantity outside numeric(14,3) scale"))
			return
		}
		if balMilli < needMilli {
			c.JSON(http.StatusConflict, gin.H{"error": "insufficient stock"})
			return
		}
		item, err := q.GetInventoryItemByID(c.Request.Context(), l.ItemID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var unitCost pgtype.Numeric
		if l.Source == "purchase" || l.Source == "consignment" {
			avg, errA := q.GetItemAvgCost(c.Request.Context(), sqlcgen.GetItemAvgCostParams{
				MerchantID: d.MerchantID, ItemID: l.ItemID,
			})
			if errA != nil {
				respondInternalError(c, errA)
				return
			}
			unitCost = avg
		} else {
			unitCost = pgtype.Numeric{Int: big.NewInt(0), Valid: true}
		}
		if destructionReportRequired(item.DrugClass, destructionWitnesses(d.Witnesses), d.Method) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "destruction report required"})
			return
		}
		plans = append(plans, destructionPlan{
			Line: l, Item: item, UnitCost: unitCost, BalID: row.ID, NeedMilli: needMilli,
		})
	}
	moveDate := time.Now().UTC()
	for _, p := range plans {
		if _, err := applyStockMove(c.Request.Context(), q, d.MerchantID, d.CompanyID, AuthUserID(c), stockMoveInput{
			Key:      stockKey{LocationID: d.LocationID, LotID: p.Line.LotID, Source: p.Line.Source, Status: p.Line.FromStatus},
			ItemID:   p.Line.ItemID,
			Qty:      numericFromMilli(-p.NeedMilli),
			UnitCost: p.UnitCost,
			MoveType: "destruction",
			RefType:  "destruction",
			RefID:    d.ID,
			Reason:   d.Method,
			MoveDate: moveDate,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	posted, err := q.MarkDestructionPosted(c.Request.Context(), sqlcgen.MarkDestructionPostedParams{
		PostedBy: AuthUserID(c), DestructionID: d.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "destruction is not draft"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": destructionJSON(posted, saved), "meta": gin.H{}})
}

// VoidDestructionHandler godoc
// @Summary Void a posted destruction (positive reversal moves, HPP untouched)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Destruction UUID"
// @Param request body voidOutboundRequest true "Void reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /destructions/{id}/void [post]
func VoidDestructionHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	d, ok := loadDestruction(c, q, c.Param("id"))
	if !ok {
		return
	}
	if d.Status != "posted" {
		c.JSON(http.StatusConflict, gin.H{"error": "destruction is not posted"})
		return
	}
	var req voidOutboundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "void reason required"})
		return
	}
	moves, okM := loadPostedMoves(c, "destruction", d.ID)
	if !okM {
		return
	}
	moveDate := time.Now().UTC()
	for _, m := range moves {
		if _, err := applyStockMove(c.Request.Context(), q, d.MerchantID, d.CompanyID, AuthUserID(c), stockMoveInput{
			Key:        stockKey{LocationID: m.LocationID, LotID: m.LotID, Source: m.Source, Status: m.Status},
			ItemID:     m.ItemID,
			Qty:        negateNumeric(m.Quantity), // the posted move was negative
			UnitCost:   m.UnitCost,
			MoveType:   "reversal",
			RefType:    "destruction",
			RefID:      d.ID,
			ReversalOf: m.ID,
			Reason:     req.Reason,
			MoveDate:   moveDate,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	voided, err := q.MarkDestructionVoid(c.Request.Context(), sqlcgen.MarkDestructionVoidParams{
		VoidReason: req.Reason, UpdatedBy: AuthUserID(c), DestructionID: d.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "destruction is not posted"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListDestructionLines(c.Request.Context(), d.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": destructionJSON(voided, saved), "meta": gin.H{}})
}

// §5.2 #4 inter-merchant transfers.

func interMerchantTransferLineJSON(l sqlcgen.InventoryInterMerchantTransferLine) gin.H {
	return gin.H{
		"id":        l.ID.String(),
		"item_id":   l.ItemID.String(),
		"lot_id":    l.LotID.String(),
		"source":    l.Source,
		"status":    l.Status,
		"base_qty":  numericExact(l.BaseQty),
		"unit_cost": numericExact(l.UnitCost),
	}
}

func interMerchantTransferJSON(t sqlcgen.InventoryInterMerchantTransfer, lines []sqlcgen.InventoryInterMerchantTransferLine) gin.H {
	out := gin.H{
		"id":               t.ID.String(),
		"company_id":       t.CompanyID.String(),
		"from_merchant_id": t.FromMerchantID.String(),
		"to_merchant_id":   t.ToMerchantID.String(),
		"from_location_id": t.FromLocationID.String(),
		"to_location_id":   t.ToLocationID.String(),
		"imt_no":           t.ImtNo,
		"status":           t.Status,
		"note":             textOrNil(t.Note),
		"sent_at":          tsOrNil(t.SentAt),
		"sent_by":          uuidOrNil(t.SentBy),
		"received_at":      tsOrNil(t.ReceivedAt),
		"received_by":      uuidOrNil(t.ReceivedBy),
		"received_on":      dateOrNil(t.ReceivedOn),
		"created_at":       tsOrNil(t.CreatedAt),
		"row_version":      t.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, interMerchantTransferLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

type interMerchantTransferLineRequest struct {
	ItemID  string `json:"item_id" binding:"required"`
	LotID   string `json:"lot_id" binding:"required"`
	Source  string `json:"source" binding:"required"`
	Status  string `json:"status" binding:"required"`
	BaseQty string `json:"base_qty" binding:"required"`
}

type interMerchantTransferRequest struct {
	ToMerchantID   string                             `json:"to_merchant_id" binding:"required"`
	FromLocationID string                             `json:"from_location_id" binding:"required"`
	ToLocationID   string                             `json:"to_location_id" binding:"required"`
	Note           string                             `json:"note"`
	Lines          []interMerchantTransferLineRequest `json:"lines" binding:"required,min=1,dive"`
}

// parseIMTLines validates the send lines (source/status follow the migration
// CHECKs, 000081). ok=false when the response is already written.
func parseIMTLines(c *gin.Context, q *sqlcgen.Queries, reqLines []interMerchantTransferLineRequest) ([]outboundLine, bool) {
	lines := make([]outboundLine, 0, len(reqLines))
	for _, lr := range reqLines {
		itemID, ok1 := parseUUID(lr.ItemID)
		lotID, ok2 := parseUUID(lr.LotID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return nil, false
		}
		switch lr.Source {
		case "purchase", "program", "grant", "consignment":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
			return nil, false
		}
		switch lr.Status {
		case "available", "quarantine", "damaged":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return nil, false
		}
		if _, okL := validateOutboundLot(c, q, itemID, lotID); !okL {
			return nil, false
		}
		baseQty, err := numericOrZero(lr.BaseQty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		if milli, errM := numericToMilli(baseQty); errM != nil || milli <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return nil, false
		}
		lines = append(lines, outboundLine{
			ItemID: itemID, LotID: lotID, Source: lr.Source,
			Status: lr.Status, BaseQty: baseQty,
		})
	}
	return lines, true
}

// loadInterMerchantTransfer fetches a live transfer document (company-level
// RLS). The permission check stays with the caller — senders and receivers
// exercise PermStockManage at their own merchant. Writes 400/404 and returns
// false on failure.
func loadInterMerchantTransfer(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryInterMerchantTransfer, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryInterMerchantTransfer{}, false
	}
	t, err := q.GetInterMerchantTransferByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "inter-merchant transfer not found"})
		return sqlcgen.InventoryInterMerchantTransfer{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryInterMerchantTransfer{}, false
	}
	return t, true
}

// CreateInterMerchantTransferHandler godoc
// @Summary Send an inter-merchant transfer (status sent, transfer_out moves)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Sender merchant UUID"
// @Param request body interMerchantTransferRequest true "Transfer shipment"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/inter-merchant-transfers [post]
func CreateInterMerchantTransferHandler(c *gin.Context) {
	fromMerchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, fromMerchantID) {
		return
	}
	var req interMerchantTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	toMerchantID, okT := parseUUID(req.ToMerchantID)
	if !okT || toMerchantID == fromMerchantID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to_merchant_id"})
		return
	}
	fromLocationID, okFL := parseUUID(req.FromLocationID)
	toLocationID, okTL := parseUUID(req.ToLocationID)
	if !okFL || !okTL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	fromLoc, err := q.GetLocationByID(c.Request.Context(), fromLocationID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && fromLoc.MerchantID != fromMerchantID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from_location_id"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	toLoc, err := q.GetLocationByID(c.Request.Context(), toLocationID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && toLoc.MerchantID != toMerchantID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to_location_id"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	lines, okP := parseIMTLines(c, q, req.Lines)
	if !okP {
		return
	}
	// Guard every line's sender balance before the first write.
	locked := make([]int64, 0, len(lines))
	for _, l := range lines {
		row, err := q.LockStockBalance(c.Request.Context(), sqlcgen.LockStockBalanceParams{
			MerchantID: fromMerchantID, LocationID: fromLocationID,
			LotID: l.LotID, Source: l.Source, Status: l.Status,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "insufficient stock"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		balMilli, errB := numericToMilli(row.Quantity)
		needMilli, errN := numericToMilli(l.BaseQty)
		if errB != nil || errN != nil {
			respondInternalError(c, errors.New("balance quantity outside numeric(14,3) scale"))
			return
		}
		if balMilli < needMilli {
			c.JSON(http.StatusConflict, gin.H{"error": "insufficient stock"})
			return
		}
		locked = append(locked, needMilli)
	}
	// Daily number per company (spec §9.7): lock the company-scoped
	// "imt:" sequence on the sender's business day.
	imtNo, errN := nextOutboundNo(c.Request.Context(), q, fromMerchantID, AuthCompanyID(c), "IMT-", "imt:",
		func(day time.Time) (int64, error) {
			return q.CountInterMerchantTransfersSince(c.Request.Context(), sqlcgen.CountInterMerchantTransfersSinceParams{
				CompanyID: AuthCompanyID(c), CreatedAt: pgtype.Timestamptz{Time: day, Valid: true},
			})
		})
	if errN != nil {
		respondInternalError(c, errN)
		return
	}
	t, err := q.InsertInterMerchantTransfer(c.Request.Context(), sqlcgen.InsertInterMerchantTransferParams{
		CompanyID: AuthCompanyID(c), FromMerchantID: fromMerchantID, ToMerchantID: toMerchantID,
		FromLocationID: fromLocationID, ToLocationID: toLocationID,
		ImtNo: imtNo, Note: pgText(req.Note), SentBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	moveDate := time.Now().UTC()
	for i, l := range lines {
		// unit_cost = the sender's avg cost at send time (0 for program/grant).
		var unitCost pgtype.Numeric
		if l.Source == "purchase" || l.Source == "consignment" {
			avg, errA := q.GetItemAvgCost(c.Request.Context(), sqlcgen.GetItemAvgCostParams{
				MerchantID: fromMerchantID, ItemID: l.ItemID,
			})
			if errA != nil {
				respondInternalError(c, errA)
				return
			}
			unitCost = avg
		} else {
			unitCost = pgtype.Numeric{Int: big.NewInt(0), Valid: true}
		}
		if _, err := q.InsertInterMerchantTransferLine(c.Request.Context(), sqlcgen.InsertInterMerchantTransferLineParams{
			CompanyID: AuthCompanyID(c), InterTransferID: t.ID,
			ItemID: l.ItemID, LotID: l.LotID, Source: l.Source, Status: l.Status,
			BaseQty: l.BaseQty, UnitCost: unitCost, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		if _, err := applyStockMove(c.Request.Context(), q, fromMerchantID, AuthCompanyID(c), AuthUserID(c), stockMoveInput{
			Key:      stockKey{LocationID: fromLocationID, LotID: l.LotID, Source: l.Source, Status: l.Status},
			ItemID:   l.ItemID,
			Qty:      numericFromMilli(-locked[i]),
			UnitCost: unitCost,
			MoveType: "transfer_out",
			RefType:  "inter_merchant_transfer",
			RefID:    t.ID,
			MoveDate: moveDate,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	saved, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": interMerchantTransferJSON(t, saved), "meta": gin.H{}})
}

// ListInterMerchantTransfersHandler godoc
// @Summary List a sender merchant's inter-merchant transfers (?status=sent = in transit)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param status query string false "Filter by status (sent|received|cancelled)"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/inter-merchant-transfers [get]
func ListInterMerchantTransfersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var status pgtype.Text
	if v := c.Query("status"); v != "" {
		status = pgtype.Text{String: v, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListInterMerchantTransfers(c.Request.Context(), sqlcgen.ListInterMerchantTransfersParams{
		CompanyID: AuthCompanyID(c), Status: status,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, t := range rows {
		data = append(data, interMerchantTransferJSON(t, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetInterMerchantTransferHandler godoc
// @Summary Fetch one inter-merchant transfer with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Transfer UUID"
// @Success 200 {object} apiResponse
// @Router /inter-merchant-transfers/{id} [get]
func GetInterMerchantTransferHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	t, ok := loadInterMerchantTransfer(c, q, c.Param("id"))
	if !ok {
		return
	}
	if AuthMerchantID(c) == t.ToMerchantID {
		if !RequirePermissionForMerchant(c, PermStockManage, t.ToMerchantID) {
			return
		}
	} else if !RequirePermissionForMerchant(c, PermStockManage, t.FromMerchantID) {
		return
	}
	lines, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": interMerchantTransferJSON(t, lines), "meta": gin.H{}})
}

// ReceiveInterMerchantTransferHandler godoc
// @Summary Receive an in-transit transfer (transfer_in moves + HPP at receiver)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Transfer UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inter-merchant-transfers/{id}/receive [post]
func ReceiveInterMerchantTransferHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	t, ok := loadInterMerchantTransfer(c, q, c.Param("id"))
	if !ok {
		return
	}
	// Only the receiving merchant may receive (spec §5.2 #4).
	if AuthMerchantID(c) != t.ToMerchantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "wrong receiving merchant"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, t.ToMerchantID) {
		return
	}
	if t.Status != "sent" {
		c.JSON(http.StatusConflict, gin.H{"error": "transfer is not sent"})
		return
	}
	lines, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// received_on follows the receiving merchant's business day (txDayStart).
	loc, err := merchantLocation(c.Request.Context(), q, t.ToMerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	dayStart, err := txDayStart(c.Request.Context(), q, loc)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, l := range lines {
		// HPP at the receiver moves forward with the shipped cost (spec §9.3).
		if l.Source == "purchase" {
			if _, err := q.ApplyReceiptMovingAverage(c.Request.Context(), sqlcgen.ApplyReceiptMovingAverageParams{
				CompanyID: t.CompanyID, MerchantID: t.ToMerchantID, ItemID: l.ItemID,
				UnitCost: l.UnitCost, BaseQty: l.BaseQty,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		if _, err := applyStockMove(c.Request.Context(), q, t.ToMerchantID, t.CompanyID, AuthUserID(c), stockMoveInput{
			Key:      stockKey{LocationID: t.ToLocationID, LotID: l.LotID, Source: l.Source, Status: l.Status},
			ItemID:   l.ItemID,
			Qty:      l.BaseQty,
			UnitCost: l.UnitCost,
			MoveType: "transfer_in",
			RefType:  "inter_merchant_transfer",
			RefID:    t.ID,
			MoveDate: dayStart,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	received, err := q.MarkInterMerchantTransferReceived(c.Request.Context(), sqlcgen.MarkInterMerchantTransferReceivedParams{
		ReceivedBy: AuthUserID(c), ReceivedOn: pgDate(dayStart), TransferID: t.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "transfer is not sent"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": interMerchantTransferJSON(received, saved), "meta": gin.H{}})
}

// CancelInterMerchantTransferHandler godoc
// @Summary Cancel an in-transit transfer (stock + HPP back at the sender)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Transfer UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inter-merchant-transfers/{id}/cancel [post]
func CancelInterMerchantTransferHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	t, ok := loadInterMerchantTransfer(c, q, c.Param("id"))
	if !ok {
		return
	}
	// Cancel is the sender's prerogative (spec §5.2 #4): the active session
	// must be the sender merchant before the permission check.
	if AuthMerchantID(c) != t.FromMerchantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "wrong sending merchant"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, t.FromMerchantID) {
		return
	}
	if t.Status != "sent" {
		c.JSON(http.StatusConflict, gin.H{"error": "transfer is not sent"})
		return
	}
	lines, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, l := range lines {
		// Re-apply the sender's HPP with the shipped cost (spec §9.3).
		if l.Source == "purchase" {
			if _, err := q.ApplyReceiptMovingAverage(c.Request.Context(), sqlcgen.ApplyReceiptMovingAverageParams{
				CompanyID: t.CompanyID, MerchantID: t.FromMerchantID, ItemID: l.ItemID,
				UnitCost: l.UnitCost, BaseQty: l.BaseQty,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		if _, err := applyStockMove(c.Request.Context(), q, t.FromMerchantID, t.CompanyID, AuthUserID(c), stockMoveInput{
			Key:      stockKey{LocationID: t.FromLocationID, LotID: l.LotID, Source: l.Source, Status: l.Status},
			ItemID:   l.ItemID,
			Qty:      l.BaseQty,
			UnitCost: l.UnitCost,
			MoveType: "transfer_in",
			RefType:  "inter_merchant_transfer",
			RefID:    t.ID,
			MoveDate: time.Now().UTC(),
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	cancelled, err := q.MarkInterMerchantTransferCancelled(c.Request.Context(), sqlcgen.MarkInterMerchantTransferCancelledParams{
		UpdatedBy: AuthUserID(c), TransferID: t.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "transfer is not sent"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListInterMerchantTransferLines(c.Request.Context(), t.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": interMerchantTransferJSON(cancelled, saved), "meta": gin.H{}})
}
