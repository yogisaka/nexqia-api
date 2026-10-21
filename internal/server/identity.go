// internal/server/identity.go
// Identity & packaging API (plan 2026-10-20-inventory-17b-p3-identity-packaging
// Task 3, spec 2026-10-20-inventory-17b-procurement-special-cases §6, migration
// 000082): serial-number registration and issue/transfer enforcement, repack
// posting with carried cost (HPP untouched) plus its void reversal pairs, and
// the consignment billing report. All balance mutations go through the locked
// ledger in stock_ledger.go — this file never writes stock_balance or
// stock_move directly.
package server

import (
	"context"
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

// Sentinel errors mapped to their fixed client responses by
// respondIdentityError (spec §6.2 messages).
var (
	errSerialRequired        = errors.New("serial required")
	errSerialQuantityMismat  = errors.New("serial quantity mismatch")
	errSerialNotInStock      = errors.New("serial not in stock")
	errRepackNotEditable     = errors.New("repack is not editable")
	errRepackVoidReasonNeeds = errors.New("void reason required")
)

// respondIdentityError maps identity sentinel errors to their fixed client
// responses; it returns true when err was one of them and the response is
// written, so callers can fall through to respondInternalError otherwise.
func respondIdentityError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, errSerialRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "serial required"})
		return true
	case errors.Is(err, errSerialQuantityMismat):
		c.JSON(http.StatusBadRequest, gin.H{"error": "serial quantity mismatch"})
		return true
	case errors.Is(err, errSerialNotInStock):
		c.JSON(http.StatusConflict, gin.H{"error": "serial not in stock"})
		return true
	case errors.Is(err, errRepackNotEditable):
		c.JSON(http.StatusConflict, gin.H{"error": "repack is not editable"})
		return true
	case errors.Is(err, errRepackVoidReasonNeeds):
		c.JSON(http.StatusBadRequest, gin.H{"error": "void reason required"})
		return true
	}
	return false
}

// RegisterIdentityRoutes registers the P3 identity & packaging endpoints
// (authenticated router group; wired in server.go right after
// RegisterStockOutRoutes in Task 4).
func RegisterIdentityRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/serials", ListSerialsHandler)
	rg.POST("/merchants/:id/serials", RegisterSerialsHandler)

	rg.POST("/merchants/:id/repacks", CreateRepackHandler)
	rg.GET("/merchants/:id/repacks", ListRepacksHandler)
	rg.GET("/repacks/:id", GetRepackHandler)
	rg.PATCH("/repacks/:id", PatchRepackHandler)
	rg.POST("/repacks/:id/post", PostRepackHandler)
	rg.POST("/repacks/:id/void", VoidRepackHandler)

	rg.GET("/merchants/:id/consignment/report", ConsignmentReportHandler)
}

// cleanSerialNos trims serial numbers and rejects blanks (400). ok=false when
// the response is already written.
func cleanSerialNos(c *gin.Context, raw []string) ([]string, bool) {
	out := make([]string, 0, len(raw))
	for _, sn := range raw {
		trimmed := strings.TrimSpace(sn)
		if trimmed == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid serial_no"})
			return nil, false
		}
		out = append(out, trimmed)
	}
	return out, true
}

// enforceSerialIssue validates the requested serial numbers of one item at
// one location and flips them to targetStatus with the issuing document as
// issued_ref (spec §6.2 #2). issued/destroyed/returned serials lose their
// location (location_id = NULL, done by UpdateSerialIssued).
func enforceSerialIssue(ctx context.Context, q *sqlcgen.Queries, merchantID, locationID, itemID pgtype.UUID, serialNos []string, targetStatus string, ref stockRef) error {
	rows, err := q.GetSerialsByIDs(ctx, sqlcgen.GetSerialsByIDsParams{
		MerchantID: merchantID, ItemID: itemID, SerialNos: serialNos,
	})
	if err != nil {
		return err
	}
	if len(rows) != len(serialNos) {
		return errSerialQuantityMismat
	}
	for _, s := range rows {
		if s.Status != "in_stock" || s.LocationID != locationID {
			return errSerialNotInStock
		}
	}
	n, err := q.UpdateSerialIssued(ctx, sqlcgen.UpdateSerialIssuedParams{
		Status: targetStatus, IssuedRefType: ref.RefType, IssuedRefID: ref.RefID,
		MerchantID: merchantID, ItemID: itemID, SerialNos: serialNos,
	})
	if err != nil {
		return err
	}
	if n != int64(len(serialNos)) {
		return errSerialNotInStock
	}
	return nil
}

// moveSerialsForTransfer validates the requested serial numbers like
// enforceSerialIssue (in_stock at the source location) and moves them to the
// target merchant/location; the status stays in_stock (spec §6.2 #2).
func moveSerialsForTransfer(ctx context.Context, q *sqlcgen.Queries, fromMerchant, toMerchant, toLocationID, itemID pgtype.UUID, serialNos []string) error {
	rows, err := q.GetSerialsByIDs(ctx, sqlcgen.GetSerialsByIDsParams{
		MerchantID: fromMerchant, ItemID: itemID, SerialNos: serialNos,
	})
	if err != nil {
		return err
	}
	if len(rows) != len(serialNos) {
		return errSerialQuantityMismat
	}
	for _, s := range rows {
		if s.Status != "in_stock" {
			return errSerialNotInStock
		}
	}
	n, err := q.MoveSerialsForTransfer(ctx, sqlcgen.MoveSerialsForTransferParams{
		ToMerchantID: toMerchant, ToLocationID: toLocationID,
		FromMerchantID: fromMerchant, ItemID: itemID, SerialNos: serialNos,
	})
	if err != nil {
		return err
	}
	if n != int64(len(serialNos)) {
		return errSerialNotInStock
	}
	return nil
}

// enforceOutboundSerials applies serial enforcement to the parsed lines of an
// outbound document draft (supplier return / destruction, spec §6.2 #2):
// serial-tracked items require serial_ids, untracked lines ignore them. A
// valid locationID pins every serial to that location (destruction); a zero
// locationID resolves each serial's own registered location (supplier
// returns, whose balances allocate across locations). Returns false when the
// response is already written.
func enforceOutboundSerials(c *gin.Context, q *sqlcgen.Queries, merchantID, locationID pgtype.UUID, lines []outboundLine, targetStatus string, ref stockRef) bool {
	for _, l := range lines {
		item, err := q.GetInventoryItemByID(c.Request.Context(), l.ItemID)
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if !item.TrackSerial {
			continue
		}
		if len(l.SerialIDs) == 0 {
			if !respondIdentityError(c, errSerialRequired) {
				respondInternalError(c, errSerialRequired)
			}
			return false
		}
		serials, okS := cleanSerialNos(c, l.SerialIDs)
		if !okS {
			return false
		}
		if locationID.Valid {
			if err := enforceSerialIssue(c.Request.Context(), q, merchantID, locationID, l.ItemID, serials, targetStatus, ref); err != nil {
				if !respondIdentityError(c, err) {
					respondInternalError(c, err)
				}
				return false
			}
			continue
		}
		// No document location: enforce per group of serials sharing their
		// registered location.
		rows, err := q.GetSerialsByIDs(c.Request.Context(), sqlcgen.GetSerialsByIDsParams{
			MerchantID: merchantID, ItemID: l.ItemID, SerialNos: serials,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if len(rows) != len(serials) {
			if !respondIdentityError(c, errSerialQuantityMismat) {
				respondInternalError(c, errSerialQuantityMismat)
			}
			return false
		}
		perLocation := map[pgtype.UUID][]string{}
		for _, s := range rows {
			if s.Status != "in_stock" {
				if !respondIdentityError(c, errSerialNotInStock) {
					respondInternalError(c, errSerialNotInStock)
				}
				return false
			}
			perLocation[s.LocationID] = append(perLocation[s.LocationID], s.SerialNo)
		}
		for loc, sns := range perLocation {
			if err := enforceSerialIssue(c.Request.Context(), q, merchantID, loc, l.ItemID, sns, targetStatus, ref); err != nil {
				if !respondIdentityError(c, err) {
					respondInternalError(c, err)
				}
				return false
			}
		}
	}
	return true
}

// §6.2 #2 serial endpoints.

type registerSerialsRequest struct {
	ItemID     string   `json:"item_id" binding:"required"`
	LocationID string   `json:"location_id" binding:"required"`
	Serials    []string `json:"serials" binding:"required,min=1,dive"`
}

func serialJSON(s sqlcgen.InventorySerial) gin.H {
	return gin.H{
		"id":              s.ID.String(),
		"item_id":         s.ItemID.String(),
		"serial_no":       s.SerialNo,
		"lot_id":          uuidOrNil(s.LotID),
		"location_id":     uuidOrNil(s.LocationID),
		"status":          s.Status,
		"issued_ref_type": textOrNil(s.IssuedRefType),
		"issued_ref_id":   uuidOrNil(s.IssuedRefID),
		"received_at":     tsOrNil(s.ReceivedAt),
	}
}

// ListSerialsHandler godoc
// @Summary List a merchant's serial numbers (?item_id=&status=)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param item_id query string false "Filter by item"
// @Param status query string false "Filter by status"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/serials [get]
func ListSerialsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	var itemID pgtype.UUID
	if v := c.Query("item_id"); v != "" {
		id, okI := parseUUID(v)
		if !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item_id"})
			return
		}
		itemID = id
	}
	var status pgtype.Text
	if v := c.Query("status"); v != "" {
		status = pgtype.Text{String: v, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListSerials(c.Request.Context(), sqlcgen.ListSerialsParams{
		MerchantID: merchantID, ItemID: itemID, Status: status,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, s := range rows {
		data = append(data, serialJSON(s))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// RegisterSerialsHandler godoc
// @Summary Register serial numbers in bulk (manual entry, status in_stock)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body registerSerialsRequest true "Serial numbers"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/serials [post]
func RegisterSerialsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req registerSerialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	itemID, okI := parseUUID(req.ItemID)
	locID, okL := parseUUID(req.LocationID)
	if !okI || !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	serials, okS := cleanSerialNos(c, req.Serials)
	if !okS {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	if _, err := q.GetInventoryItemByID(c.Request.Context(), itemID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown item"})
		} else {
			respondInternalError(c, err)
		}
		return
	}
	created := make([]gin.H, 0, len(serials))
	for _, sn := range serials {
		s, err := q.InsertSerial(c.Request.Context(), sqlcgen.InsertSerialParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID,
			ItemID: itemID, SerialNo: sn, LocationID: locID, CreatedBy: AuthUserID(c),
		})
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "serial already registered"})
			} else {
				respondInternalError(c, err)
			}
			return
		}
		created = append(created, serialJSON(s))
	}
	c.JSON(http.StatusCreated, gin.H{"data": created, "meta": gin.H{}})
}

// §6.2 #3 repacking.

type repackOutLineRequest struct {
	ItemID  string `json:"item_id" binding:"required"`
	LotID   string `json:"lot_id" binding:"required"`
	Source  string `json:"source" binding:"required"`
	BaseQty string `json:"base_qty" binding:"required"`
}

type repackInLineRequest struct {
	ItemID      string `json:"item_id" binding:"required"`
	ItemUnitID  string `json:"item_unit_id" binding:"required"`
	PackQty     string `json:"pack_qty" binding:"required"`
	PackContent string `json:"pack_content" binding:"required"`
	BatchNo     string `json:"batch_no"`
	ExpiryDate  string `json:"expiry_date"`
	BaseQty     string `json:"base_qty" binding:"required"`
}

type saveRepackRequest struct {
	OutLocationID string                 `json:"out_location_id" binding:"required"`
	InLocationID  string                 `json:"in_location_id" binding:"required"`
	Note          string                 `json:"note"`
	OutLines      []repackOutLineRequest `json:"out_lines" binding:"required,min=1,dive"`
	InLines       []repackInLineRequest  `json:"in_lines" binding:"required,min=1,dive"`
	RowVersion    int64                  `json:"row_version"`
}

func repackOutLineJSON(l sqlcgen.InventoryRepackOutLine) gin.H {
	return gin.H{
		"id":       l.ID.String(),
		"item_id":  l.ItemID.String(),
		"lot_id":   l.LotID.String(),
		"source":   l.Source,
		"base_qty": numericExact(l.BaseQty),
	}
}

func repackInLineJSON(l sqlcgen.InventoryRepackInLine) gin.H {
	return gin.H{
		"id":           l.ID.String(),
		"item_id":      l.ItemID.String(),
		"item_unit_id": l.ItemUnitID.String(),
		"pack_qty":     numericExact(l.PackQty),
		"pack_content": numericExact(l.PackContent),
		"batch_no":     textOrNil(l.BatchNo),
		"expiry_date":  dateOrNil(l.ExpiryDate),
		"base_qty":     numericExact(l.BaseQty),
	}
}

func repackJSON(r sqlcgen.InventoryRepack, outLines []sqlcgen.InventoryRepackOutLine, inLines []sqlcgen.InventoryRepackInLine) gin.H {
	out := gin.H{
		"id":              r.ID.String(),
		"merchant_id":     r.MerchantID.String(),
		"out_location_id": r.OutLocationID.String(),
		"in_location_id":  r.InLocationID.String(),
		"status":          r.Status,
		"note":            textOrNil(r.Note),
		"posted_at":       tsOrNil(r.PostedAt),
		"void_reason":     textOrNil(r.VoidReason),
		"created_at":      tsOrNil(r.CreatedAt),
		"row_version":     r.RowVersion,
	}
	if outLines != nil {
		rendered := make([]gin.H, 0, len(outLines))
		for _, l := range outLines {
			rendered = append(rendered, repackOutLineJSON(l))
		}
		out["out_lines"] = rendered
	}
	if inLines != nil {
		rendered := make([]gin.H, 0, len(inLines))
		for _, l := range inLines {
			rendered = append(rendered, repackInLineJSON(l))
		}
		out["in_lines"] = rendered
	}
	return out
}

// loadRepack fetches a live repack and checks PermStockManage at its merchant.
// Writes 400/403/404 and returns false on failure.
func loadRepack(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryRepack, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryRepack{}, false
	}
	r, err := q.GetRepackByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "repack not found"})
		return sqlcgen.InventoryRepack{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryRepack{}, false
	}
	if !RequirePermissionForMerchant(c, PermStockManage, r.MerchantID) {
		return sqlcgen.InventoryRepack{}, false
	}
	return r, true
}

// parseRepackLines validates and inserts the draft's out and in lines
// (full replace). ok=false when the response is already written.
func parseRepackLines(c *gin.Context, q *sqlcgen.Queries, r sqlcgen.InventoryRepack, req saveRepackRequest) bool {
	if err := q.DeleteRepackLines(c.Request.Context(), r.ID); err != nil {
		respondInternalError(c, err)
		return false
	}
	for _, lr := range req.OutLines {
		itemID, ok1 := parseUUID(lr.ItemID)
		lotID, ok2 := parseUUID(lr.LotID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return false
		}
		switch lr.Source {
		case "purchase", "program", "grant", "consignment":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
			return false
		}
		baseQty, err := numericOrZero(lr.BaseQty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return false
		}
		if milli, errM := numericToMilli(baseQty); errM != nil || milli <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_qty"})
			return false
		}
		if _, err := q.InsertRepackOutLine(c.Request.Context(), sqlcgen.InsertRepackOutLineParams{
			CompanyID: r.CompanyID, MerchantID: r.MerchantID, RepackID: r.ID,
			ItemID: itemID, LotID: lotID, Source: lr.Source,
			BaseQty: baseQty, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	for _, lr := range req.InLines {
		itemID, ok1 := parseUUID(lr.ItemID)
		unitID, ok2 := parseUUID(lr.ItemUnitID)
		if !ok1 || !ok2 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line reference"})
			return false
		}
		packQty, errQ := numericOrZero(lr.PackQty)
		packContent, errC := numericOrZero(lr.PackContent)
		baseQty, errB := numericOrZero(lr.BaseQty)
		if errQ != nil || errC != nil || errB != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line quantity"})
			return false
		}
		for _, n := range []pgtype.Numeric{packQty, packContent, baseQty} {
			if milli, errM := numericToMilli(n); errM != nil || milli <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line quantity"})
				return false
			}
		}
		var expiry pgtype.Date
		if lr.ExpiryDate != "" {
			d, okD := parseDateStr(lr.ExpiryDate)
			if !okD {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expiry_date"})
				return false
			}
			expiry = d
		}
		if _, err := q.InsertRepackInLine(c.Request.Context(), sqlcgen.InsertRepackInLineParams{
			CompanyID: r.CompanyID, MerchantID: r.MerchantID, RepackID: r.ID,
			ItemID: itemID, ItemUnitID: unitID,
			PackQty: packQty, PackContent: packContent,
			BatchNo: nargText(&lr.BatchNo), ExpiryDate: expiry,
			BaseQty: baseQty, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// CreateRepackHandler godoc
// @Summary Create a repack draft (out lines, in lines)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body saveRepackRequest true "Repack draft"
// @Success 201 {object} apiResponse
// @Router /merchants/{id}/repacks [post]
func CreateRepackHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	var req saveRepackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	outID, okO := parseUUID(req.OutLocationID)
	inID, okI := parseUUID(req.InLocationID)
	if !okO || !okI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location"})
		return
	}
	if outID == inID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "locations must differ"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, outID, merchantID); !ok {
		return
	}
	if _, ok := loadMerchantRoom(c, q, inID, merchantID); !ok {
		return
	}
	r, err := q.InsertRepack(c.Request.Context(), sqlcgen.InsertRepackParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		OutLocationID: outID, InLocationID: inID,
		Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !parseRepackLines(c, q, r, req) {
		return
	}
	outLines, err := q.ListRepackOutLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	inLines, err := q.ListRepackInLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": repackJSON(r, outLines, inLines), "meta": gin.H{}})
}

// ListRepacksHandler godoc
// @Summary List a merchant's repacks
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/repacks [get]
func ListRepacksHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListRepacks(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, repackJSON(r, nil, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetRepackHandler godoc
// @Summary Fetch one repack with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Repack UUID"
// @Success 200 {object} apiResponse
// @Router /repacks/{id} [get]
func GetRepackHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadRepack(c, q, c.Param("id"))
	if !ok {
		return
	}
	outLines, err := q.ListRepackOutLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	inLines, err := q.ListRepackInLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": repackJSON(r, outLines, inLines), "meta": gin.H{}})
}

// PatchRepackHandler godoc
// @Summary Replace a draft repack's header and lines (optimistic locking)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Repack UUID"
// @Param request body saveRepackRequest true "Repack draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /repacks/{id} [patch]
func PatchRepackHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadRepack(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "repack is not editable"})
		return
	}
	var req saveRepackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	outID, okO := parseUUID(req.OutLocationID)
	inID, okI := parseUUID(req.InLocationID)
	if !okO || !okI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location"})
		return
	}
	if outID == inID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "locations must differ"})
		return
	}
	updated, err := q.UpdateRepackDraft(c.Request.Context(), sqlcgen.UpdateRepackDraftParams{
		OutLocationID: outID, InLocationID: inID, Note: nargText(&req.Note),
		UpdatedBy: AuthUserID(c), RepackID: r.ID,
		ExpectedRowVersion: int32(req.RowVersion),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "repack is not editable"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !parseRepackLines(c, q, updated, req) {
		return
	}
	outLines, err := q.ListRepackOutLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	inLines, err := q.ListRepackInLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": repackJSON(updated, outLines, inLines), "meta": gin.H{}})
}

// postRepack posts a draft repack inside the caller's transaction (spec
// §6.2 #3): one negative repack_out move per out line at its average cost, a
// find-or-create lot per in line (batch/ED default copied from the first out
// line of the same item), ONE positive repack_in move per in line whose
// unit_cost carries the full out value (Σ qty_out × avg_cost ÷ Σ qty_in_base,
// 6 decimals half-up) — the moving average is NOT reapplied (HPP untouched,
// spec §9.3) — then the repack becomes posted.
func postRepack(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, merchantID, companyID, userID pgtype.UUID, r sqlcgen.InventoryRepack, moveDate time.Time) error {
	outLines, err := q.ListRepackOutLines(ctx, r.ID)
	if err != nil {
		return err
	}
	inLines, err := q.ListRepackInLines(ctx, r.ID)
	if err != nil {
		return err
	}
	zero := pgtype.Numeric{Valid: true}
	totalCost := new(big.Rat)
	for _, l := range outLines {
		avg, err := q.GetItemAvgCost(ctx, sqlcgen.GetItemAvgCostParams{
			MerchantID: merchantID, ItemID: l.ItemID,
		})
		if err != nil {
			return err
		}
		unit := zero
		if l.Source == "purchase" || l.Source == "consignment" {
			unit = avg
		}
		// A consignment out line keys its balance's supplier (amendment
		// 2026-10-21, spec §6.1).
		var supplier pgtype.UUID
		if l.Source == "consignment" {
			supplier, err = balanceSupplierAtSlot(ctx, tx, merchantID, r.OutLocationID, l.LotID, l.Source, "available")
			if err != nil {
				return err
			}
		}
		if _, err := applyStockMove(ctx, q, merchantID, companyID, userID, stockMoveInput{
			Key:      stockKey{LocationID: r.OutLocationID, LotID: l.LotID, Source: l.Source, Status: "available", SupplierID: supplier},
			ItemID:   l.ItemID,
			Qty:      negateNumeric(l.BaseQty),
			UnitCost: unit,
			MoveType: "repack_out",
			RefType:  "repack",
			RefID:    r.ID,
			MoveDate: moveDate,
		}); err != nil {
			return err
		}
		totalCost.Add(totalCost, new(big.Rat).Mul(numericToRat(unit), numericToRat(l.BaseQty)))
	}
	totalIn := new(big.Rat)
	for _, l := range inLines {
		totalIn.Add(totalIn, numericToRat(l.BaseQty))
	}
	if totalIn.Sign() == 0 {
		return errors.New("repack in lines have zero base quantity")
	}
	unitCostRat := roundRatHalfUp(new(big.Rat).Quo(totalCost, totalIn), 6)
	unitCost, err := parseNumeric(unitCostRat.FloatString(6))
	if err != nil {
		return err
	}
	for _, l := range inLines {
		batchNo, expiry, err := repackInLotDefaults(ctx, q, outLines, l)
		if err != nil {
			return err
		}
		lot, err := q.UpsertReceiptLot(ctx, sqlcgen.UpsertReceiptLotParams{
			CompanyID: companyID, ItemID: l.ItemID,
			BatchNo: batchNo, ExpiryDate: expiry,
		})
		if err != nil {
			return err
		}
		if _, err := applyStockMove(ctx, q, merchantID, companyID, userID, stockMoveInput{
			Key:      stockKey{LocationID: r.InLocationID, LotID: lot.ID, Source: "purchase", Status: "available"},
			ItemID:   l.ItemID,
			Qty:      l.BaseQty,
			UnitCost: unitCost,
			MoveType: "repack_in",
			RefType:  "repack",
			RefID:    r.ID,
			MoveDate: moveDate,
		}); err != nil {
			return err
		}
	}
	_, err = q.MarkRepackPosted(ctx, sqlcgen.MarkRepackPostedParams{
		PostedBy: userID, RepackID: r.ID,
	})
	return err
}

// repackInLotDefaults resolves the in line's batch/ED: input values win;
// empty input copies the first out line's lot of the same item (spec §6.2 #3).
func repackInLotDefaults(ctx context.Context, q *sqlcgen.Queries, outLines []sqlcgen.InventoryRepackOutLine, in sqlcgen.InventoryRepackInLine) (string, pgtype.Date, error) {
	if in.BatchNo.Valid || in.ExpiryDate.Valid {
		batch := ""
		if in.BatchNo.Valid {
			batch = in.BatchNo.String
		}
		return batch, in.ExpiryDate, nil
	}
	for _, out := range outLines {
		if out.ItemID != in.ItemID {
			continue
		}
		lot, err := q.GetLotByID(ctx, out.LotID)
		if err != nil {
			return "", pgtype.Date{}, err
		}
		return lot.BatchNo, lot.ExpiryDate, nil
	}
	// No same-item out line: fall back to the input (possibly empty batch).
	batch := ""
	if in.BatchNo.Valid {
		batch = in.BatchNo.String
	}
	return batch, in.ExpiryDate, nil
}

// repackPostedMoves reads the repack's own posted moves in write order — the
// reversal sources for void (the sqlc queries predate this read-back need).
type repackMoveRow struct {
	ID       pgtype.UUID
	MoveType string
	Location pgtype.UUID
	ItemID   pgtype.UUID
	LotID    pgtype.UUID
	Source   string
	Status   string
	Quantity pgtype.Numeric
	UnitCost pgtype.Numeric
}

func repackPostedMoves(ctx context.Context, tx pgx.Tx, repackID pgtype.UUID) ([]repackMoveRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, move_type, location_id, item_id, lot_id, source, status, quantity, unit_cost
		FROM inventory.stock_move
		WHERE ref_type = 'repack' AND ref_id = $1 AND move_type IN ('repack_in','repack_out')
		ORDER BY created_at, id`, repackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []repackMoveRow{}
	for rows.Next() {
		var m repackMoveRow
		if err := rows.Scan(&m.ID, &m.MoveType, &m.Location, &m.ItemID,
			&m.LotID, &m.Source, &m.Status, &m.Quantity, &m.UnitCost); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// voidRepack reverses a posted repack inside the caller's transaction (pola
// void GR, spec §6.2 #3): one negative reversal per repack_in move and one
// positive reversal per repack_out move, each naming its source move in
// reversal_of. The HPP stays untouched (no moving-average call). Callers must
// have checked status = posted and a non-empty reason.
func voidRepack(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, merchantID, companyID, userID pgtype.UUID, r sqlcgen.InventoryRepack, reason string) error {
	moves, err := repackPostedMoves(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	for _, m := range moves {
		// Reversal quantities negate the original: repack_in (positive)
		// becomes negative; repack_out (negative) becomes positive.
		var supplier pgtype.UUID
		if m.Source == "consignment" {
			supplier, err = balanceSupplierAtSlot(ctx, tx, merchantID, m.Location, m.LotID, m.Source, m.Status)
			if err != nil {
				return err
			}
		}
		if _, err := applyStockMove(ctx, q, merchantID, companyID, userID, stockMoveInput{
			Key:        stockKey{LocationID: m.Location, LotID: m.LotID, Source: m.Source, Status: m.Status, SupplierID: supplier},
			ItemID:     m.ItemID,
			Qty:        negateNumeric(m.Quantity),
			UnitCost:   m.UnitCost,
			MoveType:   "reversal",
			RefType:    "repack",
			RefID:      r.ID,
			ReversalOf: m.ID,
			Reason:     reason,
			MoveDate:   r.PostedAt.Time,
		}); err != nil {
			return err
		}
	}
	_, err = q.MarkRepackVoid(ctx, sqlcgen.MarkRepackVoidParams{
		VoidReason: reason, UpdatedBy: userID, RepackID: r.ID,
	})
	return err
}

// PostRepackHandler godoc
// @Summary Post a draft repack (repack_out/repack_in moves, cost carried)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Repack UUID"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /repacks/{id}/post [post]
func PostRepackHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadRepack(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "repack is not editable"})
		return
	}
	// Moves follow the merchant's business day (txDayStart), not UTC now.
	loc, err := merchantLocation(c.Request.Context(), q, r.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	moveDate, err := txDayStart(c.Request.Context(), q, loc)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := postRepack(c.Request.Context(), TxFromContext(c), q, r.MerchantID, r.CompanyID, AuthUserID(c), r, moveDate); err != nil {
		if !respondStockError(c, err) {
			respondInternalError(c, err)
		}
		return
	}
	posted, err := q.GetRepackByID(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	outLines, err := q.ListRepackOutLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	inLines, err := q.ListRepackInLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": repackJSON(posted, outLines, inLines), "meta": gin.H{}})
}

type voidRepackRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// VoidRepackHandler godoc
// @Summary Void a posted repack (reversal pairs, HPP untouched)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Repack UUID"
// @Param request body voidRepackRequest true "Void reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /repacks/{id}/void [post]
func VoidRepackHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadRepack(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "posted" {
		c.JSON(http.StatusConflict, gin.H{"error": "repack is not posted"})
		return
	}
	var req voidRepackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "void reason required"})
		return
	}
	if err := voidRepack(c.Request.Context(), TxFromContext(c), q, r.MerchantID, r.CompanyID, AuthUserID(c), r, req.Reason); err != nil {
		if !respondStockError(c, err) {
			respondInternalError(c, err)
		}
		return
	}
	voided, err := q.GetRepackByID(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": repackJSON(voided, nil, nil), "meta": gin.H{}})
}

// §6.2 #1 consignment billing report.

type consignmentReportItem struct {
	ItemID  pgtype.UUID
	Name    string
	Qty     pgtype.Numeric
	Value   pgtype.Numeric
	Details []gin.H
}

// ConsignmentReportHandler godoc
// @Summary Consignment billing report per supplier and item (?from=&to=&supplier_id=, YYYY-MM-DD)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param from query string true "Range start (YYYY-MM-DD)"
// @Param to query string true "Range end (YYYY-MM-DD)"
// @Param supplier_id query string false "Filter by supplier"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/consignment/report [get]
func ConsignmentReportHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	from, okF := parseDateStr(c.Query("from"))
	to, okT := parseDateStr(c.Query("to"))
	if c.Query("from") == "" || c.Query("to") == "" || !okF || !okT {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from/to"})
		return
	}
	var supplierID pgtype.UUID
	if v := c.Query("supplier_id"); v != "" {
		id, okS := parseUUID(v)
		if !okS {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
			return
		}
		supplierID = id
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListConsignmentReport(c.Request.Context(), sqlcgen.ListConsignmentReportParams{
		MerchantID: merchantID, Column2: from, Column3: to, SupplierID: supplierID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	details, err := q.ListConsignmentReportDetails(c.Request.Context(), sqlcgen.ListConsignmentReportDetailsParams{
		MerchantID: merchantID, Column2: from, Column3: to, SupplierID: supplierID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	supplierNames := map[pgtype.UUID]string{}
	itemNames := map[pgtype.UUID]string{}
	order := make([]pgtype.UUID, 0, len(rows))
	perSupplier := map[pgtype.UUID][]*consignmentReportItem{}
	perItem := map[pgtype.UUID]map[pgtype.UUID]*consignmentReportItem{}
	getSupplierName := func(id pgtype.UUID) (string, error) {
		if n, seen := supplierNames[id]; seen {
			return n, nil
		}
		s, err := q.GetSupplierByID(c.Request.Context(), id)
		if err != nil {
			return "", err
		}
		supplierNames[id] = s.Name
		return s.Name, nil
	}
	getItemName := func(id pgtype.UUID) (string, error) {
		if n, seen := itemNames[id]; seen {
			return n, nil
		}
		it, err := q.GetInventoryItemByID(c.Request.Context(), id)
		if err != nil {
			return "", err
		}
		itemNames[id] = it.Name
		return it.Name, nil
	}
	for _, row := range rows {
		name, err := getSupplierName(row.SupplierID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		supplierNames[row.SupplierID] = name
		itemName, err := getItemName(row.ItemID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if _, seen := perSupplier[row.SupplierID]; !seen {
			order = append(order, row.SupplierID)
			perSupplier[row.SupplierID] = nil
			perItem[row.SupplierID] = map[pgtype.UUID]*consignmentReportItem{}
		}
		if _, seen := perItem[row.SupplierID][row.ItemID]; !seen {
			entry := &consignmentReportItem{ItemID: row.ItemID, Name: itemName, Qty: row.Qty, Value: row.Value}
			perSupplier[row.SupplierID] = append(perSupplier[row.SupplierID], entry)
			perItem[row.SupplierID][row.ItemID] = entry
		}
	}
	for _, d := range details {
		entry, seen := perItem[d.SupplierID][d.ItemID]
		if !seen {
			continue
		}
		entry.Details = append(entry.Details, gin.H{
			"used_on":    dateText(d.UsedOn),
			"qty":        numericExact(d.Qty),
			"unit_price": numericExact(d.UnitPrice),
			"ref_type":   textOrNil(d.RefType),
		})
	}
	suppliers := make([]gin.H, 0, len(order))
	for _, supID := range order {
		items := make([]gin.H, 0, len(perSupplier[supID]))
		for _, it := range perSupplier[supID] {
			details := it.Details
			if details == nil {
				details = []gin.H{}
			}
			items = append(items, gin.H{
				"item_id":   it.ItemID.String(),
				"item_name": it.Name,
				"qty":       numericExact(it.Qty),
				"value":     numericExact(it.Value),
				"details":   details,
			})
		}
		suppliers = append(suppliers, gin.H{
			"supplier_id":   supID.String(),
			"supplier_name": supplierNames[supID],
			"items":         items,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"suppliers": suppliers}, "meta": gin.H{}})
}
