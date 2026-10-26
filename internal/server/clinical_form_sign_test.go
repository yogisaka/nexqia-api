//go:build integration

package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

	// One character changed → different hash.
	changed := `{"sections":{"subjective":"Demam 4 hari, batuk berdahak.","objective":{"sistolic":120,"diastolic":80,"heart_rate":88,"temperature":37.4,"respiratory_rate":20,"spo2":97,"weight_kg":62.5,"height_cm":168,"pain_score":2},"assessment":{"narrative":"Suspek infeksi saluran napas atas","diagnoses":[]},"plan":"Parasetamol 500 mg 3×1 bila demam; kontrol 3 hari bila memburuk."}}`
	idC := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, changed)
	cfPinFilledAt(t, ctx, pool, idC)
	if hashC := sign(idC); hashC == hashA {
		t.Fatalf("one-char change produced the same hash")
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
	addendumData := `{"sections":{"subjective":"Demam 3 hari, muntah tambahan.","objective":{"sistolic":115,"diastolic":75},"assessment":{"narrative":"ISEL","diagnoses":[]},"plan":"Lanjut terapi."}}`
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
