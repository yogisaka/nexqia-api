//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestFeePayout_* covers the monthly fee payout batch API (spec
// 2026-10-07-tariff-d-practitioner-fees-tax §4.3): create from open fees,
// other income for employees, lock with PPh 21 (partner 50% × Pasal 17,
// TER monthly, annual December), NIK requirement, reopen guard, draft
// deletion and the tax CSV export.

// fpNewEnv is payNewEnv with a router whose NIK encryption/search keys are
// set — testConfig() leaves them empty and pgcrypto rejects an empty
// passphrase (pola mergeTestRouter).
func fpNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 6000
	cfg.RateLimitAPIBurst = 1000
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.NIKSearchKey = "test-search-key"
	router := server.NewRouter(pool, redisClient, cfg)
	phone := fmt.Sprintf("0812397%05d", qePhoneSeq.Add(1))
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "fp."+label, phone)
	if _, err := pool.Exec(ctx, "UPDATE core.merchant SET timezone = 'Asia/Jakarta' WHERE id = $1", merchantID); err != nil {
		t.Fatalf("set merchant timezone: %v", err)
	}
	return &qeEnv{pool: pool, router: router, companyID: companyID, merchantID: merchantID, token: token}
}

// fpSetNIK stores an encrypted NIK on a person (same pgcrypto shape as the
// person API, key = fpNewEnv's NIKEncryptionKey).
func fpSetNIK(t *testing.T, env *qeEnv, personID, nik string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE core.person SET nik = encode(pgp_sym_encrypt($2, 'test-nik-key'), 'base64') WHERE id = $1",
		personID, nik); err != nil {
		t.Fatalf("set nik: %v", err)
	}
}

// fpMedisComponent returns the id of the merchant's MEDIS rate component,
// creating it once per merchant (bootstrap already seeds a default MEDIS
// component for some flows — never duplicate it).
var (
	fpMedisMu   sync.Mutex
	fpMedisByOp = map[string]string{} // merchantID → component id
)

func fpMedisComponent(t *testing.T, env *qeEnv) string {
	t.Helper()
	fpMedisMu.Lock()
	defer fpMedisMu.Unlock()
	if id, ok := fpMedisByOp[env.merchantID]; ok {
		return id
	}
	id := plCreateComponent(t, env, "MEDIS")
	fpMedisByOp[env.merchantID] = id
	return id
}

// fpSingleMedisFee creates a price list with a single MEDIS component priced
// unitPrice per unit, sets it as the merchant default, a 100% MEDIS fee rule
// for the physician, one charge + performer and finalizes the billing; it
// returns the billing account id.
func fpSingleMedisFee(t *testing.T, env *qeEnv, label, physicianID, unitPrice, qty string) string {
	t.Helper()
	itemID := plCreateItem(t, env, "FP-"+label, "consultation")
	medis := fpMedisComponent(t, env)
	listID := plCreateList(t, env, "FP-L-"+label, "", "", "")
	if status, out := plSetPrice(t, env, listID, itemID, plToday(env, 0), map[string]string{medis: unitPrice}); status != http.StatusOK {
		t.Fatalf("set price = %d %v", status, out)
	}
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"default_price_list_id": listID})
	payMapData(t, status, out, http.StatusOK)
	frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "100.00", "effective_from": plToday(env, 0),
	}, http.StatusCreated)
	_, admissionID := bilNewPersonAndVisit(t, env, label, "Pasien "+label)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": qty})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+chargeID+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	return bfeeAccountID(t, env, admissionID)
}

// fpCreateBatch posts the batch create endpoint and asserts the status.
func fpCreateBatch(t *testing.T, env *qeEnv, year, month, want int) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-batches",
		map[string]any{"year": year, "month": month})
	return payMapData(t, status, out, want)
}

// fpLine finds the batch line of one physician in a batch response.
func fpLine(t *testing.T, batch map[string]any, physicianID string) map[string]any {
	t.Helper()
	lines, _ := batch["lines"].([]any)
	for _, l := range lines {
		m, _ := l.(map[string]any)
		if m["physician_id"] == physicianID {
			return m
		}
	}
	t.Fatalf("line for %s missing in %v", physicianID, batch)
	return nil
}

// fpRequestRaw performs an authenticated request and returns the raw body
// (for the CSV export).
func fpRequestRaw(t *testing.T, env *qeEnv, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("X-Company-ID", env.companyID)
	req.Header.Set("X-Merchant-ID", env.merchantID)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// fpSetFeeServiceDate rewrites the service_date of the single fee of an
// account (used to stage November/December batches of a previous year).
func fpSetFeeServiceDate(t *testing.T, env *qeEnv, accountID, date string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE operations.practitioner_fee SET service_date = $2 WHERE account_id = $1",
		accountID, date); err != nil {
		t.Fatalf("set fee service_date: %v", err)
	}
}

// TestFeePayout_CreateAndLines — a batch gathers the open fees per nakes
// (gross/fee sums, employment snapshot), marks them in_batch, guards
// duplicates / future months / empty months and lists per year.
func TestFeePayout_CreateAndLines(t *testing.T) {
	env := fpNewEnv(t, "create")
	physicianID, _ := frCreatePhysician(t, env, "FP Cr", nil)
	fpSingleMedisFee(t, env, "CR", physicianID, "100000.00", "1")

	now := time.Now()
	y, m := now.Year(), int(now.Month())
	batch := fpCreateBatch(t, env, y, m, http.StatusCreated)
	if batch["status"] != "draft" || batch["period_year"] != float64(y) || batch["period_month"] != float64(m) {
		t.Fatalf("batch = %v", batch)
	}
	line := fpLine(t, batch, physicianID)
	if line["gross_amount"] != "100000.00" || line["fee_amount"] != "100000.00" || line["employment_type"] != "partner" {
		t.Fatalf("line = %v", line)
	}
	wantSetor := time.Date(y, time.Month(m+1), 15, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	wantLapor := time.Date(y, time.Month(m+1), 20, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	if batch["deadline_setor"] != wantSetor || batch["deadline_lapor"] != wantLapor {
		t.Fatalf("deadlines = %v/%v, want %s/%s", batch["deadline_setor"], batch["deadline_lapor"], wantSetor, wantLapor)
	}
	// The fee is now in the batch.
	var feeStatus string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT status FROM operations.practitioner_fee WHERE account_id = (SELECT id FROM operations.billing_account WHERE merchant_id = $1 LIMIT 1)",
		env.merchantID).Scan(&feeStatus); err != nil || feeStatus != "in_batch" {
		t.Fatalf("fee status = %q (%v), want in_batch", feeStatus, err)
	}
	// Duplicate month → 409.
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-batches",
		map[string]any{"year": y, "month": m})
	if status != http.StatusConflict || out["error"] != "fee batch for this month already exists" {
		t.Fatalf("duplicate = %d %v", status, out)
	}
	// Future month → 400.
	fpCreateBatchStatus := func(yy, mm, want int, msg string) {
		t.Helper()
		st, o := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-batches",
			map[string]any{"year": yy, "month": mm})
		if st != want {
			t.Fatalf("%s = %d %v, want %d", msg, st, o, want)
		}
		return
	}
	fpCreateBatchStatus(y, m+1, http.StatusBadRequest, "future month")
	// A past month without open fees → 409.
	pm, py := m-1, y
	if pm == 0 {
		pm, py = 12, y-1
	}
	st, o := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-batches",
		map[string]any{"year": py, "month": pm})
	if st != http.StatusConflict || o["error"] != "no open fees up to this month" {
		t.Fatalf("empty month = %d %v", st, o)
	}
	// GET one + list by year.
	batchID, _ := batch["id"].(string)
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/fee-batches/"+batchID, nil)
	if data := payMapData(t, status, out, http.StatusOK); data["id"] != batchID {
		t.Fatalf("get batch = %v", data)
	}
	status, out = qeRequest(t, env, http.MethodGet, fmt.Sprintf("/api/v1/merchants/%s/fee-batches?year=%d", env.merchantID, y), nil)
	if rows := payDataSlice(t, out); len(rows) != 1 || rows[0]["id"] != batchID {
		t.Fatalf("list = %v", rows)
	}
}

// TestFeePayout_OtherGrossEmployeeOnly — other_gross is accepted on employee
// lines in a draft batch only; partner lines are rejected.
func TestFeePayout_OtherGrossEmployeeOnly(t *testing.T) {
	env := fpNewEnv(t, "other")
	employeeID, employee := frCreatePhysician(t, env, "FP Emp", map[string]any{
		"employment_type": "employee", "ptkp_status": "TK/0",
	})
	fpSetNIK(t, env, employee["PersonID"].(string), "3201010101800099")
	partnerID, partner := frCreatePhysician(t, env, "FP Prt", nil)
	fpSetNIK(t, env, partner["PersonID"].(string), "3201010101800098")
	fpSingleMedisFee(t, env, "EMP", employeeID, "100000.00", "1")
	fpSingleMedisFee(t, env, "PRT", partnerID, "100000.00", "1")

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	path := "/api/v1/fee-batches/" + batchID + "/lines/"
	status, out := qeRequest(t, env, http.MethodPut, path+employeeID, map[string]any{"other_gross": "500000"})
	data := payMapData(t, status, out, http.StatusOK)
	// The PUT returns the full batch (contract §2): draft status and a lines array.
	if data["status"] != "draft" {
		t.Fatalf("batch = %v", data)
	}
	if _, ok := data["lines"].([]any); !ok {
		t.Fatalf("lines not an array: %v", data["lines"])
	}
	if line := fpLine(t, data, employeeID); line["other_gross"] != "500000.00" {
		t.Fatalf("employee line = %v", line)
	}
	// Partner line → 400.
	status, out = qeRequest(t, env, http.MethodPut, path+partnerID, map[string]any{"other_gross": "500000"})
	if status != http.StatusBadRequest {
		t.Fatalf("partner other_gross = %d %v, want 400", status, out)
	}
	// After lock (draft only) → 409.
	status, _ = qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+batchID+"/lock", nil)
	if status != http.StatusOK {
		t.Fatalf("lock = %d", status)
	}
	status, _ = qeRequest(t, env, http.MethodPut, path+employeeID, map[string]any{"other_gross": "600000"})
	if status != http.StatusConflict {
		t.Fatalf("locked line edit = %d, want 409", status)
	}
}

// TestFeePayout_LockPartnerTax — a partner with a 10,000,000.00 gross pays
// 50% × Pasal 17: base 5,000,000.00 → 5% bracket = 250,000.00; net 9,750,000.00.
func TestFeePayout_LockPartnerTax(t *testing.T) {
	env := fpNewEnv(t, "partner")
	partnerID, partner := frCreatePhysician(t, env, "FP Tax", nil)
	fpSetNIK(t, env, partner["PersonID"].(string), "3201010101800097")
	fpSingleMedisFee(t, env, "TAX", partnerID, "100000.00", "100")

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+batchID+"/lock", nil)
	data := payMapData(t, status, out, http.StatusOK)
	if data["status"] != "locked" {
		t.Fatalf("batch = %v", data)
	}
	line := fpLine(t, data, partnerID)
	// 10.000.000 bruto → dasar 5.000.000 (50%) → Pasal 17 5% = 250.000.
	if line["tax_method"] != "partner_50pct" || line["tax_base"] != "5000000.00" ||
		line["tax_amount"] != "250000.00" || line["net_amount"] != "9750000.00" {
		t.Fatalf("partner line = %v", line)
	}
	if line["ter_category"] != nil {
		t.Fatalf("partner ter_category = %v", line["ter_category"])
	}
}

// TestFeePayout_LockNeedsNIK — locking fails with the contract message when
// a practitioner has no NIK.
func TestFeePayout_LockNeedsNIK(t *testing.T) {
	env := fpNewEnv(t, "nonik")
	physicianID, _ := frCreatePhysician(t, env, "FP NoNik", nil)
	fpSingleMedisFee(t, env, "NONIK", physicianID, "100000.00", "1")

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+batchID+"/lock", nil)
	if status != http.StatusConflict {
		t.Fatalf("lock without NIK = %d %v, want 409", status, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "practitioner Nakes FP NoNik has no NIK") {
		t.Fatalf("error = %q", msg)
	}
}

// TestFeePayout_ReopenBlocked — an account whose fees sit in a (draft)
// batch cannot be reopened.
func TestFeePayout_ReopenBlocked(t *testing.T) {
	env := fpNewEnv(t, "reopen")
	physicianID, _ := frCreatePhysician(t, env, "FP Re", nil)
	accountID := fpSingleMedisFee(t, env, "RE", physicianID, "100000.00", "1")
	var admissionID string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT admission_id FROM operations.billing_account WHERE id = $1", accountID).Scan(&admissionID); err != nil {
		t.Fatalf("find admission: %v", err)
	}
	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	_ = batchID
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "salah"})
	if status != http.StatusConflict || out["error"] != "fees of this billing are in a payout batch" {
		t.Fatalf("reopen = %d %v, want 409", status, out)
	}
}

// TestFeePayout_DeleteDraftReleasesFees — deleting a draft batch returns its
// fees to open and removes the batch.
func TestFeePayout_DeleteDraftReleasesFees(t *testing.T) {
	env := fpNewEnv(t, "delete")
	physicianID, _ := frCreatePhysician(t, env, "FP Del", nil)
	accountID := fpSingleMedisFee(t, env, "DEL", physicianID, "100000.00", "1")
	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	status, _ := qeRequest(t, env, http.MethodDelete, "/api/v1/fee-batches/"+batchID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", status)
	}
	var feeStatus string
	var batchIDNull any
	if err := env.pool.QueryRow(context.Background(),
		"SELECT status, batch_id FROM operations.practitioner_fee WHERE account_id = $1",
		accountID).Scan(&feeStatus, &batchIDNull); err != nil || feeStatus != "open" || batchIDNull != nil {
		t.Fatalf("fee after delete = %q/%v (%v), want open/NULL", feeStatus, batchIDNull, err)
	}
	status, _ = qeRequest(t, env, http.MethodGet, "/api/v1/fee-batches/"+batchID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("get deleted = %d, want 404", status)
	}
}

// TestFeePayout_TaxExportCSV — the CSV header/columns are literal, the NIK is
// decrypted in the export, drafts are rejected.
func TestFeePayout_TaxExportCSV(t *testing.T) {
	env := fpNewEnv(t, "csv")
	employeeID, employee := frCreatePhysician(t, env, "FP CsvE", map[string]any{
		"employment_type": "employee", "ptkp_status": "TK/0", "npwp": "12.345.678.9-012.345",
	})
	fpSetNIK(t, env, employee["PersonID"].(string), "3201010101800099")
	partnerID, partner := frCreatePhysician(t, env, "FP CsvP", map[string]any{"npwp": "98.765.432.1-543.210"})
	fpSetNIK(t, env, partner["PersonID"].(string), "3201010101800098")
	fpSingleMedisFee(t, env, "CSVE", employeeID, "100000.00", "1")
	fpSingleMedisFee(t, env, "CSVP", partnerID, "100000.00", "1")

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	status, out := qeRequest(t, env, http.MethodPut, "/api/v1/fee-batches/"+batchID+"/lines/"+employeeID,
		map[string]any{"other_gross": "500000"})
	payMapData(t, status, out, http.StatusOK)
	csvPath := "/api/v1/fee-batches/" + batchID + "/tax-export.csv"
	// Draft batch → 409.
	if st, _ := fpRequestRaw(t, env, http.MethodGet, csvPath); st != http.StatusConflict {
		t.Fatalf("draft export = %d, want 409", st)
	}
	status, _ = qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+batchID+"/lock", nil)
	if status != http.StatusOK {
		t.Fatalf("lock = %d", status)
	}
	code, body := fpRequestRaw(t, env, http.MethodGet, csvPath)
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, body)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	wantHeader := "masa_pajak;tahun;nik;npwp;nama;status_ptkp;jenis_pemotongan;penghasilan_bruto;dasar_pengenaan;tarif;pph_dipotong"
	if len(lines) != 3 || lines[0] != wantHeader {
		t.Fatalf("csv header/rows = %q (%d rows)", lines[0], len(lines))
	}
	joined := body
	if !strings.Contains(joined, "3201010101800099") || !strings.Contains(joined, "3201010101800098") {
		t.Fatalf("decrypted NIK missing: %s", body)
	}
	if !strings.Contains(joined, "BPMP-pegawai_tetap") || !strings.Contains(joined, "BP21-bukan_pegawai") {
		t.Fatalf("pemotongan codes missing: %s", body)
	}
	if !strings.Contains(joined, "12.345.678.9-012.345") || !strings.Contains(joined, "98.765.432.1-543.210") {
		t.Fatalf("npwp missing: %s", body)
	}
	// Employee row: bruto = 100000.00 + 500000.00 = 600000.00 (TER A: 0%).
	empRow := strings.Split(lines[1], ";")
	if empRow[0] != strconv.Itoa(int(now.Month())) || empRow[1] != strconv.Itoa(now.Year()) ||
		empRow[5] != "TK/0" || empRow[6] != "BPMP-pegawai_tetap" || empRow[7] != "600000.00" || empRow[10] != "0.00" {
		t.Fatalf("employee row = %v", empRow)
	}
}

// TestFeePayout_December — for an employee, the December batch uses the
// annual_december method over the locked November line of the same year:
// annual tax = Pasal 17 on the annual PKP minus the Jan–Nov withholding.
func TestFeePayout_December(t *testing.T) {
	env := fpNewEnv(t, "dec")
	employeeID, employee := frCreatePhysician(t, env, "FP Dec", map[string]any{
		"employment_type": "employee", "ptkp_status": "TK/0",
	})
	fpSetNIK(t, env, employee["PersonID"].(string), "3201010101800096")
	// November fee 6,000,000.00 (TER band 5.4–6.2 jt) and December fee 2,000,000.00.
	novAccount := fpSingleMedisFee(t, env, "NOV", employeeID, "6000000.00", "1")
	decAccount := fpSingleMedisFee(t, env, "DEC", employeeID, "2000000.00", "1")
	prevYear := time.Now().Year() - 1
	fpSetFeeServiceDate(t, env, novAccount, fmt.Sprintf("%d-11-10", prevYear))
	fpSetFeeServiceDate(t, env, decAccount, fmt.Sprintf("%d-12-05", prevYear))

	novBatch := fpCreateBatch(t, env, prevYear, 11, http.StatusCreated)
	novID, _ := novBatch["id"].(string)
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+novID+"/lock", nil)
	novData := payMapData(t, status, out, http.StatusOK)
	novLine := fpLine(t, novData, employeeID)
	if novLine["tax_method"] != "ter_monthly" {
		t.Fatalf("november line = %v", novLine)
	}
	// Annual gross 8,000,000 − biaya jabatan 400,000 − PTKP 54,000,000 < 0
	// → PKP 0 → annual tax 0 → December tax = 0 − November withholding.
	novTax, err := strconv.ParseInt(strings.TrimSuffix(novLine["tax_amount"].(string), ".00"), 10, 64)
	if err != nil {
		t.Fatalf("nov line = %v", novLine)
	}
	decBatch := fpCreateBatch(t, env, prevYear, 12, http.StatusCreated)
	decID, _ := decBatch["id"].(string)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+decID+"/lock", nil)
	decData := payMapData(t, status, out, http.StatusOK)
	decLine := fpLine(t, decData, employeeID)
	if decLine["tax_method"] != "annual_december" {
		t.Fatalf("december line = %v", decLine)
	}
	decTax, err := strconv.ParseInt(strings.TrimSuffix(decLine["tax_amount"].(string), ".00"), 10, 64)
	if err != nil {
		t.Fatalf("dec tax = %v", decLine["tax_amount"])
	}
	if decTax != -novTax {
		t.Fatalf("dec tax = %d, want -%d (over-withheld)", decTax, novTax)
	}
	// Negative tax does not add to net: net = fee 2,000,000.00.
	if decLine["net_amount"] != "2000000.00" {
		t.Fatalf("dec net = %v", decLine["net_amount"])
	}
}

// TestFeePayout_FutureMonthRejected — creating a batch for a future month is
// rejected with 400 (guard evaluated in the merchant timezone).
func TestFeePayout_FutureMonthRejected(t *testing.T) {
	env := fpNewEnv(t, "future")
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/fee-batches",
		map[string]any{"year": time.Now().Year() + 2, "month": 1})
	if status != http.StatusBadRequest || out["error"] != "fee batch month is in the future" {
		t.Fatalf("future month = %d %v, want 400", status, out)
	}
}

// TestFeePayout_SoftDeletedPhysicianStillPaid — regression locking the spec
// 2026-10-16 §4 decision: a soft-deleted physician's open fees are still
// gathered into the payout batch; do NOT add a deleted_at filter to the fee
// batch query.
func TestFeePayout_SoftDeletedPhysicianStillPaid(t *testing.T) {
	env := fpNewEnv(t, "softdel")
	physicianID, physician := frCreatePhysician(t, env, "FP Soft", map[string]any{
		"employment_type": "employee", "ptkp_status": "TK/0",
	})
	fpSetNIK(t, env, physician["PersonID"].(string), "3201010101800095")
	fpSingleMedisFee(t, env, "SOFT", physicianID, "100000.00", "1")

	status, _ := qeRequest(t, env, http.MethodDelete, "/api/v1/physicians/"+physicianID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete physician = %d, want 204", status)
	}

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	if batch["status"] != "draft" {
		t.Fatalf("batch = %v", batch)
	}
	line := fpLine(t, batch, physicianID)
	if line["fee_amount"] != "100000.00" {
		t.Fatalf("soft-deleted physician line = %v", line)
	}
}

// TestFeePayout_TaxExportCSVFormulaSafety — a person name that Excel would
// read as a formula is neutralized with a leading apostrophe in the tax CSV.
func TestFeePayout_TaxExportCSVFormulaSafety(t *testing.T) {
	env := fpNewEnv(t, "csvsafe")
	employeeID, employee := frCreatePhysician(t, env, "FP SafeE", map[string]any{
		"employment_type": "employee", "ptkp_status": "TK/0", "npwp": "12.345.678.9-012.345",
	})
	fpSetNIK(t, env, employee["PersonID"].(string), "3201010101800099")
	fpSingleMedisFee(t, env, "SAFE", employeeID, "100000.00", "1")

	now := time.Now()
	batch := fpCreateBatch(t, env, now.Year(), int(now.Month()), http.StatusCreated)
	batchID, _ := batch["id"].(string)
	// Formula-like person name, before the lock snapshot the CSV reads.
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE core.person SET full_name = '=SUM(1+1)' WHERE id = $1",
		employee["PersonID"].(string)); err != nil {
		t.Fatalf("set full_name: %v", err)
	}
	status, _ := qeRequest(t, env, http.MethodPost, "/api/v1/fee-batches/"+batchID+"/lock", nil)
	if status != http.StatusOK {
		t.Fatalf("lock = %d", status)
	}
	code, body := fpRequestRaw(t, env, http.MethodGet, "/api/v1/fee-batches/"+batchID+"/tax-export.csv")
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, body)
	}
	if !strings.Contains(body, "'=SUM(1+1)") {
		t.Fatalf("csv-safe name missing: %s", body)
	}
}
