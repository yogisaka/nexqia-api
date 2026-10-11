//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClaimBatch_* covers the claim batch API (spec
// 2026-10-07-tariff-b2-receivables-claims §4.2/§4.3): batch creation with the
// BPJS deadline, the bpjs_network kind + FKTP induk CSV row, draft-only
// editing, the document-gated submit, the expiry guard, the sequential status
// flow and the Excel-local CSV export.

// cbCreateBatch creates a claim batch for the payer/period and asserts 201.
func cbCreateBatch(t *testing.T, env *qeEnv, payerID string, start, end time.Time) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost,
		"/api/v1/merchants/"+env.merchantID+"/claim-batches",
		map[string]any{
			"payer_id":     payerID,
			"period_start": start.Format("2006-01-02"),
			"period_end":   end.Format("2006-01-02"),
		})
	return payMapData(t, status, out, http.StatusCreated)
}

// cbGetBatch fetches GET /claim-batches/:id data.
func cbGetBatch(t *testing.T, env *qeEnv, batchID string) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/claim-batches/"+batchID, nil)
	return payMapData(t, status, out, http.StatusOK)
}

// cbSubmit posts the submit endpoint and returns the raw status/body.
func cbSubmit(t *testing.T, env *qeEnv, batchID string) (int, map[string]any) {
	t.Helper()
	return qeRequest(t, env, http.MethodPost, "/api/v1/claim-batches/"+batchID+"/submit", nil)
}

// cbTickBatchDocs ticks the given batch document keys.
func cbTickBatchDocs(t *testing.T, env *qeEnv, batchID string, docs map[string]any) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPatch,
		"/api/v1/claim-batches/"+batchID+"/documents", map[string]any{"documents": docs})
	return payMapData(t, status, out, http.StatusOK)
}

// cbOnlyReceivable returns the single receivable of a batch (by admission id).
func cbOnlyReceivable(t *testing.T, env *qeEnv, batchID, admissionID string) string {
	t.Helper()
	recs, _ := cbGetBatch(t, env, batchID)["receivables"].([]any)
	for _, r := range recs {
		row, _ := r.(map[string]any)
		adm, _ := row["admission"].(map[string]any)
		if adm["id"] == admissionID {
			id, _ := row["id"].(string)
			return id
		}
	}
	t.Fatalf("no receivable for admission %s in batch %s: %v", admissionID, batchID, recs)
	return ""
}

// cbRaw performs an authenticated GET and returns status/headers/raw body.
func cbRaw(t *testing.T, env *qeEnv, path string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("X-Company-ID", env.companyID)
	req.Header.Set("X-Merchant-ID", env.merchantID)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// TestClaimBatch_CreateBPJS — a BPJS batch is a draft with the deadline on the
// 10th of the month after period_end, holding exactly the open receivables of
// that period (which flip to in_batch); an empty period answers 409.
func TestClaimBatch_CreateBPJS(t *testing.T) {
	env := payNewEnv(t, "cb.create")
	payerID, itemID := rcClaimBPJS(t, env, "CB1", "other")
	admA := rcNewClaimAdmission(t, env, payerID, itemID, "CB1A")
	admB := rcNewClaimAdmission(t, env, payerID, itemID, "CB1B")
	today := rcToday()

	data := cbCreateBatch(t, env, payerID, today, today)
	if data["kind"] != "bpjs_noncapitation" || data["status"] != "draft" {
		t.Fatalf("batch = %v", data)
	}
	// Deadline: the 10th of the NEXT month after period_end.
	wantDeadline := time.Date(today.Year(), today.Month()+1, 10, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	if data["submission_deadline"] != wantDeadline {
		t.Fatalf("submission_deadline = %v, want %v", data["submission_deadline"], wantDeadline)
	}
	if data["total_amount"] != "20000.00" {
		t.Fatalf("total_amount = %v, want 20000.00", data["total_amount"])
	}
	batchID, _ := data["id"].(string)
	detail := cbGetBatch(t, env, batchID)
	recs, _ := detail["receivables"].([]any)
	if len(recs) != 2 {
		t.Fatalf("batch receivables = %d, want 2: %v", len(recs), detail)
	}
	for _, r := range recs {
		row, _ := r.(map[string]any)
		if row["status"] != "in_batch" {
			t.Fatalf("receivable status = %v, want in_batch", row["status"])
		}
	}
	// The batch checklist is the literal BPJS four.
	req, _ := json.Marshal(detail["required_documents"])
	if string(req) != `["fpk","rekap","kuitansi","sptjm"]` {
		t.Fatalf("required_documents = %s", req)
	}

	// A period without open receivables is rejected and leaves no batch.
	prev := today.AddDate(0, -1, 0)
	status, out := qeRequest(t, env, http.MethodPost,
		"/api/v1/merchants/"+env.merchantID+"/claim-batches",
		map[string]any{
			"payer_id":     payerID,
			"period_start": prev.Format("2006-01-02"),
			"period_end":   prev.Format("2006-01-02"),
		})
	if status != http.StatusConflict || out["error"] != "no open receivables in this period" {
		t.Fatalf("empty period = %d %v", status, out)
	}
	if rows := rcList(t, env, "?payer_id="+payerID); len(rows) != 2 {
		t.Fatalf("receivables = %d, want 2 still in batch", len(rows))
	}
	_ = admA
	_ = admB
}

// TestClaimBatch_NetworkKind — an fktp_network merchant yields kind
// bpjs_network and the CSV opens with `FKTP induk;<bpjs_network_parent>`.
func TestClaimBatch_NetworkKind(t *testing.T) {
	env := payNewEnv(t, "cb.net")
	payerID, itemID := rcClaimBPJS(t, env, "CBN", "other")
	status, out := qeRequest(t, env, http.MethodPatch,
		"/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_network", "bpjs_network_parent": "FKTP Induk"})
	payMapData(t, status, out, http.StatusOK)
	rcNewClaimAdmission(t, env, payerID, itemID, "CBNA")
	today := rcToday()

	data := cbCreateBatch(t, env, payerID, today, today)
	if data["kind"] != "bpjs_network" {
		t.Fatalf("kind = %v, want bpjs_network", data["kind"])
	}
	batchID, _ := data["id"].(string)
	_, _, body := cbRaw(t, env, "/api/v1/claim-batches/"+batchID+"/export.csv")
	lines := strings.Split(strings.TrimRight(body, "\r\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("csv too short: %q", body)
	}
	if lines[0] != "FKTP induk;FKTP Induk" {
		t.Fatalf("first csv line = %q, want FKTP induk;FKTP Induk", lines[0])
	}
	wantHeader := "no;nomor_peserta;nama_pasien;no_rm;no_kunjungan;tanggal_layanan;jenis_klaim;kode_layanan;nama_layanan;jumlah;nominal"
	if lines[1] != wantHeader {
		t.Fatalf("csv header = %q", lines[1])
	}
}

// TestClaimBatch_EditDraftOnly — add/remove works on a draft only; removal
// returns the receivable to open with batch_id NULL and the total recomputes.
func TestClaimBatch_EditDraftOnly(t *testing.T) {
	env := payNewEnv(t, "cb.edit")
	payerID, itemID := rcClaimBPJS(t, env, "CBE", "other")
	rcNewClaimAdmission(t, env, payerID, itemID, "CBEA")
	admB := rcNewClaimAdmission(t, env, payerID, itemID, "CBEB")
	today := rcToday()
	data := cbCreateBatch(t, env, payerID, today, today)
	batchID, _ := data["id"].(string)
	recB := cbOnlyReceivable(t, env, batchID, admB)

	status, out := qeRequest(t, env, http.MethodPut,
		"/api/v1/claim-batches/"+batchID+"/receivables",
		map[string]any{"add": []string{}, "remove": []string{recB}})
	data = payMapData(t, status, out, http.StatusOK)
	if data["total_amount"] != "10000.00" {
		t.Fatalf("total after remove = %v, want 10000.00", data["total_amount"])
	}
	ctx := context.Background()
	var st string
	var batch any
	if err := env.pool.QueryRow(ctx,
		"SELECT status, batch_id FROM operations.receivable WHERE id = $1", recB).Scan(&st, &batch); err != nil {
		t.Fatalf("read receivable: %v", err)
	}
	if st != "open" || batch != nil {
		t.Fatalf("removed receivable = (%s, %v), want (open, NULL)", st, batch)
	}

	status, out = qeRequest(t, env, http.MethodPut,
		"/api/v1/claim-batches/"+batchID+"/receivables",
		map[string]any{"add": []string{recB}, "remove": []string{}})
	data = payMapData(t, status, out, http.StatusOK)
	if data["total_amount"] != "20000.00" || data["status"] != "draft" {
		t.Fatalf("after re-add = %v", data)
	}

	// Beyond draft the content is frozen.
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.claim_batch SET status = 'verified', submitted_at = now() WHERE id = $1",
		batchID); err != nil {
		t.Fatalf("force verified: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPut,
		"/api/v1/claim-batches/"+batchID+"/receivables",
		map[string]any{"add": []string{}, "remove": []string{recB}})
	if status != http.StatusConflict || out["error"] != "batch is not editable" {
		t.Fatalf("edit non-draft = %d %v", status, out)
	}
}

// TestClaimBatch_SubmitNeedsDocuments — submit is blocked until every batch
// document AND every per-receivable document is ticked, then promotes the
// batch and its receivables to submitted.
func TestClaimBatch_SubmitNeedsDocuments(t *testing.T) {
	env := payNewEnv(t, "cb.submit")
	payerID, itemID := rcClaimBPJS(t, env, "CBD", "anc")
	admID := rcNewClaimAdmission(t, env, payerID, itemID, "CBDA")
	today := rcToday()
	data := cbCreateBatch(t, env, payerID, today, today)
	batchID, _ := data["id"].(string)
	recID := cbOnlyReceivable(t, env, batchID, admID)

	status, out := cbSubmit(t, env, batchID)
	if status != http.StatusConflict || out["error"] != "documents_incomplete" {
		t.Fatalf("incomplete submit = %d %v", status, out)
	}
	missing, _ := out["missing"].(map[string]any)
	if missing == nil {
		t.Fatalf("missing missing map: %v", out)
	}
	batchMiss, _ := missing["batch"].([]any)
	if len(batchMiss) != 4 {
		t.Fatalf("batch missing = %v, want 4 documents", missing["batch"])
	}
	recMiss, _ := missing[recID].([]any)
	if len(recMiss) != 1 || recMiss[0] != "buku_kia" {
		t.Fatalf("receivable missing = %v, want [buku_kia]", missing[recID])
	}

	// Batch documents alone are not enough.
	cbTickBatchDocs(t, env, batchID, map[string]any{
		"fpk": true, "rekap": true, "kuitansi": true, "sptjm": true,
	})
	status, out = cbSubmit(t, env, batchID)
	if status != http.StatusConflict || out["error"] != "documents_incomplete" {
		t.Fatalf("partial submit = %d %v", status, out)
	}

	// The last per-receivable document completes it.
	status, out = qeRequest(t, env, http.MethodPatch,
		"/api/v1/receivables/"+recID+"/documents", map[string]any{"documents": map[string]any{"buku_kia": true}})
	payMapData(t, status, out, http.StatusOK)
	status, out = cbSubmit(t, env, batchID)
	data = payMapData(t, status, out, http.StatusOK)
	if data["status"] != "submitted" {
		t.Fatalf("submitted status = %v", data["status"])
	}
	rows := rcList(t, env, "?payer_id="+payerID)
	if len(rows) != 1 || rows[0]["status"] != "submitted" {
		t.Fatalf("receivable after submit = %v", rows)
	}
	// Submitting twice is refused.
	if status, out = cbSubmit(t, env, batchID); status != http.StatusConflict || out["error"] != "batch is not draft" {
		t.Fatalf("resubmit = %d %v", status, out)
	}
}

// TestClaimBatch_SubmitRejectsExpired — a receivable whose expires_on is
// before today blocks submission until the expiry is cleared.
func TestClaimBatch_SubmitRejectsExpired(t *testing.T) {
	env := payNewEnv(t, "cb.exp")
	payerID, itemID := rcClaimBPJS(t, env, "CBX", "other")
	admID := rcNewClaimAdmission(t, env, payerID, itemID, "CBXA")
	today := rcToday()
	data := cbCreateBatch(t, env, payerID, today, today)
	batchID, _ := data["id"].(string)
	recID := cbOnlyReceivable(t, env, batchID, admID)
	cbTickBatchDocs(t, env, batchID, map[string]any{
		"fpk": true, "rekap": true, "kuitansi": true, "sptjm": true,
	})

	ctx := context.Background()
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.receivable SET expires_on = $2 WHERE id = $1",
		recID, today.AddDate(0, 0, -1).Format("2006-01-02")); err != nil {
		t.Fatalf("set expired: %v", err)
	}
	status, out := cbSubmit(t, env, batchID)
	if status != http.StatusConflict || out["error"] != "expired_receivables" {
		t.Fatalf("expired submit = %d %v", status, out)
	}
	ids, _ := out["receivable_ids"].([]any)
	if len(ids) != 1 || ids[0] != recID {
		t.Fatalf("receivable_ids = %v, want [%s]", ids, recID)
	}

	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.receivable SET expires_on = $2 WHERE id = $1",
		recID, today.AddDate(0, 0, 30).Format("2006-01-02")); err != nil {
		t.Fatalf("reset expiry: %v", err)
	}
	status, out = cbSubmit(t, env, batchID)
	data = payMapData(t, status, out, http.StatusOK)
	if data["status"] != "submitted" {
		t.Fatalf("submit after reset = %v", data)
	}
}

// TestClaimBatch_StatusFlow — statuses move strictly forward (submitted →
// verified → paid → closed); closed requires every receivable paid/settled.
func TestClaimBatch_StatusFlow(t *testing.T) {
	env := payNewEnv(t, "cb.status")
	payerID, itemID := rcClaimBPJS(t, env, "CBS", "other")
	admID := rcNewClaimAdmission(t, env, payerID, itemID, "CBSA")
	today := rcToday()
	data := cbCreateBatch(t, env, payerID, today, today)
	batchID, _ := data["id"].(string)
	recID := cbOnlyReceivable(t, env, batchID, admID)
	cbTickBatchDocs(t, env, batchID, map[string]any{
		"fpk": true, "rekap": true, "kuitansi": true, "sptjm": true,
	})
	if status, out := cbSubmit(t, env, batchID); status != http.StatusOK {
		t.Fatalf("submit = %d %v", status, out)
	}

	move := func(target string) (int, map[string]any) {
		return qeRequest(t, env, http.MethodPost,
			"/api/v1/claim-batches/"+batchID+"/status", map[string]any{"status": target})
	}
	// Skipping verified is refused; unknown statuses are 400.
	if status, out := move("paid"); status != http.StatusConflict || out["error"] != "invalid status transition" {
		t.Fatalf("skip status = %d %v", status, out)
	}
	if status, out := move("bogus"); status != http.StatusBadRequest {
		t.Fatalf("bogus status = %d %v", status, out)
	}
	for _, target := range []string{"verified", "paid"} {
		status, out := move(target)
		if data := payMapData(t, status, out, http.StatusOK); data["status"] != target {
			t.Fatalf("%s = %v", target, data["status"])
		}
	}
	// Closed needs the receivable money-settled first.
	if status, out := move("closed"); status != http.StatusConflict || out["error"] != "batch has unpaid receivables" {
		t.Fatalf("premature closed = %d %v", status, out)
	}
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.receivable SET status = 'paid', paid_amount = amount WHERE id = $1", recID); err != nil {
		t.Fatalf("pay receivable: %v", err)
	}
	status, out := move("closed")
	if data := payMapData(t, status, out, http.StatusOK); data["status"] != "closed" {
		t.Fatalf("closed = %v", data["status"])
	}
}

// TestClaimBatch_ExportCSV — the CSV carries the literal header, one row per
// payer-side charge split (nomor_peserta from the coverage membership, quantity
// raw, nominal formatted) and a TOTAL row, served with attachment headers.
func TestClaimBatch_ExportCSV(t *testing.T) {
	env := payNewEnv(t, "cb.export")
	payerID, itemID := rcClaimBPJS(t, env, "CBF", "other")
	personID, admID := bilNewPersonAndVisit(t, env, "CBF", "Pasien Klaim CBF")
	// Kepesertaan: member number flows into the nomor_peserta column.
	status, out := qeRequest(t, env, http.MethodPost,
		"/api/v1/persons/"+personID+"/coverages",
		map[string]any{"payer_id": payerID, "member_number": "CB-0001"})
	covID := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut,
		"/api/v1/admissions/"+admID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{{"payer_id": payerID, "coverage_id": covID}}})
	if status != http.StatusOK {
		t.Fatalf("put guarantor = %d %v", status, out)
	}
	// One charge with quantity 2 → one payer split of 20000.00.
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/admissions/"+admID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "2"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/admissions/"+admID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	today := rcToday()
	data := cbCreateBatch(t, env, payerID, today, today)
	batchID, _ := data["id"].(string)

	code, header, body := cbRaw(t, env, "/api/v1/claim-batches/"+batchID+"/export.csv")
	if code != http.StatusOK {
		t.Fatalf("export = %d %s", code, body)
	}
	if header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("content-type = %q", header.Get("Content-Type"))
	}
	wantDisp := `attachment; filename="klaim-CBF-BPJS-` + today.Format("2006-01-02") + `-` +
		today.Format("2006-01-02") + `.csv"`
	if header.Get("Content-Disposition") != wantDisp {
		t.Fatalf("content-disposition = %q, want %q", header.Get("Content-Disposition"), wantDisp)
	}
	lines := strings.Split(strings.TrimRight(body, "\r\n"), "\n")
	wantHeader := "no;nomor_peserta;nama_pasien;no_rm;no_kunjungan;tanggal_layanan;jenis_klaim;kode_layanan;nama_layanan;jumlah;nominal"
	if lines[0] != wantHeader {
		t.Fatalf("header = %q", lines[0])
	}
	// One split payer row + the TOTAL row.
	if len(lines) != 3 {
		t.Fatalf("csv lines = %d, want 3: %q", len(lines), body)
	}
	fields := strings.Split(lines[1], ";")
	if len(fields) != 11 {
		t.Fatalf("row fields = %d: %q", len(fields), lines[1])
	}
	if fields[0] != "1" || fields[1] != "CB-0001" || fields[2] != "Pasien Klaim CBF" ||
		fields[4] == "" || fields[5] != today.Format("2006-01-02") ||
		fields[6] != "other" || fields[7] != "BIL-CBF" || fields[8] == "" ||
		fields[9] != "2.000" || fields[10] != "20000.00" {
		t.Fatalf("split row = %q", lines[1])
	}
	total := strings.Split(lines[2], ";")
	if total[0] != "TOTAL" || total[10] != "20000.00" {
		t.Fatalf("total row = %q", lines[2])
	}
	for i := 1; i < 10; i++ {
		if total[i] != "" {
			t.Fatalf("total row field %d = %q, want empty", i, total[i])
		}
	}
}
