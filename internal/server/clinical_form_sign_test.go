//go:build integration

package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// Sign/lock/addendum/counter-sign endpoint tests (plan 2026-10-21-cppt-rajal-a
// Task 3, spec 2026-10-21-cppt-rajal-fase1-design §6). Fixtures reuse the
// Task 2 helpers; the owner password from seedAccountOwner is "Passw0rd123!".

const ownerPassword = "Passw0rd123!"

// cfCreateDraft creates a draft SOAP entry and returns its id.
func cfCreateDraft(t *testing.T, router *gin.Engine, token, companyID, merchantID, admissionID, formData string) string {
	t.Helper()
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "soap_dokter", "form_data": cfRawJSON(t, formData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft = %d: %s", rec.Code, rec.Body.String())
	}
	return resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
}

// cfPinFilledAt pins filled_at to the same instant for all given entries so
// their signature preimages differ only by content (hash includes filled_at).
func cfPinFilledAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	at := time.Date(2026, 10, 21, 9, 30, 0, 123456789, time.FixedZone("WIB", 7*3600))
	for _, id := range ids {
		if _, err := pool.Exec(ctx,
			"UPDATE clinical_form.form_instance SET filled_at = $1 WHERE id = $2::uuid", at, id); err != nil {
			t.Fatalf("pin filled_at: %v", err)
		}
	}
}

// cfGetEntryRow reads the full entry row straight from the pool.
func cfGetEntryRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) sqlcgen.ClinicalFormFormInstance {
	t.Helper()
	var entryID pgtype.UUID
	_ = entryID.Scan(id)
	entry, err := sqlcgen.New(pool).GetClinicalFormInstance(ctx, entryID)
	if err != nil {
		t.Fatalf("get entry row: %v", err)
	}
	return entry
}

// TestClinicalFormSignLocksEntry — after sign: PATCH/DELETE/re-sign → 409,
// TTE fields filled, VerifyEntrySignature passes (spec §6.1/§6.3).
func TestClinicalFormSignLocksEntry(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "sign")
	cdSeedICD11(t, ctx, pool)
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "sign")
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign = %d: %s", rec.Code, rec.Body.String())
	}
	entry := resp["data"].(map[string]any)["entry"].(map[string]any)
	if entry["status"] != "signed" {
		t.Fatalf("status = %v, want signed", entry["status"])
	}
	hash, _ := entry["signature_hash"].(string)
	if hash == "" || entry["signed_at"] == nil {
		t.Fatalf("TTE fields = %v", entry)
	}

	// Lock: PATCH / DELETE / sign again all 409.
	body := map[string]any{"form_data": cfRawJSON(t, soapFormData)}
	if rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, body); rec.Code != http.StatusConflict || resp["error"] != "entri sudah ditandatangani dan terkunci" {
		t.Fatalf("patch after sign = %d %v", rec.Code, resp)
	}
	if rec, resp = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, nil); rec.Code != http.StatusConflict || resp["error"] != "entri sudah ditandatangani dan terkunci" {
		t.Fatalf("delete after sign = %d %v", rec.Code, resp)
	}
	if rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusConflict || resp["error"] != "entri sudah ditandatangani dan terkunci" {
		t.Fatalf("re-sign = %d %v", rec.Code, resp)
	}

	// Signature verification recomputes the preimage from stored fields.
	row := cfGetEntryRow(t, ctx, pool, id)
	if row.SignatureHash.String != hash {
		t.Fatalf("stored hash = %s, want %s", row.SignatureHash.String, hash)
	}
	if err := server.VerifyEntrySignature(row); err != nil {
		t.Fatalf("VerifyEntrySignature = %v, want nil", err)
	}
}

// TestClinicalFormSignatureDeterministic — identical content (same pinned
// filled_at) hashes SAMA; a one-character change BERBEDA; the preimage prefix
// "nexqia-clinical-sign-v1|" reproduces the stored hash (spec §6.2 LITERAL).
func TestClinicalFormSignatureDeterministic(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, userID, token := cfSeedEnv(t, ctx, pool, router, "detsig")
	cdSeedICD11(t, ctx, pool)
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "detsig")

	idA := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	idB := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	cfPinFilledAt(t, ctx, pool, idA, idB)

	sign := func(id string) string {
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
			map[string]any{"password": ownerPassword})
		if rec.Code != http.StatusOK {
			t.Fatalf("sign %s = %d: %s", id, rec.Code, rec.Body.String())
		}
		h, _ := resp["data"].(map[string]any)["entry"].(map[string]any)["signature_hash"].(string)
		return h
	}
	hashA, hashB := sign(idA), sign(idB)
	// The preimage includes the entry id, so two distinct entries hash
	// differently by design; determinism lives in the canonical form_data:
	// identical content must canonicalize identically (SAMA), one changed
	// character differently (BERBEDA).
	rowB := cfGetEntryRow(t, ctx, pool, idB)
	var fdB map[string]any
	if err := json.Unmarshal(rowB.FormData, &fdB); err != nil {
		t.Fatalf("unmarshal B form_data: %v", err)
	}
	canonicalB, _ := json.Marshal(fdB)
	if hashA == "" || hashB == "" {
		t.Fatalf("empty hashes A=%s B=%s", hashA, hashB)
	}

	// One character changed → different hash. v3-shaped fixture (guard TTE
	// sign passes — spec 2026-11-10-cppt-pemeriksaan-v3 §Guard TTE sign).
	changed := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 4 hari, batuk berdahak.","diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]}}}`
	idC := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, changed)
	cfPinFilledAt(t, ctx, pool, idC)
	if hashC := sign(idC); hashC == hashA {
		t.Fatalf("one-char change produced the same hash")
	}

	// v5 shape (spec 2026-11-20 §2/§3.3): organ marks/pins + legend + nyeri
	// lokasi array + canvas_marks — bentuk baru ikut ter-hash kanonik tanpa
	// parser: recompute preimage reproduces the hash and the stored
	// signature verifies.
	marked := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Nyeri ulu hati sejak 2 hari",` +
		`"diagnosa":{"narrative":"Suspek dispepsia","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]},` +
		`"pemeriksaan_fisik":{"keadaan_umum":"sedang","organ":{"thorax":{"status":"ada_kelainan",` +
		`"marks":{"pf-16":{"strokes":[{"points":[{"x":0.31,"y":0.42}],"color":"#dc2626","width":3,"label":null}],"pins":[{"x":0.4,"y":0.3,"n":1}]}},` +
		`"legend":{"1":"Ronki basa halus kiri"}}}},"nyeri":[{"skala":"nrs","skor":3,"lokasi":["Regio frontalis","Regio epigastrium"],` +
		`"durasi":"3 hari","frekuensi":"intermiten","karakteristik":"tertusuk","berkurang":"istirahat","penyebab":"batuk","lama":"<1 minggu","poss":"1",` +
		`"marks":{"a1":[{"points":[{"x":0.5,"y":0.3}],"color":"#dc2626","width":3,"label":null}]}}]}}`
	idD := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, marked)
	cfPinFilledAt(t, ctx, pool, idD)
	hashD := sign(idD)
	rowD := cfGetEntryRow(t, ctx, pool, idD)
	var fdD map[string]any
	if err := json.Unmarshal(rowD.FormData, &fdD); err != nil {
		t.Fatalf("unmarshal D form_data: %v", err)
	}
	canonicalD, _ := json.Marshal(fdD)
	preimageD := fmt.Sprintf("nexqia-clinical-sign-v1|%s|%s|%s|%d|%s|%s|%s",
		idD, admissionID, "soap_dokter", rowD.TemplateVersion, userID,
		rowD.FilledAt.Time.Format(time.RFC3339Nano), string(canonicalD))
	sumD := sha256.Sum256([]byte(preimageD))
	if got := hex.EncodeToString(sumD[:]); got != hashD {
		t.Fatalf("recomputed marked preimage hash = %s, want %s", got, hashD)
	}
	if err := server.VerifyEntrySignature(rowD); err != nil {
		t.Fatalf("VerifyEntrySignature D = %v", err)
	}

	// Prefix shape: recomputing the spec §6.2 preimage (canonical JSON from a
	// Go map, RFC3339Nano filled_at) reproduces hashA.
	row := cfGetEntryRow(t, ctx, pool, idA)
	var fd map[string]any
	if err := json.Unmarshal(row.FormData, &fd); err != nil {
		t.Fatalf("unmarshal form_data: %v", err)
	}
	canonical, _ := json.Marshal(fd)
	if string(canonical) != string(canonicalB) {
		t.Fatalf("canonical JSON of identical content differs:\nA=%s\nB=%s", canonical, canonicalB)
	}
	preimage := fmt.Sprintf("nexqia-clinical-sign-v1|%s|%s|%s|%d|%s|%s|%s",
		idA, admissionID, "soap_dokter", row.TemplateVersion, userID,
		row.FilledAt.Time.Format(time.RFC3339Nano), string(canonical))
	sum := sha256.Sum256([]byte(preimage))
	if got := hex.EncodeToString(sum[:]); got != hashA {
		t.Fatalf("recomputed preimage hash = %s, want %s", got, hashA)
	}
	if err := server.VerifyEntrySignature(rowB); err != nil {
		t.Fatalf("VerifyEntrySignature B = %v", err)
	}
}

// TestClinicalFormSignWrongPassword — 401, no write: the entry stays draft.
func TestClinicalFormSignWrongPassword(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "wrongpw")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "wrongpw")
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": "wrongPassw0rd!"})
	if rec.Code != http.StatusUnauthorized || resp["error"] != "password salah" {
		t.Fatalf("wrong password = %d %v", rec.Code, resp)
	}
	row := cfGetEntryRow(t, ctx, pool, id)
	if row.Status != "draft" || row.SignatureHash.Valid {
		t.Fatalf("entry mutated on failed sign: status=%s hash=%v", row.Status, row.SignatureHash)
	}
}

// TestClinicalFormAddendum — target draft → 409; cross form_type → 400;
// success: source entry unchanged and the chain is readable via GET by id.
func TestClinicalFormAddendum(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "addendum")
	cdSeedICD11(t, ctx, pool)
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "addendum")

	// Target draft → 409.
	draftID := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+draftID+"/addendum", token, companyID, merchantID,
		map[string]any{"form_data": cfRawJSON(t, soapFormData), "reason": "koreksi"})
	if rec.Code != http.StatusConflict || resp["error"] != "entri asal belum ditandatangani" {
		t.Fatalf("addendum on draft = %d %v", rec.Code, resp)
	}

	// Cross form_type → 400.
	targetID := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, soapFormData)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+targetID+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("sign target = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+targetID+"/addendum", token, companyID, merchantID,
		map[string]any{"form_data": cfRawJSON(t, soapFormData), "reason": "koreksi", "form_type": "resume_rajal"})
	if rec.Code != http.StatusBadRequest || resp["error"] != "addendum harus sesama kunjungan dan jenis form" {
		t.Fatalf("cross form_type = %d %v", rec.Code, resp)
	}
	// Missing reason → 400.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+targetID+"/addendum", token, companyID, merchantID,
		map[string]any{"form_data": cfRawJSON(t, soapFormData), "reason": "   "})
	if rec.Code != http.StatusBadRequest || resp["error"] != "alasan addendum wajib" {
		t.Fatalf("empty reason = %d %v", rec.Code, resp)
	}

	// Success: new draft referencing the signed target.
	addendumData := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari, muntah tambahan.","diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]}}}`
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+targetID+"/addendum", token, companyID, merchantID,
		map[string]any{"form_data": cfRawJSON(t, addendumData), "reason": " pasien muntah setelah catatan awal "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("addendum = %d: %s", rec.Code, rec.Body.String())
	}
	add := resp["data"].(map[string]any)["entry"].(map[string]any)
	if add["status"] != "draft" || add["addendum_to_id"] != targetID || add["addendum_reason"] != "pasien muntah setelah catatan awal" {
		t.Fatalf("addendum entry = %v", add)
	}

	// Source entry unchanged.
	row := cfGetEntryRow(t, ctx, pool, targetID)
	if row.Status != "signed" || row.AddendumToID.Valid {
		t.Fatalf("source mutated: status=%s addendum_to=%v", row.Status, row.AddendumToID)
	}
	// Chain readable via GET by id.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/forms/"+targetID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get target = %d: %s", rec.Code, rec.Body.String())
	}
	chain, _ := resp["data"].(map[string]any)["addendums"].([]any)
	if len(chain) != 1 || chain[0] != add["id"] {
		t.Fatalf("addendums = %v, want [%v]", chain, add["id"])
	}
}

// soapGuardPass is the minimal v3 fixture that passes the TTE sign guard
// (spec 2026-11-10-cppt-pemeriksaan-v3 §Guard TTE sign): diagnosa primer +
// keluhan utama + jenis anamnesis filled. The primary diagnosis carries the
// icd10 auto mapping so diagnosis persistence succeeds under the default
// ICD-10 merchant standard (spec §7.3).
const soapGuardPass = `{"sections":{
 "anamnesis_info":{"jenis":"autoanamnesis"},
 "keluhan_utama":"Demam 3 hari.",
 "diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]}}}`

// cfGuardSign creates a draft with formData then signs it, returning the
// sign response recorder and decoded body.
func cfGuardSign(t *testing.T, router *gin.Engine, token, companyID, merchantID, admissionID, formData string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, formData)
	return cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
}

// TestClinicalFormSignGuardRequired — sign blocks (400) per wajib kosong,
// with the exact spec messages and fixed order; >1 primary has its own
// message; moda RTL kosong TIDAK blocks; empty draft 400s without panic
// (spec 2026-11-10-cppt-pemeriksaan-v3 §Guard TTE sign).
func TestClinicalFormSignGuardRequired(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	// ~18 API calls in one test exceeds testConfig's burst of 20 — lift the
	// rate limit for this test only (pola TestClinicalFormValidation).
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 1000
	cfg.RateLimitAPIBurst = 100
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), cfg)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "guard")
	cdSeedICD11(t, ctx, pool)
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "guard")

	// 3 wajib terisi → 200 + status signed.
	rec, resp := cfGuardSign(t, router, token, companyID, merchantID, admissionID, soapGuardPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign pass = %d: %s", rec.Code, rec.Body.String())
	}
	if entry := resp["data"].(map[string]any)["entry"].(map[string]any); entry["status"] != "signed" {
		t.Fatalf("status = %v, want signed", entry["status"])
	}

	// Tanpa diagnosa primer saja → 400, 1 item.
	noPrimary := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"Suspek ISPA"}}}`
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, noPrimary)
	if rec.Code != http.StatusBadRequest || resp["error"] != "bagian wajib belum terisi: diagnosa primer" {
		t.Fatalf("no primary = %d %v", rec.Code, resp)
	}

	// keluhan_utama kosong (trim) saja → 400 (diagnosa primer ada, jadi
	// hanya "keluhan utama" yang dilaporkan).
	emptyKeluhan := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"   ","diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"}}]}}}`
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, emptyKeluhan)
	if rec.Code != http.StatusBadRequest || resp["error"] != "bagian wajib belum terisi: keluhan utama" {
		t.Fatalf("empty keluhan = %d %v", rec.Code, resp)
	}

	// jenis anamnesis kosong saja (section absent) → 400.
	noJenis := `{"sections":{"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"}}]}}}`
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, noJenis)
	if rec.Code != http.StatusBadRequest || resp["error"] != "bagian wajib belum terisi: jenis anamnesis" {
		t.Fatalf("empty jenis = %d %v", rec.Code, resp)
	}

	// Kombinasi 2+ → daftar gabungan urutan tetap (contoh eksak spec).
	combo := `{"sections":{"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"Suspek ISPA"}}}`
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, combo)
	if rec.Code != http.StatusBadRequest || resp["error"] != "bagian wajib belum terisi: diagnosa primer, jenis anamnesis" {
		t.Fatalf("combo = %d %v", rec.Code, resp)
	}
	// Guard fires before any write: the entry stays draft.
	id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, combo)
	if _, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); resp["error"] == nil {
		t.Fatalf("combo sign expected 400, got %v", resp)
	}
	if row := cfGetEntryRow(t, ctx, pool, id); row.Status != "draft" || row.SignatureHash.Valid {
		t.Fatalf("entry mutated by guard 400: status=%s hash=%v", row.Status, row.SignatureHash)
	}

	// >1 diagnosa primer → 400 "diagnosa primer maksimal 1".
	twoPrimary := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"ISPA + ISK","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"A"}},{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"A"}}]}}}`
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, twoPrimary)
	if rec.Code != http.StatusBadRequest || resp["error"] != "diagnosa primer maksimal 1" {
		t.Fatalf("two primary = %d %v", rec.Code, resp)
	}

	// Moda RTL kosong (rtl section tanpa moda) → TIDAK 400.
	withRtlEmpty := `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"Suspek ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]},"rencana_tindak_lanjut":{"kriteria_rujukan_darurat":"Demam >3 hari"}}}`
	rec, _ = cfGuardSign(t, router, token, companyID, merchantID, admissionID, withRtlEmpty)
	if rec.Code != http.StatusOK {
		t.Fatalf("rtl empty moda = %d: %s", rec.Code, rec.Body.String())
	}

	// Draft tanpa isi apa pun → 400 gabungan lengkap (bukan panic).
	rec, resp = cfGuardSign(t, router, token, companyID, merchantID, admissionID, `{"sections":{}}`)
	if rec.Code != http.StatusBadRequest || resp["error"] != "bagian wajib belum terisi: diagnosa primer, keluhan utama, jenis anamnesis" {
		t.Fatalf("empty draft = %d %v", rec.Code, resp)
	}
}

// TestClinicalFormCountersign — no permission → 403; success fills fields;
// repeat → 409 "entri sudah di-counter-sign" (spec §6.5).
func TestClinicalFormCountersign(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "csign")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "csign")

	// Keperawatan entry (ases_awal_perawat), signed by the owner.
	perawatData := `{"sections":{"keluhan":"Pusing sejak pagi.","tanda_vital":{"sistolic":130,"diastolic":85}}}`
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_awal_perawat", "form_data": cfRawJSON(t, perawatData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create perawat = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
		t.Fatalf("sign = %d: %s", rec.Code, rec.Body.String())
	}

	// User without clinical.record.countersign → 403.
	other := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.nocountersign", server.PermClinicalRecordView)
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/countersign", other, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("countersign without perm = %d %v", rec.Code, resp)
	}

	// Holder with password → 200, counter_signed_* filled.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/countersign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("countersign = %d: %s", rec.Code, rec.Body.String())
	}
	entry := resp["data"].(map[string]any)["entry"].(map[string]any)
	if entry["counter_signed_at"] == nil || entry["counter_signed_by"] == nil {
		t.Fatalf("counter-sign fields = %v", entry)
	}

	// Idempoten: repeat → 409.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/countersign", token, companyID, merchantID,
		map[string]any{"password": ownerPassword})
	if rec.Code != http.StatusConflict || resp["error"] != "entri sudah di-counter-sign" {
		t.Fatalf("repeat countersign = %d %v", rec.Code, resp)
	}
}
