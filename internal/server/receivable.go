// internal/server/receivable.go
// Payer receivables API (spec 2026-10-07-tariff-b2-receivables-claims
// §4.1/§4.3/§4.4.3–4/§4.5): merchant-level list/aging/expiring reports plus the
// per-receivable dispute flag, document checklist and balance adjustments.
// Creation happens inside finalize and the reopen guard lives in
// billing_account.go; both reuse the helpers here.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermReceivableManage guards every receivable endpoint (Task 2–4).
const PermReceivableManage = "billing.receivable.manage"

// receivableRequiredDocuments is the LITERAL per-receivable BPJS document
// checklist (spec §4.3), keyed by claim type; private payers have none.
var receivableRequiredDocuments = map[string][]string{
	"anc":      {"buku_kia"},
	"pnc":      {"buku_kia"},
	"delivery": {"buku_kia", "partograf", "surat_keterangan_lahir"},
	"kb":       {"buku_kb", "persetujuan_kb"},
}

// requiredDocumentsFor unions the literal checklists of a receivable's claim
// types (BPJS payers only; first occurrence keeps position, no duplicates).
func requiredDocumentsFor(payerType string, claimTypes []string) []string {
	if payerType != "bpjs" {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, ct := range claimTypes {
		for _, doc := range receivableRequiredDocuments[ct] {
			if !seen[doc] {
				seen[doc] = true
				out = append(out, doc)
			}
		}
	}
	return out
}

// RegisterReceivableRoutes wires the receivable endpoints (spec §4.5, §4.4.3–4).
func RegisterReceivableRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/receivables", AccessLog("receivable", "list", "id"), ListReceivablesHandler)
	rg.GET("/merchants/:id/receivables/aging", AccessLog("receivable", "aging", "id"), ReceivableAgingHandler)
	rg.GET("/merchants/:id/receivables/expiring", AccessLog("receivable", "expiring", "id"), ListExpiringReceivablesHandler)
	rg.PATCH("/receivables/:id", PatchReceivableHandler)
	rg.PATCH("/receivables/:id/documents", PatchReceivableDocumentsHandler)
	rg.POST("/receivables/:id/adjustments", CreateReceivableAdjustmentHandler)
}

// recvDate renders a pgtype.Date as an ISO string ("" when NULL).
func recvDate(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

// receivableJoined carries one receivable row plus the payer/admission/person
// columns joined for display; every list-shaped query returns exactly these.
type receivableJoined struct {
	rec        sqlcgen.OperationsReceivable
	payerName  string
	payerType  string
	visitNo    string
	personID   pgtype.UUID
	personName string
	personMRN  pgtype.Text
}

// joinedView re-couples an updated receivable row with its display join.
func joinedView(rec sqlcgen.OperationsReceivable, row sqlcgen.GetReceivableByIDRow) receivableJoined {
	return receivableJoined{
		rec: rec, payerName: row.PayerName, payerType: row.PayerType,
		visitNo: row.AdmissionVisitNo, personID: row.PersonID,
		personName: row.PersonFullName, personMRN: row.PersonMrn,
	}
}

// receivableJSON renders one receivable (snake_case, money as decimal strings).
func receivableJSON(v receivableJoined) gin.H {
	amountC, _ := numericToCents(v.rec.Amount)
	paidC, _ := numericToCents(v.rec.PaidAmount)
	adjC, _ := numericToCents(v.rec.AdjustedAmount)
	var mrn any
	if v.personMRN.Valid {
		mrn = v.personMRN.String
	}
	var expiresOn, batchID any
	if v.rec.ExpiresOn.Valid {
		expiresOn = recvDate(v.rec.ExpiresOn)
	}
	if v.rec.BatchID.Valid {
		batchID = v.rec.BatchID.String()
	}
	var documents json.RawMessage
	if len(v.rec.Documents) > 0 {
		documents = json.RawMessage(v.rec.Documents)
	}
	return gin.H{
		"id":    v.rec.ID.String(),
		"payer": gin.H{"id": v.rec.PayerID.String(), "name": v.payerName},
		"person": gin.H{
			"id":                v.personID.String(),
			"full_name":         v.personName,
			"medical_record_no": mrn,
		},
		"admission":          gin.H{"id": v.rec.AdmissionID.String(), "visit_no": v.visitNo},
		"service_date_from":  recvDate(v.rec.ServiceDateFrom),
		"service_date_to":    recvDate(v.rec.ServiceDateTo),
		"amount":             numStr2(v.rec.Amount),
		"paid_amount":        numStr2(v.rec.PaidAmount),
		"adjusted_amount":    numStr2(v.rec.AdjustedAmount),
		"balance":            formatCents(amountC - paidC - adjC),
		"status":             v.rec.Status,
		"due_date":           recvDate(v.rec.DueDate),
		"expires_on":         expiresOn,
		"batch_id":           batchID,
		"claim_types":        v.rec.ClaimTypes,
		"documents":          documents,
		"required_documents": requiredDocumentsFor(v.payerType, v.rec.ClaimTypes),
	}
}

// refreshReceivableStatus recomputes one receivable's paid/adjusted totals and
// status (spec §4.1.3): paid (paid = amount), settled (paid + adjusted =
// amount, adjusted > 0), partially_paid (0 < paid + adjusted < amount), else
// the claim batch status (in_batch/submitted) or open. A disputed receivable
// (dispute_reason filled, chk_receivable_dispute) stays disputed — only the
// amounts are refreshed. Used by Task 2–4 after every allocation/adjustment.
func refreshReceivableStatus(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.OperationsReceivable, error) {
	ctx := c.Request.Context()
	rec, err := q.GetReceivableForUpdate(ctx, id)
	if err != nil {
		return rec, err
	}
	paidRow, err := q.SumReceivableAllocations(ctx, id)
	if err != nil {
		return rec, err
	}
	adjRow, err := q.SumReceivableAdjustments(ctx, id)
	if err != nil {
		return rec, err
	}
	paid, err := numericToCents(paidRow)
	if err != nil {
		return rec, err
	}
	adjusted, err := numericToCents(adjRow)
	if err != nil {
		return rec, err
	}
	amount, err := numericToCents(rec.Amount)
	if err != nil {
		return rec, err
	}
	status := "open"
	switch {
	case paid == amount:
		status = "paid"
	case adjusted > 0 && paid+adjusted == amount:
		status = "settled"
	case paid+adjusted > 0:
		status = "partially_paid"
	case rec.BatchID.Valid:
		// No money moved yet: the claim batch carries the state (draft batch →
		// in_batch; submitted or beyond → submitted).
		batch, err := q.GetClaimBatchByID(ctx, rec.BatchID)
		if err != nil {
			return rec, err
		}
		if batch.Status == "draft" {
			status = "in_batch"
		} else {
			status = "submitted"
		}
	}
	if strings.TrimSpace(rec.DisputeReason.String) != "" {
		status = "disputed"
	}
	return q.UpdateReceivableTotalsAndStatus(ctx, sqlcgen.UpdateReceivableTotalsAndStatusParams{
		ID: id, PaidAmount: centsToNumeric(paid), AdjustedAmount: centsToNumeric(adjusted),
		Status: status, UpdatedBy: AuthUserID(c),
	})
}

// loadReceivableAuthorized fetches the joined receivable row and checks
// billing.receivable.manage against its merchant; writes the response itself.
func loadReceivableAuthorized(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.GetReceivableByIDRow, bool) {
	rec, err := q.GetReceivableByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "receivable not found"})
			return sqlcgen.GetReceivableByIDRow{}, false
		}
		respondInternalError(c, err)
		return sqlcgen.GetReceivableByIDRow{}, false
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, rec.MerchantID) {
		return sqlcgen.GetReceivableByIDRow{}, false
	}
	return rec, true
}

// ListReceivablesHandler godoc
// @Summary List a merchant's payer receivables
// @Tags receivable
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param payer_id query string false "Filter by payer UUID"
// @Param status query string false "Filter by status"
// @Param from query string false "Service date-to lower bound (YYYY-MM-DD)"
// @Param to query string false "Service date-to upper bound (YYYY-MM-DD)"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/receivables [get]
func ListReceivablesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	params := sqlcgen.ListReceivablesByMerchantParams{
		MerchantID: merchantID, RowLimit: limit, RowOffset: offset,
	}
	if id, ok := parseUUID(c.Query("payer_id")); ok {
		params.PayerID = id
	}
	if s := c.Query("status"); s != "" {
		params.Status = pgtype.Text{String: s, Valid: true}
	}
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
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListReceivablesByMerchant(c.Request.Context(), params)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, receivableJSON(receivableJoined{
			rec: sqlcgen.OperationsReceivable{
				ID: r.ID, CompanyID: r.CompanyID, MerchantID: r.MerchantID,
				AccountID: r.AccountID, AdmissionID: r.AdmissionID, PayerID: r.PayerID,
				ServiceDateFrom: r.ServiceDateFrom, ServiceDateTo: r.ServiceDateTo,
				Amount: r.Amount, ClaimTypes: r.ClaimTypes, PaidAmount: r.PaidAmount,
				AdjustedAmount: r.AdjustedAmount, Status: r.Status, DisputeReason: r.DisputeReason,
				DueDate: r.DueDate, ExpiresOn: r.ExpiresOn, BatchID: r.BatchID, Documents: r.Documents,
			},
			payerName: r.PayerName, payerType: r.PayerType, visitNo: r.AdmissionVisitNo,
			personID: r.PersonID, personName: r.PersonFullName, personMRN: r.PersonMrn,
		}))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// ReceivableAgingHandler godoc
// @Summary AR aging per payer (0-30/31-60/61-90/over-90 + disputed)
// @Tags receivable
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param as_of query string false "Aging date (YYYY-MM-DD, default today in the merchant timezone)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/receivables/aging [get]
func ReceivableAgingHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	asOf := pgtype.Date{Time: time.Now(), Valid: true}
	if loc, err := merchantLocation(c.Request.Context(), q, merchantID); err == nil {
		asOf.Time = localDayStart(time.Now(), loc)
	}
	if s := c.Query("as_of"); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid as_of"})
			return
		}
		asOf = pgtype.Date{Time: d, Valid: true}
	}
	rows, err := q.ReceivableAgingByMerchant(c.Request.Context(), sqlcgen.ReceivableAgingByMerchantParams{
		MerchantID: merchantID, AsOf: asOf,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"payer_id": r.PayerID.String(), "name": r.PayerName,
			"d0_30": numStr2(r.D030), "d31_60": numStr2(r.D3160),
			"d61_90": numStr2(r.D6190), "over_90": numStr2(r.Over90),
			"disputed": numStr2(r.Disputed),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// ListExpiringReceivablesHandler godoc
// @Summary BPJS receivables expiring within N days (default 30, 1-180)
// @Tags receivable
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param days query string false "Look-ahead days (1-180)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/receivables/expiring [get]
func ListExpiringReceivablesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, merchantID) {
		return
	}
	days := 30
	if s := c.Query("days"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 || v > 180 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid days"})
			return
		}
		days = v
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	today := localDayStart(time.Now(), loc)
	rows, err := q.ListExpiringReceivables(ctx, sqlcgen.ListExpiringReceivablesParams{
		MerchantID:    merchantID,
		ExpiresBefore: pgtype.Date{Time: today.AddDate(0, 0, days), Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, receivableJSON(receivableJoined{
			rec: sqlcgen.OperationsReceivable{
				ID: r.ID, CompanyID: r.CompanyID, MerchantID: r.MerchantID,
				AccountID: r.AccountID, AdmissionID: r.AdmissionID, PayerID: r.PayerID,
				ServiceDateFrom: r.ServiceDateFrom, ServiceDateTo: r.ServiceDateTo,
				Amount: r.Amount, ClaimTypes: r.ClaimTypes, PaidAmount: r.PaidAmount,
				AdjustedAmount: r.AdjustedAmount, Status: r.Status, DisputeReason: r.DisputeReason,
				DueDate: r.DueDate, ExpiresOn: r.ExpiresOn, BatchID: r.BatchID, Documents: r.Documents,
			},
			payerName: r.PayerName, payerType: r.PayerType, visitNo: r.AdmissionVisitNo,
			personID: r.PersonID, personName: r.PersonFullName, personMRN: r.PersonMrn,
		}))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type receivableDisputeRequest struct {
	Disputed bool   `json:"disputed"`
	Reason   string `json:"reason"`
}

// PatchReceivableHandler godoc
// @Summary Set or clear the manual dispute flag of a receivable
// @Tags receivable
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receivable UUID"
// @Param request body receivableDisputeRequest true "Dispute flag + reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /receivables/{id} [patch]
func PatchReceivableHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	joined, ok := loadReceivableAuthorized(c, q, id)
	if !ok {
		return
	}
	var req receivableDisputeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disputed (bool) is required"})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if req.Disputed && reason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dispute needs a reason"})
		return
	}
	if _, err := q.SetReceivableDispute(c.Request.Context(), sqlcgen.SetReceivableDisputeParams{
		ID: joined.ID, DisputeReason: optText(reason), UpdatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	// Mandatory recompute: chk_receivable_dispute ties the status to the
	// reason (clearing it must land on a computed status, never stay disputed).
	updated, err := refreshReceivableStatus(c, q, joined.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": receivableJSON(joinedView(updated, joined)), "meta": gin.H{}})
}

type receivableDocumentsRequest struct {
	Documents map[string]bool `json:"documents" binding:"required"`
}

// PatchReceivableDocumentsHandler godoc
// @Summary Toggle the document checklist of a receivable (spec §4.4.4/§4.3)
// @Tags receivable
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receivable UUID"
// @Param request body receivableDocumentsRequest true "Document keys to set/unset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /receivables/{id}/documents [patch]
func PatchReceivableDocumentsHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	joined, ok := loadReceivableAuthorized(c, q, id)
	if !ok {
		return
	}
	var req receivableDocumentsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "documents object is required"})
		return
	}
	allowed := map[string]bool{}
	for _, doc := range requiredDocumentsFor(joined.PayerType, joined.ClaimTypes) {
		allowed[doc] = true
	}
	keys := make([]string, 0, len(req.Documents))
	for key := range req.Documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !allowed[key] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown document " + key})
			return
		}
	}
	docs := map[string]bool{}
	if len(joined.Documents) > 0 {
		_ = json.Unmarshal(joined.Documents, &docs)
	}
	for _, key := range keys {
		if req.Documents[key] {
			docs[key] = true
		} else {
			delete(docs, key)
		}
	}
	blob, err := json.Marshal(docs)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	updated, err := q.UpdateReceivableDocuments(c.Request.Context(), sqlcgen.UpdateReceivableDocumentsParams{
		ID: joined.ID, Documents: blob, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": receivableJSON(joinedView(updated, joined)), "meta": gin.H{}})
}

type receivableAdjustmentRequest struct {
	Kind   string `json:"kind" binding:"required"`
	Amount string `json:"amount" binding:"required"`
	Reason string `json:"reason" binding:"required"`
}

// CreateReceivableAdjustmentHandler godoc
// @Summary Record a balance adjustment (to_patient / write_off)
// @Tags receivable
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Receivable UUID"
// @Param request body receivableAdjustmentRequest true "Adjustment"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /receivables/{id}/adjustments [post]
func CreateReceivableAdjustmentHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	joined, ok := loadReceivableAuthorized(c, q, id)
	if !ok {
		return
	}
	var req receivableAdjustmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind, amount and reason are required"})
		return
	}
	if req.Kind != "to_patient" && req.Kind != "write_off" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid kind"})
		return
	}
	amount, err := parseCents(req.Amount)
	if err != nil || amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	locked, err := q.GetReceivableForUpdate(c.Request.Context(), joined.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	amountC, _ := numericToCents(locked.Amount)
	paidC, _ := numericToCents(locked.PaidAmount)
	adjC, _ := numericToCents(locked.AdjustedAmount)
	if amount > amountC-paidC-adjC {
		c.JSON(http.StatusConflict, gin.H{"error": "adjustment exceeds receivable balance"})
		return
	}
	if err := q.InsertReceivableAdjustment(c.Request.Context(), sqlcgen.InsertReceivableAdjustmentParams{
		CompanyID: locked.CompanyID, MerchantID: locked.MerchantID, ReceivableID: locked.ID,
		Kind: req.Kind, Amount: centsToNumeric(amount), Reason: req.Reason, CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	updated, err := refreshReceivableStatus(c, q, locked.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": receivableJSON(joinedView(updated, joined)), "meta": gin.H{}})
}
