// internal/server/fee_payout.go
// Monthly practitioner fee payout batches: create, other income, lock
// (PPh 21 per spec 2026-10-07-tariff-d-practitioner-fees-tax §4.3),
// paid, and the tax CSV export. Money is in cents; JSON money is a
// 2-decimal string.
package server

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterFeePayoutRoutes wires the fee payout batch endpoints under the
// authenticated router group (wired in server.go right after
// RegisterFeeRoutes, whose signature is unchanged).
func RegisterFeePayoutRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.POST("/merchants/:id/fee-batches", CreateFeeBatchHandler)
	rg.GET("/merchants/:id/fee-batches", ListFeeBatchesHandler)
	rg.GET("/fee-batches/:id", GetFeeBatchHandler)
	rg.PUT("/fee-batches/:id/lines/:physician_id", UpdateFeeBatchLineHandler)
	rg.DELETE("/fee-batches/:id", DeleteFeeBatchHandler)
	rg.POST("/fee-batches/:id/lock", LockFeeBatchHandler)
	rg.POST("/fee-batches/:id/paid", PayFeeBatchHandler)
	rg.GET("/fee-batches/:id/tax-export.csv", AccessLog("fee_batch", "tax_export", "id"), TaxExportHandler(cfg))
}

// moneyStr renders a non-null numeric(15,2) as "1234.56".
func moneyStr(n pgtype.Numeric) string {
	c, err := numericToCents(n)
	if err != nil {
		return "0.00"
	}
	return formatCents(c)
}

// moneyOrNil renders a nullable numeric as "1234.56" (nil when NULL).
func moneyOrNil(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	return moneyStr(n)
}

// fpTextOrNil renders a nullable text (nil when NULL/empty).
func fpTextOrNil(t pgtype.Text) any {
	if !t.Valid || t.String == "" {
		return nil
	}
	return t.String
}

// feeLineJSON maps a fee payout line to a snake_case JSON object.
func feeLineJSON(l sqlcgen.OperationsFeePayoutLine) gin.H {
	return gin.H{
		"id":              uuidJSON(l.ID),
		"batch_id":        uuidJSON(l.BatchID),
		"physician_id":    uuidJSON(l.PhysicianID),
		"employment_type": l.EmploymentType,
		"ptkp_status":     fpTextOrNil(l.PtkpStatus),
		"gross_amount":    moneyStr(l.GrossAmount),
		"fee_amount":      moneyStr(l.FeeAmount),
		"other_gross":     moneyStr(l.OtherGross),
		"tax_method":      fpTextOrNil(l.TaxMethod),
		"tax_base":        moneyOrNil(l.TaxBase),
		"ter_category":    fpTextOrNil(l.TerCategory),
		"tax_rate":        moneyOrNil(l.TaxRate),
		"tax_amount":      moneyOrNil(l.TaxAmount),
		"net_amount":      moneyOrNil(l.NetAmount),
	}
}

// feeBatchDeadlines returns the §4.3.7 deadlines: pay by the 15th and file
// by the 20th of the following month.
func feeBatchDeadlines(year, month int) (deadlineSetor, deadlineLapor string) {
	next := time.Month(month + 1)
	return time.Date(year, next, 15, 0, 0, 0, 0, time.UTC).Format("2006-01-02"),
		time.Date(year, next, 20, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// feeBatchJSON maps a batch and its lines to a snake_case JSON object.
func feeBatchJSON(b sqlcgen.OperationsFeePayoutBatch, lines []sqlcgen.OperationsFeePayoutLine) gin.H {
	data := make([]gin.H, 0, len(lines))
	for _, l := range lines {
		data = append(data, feeLineJSON(l))
	}
	setor, lapor := feeBatchDeadlines(int(b.PeriodYear), int(b.PeriodMonth))
	return gin.H{
		"id":             uuidJSON(b.ID),
		"company_id":     uuidJSON(b.CompanyID),
		"merchant_id":    uuidJSON(b.MerchantID),
		"period_year":    b.PeriodYear,
		"period_month":   b.PeriodMonth,
		"status":         b.Status,
		"locked_at":      tsJSON(b.LockedAt),
		"locked_by":      uuidJSON(b.LockedBy),
		"paid_at":        tsJSON(b.PaidAt),
		"paid_by":        uuidJSON(b.PaidBy),
		"created_at":     tsJSON(b.CreatedAt),
		"created_by":     uuidJSON(b.CreatedBy),
		"updated_at":     tsJSON(b.UpdatedAt),
		"row_version":    b.RowVersion,
		"deadline_setor": setor,
		"deadline_lapor": lapor,
		"lines":          data,
	}
}

// loadFeeBatchAuthorized loads a fee payout batch and enforces PermFeeManage
// against its merchant; writes the error response itself.
func loadFeeBatchAuthorized(c *gin.Context, rawID string) (sqlcgen.OperationsFeePayoutBatch, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.OperationsFeePayoutBatch{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, err := q.GetFeeBatchByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fee batch not found"})
		return sqlcgen.OperationsFeePayoutBatch{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsFeePayoutBatch{}, false
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, batch.MerchantID) {
		return sqlcgen.OperationsFeePayoutBatch{}, false
	}
	return batch, true
}

// loadTaxTables loads the tax reference rows effective on onDate (the last
// day of the batch month): latest row per key/category/status.
func loadTaxTables(c *gin.Context, q *sqlcgen.Queries, onDate time.Time) (pph21Tables, error) {
	ctx := c.Request.Context()
	on := pgtype.Date{Time: onDate, Valid: true}
	tables := pph21Tables{
		PTKP: map[string]int64{},
	}
	terRows, err := q.ListTaxTERCurrent(ctx, on)
	if err != nil {
		return tables, err
	}
	for _, r := range terRows {
		lower, err := numericToCents(r.LowerExclusive)
		if err != nil {
			return tables, err
		}
		upper := int64(-1)
		if r.UpperInclusive.Valid {
			if upper, err = numericToCents(r.UpperInclusive); err != nil {
				return tables, err
			}
		}
		rate, err := numericToCents(r.RatePercent) // percent → basis points
		if err != nil {
			return tables, err
		}
		tables.TER = append(tables.TER, terRow{
			Category: r.Category, LowerExclusive: lower, UpperInclusive: upper, RateBP: rate,
		})
	}
	ptkpRows, err := q.ListTaxPTKPCurrent(ctx, on)
	if err != nil {
		return tables, err
	}
	for _, r := range ptkpRows {
		amount, err := numericToCents(r.AnnualAmount)
		if err != nil {
			return tables, err
		}
		tables.PTKP[r.Status] = amount
	}
	brackets, err := q.ListTaxPasal17Current(ctx, on)
	if err != nil {
		return tables, err
	}
	for _, r := range brackets {
		rate, err := numericToCents(r.RatePercent)
		if err != nil {
			return tables, err
		}
		upper := int64(-1)
		if r.UpperInclusive.Valid {
			if upper, err = numericToCents(r.UpperInclusive); err != nil {
				return tables, err
			}
		}
		tables.Pasal17 = append(tables.Pasal17, taxBracket{UpperInclusive: upper, RateBP: rate})
	}
	settings, err := q.ListTaxSettingsCurrent(ctx, on)
	if err != nil {
		return tables, err
	}
	for _, s := range settings {
		val, err := numericToCents(s.Value)
		if err != nil {
			return tables, err
		}
		switch s.Key {
		case "biaya_jabatan_percent":
			tables.BiayaJabatanBP = val
		case "biaya_jabatan_max_annual":
			tables.BiayaJabatanMaxAnnual = val
		case "partner_base_percent":
			tables.PartnerBaseBP = val
		}
	}
	return tables, nil
}

// CreateFeeBatchHandler godoc
// @Summary Create a monthly fee payout batch from open fees
// @Tags fee-payout
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body object true "{year, month}"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/fee-batches [post]
func CreateFeeBatchHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req struct {
		Year  int `json:"year" binding:"required"`
		Month int `json:"month" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Month < 1 || req.Month > 12 || req.Year < 2024 || req.Year > 2100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid year or month"})
		return
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	now := time.Now().In(loc)
	if req.Year > now.Year() || (req.Year == now.Year() && req.Month > int(now.Month())) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fee batch month is in the future"})
		return
	}
	monthEnd := time.Date(req.Year, time.Month(req.Month+1), 0, 0, 0, 0, 0, time.UTC)
	if _, err := q.GetFeeBatchByPeriod(c.Request.Context(), sqlcgen.GetFeeBatchByPeriodParams{
		MerchantID: merchantID, PeriodYear: int16(req.Year), PeriodMonth: int16(req.Month),
	}); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "fee batch for this month already exists"})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	sums, err := q.SumOpenFeesByPhysician(c.Request.Context(), sqlcgen.SumOpenFeesByPhysicianParams{
		MerchantID: merchantID, ServiceDate: pgtype.Date{Time: monthEnd, Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(sums) == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "no open fees up to this month"})
		return
	}
	batch, err := q.CreateFeeBatch(c.Request.Context(), sqlcgen.CreateFeeBatchParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		PeriodYear: int16(req.Year), PeriodMonth: int16(req.Month), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, s := range sums {
		gross, err := numericToCents(s.GrossAmount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		fee, err := numericToCents(s.FeeAmount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if err := q.InsertFeePayoutLine(c.Request.Context(), sqlcgen.InsertFeePayoutLineParams{
			CompanyID: batch.CompanyID, MerchantID: batch.MerchantID, BatchID: batch.ID,
			PhysicianID: s.PhysicianID, EmploymentType: s.EmploymentType, PtkpStatus: optText(s.PtkpStatus),
			GrossAmount: centsNum(gross), FeeAmount: centsNum(fee),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if err := q.MarkFeesInBatch(c.Request.Context(), sqlcgen.MarkFeesInBatchParams{
		MerchantID: merchantID, BatchID: batch.ID, ServiceDate: pgtype.Date{Time: monthEnd, Valid: true},
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	lines, err := q.ListFeePayoutLines(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": feeBatchJSON(batch, lines), "meta": gin.H{}})
}

// ListFeeBatchesHandler godoc
// @Summary List fee payout batches of a merchant
// @Tags fee-payout
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param year query int false "Filter by period year"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/fee-batches [get]
func ListFeeBatchesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermFeeManage, merchantID) {
		return
	}
	var year pgtype.Int2
	if raw := c.Query("year"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid year"})
			return
		}
		year = pgtype.Int2{Int16: int16(v), Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	batches, err := q.ListFeeBatchesByMerchant(c.Request.Context(), sqlcgen.ListFeeBatchesByMerchantParams{
		MerchantID: merchantID, Year: year,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(batches))
	for _, b := range batches {
		lines, err := q.ListFeePayoutLines(c.Request.Context(), b.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		data = append(data, feeBatchJSON(b, lines))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetFeeBatchHandler godoc
// @Summary Get one fee payout batch with its lines
// @Tags fee-payout
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /fee-batches/{id} [get]
func GetFeeBatchHandler(c *gin.Context) {
	batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	lines, err := q.ListFeePayoutLines(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": feeBatchJSON(batch, lines), "meta": gin.H{}})
}

// UpdateFeeBatchLineHandler godoc
// @Summary Set the other income of one line (employees, draft only)
// @Tags fee-payout
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Param physician_id path string true "Physician UUID"
// @Param request body object true "{other_gross}"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-batches/{id}/lines/{physician_id} [put]
func UpdateFeeBatchLineHandler(c *gin.Context) {
	batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	physicianID, ok := parseUUID(c.Param("physician_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid physician_id"})
		return
	}
	if batch.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "fee batch is not draft"})
		return
	}
	var req struct {
		OtherGross *string `json:"other_gross" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	otherCents, err := parseCents(*req.OtherGross)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid other_gross"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	line, err := q.GetFeePayoutLine(c.Request.Context(), sqlcgen.GetFeePayoutLineParams{
		BatchID: batch.ID, PhysicianID: physicianID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fee payout line not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if line.EmploymentType != "employee" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "other gross applies to employees only"})
		return
	}
	if _, err := q.SetFeePayoutLineOtherGross(c.Request.Context(), sqlcgen.SetFeePayoutLineOtherGrossParams{
		BatchID: batch.ID, PhysicianID: physicianID, OtherGross: centsNum(otherCents),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	lines, err := q.ListFeePayoutLines(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": feeBatchJSON(batch, lines), "meta": gin.H{}})
}

// DeleteFeeBatchHandler godoc
// @Summary Delete a draft fee batch and release its fees
// @Tags fee-payout
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-batches/{id} [delete]
func DeleteFeeBatchHandler(c *gin.Context) {
	batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if batch.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "fee batch is not draft"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.ReleaseBatchFees(c.Request.Context(), batch.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.DeleteFeeBatch(c.Request.Context(), batch.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// fpTaxParams builds the tax columns of one locked line per §4.3.4: owner →
// none; partner → partner_50pct; employee Jan–Nov → ter_monthly; employee
// December → annual_december over the locked/paid Jan–Nov lines of the same
// year plus this line.
func fpTaxParams(c *gin.Context, q *sqlcgen.Queries, batch sqlcgen.OperationsFeePayoutBatch,
	row sqlcgen.ListFeePayoutLinesWithPersonRow, tables pph21Tables,
) (sqlcgen.UpdateFeePayoutLineTaxParams, error) {
	gross, err := numericToCents(row.GrossAmount)
	if err != nil {
		return sqlcgen.UpdateFeePayoutLineTaxParams{}, err
	}
	other, err := numericToCents(row.OtherGross)
	if err != nil {
		return sqlcgen.UpdateFeePayoutLineTaxParams{}, err
	}
	fee, err := numericToCents(row.FeeAmount)
	if err != nil {
		return sqlcgen.UpdateFeePayoutLineTaxParams{}, err
	}
	tax := int64(0)
	params := sqlcgen.UpdateFeePayoutLineTaxParams{ID: row.ID}
	ptkp := row.PtkpStatus.String
	switch {
	case row.EmploymentType == "owner":
		params.TaxMethod = optText("none")
	case row.EmploymentType == "partner":
		base, partnerTax := pph21Partner(gross, tables)
		tax = partnerTax
		params.TaxMethod = optText("partner_50pct")
		params.TaxBase = centsNum(base)
	case int(batch.PeriodMonth) < 12:
		category, rateBP, terTax := pph21TERMonthly(gross+other, ptkp, tables)
		tax = terTax
		params.TaxMethod = optText("ter_monthly")
		params.TaxBase = centsNum(gross + other)
		params.TerCategory = optText(category)
		if rateBP > 0 {
			params.TaxRate = centsToNumeric(rateBP)
		}
	default:
		var annualGross, janNovTax int64
		yearRows, err := q.ListFeePayoutYearLines(c.Request.Context(), sqlcgen.ListFeePayoutYearLinesParams{
			PhysicianID: row.PhysicianID, MerchantID: batch.MerchantID, PeriodYear: batch.PeriodYear,
		})
		if err != nil {
			return params, err
		}
		for _, y := range yearRows {
			g, err := numericToCents(y.GrossAmount)
			if err != nil {
				return params, err
			}
			o, err := numericToCents(y.OtherGross)
			if err != nil {
				return params, err
			}
			t, err := numericToCents(y.TaxAmount)
			if err != nil {
				return params, err
			}
			annualGross += g + o
			janNovTax += t
		}
		annualGross += gross + other
		pkp, _, decTax := pph21December(annualGross, ptkp, janNovTax, tables)
		tax = decTax
		params.TaxMethod = optText("annual_december")
		params.TaxBase = centsNum(pkp)
		params.TerCategory = optText(terCategory(ptkp))
	}
	if tax > 0 {
		params.TaxAmount = centsNum(tax)
	} else {
		params.TaxAmount = centsNum(tax) // may be negative (December over-withholding)
	}
	params.NetAmount = centsNum(fee - max64(tax, 0))
	return params, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// LockFeeBatchHandler godoc
// @Summary Lock a draft fee batch and compute PPh 21 per line
// @Tags fee-payout
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-batches/{id}/lock [post]
func LockFeeBatchHandler(c *gin.Context) {
	batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if batch.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "fee batch is not draft"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListFeePayoutLinesWithPerson(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, r := range rows {
		if !r.Nik.Valid || r.Nik.String == "" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("practitioner %s has no NIK", r.PersonFullName)})
			return
		}
	}
	tables, err := loadTaxTables(c, q, time.Date(int(batch.PeriodYear), time.Month(batch.PeriodMonth+1), 0, 0, 0, 0, 0, time.UTC))
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, r := range rows {
		params, err := fpTaxParams(c, q, batch, r, tables)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if _, err := q.UpdateFeePayoutLineTax(c.Request.Context(), params); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	locked, err := q.LockFeeBatch(c.Request.Context(), sqlcgen.LockFeeBatchParams{ID: batch.ID, LockedBy: AuthUserID(c)})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	lines, err := q.ListFeePayoutLines(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": feeBatchJSON(locked, lines), "meta": gin.H{}})
}

// PayFeeBatchHandler godoc
// @Summary Mark a locked fee batch as paid
// @Tags fee-payout
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-batches/{id}/paid [post]
func PayFeeBatchHandler(c *gin.Context) {
	batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
	if !ok {
		return
	}
	if batch.Status != "locked" {
		c.JSON(http.StatusConflict, gin.H{"error": "fee batch is not locked"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	paid, err := q.PayFeeBatch(c.Request.Context(), sqlcgen.PayFeeBatchParams{ID: batch.ID, PaidBy: AuthUserID(c)})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.PayBatchFees(c.Request.Context(), batch.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	lines, err := q.ListFeePayoutLines(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": feeBatchJSON(paid, lines), "meta": gin.H{}})
}

// TaxExportHandler godoc
// @Summary Export the PPh 21 CSV of a locked/paid fee batch
// @Tags fee-payout
// @Produce text/csv
// @Security BearerAuth
// @Param id path string true "Fee batch UUID"
// @Success 200 {string} string "CSV"
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /fee-batches/{id}/tax-export.csv [get]
func TaxExportHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		batch, ok := loadFeeBatchAuthorized(c, c.Param("id"))
		if !ok {
			return
		}
		if batch.Status == "draft" {
			c.JSON(http.StatusConflict, gin.H{"error": "fee batch is not locked"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		rows, err := q.ListFeePayoutLinesWithPerson(c.Request.Context(), batch.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var sb strings.Builder
		w := csv.NewWriter(&sb)
		w.Comma = ';'
		if err := w.Write([]string{"masa_pajak", "tahun", "nik", "npwp", "nama", "status_ptkp", "jenis_pemotongan", "penghasilan_bruto", "dasar_pengenaan", "tarif", "pph_dipotong"}); err != nil {
			respondInternalError(c, err)
			return
		}
		for _, r := range rows {
			nik, err := decryptNIK(c.Request.Context(), TxFromContext(c), r.Nik, cfg.NIKEncryptionKey)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			gross, err := numericToCents(r.GrossAmount)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			other, err := numericToCents(r.OtherGross)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			bruto := gross
			if r.EmploymentType == "employee" {
				bruto += other
			}
			jenis := ""
			switch r.TaxMethod.String {
			case "partner_50pct":
				jenis = "BP21-bukan_pegawai"
			case "ter_monthly":
				jenis = "BPMP-pegawai_tetap"
			case "annual_december":
				jenis = "BPA1-desember"
			}
			tarif := ""
			if r.TaxRate.Valid {
				tarif = numStr2(r.TaxRate)
			}
			tax := ""
			if r.TaxAmount.Valid {
				tax = moneyStr(r.TaxAmount)
			}
			base := ""
			if r.TaxBase.Valid {
				base = moneyStr(r.TaxBase)
			}
			ptkp := ""
			if r.PtkpStatus.Valid {
				ptkp = r.PtkpStatus.String
			}
			npwp := ""
			if r.Npwp.Valid {
				npwp = r.Npwp.String
			}
			if err := w.Write([]string{
				csvSafe(strconv.Itoa(int(batch.PeriodMonth))), csvSafe(strconv.Itoa(int(batch.PeriodYear))),
				csvSafe(nik), csvSafe(npwp), csvSafe(r.PersonFullName), csvSafe(ptkp), csvSafe(jenis),
				csvSafe(formatCents(bruto)), csvSafe(base), csvSafe(tarif), csvSafe(tax),
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			respondInternalError(c, err)
			return
		}
		body := sb.String()
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"tax-export-%d-%02d.csv\"", batch.PeriodYear, batch.PeriodMonth))
		c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(body))
	}
}
