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
