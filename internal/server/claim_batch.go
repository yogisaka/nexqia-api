// internal/server/claim_batch.go
// Claim batch API (spec 2026-10-07-tariff-b2-receivables-claims §4.2/§4.3):
// period claim batches per payer with the literal document checklist, submit
// gating (documents + expiry), the sequential status flow and the Excel-local
// CSV export.
package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// claimBatchBPJSDocuments is the LITERAL BPJS batch checklist (spec §4.3).
var claimBatchBPJSDocuments = []string{"fpk", "rekap", "kuitansi", "sptjm"}

// batchRequiredDocuments unions the batch checklist: BPJS payers use the
// literal four documents, private payers use payer.claim_documents normalized
// (lowercase, spaces → underscores).
func batchRequiredDocuments(payerType string, claimDocuments []string) []string {
	if payerType == "bpjs" {
		return claimBatchBPJSDocuments
	}
	seen := map[string]bool{}
	out := []string{}
	for _, doc := range claimDocuments {
		key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(doc)), " ", "_")
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// claimBatchJSON renders one batch (snake_case, money as decimal strings).
func claimBatchJSON(b sqlcgen.OperationsClaimBatch, payerCode, payerName string, required []string) gin.H {
	totalC, _ := numericToCents(b.TotalAmount)
	var deadline any
	if b.SubmissionDeadline.Valid {
		deadline = recvDate(b.SubmissionDeadline)
	}
	var note any
	if b.Note.Valid {
		note = b.Note.String
	}
	var documents json.RawMessage
	if len(b.Documents) > 0 {
		documents = json.RawMessage(b.Documents)
	}
	return gin.H{
		"id":                  b.ID.String(),
		"payer":               gin.H{"id": b.PayerID.String(), "code": payerCode, "name": payerName},
		"kind":                b.Kind,
		"period_start":        recvDate(b.PeriodStart),
		"period_end":          recvDate(b.PeriodEnd),
		"status":              b.Status,
		"submission_deadline": deadline,
		"total_amount":        formatCents(totalC),
		"documents":           documents,
		"required_documents":  required,
		"note":                note,
	}
}

// loadClaimBatchAuthorized fetches the joined batch row and checks
// billing.receivable.manage against its merchant.
func loadClaimBatchAuthorized(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.GetClaimBatchDetailRow, bool) {
	batch, err := q.GetClaimBatchDetail(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "claim batch not found"})
			return sqlcgen.GetClaimBatchDetailRow{}, false
		}
		respondInternalError(c, err)
		return sqlcgen.GetClaimBatchDetailRow{}, false
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, batch.MerchantID) {
		return sqlcgen.GetClaimBatchDetailRow{}, false
	}
	return batch, true
}

type claimBatchCreateRequest struct {
	PayerID     string `json:"payer_id" binding:"required"`
	PeriodStart string `json:"period_start" binding:"required"`
	PeriodEnd   string `json:"period_end" binding:"required"`
	Note        string `json:"note"`
}

// CreateClaimBatchHandler godoc
// @Summary Create a claim batch for one payer and period (spec §4.2.1)
// @Tags claim-batch
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body claimBatchCreateRequest true "Batch period"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /merchants/{id}/claim-batches [post]
func CreateClaimBatchHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, merchantID) {
		return
	}
	var req claimBatchCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payer_id, period_start and period_end are required"})
		return
	}
	payerID, ok := parseUUID(req.PayerID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payer_id"})
		return
	}
	start, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period_start"})
		return
	}
	end, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period_end"})
		return
	}
	if end.Before(start) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid period"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	payer, err := q.GetPayerByID(ctx, payerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
			return
		}
		respondInternalError(c, err)
		return
	}
	if payer.MerchantID != merchantID {
		c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
		return
	}
	merchant, err := q.GetMerchantByID(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	kind := "private"
	var deadline pgtype.Date
	if payer.PayerType == "bpjs" {
		if merchant.BpjsCooperation == "fktp_network" {
			kind = "bpjs_network"
		} else {
			kind = "bpjs_noncapitation"
		}
		// BPJS submission deadline: the 10th of the month AFTER period_end.
		y, m, _ := end.Date()
		deadline = pgtype.Date{Time: time.Date(y, m+1, 10, 0, 0, 0, 0, time.UTC), Valid: true}
	}
	batch, err := q.CreateClaimBatch(ctx, sqlcgen.CreateClaimBatchParams{
		CompanyID: payer.CompanyID, MerchantID: merchantID, PayerID: payer.ID,
		Kind: kind, PeriodStart: pgtype.Date{Time: start, Valid: true},
		PeriodEnd: pgtype.Date{Time: end, Valid: true}, Note: optText(req.Note),
		SubmissionDeadline: deadline, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	candidates, err := q.ListBatchCandidates(ctx, sqlcgen.ListBatchCandidatesParams{
		MerchantID: merchantID, PayerID: payer.ID,
		PeriodStart: pgtype.Date{Time: start, Valid: true},
		PeriodEnd:   pgtype.Date{Time: end, Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(candidates) == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "no open receivables in this period"})
		return
	}
	for _, rec := range candidates {
		if err := q.AttachReceivableToBatch(ctx, sqlcgen.AttachReceivableToBatchParams{
			ID: rec.ID, BatchID: batch.ID, UpdatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	batch, err = q.RecomputeClaimBatchTotal(ctx, sqlcgen.RecomputeClaimBatchTotalParams{
		ID: batch.ID, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"data": claimBatchJSON(batch, payer.Code, payer.Name,
			batchRequiredDocuments(payer.PayerType, payer.ClaimDocuments)),
		"meta": gin.H{},
	})
}

// ListClaimBatchesHandler godoc
// @Summary List a merchant's claim batches
// @Tags claim-batch
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param payer_id query string false "Filter by payer UUID"
// @Param status query string false "Filter by status"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/claim-batches [get]
func ListClaimBatchesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermReceivableManage, merchantID) {
		return
	}
	params := sqlcgen.ListClaimBatchesByMerchantParams{MerchantID: merchantID}
	if id, ok := parseUUID(c.Query("payer_id")); ok {
		params.PayerID = id
	}
	if s := c.Query("status"); s != "" {
		params.Status = pgtype.Text{String: s, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListClaimBatchesByMerchant(c.Request.Context(), params)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// The list view omits required_documents (computed per payer in the detail).
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, claimBatchJSON(sqlcgen.OperationsClaimBatch{
			ID: r.ID, CompanyID: r.CompanyID, MerchantID: r.MerchantID, PayerID: r.PayerID,
			Kind: r.Kind, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Status: r.Status,
			SubmittedAt: r.SubmittedAt, SubmittedBy: r.SubmittedBy, SubmissionDeadline: r.SubmissionDeadline,
			Documents: r.Documents, TotalAmount: r.TotalAmount, Note: r.Note,
		}, r.PayerCode, r.PayerName, nil))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetClaimBatchHandler godoc
// @Summary Show one claim batch with its receivables
// @Tags claim-batch
// @Produce json
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /claim-batches/{id} [get]
func GetClaimBatchHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	rows, err := q.ListBatchReceivables(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := claimBatchJSON(sqlcgen.OperationsClaimBatch{
		ID: batch.ID, CompanyID: batch.CompanyID, MerchantID: batch.MerchantID, PayerID: batch.PayerID,
		Kind: batch.Kind, PeriodStart: batch.PeriodStart, PeriodEnd: batch.PeriodEnd, Status: batch.Status,
		SubmittedAt: batch.SubmittedAt, SubmittedBy: batch.SubmittedBy, SubmissionDeadline: batch.SubmissionDeadline,
		Documents: batch.Documents, TotalAmount: batch.TotalAmount, Note: batch.Note,
	}, batch.PayerCode, batch.PayerName, batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments))
	data["receivables"] = batchReceivablesJSON(rows)
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// batchReceivablesJSON renders the joined batch receivable rows.
func batchReceivablesJSON(rows []sqlcgen.ListBatchReceivablesRow) []gin.H {
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, receivableJSON(receivableJoined{
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
	return out
}

type claimBatchEditRequest struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// EditClaimBatchReceivablesHandler godoc
// @Summary Add/remove receivables of a draft claim batch (spec §4.2.2)
// @Tags claim-batch
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Param request body claimBatchEditRequest true "Receivable ids to add/remove"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /claim-batches/{id}/receivables [put]
func EditClaimBatchReceivablesHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	if batch.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "batch is not editable"})
		return
	}
	var req claimBatchEditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "add and remove arrays are required"})
		return
	}
	ctx := c.Request.Context()
	// Adds: the receivable must be this payer's open receivable.
	for _, raw := range req.Add {
		recID, okID := parseUUID(raw)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid receivable id"})
			return
		}
		recs, err := q.ListReceivablesByIDs(ctx, []pgtype.UUID{recID})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if len(recs) != 1 || recs[0].MerchantID != batch.MerchantID ||
			recs[0].PayerID != batch.PayerID || recs[0].Status != "open" {
			c.JSON(http.StatusConflict, gin.H{"error": "receivable not open"})
			return
		}
		if err := q.AttachReceivableToBatch(ctx, sqlcgen.AttachReceivableToBatchParams{
			ID: recID, BatchID: batch.ID, UpdatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	// Removes: the receivable must currently sit in THIS batch; it returns to
	// open with a NULL batch_id.
	for _, raw := range req.Remove {
		recID, okID := parseUUID(raw)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid receivable id"})
			return
		}
		recs, err := q.ListReceivablesByIDs(ctx, []pgtype.UUID{recID})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if len(recs) != 1 || recs[0].BatchID != batch.ID {
			c.JSON(http.StatusConflict, gin.H{"error": "receivable not in batch"})
			return
		}
		if err := q.DetachReceivableFromBatch(ctx, sqlcgen.DetachReceivableFromBatchParams{
			ID: recID, UpdatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		if _, err := refreshReceivableStatus(c, q, recID); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	updated, err := q.RecomputeClaimBatchTotal(ctx, sqlcgen.RecomputeClaimBatchTotalParams{
		ID: batch.ID, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": claimBatchJSON(updated, batch.PayerCode, batch.PayerName,
			batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments)),
		"meta": gin.H{},
	})
}

// PatchClaimBatchDocumentsHandler godoc
// @Summary Toggle the document checklist of a claim batch (spec §4.2.3/§4.3)
// @Tags claim-batch
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Param request body receivableDocumentsRequest true "Document keys to set/unset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /claim-batches/{id}/documents [patch]
func PatchClaimBatchDocumentsHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	var req receivableDocumentsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "documents object is required"})
		return
	}
	allowed := map[string]bool{}
	for _, doc := range batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments) {
		allowed[doc] = true
	}
	for key := range req.Documents {
		if !allowed[key] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown document " + key})
			return
		}
	}
	docs := map[string]bool{}
	if len(batch.Documents) > 0 {
		_ = json.Unmarshal(batch.Documents, &docs)
	}
	for key, set := range req.Documents {
		if set {
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
	updated, err := q.UpdateClaimBatchDocuments(c.Request.Context(), sqlcgen.UpdateClaimBatchDocumentsParams{
		ID: batch.ID, Documents: blob, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": claimBatchJSON(updated, batch.PayerCode, batch.PayerName,
			batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments)),
		"meta": gin.H{},
	})
}

// batchMissingDocuments collects the unmet checklist entries: key "batch" for
// the batch checklist, each receivable id for its own checklist.
func batchMissingDocuments(docs map[string]bool, batchDocs []string, rows []sqlcgen.ListBatchReceivablesRow) map[string][]string {
	missing := map[string][]string{}
	for _, doc := range batchDocs {
		if !docs[doc] {
			missing["batch"] = append(missing["batch"], doc)
		}
	}
	for _, r := range rows {
		rdocs := map[string]bool{}
		if len(r.Documents) > 0 {
			_ = json.Unmarshal(r.Documents, &rdocs)
		}
		for _, doc := range requiredDocumentsFor(r.PayerType, r.ClaimTypes) {
			if !rdocs[doc] {
				missing[r.ID.String()] = append(missing[r.ID.String()], doc)
			}
		}
	}
	return missing
}

// SubmitClaimBatchHandler godoc
// @Summary Submit a draft claim batch (spec §4.2.4)
// @Tags claim-batch
// @Produce json
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /claim-batches/{id}/submit [post]
func SubmitClaimBatchHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	if batch.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "batch is not draft"})
		return
	}
	ctx := c.Request.Context()
	rows, err := q.ListBatchReceivables(ctx, batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	docs := map[string]bool{}
	if len(batch.Documents) > 0 {
		_ = json.Unmarshal(batch.Documents, &docs)
	}
	// Every batch document AND every per-receivable document must be ticked.
	if missing := batchMissingDocuments(docs,
		batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments), rows); len(missing) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "documents_incomplete", "missing": missing})
		return
	}
	// Expired receivables (expires_on < today in the merchant timezone) block.
	loc, err := merchantLocation(ctx, q, batch.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	today := localDayStart(time.Now(), loc).Format("2006-01-02")
	var expired []string
	for _, r := range rows {
		if r.ExpiresOn.Valid && r.ExpiresOn.Time.Format("2006-01-02") < today {
			expired = append(expired, r.ID.String())
		}
	}
	if len(expired) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "expired_receivables", "receivable_ids": expired})
		return
	}
	updated, err := q.SubmitClaimBatch(ctx, sqlcgen.SubmitClaimBatchParams{
		ID: batch.ID, SubmittedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.PromoteClaimBatchReceivables(ctx, sqlcgen.PromoteClaimBatchReceivablesParams{
		BatchID: batch.ID, Status: "submitted", UpdatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": claimBatchJSON(updated, batch.PayerCode, batch.PayerName,
			batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments)),
		"meta": gin.H{},
	})
}

// claimBatchNextStatus maps the target status to the required current one
// (spec §4.2.5: strictly forward).
var claimBatchNextStatus = map[string]string{
	"verified": "submitted",
	"paid":     "verified",
	"closed":   "paid",
}

type claimBatchStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// SetClaimBatchStatusHandler godoc
// @Summary Advance a claim batch status (verified → paid → closed)
// @Tags claim-batch
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Param request body claimBatchStatusRequest true "Target status"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /claim-batches/{id}/status [post]
func SetClaimBatchStatusHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	var req claimBatchStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil || claimBatchNextStatus[req.Status] == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if batch.Status != claimBatchNextStatus[req.Status] {
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition"})
		return
	}
	// Closed requires every receivable to be paid or settled.
	if req.Status == "closed" {
		rows, err := q.ListBatchReceivables(c.Request.Context(), batch.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for _, r := range rows {
			if r.Status != "paid" && r.Status != "settled" {
				c.JSON(http.StatusConflict, gin.H{"error": "batch has unpaid receivables"})
				return
			}
		}
	}
	updated, err := q.SetClaimBatchStatus(c.Request.Context(), sqlcgen.SetClaimBatchStatusParams{
		ID: batch.ID, Status: req.Status, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data": claimBatchJSON(updated, batch.PayerCode, batch.PayerName,
			batchRequiredDocuments(batch.PayerType, batch.PayerClaimDocuments)),
		"meta": gin.H{},
	})
}

// ExportClaimBatchCSVHandler godoc
// @Summary Export a claim batch as Excel-local CSV (spec §4.2.6)
// @Tags claim-batch
// @Produce text/csv
// @Security BearerAuth
// @Param id path string true "Claim batch UUID"
// @Success 200 {string} string
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /claim-batches/{id}/export.csv [get]
func ExportClaimBatchCSVHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	batch, ok := loadClaimBatchAuthorized(c, q, id)
	if !ok {
		return
	}
	rows, err := q.ClaimBatchExportRows(c.Request.Context(), batch.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(
		`attachment; filename="klaim-%s-%s-%s.csv"`,
		batch.PayerCode, recvDate(batch.PeriodStart), recvDate(batch.PeriodEnd)))
	w := csv.NewWriter(c.Writer)
	w.Comma = ';'
	if batch.Kind == "bpjs_network" {
		merchant, err := q.GetMerchantByID(c.Request.Context(), batch.MerchantID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		_ = w.Write([]string{"FKTP induk", merchant.BpjsNetworkParent.String})
	}
	_ = w.Write([]string{
		"no", "nomor_peserta", "nama_pasien", "no_rm", "no_kunjungan",
		"tanggal_layanan", "jenis_klaim", "kode_layanan", "nama_layanan",
		"jumlah", "nominal",
	})
	total := int64(0)
	for i, r := range rows {
		cents, err := numericToCents(r.Amount)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		total += cents
		_ = w.Write([]string{
			fmt.Sprint(i + 1),
			r.MemberNumber.String,
			r.PersonName,
			r.PersonMrn.String,
			r.VisitNo,
			recvDate(r.ServiceDate),
			r.ClaimType.String,
			r.ItemCode,
			r.ItemName,
			r.QuantityText,
			formatCents(cents),
		})
	}
	_ = w.Write([]string{"TOTAL", "", "", "", "", "", "", "", "", "", formatCents(total)})
	w.Flush()
}
