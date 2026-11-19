//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestGiziGrowthContext — plan 2026-11-19-gizi-balita-1-be Task 2 (spec
// 2026-11-19-gizi-balita-revisi-design §3–§4, §6): POST
// /clinical/person-growth-context (upsert idempoten), growth_context di GET
// nutrition-episode, dan trigger snapshot v3 (koreksi usia prematur,
// migration 000106). Nilai ekspektasi dihitung dari definisi kontrak
// (rumus LMS + koreksi Task 1), bukan dari output handler. Fixture ref
// kurva SINTETIS (bukan WHO) per subtest dengan source_version unik +
// cleanup supaya nearest-x antar subtest tidak saling mengganggu.
func TestGiziGrowthContext(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	// --- Tenant minimal ala TestGiziAnthropometrySnapshot (pool owner):
	// company -> merchant -> department; person per subtest.
	var companyID, merchantID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ('GZC', 'Gizi Ctx Co', 10) RETURNING id::text",
	).Scan(&companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.merchant (company_id, code, name) VALUES ($1, 'gzctx', 'Gizi Ctx Merchant') RETURNING id::text",
		companyID).Scan(&merchantID); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, 'GZC-DEP', 'Poli Ctx')",
		companyID, merchantID); err != nil {
		t.Fatalf("seed department: %v", err)
	}

	// Tenant kedua untuk tes isolasi RLS (g).
	var companyB, merchantB string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ('GZCB', 'Gizi Ctx B', 10) RETURNING id::text",
	).Scan(&companyB); err != nil {
		t.Fatalf("seed company B: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.merchant (company_id, code, name) VALUES ($1, 'gzctxb', 'Gizi Ctx Merchant B') RETURNING id::text",
		companyB).Scan(&merchantB); err != nil {
		t.Fatalf("seed merchant B: %v", err)
	}

	seedPerson := func(t *testing.T, label string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
			companyID, "Gizi Ctx "+label).Scan(&id); err != nil {
			t.Fatalf("seed person %s: %v", label, err)
		}
		return id
	}

	// execTenant menjalankan fn dalam SATU tx app_runtime dengan GUC tenant,
	// lalu COMMIT — pool test ≤ 4 koneksi, tidak menumpuk tx cleanup.
	execTenant := func(t *testing.T, compID, merchID string, fn func(tx pgx.Tx)) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			t.Fatalf("set local role: %v", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", compID); err != nil {
			t.Fatalf("set company GUC: %v", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchID); err != nil {
			t.Fatalf("set merchant GUC: %v", err)
		}
		fn(tx)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// insertGc menulis konteks bayi kecil (RLS + tenant consistency aktif).
	insertGc := func(t *testing.T, personID string, bbl float64, gest int) {
		t.Helper()
		execTenant(t, companyID, merchantID, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO clinical.person_growth_context
					(company_id, person_id, bbl_kg, usia_gestasi_mgg, recorded_at, recorded_by)
				VALUES ($1, $2, $3, $4, now(), '00000000-0000-0000-0000-000000000000')`,
				companyID, personID, bbl, gest); err != nil {
				t.Fatalf("insert growth context: %v", err)
			}
		})
	}

	// giziSnap holds the snapshot columns written by the trigger for one row.
	type giziSnap struct {
		bbu, tbu, bbtb, imtu, lku, lila *float64
		status                          map[string]any
		curveVersion                    *string
	}

	// insertAnthro inserts one measurement via runtime tx and reads back the
	// trigger snapshot (pola gizi_anthropometry_db_test.go).
	insertAnthro := func(t *testing.T, personID string, ageDays int, bb, tb float64) giziSnap {
		t.Helper()
		var s giziSnap
		execTenant(t, companyID, merchantID, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO clinical.anthropometry
					(company_id, merchant_id, person_id, admission_id, measured_at, gender, age_days, bb_kg, tb_cm, extras)
				VALUES ($1, $2, $3, NULL, now(), 'laki_laki', $4, $5, $6, '{}'::jsonb)`,
				companyID, merchantID, personID, ageDays, bb, tb); err != nil {
				t.Fatalf("insert anthropometry: %v", err)
			}
			var statusRaw []byte
			if err := tx.QueryRow(ctx, `
				SELECT zscore_bbu, zscore_tbu, zscore_bbtb, zscore_imtu, zscore_lku, zscore_lila,
				       status, curve_version
				FROM clinical.anthropometry
				WHERE person_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, personID).
				Scan(&s.bbu, &s.tbu, &s.bbtb, &s.imtu, &s.lku, &s.lila, &statusRaw, &s.curveVersion); err != nil {
				t.Fatalf("read snapshot: %v", err)
			}
			if err := json.Unmarshal(statusRaw, &s.status); err != nil {
				t.Fatalf("unmarshal status: %v", err)
			}
		})
		return s
	}

	// seedRef + cleanupRef: baris ref sintetis via pool owner (app_runtime
	// hanya SELECT), dihapus lagi di akhir subtest.
	seedRef := func(t *testing.T, indicator, sourceVersion string, x int, l, m, s float64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at)
			VALUES ($1, 'laki_laki', 'age_days', $2, $3, $4, $5, 'sintetis', $6, 'http://test.local/sintetis', now())`,
			indicator, x, l, m, s, sourceVersion); err != nil {
			t.Fatalf("seed ref %s x=%d: %v", indicator, x, err)
		}
	}
	cleanupRef := func(t *testing.T, indicator, sourceVersion string, x int) {
		t.Helper()
		t.Cleanup(func() {
			if _, err := pool.Exec(ctx,
				"DELETE FROM ref.growth_curve WHERE indicator=$1 AND gender='laki_laki' AND source_version=$2 AND x=$3",
				indicator, sourceVersion, x); err != nil {
				t.Fatalf("cleanup ref %s x=%d: %v", indicator, x, err)
			}
		})
	}

	expectVal := func(t *testing.T, name string, got *float64, want float64) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: expected %v, got NULL", name, want)
		}
		if *got != want {
			t.Fatalf("%s: expected %v, got %v", name, want, *got)
		}
	}

	// noKoreksiKey: status TIDAK memuat age_eff_days/usia_koreksi_bulan
	// (perilaku v2 tanpa koreksi).
	noKoreksiKey := func(t *testing.T, status map[string]any) {
		t.Helper()
		for _, k := range []string{"age_eff_days", "usia_koreksi_bulan"} {
			if _, ok := status[k]; ok {
				t.Fatalf("status[%s] tidak boleh ada tanpa koreksi: %v", k, status)
			}
		}
	}

	// (a) POST konteks + GET episode memuat growth_context + bblr flag.
	t.Run("a post dan get episode", func(t *testing.T) {
		router := gzNewRouter(t, ctx, pool)
		envCompany, envMerchant, _, token := gzSeedEnv(t, ctx, pool, router, "gcctx")
		personID, adm := cfSeedAdmission(t, ctx, pool, router, token, envCompany, envMerchant, "gcctx")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-growth-context", token, envCompany, envMerchant, map[string]any{
			"person_id": personID, "bbl_kg": 3.2, "usia_gestasi_mgg": 39, "source_ref": "kia",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("POST growth-context = %d: %s", rec.Code, rec.Body.String())
		}
		data := resp["data"].(map[string]any)
		if gzNum(t, data["bbl_kg"]) != 3.2 || data["usia_gestasi_mgg"].(float64) != 39 || data["source_ref"] != "kia" {
			t.Fatalf("POST response salah: %v", data)
		}
		if data["bblr"] != false {
			t.Fatalf("bblr: expected false untuk bbl 3.2, got %v", data["bblr"])
		}
		if data["usia_koreksi_bulan"] != nil {
			t.Fatalf("usia_koreksi_bulan: expected null (gest 39 >= 37), got %v", data["usia_koreksi_bulan"])
		}
		rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+adm, token, envCompany, envMerchant, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET episode = %d: %s", rec.Code, rec.Body.String())
		}
		gc, ok := resp["growth_context"].(map[string]any)
		if !ok {
			t.Fatalf("growth_context hilang di GET episode: %v", resp)
		}
		if gzNum(t, gc["bbl_kg"]) != 3.2 || gc["bblr"] != false || gc["recorded_at"] == nil {
			t.Fatalf("growth_context GET salah: %v", gc)
		}
	})

	// (f) upsert idempoten: 2× POST nilai beda → satu baris nilai terbaru.
	t.Run("f upsert idempoten", func(t *testing.T) {
		router := gzNewRouter(t, ctx, pool)
		envCompany, envMerchant, _, token := gzSeedEnv(t, ctx, pool, router, "gcidem")
		personID, adm := cfSeedAdmission(t, ctx, pool, router, token, envCompany, envMerchant, "gcidem")
		for _, body := range []map[string]any{
			{"person_id": personID, "bbl_kg": 2.4, "usia_gestasi_mgg": 41},
			{"person_id": personID, "bbl_kg": 4.1, "usia_gestasi_mgg": 38, "source_ref": "rekam"},
		} {
			rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-growth-context", token, envCompany, envMerchant, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST growth-context = %d: %s", rec.Code, rec.Body.String())
			}
		}
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM clinical.person_growth_context WHERE company_id=$1::uuid AND person_id=$2::uuid",
			envCompany, personID).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if n != 1 {
			t.Fatalf("upsert idempoten: expected 1 baris, got %d", n)
		}
		rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+adm, token, envCompany, envMerchant, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET episode = %d: %s", rec.Code, rec.Body.String())
		}
		gc := resp["growth_context"].(map[string]any)
		if gzNum(t, gc["bbl_kg"]) != 4.1 || gc["usia_gestasi_mgg"].(float64) != 38 || gc["source_ref"] != "rekam" {
			t.Fatalf("upsert tidak memakai nilai terbaru: %v", gc)
		}
	})

	// (g) RLS: konteks company A tidak terbaca di bawah GUC company B.
	t.Run("g rls isolasi company", func(t *testing.T) {
		personID := seedPerson(t, "rls")
		insertGc(t, personID, 2.9, 33)
		var n int
		execTenant(t, companyB, merchantB, func(tx pgx.Tx) {
			if err := tx.QueryRow(ctx,
				"SELECT count(*) FROM clinical.person_growth_context WHERE company_id=$1::uuid AND person_id=$2::uuid",
				companyID, personID).Scan(&n); err != nil {
				t.Fatalf("select rls: %v", err)
			}
		})
		if n != 0 {
			t.Fatalf("RLS bocor: konteks company A terlihat dari company B (%d baris)", n)
		}
		// Kontrol: GUC company A tetap melihat barisnya.
		execTenant(t, companyID, merchantID, func(tx pgx.Tx) {
			if err := tx.QueryRow(ctx,
				"SELECT count(*) FROM clinical.person_growth_context WHERE person_id=$1::uuid",
				personID).Scan(&n); err != nil {
				t.Fatalf("select rls kontrol: %v", err)
			}
		})
		if n != 1 {
			t.Fatalf("kontrol RLS: expected 1 baris di company A, got %d", n)
		}
	})

	// (b) prematur gestasi 32, kronologis 545 hari → z di age_eff 489
	// (545 − (40−32)×7), usia_koreksi_bulan 16 (floor(489/30.4375)); z beda
	// dari tanpa konteks — keduanya di-assert.
	t.Run("b prematur koreksi 489 hari", func(t *testing.T) {
		seedRef(t, "bbu", "gc-premat", 489, 1, 10, 0.1)
		cleanupRef(t, "bbu", "gc-premat", 489)
		seedRef(t, "bbu", "gc-premat", 545, 1, 16, 0.1)
		cleanupRef(t, "bbu", "gc-premat", 545)
		personKron := seedPerson(t, "kron545")
		personPre := seedPerson(t, "prem545")
		// Tanpa konteks: nearest x = 545, z = (8/16−1)/0.1 = −5.00.
		sKron := insertAnthro(t, personKron, 545, 8, 75)
		expectVal(t, "zscore_bbu tanpa konteks", sKron.bbu, -5.00)
		noKoreksiKey(t, sKron.status)
		// Dengan konteks prematur 32: age_eff 489 → nearest x = 489,
		// z = (8/10−1)/0.1 = −2.00.
		insertGc(t, personPre, 2.9, 32)
		sPre := insertAnthro(t, personPre, 545, 8, 75)
		expectVal(t, "zscore_bbu prematur", sPre.bbu, -2.00)
		if v, ok := sPre.status["age_eff_days"].(float64); !ok || v != 489 {
			t.Fatalf("status age_eff_days: expected 489, got %v", sPre.status["age_eff_days"])
		}
		if v, ok := sPre.status["usia_koreksi_bulan"].(float64); !ok || v != 16 {
			t.Fatalf("status usia_koreksi_bulan: expected 16, got %v", sPre.status["usia_koreksi_bulan"])
		}
	})

	// (c) aterm gestasi 38 (≥ 37) usia sama → tanpa koreksi, z identik v2.
	t.Run("c aterm gest 38 tanpa koreksi", func(t *testing.T) {
		seedRef(t, "bbu", "gc-aterm", 545, 1, 16, 0.1)
		cleanupRef(t, "bbu", "gc-aterm", 545)
		personNo := seedPerson(t, "aterm-no")
		personAt := seedPerson(t, "aterm38")
		sNo := insertAnthro(t, personNo, 545, 8, 75)
		insertGc(t, personAt, 3.1, 38)
		sAt := insertAnthro(t, personAt, 545, 8, 75)
		// z = (8/16−1)/0.1 = −5.00 — identik tanpa konteks.
		expectVal(t, "zscore_bbu aterm", sAt.bbu, -5.00)
		if sNo.bbu == nil || *sAt.bbu != *sNo.bbu {
			t.Fatalf("z aterm harus identik dengan tanpa konteks: %v vs %v", sAt.bbu, sNo.bbu)
		}
		noKoreksiKey(t, sAt.status)
	})

	// (d) prematur gestasi 32 usia 800 hari (≥ 731) → tanpa koreksi.
	t.Run("d prematur 800 hari tanpa koreksi", func(t *testing.T) {
		seedRef(t, "bbu", "gc-800", 800, 1, 12, 0.1)
		cleanupRef(t, "bbu", "gc-800", 800)
		person := seedPerson(t, "prem800")
		insertGc(t, person, 2.7, 32)
		// z kronologis di 800: (8/12−1)/0.1 = −3.33.
		s := insertAnthro(t, person, 800, 8, 75)
		expectVal(t, "zscore_bbu 800", s.bbu, -3.33)
		noKoreksiKey(t, s.status)
	})

	// (e) BBLR aterm (bbl 2,3, gestasi 40) → bblr true, z tanpa koreksi.
	t.Run("e bblr aterm", func(t *testing.T) {
		router := gzNewRouter(t, ctx, pool)
		envCompany, envMerchant, _, token := gzSeedEnv(t, ctx, pool, router, "gcbblr")
		personID, adm := cfSeedAdmission(t, ctx, pool, router, token, envCompany, envMerchant, "gcbblr")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-growth-context", token, envCompany, envMerchant, map[string]any{
			"person_id": personID, "bbl_kg": 2.3, "usia_gestasi_mgg": 40,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("POST growth-context = %d: %s", rec.Code, rec.Body.String())
		}
		if resp["data"].(map[string]any)["bblr"] != true {
			t.Fatalf("bblr: expected true untuk bbl 2.3, got %v", resp["data"])
		}
		rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+adm, token, envCompany, envMerchant, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET episode = %d: %s", rec.Code, rec.Body.String())
		}
		if gc := resp["growth_context"].(map[string]any); gc["bblr"] != true {
			t.Fatalf("growth_context.bblr GET: expected true, got %v", gc)
		}
		// Trigger: gestasi 40 → tanpa koreksi; z = (7.9/10−1)/0.1 = −2.10
		// (sama seperti tanpa konteks).
		seedRef(t, "bbu", "gc-bblr", 548, 1, 10, 0.1)
		cleanupRef(t, "bbu", "gc-bblr", 548)
		person := seedPerson(t, "bblr548")
		insertGc(t, person, 2.3, 40)
		s := insertAnthro(t, person, 548, 7.9, 75)
		expectVal(t, "zscore_bbu bblr aterm", s.bbu, -2.10)
		noKoreksiKey(t, s.status)
	})

	// (h) regresi: balita 30 bln (913 hari) TANPA konteks → z persis v2
	// (band + curve_version who-cgs-2006).
	t.Run("h regresi balita 30 bln v2", func(t *testing.T) {
		seedRef(t, "bbu", "who-cgs-2006", 913, 1, 10, 0.1)
		cleanupRef(t, "bbu", "who-cgs-2006", 913)
		seedRef(t, "imtu", "who-cgs-2006", 913, 1, 15, 0.05)
		cleanupRef(t, "imtu", "who-cgs-2006", 913)
		person := seedPerson(t, "reg30bln")
		// bbu: (10/10−1)/0.1 = 0; imt = round(10/0.75²,1) = 17.8;
		// imtu: (17.8/15−1)/0.05 = 3.73.
		s := insertAnthro(t, person, 913, 10, 75)
		expectVal(t, "zscore_bbu", s.bbu, 0)
		expectVal(t, "zscore_imtu", s.imtu, 3.73)
		if s.curveVersion == nil || *s.curveVersion != "who-cgs-2006" {
			t.Fatalf("curve_version: expected who-cgs-2006, got %v", s.curveVersion)
		}
		band, ok := s.status["bbu"].(map[string]any)
		if !ok || band["code"] != "gizi_baik" {
			t.Fatalf("status[bbu]: expected band gizi_baik (z=0), got %v", s.status["bbu"])
		}
		noKoreksiKey(t, s.status)
	})
}
