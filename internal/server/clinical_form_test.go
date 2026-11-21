//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// CPPT fase 1 endpoint tests (plan 2026-10-21-cppt-rajal-a-backend Task 2,
// spec 2026-10-21-cppt-rajal-fase1-design §5, §9). Fixtures reuse the shared
// helpers (newTestPostgresPool, seedAccountOwner, seedAuditPersonAdmission);
// each test gets its own Postgres container so fixed UUIDs are safe.

var cfCounter int64

func cfNext() int64 { return atomic.AddInt64(&cfCounter, 1) }

// cfSeedEnv seeds an Owner (holds clinical.record.view/write, countersign,
// clinical.nutrition.write, terminology.lookup via migrations 000065/000089)
// with their first merchant.
func cfSeedEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, label string) (companyID, merchantID, userID, token string) {
	t.Helper()
	n := cfNext()
	companyID, merchantID, userID, token, _ = seedAccountOwner(t, ctx, pool, router, "cf."+label, fmt.Sprintf("08123491%04d", n))
	return companyID, merchantID, userID, token
}

// cfSeedAdmission seeds a person + department + payer and registers an
// outpatient admission through POST /admissions.
func cfSeedAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID, label string) (personID, admissionID string) {
	t.Helper()
	createPersonViaAPI(t, router, token, companyID, merchantID, "Pasien CPPT "+label, "male")
	personID, _ = latestPerson(t, ctx, pool, companyID, "Pasien CPPT "+label)
	admissionID, _ = seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "cf-"+label)
	return personID, admissionID
}

// cfSeedInpatientAdmission inserts a non-outpatient admission directly (the
// API only creates outpatient visits) to prove the fase-1 gate.
func cfSeedInpatientAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, personID, label string) string {
	t.Helper()
	var departmentID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $3) RETURNING id::text",
		companyID, merchantID, "CFD-"+label).Scan(&departmentID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	var admissionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, status)
		 VALUES ($1, $2, $3, $4, 'inpatient', $5, 'admitted') RETURNING id::text`,
		companyID, merchantID, "CFD-"+label+"-001", personID, departmentID).Scan(&admissionID); err != nil {
		t.Fatalf("seed inpatient admission: %v", err)
	}
	return admissionID
}

// cfSeedUserWithPerms inserts a user whose role holds exactly `codes` at the
// merchant, then logs in and returns their token.
func cfSeedUserWithPerms(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, companyID, merchantID, username string, codes ...string) string {
	t.Helper()
	n := cfNext()
	roleID := fmt.Sprintf("bb000000-0000-0000-0000-%012x", n)
	userID := fmt.Sprintf("bc000000-0000-0000-0000-%012x", n)
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, companyID, username); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if len(codes) > 0 {
		if _, err := pool.Exec(ctx,
			"INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = ANY($2)",
			roleID, codes); err != nil {
			t.Fatalf("seed role_permission: %v", err)
		}
	}
	passwordHash, err := testHasher().Hash(ctx, "Passw0rd123!")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, companyID, username, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	// The register auto-login used the default single session slot.
	if _, err := pool.Exec(ctx, "UPDATE core.company SET max_concurrent_sessions = 10 WHERE id = $1", companyID); err != nil {
		t.Fatalf("raise max_concurrent_sessions: %v", err)
	}
	rec := tenantLogin(t, router, companyID, username, "Passw0rd123!", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s expected 200, got %d: %s", username, rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return resp.Data.Token
}

// cfDo performs an authenticated tenant request and decodes the JSON body.
func cfDo(t *testing.T, router *gin.Engine, method, path, token, companyID, merchantID string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func newTestRouter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	return server.NewRouter(pool, newTestRedisClient(t, ctx), testConfig())
}

// soapFormData is the LITERAL spec §Bentuk nilai example (template v3,
// migration 000095; expectation values come from the spec, not from the
// code): NEWS2 = RR22(2)+nadi96(1)+suhu38.2(1)+SpO₂95 sk1(1)+SBP118(0)
// +respons a(0)+udara(0) = 5; GCS 4+5+6 = 15 → compos_mentis.
const soapFormData = `{"sections":{
  "anamnesis_info":{"jenis":"autoanamnesis"},
  "keluhan_utama":"Demam 3 hari, batuk berdahak.",
  "rps":"Demam 3 hari, batuk berdahak, memburuk saat malam.",
  "alergi":[{"pemicu":"Penisilin","jenis":"drug","reaksi":"Urtikaria","keparahan":"sedang","efek_samping":"","tanggal":"2019-03-01","sumber":"master"}],
  "tanda_vital":{"td_sistolik":118,"td_diastolik":76,"nafas":22,"nadi":96,"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","waktu_asesmen":"2026-11-06T08:15:00+07:00","kriteria":"rutin","news2_total":5},
  "antropometri":{"bb":62,"tb":158,"lk":null,"imt":24.8,"imt_kategori":"bb_lebih","var_tambahan":[{"variable":"","value":""}]},
  "pemeriksaan_fisik":{"keadaan_umum":"sedang","organ":{"kepala":{"status":"tidak_ada"},"thorax":{"status":"ada_kelainan","keterangan":"Ronki basa halus kiri","strokes":[{"points":[{"x":0.42,"y":0.31}],"color":"#D92D20","width":3,"label":"A"}]},"genital_urogenital":{"status":"tidak_ada"}}},
  "kesadaran":{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"kiri_mm":3,"kanan_mm":3,"bentuk":"bulat","refleks_kiri":"cepat","refleks_kanan":"cepat"},"pupil_score":0,"gcs_p":15,"respons":"a"},
  "diagnosa":{"narrative":"Suspek infeksi saluran napas atas",
    "diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"},"suspect":false}]},
  "rencana_tindak_lanjut":{"moda":"kontrol_poliklinik","target_tanggal":"2026-11-13","kriteria_rujukan_darurat":"Demam >3 hari, sesak berat"},
  "rekomendasi":{"teks":"Kontrol 7 hari, amoksisilin 500mg 3×1 habiskan","tanggal_kontrol":"2026-11-13"}}}`

func cfRawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

// TestClinicalFormTemplateSeed — 9 platform templates resolve and sections
// arrive as a JSON object (spec §9:178; gizi templates added by migration
// 000098, plan 2026-11-13-gizi-nutrition-care-chunk-2).
func TestClinicalFormTemplateSeed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "tpl")

	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/form-templates", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("form-templates = %d: %s", rec.Code, rec.Body.String())
	}
	data, _ := resp["data"].([]any)
	if len(data) != 11 {
		t.Fatalf("form-templates: got %d items, want 11", len(data))
	}
	var soap, asuhan, mnasf map[string]any
	for _, it := range data {
		item := it.(map[string]any)
		sections, ok := item["sections"].(map[string]any)
		if !ok {
			t.Fatalf("%s sections must decode as an object, got %T", item["form_type"], item["sections"])
		}
		defs, _ := sections["sections"].([]any)
		if len(defs) == 0 {
			t.Fatalf("%s has no section defs", item["form_type"])
		}
		switch item["form_type"] {
		case "soap_dokter":
			soap = item
		case "asuhan_gizi":
			asuhan = item
		case "ases_gizi_mnasf":
			mnasf = item
		}
	}
	if soap == nil {
		t.Fatal("soap_dokter template missing")
	}
	if soap["scope"] != "platform" || soap["category"] != "soap" {
		t.Fatalf("soap_dokter = %v", soap)
	}
	if v, _ := soap["version"].(float64); v != 5 {
		t.Fatalf("soap_dokter version = %v, want 5", soap["version"])
	}
	// soap_dokter = 22 sections (migration 000096; v5 000107 hanya patch
	// section nyeri — lokasi text_multi + field marks).
	defs, _ := soap["sections"].(map[string]any)["sections"].([]any)
	if len(defs) != 22 {
		t.Fatalf("soap_dokter sections = %d, want 22", len(defs))
	}
	kindOf := map[string]string{}
	for _, d := range defs {
		def := d.(map[string]any)
		key, _ := def["key"].(string)
		kind, _ := def["kind"].(string)
		kindOf[key] = kind
	}
	wantKind := map[string]string{
		"anamnesis_info": "radio_conditional", "tanda_vital": "vitals",
		"antropometri": "antropometri", "pemeriksaan_fisik": "physical_exam",
		"kesadaran": "consciousness", "nyeri": "repeatable",
		"alergi": "repeatable", "tindakan": "repeatable",
		"diagnosa": "diagnosis", "rencana_tindak_lanjut": "rtl",
		"rekomendasi": "rekomendasi",
	}
	for key, want := range wantKind {
		if got := kindOf[key]; got != want {
			t.Fatalf("soap_dokter v3 section %s kind = %q, want %q", key, got, want)
		}
	}

	// soap_dokter v5 (migration 000107) — section nyeri: lokasi type
	// text_multi + field marks (canvas_marks, figures a1|a2b); spec §3.
	for _, d := range defs {
		def := d.(map[string]any)
		if def["key"] != "nyeri" {
			continue
		}
		fields, _ := def["fields"].([]any)
		var lokasiType string
		var marks map[string]any
		for _, f := range fields {
			fld := f.(map[string]any)
			switch fld["key"] {
			case "lokasi":
				lokasiType, _ = fld["type"].(string)
			case "marks":
				marks = fld
			}
		}
		if lokasiType != "text_multi" {
			t.Fatalf("nyeri lokasi type = %q, want text_multi", lokasiType)
		}
		if marks == nil {
			t.Fatal("nyeri marks field missing")
		}
		if marks["type"] != "canvas_marks" {
			t.Fatalf("nyeri marks type = %v, want canvas_marks", marks["type"])
		}
		figs, _ := marks["figures"].([]any)
		if len(figs) != 2 || figs[0] != "a1" || figs[1] != "a2b" {
			t.Fatalf("nyeri marks figures = %v, want [a1 a2b]", figs)
		}
	}

	// asuhan_gizi (migration 000098; 000102 tambah diagnosis_terkait):
	// category gizi, 22 sections, key kinds.
	if asuhan == nil {
		t.Fatal("asuhan_gizi template missing")
	}
	if asuhan["scope"] != "platform" || asuhan["category"] != "gizi" {
		t.Fatalf("asuhan_gizi = %v", asuhan)
	}
	giziDefs, _ := asuhan["sections"].(map[string]any)["sections"].([]any)
	if len(giziDefs) != 22 {
		t.Fatalf("asuhan_gizi sections = %d, want 22", len(giziDefs))
	}
	giziKind := map[string]string{}
	for _, d := range giziDefs {
		def := d.(map[string]any)
		key, _ := def["key"].(string)
		kind, _ := def["kind"].(string)
		giziKind[key] = kind
	}
	for key, want := range map[string]string{
		"biokimia": "repeatable", "pes": "repeatable",
		"me_frekuensi": "select", "ttd": "repeatable",
		"diagnosis_terkait": "multiselect",
	} {
		if got := giziKind[key]; got != want {
			t.Fatalf("asuhan_gizi section %s kind = %q, want %q", key, got, want)
		}
	}

	// ases_gizi_mnasf (migration 000098): skor_mnasf + interpretasi_mnasf +
	// atribusi footer verbatim Nestlé (NFR-GZ-07).
	if mnasf == nil {
		t.Fatal("ases_gizi_mnasf template missing")
	}
	if mnasf["category"] != "gizi" {
		t.Fatalf("ases_gizi_mnasf = %v", mnasf)
	}
	mnasfDefs, _ := mnasf["sections"].(map[string]any)["sections"].([]any)
	mnasfKey := map[string]map[string]any{}
	for _, d := range mnasfDefs {
		def := d.(map[string]any)
		key, _ := def["key"].(string)
		mnasfKey[key] = def
	}
	for _, key := range []string{"mnasf_a", "mnasf_b", "mnasf_c", "mnasf_d", "mnasf_e", "mnasf_f", "skor_mnasf", "interpretasi_mnasf", "atribusi"} {
		if mnasfKey[key] == nil {
			t.Fatalf("ases_gizi_mnasf missing section %s", key)
		}
	}
	if k, _ := mnasfKey["skor_mnasf"]["kind"].(string); k != "number" {
		t.Fatalf("skor_mnasf kind = %q, want number", k)
	}
	if k, _ := mnasfKey["interpretasi_mnasf"]["kind"].(string); k != "select" {
		t.Fatalf("interpretasi_mnasf kind = %q, want select", k)
	}
	if label, _ := mnasfKey["atribusi"]["value"].(string); !strings.Contains(label, "Société des Produits Nestlé SA") {
		t.Fatalf("atribusi = %v, want Nestlé footer", mnasfKey["atribusi"])
	}
}

// TestClinicalFormDraftCreateUpdateDelete — full draft lifecycle; empty
// sections only warn and never block (user decision 7).
func TestClinicalFormDraftCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "crud")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "crud")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "soap_dokter", "form_data": cfRawJSON(t, soapFormData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)
	entry := data["entry"].(map[string]any)
	if entry["status"] != "draft" || entry["form_type"] != "soap_dokter" {
		t.Fatalf("entry = %v", entry)
	}
	if _, ok := data["warnings"].([]any); !ok {
		t.Fatalf("warnings must always be an array, got %v", data["warnings"])
	}
	id := entry["id"].(string)

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	got := resp["data"].(map[string]any)
	if addums, ok := got["addendums"].([]any); !ok || len(addums) != 0 {
		t.Fatalf("addendums = %v, want empty array", got["addendums"])
	}

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/forms?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 1 {
		t.Fatalf("list = %d %v", rec.Code, resp)
	}

	updated := `{"sections":{"keluhan_utama":"Demam 3 hari, membaik.","rps":"Demam membaik, batuk berkurang.","narasi_asesmen":"ISEL, membaik","rencana_tindak_lanjut":{"moda":"kontrol_poliklinik","target_tanggal":"2026-11-13","kriteria_rujukan_darurat":"Demam >3 hari, sesak berat"}}}`
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, map[string]any{
		"form_data": cfRawJSON(t, updated),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	patched := resp["data"].(map[string]any)["entry"].(map[string]any)["form_data"].(map[string]any)
	if patched["sections"].(map[string]any)["keluhan_utama"] != "Demam 3 hari, membaik." {
		t.Fatalf("patched form_data = %v", patched)
	}

	rec, _ = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/forms?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 0 {
		t.Fatalf("list after delete = %d %v", rec.Code, resp)
	}

	// Empty sections produce warnings but never block saving.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_adime", "form_data": map[string]any{"sections": map[string]any{}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create empty ases_gizi_adime = %d: %s", rec.Code, rec.Body.String())
	}
	warnings, _ := resp["data"].(map[string]any)["warnings"].([]any)
	var sawEmpty bool
	for _, w := range warnings {
		if w.(string) == "section 'assessment' kosong" {
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Fatalf("warnings = %v, want 'section assessment kosong'", warnings)
	}
}

// TestClinicalFormValidation — unknown key / wrong type / vitals range /
// MST consistency / unsupported kind → 400 (spec §5.2; example values are the
// LITERAL spec example).
func TestClinicalFormValidation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	// This test makes ~60 API calls with one token — lift the default burst
	// of 20 (pattern rbac_physician_cleanup_test.go / incident_test.go).
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 6000
	cfg.RateLimitAPIBurst = 1000
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), cfg)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "valid")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "valid")
	// v3_calc_antropometri asserts the imt_kategori mismatch (400), so the
	// usia gate (clinical_form_kinds.go, skip bila DOB kosong) must be
	// ACTIVE: give the fixture person an adult DOB (> 228 bulan; 2026-11 →
	// ±26 tahun).
	if _, err := pool.Exec(ctx, `UPDATE core.person SET birth_date = '2000-06-15' WHERE id = $1::uuid`, personID); err != nil {
		t.Fatalf("set fixture DOB: %v", err)
	}

	post := func(formType string, formData any) (*httptest.ResponseRecorder, map[string]any) {
		return cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": formType, "form_data": formData,
		})
	}

	// Happy path: the literal §Bentuk nilai example (template v3) is
	// accepted; only the shape of warnings is asserted, not emptiness.
	rec, resp := post("soap_dokter", cfRawJSON(t, soapFormData))
	if rec.Code != http.StatusCreated {
		t.Fatalf("happy soap = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := resp["data"].(map[string]any)["warnings"].([]any); !ok {
		t.Fatalf("happy soap warnings must be an array, got %v", resp["data"])
	}

	sections := func(pairs ...string) json.RawMessage {
		s := `{"sections":{`
		for i, p := range pairs {
			if i > 0 {
				s += ","
			}
			s += p
		}
		return json.RawMessage(s + "}}")
	}

	// Unknown section key.
	rec, resp = post("soap_dokter", sections(`"keluhan_utama":"ada"`, `"halo":"x"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "unknown section key: halo" {
		t.Fatalf("unknown key = %d %v", rec.Code, resp)
	}
	// Wrong type per kind (textarea given a number).
	rec, resp = post("soap_dokter", sections(`"rps":123`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'rps'" {
		t.Fatalf("wrong type = %d %v", rec.Code, resp)
	}
	// Vitals outside (0,1000).
	rec, resp = post("ases_awal_perawat", sections(`"tanda_vital":{"sistolic":5000}`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'tanda_vital'" {
		t.Fatalf("vitals range = %d %v", rec.Code, resp)
	}
	// pain_score 0 valid (NRS 0–10 inklusif, review A4).
	rec, _ = post("ases_awal_perawat", sections(`"tanda_vital":{"sistolic":120,"pain_score":0}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pain_score 0 = %d: %s", rec.Code, rec.Body.String())
	}
	// pain_score 11 ditolak.
	rec, resp = post("ases_awal_perawat", sections(`"tanda_vital":{"sistolic":120,"pain_score":11}`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'tanda_vital'" {
		t.Fatalf("pain_score 11 = %d %v", rec.Code, resp)
	}
	// diagnosa narrative-only (tanpa diagnoses) → draft tersimpan
	// (soft_required, review A3).
	rec, _ = post("soap_dokter", sections(`"diagnosa":{"narrative":"Hanya naratif klinis."}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("narrative-only assessment = %d: %s", rec.Code, rec.Body.String())
	}
	// MST: skor_mst ≠ jumlah jawaban (1+1+0 = 2, skor ditulis 1).
	rec, resp = post("ases_gizi_skrining", sections(
		`"mst_penurunan_bb":"1"`, `"mst_nafsu_makan":"1"`, `"mst_sakit_akut":"0"`,
		`"skor_mst":1`, `"risiko_gizi":"berisiko"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "skor MST tidak konsisten" {
		t.Fatalf("mst skor = %d %v", rec.Code, resp)
	}
	// MST: risiko_gizi tidak sesuai skor < 2.
	rec, resp = post("ases_gizi_skrining", sections(
		`"mst_penurunan_bb":"0"`, `"mst_nafsu_makan":"0"`, `"mst_sakit_akut":"0"`,
		`"skor_mst":0`, `"risiko_gizi":"berisiko"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "skor MST tidak konsisten" {
		t.Fatalf("mst risiko = %d %v", rec.Code, resp)
	}
	// MST konsisten → 201.
	rec, _ = post("ases_gizi_skrining", sections(
		`"mst_penurunan_bb":"1"`, `"mst_nafsu_makan":"1"`, `"mst_sakit_akut":"0"`,
		`"skor_mst":2`, `"risiko_gizi":"berisiko"`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mst konsisten = %d: %s", rec.Code, rec.Body.String())
	}
	// MST skor 5 (opsi 0–5, regresi plan 1): 2+1+2 = 5 konsisten → 201.
	rec, _ = post("ases_gizi_skrining", sections(
		`"mst_penurunan_bb":"2"`, `"mst_nafsu_makan":"1"`, `"mst_sakit_akut":"2"`,
		`"skor_mst":5`, `"risiko_gizi":"berisiko"`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mst skor 5 = %d: %s", rec.Code, rec.Body.String())
	}

	// ── asuhan_gizi PES NI + multiselect (plan 2026-11-15-gizi-dewasa-
	// 4-adime-pes Task 2, migration 000102) ──
	// problem = kode NI pertama Lampiran A → diterima.
	rec, _ = post("asuhan_gizi", sections(`"pes":[{"problem":"NI-1.1"}]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pes NI-1.1 = %d: %s", rec.Code, rec.Body.String())
	}
	// Kode GD- legacy ditolak (enum berganti ke NI, value set Lampiran A).
	rec, resp = post("asuhan_gizi", sections(`"pes":[{"problem":"GD-01"}]`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'pes'" {
		t.Fatalf("pes GD-01 = %d %v", rec.Code, resp)
	}
	// diagnosis_terkait multiselect: options [] → kode episode bebas.
	rec, _ = post("asuhan_gizi", sections(`"diagnosis_terkait":["E11.9","I10"]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("diagnosis_terkait array = %d: %s", rec.Code, rec.Body.String())
	}
	// Bukan array → ditolak.
	rec, resp = post("asuhan_gizi", sections(`"diagnosis_terkait":"E11.9"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'diagnosis_terkait'" {
		t.Fatalf("diagnosis_terkait string = %d %v", rec.Code, resp)
	}
	// me_reassessment multiselect (migration 000103): FE DlgJadwal menulis
	// array → diterima; string tunggal ditolak.
	rec, _ = post("asuhan_gizi", sections(`"me_reassessment":["ranap_h3","saat_pulang"]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("me_reassessment array = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp = post("asuhan_gizi", sections(`"me_reassessment":"ranap_h3"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'me_reassessment'" {
		t.Fatalf("me_reassessment string = %d %v", rec.Code, resp)
	}
	// Unsupported kind in template data → 400 (merchant sees the platform row).
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.template (kind, code, name, scope, usage) VALUES ('clinical_form','cf_bad_form','Bad Form','platform','insert')`); err != nil {
		t.Fatalf("seed bad template: %v", err)
	}
	var badTemplateID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM core.template WHERE code = 'cf_bad_form'`).Scan(&badTemplateID); err != nil {
		t.Fatalf("resolve bad template: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
		 VALUES ($1::uuid, 'cf_bad_form', 'soap', '{"sections":[{"key":"x","label":"X","kind":"widget"}]}')`,
		badTemplateID); err != nil {
		t.Fatalf("seed bad sections: %v", err)
	}
	rec, resp = post("cf_bad_form", sections(`"x":"hi"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "unsupported section kind: widget" {
		t.Fatalf("unsupported kind = %d %v", rec.Code, resp)
	}

	// ── Kinds repeatable/calc + suspect (plan 2a Task 2, spec §5/§6) ──
	// soapV3FormData = LITERAL spec §Bentuk nilai example (migration 000095
	// template v3). Expectations computed from the spec: pupil
	// lambat(1)+cepat(0) → pupil_score 1 → gcs_p 14; imt = round(62/(158/100)²,1)
	// = round(24.8357,1) = 24.8 → bb_lebih; NEWS2 = RR18(0)+nadi84(0)+
	// suhu37.6(0)+SpO₂98 sk1(0)+SBP120(0)+udara(0)+respons a(0) = 0.
	const soapV3FormData = `{"sections":{
	  "anamnesis_info":{"jenis":"alloanamnesis","informan":{"nama":"Bapak Sutoyo","hubungan":"orang_tua","alasan":"pasien anak"},"keandalan":"andal"},
	  "keluhan_utama":"Batuk berdahak 5 hari, demam sejak 2 hari lalu",
	  "rps":"Batuk berdahak kuning, memburuk saat malam. Demam 38-39°C turun-naik.",
	  "alergi":[{"pemicu":"amoksisilin","jenis":"drug","reaksi":"Biduran dan gatal","keparahan":"sedang","efek_samping":"","tanggal":"2024-03-10","sumber":"catatan"}],
	  "tanda_vital":{"td_sistolik":120,"td_diastolik":80,"nafas":18,"nadi":84,"suhu":37.6,"spo2":98,"spo2_scale":1,"oksigen":"udara_ruang","waktu_asesmen":"2026-11-06T08:15:00+07:00","kriteria":"rutin","news2_total":0},
	  "antropometri":{"bb":62,"tb":158,"lk":null,"imt":24.8,"imt_kategori":"bb_lebih","var_tambahan":[{"variable":"","value":""}]},
	  "kesadaran":{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"kiri_mm":3,"kanan_mm":3,"bentuk":"bulat","refleks_kiri":"lambat","refleks_kanan":"cepat"},"pupil_score":1,"gcs_p":14,"respons":"a"},
	  "nyeri":[{"skala":"nrs","skor":3,"lokasi":"Regio pectoralis","durasi":"3 hari","frekuensi":"ireguler","karakteristik":"tertusuk","berkurang":"istirahat","penyebab":"batuk","lama":"<1 minggu","poss":"1"}],
	  "diagnosa":{"narrative":"Suspek infeksi saluran napas atas",
	    "diagnoses":[
	      {"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"},"suspect":false},
	      {"role":"secondary","concept":{"system":"ICD-11","code":"DA0A.0","display":"Dental caries"},"suspect":true}]},
	  "tindakan":[{"kode":"96.72","nama":"Irrigasi kateter vesika urinaria","tarif_snapshot":150000,"waktu":"09:30","dokter":"dr. X","cito":false,"catatan_hasil":"Keluar kateter F16","lokasi_unit":"Poli tindakan","petugas_mengorder":"sdr. Rina"}],
	  "rencana_tindak_lanjut":{"moda":"kontrol_poliklinik","target_tanggal":"2026-11-13","kriteria_rujukan_darurat":"Demam >3 hari, sesak berat"},
	  "rekomendasi":{"teks":"Kontrol 3 hari jika demam >38.5°C atau batuk memburuk","tanggal_kontrol":"2026-11-13"}}}`

	t.Run("v3 example accepted", func(t *testing.T) {
		// (a) full spec §Bentuk nilai example → 201.
		if rec, _ := post("soap_dokter", cfRawJSON(t, soapV3FormData)); rec.Code != http.StatusCreated {
			t.Fatalf("spec §Bentuk nilai example = %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("calc gcs", func(t *testing.T) {
		// (b) 4+5+6 = 15, ditulis 14 → 400.
		rec, resp := post("soap_dokter", sections(
			`"kesadaran":{"e":"4","v":"5","m":"6","gcs":14}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi gcs tidak konsisten" {
			t.Fatalf("gcs mismatch = %d %v", rec.Code, resp)
		}
		// (c) "T" + gcs/kategori/gcs_p null → lolos (T dinilai konsisten).
		rec, _ = post("soap_dokter", sections(`"kesadaran":{"v":"T","gcs":null,"kategori":null,"pupil_score":null,"gcs_p":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs T = %d: %s", rec.Code, rec.Body.String())
		}
		// (d) "T" + gcs numerik → 400 (bentuk).
		rec, resp = post("soap_dokter", sections(`"kesadaran":{"v":"T","gcs":15}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'kesadaran'" {
			t.Fatalf("gcs T numeric = %d %v", rec.Code, resp)
		}
	})
	t.Run("kesadaran empty string optional", func(t *testing.T) {
		// (l) "" dari clear combobox FE (legacy) = kosong untuk field
		// opsional kesadaran (keputusan user 2026-11-21): e/v/m "" +
		// refleks_kiri "" + lainnya sah → 201.
		rec, _ = post("soap_dokter", sections(`"kesadaran":{"e":"","v":"","m":"6","pupil":{"kiri_mm":3,"kanan_mm":3,"refleks_kiri":"","refleks_kanan":"cepat"},"kategori":"","respons":""}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("kesadaran empty-string = %d: %s", rec.Code, rec.Body.String())
		}
		// Aturan lama utuh: "t" lowercase tetap 400.
		rec, resp = post("soap_dokter", sections(`"kesadaran":{"v":"t"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'kesadaran'" {
			t.Fatalf("kesadaran v lowercase t = %d %v", rec.Code, resp)
		}
	})
	t.Run("calc imt", func(t *testing.T) {
		// (e) BB 62 / TB 158 → imt seharusnya 24.8; ditulis 30 → 400.
		rec, resp := post("soap_dokter", sections(
			`"antropometri":{"bb":62,"tb":158,"imt":30,"imt_kategori":"bb_lebih"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt mismatch = %d %v", rec.Code, resp)
		}
		rec, _ = post("soap_dokter", sections(
			`"antropometri":{"bb":62,"tb":158,"imt":24.8,"imt_kategori":"bb_lebih"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("imt ok = %d: %s", rec.Code, rec.Body.String())
		}
		// (f) imt terisi, bb kosong (parsial) → 400.
		rec, resp = post("soap_dokter", sections(
			`"antropometri":{"tb":158,"imt":24.8}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt partial filled = %d %v", rec.Code, resp)
		}
	})
	t.Run("repeatable alergi", func(t *testing.T) {
		// (g) field tak dikenal dalam row → 400.
		rec, resp := post("soap_dokter", sections(`"alergi":[{"pemicu":"depan","foo":"x"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "unknown field 'foo' di section 'alergi'" {
			t.Fatalf("unknown field = %d %v", rec.Code, resp)
		}
		// (h) select di luar options → 400.
		rec, resp = post("soap_dokter", sections(`"alergi":[{"jenis":"zat"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'alergi'" {
			t.Fatalf("jenis = %d %v", rec.Code, resp)
		}
		// (i) tanggal valid → lolos; format salah → 400.
		rec, _ = post("soap_dokter", sections(`"alergi":[{"tanggal":"2024-03-10"}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("tanggal ok = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("soap_dokter", sections(`"alergi":[{"tanggal":"10/03/2024"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'alergi'" {
			t.Fatalf("tanggal bad = %d %v", rec.Code, resp)
		}
		// (k) waktu kejadian HH:MM (template v4, 000096): valid → lolos;
		// di luar rentang → 400 (clinicalTimeRe).
		rec, _ = post("soap_dokter", sections(`"alergi":[{"tanggal":"2024-03-10","waktu":"14:30"}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("waktu ok = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("soap_dokter", sections(`"alergi":[{"waktu":"25:00"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'alergi'" {
			t.Fatalf("waktu bad = %d %v", rec.Code, resp)
		}
	})
	t.Run("suspect", func(t *testing.T) {
		// (j) REMEDIAL: shape/suspect divalidasi saat sign, bukan create.
		// Keduanya pakai entri valid literal withMap (pattern
		// clinical_diagnosis_test.go:180 — merchant default ICD-10 butuh
		// mapping icd10) plus "suspect".
		const withMapBad = `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"},"suspect":"ya"}]}}}`
		const withMapOK = `{"sections":{"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"},"suspect":true}]}}}`
		cdSeedICD11(t, ctx, pool)
		// suspect non-boolean → create 201 (draft lolos), sign 400,
		// entri tetap draft (rollback RunRequestTx).
		id := cfCreateDraft(t, router, token, companyID, merchantID, admissionID, withMapBad)
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
			map[string]any{"password": ownerPassword})
		if rec.Code != http.StatusBadRequest || resp["error"] != "suspect harus boolean" {
			t.Fatalf("suspect string sign = %d %v", rec.Code, resp)
		}
		if row := cfGetEntryRow(t, ctx, pool, id); row.Status != "draft" {
			t.Fatalf("suspect string: status = %s, want draft (rollback)", row.Status)
		}
		// suspect boolean → create 201 + sign 200.
		id = cfCreateDraft(t, router, token, companyID, merchantID, admissionID, withMapOK)
		if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
			map[string]any{"password": ownerPassword}); rec.Code != http.StatusOK {
			t.Fatalf("suspect bool sign = %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("diagnosis malformed shape create ok", func(t *testing.T) {
		// (n) guard regresi: diagnoses ["ISPA"] (item bukan objek) tidak
		// ditolak saat create; penolakan 400 invalid diagnosis entry saat
		// sign; entri tetap draft (semantik fase 1). REMEDIAL: guard TTE
		// sign wajib lolos dulu — entri primary valid (mapping icd10, pola
		// clinical_diagnosis_test.go:154) + keluhan_utama + jenis terisi,
		// sehingga guard gagal justru di shape entri, bukan wajib kosong.
		cdSeedICD11(t, ctx, pool)
		rec, resp := post("soap_dokter", sections(`"anamnesis_info":{"jenis":"autoanamnesis"},"keluhan_utama":"Demam 3 hari.","diagnosa":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"}},"ISPA"]}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("malformed create = %d: %s", rec.Code, rec.Body.String())
		}
		id := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
		rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms/"+id+"/sign", token, companyID, merchantID,
			map[string]any{"password": ownerPassword})
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid diagnosis entry" {
			t.Fatalf("malformed sign = %d %v", rec.Code, resp)
		}
		if row := cfGetEntryRow(t, ctx, pool, id); row.Status != "draft" {
			t.Fatalf("malformed: status = %s, want draft (rollback)", row.Status)
		}
	})
	t.Run("old v1/v2 shape rejected on v3 template", func(t *testing.T) {
		// (k) key v1/v2 pada template v3 → 400 unknown section key
		// (perilaku baru resolve-aktif, migration 000095).
		rec, resp := post("soap_dokter", sections(`"subjective":"lama"`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "unknown section key: subjective" {
			t.Fatalf("v1 key = %d %v", rec.Code, resp)
		}
		rec, resp = post("soap_dokter", sections(`"gcs_eye":"4"`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "unknown section key: gcs_eye" {
			t.Fatalf("v2 gcs key = %d %v", rec.Code, resp)
		}
	})
	t.Run("calc partial inputs", func(t *testing.T) {
		// (l) PARSIAL-LOLOS: satu input, calc absen/null → draft sah.
		rec, _ := post("soap_dokter", sections(`"kesadaran":{"e":"4","gcs":null,"kategori":null,"pupil_score":null,"gcs_p":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs partial ok = %d: %s", rec.Code, rec.Body.String())
		}
		rec, _ = post("soap_dokter", sections(`"antropometri":{"bb":62,"imt":null,"imt_kategori":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("imt partial ok = %d: %s", rec.Code, rec.Body.String())
		}
		// (m) PARSIAL-TERISI: calc terisi padahal input belum lengkap → 400.
		rec, resp := post("soap_dokter", sections(`"kesadaran":{"e":"4","gcs":4}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi gcs tidak konsisten" {
			t.Fatalf("gcs partial filled = %d %v", rec.Code, resp)
		}
		rec, resp = post("soap_dokter", sections(`"antropometri":{"bb":62,"imt":24.8}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt partial filled = %d %v", rec.Code, resp)
		}
	})

	// ── SOAP v3 kinds (plan 2026-11-10-cppt-pemeriksaan-v3-1-be Task 1) ──
	// Synthetic v3 defs (spec 2026-11-10-cppt-pemeriksaan-v3 §Kontrak data)
	// seeded as a platform template and exercised through Create so
	// validateClinicalFormData runs against the new kinds. The v3 template
	// migration (plan Task 3) does not exist yet, so a synthetic one is the
	// only way to reach the new dispatch from package server_test (the
	// validator itself is unexported).
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.template (kind, code, name, scope, usage) VALUES ('clinical_form','cf_v3_form','V3 Form','platform','insert')`); err != nil {
		t.Fatalf("seed v3 template: %v", err)
	}
	var v3TemplateID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM core.template WHERE code = 'cf_v3_form'`).Scan(&v3TemplateID); err != nil {
		t.Fatalf("resolve v3 template: %v", err)
	}
	const cfV3Sections = `{"sections":[
	  {"key":"anamnesis_info","label":"Informasi Anamnesis","kind":"radio_conditional",
	   "options":[{"value":"autoanamnesis","label":"Autoanamnesis"},{"value":"alloanamnesis","label":"Alloanamnesis"}]},
	  {"key":"antropometri","label":"Antropometri","kind":"antropometri"},
	  {"key":"pemeriksaan_fisik","label":"Pemeriksaan Fisik","kind":"physical_exam"},
	  {"key":"kesadaran","label":"Kesadaran","kind":"consciousness"},
	  {"key":"rencana_tindak_lanjut","label":"Rencana Tindak Lanjut","kind":"rtl"},
	  {"key":"rekomendasi","label":"Rekomendasi","kind":"rekomendasi"},
	  {"key":"tanda_vital","label":"Tanda Vital","kind":"vitals"},
	  {"key":"tindakan","label":"Tindakan","kind":"repeatable","fields":[
	    {"key":"concept_id","label":"Konsep","type":"text"},
	    {"key":"waktu","label":"Waktu","type":"time"},
	    {"key":"tarif_snapshot","label":"Tarif","type":"number"},
	    {"key":"cito","label":"Cito","type":"boolean"},
	    {"key":"dokter","label":"Dokter","type":"text"},
	    {"key":"tanggal","label":"Tanggal","type":"date"}]}
	]}`
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
		 VALUES ($1::uuid, 'cf_v3_form', 'soap', $2)`, v3TemplateID, cfV3Sections); err != nil {
		t.Fatalf("seed v3 sections: %v", err)
	}

	// 513 points / 51 strokes for the guard limits (spec: ≤512 titik/stroke,
	// ≤50 stroke/organ) — expectations computed from the spec limits.
	manyPoints := `[{"points":[` + strings.Repeat(`{"x":0.1,"y":0.1},`, 513) + `{"x":0.2,"y":0.2}],"color":"#D92D20","width":3}]`
	manyStrokes := `[` + strings.Repeat(`{"points":[{"x":0.1,"y":0.1}],"color":"#D92D20","width":3},`, 51) + `{"points":[{"x":0.2,"y":0.2}],"color":"#D92D20","width":3}]`

	v3Cases := []struct{ name, section, value, wantErr string }{
		// Valid — LITERAL spec §Bentuk nilai examples.
		{"radio allo valid", "anamnesis_info",
			`{"jenis":"alloanamnesis","informan":{"nama":"Bapak Sutoyo","hubungan":"orang_tua","alasan":"pasien anak"},"keandalan":"andal"}`, ""},
		{"radio auto valid", "anamnesis_info", `{"jenis":"autoanamnesis"}`, ""},
		{"antropometri valid", "antropometri",
			`{"bb":62,"tb":158,"lk":null,"imt":24.8,"imt_kategori":"bb_lebih","var_tambahan":[{"variable":"","value":""}]}`, ""},
		{"physical exam valid", "pemeriksaan_fisik",
			`{"keadaan_umum":"sedang","organ":{"kepala":{"status":"tidak_ada"},"thorax":{"status":"ada_kelainan","keterangan":"Ronki basa halus kiri","strokes":[{"points":[{"x":0.42,"y":0.31}],"color":"#D92D20","width":3,"label":"A"}]},"genital_urogenital":{"status":"tidak_ada"}}}`, ""},
		{"consciousness valid", "kesadaran",
			`{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"kiri_mm":3,"kanan_mm":3,"bentuk":"bulat","refleks_kiri":"cepat","refleks_kanan":"cepat"},"pupil_score":0,"gcs_p":15,"respons":"a"}`, ""},
		{"rtl valid", "rencana_tindak_lanjut",
			`{"moda":"kontrol_poliklinik","target_tanggal":"2026-11-13","kriteria_rujukan_darurat":"Demam >3 hari, sesak berat"}`, ""},
		{"rekomendasi valid", "rekomendasi",
			`{"teks":"Kontrol 7 hari, amoksisilin 500mg 3×1 habiskan","tanggal_kontrol":"2026-11-13"}`, ""},
		{"tanda vital valid", "tanda_vital",
			// news2_total harus null tanpa kesadaran.respons (calc parsial —
			// plan Task 2); literal contoh lengkap (news2 5) di sub-test
			// "v3 calc news2" di bawah.
			`{"td_sistolik":118,"td_diastolik":76,"nafas":22,"nadi":96,"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","waktu_asesmen":"2026-11-06T08:15:00+07:00","kriteria":"rutin","news2_total":null}`, ""},
		{"tindakan row valid", "tindakan",
			`[{"waktu":"09:30","tarif_snapshot":150000,"cito":false,"dokter":"dr. X","tanggal":"2026-11-13"}]`, ""},
		// Invalid — enum/bentuk/rentang violations.
		{"radio jenis salah", "anamnesis_info", `{"jenis":"wawancara"}`,
			"invalid value for section 'anamnesis_info'"},
		{"radio allo tanpa informan", "anamnesis_info", `{"jenis":"alloanamnesis"}`,
			"informan wajib diisi saat alloanamnesis"},
		{"radio allo informan kosong", "anamnesis_info",
			`{"jenis":"alloanamnesis","informan":{"nama":"","hubungan":"orang_tua"}}`,
			"informan wajib diisi saat alloanamnesis"},
		{"radio key asing", "anamnesis_info", `{"jenis":"autoanamnesis","foo":1}`,
			"invalid value for section 'anamnesis_info'"},
		{"antropometri bb 1000", "antropometri", `{"bb":1000,"tb":158}`,
			"invalid value for section 'antropometri'"},
		{"antropometri kategori salah", "antropometri", `{"bb":62,"tb":158,"imt_kategori":"gemuk"}`,
			"invalid value for section 'antropometri'"},
		{"physical organ asing", "pemeriksaan_fisik", `{"organ":{"kaki":{"status":"tidak_ada"}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"physical status salah", "pemeriksaan_fisik", `{"organ":{"kepala":{"status":"normal"}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"physical keadaan umum salah", "pemeriksaan_fisik", `{"keadaan_umum":"sehat"}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"stroke koordinat >1", "pemeriksaan_fisik",
			`{"organ":{"thorax":{"status":"ada_kelainan","strokes":[{"points":[{"x":1.5,"y":0.1}],"color":"#D92D20","width":3}]}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"stroke width 6", "pemeriksaan_fisik",
			`{"organ":{"thorax":{"status":"ada_kelainan","strokes":[{"points":[{"x":0.1,"y":0.1}],"color":"#D92D20","width":6}]}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"stroke label C", "pemeriksaan_fisik",
			`{"organ":{"thorax":{"status":"ada_kelainan","strokes":[{"points":[{"x":0.1,"y":0.1}],"color":"#D92D20","width":3,"label":"C"}]}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"stroke >512 titik", "pemeriksaan_fisik",
			`{"organ":{"thorax":{"status":"ada_kelainan","strokes":` + manyPoints + `}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"stroke >50 per organ", "pemeriksaan_fisik",
			`{"organ":{"thorax":{"status":"ada_kelainan","strokes":` + manyStrokes + `}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"strokes di genital", "pemeriksaan_fisik",
			`{"organ":{"genital_urogenital":{"status":"tidak_ada","strokes":[]}}}`,
			"invalid value for section 'pemeriksaan_fisik'"},
		{"consciousness t kecil", "kesadaran", `{"e":"t"}`,
			"invalid value for section 'kesadaran'"},
		{"consciousness T gcs terisi", "kesadaran", `{"e":"T","gcs":15,"kategori":null,"gcs_p":null}`,
			"invalid value for section 'kesadaran'"},
		{"consciousness pupil mm 10", "kesadaran",
			`{"e":"4","v":"5","m":"6","pupil":{"kiri_mm":10,"kanan_mm":3,"refleks_kiri":"cepat","refleks_kanan":"cepat"}}`,
			"invalid value for section 'kesadaran'"},
		{"consciousness refleks salah", "kesadaran",
			`{"e":"4","v":"5","m":"6","pupil":{"kiri_mm":3,"kanan_mm":3,"refleks_kiri":"cepat","refleks_kanan":"normal"}}`,
			"invalid value for section 'kesadaran'"},
		{"rtl moda salah", "rencana_tindak_lanjut", `{"moda":"kontrol_rumah"}`,
			"invalid value for section 'rencana_tindak_lanjut'"},
		{"rtl tanggal salah", "rencana_tindak_lanjut",
			`{"moda":"kontrol_poliklinik","target_tanggal":"13/11/2026"}`,
			"invalid value for section 'rencana_tindak_lanjut'"},
		{"rekomendasi tanggal salah", "rekomendasi", `{"teks":"x","tanggal_kontrol":"besok"}`,
			"invalid value for section 'rekomendasi'"},
		{"tanda vital waktu bukan RFC3339", "tanda_vital",
			`{"td_sistolik":118,"waktu_asesmen":"2026-11-06 08:15:00"}`,
			"invalid value for section 'tanda_vital'"},
		{"tanda vital spo2 101", "tanda_vital", `{"spo2":101}`,
			"invalid value for section 'tanda_vital'"},
		{"tanda vital kriteria salah", "tanda_vital", `{"kriteria":"lain"}`,
			"invalid value for section 'tanda_vital'"},
		{"tanda vital news2 21", "tanda_vital", `{"news2_total":21}`,
			"invalid value for section 'tanda_vital'"},
		{"tindakan waktu salah", "tindakan", `[{"waktu":"24:00"}]`,
			"invalid value for section 'tindakan'"},
		{"tindakan tarif string", "tindakan", `[{"tarif_snapshot":"150000"}]`,
			"invalid value for section 'tindakan'"},
		{"tindakan cito string", "tindakan", `[{"cito":"ya"}]`,
			"invalid value for section 'tindakan'"},
	}
	for _, tc := range v3Cases {
		t.Run("v3 "+tc.name, func(t *testing.T) {
			rec, resp := post("cf_v3_form", sections(`"`+tc.section+`":`+tc.value))
			if tc.wantErr == "" {
				if rec.Code != http.StatusCreated {
					t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest || resp["error"] != tc.wantErr {
				t.Fatalf("want 400 %q, got %d %v", tc.wantErr, rec.Code, resp)
			}
		})
	}

	// ── v5 marks/pins/legend + nyeri text_multi/canvas_marks (plan
	// 2026-11-20-cppt-o-1-be-validator Task 4, spec 2026-11-20 §2/§3.1/§3.2) ──
	// Expectations computed from the contract: pin n int 1..9, UNIK per
	// organ lintas figur, ≤9 pin total per organ; legend key "1".."9";
	// figureId `^[a-z0-9][a-z0-9-]*$`; genital marks tunduk aturan sama
	// (revisi 2026-11-21); nyeri marks
	// whitelist ["a1","a2b"] dari template v5 (migration 000107).
	t.Run("v5 marks pins legend", func(t *testing.T) {
		// (a) marks valid multi-figur + pins lintas figur + legend → 201.
		rec, _ = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"keadaan_umum":"sedang","organ":{"thorax":{"status":"ada_kelainan",`+
			`"marks":{"pf-16":{"strokes":[{"points":[{"x":0.31,"y":0.42}],"color":"#dc2626","width":3,"label":null}],"pins":[{"x":0.4,"y":0.3,"n":1}]},"pf-26-belakang":{"pins":[{"x":0.5,"y":0.5,"n":2}]}},`+
			`"legend":{"1":"Ronki basa halus kiri"}}}}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("marks valid = %d: %s", rec.Code, rec.Body.String())
		}
		// (b) pin ke-10 lintas figur → 400 "too many pins" (≤9 per organ).
		// Figur dikunjungi deterministik by figureId sort (plan 2026-11-21
		// Task 4): "pf-16" < "pf-26-belakang", jadi 9 pin pf-16 divalidasi
		// lebih dulu. n=1 di pin ke-10 (nomor ganda) membuktikan urutan
		// guard: cek pinTotal > 9 berjalan SEBELUM duplicate, jadi 400-nya
		// "too many pins", bukan "duplicate pin number".
		pins := make([]string, 9)
		for i := range pins {
			pins[i] = fmt.Sprintf(`{"x":0.1,"y":0.1,"n":%d}`, i+1)
		}
		rec, resp = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"thorax":{"status":"ada_kelainan","marks":{`+
			`"pf-16":{"pins":[`+strings.Join(pins, ",")+`]},"pf-26-belakang":{"pins":[{"x":0.2,"y":0.2,"n":1}]}}}}}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "too many pins" {
			t.Fatalf("pin ke-10 = %d %v", rec.Code, resp)
		}
		// (c) nomor pin ganda lintas figur → 400 "duplicate pin number".
		rec, resp = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"thorax":{"status":"ada_kelainan","marks":{`+
			`"pf-16":{"pins":[{"x":0.4,"y":0.3,"n":1}]},"pf-26-belakang":{"pins":[{"x":0.5,"y":0.5,"n":1}]}}}}}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "duplicate pin number" {
			t.Fatalf("pin ganda = %d %v", rec.Code, resp)
		}
		// (d) marks di genital DIVALIDASI SAMA seperti organ lain (keputusan
		// user 2026-11-21: larangan dihapus): figureId grup gender pasien
		// (pria pf-20-depan) → 201; figureId asing → 400 "invalid figure id".
		rec, _ = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"genital_urogenital":{"status":"tidak_ada","marks":{"pf-20-depan":{"pins":[{"x":0.5,"y":0.5,"n":1}]}}}}}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("marks genital valid = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"genital_urogenital":{"status":"tidak_ada","marks":{"XX":{"pins":[{"x":0.5,"y":0.5,"n":1}]}}}}}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid figure id" {
			t.Fatalf("marks genital figureId asing = %d %v", rec.Code, resp)
		}
		// (e) figureId asing (huruf besar) → 400 "invalid figure id".
		rec, resp = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"thorax":{"status":"ada_kelainan","marks":{"XX":{"pins":[{"x":0.5,"y":0.5,"n":1}]}}}}}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid figure id" {
			t.Fatalf("figureId asing = %d %v", rec.Code, resp)
		}
		// (f) legend key "10" di luar "1".."9" → 400 "invalid legend key".
		rec, resp = post("cf_v3_form", sections(`"pemeriksaan_fisik":{"organ":{"thorax":{"status":"ada_kelainan","legend":{"10":"dua angka"}}}}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid legend key" {
			t.Fatalf("legend key 10 = %d %v", rec.Code, resp)
		}
		// (g) nyeri lokasi array 2 region → 201 (text_multi union, spec §2).
		rec, _ = post("soap_dokter", sections(`"nyeri":[{"skala":"nrs","skor":3,"lokasi":["Regio frontalis","Regio epigastrium"],`+
			`"durasi":"3 hari","frekuensi":"intermiten","karakteristik":"tertusuk","berkurang":"istirahat","penyebab":"batuk","lama":"<1 minggu","poss":"1"}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("nyeri lokasi array = %d: %s", rec.Code, rec.Body.String())
		}
		// (h) nyeri marks a1 + a2b (whitelist template v5) → 201; key "a3"
		// di luar whitelist → 400 "unknown figure id a3".
		rec, _ = post("soap_dokter", sections(`"nyeri":[{"skala":"nrs","skor":3,"lokasi":["Regio frontalis"],`+
			`"durasi":"3 hari","frekuensi":"intermiten","karakteristik":"tertusuk","berkurang":"istirahat","penyebab":"batuk","lama":"<1 minggu","poss":"1",`+
			`"marks":{"a1":[{"points":[{"x":0.5,"y":0.3}],"color":"#dc2626","width":3,"label":null}],"a2b":[{"points":[{"x":0.6,"y":0.4}],"color":"#dc2626","width":3,"label":null}]}}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("nyeri marks a1+a2b = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("soap_dokter", sections(`"nyeri":[{"skala":"nrs","skor":3,"lokasi":"Regio frontalis",`+
			`"durasi":"3 hari","frekuensi":"intermiten","karakteristik":"tertusuk","berkurang":"istirahat","penyebab":"batuk","lama":"<1 minggu","poss":"1",`+
			`"marks":{"a3":[{"points":[{"x":0.5,"y":0.3}],"color":"#dc2626","width":3,"label":null}]}}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "unknown figure id a3" {
			t.Fatalf("nyeri marks a3 = %d %v", rec.Code, resp)
		}
	})

	// ── v3 calc consistency (plan Task 2, spec §Validasi server + §NEWS2) ──
	// Expectations computed from the contract definitions: imt =
	// round(62/(158/100)²,1) = round(24.8357,1) = 24.8 → bb_lebih (23–24.9);
	// gcs = 4+5+6 = 15 → compos_mentis; refleks lambat(1)+cepat(0) →
	// pupil_score 1 → gcs_p = 15−1 = 14; NEWS2: RR22(+2)+nadi96(+1)+
	// suhu38.2(+1)+SpO₂ 95 sk1(+1)+SBP118(0)+Alert(0)+udara(0) = 5.
	t.Run("v3 calc antropometri", func(t *testing.T) {
		rec, _ = post("cf_v3_form", sections(`"antropometri":{"bb":62,"tb":158,"imt":24.8,"imt_kategori":"bb_lebih"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("antro spec example = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("cf_v3_form", sections(`"antropometri":{"bb":62,"tb":158,"imt":24.9,"imt_kategori":"bb_lebih"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt mismatch = %d %v", rec.Code, resp)
		}
		rec, resp = post("cf_v3_form", sections(`"antropometri":{"bb":62,"tb":158,"imt":24.8,"imt_kategori":"normal"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt_kategori tidak konsisten" {
			t.Fatalf("imt_kategori mismatch = %d %v", rec.Code, resp)
		}
		// Parsial (tb saja): imt null diterima, terisi → 400.
		rec, _ = post("cf_v3_form", sections(`"antropometri":{"tb":158,"imt":null,"imt_kategori":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("antro partial null = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("cf_v3_form", sections(`"antropometri":{"tb":158,"imt":24.8}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("antro partial filled = %d %v", rec.Code, resp)
		}
	})
	t.Run("v3 calc kesadaran", func(t *testing.T) {
		const gcsFull = `"kesadaran":{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis",` +
			`"pupil":{"kiri_mm":3,"kanan_mm":3,"refleks_kiri":"lambat","refleks_kanan":"cepat"},` +
			`"pupil_score":1,"gcs_p":14,"respons":"a"}`
		rec, _ = post("cf_v3_form", sections(gcsFull))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs spec example = %d: %s", rec.Code, rec.Body.String())
		}
		for _, tc := range []struct{ val, wantErr string }{
			{`{"gcs":14}`, "kalkulasi gcs tidak konsisten"},
			{`{"e":"4","v":"5","m":"6","gcs":14}`, "kalkulasi gcs tidak konsisten"},
			{`{"e":"4","v":"5","m":"6","gcs":14,"kategori":"compos_mentis","respons":"a"}`, "kalkulasi gcs tidak konsisten"},
			{`{"e":"4","v":"5","m":"6","gcs":15,"kategori":"somnolen","respons":"a"}`, "kalkulasi kategori tidak konsisten"},
			{`{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"refleks_kiri":"lambat","refleks_kanan":"cepat"},"pupil_score":2,"gcs_p":14,"respons":"a"}`, "kalkulasi pupil_score tidak konsisten"},
			{`{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"refleks_kiri":"lambat","refleks_kanan":"cepat"},"pupil_score":1,"gcs_p":15,"respons":"a"}`, "kalkulasi gcs_p tidak konsisten"},
		} {
			rec, resp = post("cf_v3_form", sections(`"kesadaran":`+tc.val))
			if rec.Code != http.StatusBadRequest || resp["error"] != tc.wantErr {
				t.Fatalf("want 400 %q, got %d %v body=%s", tc.wantErr, rec.Code, resp, rec.Body.String())
			}
		}
		// Parsial: e saja + null diterima.
		rec, _ = post("cf_v3_form", sections(`"kesadaran":{"e":"4","gcs":null,"kategori":null,"pupil_score":null,"gcs_p":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs partial null = %d: %s", rec.Code, rec.Body.String())
		}
		// GCS-P clamp (Teasdale 2014 min 1; keputusan user 2026-11-21):
		// gcs = 1+1+1 = 3, refleks tidak_ada kiri+kanan → pupil_score
		// 2+2 = 4, gcs_p = max(3−4, 1) = 1 → diterima.
		rec, _ = post("cf_v3_form", sections(`"kesadaran":{"e":"1","v":"1","m":"1","gcs":3,`+
			`"pupil":{"refleks_kiri":"tidak_ada","refleks_kanan":"tidak_ada"},`+
			`"pupil_score":4,"gcs_p":1,"respons":"a"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs_p clamped 1 = %d: %s", rec.Code, rec.Body.String())
		}
		// gcs_p −1 (tidak di-clamp di input) → 400 invalid (shape rule
		// gcs_p int ≥ 0; nilai sah 1..15 tetap lewat shape).
		rec, resp = post("cf_v3_form", sections(`"kesadaran":{"e":"1","v":"1","m":"1","gcs":3,`+
			`"pupil":{"refleks_kiri":"tidak_ada","refleks_kanan":"tidak_ada"},`+
			`"pupil_score":4,"gcs_p":-1,"respons":"a"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'kesadaran'" {
			t.Fatalf("gcs_p -1 = %d %v", rec.Code, resp)
		}
		// "T": gcs/kategori/gcs_p null diterima; gcs terisi → 400 (bentuk).
		rec, _ = post("cf_v3_form", sections(`"kesadaran":{"e":"T","v":"T","m":"T","gcs":null,"kategori":null,"gcs_p":null,"pupil_score":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs T null = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("cf_v3_form", sections(`"kesadaran":{"e":"T","gcs":15}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'kesadaran'" {
			t.Fatalf("gcs T filled = %d %v", rec.Code, resp)
		}
	})
	t.Run("v3 calc news2", func(t *testing.T) {
		const news2Vitals = `"tanda_vital":{"td_sistolik":118,"td_diastolik":76,"nafas":22,"nadi":96,` +
			`"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","news2_total":5}`
		// Contoh ledger lengkap (dengan kesadaran.respons) → 5, diterima.
		rec, _ = post("cf_v3_form", sections(news2Vitals, `"kesadaran":{"respons":"a"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("news2 spec example = %d: %s", rec.Code, rec.Body.String())
		}
		// Mismatch 6 → 400.
		rec, resp = post("cf_v3_form", sections(`"tanda_vital":{"td_sistolik":118,"td_diastolik":76,"nafas":22,"nadi":96,"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","news2_total":6}`, `"kesadaran":{"respons":"a"}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi news2_total tidak konsisten" {
			t.Fatalf("news2 mismatch = %d %v", rec.Code, resp)
		}
		// Parsial (kesadaran absen): null diterima, terisi → 400.
		rec, _ = post("cf_v3_form", sections(`"tanda_vital":{"td_sistolik":118,"nafas":22,"nadi":96,"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","news2_total":null}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("news2 partial null = %d: %s", rec.Code, rec.Body.String())
		}
		rec, resp = post("cf_v3_form", sections(`"tanda_vital":{"td_sistolik":118,"nafas":22,"nadi":96,"suhu":38.2,"spo2":95,"spo2_scale":1,"oksigen":"udara_ruang","news2_total":5}`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi news2_total tidak konsisten" {
			t.Fatalf("news2 partial filled = %d %v", rec.Code, resp)
		}
	})
	t.Run("v3 tindakan concept", func(t *testing.T) {
		// Seed ICD-9-CM system + concept (pattern price_list_test.go:496-511).
		var systemID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO terminology.code_system (system_uri, name, version)
			 VALUES ('http://hl7.org/fhir/sid/icd-9-cm', 'ICD-9-CM', 'test')
			 ON CONFLICT (system_uri) DO UPDATE SET name = EXCLUDED.name
			 RETURNING id::text`).Scan(&systemID); err != nil {
			t.Fatalf("seed icd-9-cm system: %v", err)
		}
		conceptCode := fmt.Sprintf("CF9-%06d", cfNext())
		var conceptID string
		if err := pool.QueryRow(ctx,
			"INSERT INTO terminology.concept (code_system_id, code, display) VALUES ($1, $2, 'Test procedure') RETURNING id::text",
			systemID, conceptCode).Scan(&conceptID); err != nil {
			t.Fatalf("seed concept: %v", err)
		}
		// Concept aktif → 201.
		rec, _ = post("cf_v3_form", sections(`"tindakan":[{"concept_id":"`+conceptID+`","dokter":"dr. X"}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("concept aktif = %d: %s", rec.Code, rec.Body.String())
		}
		// Baris tanpa concept_id (draft parsial) → tetap 201.
		rec, _ = post("cf_v3_form", sections(`"tindakan":[{"dokter":"dr. X"}]`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("concept kosong = %d: %s", rec.Code, rec.Body.String())
		}
		// Non-aktif → 400 pesan kontrak.
		if _, err := pool.Exec(ctx, `UPDATE terminology.concept SET is_active = false WHERE id = $1::uuid`, conceptID); err != nil {
			t.Fatalf("deactivate concept: %v", err)
		}
		rec, resp = post("cf_v3_form", sections(`"tindakan":[{"concept_id":"`+conceptID+`","dokter":"dr. X"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "tindakan harus konsep ICD-9-CM aktif" {
			t.Fatalf("concept non-aktif = %d %v", rec.Code, resp)
		}
		// Non-UUID → 400.
		rec, resp = post("cf_v3_form", sections(`"tindakan":[{"concept_id":"bukan-uuid","dokter":"dr. X"}]`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'tindakan'" {
			t.Fatalf("concept non-uuid = %d %v", rec.Code, resp)
		}
	})
	// Legacy vitals path on key tanda_vital must not change (v2 template
	// ases_awal_perawat): pain_score 0–10 stays valid beside v3 fields.
	rec, _ = post("ases_awal_perawat", sections(`"tanda_vital":{"sistolic":120,"pain_score":3}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("legacy tanda_vital pain_score = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestClinicalFormDraftAuthorOnly — PATCH/DELETE by another writer → 403.
func TestClinicalFormDraftAuthorOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "author")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "author")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "soap_dokter", "form_data": cfRawJSON(t, soapFormData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)

	other := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.other.writer", server.PermClinicalRecordWrite)
	body := map[string]any{"form_data": cfRawJSON(t, soapFormData)}
	if rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, other, companyID, merchantID, body); rec.Code != http.StatusForbidden || resp["error"] != "hanya penulis entri" {
		t.Fatalf("patch by other = %d %v", rec.Code, resp)
	}
	if rec, resp = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/forms/"+id, other, companyID, merchantID, nil); rec.Code != http.StatusForbidden || resp["error"] != "hanya penulis entri" {
		t.Fatalf("delete by other = %d %v", rec.Code, resp)
	}
	// The author can still edit their draft.
	if rec, _ = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, body); rec.Code != http.StatusOK {
		t.Fatalf("patch by author = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestClinicalFormRajalOnly — CPPT fase 1 menolak admission non-rawat jalan.
func TestClinicalFormRajalOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "rajal")
	personID, _ := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "rajal")
	inpatientID := cfSeedInpatientAdmission(t, ctx, pool, companyID, merchantID, personID, "rajal")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": inpatientID, "form_type": "soap_dokter", "form_data": cfRawJSON(t, soapFormData),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "cppt fase 1 hanya rawat jalan" {
		t.Fatalf("inpatient = %d %v", rec.Code, resp)
	}
}

// TestClinicalFormTenantIsolation — RLS on clinical_form.form_instance: as
// app_runtime scoped to merchant B, merchant A's entry is invisible (the GET
// would 404). The HTTP path cannot prove this here — the test pool connects as
// the table owner, bypassing RLS (documented blind spot in server_test.go) —
// so the visibility assertion runs in an app_runtime tx like
// TestChildRLS_IsolationUnderAppRuntime.
func TestClinicalFormTenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyA, merchantA, _, tokenA := cfSeedEnv(t, ctx, pool, router, "tena")
	_, admissionA := cfSeedAdmission(t, ctx, pool, router, tokenA, companyA, merchantA, "tena")
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", tokenA, companyA, merchantA, map[string]any{
		"admission_id": admissionA, "form_type": "soap_dokter", "form_data": cfRawJSON(t, soapFormData),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)

	companyB, merchantB, _, _ := cfSeedEnv(t, ctx, pool, router, "tenb")

	tx := childRLSAppRuntimeTx(t, ctx, pool, companyB, merchantB)
	defer tx.Rollback(ctx)
	var countB int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM clinical_form.form_instance WHERE id = $1::uuid", id).Scan(&countB); err != nil {
		t.Fatalf("count as merchant B: %v", err)
	}
	if countB != 0 {
		t.Fatalf("tenant isolation broken: merchant B sees %d rows, want 0", countB)
	}
	tx.Rollback(ctx)

	tx = childRLSAppRuntimeTx(t, ctx, pool, companyA, merchantA)
	defer tx.Rollback(ctx)
	var countA int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM clinical_form.form_instance WHERE id = $1::uuid", id).Scan(&countA); err != nil {
		t.Fatalf("count as merchant A: %v", err)
	}
	if countA != 1 {
		t.Fatalf("merchant A sees %d rows, want 1", countA)
	}
}

// TestTerminologyAPILookupPermission — terminology endpoints are open to
// terminology.lookup holders (migration 000089) and stay open for
// core.person.manage admins; users with neither get 403.
func TestTerminologyAPILookupPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "term")

	// Owner keeps access via core.person.manage.
	if rec, _ := cfDo(t, router, http.MethodGet, "/api/v1/terminology/search?system=SRC&q=One", token, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner search = %d: %s", rec.Code, rec.Body.String())
	}
	// terminology.lookup alone → 200.
	lookup := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.term.lookup", "terminology.lookup")
	if rec, _ := cfDo(t, router, http.MethodGet, "/api/v1/terminology/search?system=SRC&q=One", lookup, companyID, merchantID, nil); rec.Code != http.StatusOK {
		t.Fatalf("lookup-only search = %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ := cfDo(t, router, http.MethodGet, "/api/v1/terminology/map?system=SRC&code=S1&target=DST", lookup, companyID, merchantID, nil); rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("lookup-only map = %d: %s", rec.Code, rec.Body.String())
	}
	// Neither permission → 403.
	none := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.term.none")
	if rec, _ := cfDo(t, router, http.MethodGet, "/api/v1/terminology/search?system=SRC&q=One", none, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no-perm search = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestClinicalFormAsuhanGiziPesOptionsDiff — diff mekanis sections
// asuhan_gizi post-migration 000102 vs baseline 000098:15-88 (melindungi 21
// section lain dari typo): semua key baseline tetap ada, delta hanya
// +diagnosis_terkait, opsi `problem` seluruhnya ^NI-, dan tidak ada "GD-"
// di opsi manapun (plan 2026-11-15-gizi-dewasa-4-adime-pes Task 2).
func TestClinicalFormAsuhanGiziPesOptionsDiff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	var raw string
	if err := pool.QueryRow(ctx,
		`SELECT sections::text FROM core.template_clinical_form WHERE form_type = 'asuhan_gizi'`,
	).Scan(&raw); err != nil {
		t.Fatalf("load asuhan_gizi sections: %v", err)
	}
	var defs struct {
		Sections []struct {
			Key     string `json:"key"`
			Options []struct {
				Value string `json:"value"`
			} `json:"options"`
			Fields []struct {
				Key     string `json:"key"`
				Options []struct {
					Value string `json:"value"`
				} `json:"options"`
			} `json:"fields"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(raw), &defs); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	// 21 key baseline 000098:15-88, urutan definisi.
	baseline := []string{
		"biokimia", "diet_history", "asupan", "lingkungan", "pes",
		"diet_jenis", "diet_porsi_hari", "preskripsi", "preskripsi_catatan",
		"suplementasi", "pangan_lokal", "edukasi", "edukasi_catatan",
		"me_indikator", "me_rentang", "me_frekuensi", "me_reassessment",
		"me_evaluasi", "me_status", "ttd", "tinja",
	}
	got := make(map[string]bool, len(defs.Sections))
	for _, d := range defs.Sections {
		got[d.Key] = true
	}
	if len(defs.Sections) != len(baseline)+1 {
		t.Fatalf("sections = %d, want %d (21 baseline + diagnosis_terkait)", len(defs.Sections), len(baseline)+1)
	}
	for _, k := range baseline {
		if !got[k] {
			t.Fatalf("section baseline 000098 hilang: %s", k)
		}
	}
	if !got["diagnosis_terkait"] {
		t.Fatal("delta sections bukan hanya +diagnosis_terkait: key itu tidak ada")
	}
	niRe := regexp.MustCompile(`^NI-`)
	for _, d := range defs.Sections {
		for _, o := range d.Options {
			if strings.Contains(o.Value, "GD-") {
				t.Fatalf("section %s: opsi %q masih memakai kode GD-", d.Key, o.Value)
			}
		}
		for _, f := range d.Fields {
			for _, o := range f.Options {
				if strings.Contains(o.Value, "GD-") {
					t.Fatalf("section %s field %s: opsi %q masih memakai kode GD-", d.Key, f.Key, o.Value)
				}
			}
			// Lampiran A: seluruh opsi problem cocok pola ^NI-.
			if d.Key == "pes" && f.Key == "problem" {
				if len(f.Options) != 42 {
					t.Fatalf("pes problem options = %d, want 42 (Lampiran A)", len(f.Options))
				}
				for _, o := range f.Options {
					if !niRe.MatchString(o.Value) {
						t.Fatalf("pes problem opsi %q tidak cocok pola ^NI-", o.Value)
					}
				}
			}
		}
	}
}

// TestClinicalFormPemicuValidation — validator rentang/konsistensi form
// ases_gizi_pemicu_bumil (plan 2026-11-16-gizi-maternal-1-be-hook Task 3,
// spec §5): lila 61 di luar (0;60] → 400 rentang; hb 0,5 di luar [1;25] →
// 400 rentang; interpretasi_pemicu "rendah" padahal hitungan server "tinggi"
// (kek 22,8<23,5 + anemia 10,2<10,5 t2, score 2, tanpa MST episode) → 400
// konsistensi; payload valid tanpa interpretasi → 201.
func TestClinicalFormPemicuValidation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "pemicuval")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzpemicuval")
	const create = "/api/v1/clinical/forms"
	post := func(form map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		rec, _ := cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": "ases_gizi_pemicu_bumil",
			"form_data": form,
		})
		return rec
	}

	// (a) lila 61 → 400 "nilai pemicu di luar rentang wajar (cek satuan)".
	lila, hb, mgg := 61.0, 10.2, 26.0
	if rec := post(gzPemicuBumilForm(&lila, &hb, &mgg)); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "nilai pemicu di luar rentang wajar (cek satuan)") {
		t.Fatalf("lila 61 = %d: %s", rec.Code, rec.Body.String())
	}

	// (b) hb 0,5 → 400 rentang yang sama.
	lila, hb = 22.0, 0.5
	if rec := post(gzPemicuBumilForm(&lila, &hb, &mgg)); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "nilai pemicu di luar rentang wajar (cek satuan)") {
		t.Fatalf("hb 0.5 = %d: %s", rec.Code, rec.Body.String())
	}

	// (c) interpretasi_pemicu "rendah" vs hitungan server "tinggi" → 400
	// "stratifikasi pemicu tidak konsisten".
	lila, hb = 22.8, 10.2
	rec := post(map[string]any{
		"sections": map[string]any{
			"lila_cm": lila, "hb_gdl": hb, "mgg": mgg, "interpretasi_pemicu": "rendah",
		},
	})
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "stratifikasi pemicu tidak konsisten") {
		t.Fatalf("interp rendah = %d: %s", rec.Code, rec.Body.String())
	}

	// (d) payload valid (tanpa interpretasi) → 201.
	if rec = post(gzPemicuBumilForm(&lila, &hb, &mgg)); rec.Code != http.StatusCreated {
		t.Fatalf("valid = %d: %s", rec.Code, rec.Body.String())
	}
}
