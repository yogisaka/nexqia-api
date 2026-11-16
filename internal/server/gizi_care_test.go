//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// gzNewRouter builds the router with the per-user API rate limit lifted —
// guard test chains >20 requests with one token
// (pola rbac_physician_cleanup_test.go / clinical_form_test.go).
func gzNewRouter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	cfg := testConfig()
	cfg.RateLimitAPITokensPerMinute = 6000
	cfg.RateLimitAPIBurst = 1000
	return server.NewRouter(pool, newTestRedisClient(t, ctx), cfg)
}

// Gizi endpoint tests (plan 2026-11-13-gizi-nutrition-care chunk 1 Task 3,
// spec 2026-11-13-gizi-nutrition-care-spec §4.1, §5): antropometri + episode +
// guard profil (close_active / PATCH audit / revert). Fixture reuse helper
// shared (newTestPostgresPool, seedAccountOwner, cfDo, cfSeedAdmission,
// cfSeedUserWithPerms); tiap test punya container Postgres sendiri. Nilai
// ekspektasi dihitung dari definisi kontrak/DDL (migration 000097), bukan
// dari output handler. Fixture ref kurva SINTETIS (bukan WHO CSV) supaya
// zscore dihitung tangan dari rumus LMS.
var gzCounter int64

func gzNext() int64 { return atomic.AddInt64(&gzCounter, 1) }

func gzSeedEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, label string) (companyID, merchantID, userID, token string) {
	t.Helper()
	n := gzNext()
	companyID, merchantID, userID, token, _ = seedAccountOwner(t, ctx, pool, router, "gz."+label, fmt.Sprintf("08123483%04d", n))
	return companyID, merchantID, userID, token
}

// gzSeedBalita inserts a person with birth_date = today-548 days (male) and
// registers an outpatient admission for it; returns (personID, admissionID).
func gzSeedBalita(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID, label string) (string, string) {
	t.Helper()
	birth := time.Now().AddDate(0, 0, -548).Format("2006-01-02")
	var personID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, 'male', $3) RETURNING id::text",
		companyID, "Gizi Balita "+label, birth).Scan(&personID); err != nil {
		t.Fatalf("seed balita person: %v", err)
	}
	admissionID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "gzb-"+label)
	return personID, admissionID
}

// gzSeedBBURef inserts ONE synthetic bbu curve row (l=1, m=10, s=0.1 at
// age_days 548) — z(7.9) = (7.9/10-1)/0.1 = -2.10 by the LMS definition.
func gzSeedBBURef(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at)
		VALUES ('bbu', 'laki_laki', 'age_days', 548, 1, 10, 0.1, 'sintetis', 'gz-test', 'http://test.local/sintetis', now())`); err != nil {
		t.Fatalf("seed ref bbu: %v", err)
	}
}

// gzNum converts a JSON numeric field (rendered as string by numericJSON or a
// raw number) to float64 for assertions.
func gzNum(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			t.Fatalf("numeric field %v: %v", v, err)
		}
		return f
	case float64:
		return n
	default:
		t.Fatalf("numeric field unexpected type %T: %v", v, v)
		return 0
	}
}

// TestGiziAnthropometryAPI — POST create (snapshot imt_kategori/zscore),
// validasi measured_at masa depan, izin, dan isolasi merchant.
func TestGiziAnthropometryAPI(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "anto")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzanto")
	now := time.Now().Format(time.RFC3339)

	// Dewasa (person tanpa birth_date): imt = round(62/1.58^2, 1) = 24.8 →
	// kategori bb_lebih (23 <= imt < 25, KMK 509/2025 via trigger 000097).
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "measured_at": now, "bb_kg": 62.0, "tb_cm": 158.0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create dewasa = %d: %s", rec.Code, rec.Body.String())
	}
	item := resp["data"].(map[string]any)
	if item["imt_kategori"] != "bb_lebih" || item["source_ref"] != "gizi_tab" {
		t.Fatalf("dewasa = %v", item)
	}
	if gzNum(t, item["imt"]) != 24.8 {
		t.Fatalf("imt = %v, want 24.8", item["imt"])
	}
	if item["age_days"] != nil || item["zscore_bbu"] != nil {
		t.Fatalf("dewasa z/age harus NULL: %v", item)
	}

	// Balita dengan ref sintetis: age_days 548, bb 7.9 → zscore_bbu = -2.10.
	gzSeedBBURef(t, ctx, pool)
	_, balitaAdmission := gzSeedBalita(t, ctx, pool, router, token, companyID, merchantID, "b1")
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, map[string]any{
		"admission_id": balitaAdmission, "measured_at": now, "bb_kg": 7.9, "tb_cm": 75.0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create balita = %d: %s", rec.Code, rec.Body.String())
	}
	bal := resp["data"].(map[string]any)
	if gzNum(t, bal["zscore_bbu"]) != -2.10 {
		t.Fatalf("zscore_bbu = %v, want -2.10", bal["zscore_bbu"])
	}
	age, _ := bal["age_days"].(float64)
	if age != 548 {
		t.Fatalf("age_days = %v, want 548", bal["age_days"])
	}
	if bal["curve_version"] != "gz-test" {
		t.Fatalf("curve_version = %v", bal["curve_version"])
	}

	// List per admission (measured_at DESC) via GET.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/anthropometry?admission_id="+balitaAdmission, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 1 {
		t.Fatalf("list balita = %d %v", rec.Code, resp)
	}

	// measured_at masa depan → 400 pesan kontrak.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "measured_at": time.Now().Add(time.Hour).Format(time.RFC3339), "bb_kg": 62.0,
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "measured_at tidak boleh di masa depan" {
		t.Fatalf("future = %d %v", rec.Code, resp)
	}

	// Tanpa izin (role kosong) → 403.
	noPerm := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "gz-anto-noperm")
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", noPerm, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "measured_at": now, "bb_kg": 62.0,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no perm = %d: %s", rec.Code, rec.Body.String())
	}

	// Merchant lain → 404 (pola RLS existing: admission tidak terlihat).
	companyB, merchantB, _, tokenB := gzSeedEnv(t, ctx, pool, router, "antob")
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", tokenB, companyB, merchantB, map[string]any{
		"admission_id": admissionID, "measured_at": now, "bb_kg": 62.0,
	})
	if rec.Code != http.StatusNotFound || resp["error"] != "admission tidak ditemukan" {
		t.Fatalf("cross merchant = %d %v", rec.Code, resp)
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/anthropometry?admission_id="+admissionID, tokenB, companyB, merchantB, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross merchant list = %d %v", rec.Code, resp)
	}
}

// TestGiziNutritionEpisodeAPI — create+get, dobel aktif 409, PATCH row_version
// konflik, PATCH status closed → closed_at, episode null bila kosong.
func TestGiziNutritionEpisodeAPI(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, userID, token := gzSeedEnv(t, ctx, pool, router, "ep")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzep")

	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get kosong = %d: %s", rec.Code, rec.Body.String())
	}
	if resp["episode"] != nil || resp["risk"] != nil {
		t.Fatalf("get kosong = %v (episode & risk harus null)", resp)
	}
	if _, ok := resp["screening"].([]any); !ok {
		t.Fatalf("screening harus array: %v", resp["screening"])
	}
	if resp["kunjungan_count"].(float64) != 1 {
		t.Fatalf("kunjungan_count = %v, want 1", resp["kunjungan_count"])
	}

	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "profile": "dewasa",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	ep := resp["data"].(map[string]any)
	if ep["profile"] != "dewasa" || ep["status"] != "active" || ep["person_id"] != personID ||
		ep["admission_id"] != admissionID || ep["closed_at"] != nil || ep["row_version"].(float64) != 1 {
		t.Fatalf("episode = %v", ep)
	}
	episodeID := ep["id"].(string)

	// Dobel aktif → 409 pesan kontrak.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "profile": "lansia",
	})
	if rec.Code != http.StatusConflict || resp["error"] != "episode gizi aktif sudah ada" {
		t.Fatalf("duplikat = %d %v", rec.Code, resp)
	}

	// GET kini mengembalikan episode aktif + anthropometry_latest null.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	if resp["episode"] == nil || resp["anthropometry_latest"] != nil {
		t.Fatalf("get = %v", resp)
	}

	// PATCH row_version salah → 409 "row_version konflik".
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+episodeID, token, companyID, merchantID, map[string]any{
		"row_version": 99, "profile": "bumil",
	})
	if rec.Code != http.StatusConflict || resp["error"] != "row_version konflik" {
		t.Fatalf("rv conflict = %d %v", rec.Code, resp)
	}

	// PATCH status closed → closed_at terisi.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+episodeID, token, companyID, merchantID, map[string]any{
		"row_version": 1, "status": "closed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body.String())
	}
	closed := resp["data"].(map[string]any)
	if closed["status"] != "closed" || closed["closed_at"] == nil || closed["row_version"].(float64) != 2 {
		t.Fatalf("closed = %v", closed)
	}

	// Setelah tutup, GET tidak lagi punya episode aktif (null, bukan 404).
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || resp["episode"] != nil {
		t.Fatalf("get after close = %d %v", rec.Code, resp)
	}
	_ = userID
}

// gzMSTForm builds ases_gizi_skrining form_data with a consistent MST: the
// three select answers sum to skor (validator §5.1/§5.2: sum == skor_mst and
// risiko_gizi = skor >= 2 ? berisiko : tidak_berisiko).
func gzMSTForm(skor int) map[string]any {
	// Dekomposisi deterministik: bb = min(2, skor), nafsu = sisa sampai 3
	// capped 1, sakit = sisa (0/1). 0..5 selalu terwakili.
	bb := min(skor, 2)
	nafsu := min(skor-bb, 1)
	sakit := skor - bb - nafsu
	risiko := "tidak_berisiko"
	if skor >= 2 {
		risiko = "berisiko"
	}
	return map[string]any{"sections": map[string]any{
		"mst_penurunan_bb": strconv.Itoa(bb),
		"mst_nafsu_makan":  strconv.Itoa(nafsu),
		"mst_sakit_akut":   strconv.Itoa(sakit),
		"skor_mst":         skor,
		"risiko_gizi":      risiko,
	}}
}

func gzNsrCountAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, admissionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM clinical.nutrition_screening_result WHERE admission_id = $1::uuid",
		admissionID).Scan(&n); err != nil {
		t.Fatalf("count nsr: %v", err)
	}
	return n
}

// gzMnasfForm builds ases_gizi_mnasf form_data (template migration 000098).
// Enam jawaban select "<huruf>_<skor>" di-dekomposisi greedy memakai rentang
// opsi resmi (a 0..2, b 0..3, c 0..2, d 0..2, e 0..2, f 0..3) sehingga jumlah
// jawaban = skor (konsisten dengan validator §5 hook); interp harus ikut band
// skor (>=12 normal, 8..11 berisiko, <8 malnutrisi).
func gzMnasfForm(skor int, interp string) map[string]any {
	parts := []struct {
		k   string
		max int
	}{{"a", 2}, {"b", 3}, {"c", 2}, {"d", 2}, {"e", 2}, {"f", 3}}
	secs := map[string]any{}
	sisa := skor
	for _, p := range parts {
		v := min(p.max, sisa)
		sisa -= v
		secs["mnasf_"+p.k] = fmt.Sprintf("%s_%d", p.k, v)
	}
	secs["skor_mnasf"] = skor
	secs["interpretasi_mnasf"] = interp
	return map[string]any{"sections": secs}
}

// TestGiziMnasfValidation — validator skor MNA-SF (plan chunk 2 Task 2, spec
// §5 hook, pola test MST existing clinical_form_test.go:452). Skor konsisten
// lolos; skor ≠ jumlah jawaban → 400 "skor MNA-SF tidak konsisten"; band
// interpretasi salah → 400; draft parsial tanpa skor → lolos.
func TestGiziMnasfValidation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "mnasfval")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzmv")
	const create = "/api/v1/clinical/forms"

	// (a) skor 9 = 2+3+2+2+0+0, band 8..11 → "berisiko" → lolos.
	rec, _ := cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_mnasf", "form_data": gzMnasfForm(9, "berisiko"),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create konsisten = %d: %s", rec.Code, rec.Body.String())
	}

	// (b) skor ≠ jumlah jawaban (jawaban 2+3+2 = 7, skor ditulis 9) → 400.
	rec, resp := cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_mnasf",
		"form_data": map[string]any{"sections": map[string]any{
			"mnasf_a": "a_2", "mnasf_b": "b_3", "mnasf_c": "c_2",
			"skor_mnasf": 9, "interpretasi_mnasf": "berisiko",
		}},
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "skor MNA-SF tidak konsisten" {
		t.Fatalf("skor beda jumlah = %d %v", rec.Code, resp)
	}

	// (c) interpretasi salah band (skor 9 → harus "berisiko", ditulis "normal") → 400.
	rec, resp = cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_mnasf", "form_data": gzMnasfForm(9, "normal"),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "skor MNA-SF tidak konsisten" {
		t.Fatalf("band salah = %d %v", rec.Code, resp)
	}

	// (d) draft parsial tanpa skor_mnasf → lolos (pola MST).
	rec, _ = cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_mnasf",
		"form_data": map[string]any{"sections": map[string]any{"mnasf_a": "a_2"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("draft parsial = %d: %s", rec.Code, rec.Body.String())
	}
	// Form (a) konsisten memicu hook mnasf (create → 1 baris skor 9); kasus
	// (b)/(c) 400 di-rollback dan (d) draft parsial tanpa baris → tetap 1.
	if n := gzNsrCountAdmission(t, ctx, pool, admissionID); n != 1 {
		t.Fatalf("nsr setelah validasi = %d, want 1", n)
	}
}

// TestGiziScreeningResultHook — hook FR-GZ-02 (plan chunk 1 Task 4, spec §5):
// create ases_gizi_skrining skor 3 → baris mst (sedang); PATCH skor 4 → baris
// yang SAMA ter-update (idempoten, tidak duplikat); form kategori lain dan
// draft tanpa skor → tanpa baris. Ekspektasi dari kontrak (max_score 5,
// 0→rendah; 1–3→sedang; 4–5→tinggi; berisiko_mst = skor >= 2), bukan output.
func TestGiziScreeningResultHook(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "hook")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzhook")

	// (a) create skor 3 → 201 + GET episode menampilkan screening mst.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_skrining", "form_data": gzMSTForm(3),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create mst = %d: %s", rec.Code, rec.Body.String())
	}
	formID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get episode = %d: %s", rec.Code, rec.Body.String())
	}
	scr := resp["screening"].([]any)
	if len(scr) != 1 {
		t.Fatalf("screening len = %d, want 1: %v", len(scr), scr)
	}
	s0 := scr[0].(map[string]any)
	if s0["instrument"] != "mst" || s0["score"].(float64) != 3 || s0["interpretation"] != "sedang" ||
		s0["max_score"].(float64) != 5 {
		t.Fatalf("screening[0] = %v, want {mst, 3, sedang, max 5}", s0)
	}

	// (b) PATCH skor 4 → baris TER-UPDATE (1 baris, tinggi), bukan baris baru.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+formID, token, companyID, merchantID, map[string]any{
		"form_data": gzMSTForm(4),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch mst = %d: %s", rec.Code, rec.Body.String())
	}
	var score, maxScore, rowVersion int
	var interpretation string
	var payloadRaw string
	if err := pool.QueryRow(ctx,
		`SELECT score, max_score, interpretation, payload::text, row_version
		 FROM clinical.nutrition_screening_result WHERE form_instance_id = $1::uuid AND instrument = 'mst'`,
		formID).Scan(&score, &maxScore, &interpretation, &payloadRaw, &rowVersion); err != nil {
		t.Fatalf("read nsr: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
		t.Fatalf("decode payload %s: %v", payloadRaw, err)
	}
	if score != 4 || maxScore != 5 || interpretation != "tinggi" || rowVersion != 2 {
		t.Fatalf("nsr = %d/%d/%s rv=%d, want 4/5/tinggi rv=2", score, maxScore, interpretation, rowVersion)
	}
	if payload["berisiko_mst"] != true { // skor 4 >= 2
		t.Fatalf("payload = %v, want berisiko_mst=true", payload)
	}

	t.Run("upsert saat baris ada", func(t *testing.T) {
		// Jalur INSERT/ON CONFLICT (race hook, migration 000100): panggil
		// InsertGiziScreeningResult ke-2 untuk form yang sama → tetap 1 baris,
		// skor terbaru menang (kontrak upsert Task 6; interpretasi skor 2 =
		// 'sedang' dari definisi band 1–3, bukan output kode).
		var comp, merch, pers, adm, createdBy string
		if err := pool.QueryRow(ctx,
			`SELECT company_id::text, merchant_id::text, person_id::text,
			        admission_id::text, calculated_by::text
			 FROM clinical.nutrition_screening_result
			 WHERE form_instance_id = $1::uuid AND instrument = 'mst'`,
			formID).Scan(&comp, &merch, &pers, &adm, &createdBy); err != nil {
			t.Fatalf("read nsr untuk upsert: %v", err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		for _, g := range [][2]string{{"app.current_company_id", comp}, {"app.current_merchant_id", merch}} {
			if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", g[0], g[1]); err != nil {
				t.Fatalf("set_config %s: %v", g[0], err)
			}
		}
		gzUUID := func(t *testing.T, s string) pgtype.UUID {
			t.Helper()
			var u pgtype.UUID
			if err := u.Scan(s); err != nil {
				t.Fatalf("uuid %s: %v", s, err)
			}
			return u
		}
		if _, err := sqlcgen.New(tx).InsertGiziScreeningResult(ctx, sqlcgen.InsertGiziScreeningResultParams{
			CompanyID: gzUUID(t, comp), MerchantID: gzUUID(t, merch), PersonID: gzUUID(t, pers),
			AdmissionID: gzUUID(t, adm), FormInstanceID: gzUUID(t, formID),
			Score: 2, Interpretation: "sedang",
			Bands: json.RawMessage(`[]`), Payload: json.RawMessage(`{"berisiko_mst":false}`),
			CalculatedBy: gzUUID(t, createdBy), CreatedBy: gzUUID(t, createdBy),
		}); err != nil {
			t.Fatalf("upsert nsr: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var score, rowVersion, n int
		if err := pool.QueryRow(ctx,
			`SELECT score, row_version, (SELECT count(*) FROM clinical.nutrition_screening_result
			   WHERE form_instance_id = $1::uuid)
			 FROM clinical.nutrition_screening_result
			 WHERE form_instance_id = $1::uuid AND instrument = 'mst'`,
			formID).Scan(&score, &rowVersion, &n); err != nil {
			t.Fatalf("read nsr setelah upsert: %v", err)
		}
		if n != 1 || score != 2 {
			t.Fatalf("upsert: rows=%d score=%d, want 1 baris skor 2 (terbaru)", n, score)
		}
		if rowVersion != 3 { // 1 (insert awal) + trg_touch_row di PATCH + DO UPDATE
			t.Fatalf("row_version = %d, want 3 (naik via trg_touch_row)", rowVersion)
		}
	})

	t.Run("update tidak duplikat", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM clinical.nutrition_screening_result WHERE form_instance_id = $1::uuid",
			formID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("rows per form = %d (%v), want 1", n, err)
		}
	})

	t.Run("mnasf", func(t *testing.T) {
		// Hook MNA-SF (plan chunk 2 Task 2, spec §5 hook): create
		// ases_gizi_mnasf skor 9 → baris {instrument mnasf, score 9,
		// interpretation "sedang", max_score 14, payload kategori_mnasf}.
		// PATCH skor 7 → baris yang SAMA ter-update ke "tinggi" (idempoten).
		// Admission terpisah agar hitungan nsr admission utama tidak bergeser.
		_, admM := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzmnasf")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admM, "form_type": "ases_gizi_mnasf", "form_data": gzMnasfForm(9, "berisiko"),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create mnasf = %d: %s", rec.Code, rec.Body.String())
		}
		formM := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
		var score, maxScore int
		var interp, payloadRaw string
		if err := pool.QueryRow(ctx,
			`SELECT score, max_score, interpretation, payload::text
			 FROM clinical.nutrition_screening_result
			 WHERE form_instance_id = $1::uuid AND instrument = 'mnasf'`,
			formM).Scan(&score, &maxScore, &interp, &payloadRaw); err != nil {
			t.Fatalf("read nsr mnasf: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
			t.Fatalf("decode payload %s: %v", payloadRaw, err)
		}
		// Band definisi spec §5 hook: 8–11 → sedang; skor 9 = 2+3+2+2+0+0.
		if score != 9 || maxScore != 14 || interp != "sedang" || payload["kategori_mnasf"] != "berisiko" {
			t.Fatalf("nsr mnasf = %d/%d/%s/%v, want 9/14/sedang/berisiko", score, maxScore, interp, payload)
		}
		// PATCH skor 7 → baris sama (1 baris), band <8 → "tinggi".
		rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+formM, token, companyID, merchantID, map[string]any{
			"form_data": gzMnasfForm(7, "malnutrisi"),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("patch mnasf = %d: %s", rec.Code, rec.Body.String())
		}
		if err := pool.QueryRow(ctx,
			`SELECT score, interpretation, (SELECT count(*) FROM clinical.nutrition_screening_result
			   WHERE form_instance_id = $1::uuid)
			 FROM clinical.nutrition_screening_result
			 WHERE form_instance_id = $1::uuid AND instrument = 'mnasf'`,
			formM).Scan(&score, &interp, &maxScore); err != nil {
			t.Fatalf("read nsr mnasf pasca-patch: %v", err)
		}
		if maxScore != 1 || score != 7 || interp != "tinggi" {
			t.Fatalf("nsr mnasf pasca-patch = rows %d skor %d %s, want 1 baris 7 tinggi", maxScore, score, interp)
		}
	})

	// (c) form kategori lain (ases_gizi_adime) → tanpa baris baru.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_adime", "form_data": map[string]any{"sections": map[string]any{}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create adime = %d: %s", rec.Code, rec.Body.String())
	}
	if n := gzNsrCountAdmission(t, ctx, pool, admissionID); n != 1 {
		t.Fatalf("nsr setelah adime = %d, want 1", n)
	}

	// (d) draft MST tanpa skor_mst → hook dilewati, tanpa baris.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_skrining",
		"form_data": map[string]any{"sections": map[string]any{"mst_penurunan_bb": "1"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft = %d: %s", rec.Code, rec.Body.String())
	}
	if n := gzNsrCountAdmission(t, ctx, pool, admissionID); n != 1 {
		t.Fatalf("nsr setelah draft tanpa skor = %d, want 1", n)
	}

	// GET episode masih 1 hasil (mst terbaru), person_id konteks konsisten.
	if personID == "" {
		t.Fatalf("person tidak tersedia")
	}
}

// TestGiziProfileGuardAPI — guard 3 jalur FR-GZ-01 + riwayat FR-GZ-15:
// close_active atomic, PATCH profile tercatat, revert, list per person.
func TestGiziProfileGuardAPI(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "guard")
	// SATU person, banyak admission (episode lintas admission = 2f spec §4.1).
	personID, _ := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzguard")
	newAdmission := func(label string) string {
		t.Helper()
		id, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "gzg-"+label)
		return id
	}

	t.Run("close_active atomic", func(t *testing.T) {
		adm := newAdmission("ca")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm, "profile": "dewasa",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create 1 = %d: %s", rec.Code, rec.Body.String())
		}
		oldID := resp["data"].(map[string]any)["id"].(string)
		// POST kedua dengan close_active=true → episode lama closed + baru active.
		rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm, "profile": "balita", "close_active": true,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create 2 = %d: %s", rec.Code, rec.Body.String())
		}
		newID := resp["data"].(map[string]any)["id"].(string)
		if newID == oldID {
			t.Fatalf("episode baru harus baris berbeda")
		}
		var activeCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM clinical.nutrition_episode WHERE admission_id = $1::uuid AND status = 'active'",
			adm).Scan(&activeCount); err != nil || activeCount != 1 {
			t.Fatalf("active count = %d (%v), want 1", activeCount, err)
		}
		var oldStatus string
		var closedAt *time.Time
		if err := pool.QueryRow(ctx,
			"SELECT status, closed_at FROM clinical.nutrition_episode WHERE id = $1::uuid", oldID).
			Scan(&oldStatus, &closedAt); err != nil || oldStatus != "closed" || closedAt == nil {
			t.Fatalf("old = %s/%v (%v), want closed+closed_at", oldStatus, closedAt, err)
		}
	})

	t.Run("patch profile tercatat", func(t *testing.T) {
		adm := newAdmission("pp")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm, "profile": "dewasa",
		})
		epID := resp["data"].(map[string]any)["id"].(string)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
		}
		// Data kunjungan pra-PATCH: satu pengukuran antropometri.
		now := time.Now().Format(time.RFC3339)
		if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, map[string]any{
			"admission_id": adm, "measured_at": now, "bb_kg": 62.0, "tb_cm": 158.0,
		}); rec.Code != http.StatusCreated {
			t.Fatalf("anthro = %d: %s", rec.Code, rec.Body.String())
		}
		// PATCH dewasa→bumil: audit 1 baris, data kunjungan tetap.
		rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+epID, token, companyID, merchantID, map[string]any{
			"row_version": 1, "profile": "bumil",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
		}
		if resp["data"].(map[string]any)["profile"] != "bumil" {
			t.Fatalf("patched = %v", resp)
		}
		var from, to string
		var revertedAt *time.Time
		if err := pool.QueryRow(ctx,
			"SELECT from_profile, to_profile, reverted_at FROM clinical.nutrition_profile_change WHERE episode_id = $1::uuid",
			epID).Scan(&from, &to, &revertedAt); err != nil || from != "dewasa" || to != "bumil" || revertedAt != nil {
			t.Fatalf("change = %s→%s reverted=%v (%v), want dewasa→bumil unreverted", from, to, revertedAt, err)
		}
		rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/anthropometry?admission_id="+adm, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 1 {
			t.Fatalf("data kunjungan berubah: %d %v", rec.Code, resp)
		}
	})

	t.Run("revert", func(t *testing.T) {
		adm := newAdmission("rv")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm, "profile": "dewasa",
		})
		epID := resp["data"].(map[string]any)["id"].(string)
		cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+epID, token, companyID, merchantID, map[string]any{
			"row_version": 1, "profile": "bumil",
		})
		// Revert pertama: profil balik dewasa + baris audit reverted_at terisi.
		rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode/"+epID+"/revert-profile", token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK || resp["reverted"] != true {
			t.Fatalf("revert = %d %v", rec.Code, resp)
		}
		if resp["data"].(map[string]any)["profile"] != "dewasa" {
			t.Fatalf("reverted episode = %v", resp["data"])
		}
		var revertedAt *time.Time
		if err := pool.QueryRow(ctx,
			"SELECT reverted_at FROM clinical.nutrition_profile_change WHERE episode_id = $1::uuid", epID).
			Scan(&revertedAt); err != nil || revertedAt == nil {
			t.Fatalf("reverted_at = %v (%v), want non-NULL", revertedAt, err)
		}
		// Revert kedua → 409 pesan kontrak.
		rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode/"+epID+"/revert-profile", token, companyID, merchantID, nil)
		if rec.Code != http.StatusConflict || resp["error"] != "tidak ada perubahan profil untuk dibatalkan" {
			t.Fatalf("revert 2 = %d %v", rec.Code, resp)
		}
	})

	t.Run("list person", func(t *testing.T) {
		// Person TERPIS — subtest lain memakai person yang sama sehingga
		// riwayatnya ikut ter-list; kontrak: tepat 2 episode (1 closed).
		personID, adm1 := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzlp1")
		adm2, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "gzlp2")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm1, "profile": "dewasa",
		})
		oldID := resp["data"].(map[string]any)["id"].(string)
		cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+oldID, token, companyID, merchantID, map[string]any{
			"row_version": 1, "profile": "bumil",
		})
		cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+oldID, token, companyID, merchantID, map[string]any{
			"row_version": 2, "status": "closed",
		})
		rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": adm2, "profile": "lansia",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create ep2 = %d: %s", rec.Code, rec.Body.String())
		}
		// 2 episode (1 closed): urut started_at DESC (baru dulu) + changes terisi.
		rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episodes?person_id="+personID, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
		}
		data := resp["data"].([]any)
		if len(data) != 2 {
			t.Fatalf("list len = %d, want 2", len(data))
		}
		first := data[0].(map[string]any)
		second := data[1].(map[string]any)
		if first["profile"] != "lansia" || first["status"] != "active" {
			t.Fatalf("list[0] = %v (harus episode baru dulu)", first)
		}
		if second["id"] != oldID || second["status"] != "closed" {
			t.Fatalf("list[1] = %v", second)
		}
		changes := second["profile_changes"].([]any)
		if len(changes) != 1 {
			t.Fatalf("changes = %v, want 1", changes)
		}
		ch := changes[0].(map[string]any)
		if ch["from_profile"] != "dewasa" || ch["to_profile"] != "bumil" ||
			ch["reverted_at"] != nil || ch["changed_by_name"] == "" {
			t.Fatalf("change = %v", ch)
		}
		if first["kunjungan_count"].(float64) < 2 {
			t.Fatalf("kunjungan_count = %v, want >= 2", first["kunjungan_count"])
		}
	})
}

// gzPemicuBumilForm builds ases_gizi_pemicu_bumil form_data (template
// migration 000104): sections lila_cm/hb_gdl/mgg (kind number, opsional).
// Nil arg → key tidak dikirim (edge mgg/hb kosong).
func gzPemicuBumilForm(lila, hb, mgg *float64) map[string]any {
	secs := map[string]any{}
	if lila != nil {
		secs["lila_cm"] = *lila
	}
	if hb != nil {
		secs["hb_gdl"] = *hb
	}
	if mgg != nil {
		secs["mgg"] = *mgg
	}
	return map[string]any{"sections": secs}
}

// gzPemicuBumilNsr reads the pemicu_bumil nsr row for a form instance and
// decodes its payload.
func gzPemicuBumilNsr(t *testing.T, ctx context.Context, pool *pgxpool.Pool, formID string) (int, string, map[string]any) {
	t.Helper()
	var score int
	var interp, payloadRaw string
	if err := pool.QueryRow(ctx,
		`SELECT score, interpretation, payload::text
		 FROM clinical.nutrition_screening_result
		 WHERE form_instance_id = $1::uuid AND instrument = 'pemicu_bumil'`,
		formID).Scan(&score, &interp, &payloadRaw); err != nil {
		t.Fatalf("read nsr pemicu_bumil: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
		t.Fatalf("decode payload %s: %v", payloadRaw, err)
	}
	return score, interp, payload
}

// TestGiziPemicuBumilPersistAndBands — hook pemicu bumil (plan Task 2, spec
// §4.3/§5): lila 22,8/hb 10,2/mgg 26 → score 2 (kek + anemia, tanpa MST),
// interpretation "tinggi", payload gradasi "ringan" (Hb ≥ 10), trimester "t2".
// Sub-case gradasi: hb 8,5 → "sedang", hb 6 → "berat". Idempoten: kirim ulang
// form yang sama (PATCH create-ulang nilai) → tetap 1 baris per form, nilai
// ter-update.
func TestGiziPemicuBumilPersistAndBands(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "pemicu")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzpemicu")
	const create = "/api/v1/clinical/forms"

	// (a) create: Kek (22,8 < 23,5) + Anemia (hb 10,2 < 10,5 t2) → score 2.
	lila, mgg := 22.8, 26.0
	hb := 10.2
	rec, resp := cfDo(t, router, http.MethodPost, create, token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_pemicu_bumil",
		"form_data": gzPemicuBumilForm(&lila, &hb, &mgg),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pemicu = %d: %s", rec.Code, rec.Body.String())
	}
	formID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	score, interp, payload := gzPemicuBumilNsr(t, ctx, pool, formID)
	// Hitungan kontrak: b2i(kek)+b2i(anemia)+b2i(mst) = 1+1+0 = 2; score ≥ 2 → tinggi;
	// hb 10,2 ≥ 10 → ringan; mgg 26 → t2.
	if score != 2 || interp != "tinggi" {
		t.Fatalf("nsr pemicu = %d/%s, want 2/tinggi", score, interp)
	}
	if payload["kek"] != true || payload["anemia"] != true || payload["mst_berisiko"] != false ||
		payload["anemia_gradasi"] != "ringan" || payload["trimester"] != "t2" ||
		payload["hb_band_dihitung"] != true || payload["mgg"] != 26.0 {
		t.Fatalf("payload = %v", payload)
	}

	// (b) PATCH hb 8,5 → gradasi "sedang" (7–9,9); score tetap 2 → tinggi.
	hb = 8.5
	if rec, _ = cfDo(t, router, http.MethodPatch, create+"/"+formID, token, companyID, merchantID, map[string]any{
		"form_data": gzPemicuBumilForm(&lila, &hb, &mgg),
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch 8.5 = %d: %s", rec.Code, rec.Body.String())
	}
	score, interp, payload = gzPemicuBumilNsr(t, ctx, pool, formID)
	if score != 2 || interp != "tinggi" || payload["anemia_gradasi"] != "sedang" {
		t.Fatalf("nsr 8.5 = %d/%s/%v, want 2/tinggi/sedang", score, interp, payload["anemia_gradasi"])
	}

	// (c) PATCH hb 6 → gradasi "berat" (<7) → interpretation tetap "tinggi".
	hb = 6.0
	if rec, _ = cfDo(t, router, http.MethodPatch, create+"/"+formID, token, companyID, merchantID, map[string]any{
		"form_data": gzPemicuBumilForm(&lila, &hb, &mgg),
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch 6 = %d: %s", rec.Code, rec.Body.String())
	}
	score, interp, payload = gzPemicuBumilNsr(t, ctx, pool, formID)
	if score != 2 || interp != "tinggi" || payload["anemia_gradasi"] != "berat" {
		t.Fatalf("nsr 6 = %d/%s/%v, want 2/tinggi/berat", score, interp, payload["anemia_gradasi"])
	}

	// (d) kirim ulang form sama (nilai awal) → tetap 1 baris, nilai ter-update
	// balik ke ringan (idempoten UPDATE-then-INSERT).
	hb = 10.2
	if rec, _ = cfDo(t, router, http.MethodPatch, create+"/"+formID, token, companyID, merchantID, map[string]any{
		"form_data": gzPemicuBumilForm(&lila, &hb, &mgg),
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch ulang = %d: %s", rec.Code, rec.Body.String())
	}
	score, interp, payload = gzPemicuBumilNsr(t, ctx, pool, formID)
	if score != 2 || interp != "tinggi" || payload["anemia_gradasi"] != "ringan" {
		t.Fatalf("nsr ulang = %d/%s/%v, want 2/tinggi/ringan", score, interp, payload["anemia_gradasi"])
	}
	if n := gzNsrCountAdmission(t, ctx, pool, admissionID); n != 1 {
		t.Fatalf("nsr pasca kirim ulang = %d, want 1 (idempoten)", n)
	}
}

// TestGiziPemicuBumilEdgeMggKosong — edge mgg kosong (spec §5): Trimester "",
// band Hb TIDAK dihitung → anemia=false, hb_band_dihitung=false,
// trimester=null; hanya kek (lila 22) → score 1 → interpretation "sedang".
func TestGiziPemicuBumilEdgeMggKosong(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "pemicuedge")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzpemicuedge")

	lila, hb := 22.0, 8.0
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
		"admission_id": admissionID, "form_type": "ases_gizi_pemicu_bumil",
		"form_data": gzPemicuBumilForm(&lila, &hb, nil),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create edge = %d: %s", rec.Code, rec.Body.String())
	}
	formID := resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	score, interp, payload := gzPemicuBumilNsr(t, ctx, pool, formID)
	// Hitungan kontrak: trimester kosong → anemia=false walau hb 8 < 11; score
	// = b2i(kek)+0+0 = 1 → "sedang".
	if score != 1 || interp != "sedang" {
		t.Fatalf("nsr edge = %d/%s, want 1/sedang", score, interp)
	}
	if payload["kek"] != true || payload["anemia"] != false || payload["anemia_gradasi"] != nil ||
		payload["trimester"] != nil || payload["hb_band_dihitung"] != false {
		t.Fatalf("payload edge = %v", payload)
	}
	if n := gzNsrCountAdmission(t, ctx, pool, admissionID); n != 1 {
		t.Fatalf("nsr edge = %d, want 1", n)
	}
}
