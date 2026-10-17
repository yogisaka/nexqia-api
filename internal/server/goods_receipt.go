// internal/server/goods_receipt.go
// Supplier master data and goods-receipt API (plan 2026-10-07-inventory-17a
// Task 4, spec 2026-10-07-inventory-17-stock-procurement §4.1): supplier CRUD
// (company- and merchant-level), receipt drafts and posting — lots, stock
// balances, moving-average HPP, formulary reference price + proposals — and
// void with per-line availability checks and reversal moves.
package server

import (
	"encoding/json"
	"errors"
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

// PermStockReceive guards supplier master data and goods receipts
// (permission inventory.stock.receive, migration 000071).
const PermStockReceive = "inventory.stock.receive"

// RegisterStockRoutes registers the supplier and goods-receipt endpoints
// under the authenticated router group (wired in server.go right after
// RegisterFeeRoutes).
func RegisterStockRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/suppliers", ListSuppliersHandler)
	rg.POST("/suppliers", CreateSupplierHandler)
	rg.PATCH("/suppliers/:id", UpdateSupplierHandler)
	rg.DELETE("/suppliers/:id", DeleteSupplierHandler)

	rg.POST("/merchants/:id/goods-receipts", CreateGoodsReceiptHandler)
	rg.GET("/merchants/:id/goods-receipts", ListGoodsReceiptsHandler)
	rg.GET("/goods-receipts/:id", GetGoodsReceiptHandler)
	rg.PATCH("/goods-receipts/:id", UpdateGoodsReceiptHandler)
	rg.POST("/goods-receipts/:id/post", PostGoodsReceiptHandler)
	rg.POST("/goods-receipts/:id/void", VoidGoodsReceiptHandler)

	// Stock operations (Task 5, spec §4.3–4.5).
	rg.POST("/merchants/:id/stock/usage", StockUsageHandler)
	rg.POST("/merchants/:id/stock/transfers", StockTransferHandler)
	rg.POST("/merchants/:id/stock/adjustments", StockAdjustmentHandler)
	rg.POST("/merchants/:id/stock/status-change", StockStatusChangeHandler)
	rg.PATCH("/lots/:id/recall", RecallLotHandler)
	rg.POST("/merchants/:id/stock-counts", CreateStockCountHandler)
	rg.PUT("/stock-counts/:id/lines", UpdateStockCountLinesHandler)
	rg.POST("/stock-counts/:id/post", PostStockCountHandler)
	rg.DELETE("/stock-counts/:id", DeleteStockCountHandler)
	rg.PUT("/merchants/:id/stock-levels", PutStockLevelsHandler)
	rg.GET("/merchants/:id/inventory-settings", GetInventorySettingsHandler)
	rg.PATCH("/merchants/:id/inventory-settings", PatchInventorySettingsHandler)
}

// numericExact renders a pgtype numeric as its exact decimal string
// (scale taken from Exp), e.g. numeric(19,6) → "10000.000000".
func numericExact(n pgtype.Numeric) string {
	if !n.Valid || n.Int == nil {
		return "0"
	}
	s := n.Int.String()
	if n.Exp >= 0 {
		return s + strings.Repeat("0", int(n.Exp))
	}
	frac := -int(n.Exp)
	if len(s) <= frac {
		s = strings.Repeat("0", frac-len(s)+1) + s
	}
	return s[:len(s)-frac] + "." + s[len(s)-frac:]
}

// numericExactOrNil is numericExact for nullable result columns.
func numericExactOrNil(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	return numericExact(n)
}

// parseNumeric reads a decimal string (JSON input) into a pgtype numeric.
func parseNumeric(s string) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if err := n.Scan(strings.TrimSpace(s)); err != nil {
		return pgtype.Numeric{}, err
	}
	return n, nil
}

// numericToRat converts a stored numeric to an exact rational for the pure
// landed-cost engine (receipt_cost.go).
func numericToRat(n pgtype.Numeric) *big.Rat {
	if !n.Valid || n.Int == nil {
		return new(big.Rat)
	}
	r := new(big.Rat).SetInt(n.Int)
	if n.Exp != 0 {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absI64(int64(n.Exp)))), nil)
		sr := new(big.Rat).SetInt(scale)
		if n.Exp > 0 {
			r.Mul(r, sr)
		} else {
			r.Quo(r, sr)
		}
	}
	return r
}

func absI64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// negateNumeric returns -n exactly (used for reversal move quantities).
func negateNumeric(n pgtype.Numeric) pgtype.Numeric {
	if !n.Valid || n.Int == nil {
		return pgtype.Numeric{Valid: true}
	}
	return pgtype.Numeric{Int: new(big.Int).Neg(n.Int), Exp: n.Exp, Valid: true}
}

// parseDateStr reads a YYYY-MM-DD JSON string into a pgtype date.
func parseDateStr(s string) (pgtype.Date, bool) {
	var d pgtype.Date
	if s == "" {
		return d, true
	}
	if err := d.Scan(s); err != nil {
		return pgtype.Date{}, false
	}
	return d, true
}

// monthsLeft is the whole months of shelf life between from and to; a
// not-yet-reached day of month counts the running month as partial.
func monthsLeft(from, to time.Time) int {
	m := (to.Year()-from.Year())*12 + int(to.Month()) - int(from.Month())
	if to.Day() < from.Day() {
		m--
	}
	if m < 0 {
		m = 0
	}
	return m
}

func int4OrNil(n pgtype.Int4) any {
	if !n.Valid {
		return nil
	}
	return n.Int32
}

// §4.1 suppliers.

func supplierJSON(s sqlcgen.CoreSupplier) gin.H {
	var bank any
	if len(s.BankAccount) > 0 {
		bank = json.RawMessage(s.BankAccount)
	}
	return gin.H{
		"id":                  s.ID.String(),
		"merchant_id":         uuidOrNil(s.MerchantID),
		"code":                s.Code,
		"name":                s.Name,
		"pic_name":            textOrNil(s.PicName),
		"pic_phone":           textOrNil(s.PicPhone),
		"address":             textOrNil(s.Address),
		"npwp":                textOrNil(s.Npwp),
		"bank_account":        bank,
		"credit_limit":        numericJSON(s.CreditLimit),
		"lead_time_days":      int4OrNil(s.LeadTimeDays),
		"lead_time_cito_days": int4OrNil(s.LeadTimeCitoDays),
		"payment_term_days":   int4OrNil(s.PaymentTermDays),
		"contract_start_date": dateOrNil(s.ContractStartDate),
		"contract_end_date":   dateOrNil(s.ContractEndDate),
		"is_active":           s.IsActive,
		"row_version":         s.RowVersion,
	}
}

type createSupplierRequest struct {
	MerchantID        string          `json:"merchant_id"`
	Code              string          `json:"code" binding:"required"`
	Name              string          `json:"name" binding:"required"`
	PicName           string          `json:"pic_name"`
	PicPhone          string          `json:"pic_phone"`
	Address           string          `json:"address"`
	Npwp              string          `json:"npwp"`
	BankAccount       json.RawMessage `json:"bank_account"`
	CreditLimit       *string         `json:"credit_limit"`
	LeadTimeDays      *int32          `json:"lead_time_days"`
	LeadTimeCitoDays  *int32          `json:"lead_time_cito_days"`
	PaymentTermDays   *int32          `json:"payment_term_days"`
	ContractStartDate string          `json:"contract_start_date"`
	ContractEndDate   string          `json:"contract_end_date"`
	IsActive          *bool           `json:"is_active"`
}

type patchSupplierRequest struct {
	Name              *string         `json:"name"`
	PicName           *string         `json:"pic_name"`
	PicPhone          *string         `json:"pic_phone"`
	Address           *string         `json:"address"`
	Npwp              *string         `json:"npwp"`
	BankAccount       json.RawMessage `json:"bank_account"`
	CreditLimit       *string         `json:"credit_limit"`
	LeadTimeDays      *int32          `json:"lead_time_days"`
	LeadTimeCitoDays  *int32          `json:"lead_time_cito_days"`
	PaymentTermDays   *int32          `json:"payment_term_days"`
	ContractStartDate *string         `json:"contract_start_date"`
	ContractEndDate   *string         `json:"contract_end_date"`
	IsActive          *bool           `json:"is_active"`
	RowVersion        int64           `json:"row_version"`
}

// loadSupplier fetches a live supplier and checks PermStockReceive at its
// scope: merchant-level rows need the merchant permission, company-level
// rows the company-level permission.
func loadSupplier(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.CoreSupplier, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CoreSupplier{}, false
	}
	s, err := q.GetSupplierByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "supplier not found"})
		return sqlcgen.CoreSupplier{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CoreSupplier{}, false
	}
	if s.MerchantID.Valid {
		if !RequirePermissionForMerchant(c, PermStockReceive, s.MerchantID) {
			return sqlcgen.CoreSupplier{}, false
		}
	} else if !RequireCompanyLevelPermission(c, PermStockReceive) {
		return sqlcgen.CoreSupplier{}, false
	}
	return s, true
}

// ListSuppliersHandler godoc
// @Summary List the suppliers visible to a merchant (company + its own)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/suppliers [get]
func ListSuppliersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockReceive, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListSuppliers(c.Request.Context(), sqlcgen.ListSuppliersParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, s := range rows {
		data = append(data, supplierJSON(s))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// CreateSupplierHandler godoc
// @Summary Create a merchant-level or company-level supplier
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createSupplierRequest true "Supplier fields"
// @Success 201 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /suppliers [post]
func CreateSupplierHandler(c *gin.Context) {
	var req createSupplierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var merchantID pgtype.UUID
	if req.MerchantID != "" {
		id, ok := parseUUID(req.MerchantID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
			return
		}
		if !RequirePermissionForMerchant(c, PermStockReceive, id) {
			return
		}
		// RLS pins supplier rows to the session merchant (X-Merchant-ID); a
		// cross-merchant create could only fail deep inside the transaction.
		if header := c.GetHeader("X-Merchant-ID"); header != "" {
			hid, okH := parseUUID(header)
			if !okH {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
				return
			}
			if hid != id {
				c.JSON(http.StatusForbidden, gin.H{"error": "supplier merchant mismatch"})
				return
			}
		}
		merchantID = id
	} else if !RequireCompanyLevelPermission(c, PermStockReceive) {
		return
	}
	start, errS := optDate(req.ContractStartDate)
	end, errE := optDate(req.ContractEndDate)
	if errS != nil || errE != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid contract date"})
		return
	}
	var creditLimit pgtype.Numeric
	if req.CreditLimit != nil && *req.CreditLimit != "" {
		var err error
		if creditLimit, err = parseNumeric(*req.CreditLimit); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credit_limit"})
			return
		}
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	q := sqlcgen.New(TxFromContext(c))
	s, err := q.InsertSupplier(c.Request.Context(), sqlcgen.InsertSupplierParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		Code: req.Code, Name: req.Name,
		PicName: pgText(req.PicName), PicPhone: pgText(req.PicPhone),
		Address: pgText(req.Address), Npwp: pgText(req.Npwp),
		BankAccount: []byte(req.BankAccount), CreditLimit: creditLimit,
		LeadTimeDays: optInt4(req.LeadTimeDays), LeadTimeCitoDays: optInt4(req.LeadTimeCitoDays),
		PaymentTermDays:   optInt4(req.PaymentTermDays),
		ContractStartDate: start, ContractEndDate: end,
		IsActive: isActive, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "supplier code already exists"})
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": supplierJSON(s), "meta": gin.H{}})
}

// UpdateSupplierHandler godoc
// @Summary Update a supplier (optimistic locking)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Supplier UUID"
// @Param request body patchSupplierRequest true "Supplier fields"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /suppliers/{id} [patch]
func UpdateSupplierHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	s, ok := loadSupplier(c, q, c.Param("id"))
	if !ok {
		return
	}
	var req patchSupplierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(s.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "supplier changed, reload"})
		return
	}
	var creditLimit pgtype.Numeric
	if req.CreditLimit != nil && *req.CreditLimit != "" {
		var err error
		if creditLimit, err = parseNumeric(*req.CreditLimit); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credit_limit"})
			return
		}
	}
	start := pgtype.Date{}
	if req.ContractStartDate != nil {
		if v, okD := parseDateStr(*req.ContractStartDate); !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid contract date"})
			return
		} else {
			start = v
		}
	}
	end := pgtype.Date{}
	if req.ContractEndDate != nil {
		if v, okD := parseDateStr(*req.ContractEndDate); !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid contract date"})
			return
		} else {
			end = v
		}
	}
	updated, err := q.UpdateSupplier(c.Request.Context(), sqlcgen.UpdateSupplierParams{
		Name: nargText(req.Name), PicName: nargText(req.PicName),
		PicPhone: nargText(req.PicPhone), Address: nargText(req.Address),
		Npwp: nargText(req.Npwp), BankAccount: []byte(req.BankAccount),
		CreditLimit:  creditLimit,
		LeadTimeDays: optInt4(req.LeadTimeDays), LeadTimeCitoDays: optInt4(req.LeadTimeCitoDays),
		PaymentTermDays:   optInt4(req.PaymentTermDays),
		ContractStartDate: start,
		ContractEndDate:   end,
		IsActive:          nargBool(req.IsActive),
		UpdatedBy:         AuthUserID(c), SupplierID: s.ID, ExpectedRowVersion: s.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "supplier changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": supplierJSON(updated), "meta": gin.H{}})
}

// DeleteSupplierHandler godoc
// @Summary Soft-delete a supplier
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Supplier UUID"
// @Success 200 {object} apiResponse
// @Router /suppliers/{id} [delete]
func DeleteSupplierHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	s, ok := loadSupplier(c, q, c.Param("id"))
	if !ok {
		return
	}
	if _, err := q.SoftDeleteSupplier(c.Request.Context(), sqlcgen.SoftDeleteSupplierParams{
		SupplierID: s.ID, DeletedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": s.ID.String()}, "meta": gin.H{}})
}

// §4.1 goods receipts.

func receiptLineJSON(l sqlcgen.InventoryGoodsReceiptLine) gin.H {
	return gin.H{
		"id":                      l.ID.String(),
		"item_id":                 l.ItemID.String(),
		"item_unit_id":            l.ItemUnitID.String(),
		"pack_qty":                numericExact(l.PackQty),
		"pack_content":            numericExact(l.PackContent),
		"bonus_pack_qty":          numericExact(l.BonusPackQty),
		"pack_price":              numericExact(l.PackPrice),
		"line_discount_percent":   numericExact(l.LineDiscountPercent),
		"batch_no":                textOrNil(l.BatchNo),
		"expiry_date":             dateOrNil(l.ExpiryDate),
		"short_shelf_life_reason": textOrNil(l.ShortShelfLifeReason),
		"base_qty":                numericExactOrNil(l.BaseQty),
		"unit_cost":               numericExactOrNil(l.UnitCost),
		"hna_ppn_per_pack":        numericExactOrNil(l.HnaPpnPerPack),
		"lot_id":                  uuidOrNil(l.LotID),
	}
}

func goodsReceiptJSON(r sqlcgen.InventoryGoodsReceipt, lines []sqlcgen.InventoryGoodsReceiptLine) gin.H {
	out := gin.H{
		"id":               r.ID.String(),
		"merchant_id":      r.MerchantID.String(),
		"supplier_id":      uuidOrNil(r.SupplierID),
		"location_id":      r.LocationID.String(),
		"source":           r.Source,
		"invoice_no":       textOrNil(r.InvoiceNo),
		"invoice_date":     dateOrNil(r.InvoiceDate),
		"received_on":      dateText(r.ReceivedOn),
		"invoice_discount": numericJSON(r.InvoiceDiscount),
		"freight":          numericJSON(r.Freight),
		"vat_included":     r.VatIncluded,
		"vat_percent":      numericJSON(r.VatPercent),
		"status":           r.Status,
		"note":             textOrNil(r.Note),
		"void_reason":      textOrNil(r.VoidReason),
		"posted_at":        tsOrNil(r.PostedAt),
		"row_version":      r.RowVersion,
	}
	if lines != nil {
		rendered := make([]gin.H, 0, len(lines))
		for _, l := range lines {
			rendered = append(rendered, receiptLineJSON(l))
		}
		out["lines"] = rendered
	}
	return out
}

type receiptLineRequest struct {
	ItemID               string `json:"item_id" binding:"required"`
	ItemUnitID           string `json:"item_unit_id" binding:"required"`
	PackQty              string `json:"pack_qty" binding:"required"`
	PackContent          string `json:"pack_content"`
	BonusPackQty         string `json:"bonus_pack_qty"`
	PackPrice            string `json:"pack_price"`
	LineDiscountPercent  string `json:"line_discount_percent"`
	BatchNo              string `json:"batch_no"`
	ExpiryDate           string `json:"expiry_date"`
	ShortShelfLifeReason string `json:"short_shelf_life_reason"`
}

type receiptRequest struct {
	SupplierID      string               `json:"supplier_id"`
	LocationID      string               `json:"location_id" binding:"required"`
	Source          string               `json:"source"`
	InvoiceNo       string               `json:"invoice_no"`
	InvoiceDate     string               `json:"invoice_date"`
	ReceivedOn      string               `json:"received_on" binding:"required"`
	InvoiceDiscount string               `json:"invoice_discount"`
	Freight         string               `json:"freight"`
	VatIncluded     *bool                `json:"vat_included"`
	VatPercent      string               `json:"vat_percent"`
	Note            string               `json:"note"`
	Lines           []receiptLineRequest `json:"lines" binding:"required,min=1,dive"`
	RowVersion      int64                `json:"row_version"`
}

// validReceiptSource reports whether s is one of the receipt sources.
func validReceiptSource(s string) bool {
	switch s {
	case "purchase", "program", "grant", "consignment":
		return true
	}
	return false
}

// loadGoodsReceipt fetches a live receipt and checks PermStockReceive at its
// merchant. Writes 400/403/404 and returns false on failure.
func loadGoodsReceipt(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryGoodsReceipt, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryGoodsReceipt{}, false
	}
	r, err := q.GetGoodsReceiptByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "goods receipt not found"})
		return sqlcgen.InventoryGoodsReceipt{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryGoodsReceipt{}, false
	}
	if !RequirePermissionForMerchant(c, PermStockReceive, r.MerchantID) {
		return sqlcgen.InventoryGoodsReceipt{}, false
	}
	return r, true
}

// receiptLine is a parsed draft line ready for insert.
type receiptLine struct {
	ItemID               pgtype.UUID
	ItemUnitID           pgtype.UUID
	PackQty              pgtype.Numeric
	PackContent          pgtype.Numeric
	BonusPackQty         pgtype.Numeric
	PackPrice            pgtype.Numeric
	LineDiscountPercent  pgtype.Numeric
	BatchNo              pgtype.Text
	ExpiryDate           pgtype.Date
	ShortShelfLifeReason pgtype.Text
}

// parseReceiptRequest validates the shared draft body and returns the
// receipt-level fields plus the parsed lines (pack_content defaulted to the
// packaging factor when 0/empty — spec §4.1.1). Returns ok=false when the
// response is already written.
func parseReceiptRequest(c *gin.Context, q *sqlcgen.Queries, req receiptRequest, merchantID pgtype.UUID) (supplierID, locationID pgtype.UUID, source string, invoiceDate, receivedOn pgtype.Date, invoiceDiscount, freight pgtype.Numeric, vatIncluded bool, vatPercent pgtype.Numeric, lines []receiptLine, ok bool) {
	source = req.Source
	if source == "" {
		source = "purchase"
	}
	if !validReceiptSource(source) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}
	if req.SupplierID != "" {
		id, okS := parseUUID(req.SupplierID)
		if !okS {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid supplier_id"})
			return
		}
		sup, err := q.GetSupplierByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && sup.MerchantID.Valid && sup.MerchantID != merchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "supplier not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		supplierID = id
	}
	// chk_goods_receipt_supplier: purchase/consignment receipts need one.
	if (source == "purchase" || source == "consignment") && !supplierID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "supplier is required for purchase receipts"})
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
	// Receipts land in a ruangan (room) of the merchant — building/site/bed
	// locations of the same merchant are rejected.
	if err == nil && loc.Kind != "room" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "location is not a room"})
		return
	}
	locationID = locID
	invDate, okI := parseDateStr(req.InvoiceDate)
	if !okI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice_date"})
		return
	}
	recv, okR := parseDateStr(req.ReceivedOn)
	if !okR {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid received_on"})
		return
	}
	receivedOn = recv
	invoiceDate = invDate
	var errD error
	if invoiceDiscount, errD = numericOrZero(req.InvoiceDiscount); errD != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid invoice_discount"})
		return
	}
	if freight, errD = numericOrZero(req.Freight); errD != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid freight"})
		return
	}
	vatIncluded = true
	if req.VatIncluded != nil {
		vatIncluded = *req.VatIncluded
	}
	if vatPercent, errD = numericOrZero(req.VatPercent); errD != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid vat_percent"})
		return
	}
	if !vatPercent.Valid {
		vatPercent = pgtype.Numeric{Int: big.NewInt(11), Exp: 0, Valid: true}
	}
	for _, lr := range req.Lines {
		var line receiptLine
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
		var errN error
		if line.PackQty, errN = numericOrZero(lr.PackQty); errN != nil || !line.PackQty.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_qty"})
			return
		}
		// pack_content defaults to the packaging factor when 0/empty.
		if strings.TrimSpace(lr.PackContent) == "" || lr.PackContent == "0" {
			line.PackContent = unit.Factor
			line.PackContent.Valid = true
		} else if line.PackContent, errN = numericOrZero(lr.PackContent); errN != nil || !line.PackContent.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_content"})
			return
		}
		if line.BonusPackQty, errN = numericOrZero(lr.BonusPackQty); errN != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bonus_pack_qty"})
			return
		}
		if line.PackPrice, errN = numericOrZero(lr.PackPrice); errN != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pack_price"})
			return
		}
		if line.LineDiscountPercent, errN = numericOrZero(lr.LineDiscountPercent); errN != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line_discount_percent"})
			return
		}
		line.ItemID = itemID
		line.ItemUnitID = unitID
		line.BatchNo = pgText(lr.BatchNo)
		ed, okE := parseDateStr(lr.ExpiryDate)
		if !okE {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expiry_date"})
			return
		}
		line.ExpiryDate = ed
		line.ShortShelfLifeReason = pgText(lr.ShortShelfLifeReason)
		lines = append(lines, line)
	}
	ok = true
	return
}

// numericOrZero parses "" as 0 (the columns are NOT NULL DEFAULT 0) and any
// decimal string as a numeric.
func numericOrZero(s string) (pgtype.Numeric, error) {
	if strings.TrimSpace(s) == "" {
		return pgtype.Numeric{Int: big.NewInt(0), Exp: -2, Valid: true}, nil
	}
	return parseNumeric(s)
}

// insertReceiptLines writes the parsed lines of a draft (full replace is a
// delete + insert of the same shape).
func insertReceiptLines(c *gin.Context, q *sqlcgen.Queries, receipt sqlcgen.InventoryGoodsReceipt, lines []receiptLine) bool {
	for i, l := range lines {
		if _, err := q.InsertGoodsReceiptLine(c.Request.Context(), sqlcgen.InsertGoodsReceiptLineParams{
			CompanyID: receipt.CompanyID, MerchantID: receipt.MerchantID,
			ReceiptID: receipt.ID, ItemID: l.ItemID, ItemUnitID: l.ItemUnitID,
			PackQty: l.PackQty, PackContent: l.PackContent, BonusPackQty: l.BonusPackQty,
			PackPrice: l.PackPrice, LineDiscountPercent: l.LineDiscountPercent,
			BatchNo: l.BatchNo, ExpiryDate: l.ExpiryDate,
			ShortShelfLifeReason: l.ShortShelfLifeReason, SortOrder: int32(i),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// CreateGoodsReceiptHandler godoc
// @Summary Create a goods-receipt draft with lines
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body receiptRequest true "Receipt draft"
// @Success 201 {object} apiResponse
// @Router /merchants/{id}/goods-receipts [post]
func CreateGoodsReceiptHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockReceive, merchantID) {
		return
	}
	var req receiptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	supplierID, locationID, source, invoiceDate, receivedOn, invoiceDiscount, freight, vatIncluded, vatPercent, lines, ok := parseReceiptRequest(c, q, req, merchantID)
	if !ok {
		return
	}
	receipt, err := q.InsertGoodsReceipt(c.Request.Context(), sqlcgen.InsertGoodsReceiptParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		SupplierID: supplierID, LocationID: locationID, Source: source,
		InvoiceNo: pgText(req.InvoiceNo), InvoiceDate: invoiceDate, ReceivedOn: receivedOn,
		InvoiceDiscount: invoiceDiscount, Freight: freight,
		VatIncluded: vatIncluded, VatPercent: vatPercent,
		Note: pgText(req.Note), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertReceiptLines(c, q, receipt, lines) {
		return
	}
	saved, err := q.ListGoodsReceiptLines(c.Request.Context(), receipt.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": goodsReceiptJSON(receipt, saved), "meta": gin.H{}})
}

// ListGoodsReceiptsHandler godoc
// @Summary List a merchant's goods receipts
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param status query string false "Filter by status"
// @Param from query string false "received_on >= (YYYY-MM-DD)"
// @Param to query string false "received_on <= (YYYY-MM-DD)"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/goods-receipts [get]
func ListGoodsReceiptsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermStockReceive, merchantID) {
		return
	}
	var status pgtype.Text
	if v := c.Query("status"); v != "" {
		status = pgtype.Text{String: v, Valid: true}
	}
	var from, to pgtype.Date
	if v := c.Query("from"); v != "" {
		if d, okF := parseDateStr(v); okF {
			from = d
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from"})
			return
		}
	}
	if v := c.Query("to"); v != "" {
		if d, okT := parseDateStr(v); okT {
			to = d
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to"})
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListGoodsReceipts(c.Request.Context(), sqlcgen.ListGoodsReceiptsParams{
		MerchantID: merchantID, Status: status, FromDate: from, ToDate: to,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, goodsReceiptJSON(r, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetGoodsReceiptHandler godoc
// @Summary Fetch one goods receipt with its lines
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receipt UUID"
// @Success 200 {object} apiResponse
// @Router /goods-receipts/{id} [get]
func GetGoodsReceiptHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadGoodsReceipt(c, q, c.Param("id"))
	if !ok {
		return
	}
	lines, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": goodsReceiptJSON(r, lines), "meta": gin.H{}})
}

// UpdateGoodsReceiptHandler godoc
// @Summary Replace a draft receipt (header + full line replace)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receipt UUID"
// @Param request body receiptRequest true "Receipt draft"
// @Success 200 {object} apiResponse
// @Failure 409 {object} apiErrorResponse
// @Router /goods-receipts/{id} [patch]
func UpdateGoodsReceiptHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadGoodsReceipt(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "receipt is not draft"})
		return
	}
	var req receiptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	if req.RowVersion != int64(r.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "receipt changed, reload"})
		return
	}
	supplierID, locationID, source, invoiceDate, receivedOn, invoiceDiscount, freight, vatIncluded, vatPercent, lines, ok := parseReceiptRequest(c, q, req, r.MerchantID)
	if !ok {
		return
	}
	updated, err := q.UpdateGoodsReceiptDraft(c.Request.Context(), sqlcgen.UpdateGoodsReceiptDraftParams{
		SupplierID: supplierID, LocationID: locationID, Source: source,
		InvoiceNo: pgText(req.InvoiceNo), InvoiceDate: invoiceDate, ReceivedOn: receivedOn,
		InvoiceDiscount: invoiceDiscount, Freight: freight,
		VatIncluded: vatIncluded, VatPercent: vatPercent, Note: pgText(req.Note),
		UpdatedBy: AuthUserID(c), ReceiptID: r.ID, ExpectedRowVersion: r.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "receipt changed, reload"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if err := q.DeleteGoodsReceiptLines(c.Request.Context(), r.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	if !insertReceiptLines(c, q, updated, lines) {
		return
	}
	saved, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": goodsReceiptJSON(updated, saved), "meta": gin.H{}})
}

// receiptLinePosting carries the pure cost-engine results for one line.
type receiptLinePosting struct {
	Line     sqlcgen.InventoryGoodsReceiptLine
	Item     sqlcgen.InventoryItem
	BaseQty  pgtype.Numeric
	UnitCost pgtype.Numeric
	HnaPpn   pgtype.Numeric
}

// validateReceiptLines checks batch/ED/minimum shelf life per line (spec
// §4.1.3). Past EDs are always 400; short shelf life collects a 409 line
// report unless the line carries an override reason.
func validateReceiptLines(c *gin.Context, q *sqlcgen.Queries, r sqlcgen.InventoryGoodsReceipt, lines []sqlcgen.InventoryGoodsReceiptLine, setting stockSetting) ([]receiptLinePosting, bool) {
	posting := make([]receiptLinePosting, 0, len(lines))
	shorts := make([]gin.H, 0)
	for _, l := range lines {
		item, err := q.GetInventoryItemByID(c.Request.Context(), l.ItemID)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		if item.TrackExpiry && !l.ExpiryDate.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expiry date is required"})
			return nil, false
		}
		if item.TrackBatch && !l.BatchNo.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "batch number is required"})
			return nil, false
		}
		if l.ExpiryDate.Valid && l.ExpiryDate.Time.Before(r.ReceivedOn.Time) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expiry date is in the past"})
			return nil, false
		}
		if l.ExpiryDate.Valid {
			minMonths := setting.MinShelfLife
			if item.PricingCategory == "vaccine" {
				minMonths = setting.MinShelfLifeVaccine
			}
			left := monthsLeft(r.ReceivedOn.Time, l.ExpiryDate.Time)
			reasonSet := l.ShortShelfLifeReason.Valid && strings.TrimSpace(l.ShortShelfLifeReason.String) != ""
			if left < minMonths && !reasonSet {
				shorts = append(shorts, gin.H{
					"line_id":     l.ID.String(),
					"item":        item.Code,
					"expiry_date": dateText(l.ExpiryDate),
					"months_left": left,
				})
			}
		}
		posting = append(posting, receiptLinePosting{Line: l, Item: item})
	}
	if len(shorts) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "short_shelf_life", "lines": shorts})
		return nil, false
	}
	return posting, true
}

// stockSetting is the merchant warehouse setting subset posting needs; the
// migration defaults apply when no row exists yet.
type stockSetting struct {
	MinShelfLife        int
	MinShelfLifeVaccine int
	VatCreditable       bool
}

func loadStockSetting(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) (stockSetting, error) {
	setting := stockSetting{MinShelfLife: 12, MinShelfLifeVaccine: 6}
	row, err := q.GetMerchantStockSetting(c.Request.Context(), merchantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return setting, nil
	}
	if err != nil {
		return setting, err
	}
	setting.MinShelfLife = int(row.MinShelfLifeMonths)
	setting.MinShelfLifeVaccine = int(row.MinShelfLifeVaccineMonths)
	setting.VatCreditable = row.InputVatCreditable
	return setting, nil
}

// PostGoodsReceiptHandler godoc
// @Summary Post a draft goods receipt (lots, balances, HPP, reference price)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receipt UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /goods-receipts/{id}/post [post]
func PostGoodsReceiptHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadGoodsReceipt(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "receipt is not draft"})
		return
	}
	lines, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	setting, err := loadStockSetting(c, q, r.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	posting, ok := validateReceiptLines(c, q, r, lines, setting)
	if !ok {
		return
	}
	// Pure landed-cost engine over the stored draft lines.
	input := receiptCostInput{
		VatIncluded:   r.VatIncluded,
		VatPercent:    numericToRat(r.VatPercent),
		VatCreditable: setting.VatCreditable,
	}
	for _, p := range posting {
		input.Lines = append(input.Lines, receiptLineInput{
			PackQty:             numericToRat(p.Line.PackQty),
			PackContent:         numericToRat(p.Line.PackContent),
			BonusPackQty:        numericToRat(p.Line.BonusPackQty),
			PackPrice:           numericToRat(p.Line.PackPrice),
			LineDiscountPercent: numericToRat(p.Line.LineDiscountPercent),
		})
	}
	costs := computeReceiptLineCosts(input)
	receivedAt := time.Now().UTC()
	for i, p := range posting {
		var errP error
		if p.BaseQty, errP = parseNumeric(costs[i].BaseQty); errP != nil {
			respondInternalError(c, errP)
			return
		}
		if p.UnitCost, errP = parseNumeric(costs[i].UnitCost); errP != nil {
			respondInternalError(c, errP)
			return
		}
		if p.HnaPpn, errP = parseNumeric(costs[i].HnaPpnPerPack); errP != nil {
			respondInternalError(c, errP)
			return
		}
		// Lot: one '-' lot without ED for items without batch tracking.
		batch := "-"
		if p.Item.TrackBatch {
			batch = p.Line.BatchNo.String
		}
		var ed pgtype.Date
		if p.Item.TrackExpiry {
			ed = p.Line.ExpiryDate
		}
		lot, err := q.UpsertReceiptLot(c.Request.Context(), sqlcgen.UpsertReceiptLotParams{
			CompanyID: r.CompanyID, ItemID: p.Line.ItemID, BatchNo: batch, ExpiryDate: ed,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		// Moving-average HPP in SQL numeric, BEFORE the balance moves.
		if r.Source == "purchase" {
			if _, err := q.ApplyReceiptMovingAverage(c.Request.Context(), sqlcgen.ApplyReceiptMovingAverageParams{
				CompanyID: r.CompanyID, MerchantID: r.MerchantID, ItemID: p.Line.ItemID,
				UnitCost: p.UnitCost, BaseQty: p.BaseQty,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		if _, err := applyStockMove(c.Request.Context(), q, r.MerchantID, r.CompanyID, AuthUserID(c), stockMoveInput{
			Key:        stockKey{LocationID: r.LocationID, LotID: lot.ID, Source: r.Source, Status: "available"},
			ItemID:     p.Line.ItemID,
			Qty:        p.BaseQty,
			UnitCost:   p.UnitCost,
			MoveType:   "receipt",
			RefType:    "goods_receipt",
			RefID:      r.ID,
			MoveDate:   r.ReceivedOn.Time,
			ReceivedAt: receivedAt,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
		if _, err := q.CompleteGoodsReceiptLine(c.Request.Context(), sqlcgen.CompleteGoodsReceiptLineParams{
			BaseQty: p.BaseQty, UnitCost: p.UnitCost, HnaPpnPerPack: p.HnaPpn,
			LotID: lot.ID, LineID: p.Line.ID,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		// Active formulary entry: reference price = HNA+PPN per pack, then
		// proposal regeneration (spec §4.1.6).
		mi, err := q.GetMerchantItemByMerchantItem(c.Request.Context(), sqlcgen.GetMerchantItemByMerchantItemParams{
			MerchantID: r.MerchantID, ItemID: p.Line.ItemID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			respondInternalError(c, err)
			return
		}
		if err == nil && mi.IsActive {
			updated, err := q.SetMerchantItemReference(c.Request.Context(), sqlcgen.SetMerchantItemReferenceParams{
				MerchantItemID:      mi.ID,
				ReferenceAmount:     p.HnaPpn,
				ReferenceItemUnitID: p.Line.ItemUnitID,
				ReferenceFactor:     scaledToNumeric(mustNumericScaled(p.Line.PackContent, 3), 3),
				ReferenceUpdatedBy:  AuthUserID(c),
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			if err := regenerateProposals(c, q, updated, "reference_changed"); err != nil {
				respondRegenerateError(c, err)
				return
			}
		}
	}
	posted, err := q.MarkGoodsReceiptPosted(c.Request.Context(), sqlcgen.MarkGoodsReceiptPostedParams{
		PostedBy: AuthUserID(c), ReceiptID: r.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "receipt is not draft"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": goodsReceiptJSON(posted, saved), "meta": gin.H{}})
}

type voidReceiptRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// VoidGoodsReceiptHandler godoc
// @Summary Void a posted goods receipt (reversal moves + HPP rollback)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receipt UUID"
// @Param request body voidReceiptRequest true "Void reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /goods-receipts/{id}/void [post]
func VoidGoodsReceiptHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	r, ok := loadGoodsReceipt(c, q, c.Param("id"))
	if !ok {
		return
	}
	if r.Status != "posted" {
		c.JSON(http.StatusConflict, gin.H{"error": "receipt is not posted"})
		return
	}
	var req voidReceiptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	lines, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Every line's stock must still be fully on hand (spec §4.1.7).
	type voidLine struct {
		Line      sqlcgen.InventoryGoodsReceiptLine
		BaseMilli int64
	}
	plans := make([]voidLine, 0, len(lines))
	for _, l := range lines {
		baseMilli, errM := numericToMilli(l.BaseQty)
		if errM != nil {
			respondInternalError(c, errM)
			return
		}
		row, err := q.LockStockBalance(c.Request.Context(), sqlcgen.LockStockBalanceParams{
			MerchantID: r.MerchantID, LocationID: r.LocationID,
			LotID: l.LotID, Source: r.Source, Status: "available",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "received stock was already used"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		balMilli, errB := numericToMilli(row.Quantity)
		if errB != nil {
			respondInternalError(c, errB)
			return
		}
		if balMilli < baseMilli {
			c.JSON(http.StatusConflict, gin.H{"error": "received stock was already used"})
			return
		}
		plans = append(plans, voidLine{Line: l, BaseMilli: baseMilli})
	}
	// Roll the average back while the balances still include the receipt.
	if r.Source == "purchase" {
		for _, p := range plans {
			if _, err := q.ReverseReceiptMovingAverage(c.Request.Context(), sqlcgen.ReverseReceiptMovingAverageParams{
				CompanyID: r.CompanyID, MerchantID: r.MerchantID, ItemID: p.Line.ItemID,
				UnitCost: p.Line.UnitCost, BaseQty: p.Line.BaseQty,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
	}
	for _, p := range plans {
		moveID, err := q.FindStockMoveReceipt(c.Request.Context(), sqlcgen.FindStockMoveReceiptParams{
			RefID: r.ID, LotID: p.Line.LotID,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if _, err := applyStockMove(c.Request.Context(), q, r.MerchantID, r.CompanyID, AuthUserID(c), stockMoveInput{
			Key:        stockKey{LocationID: r.LocationID, LotID: p.Line.LotID, Source: r.Source, Status: "available"},
			ItemID:     p.Line.ItemID,
			Qty:        negateNumeric(p.Line.BaseQty),
			UnitCost:   p.Line.UnitCost,
			MoveType:   "reversal",
			RefType:    "goods_receipt",
			RefID:      r.ID,
			ReversalOf: moveID,
			Reason:     req.Reason,
			MoveDate:   r.ReceivedOn.Time,
		}); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
	}
	voided, err := q.MarkGoodsReceiptVoid(c.Request.Context(), sqlcgen.MarkGoodsReceiptVoidParams{
		VoidReason: req.Reason, UpdatedBy: AuthUserID(c), ReceiptID: r.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "receipt is not posted"})
			return
		}
		respondInternalError(c, err)
		return
	}
	saved, err := q.ListGoodsReceiptLines(c.Request.Context(), r.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": goodsReceiptJSON(voided, saved), "meta": gin.H{}})
}
