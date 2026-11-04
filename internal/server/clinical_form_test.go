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

// soapFormData is the LITERAL spec §5.2 example (used as the happy-path test
// payload; expectation values come from the spec, not from the code).
const soapFormData = `{"sections":{
  "subjective":"Demam 3 hari, batuk berdahak.",
  "objective":{"sistolic":120,"diastolic":80,"heart_rate":88,"temperature":37.4,"respiratory_rate":20,"spo2":97,"weight_kg":62.5,"height_cm":168,"pain_score":2},
  "assessment":{"narrative":"Suspek infeksi saluran napas atas",
    "diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"}}]},
  "plan":"Parasetamol 500 mg 3×1 bila demam; kontrol 3 hari bila memburuk."}}`

func cfRawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

// TestClinicalFormTemplateSeed — 7 platform templates resolve and sections
// arrive as a JSON object (spec §9:178).
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
	if len(data) != 7 {
		t.Fatalf("form-templates: got %d items, want 7", len(data))
	}
	var soap map[string]any
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
		if item["form_type"] == "soap_dokter" {
			soap = item
		}
	}
	if soap == nil {
		t.Fatal("soap_dokter template missing")
	}
	if soap["scope"] != "platform" || soap["category"] != "soap" {
		t.Fatalf("soap_dokter = %v", soap)
	}
	if v, _ := soap["version"].(float64); v != 2 {
		t.Fatalf("soap_dokter version = %v, want 2", soap["version"])
	}
	// soap_dokter v2 = 12 sections (migration 000091).
	defs, _ := soap["sections"].(map[string]any)["sections"].([]any)
	if len(defs) != 12 {
		t.Fatalf("soap_dokter sections = %d, want 12", len(defs))
	}
	var alergi map[string]any
	calcKinds := map[string]bool{}
	for _, d := range defs {
		def := d.(map[string]any)
		key, _ := def["key"].(string)
		kind, _ := def["kind"].(string)
		if key == "alergi" {
			alergi = def
		}
		if kind == "calc" {
			calcKinds[key] = true
		}
	}
	if alergi == nil {
		t.Fatal("soap_dokter v2 missing alergi section")
	}
	if alergi["kind"] != "repeatable" {
		t.Fatalf("alergi kind = %v, want repeatable", alergi["kind"])
	}
	fields, _ := alergi["fields"].([]any)
	if len(fields) != 6 {
		t.Fatalf("alergi fields = %d, want 6", len(fields))
	}
	if !calcKinds["gcs_total"] || !calcKinds["imt"] {
		t.Fatalf("calc sections = %v, want gcs_total and imt", calcKinds)
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

	updated := `{"sections":{"subjective":"Demam 3 hari, membaik.","objective":{"sistolic":118,"diastolic":78},"assessment":{"narrative":"ISEL, membaik","diagnoses":[]},"plan":"Lanjut parasetamol."}}`
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, map[string]any{
		"form_data": cfRawJSON(t, updated),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	patched := resp["data"].(map[string]any)["entry"].(map[string]any)["form_data"].(map[string]any)
	if patched["sections"].(map[string]any)["subjective"] != "Demam 3 hari, membaik." {
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
	// This test makes ~25 API calls with one token — lift the default burst
	// of 20 (pattern rbac_physician_cleanup_test.go).
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 1000
	cfg.RateLimitAPIBurst = 100
	router := server.NewRouter(pool, newTestRedisClient(t, ctx), cfg)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "valid")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "valid")

	post := func(formType string, formData any) (*httptest.ResponseRecorder, map[string]any) {
		return cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": formType, "form_data": formData,
		})
	}

	// Happy path: the literal §5.2 example is accepted. On template v2 the
	// new optional sections (keluhan_utama/alergi/gcs/…/rekomendasi) are empty
	// here, so only the shape of warnings is asserted, not emptiness.
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
	rec, resp = post("soap_dokter", sections(`"subjective":"ada"`, `"halo":"x"`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "unknown section key: halo" {
		t.Fatalf("unknown key = %d %v", rec.Code, resp)
	}
	// Wrong type per kind (textarea given a number).
	rec, resp = post("soap_dokter", sections(`"subjective":123`))
	if rec.Code != http.StatusBadRequest || resp["error"] != "invalid value for section 'subjective'" {
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
	// assessment narrative-only (tanpa diagnoses) → draft tersimpan
	// (soft_required, review A3).
	rec, _ = post("soap_dokter", sections(`"assessment":{"narrative":"Hanya naratif klinis."}`))
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
	// soapV2FormData = LITERAL spec §5 example (migration 000091 template).
	// Expectations computed from the spec: gcs 4+5+6=15; imt = round(62/(158/100)²,1) = round(24.8357,1) = 24.8.
	const soapV2FormData = `{"sections":{
	  "keluhan_utama":"Batuk berdahak 5 hari, demam sejak 2 hari lalu",
	  "subjective":"Batuk berdahak kuning, memburuk saat malam. Demam 38-39°C turun-naik.",
	  "alergi":[{"pemicu":"amoksisilin","jenis":"obat","reaksi":"Biduran dan gatal","keparahan":"sedang","efek_samping":"","tanggal":"2024-03-10"}],
	  "objective":{"sistolic":120,"diastolic":80,"heart_rate":84,"temperature":37.6,"respiratory_rate":18,"spo2":98,"weight_kg":62,"height_cm":158,"pain_score":6},
	  "gcs_eye":"4","gcs_verbal":"5","gcs_motorik":"6","gcs_total":15,"imt":24.8,
	  "assessment":{"narrative":"Suspek infeksi saluran napas atas",
	    "diagnoses":[
	      {"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"Acute upper respiratory infection"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"auto","map_set":"icd10-icd11-map"},"suspect":false},
	      {"role":"secondary","concept":{"system":"ICD-11","code":"DA0A.0","display":"Dental caries"},"suspect":true}]},
	  "plan":"Rehat total 3 hari. Terapi simtomatik: paracetamol 500 mg 3x1 selama demam.",
	  "rekomendasi":"Kontrol 3 hari jika demam >38.5°C atau batuk memburuk"}}`

	t.Run("v2 example accepted", func(t *testing.T) {
		// (a) full spec §5 example → 201.
		if rec, _ := post("soap_dokter", cfRawJSON(t, soapV2FormData)); rec.Code != http.StatusCreated {
			t.Fatalf("spec §5 example = %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("calc gcs", func(t *testing.T) {
		// (b) 4+5+6 = 15, ditulis 14 → 400.
		rec, resp := post("soap_dokter", sections(
			`"gcs_eye":"4"`, `"gcs_verbal":"5"`, `"gcs_motorik":"6"`, `"gcs_total":14`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi gcs_total tidak konsisten" {
			t.Fatalf("gcs mismatch = %d %v", rec.Code, resp)
		}
		// (c) verbal "t" + total "T" → lolos (input parsial + T dinilai konsisten).
		rec, _ = post("soap_dokter", sections(`"gcs_verbal":"t"`, `"gcs_total":"T"`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs T = %d: %s", rec.Code, rec.Body.String())
		}
		// (d) verbal "t" + total numerik → 400.
		rec, resp = post("soap_dokter", sections(`"gcs_verbal":"t"`, `"gcs_total":15`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi gcs_total tidak konsisten" {
			t.Fatalf("gcs t numeric = %d %v", rec.Code, resp)
		}
	})
	t.Run("calc imt", func(t *testing.T) {
		// (e) BB 62 / TB 158 → imt seharusnya 24.8; ditulis 30 → 400.
		rec, resp := post("soap_dokter", sections(
			`"objective":{"weight_kg":62,"height_cm":158}`, `"imt":30`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt mismatch = %d %v", rec.Code, resp)
		}
		rec, _ = post("soap_dokter", sections(
			`"objective":{"weight_kg":62,"height_cm":158}`, `"imt":24.8`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("imt ok = %d: %s", rec.Code, rec.Body.String())
		}
		// (f) imt terisi, weight_kg kosong (parsial) → 400.
		rec, resp = post("soap_dokter", sections(
			`"objective":{"height_cm":158}`, `"imt":24.8`))
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
	})
	t.Run("suspect", func(t *testing.T) {
		// (j) REMEDIAL: shape/suspect divalidasi saat sign, bukan create.
		// Keduanya pakai entri valid literal withMap (pattern
		// clinical_diagnosis_test.go:180 — merchant default ICD-10 butuh
		// mapping icd10) plus "suspect".
		const withMapBad = `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"},"suspect":"ya"}]}}}`
		const withMapOK = `{"sections":{"assessment":{"narrative":"ISPA","diagnoses":[{"role":"primary","concept":{"system":"ICD-11","code":"CA00.00","display":"ISPA"},"icd10":{"code":"J06.9","display":"Acute upper respiratory infection, unspecified","source":"manual","map_set":"icd10-icd11-map"},"suspect":true}]}}}`
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
		// sign; entri tetap draft (semantik fase 1).
		rec, resp := post("soap_dokter", sections(`"assessment":{"narrative":"ISPA","diagnoses":["ISPA"]}`))
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
	t.Run("draft v1 shape on v2 template", func(t *testing.T) {
		// (k) draft dengan 4 key lama saja → PATCH di template v2 tetap 200 (D7).
		rec, resp := post("soap_dokter", sections(
			`"subjective":"lama"`, `"objective":{"sistolic":120}`,
			`"assessment":{"narrative":"ISEL"}`, `"plan":"p"`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("v1-shape create = %d: %s", rec.Code, rec.Body.String())
		}
		id := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
		rec, _ = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+id, token, companyID, merchantID, map[string]any{
			"form_data": sections(`"subjective":"masih lama"`),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("v1-shape patch = %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("calc partial inputs", func(t *testing.T) {
		// (l) PARSIAL-LOLOS: satu input, calc absen → draft sah.
		rec, _ := post("soap_dokter", sections(`"gcs_eye":"4"`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("gcs partial ok = %d: %s", rec.Code, rec.Body.String())
		}
		rec, _ = post("soap_dokter", sections(`"objective":{"weight_kg":62}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("imt partial ok = %d: %s", rec.Code, rec.Body.String())
		}
		// (m) PARSIAL-TERISI: calc terisi padahal input belum lengkap → 400.
		rec, resp := post("soap_dokter", sections(`"gcs_eye":"4"`, `"gcs_total":4`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi gcs_total tidak konsisten" {
			t.Fatalf("gcs partial filled = %d %v", rec.Code, resp)
		}
		rec, resp = post("soap_dokter", sections(`"objective":{"weight_kg":62}`, `"imt":24.8`))
		if rec.Code != http.StatusBadRequest || resp["error"] != "kalkulasi imt tidak konsisten" {
			t.Fatalf("imt partial filled = %d %v", rec.Code, resp)
		}
	})
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
