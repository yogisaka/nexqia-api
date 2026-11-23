//go:build integration

package server_test

// Kebidanan episode tests (plan 2026-11-22-kebidanan-anc-f1 Task 2, spec
// §5/§7): pola gizi_care_test.go — fixture reuse helper shared
// (newTestPostgresPool, gzSeedEnv, cfSeedAdmission, cfDo,
// cfSeedUserWithPerms); tiap test punya container Postgres sendiri. Nilai
// ekspektasi dihitung dari DEFINISI kontrak: hpl = hpht + 280 hari
// (2026-08-01 + 280d = 2027-05-08; 2026-07-01 + 280d = 2027-04-07), ordinal =
// count episode person + 1.
import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

var kebCounter int64

func kebNext() int64 { return atomic.AddInt64(&kebCounter, 1) }

func kebCreateBody(admissionID, hpht string, closeActive bool) map[string]any {
	body := map[string]any{
		"admission_id": admissionID, "gravida": 2, "para": 1, "abortus": 0,
		"riwayat_persalinan": "spontan 2023", "jarak_kehamilan_th": 3, "hpht": hpht,
	}
	if closeActive {
		body["close_active"] = true
	}
	return body
}

// kebSeedVisit inserts one ANC visit row directly (fixture SQL) so the
// latest_visit/kunjungan_count read models have data; kontrak: visited_at,
// tfu_cm, djj tampil di episodeJSON.
func kebSeedVisit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, episodeID, admissionID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO clinical.midwifery_anc_visit
			(company_id, merchant_id, care_episode_id, admission_id, visited_at, tfu_cm, djj, tanda_bahaya, jiwa)
		VALUES ($1, $2, $3, $4, now(), 12.5, 140, '[]'::jsonb, '{}'::jsonb)`,
		companyID, merchantID, episodeID, admissionID); err != nil {
		t.Fatalf("seed anc visit: %v", err)
	}
}

// TestKebidananEpisodeCreateGuardConflict — create 201 (ordinal 1, hpl =
// hpht+280), dobel aktif tanpa close_active → 409 "episode aktif masih ada".
func TestKebidananEpisodeCreateGuardConflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep1")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep1")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	ep := resp["data"].(map[string]any)
	// Kontrak: hpl = hpht + 280 hari = 2026-08-01 + 280 = 2027-05-08.
	if ep["ordinal_no"].(float64) != 1 || ep["status"] != "active" ||
		ep["gravida"].(float64) != 2 || ep["para"].(float64) != 1 || ep["abortus"].(float64) != 0 ||
		ep["hpht"] != "2026-08-01" || ep["hpl"] != "2027-05-08" ||
		ep["uk_source"] != "hpht" || ep["row_version"].(float64) != 0 || // row_version awal = 0 (touch trigger hanya +1 pada UPDATE)
		ep["riwayat_persalinan"] != "spontan 2023" || gzNum(t, ep["jarak_kehamilan_th"]) != 3 ||
		ep["latest_visit"] != nil {
		t.Fatalf("episode = %v", ep)
	}
	if _, ok := ep["uk_weeks"].(float64); !ok {
		t.Fatalf("uk_weeks harus number: %v", ep["uk_weeks"])
	}

	// Dobel aktif tanpa close_active → 409 pesan kontrak.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusConflict || resp["error"] != "episode aktif masih ada" {
		t.Fatalf("dobel aktif = %d %v", rec.Code, resp)
	}
}

// TestKebidananEpisodeCloseAndNew — step-2: confirm wajib (400), close+new
// dalam satu tx (2 write): ordinal baru = 2, aktif berpindah.
func TestKebidananEpisodeCloseAndNew(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep2")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep2")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	ep1 := resp["data"].(map[string]any)
	episode1 := ep1["id"].(string)
	kebSeedVisit(t, ctx, pool, companyID, merchantID, episode1, admissionID)

	// GET episode aktif → latest_visit dari kunjungan terakhir.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	got := resp["data"].(map[string]any)
	lv := got["latest_visit"].(map[string]any)
	if gzNum(t, lv["tfu_cm"]) != 12.5 || gzNum(t, lv["djj"]) != 140 || lv["visited_at"] == nil {
		t.Fatalf("latest_visit = %v", lv)
	}
	if got["person_id"] != personID {
		t.Fatalf("person_id = %v, want %s", got["person_id"], personID)
	}

	// confirm:false → 400 "konfirmasi wajib".
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episode/"+episode1+"/close", token, companyID, merchantID,
		map[string]any{"confirm": false})
	if rec.Code != http.StatusBadRequest || resp["error"] != "konfirmasi wajib" {
		t.Fatalf("close tanpa confirm = %d %v", rec.Code, resp)
	}

	// close + new_pregnancy → closed + baru ordinal 2 (2 write, satu tx).
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episode/"+episode1+"/close", token, companyID, merchantID,
		map[string]any{"confirm": true, "new_pregnancy": map[string]any{
			"gravida": 3, "para": 1, "abortus": 1, "jarak_kehamilan_th": 1, "hpht": "2026-09-01",
		}})
	if rec.Code != http.StatusOK {
		t.Fatalf("close+new = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)
	closed := data["closed"].(map[string]any)
	if closed["status"] != "closed" || closed["closed_at"] == nil {
		t.Fatalf("closed = %v", closed)
	}
	newEp := data["new_episode"].(map[string]any)
	// Kontrak: ordinal baru = count episode person + 1 = 2.
	if newEp["ordinal_no"].(float64) != 2 || newEp["status"] != "active" ||
		newEp["gravida"].(float64) != 3 || newEp["abortus"].(float64) != 1 ||
		newEp["hpht"] != "2026-09-01" || newEp["hpl"] != "2027-06-08" {
		t.Fatalf("new_episode = %v", newEp)
	}

	// GET kini mengembalikan episode ordinal 2.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || resp["data"].(map[string]any)["ordinal_no"].(float64) != 2 {
		t.Fatalf("get after close = %d %v", rec.Code, resp)
	}
}

// TestKebidananEpisodeGuard — dobel aktif 409; close (close_active create
// jalur 1) lalu create sukses dengan ordinal+1, episode lama closed.
func TestKebidananEpisodeGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep3")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep3")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	first := resp["data"].(map[string]any)["id"].(string)

	// Jalur 1: close_active=true menutup aktif + create baru (satu request).
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-15", true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create close_active = %d: %s", rec.Code, rec.Body.String())
	}
	second := resp["data"].(map[string]any)
	if second["ordinal_no"].(float64) != 2 || second["id"] == first {
		t.Fatalf("close_active create = %v", second)
	}

	// Riwayat: ordinal 1 closed (close_context {"reason":"ganti_episode"}).
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episodes?person_id="+personIDof(t, ctx, pool, admissionID), token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["ordinal_no"].(float64) != 2 ||
		items[1].(map[string]any)["status"] != "closed" || items[1].(map[string]any)["closed_at"] == nil {
		t.Fatalf("list = %v", items)
	}
}

// TestKebidananEpisodePatchRowVersion — 409 row_version konflik; PATCH hpht →
// hpl recompute (2026-07-01 + 280 = 2027-04-07); PATCH usg override → uk_weeks.
func TestKebidananEpisodePatchRowVersion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep4")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep4")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	episodeID := resp["data"].(map[string]any)["id"].(string)

	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/midwifery/episode/"+episodeID, token, companyID, merchantID,
		map[string]any{"row_version": 99, "hpht": "2026-07-01"})
	if rec.Code != http.StatusConflict || resp["error"] != "row_version konflik" {
		t.Fatalf("rv conflict = %d %v", rec.Code, resp)
	}

	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/midwifery/episode/"+episodeID, token, companyID, merchantID,
		map[string]any{"row_version": 0, "hpht": "2026-07-01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	patched := resp["data"].(map[string]any)
	// Kontrak: recompute hpl bila hpht berubah = 2026-07-01 + 280 hari;
	// row_version naik 0 → 1 (touch trigger).
	if patched["hpht"] != "2026-07-01" || patched["hpl"] != "2027-04-07" ||
		patched["row_version"].(float64) != 1 {
		t.Fatalf("patched = %v", patched)
	}

	// Override USG T1 → uk_weeks = nilai override.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/midwifery/episode/"+episodeID, token, companyID, merchantID,
		map[string]any{"row_version": 1, "uk_source": "usg_t1", "uk_usg_t1_weeks": 12.5})
	if rec.Code != http.StatusOK || resp["data"].(map[string]any)["uk_source"] != "usg_t1" ||
		resp["data"].(map[string]any)["row_version"].(float64) != 2 ||
		gzNum(t, resp["data"].(map[string]any)["uk_weeks"]) != 12.5 {
		t.Fatalf("patch usg = %d %v", rec.Code, resp)
	}

	// uk_source tidak valid → 400.
	rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/midwifery/episode/"+episodeID, token, companyID, merchantID,
		map[string]any{"row_version": 2, "uk_source": "bogus"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("uk_source bogus = %d %v", rec.Code, resp)
	}
}

// TestKebidananEpisodeList — riwayat per person desc ordinal, kunjungan_count
// per episode, outcome null (F2-ready, belum ada outcome).
func TestKebidananEpisodeList(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep5")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep5")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	episode1 := resp["data"].(map[string]any)["id"].(string)
	kebSeedVisit(t, ctx, pool, companyID, merchantID, episode1, admissionID)

	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episode/"+episode1+"/close", token, companyID, merchantID,
		map[string]any{"confirm": true, "new_pregnancy": map[string]any{
			"gravida": 3, "para": 2, "abortus": 0, "hpht": "2026-10-01",
		}})
	if rec.Code != http.StatusOK {
		t.Fatalf("close+new = %d: %s", rec.Code, rec.Body.String())
	}

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episodes?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("list len = %d", len(items))
	}
	// Desc ordinal: [0] = baru (ordinal 2, active, 0 kunjungan),
	// [1] = lama (ordinal 1, closed, 1 kunjungan); outcome null (F2-ready).
	ep2 := items[0].(map[string]any)
	ep1 := items[1].(map[string]any)
	if ep2["ordinal_no"].(float64) != 2 || ep2["status"] != "active" || ep2["outcome"] != nil ||
		ep2["kunjungan_count"].(float64) != 0 || ep2["started_at"] == nil {
		t.Fatalf("ep2 = %v", ep2)
	}
	if ep1["ordinal_no"].(float64) != 1 || ep1["status"] != "closed" || ep1["outcome"] != nil ||
		ep1["kunjungan_count"].(float64) != 1 || ep1["closed_at"] == nil {
		t.Fatalf("ep1 = %v", ep1)
	}

	// person tanpa episode → array kosong (bukan 404).
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		map[string]any{"admission_id": admissionID, "gravida": 9, "para": 0, "abortus": 0, "hpht": "2026-01-01"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("guard after close+new = %d %v", rec.Code, resp)
	}
}

// TestKebidananEpisodePermission — write butuh clinical.midwifery.write,
// view butuh clinical.record.view (403 tanpa izin).
func TestKebidananEpisodePermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep6")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep6")

	// Role hanya record.view → POST 403.
	viewOnly := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "kep6view", "clinical.record.view")
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", viewOnly, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewOnly create = %d: %s", rec.Code, rec.Body.String())
	}

	// Role hanya midwifery.write → GET episode/list 403.
	writeOnly := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "kep6write", "clinical.midwifery.write")
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episode?admission_id="+admissionID, writeOnly, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeOnly get = %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episodes?person_id=00000000-0000-0000-0000-000000000001", writeOnly, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeOnly list = %d: %s", rec.Code, rec.Body.String())
	}

	// Owner token (punya kedua izin dari template role Bidan + Owner) lolos.
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episode?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner get = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKebidananEpisodeRLS — tenant silang: admission tak terlihat (404),
// list kosong, PATCH/close episode tenant lain 404 (pola RLS existing).
func TestKebidananEpisodeRLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kep7")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kep7")
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, "2026-08-01", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	episodeID := resp["data"].(map[string]any)["id"].(string)

	companyB, merchantB, _, tokenB := gzSeedEnv(t, ctx, pool, router, fmt.Sprintf("kep7b%d", kebNext()))
	_ = personID
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episode?admission_id="+admissionID, tokenB, companyB, merchantB, nil)
	if rec.Code != http.StatusNotFound || resp["error"] != "admission tidak ditemukan" {
		t.Fatalf("cross get = %d %v", rec.Code, resp)
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/episodes?person_id="+personID, tokenB, companyB, merchantB, nil)
	crossList, okArr := resp["data"].([]any)
	if rec.Code != http.StatusOK || !okArr || len(crossList) != 0 {
		t.Fatalf("cross list = %d %v", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/midwifery/episode/"+episodeID, tokenB, companyB, merchantB,
		map[string]any{"row_version": 1, "hpht": "2026-07-01"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross patch = %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episode/"+episodeID+"/close", tokenB, companyB, merchantB,
		map[string]any{"confirm": true})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross close = %d: %s", rec.Code, rec.Body.String())
	}
}

// personIDof reads the person of an admission directly (RLS-free owner pool).
func personIDof(t *testing.T, ctx context.Context, pool *pgxpool.Pool, admissionID string) string {
	t.Helper()
	var personID string
	if err := pool.QueryRow(ctx,
		"SELECT person_id::text FROM operations.admission WHERE id = $1::uuid", admissionID).Scan(&personID); err != nil {
		t.Fatalf("read person: %v", err)
	}
	return personID
}

// kebNewEpisode — buat episode aktif via API untuk test Task 3 (kunjungan,
// USG, PE); return episode id.
func kebNewEpisode(t *testing.T, router *gin.Engine, token, companyID, merchantID, admissionID, hpht string) string {
	t.Helper()
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/episodes", token, companyID, merchantID,
		kebCreateBody(admissionID, hpht, false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create episode = %d: %s", rec.Code, rec.Body.String())
	}
	return resp["data"].(map[string]any)["id"].(string)
}

// TestKebidananAncVisitCrud — POST kunjungan (echo field kontrak), jiwa jsonb
// invalid → 400, GET list visited_at DESC.
func TestKebidananAncVisitCrud(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kea1")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kea1")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")

	body := map[string]any{
		"episode_id": episodeID, "admission_id": admissionID,
		"tfu_cm": 12.5, "djj": 140, "presentation": "kepala", "placenta_location": "fundus",
		"inspekulo":         map[string]any{"porsio": "normal", "vagina": "normal", "vulva": nil},
		"tanda_bahaya":      []string{"pendarahan"},
		"jiwa":              map[string]any{"q1": false, "q2": true, "q3": false, "q4": false, "tindak_lanjut": "edukasi"},
		"imunisasi_lainnya": "COVID-19 dosis 1", "next_visit_date": "2026-09-15",
	}
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create visit = %d: %s", rec.Code, rec.Body.String())
	}
	v := resp["data"].(map[string]any)
	if v["episode_id"] != episodeID || v["admission_id"] != admissionID || v["visited_at"] == nil ||
		gzNum(t, v["tfu_cm"]) != 12.5 || gzNum(t, v["djj"]) != 140 ||
		v["presentation"] != "kepala" || v["placenta_location"] != "fundus" ||
		v["imunisasi_lainnya"] != "COVID-19 dosis 1" || v["next_visit_date"] != "2026-09-15" {
		t.Fatalf("visit = %v", v)
	}
	jiwa := v["jiwa"].(map[string]any)
	if jiwa["q2"] != true || jiwa["tindak_lanjut"] != "edukasi" {
		t.Fatalf("jiwa = %v", jiwa)
	}

	// jiwa bentuk invalid (tindak_lanjut di luar enum) → 400.
	bad := map[string]any{"episode_id": episodeID, "admission_id": admissionID,
		"jiwa": map[string]any{"q1": true, "tindak_lanjut": "bogus"}}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", token, companyID, merchantID, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("jiwa invalid = %d: %s", rec.Code, rec.Body.String())
	}
	// key asing di jiwa → 400.
	bad2 := map[string]any{"episode_id": episodeID, "admission_id": admissionID,
		"jiwa": map[string]any{"q9": true}}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", token, companyID, merchantID, bad2)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("jiwa key asing = %d: %s", rec.Code, rec.Body.String())
	}

	// Kunjungan kedua + GET list (DESC: kunjungan terbaru dulu) — urutan
	// dijamin by visited_at DESC; 2 baris.
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "admission_id": admissionID, "tfu_cm": 14})
	if rec.Code != http.StatusCreated {
		t.Fatalf("visit 2 = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/anc-visit?episode_id="+episodeID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("list len = %d", len(items))
	}
	if gzNum(t, items[0].(map[string]any)["tfu_cm"]) != 14 ||
		gzNum(t, items[1].(map[string]any)["tfu_cm"]) != 12.5 {
		t.Fatalf("list order = %v", items)
	}

	// episode_id invalid → 400.
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/anc-visit?episode_id=xyz", token, companyID, merchantID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("list invalid id = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKebidananUsg — POST USG T1 (echo biometri), GET list desc tanggal,
// trimester invalid → 400.
func TestKebidananUsg(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kea2")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kea2")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")

	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/usg", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "trimester": "t1", "usg_date": "2026-09-10",
			"crl_mm": 55.2, "gs_mm": 24, "bpd_mm": 21, "hc_mm": 62, "ac_mm": 58, "fl_mm": 44, "efw_g": 480, "sdp": "normal"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create usg = %d: %s", rec.Code, rec.Body.String())
	}
	u := resp["data"].(map[string]any)
	// Kontrak echo: usg_date literal, biometri number, efw_g int.
	if u["trimester"] != "t1" || u["usg_date"] != "2026-09-10" || gzNum(t, u["crl_mm"]) != 55.2 ||
		gzNum(t, u["hc_mm"]) != 62 || gzNum(t, u["fl_mm"]) != 44 || gzNum(t, u["efw_g"]) != 480 || u["sdp"] != "normal" {
		t.Fatalf("usg = %v", u)
	}

	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/usg", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "trimester": "t3", "usg_date": "2026-12-20", "efw_g": 2800})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create usg t3 = %d: %s", rec.Code, rec.Body.String())
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/usg?episode_id="+episodeID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list usg = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["usg_date"] != "2026-12-20" ||
		items[1].(map[string]any)["usg_date"] != "2026-09-10" {
		t.Fatalf("list usg = %v", items)
	}

	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/usg", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "trimester": "t2", "usg_date": "2026-10-01"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trimester t2 = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKebidananPeScreenBand — RULE LITERAL: jumlah faktor true 0 → rendah
// (flag false), 1 → sedang (flag true), ≥2 → tinggi (flag true); GET list
// terbaru dulu.
func TestKebidananPeScreenBand(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kea3")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kea3")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")

	post := func(factors map[string]bool) map[string]any {
		return map[string]any{"episode_id": episodeID, "factors": factors}
	}
	// 0 faktor → rendah, rujukan_flag false.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", token, companyID, merchantID,
		post(map[string]bool{}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pe 0 = %d: %s", rec.Code, rec.Body.String())
	}
	d := resp["data"].(map[string]any)
	if d["band"] != "rendah" || d["rujukan_flag"] != false || d["screened_at"] == nil {
		t.Fatalf("band rendah = %v", d)
	}
	if f := d["factors"].(map[string]any); f["primigravida"] != false {
		t.Fatalf("factors = %v", f)
	}

	// 1 faktor → sedang, flag true.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", token, companyID, merchantID,
		post(map[string]bool{"primigravida": true}))
	if rec.Code != http.StatusCreated || resp["data"].(map[string]any)["band"] != "sedang" ||
		resp["data"].(map[string]any)["rujukan_flag"] != true {
		t.Fatalf("band sedang = %d %v", rec.Code, resp)
	}

	// 2 faktor → tinggi, flag true.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", token, companyID, merchantID,
		post(map[string]bool{"primigravida": true, "imt_gt30": true}))
	if rec.Code != http.StatusCreated || resp["data"].(map[string]any)["band"] != "tinggi" ||
		resp["data"].(map[string]any)["rujukan_flag"] != true {
		t.Fatalf("band tinggi = %d %v", rec.Code, resp)
	}

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/pe-screen?episode_id="+episodeID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pe = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].([]any)
	if len(items) != 3 ||
		items[0].(map[string]any)["band"] != "tinggi" ||
		items[1].(map[string]any)["band"] != "sedang" ||
		items[2].(map[string]any)["band"] != "rendah" {
		t.Fatalf("list pe order = %v", items)
	}

	// factors hilang → 400.
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("factors hilang = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKebidananAncVisitPermission — write butuh clinical.midwifery.write,
// view butuh clinical.record.view (403).
func TestKebidananAncVisitPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kea4")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kea4")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")

	viewOnly := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "kea4view", "clinical.record.view")
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", viewOnly, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "admission_id": admissionID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewOnly visit = %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", viewOnly, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "factors": map[string]bool{}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewOnly pe = %d: %s", rec.Code, rec.Body.String())
	}
	writeOnly := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "kea4write", "clinical.midwifery.write")
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/anc-visit?episode_id="+episodeID, writeOnly, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeOnly list visit = %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/usg?episode_id="+episodeID, writeOnly, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("writeOnly list usg = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKebidananAncVisitRLS — tenant silang: POST kunjungan (admission tak
// terlihat → 404), GET visit/usg/pe episode tenant lain → 404.
func TestKebidananAncVisitRLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kea5")
	_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kea5")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")

	companyB, merchantB, _, tokenB := gzSeedEnv(t, ctx, pool, router, fmt.Sprintf("kea5b%d", kebNext()))
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/anc-visit", tokenB, companyB, merchantB,
		map[string]any{"episode_id": episodeID, "admission_id": admissionID})
	if rec.Code != http.StatusNotFound || resp["error"] != "admission tidak ditemukan" {
		t.Fatalf("cross visit = %d %v", rec.Code, resp)
	}
	for _, path := range []string{
		"/api/v1/clinical/midwifery/anc-visit?episode_id=",
		"/api/v1/clinical/midwifery/usg?episode_id=",
		"/api/v1/clinical/midwifery/pe-screen?episode_id=",
	} {
		rec, _ = cfDo(t, router, http.MethodGet, path+episodeID, tokenB, companyB, merchantB, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("cross GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", tokenB, companyB, merchantB,
		map[string]any{"episode_id": episodeID, "factors": map[string]bool{}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross pe = %d: %s", rec.Code, rec.Body.String())
	}
}

// ---- Task 4: vaksin Td + anc-summary + timeline kinds (plan step 4) ----

// TestKebidananVaksin — dosis_ke SERVER (ordinal berikutnya); interval <30d →
// 422 + TIDAK tersimpan; band 2 dosis → protect_until = tahun given+3 (jadwal
// Td dosis 2 masa_perlindungan 3 th, migration 000109); riwayat kosong →
// belum_lengkap + next.dosis_ke=1.
func TestKebidananVaksin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kev1")
	personID, _ := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kev1")

	vaksinBody := func(given string) map[string]any {
		return map[string]any{"person_id": personID, "antigen_slug": "td",
			"given_date": given, "tempat": "faskes_ini", "no_batch": "TD-001", "pemberi": "Bidan A"}
	}
	// Dosis 1: 201, dosis_ke dihitung server = 1.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		vaksinBody("2026-01-01"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("vaksin 1 = %d: %s", rec.Code, rec.Body.String())
	}
	if d := resp["data"].(map[string]any); d["dosis_ke"].(float64) != 1 || d["given_date"] != "2026-01-01" ||
		d["tempat"] != "faskes_ini" || d["no_batch"] != "TD-001" {
		t.Fatalf("vaksin 1 data = %v", d)
	}
	// Interval <30d: given 2026-01-15, syarat 2026-01-01+30 = 2026-01-31 →
	// sisa 16 hari → 422 "interval Td belum terpenuhi (min 16 hari)".
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		vaksinBody("2026-01-15"))
	if rec.Code != http.StatusUnprocessableEntity || resp["error"] != "interval Td belum terpenuhi (min 16 hari)" {
		t.Fatalf("interval = %d %v", rec.Code, resp)
	}
	// TIDAK tersimpan: GET masih 1 dosis.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-vaccination?person_id="+personID+"&antigen=td", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	dosis := resp["data"].(map[string]any)["dosis"].([]any)
	if len(dosis) != 1 {
		t.Fatalf("dosis len = %d (422 harus tidak tersimpan)", len(dosis))
	}
	// Dosis 2: 2026-02-15 = 45 hari setelah dosis 1 (≥30) → 201, dosis_ke 2.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		vaksinBody("2026-02-15"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("vaksin 2 = %d: %s", rec.Code, rec.Body.String())
	}
	if resp["data"].(map[string]any)["dosis_ke"].(float64) != 2 {
		t.Fatalf("ordinal = %v", resp["data"])
	}
	// Band 2 dosis: dosis terakhir 2, masa 3 th → aktif, protect_until
	// 2026+3 = "2029"; next = dosis 3 min_date 2026-02-15+180 = 2026-08-14.
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-vaccination?person_id="+personID+"&antigen=td", token, companyID, merchantID, nil)
	band := resp["data"].(map[string]any)["band"].(map[string]any)
	if band["status"] != "aktif" || band["protect_until"] != "2029" {
		t.Fatalf("band 2 dosis = %v", band)
	}
	next := band["next"].(map[string]any)
	if next["dosis_ke"].(float64) != 3 || next["min_date"] != "2026-08-14" {
		t.Fatalf("next = %v", next)
	}

	// Riwayat kosong → belum_lengkap + next.dosis_ke=1.
	person2, _ := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, fmt.Sprintf("kev1b%d", kebNext()))
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/person-vaccination?person_id="+person2+"&antigen=td", token, companyID, merchantID, nil)
	band = resp["data"].(map[string]any)["band"].(map[string]any)
	if band["status"] != "belum_lengkap" || band["protect_until"] != nil {
		t.Fatalf("band kosong = %v", band)
	}
	if n := band["next"].(map[string]any); n["dosis_ke"].(float64) != 1 {
		t.Fatalf("next kosong = %v", n)
	}
}

// TestKebidananAncSummary — read-model G4AncData: episode aktif + latest visit
// (TFU 22 / DJJ 140) + latest USG (hc 62 = usg_lk, fl 44) → snake_case persis;
// tanpa episode aktif → 200 semua null.
func TestKebidananAncSummary(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "kes1")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "kes1")

	// Tanpa episode aktif → semua null.
	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/anc-summary?person_id="+personID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary kosong = %d: %s", rec.Code, rec.Body.String())
	}
	empty := resp["data"].(map[string]any)
	for _, k := range []string{"gravida", "para", "tfu_cm", "tfu_at", "djj", "djj_at", "usg_lk_mm", "usg_fl_mm", "usg_at", "episode_ordinal"} {
		if empty[k] != nil {
			t.Fatalf("summary kosong %s = %v", k, empty[k])
		}
	}

	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")
	// Fixture visit langsung (TFU 22 / DJJ 140; visited_at = now()).
	if _, err := pool.Exec(ctx, `
		INSERT INTO clinical.midwifery_anc_visit
			(company_id, merchant_id, care_episode_id, admission_id, visited_at, tfu_cm, djj)
		VALUES ($1, $2, $3, $4, now(), 22, 140)`,
		companyID, merchantID, episodeID, admissionID); err != nil {
		t.Fatalf("seed visit: %v", err)
	}
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/usg", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "trimester": "t1", "usg_date": "2026-09-10", "hc_mm": 62, "fl_mm": 44})
	if rec.Code != http.StatusCreated {
		t.Fatalf("usg = %d: %s", rec.Code, rec.Body.String())
	}

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/midwifery/anc-summary?person_id="+personID, token, companyID, merchantID, nil)
	s := resp["data"].(map[string]any)
	if s["gravida"].(float64) != 2 || s["para"].(float64) != 1 || s["episode_ordinal"].(float64) != 1 ||
		gzNum(t, s["tfu_cm"]) != 22 || gzNum(t, s["djj"]) != 140 ||
		gzNum(t, s["usg_lk_mm"]) != 62 || gzNum(t, s["usg_fl_mm"]) != 44 ||
		s["usg_at"] != "2026-09-10" || s["tfu_at"] == nil || s["djj_at"] == nil {
		t.Fatalf("summary = %v", s)
	}
}

// TestClinicalTimelineKebidanan — KETIGA kind kebidanan muncul di feed
// clinical-timeline admission: midwifery_visit, midwifery_pe_screen,
// person_vaccination.
func TestClinicalTimelineKebidanan(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "ket1")
	personID, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "ket1")
	episodeID := kebNewEpisode(t, router, token, companyID, merchantID, admissionID, "2026-08-01")
	kebSeedVisit(t, ctx, pool, companyID, merchantID, episodeID, admissionID)
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/midwifery/pe-screen", token, companyID, merchantID,
		map[string]any{"episode_id": episodeID, "factors": map[string]bool{"primigravida": true}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("pe = %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		map[string]any{"person_id": personID, "antigen_slug": "td", "given_date": "2026-03-01", "tempat": "luar"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("vaksin = %d: %s", rec.Code, rec.Body.String())
	}

	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/admissions/"+admissionID+"/clinical-timeline", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline = %d: %s", rec.Code, rec.Body.String())
	}
	items := resp["data"].(map[string]any)["items"].([]any)
	seen := map[string]bool{}
	for _, it := range items {
		if k, ok := it.(map[string]any)["kind"].(string); ok {
			seen[k] = true
		}
	}
	for _, kind := range []string{"midwifery_visit", "midwifery_pe_screen", "person_vaccination"} {
		if !seen[kind] {
			t.Fatalf("kind %s tidak ada di timeline (seen=%v)", kind, seen)
		}
	}
}
