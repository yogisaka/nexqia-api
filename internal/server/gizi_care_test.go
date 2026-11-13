//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

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
