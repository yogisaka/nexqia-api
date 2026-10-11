// internal/server/payer_payment.go
// Payer payment API (spec 2026-10-07-tariff-b2-receivables-claims §4.4.1–2):
// payments from a payer with allocations onto receivables, later allocations
// of unallocated remainder, and reversals as negative payments. Every
// allocation takes SELECT … FOR UPDATE on the receivable row before the
// balance check so two concurrent payments can never exceed the balance.
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

// payerPaymentView carries the rendered fields of one payment row; both the
// detail and the list sqlc rows flatten them, each gets a small converter.
type payerPaymentView struct {
	id, payerID, batchID, reversalOf pgtype.UUID
	amount                           pgtype.Numeric
	paidOn                           pgtype.Date
	reference, note                  pgtype.Text
}

func rowToPaymentView(r sqlcgen.GetPayerPaymentByIDRow) payerPaymentView {
	return payerPaymentView{id: r.ID, payerID: r.PayerID, batchID: r.BatchID, reversalOf: r.ReversalOf,
		amount: r.Amount, paidOn: r.PaidOn, reference: r.Reference, note: r.Note}
}

func listRowToPaymentView(r sqlcgen.ListPayerPaymentsByPayerRow) payerPaymentView {
	return payerPaymentView{id: r.ID, payerID: r.PayerID, batchID: r.BatchID, reversalOf: r.ReversalOf,
		amount: r.Amount, paidOn: r.PaidOn, reference: r.Reference, note: r.Note}
}

// payerPaymentJSON renders one payment (snake_case, money as decimal strings).
func payerPaymentJSON(v payerPaymentView, payerIDStr, payerCode, payerName string, allocatedC int64, reversed bool) gin.H {
	amountC, _ := numericToCents(v.amount)
	var batchID, reference, note, reversalOf any
	if v.batchID.Valid {
		batchID = v.batchID.String()
	}
	if v.reference.Valid {
		reference = v.reference.String
	}
	if v.note.Valid {
		note = v.note.String
	}
	if v.reversalOf.Valid {
		reversalOf = v.reversalOf.String()
	}
	return gin.H{
		"id":          v.id.String(),
		"payer":       gin.H{"id": payerIDStr, "code": payerCode, "name": payerName},
		"batch_id":    batchID,
		"amount":      formatCents(amountC),
		"paid_on":     recvDate(v.paidOn),
		"reference":   reference,
		"note":        note,
		"reversal_of": reversalOf,
		"allocated":   formatCents(allocatedC),
		"unallocated": formatCents(amountC - allocatedC),
		"reversed":    reversed,
	}
}

// loadPayerPaymentAuthorized fetches the joined payment row and checks
// billing.receivable.manage against its merchant; writes the response itself.
func loadPayerPaymentAuthorized(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.GetPayerPaymentByIDRow, bool) {
	pay, err := q.GetPayerPaymentByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payer payment not found"})
			return sqlcgen.GetPayerPaymentByIDRow{}, false
		}
		respondInternalError(c, err)
		return sqlcgen.GetPayerPaymentByIDRow{}, false
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, pay.MerchantID) {
		return sqlcgen.GetPayerPaymentByIDRow{}, false
	}
	return pay, true
}

// payerAllocationInput is one requested allocation in a request body.
type payerAllocationInput struct {
	ReceivableID string `json:"receivable_id" binding:"required"`
	Amount       string `json:"amount" binding:"required"`
}

// applyPaymentAllocations validates and inserts allocations for an existing
// (positive) payment row. Every receivable is locked with SELECT … FOR UPDATE
// before its balance check, and the payment total cap (allocations ≤ amount)
// is enforced against the allocations already on the payment. Returns the
// inserted allocation views; when validation fails the response has been
// written and ok is false (the ≥ 400 response rolls the tx back).
func applyPaymentAllocations(c *gin.Context, q *sqlcgen.Queries, pay sqlcgen.OperationsPayerPayment, inputs []payerAllocationInput) ([]gin.H, bool) {
	ctx := c.Request.Context()
	amountC, err := numericToCents(pay.Amount)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	// Parse and aggregate per receivable (duplicates in one request are summed).
	type target struct {
		id      pgtype.UUID
		amountC int64
	}
	targets := make([]target, 0, len(inputs))
	byID := map[pgtype.UUID]int{}
	var totalC int64
	for _, in := range inputs {
		recID, ok := parseUUID(in.ReceivableID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid receivable_id"})
			return nil, false
		}
		amt, err := parseCents(in.Amount)
		if err != nil || amt <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return nil, false
		}
		if i, dup := byID[recID]; dup {
			targets[i].amountC += amt
		} else {
			byID[recID] = len(targets)
			targets = append(targets, target{id: recID, amountC: amt})
		}
		totalC += amt
	}
	// Total allocations on the payment must stay within its amount.
	if totalC > 0 {
		sumRow, err := q.SumPayerPaymentAllocations(ctx, pay.ID)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		existing, err := numericToCents(sumRow)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		if existing+totalC > amountC {
			c.JSON(http.StatusBadRequest, gin.H{"error": "allocations exceed payment"})
			return nil, false
		}
	}
	out := make([]gin.H, 0, len(inputs))
	for _, tg := range targets {
		// Lock the receivable row before the balance check (spec §4.4.1).
		locked, err := q.GetReceivableForUpdate(ctx, tg.id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "receivable not found"})
				return nil, false
			}
			respondInternalError(c, err)
			return nil, false
		}
		if locked.PayerID != pay.PayerID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "receivable does not belong to payer"})
			return nil, false
		}
		amount, err := numericToCents(locked.Amount)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		paid, err := numericToCents(locked.PaidAmount)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		adjusted, err := numericToCents(locked.AdjustedAmount)
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		if tg.amountC > amount-paid-adjusted {
			c.JSON(http.StatusConflict, gin.H{"error": "allocation exceeds receivable balance"})
			return nil, false
		}
		if err := q.InsertPayerPaymentAllocation(ctx, sqlcgen.InsertPayerPaymentAllocationParams{
			CompanyID: locked.CompanyID, MerchantID: locked.MerchantID, PaymentID: pay.ID,
			ReceivableID: locked.ID, Amount: centsToNumeric(tg.amountC), CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		// Status recompute after every allocation (spec §4.1.3).
		if _, err := refreshReceivableStatus(c, q, locked.ID); err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		out = append(out, gin.H{
			"receivable_id": locked.ID.String(),
			"amount":        formatCents(tg.amountC),
		})
	}
	return out, true
}

type payerPaymentCreateRequest struct {
	Amount      string                 `json:"amount" binding:"required"`
	PaidOn      string                 `json:"paid_on" binding:"required"`
	Reference   string                 `json:"reference"`
	Note        string                 `json:"note"`
	BatchID     string                 `json:"batch_id"`
	Allocations []payerAllocationInput `json:"allocations"`
}

// CreatePayerPaymentHandler godoc
// @Summary Record a payment from a payer with optional allocations
// @Tags payer-payment
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param request body payerPaymentCreateRequest true "Payment + allocations"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payers/{id}/payments [post]
func CreatePayerPaymentHandler(c *gin.Context) {
	payerID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req payerPaymentCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount and paid_on are required"})
		return
	}
	amountC, err := parseCents(req.Amount)
	if err != nil || amountC <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	paidOn, err := time.Parse("2006-01-02", req.PaidOn)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid paid_on"})
		return
	}
	var batchID pgtype.UUID
	if req.BatchID != "" {
		if batchID, ok = parseUUID(req.BatchID); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch_id"})
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	payer, err := q.GetPayerByID(c.Request.Context(), payerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, payer.MerchantID) {
		return
	}
	pay, err := q.InsertPayerPayment(c.Request.Context(), sqlcgen.InsertPayerPaymentParams{
		CompanyID: payer.CompanyID, MerchantID: payer.MerchantID, PayerID: payer.ID,
		BatchID: batchID, Amount: centsToNumeric(amountC),
		PaidOn:    pgtype.Date{Time: paidOn, Valid: true},
		Reference: optText(strings.TrimSpace(req.Reference)), Note: optText(strings.TrimSpace(req.Note)),
		RecordedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allocs, ok := applyPaymentAllocations(c, q, pay, req.Allocations)
	if !ok {
		return
	}
	row, err := q.GetPayerPaymentByID(c.Request.Context(), pay.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	sumRow, err := q.SumPayerPaymentAllocations(c.Request.Context(), pay.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allocatedC, err := numericToCents(sumRow)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"payment":     payerPaymentJSON(rowToPaymentView(row), row.PayerID.String(), row.PayerCode, row.PayerName, allocatedC, false),
		"allocations": allocs,
		"unallocated": formatCents(amountC - allocatedC),
	}, "meta": gin.H{}})
}

type payerPaymentAllocateRequest struct {
	Allocations []payerAllocationInput `json:"allocations" binding:"required"`
}

// AddPayerPaymentAllocationsHandler godoc
// @Summary Allocate (part of) an unallocated payment onto receivables
// @Tags payer-payment
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer payment UUID"
// @Param request body payerPaymentAllocateRequest true "Allocations"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payer-payments/{id}/allocations [post]
func AddPayerPaymentAllocationsHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	row, ok := loadPayerPaymentAuthorized(c, q, id)
	if !ok {
		return
	}
	if row.ReversalOf.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "payment is a reversal"})
		return
	}
	var req payerPaymentAllocateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "allocations are required"})
		return
	}
	amountC, err := numericToCents(row.Amount)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allocs, ok := applyPaymentAllocations(c, q, sqlcgen.OperationsPayerPayment{
		ID: row.ID, PayerID: row.PayerID, Amount: row.Amount,
	}, req.Allocations)
	if !ok {
		return
	}
	sumRow, err := q.SumPayerPaymentAllocations(c.Request.Context(), row.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allocatedC, err := numericToCents(sumRow)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"payment":     payerPaymentJSON(rowToPaymentView(row), row.PayerID.String(), row.PayerCode, row.PayerName, allocatedC, false),
		"allocations": allocs,
		"unallocated": formatCents(amountC - allocatedC),
	}, "meta": gin.H{}})
}

type payerPaymentReverseRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// ReversePayerPaymentHandler godoc
// @Summary Reverse a payer payment with a negative payment + negative allocations
// @Tags payer-payment
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer payment UUID"
// @Param request body payerPaymentReverseRequest true "Reversal reason"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /payer-payments/{id}/reverse [post]
func ReversePayerPaymentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	row, ok := loadPayerPaymentAuthorized(c, q, id)
	if !ok {
		return
	}
	var req payerPaymentReverseRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	amountC, err := numericToCents(row.Amount)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Only an original positive payment, not yet reversed, may be reversed
	// (spec §4.4.2; the unique reversal_of index is the concurrency backstop).
	if row.ReversalOf.Valid || amountC <= 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "payment already reversed"})
		return
	}
	if _, err := q.GetPayerPaymentReversalID(ctx, row.ID); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "payment already reversed"})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	reversal, err := q.InsertPayerPaymentReversal(ctx, sqlcgen.InsertPayerPaymentReversalParams{
		CompanyID: row.CompanyID, MerchantID: row.MerchantID, PayerID: row.PayerID,
		Amount: centsToNumeric(-amountC), PaidOn: row.PaidOn,
		Note: optText(strings.TrimSpace(req.Reason)), ReversalOf: row.ID, RecordedBy: AuthUserID(c),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "payment already reversed"})
			return
		}
		respondInternalError(c, err)
		return
	}
	origs, err := q.ListPayerPaymentAllocations(ctx, row.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allocs := make([]gin.H, 0, len(origs))
	seen := map[pgtype.UUID]bool{}
	for _, a := range origs {
		amount, err := numericToCents(a.Amount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if err := q.InsertPayerPaymentAllocation(ctx, sqlcgen.InsertPayerPaymentAllocationParams{
			CompanyID: a.CompanyID, MerchantID: a.MerchantID, PaymentID: reversal.ID,
			ReceivableID: a.ReceivableID, Amount: centsToNumeric(-amount), CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		// Status recompute per affected receivable (spec §4.1.3).
		if !seen[a.ReceivableID] {
			seen[a.ReceivableID] = true
			if _, err := refreshReceivableStatus(c, q, a.ReceivableID); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		allocs = append(allocs, gin.H{
			"receivable_id": a.ReceivableID.String(),
			"amount":        formatCents(-amount),
		})
	}
	revRow, err := q.GetPayerPaymentByID(ctx, reversal.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"payment":     payerPaymentJSON(rowToPaymentView(revRow), revRow.PayerID.String(), revRow.PayerCode, revRow.PayerName, 0, false),
		"allocations": allocs,
		"unallocated": formatCents(0),
	}, "meta": gin.H{}})
}

// ListPayerPaymentsHandler godoc
// @Summary List a payer's payments with allocated/unallocated/reversed flags
// @Tags payer-payment
// @Produce json
// @Security BearerAuth
// @Param id path string true "Payer UUID"
// @Param from query string false "paid_on lower bound (YYYY-MM-DD)"
// @Param to query string false "paid_on upper bound (YYYY-MM-DD)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /payers/{id}/payments [get]
func ListPayerPaymentsHandler(c *gin.Context) {
	payerID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	payer, err := q.GetPayerByID(c.Request.Context(), payerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, payer.MerchantID) {
		return
	}
	params := sqlcgen.ListPayerPaymentsByPayerParams{PayerID: payerID}
	for name, dst := range map[string]*pgtype.Date{"from": &params.DateFrom, "to": &params.DateTo} {
		if s := c.Query(name); s != "" {
			d, err := time.Parse("2006-01-02", s)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
				return
			}
			*dst = pgtype.Date{Time: d, Valid: true}
		}
	}
	rows, err := q.ListPayerPaymentsByPayer(c.Request.Context(), params)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		sumRow, err := q.SumPayerPaymentAllocations(c.Request.Context(), r.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		allocatedC, err := numericToCents(sumRow)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		data = append(data, payerPaymentJSON(listRowToPaymentView(r), r.PayerID.String(), r.PayerCode, r.PayerName, allocatedC, r.Reversed))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
