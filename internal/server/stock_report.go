// internal/server/stock_report.go
// Stock reports (plan 2026-10-07-inventory-17a Task 6, spec
// 2026-10-07-inventory-17-stock-procurement §4.6): stock on hand with
// valuation, stock card with running balance, near-expiry buckets and
// reorder suggestions. Read-only — no stock_balance or stock_move writes;
// mutations stay in the locked ledger (stock_ledger.go). Money renders as
// an exact decimal string; avg_cost keeps its numeric(19,6) scale and
// rupiah values are rounded to cents (spec §8.3).
package server

import (
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermStockView guards read-only stock reports (permission
// inventory.stock.view, migration 000071).
const PermStockView = "inventory.stock.view"

// ratFromString parses an exact decimal string; quantity and cost columns
// always render as finite decimals.
func ratFromString(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// optionalQueryUUID reads an optional UUID query parameter; on a malformed
// value it writes 400 and returns false.
func optionalQueryUUID(c *gin.Context, name string) (pgtype.UUID, bool) {
	v := c.Query(name)
	if v == "" {
		return pgtype.UUID{}, true
	}
	id, ok := parseUUID(v)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return pgtype.UUID{}, false
	}
	return id, true
}

// nargQueryText maps an optional query string to a nullable text param.
func nargQueryText(c *gin.Context, name string) pgtype.Text {
	v := c.Query(name)
	return pgtype.Text{String: v, Valid: v != ""}
}

// optionalQueryDate reads an optional YYYY-MM-DD query parameter.
func optionalQueryDate(c *gin.Context, name string) (pgtype.Date, bool) {
	v := c.Query(name)
	if v == "" {
		return pgtype.Date{}, true
	}
	d, ok := parseDateStr(v)
	if !ok || !d.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return pgtype.Date{}, false
	}
	return d, true
}

// §4.6 stock on hand + valuation.

// StockOnHandHandler godoc
// @Summary Stock on hand per location/item/lot with valuation
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param location_id query string false "Filter by location UUID"
// @Param item_id query string false "Filter by item UUID"
// @Param q query string false "Search item code or name"
// @Param status query string false "available|quarantine|damaged"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/stock [get]
func StockOnHandHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	locID, okL := optionalQueryUUID(c, "location_id")
	if !okL {
		return
	}
	itemID, okI := optionalQueryUUID(c, "item_id")
	if !okI {
		return
	}
	status := nargQueryText(c, "status")
	if status.Valid && !validStockStatus(status.String) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	var rows []sqlcgen.ListStockOnHandRow
	var err error
	if status.Valid {
		// Status filter requested → the dedicated status query (spec §5.2 #5).
		byStatus, errS := q.ListStockOnHandByStatus(c.Request.Context(), sqlcgen.ListStockOnHandByStatusParams{
			MerchantID: merchantID,
			Status:     status.String,
			LocationID: locID,
			ItemID:     itemID,
			Q:          nargQueryText(c, "q"),
		})
		if errS == nil {
			rows = make([]sqlcgen.ListStockOnHandRow, len(byStatus))
			for i, r := range byStatus {
				rows[i] = sqlcgen.ListStockOnHandRow(r)
			}
		}
		err = errS
	} else {
		rows, err = q.ListStockOnHand(c.Request.Context(), sqlcgen.ListStockOnHandParams{
			MerchantID: merchantID,
			LocationID: locID,
			ItemID:     itemID,
			Status:     status,
			Q:          nargQueryText(c, "q"),
		})
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	total := new(big.Rat)
	for _, r := range rows {
		total.Add(total, ratFromString(numericExact(r.Value)))
		out = append(out, gin.H{
			"location": r.LocationID.String(),
			"item": gin.H{
				"id":        r.ItemID.String(),
				"code":      r.ItemCode,
				"name":      r.ItemName,
				"base_unit": textOrNil(r.BaseUnitCode),
			},
			"lot": gin.H{
				"batch_no":    r.BatchNo,
				"expiry_date": dateOrNil(r.ExpiryDate),
				"is_recalled": r.IsRecalled,
			},
			"source":   r.Source,
			"status":   r.Status,
			"quantity": numericExact(r.Quantity),
			"avg_cost": numericExact(r.AvgCost),
			"value":    numericExact(r.Value),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"rows":        out,
		"total_value": total.FloatString(2),
	}, "meta": gin.H{}})
}

// §4.6 stock card.

// StockCardHandler godoc
// @Summary Stock card: opening balance plus moves with a running balance
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param item_id query string true "Item UUID"
// @Param location_id query string false "Filter by location UUID"
// @Param from query string false "YYYY-MM-DD inclusive"
// @Param to query string false "YYYY-MM-DD inclusive"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/stock-card [get]
func StockCardHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	rawItem := c.Query("item_id")
	itemID, okI := parseUUID(rawItem)
	if !okI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_id"})
		return
	}
	locID, okL := optionalQueryUUID(c, "location_id")
	if !okL {
		return
	}
	fromDate, okF := optionalQueryDate(c, "from")
	if !okF {
		return
	}
	toDate, okT := optionalQueryDate(c, "to")
	if !okT {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	opening, err := q.SumStockCardOpening(c.Request.Context(), sqlcgen.SumStockCardOpeningParams{
		MerchantID: merchantID,
		ItemID:     itemID,
		LocationID: locID,
		FromDate:   fromDate,
		ToDate:     toDate,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	moves, err := q.ListStockCardMoves(c.Request.Context(), sqlcgen.ListStockCardMovesParams{
		MerchantID: merchantID,
		ItemID:     itemID,
		LocationID: locID,
		FromDate:   fromDate,
		ToDate:     toDate,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	balance := ratFromString(numericExact(opening))
	out := make([]gin.H, 0, len(moves))
	for _, m := range moves {
		balance.Add(balance, ratFromString(numericExact(m.Quantity)))
		out = append(out, gin.H{
			"id":          m.ID.String(),
			"move_date":   dateText(m.MoveDate),
			"created_at":  tsOrNil(m.CreatedAt),
			"move_type":   m.MoveType,
			"location_id": m.LocationID.String(),
			"lot_id":      m.LotID.String(),
			"batch_no":    m.BatchNo,
			"expiry_date": dateOrNil(m.ExpiryDate),
			"source":      m.Source,
			"status":      m.Status,
			"quantity":    numericExact(m.Quantity),
			"unit_cost":   numericExact(m.UnitCost),
			"ref_type":    textOrNil(m.RefType),
			"ref_id":      uuidOrNil(m.RefID),
			"reason":      textOrNil(m.Reason),
			"balance":     balance.FloatString(3),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"item_id":         itemID.String(),
		"opening_balance": numericExact(opening),
		"moves":           out,
	}, "meta": gin.H{}})
}

// §4.6 near-expiry buckets.

// nearExpiryBucket is one ED-dekat group: quantity and rupiah value.
func nearExpiryBucket(quantity pgtype.Numeric, value *big.Rat) gin.H {
	return gin.H{
		"quantity": numericExact(quantity),
		"value":    value.FloatString(2),
	}
}

// StockNearExpiryHandler godoc
// @Summary Near-expiry buckets (expired, within setting 1 and 2 months)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/stock/near-expiry [get]
func StockNearExpiryHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Buckets follow the merchant's day and its warehouse settings
	// (migration defaults 3 and 6 months when no setting row exists).
	m1, m2 := int16(3), int16(6)
	setting, err := q.GetMerchantNearExpiryMonths(c.Request.Context(), merchantID)
	if err == nil {
		m1, m2 = setting.NearExpiryMonths1, setting.NearExpiryMonths2
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	loc, errLoc := merchantLocation(c.Request.Context(), q, merchantID)
	if errLoc != nil {
		respondInternalError(c, errLoc)
		return
	}
	// The merchant-local day comes from the transaction time (merchant_day.go),
	// not raw time.Now().
	today, errDay := txDayStart(c.Request.Context(), q, loc)
	if errDay != nil {
		respondInternalError(c, errDay)
		return
	}
	cut1 := today.AddDate(0, int(m1), 0)
	cut2 := today.AddDate(0, int(m2), 0)
	rows, err := q.ListNearExpiryTotals(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	expiredQty, in1Qty, in2Qty := pgtype.Numeric{}, pgtype.Numeric{}, pgtype.Numeric{}
	expiredVal, in1Val, in2Val := new(big.Rat), new(big.Rat), new(big.Rat)
	for _, r := range rows {
		ed := r.ExpiryDate.Time
		switch {
		case ed.Before(today):
			expiredQty = mustAddNumeric(expiredQty, r.Quantity)
			expiredVal.Add(expiredVal, ratFromString(numericExact(r.Value)))
		case !ed.After(cut1):
			in1Qty = mustAddNumeric(in1Qty, r.Quantity)
			in1Val.Add(in1Val, ratFromString(numericExact(r.Value)))
		case !ed.After(cut2):
			in2Qty = mustAddNumeric(in2Qty, r.Quantity)
			in2Val.Add(in2Val, ratFromString(numericExact(r.Value)))
		}
	}
	// Opened containers (spec 2026-10-20-inventory-17b §7.2 #1) are merged as
	// a SEPARATE bud_containers list: their contents still sit in the lot
	// balance above, so folding them into the buckets would double-count.
	budRows, errBud := q.ListNearExpiryBudContainers(c.Request.Context(), merchantID)
	if errBud != nil {
		respondInternalError(c, errBud)
		return
	}
	budContainers := make([]gin.H, 0, len(budRows))
	for _, r := range budRows {
		bud := r.BudAt.Time
		var bucket string
		switch {
		case bud.Before(today):
			bucket = "expired"
		case !bud.After(cut1):
			bucket = "within_1"
		case !bud.After(cut2):
			bucket = "within_2"
		default:
			continue // beyond every near-expiry window, like the lot buckets
		}
		budContainers = append(budContainers, gin.H{
			"container_id":       r.ID.String(),
			"item_name":          r.ItemName,
			"batch_no":           r.BatchNo,
			"remaining_base_qty": numericExact(r.RemainingBaseQty),
			"bud_at":             bud.Format(time.RFC3339),
			"bucket":             bucket,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"expired":        nearExpiryBucket(expiredQty, expiredVal),
		"within_1":       nearExpiryBucket(in1Qty, in1Val),
		"within_2":       nearExpiryBucket(in2Qty, in2Val),
		"bud_containers": budContainers,
	}, "meta": gin.H{}})
}

// mustAddNumeric sums two numerics rendered at quantity scale 3.
func mustAddNumeric(a, b pgtype.Numeric) pgtype.Numeric {
	sum := new(big.Rat).Add(ratFromString(numericExact(a)), ratFromString(numericExact(b)))
	var n pgtype.Numeric
	_ = n.Scan(sum.FloatString(3))
	return n
}

// §4.6 reorder suggestions.

// StockReorderHandler godoc
// @Summary Items below the location minimum with suggested order quantity
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/stock/reorder [get]
func StockReorderHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListStockReorderRows(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		minMilli, errMin := numericToMilli(r.MinQty)
		availMilli, errAvail := numericToMilli(r.AvailableQty)
		if errMin != nil || errAvail != nil {
			respondInternalError(c, errMin)
			return
		}
		if availMilli >= minMilli {
			continue
		}
		suggested := new(big.Rat).Sub(
			ratFromString(numericExact(r.MaxQty)),
			ratFromString(numericExact(r.AvailableQty)))
		out = append(out, gin.H{
			"location_id": r.LocationID.String(),
			"item": gin.H{
				"id":   r.ItemID.String(),
				"code": r.ItemCode,
				"name": r.ItemName,
			},
			"min_qty":       numericExact(r.MinQty),
			"max_qty":       numericExact(r.MaxQty),
			"available_qty": numericExact(r.AvailableQty),
			"suggested":     suggested.FloatString(3),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}

// §5.2 #3 lot recall report (spec 2026-10-20-inventory-17b-procurement-
// special-cases): every remaining balance and the latest issue usages of one
// lot, company-wide.

// RecallReportHandler godoc
// @Summary Lot recall report: lot, balances and issue usages across the company
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Lot UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /lots/{id}/recall-report [get]
func RecallReportHandler(c *gin.Context) {
	lotID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequireCompanyLevelPermission(c, PermStockView) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	lot, err := q.GetLotByID(c.Request.Context(), lotID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lot.CompanyID != AuthCompanyID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "lot not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// stock_balance / stock_move RLS pin rows to app.current_merchant_id, so the
	// company-wide scan re-pins the GUC per merchant of the company inside the
	// request transaction. Patient names never reach the application log — this
	// handler logs nothing. Merchants are paged until exhausted so the recall
	// report is never silently truncated.
	const merchantPage = 500
	var merchants []sqlcgen.CoreMerchant
	for offset := int32(0); ; offset += merchantPage {
		page, err := q.ListMerchantsByCompany(c.Request.Context(), sqlcgen.ListMerchantsByCompanyParams{
			CompanyID: AuthCompanyID(c), Limit: merchantPage, Offset: offset,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		merchants = append(merchants, page...)
		if len(page) < merchantPage {
			break
		}
	}
	balances := make([]gin.H, 0, len(merchants))
	usages := make([]gin.H, 0)
	seenBal := map[pgtype.UUID]bool{}
	seenUse := map[pgtype.UUID]bool{}
	tx := TxFromContext(c)
	for _, m := range merchants {
		if _, err := tx.Exec(c.Request.Context(),
			"SELECT set_config('app.current_merchant_id', $1, true)", m.ID.String()); err != nil {
			respondInternalError(c, err)
			return
		}
		bs, errB := q.ListRecallBalances(c.Request.Context(), sqlcgen.ListRecallBalancesParams{
			CompanyID: AuthCompanyID(c),
			LotID:     lotID,
		})
		if errB != nil {
			respondInternalError(c, errB)
			return
		}
		for _, b := range bs {
			if seenBal[b.ID] {
				continue // RLS already filtered; skip duplicated scan rows
			}
			seenBal[b.ID] = true
			balances = append(balances, gin.H{
				"merchant_id":   b.MerchantID.String(),
				"merchant_name": b.MerchantName,
				"location_id":   b.LocationID.String(),
				"location_name": b.LocationName,
				"status":        b.Status,
				"quantity":      numericExact(b.Quantity),
			})
		}
		us, errU := q.ListRecallUsages(c.Request.Context(), sqlcgen.ListRecallUsagesParams{
			CompanyID: AuthCompanyID(c),
			LotID:     lotID,
		})
		if errU != nil {
			respondInternalError(c, errU)
			return
		}
		for _, u := range us {
			if seenUse[u.ID] {
				continue
			}
			seenUse[u.ID] = true
			// created_by_name / patient_name arrive as "" when unknown /
			// when the move has no admission ref.
			var patientName, createdByName any
			if u.PatientName != "" {
				patientName = u.PatientName
			}
			if u.CreatedByName != "" {
				createdByName = u.CreatedByName
			}
			usages = append(usages, gin.H{
				"moved_at":        tsOrNil(u.MovedAt),
				"merchant_name":   u.MerchantName,
				"location_name":   u.LocationName,
				"move_type":       u.MoveType,
				"quantity":        numericExact(u.Quantity),
				"created_by_name": createdByName,
				"patient_name":    patientName,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"lot":      lotJSON(lot),
		"balances": balances,
		"usages":   usages,
	}, "meta": gin.H{}})
}
