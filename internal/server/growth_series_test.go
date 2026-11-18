//go:build integration

package server_test

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"
)

// TestGiziGrowthSeries — endpoint GET /clinical/growth-series (plan
// 2026-11-13-gizi chunk 3 Task 1, spec 2026-11-13-gizi-nutrition-care §5):
// kurva LMS dari ref sintetis (BUKAN CSV WHO nyata) + titik pasien dari
// snapshot + flag_t + ref kosong + band bumil ICM + izin/403. Nilai ekspektasi
// dihitung dari DEFINISI kontrak (rumus LMS + band ICM literal), bukan dari
// output handler.
func TestGiziGrowthSeries(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "gs")

	// Ref sintetis laki_laki: bbu L=1 M=10 S=0.1 di x=548; lila L=0 M=13
	// S=0.1 di x=548 (BUKAN WHO). Sumber harga kurva: rumus LMS kontrak.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at) VALUES
			('bbu',  'laki_laki', 'age_days', 548, 1, 10, 0.1, 'sintetis', 'gs-test', 'http://test.local/sintetis', now()),
			('lila', 'laki_laki', 'age_days', 548, 0, 13, 0.1, 'sintetis', 'gs-test', 'http://test.local/sintetis', now())`); err != nil {
		t.Fatalf("seed ref sintetis: %v", err)
	}

	seedPerson := func(t *testing.T, label, gender string, birthDate any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, $3, $4) RETURNING id::text",
			companyID, "Gizi GS "+label, gender, birthDate).Scan(&id); err != nil {
			t.Fatalf("seed person %s: %v", label, err)
		}
		return id
	}

	// insertAnthro menulis baris antropometri langsung SQL (snapshot trigger
	// mengisi z dari ref yang sama). Baris dibiarkan milik merchant tenant.
	insertAnthro := func(t *testing.T, personID, gender string, ageDays int, measured time.Time, bb, tb float64, extras string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO clinical.anthropometry
				(company_id, merchant_id, person_id, admission_id, measured_at, gender, age_days, bb_kg, tb_cm, extras)
			VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, $8, $9::jsonb)`,
			companyID, merchantID, personID, measured, gender, ageDays, bb, tb, extras); err != nil {
			t.Fatalf("insert anthropometry: %v", err)
		}
	}

	growthSeries := func(t *testing.T, tok, personID, indicator string) (int, map[string]any) {
		t.Helper()
		rec, resp := cfDo(t, router, http.MethodGet,
			"/api/v1/clinical/growth-series?person_id="+personID+"&indicator="+indicator,
			tok, companyID, merchantID, nil)
		return rec.Code, resp
	}

	t.Run("nilai kurva LMS", func(t *testing.T) {
		person := seedPerson(t, "kurva", "male", time.Now().AddDate(0, 0, -548).Format("2006-01-02"))
		code, resp := growthSeries(t, token, person, "bbu")
		if code != http.StatusOK {
			t.Fatalf("bbu = %d: %v", code, resp)
		}
		data := resp["data"].(map[string]any)
		if data["gender"] != "laki_laki" || data["unit"] != "kg" || data["available"] != true {
			t.Fatalf("header = %v", data)
		}
		if src, ok := data["source"].(map[string]any); !ok || src["version"] != "gs-test" {
			t.Fatalf("source = %v", data["source"])
		}
		// L=1, M=10, S=0.1, z=+2 → v = round(10·(1+1·0.1·2)^1, 3) = 12.
		curve := data["curve"].([]any)
		if len(curve) != 1 {
			t.Fatalf("curve len = %d", len(curve))
		}
		row := curve[0].(map[string]any)
		if gzNum(t, row["x"]) != 548 {
			t.Fatalf("curve x = %v, want 548", row["x"])
		}
		lines := row["lines"].(map[string]any)
		if gzNum(t, lines["+2"]) != 12 {
			t.Fatalf("lines[+2] = %v, want 12", lines["+2"])
		}
		// L=0, M=13, S=0.1, z=+1 → v = round(13·e^0.1, 3) ≈ 14.366 (13·e^0.1
		// = 14.36722…; kontrak menyebut ≈14.366 → toleransi 0.01).
		code, resp = growthSeries(t, token, person, "lila")
		if code != http.StatusOK {
			t.Fatalf("lila = %d: %v", code, resp)
		}
		data = resp["data"].(map[string]any)
		if data["unit"] != "cm" {
			t.Fatalf("lila unit = %v", data["unit"])
		}
		lrow := data["curve"].([]any)[0].(map[string]any)
		v := gzNum(t, lrow["lines"].(map[string]any)["+1"])
		if math.Abs(v-14.366) > 0.01 {
			t.Fatalf("lila lines[+1] = %v, want ~14.366", v)
		}
	})

	t.Run("titik pasien + flag_t", func(t *testing.T) {
		person := seedPerson(t, "titik", "male", time.Now().AddDate(0, 0, -548).Format("2006-01-02"))
		now := time.Now()
		// bb 7.9 → z = (7.9/10-1)/0.1 = -2.10; bb 7.8 → z = -2.20 (snapshot
		// round 2). bb[i]-bb[i-1] = 7.8-7.9 = -0.1 ≤ 0 → flag_t titik akhir.
		insertAnthro(t, person, "laki_laki", 548, now.Add(-2*time.Hour), 7.9, 75, "{}")
		insertAnthro(t, person, "laki_laki", 548, now.Add(-1*time.Hour), 7.8, 75, "{}")
		code, resp := growthSeries(t, token, person, "bbu")
		if code != http.StatusOK {
			t.Fatalf("bbu = %d: %v", code, resp)
		}
		pts := resp["data"].(map[string]any)["points"].([]any)
		if len(pts) != 2 {
			t.Fatalf("points len = %d", len(pts))
		}
		p0 := pts[0].(map[string]any)
		if gzNum(t, p0["value"]) != 7.9 || gzNum(t, p0["x"]) != 548 {
			t.Fatalf("point0 = %v", p0)
		}
		if gzNum(t, p0["zscore"]) != -2.10 {
			t.Fatalf("point0 zscore = %v, want -2.10", p0["zscore"])
		}
		p1 := pts[1].(map[string]any)
		if gzNum(t, p1["value"]) != 7.8 || gzNum(t, p1["zscore"]) != -2.20 {
			t.Fatalf("point1 = %v", p1)
		}
		if p1["flag_t"] != true || p0["flag_t"] != false {
			t.Fatalf("flag_t = %v / %v, want true / false", p1["flag_t"], p0["flag_t"])
		}
	})

	t.Run("ref kosong", func(t *testing.T) {
		person := seedPerson(t, "kosong", "female", time.Now().AddDate(0, 0, -400).Format("2006-01-02"))
		insertAnthro(t, person, "perempuan", 400, time.Now().Add(-time.Hour), 8, 70, "{}")
		code, resp := growthSeries(t, token, person, "tbu")
		if code != http.StatusOK {
			t.Fatalf("tbu = %d: %v", code, resp)
		}
		data := resp["data"].(map[string]any)
		if data["available"] != false {
			t.Fatalf("available = %v, want false", data["available"])
		}
		if len(data["curve"].([]any)) != 0 {
			t.Fatalf("curve = %v, want kosong", data["curve"])
		}
		pts := data["points"].([]any)
		if len(pts) != 1 {
			t.Fatalf("points len = %d, want 1 (zscore null)", len(pts))
		}
		if pts[0].(map[string]any)["zscore"] != nil {
			t.Fatalf("zscore = %v, want null (ref kosong)", pts[0].(map[string]any)["zscore"])
		}
	})

	t.Run("bumil band", func(t *testing.T) {
		person := seedPerson(t, "bumil", "female", nil)
		now := time.Now()
		// anchor = bb pengukuran mgg terkecil = 60 (mgg 20). delta mgg 22 =
		// 65-60 = 6.5; band normal {11.5,16} → [11.5·22/40, 16·22/40] =
		// [6.325, 8.8] → 6.5 in_band TRUE. delta mgg 24 = 72-60 = 12;
		// [6.9, 9.6] → FALSE. Baris tanpa mgg tidak ikut.
		insertAnthro(t, person, "perempuan", 0, now.Add(-3*time.Hour), 60, 160, `{"mgg":20}`)
		insertAnthro(t, person, "perempuan", 0, now.Add(-2*time.Hour), 66.5, 160, `{"mgg":22}`)
		insertAnthro(t, person, "perempuan", 0, now.Add(-1*time.Hour), 72, 160, `{"mgg":24}`)
		insertAnthro(t, person, "perempuan", 0, now, 70, 160, `{}`)
		code, resp := growthSeries(t, token, person, "bumil_bb")
		if code != http.StatusOK {
			t.Fatalf("bumil = %d: %v", code, resp)
		}
		data := resp["data"].(map[string]any)
		if data["indicator"] != "bumil_bb" || data["band_selected"] != "normal" {
			t.Fatalf("header = %v", data)
		}
		if gzNum(t, data["anchor_bb_kg"]) != 60 {
			t.Fatalf("anchor = %v, want 60", data["anchor_bb_kg"])
		}
		bands := data["bands"].(map[string]any)
		nb := bands["normal"].(map[string]any)
		if gzNum(t, nb["min"]) != 11.5 || gzNum(t, nb["max"]) != 16 {
			t.Fatalf("band normal = %v", nb)
		}
		pts := data["points"].([]any)
		if len(pts) != 3 {
			t.Fatalf("points len = %d, want 3 (tanpa mgg tidak ikut)", len(pts))
		}
		p1 := pts[1].(map[string]any)
		if gzNum(t, p1["delta_kg"]) != 6.5 || p1["in_band"] != true {
			t.Fatalf("point mgg22 = %v, want delta 6.5 in_band true", p1)
		}
		p2 := pts[2].(map[string]any)
		if gzNum(t, p2["delta_kg"]) != 12 || p2["in_band"] != false {
			t.Fatalf("point mgg24 = %v, want delta 12 in_band false", p2)
		}
		src := data["source"].(map[string]any)
		if src["version"] != "icm-2009" {
			t.Fatalf("source = %v", src)
		}
	})

	t.Run("izin/403", func(t *testing.T) {
		person := seedPerson(t, "perm", "male", time.Now().AddDate(0, 0, -548).Format("2006-01-02"))
		noPerm := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "gz-gs-noperm")
		code, _ := growthSeries(t, noPerm, person, "bbu")
		if code != http.StatusForbidden {
			t.Fatalf("no perm = %d", code)
		}
		// person_id bukan UUID → 400.
		rec, _ := cfDo(t, router, http.MethodGet,
			"/api/v1/clinical/growth-series?person_id=abc&indicator=bbu", token, companyID, merchantID, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad person_id = %d", rec.Code)
		}
		// indicator tidak dikenal → 400.
		rec, _ = cfDo(t, router, http.MethodGet,
			"/api/v1/clinical/growth-series?person_id="+person+"&indicator=zzz", token, companyID, merchantID, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad indicator = %d", rec.Code)
		}
	})
}

// TestGrowthSeriesMergedSources — kurva referensi gabungan dua sumber (plan
// 2026-11-18-anak-a1-1-be Task 4, spec §5): who-cgs-2006 (balita) + who-gr-
// 2007 (61 bln s.d. cap per indikator: tbu/imtu 228, bbu 120) sesuai usia
// person dari DOB; z titik pasien tetap salin snapshot. Nilai ekspektasi
// dihitung dari DEFINISI kontrak (rumus LMS), bukan dari output handler.
func TestGrowthSeriesMergedSources(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "gsm")

	// Ref sintetis dua sumber laki_laki (BUKAN WHO). am = floor(x/30.4375):
	// 913→29; 2557→84; 3653→120; 3683→121; 6940→228.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at) VALUES
			('tbu', 'laki_laki', 'age_days',  913, 1,  80, 0.1, 'sintetis', 'who-cgs-2006', 'http://test.local/cgs', now()),
			('bbu', 'laki_laki', 'age_days',  913, 1,   9, 0.1, 'sintetis', 'who-cgs-2006', 'http://test.local/cgs', now()),
			('tbu', 'laki_laki', 'age_days', 2557, 1, 120, 0.1, 'sintetis', 'who-gr-2007',  'http://test.local/gr',  now()),
			('tbu', 'laki_laki', 'age_days', 6940, 1, 160, 0.1, 'sintetis', 'who-gr-2007',  'http://test.local/gr',  now()),
			('bbu', 'laki_laki', 'age_days', 2557, 1,  25, 0.1, 'sintetis', 'who-gr-2007',  'http://test.local/gr',  now()),
			('bbu', 'laki_laki', 'age_days', 3653, 1,  35, 0.1, 'sintetis', 'who-gr-2007',  'http://test.local/gr',  now()),
			('bbu', 'laki_laki', 'age_days', 3683, 1,  36, 0.1, 'sintetis', 'who-gr-2007',  'http://test.local/gr',  now())`); err != nil {
		t.Fatalf("seed ref dua sumber: %v", err)
	}

	seedPerson := func(t *testing.T, label, gender string, birthDate any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, $3, $4) RETURNING id::text",
			companyID, "Gizi GSM "+label, gender, birthDate).Scan(&id); err != nil {
			t.Fatalf("seed person %s: %v", label, err)
		}
		return id
	}

	growthSeries := func(t *testing.T, personID, indicator string) map[string]any {
		t.Helper()
		rec, resp := cfDo(t, router, http.MethodGet,
			"/api/v1/clinical/growth-series?person_id="+personID+"&indicator="+indicator,
			token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %v", indicator, rec.Code, resp)
		}
		return resp["data"].(map[string]any)
	}

	curveXs := func(t *testing.T, data map[string]any) []float64 {
		t.Helper()
		rows := data["curve"].([]any)
		xs := make([]float64, 0, len(rows))
		for _, r := range rows {
			xs = append(xs, gzNum(t, r.(map[string]any)["x"]))
		}
		return xs
	}

	t.Run("anak: gabungan tbu s.d. 228 bln", func(t *testing.T) {
		// DOB 20 th → am ±240 ≥ 228 → gabungan penuh: cgs 913 + gr 2557,6940.
		person := seedPerson(t, "anak-tbu", "male", time.Now().AddDate(-20, 0, 0).Format("2006-01-02"))
		data := growthSeries(t, person, "tbu")
		xs := curveXs(t, data)
		if len(xs) != 3 || xs[0] != 913 || xs[1] != 2557 || xs[2] != 6940 {
			t.Fatalf("tbu curve x = %v, want [913 2557 6940]", xs)
		}
		src := data["source"].(map[string]any)
		if src["version"] != "who-gr-2007" {
			t.Fatalf("source = %v, want who-gr-2007", src)
		}
		// L=1, M=120, S=0.1, z=+2 → v = round(120·(1+0.1·2)^1, 3) = 144.
		row := data["curve"].([]any)[1].(map[string]any)
		if v := gzNum(t, row["lines"].(map[string]any)["+2"]); v != 144 {
			t.Fatalf("gr lines[+2] = %v, want 144", v)
		}
		// z titik pasien salin snapshot: tb 144 di age_days 2557 →
		// z = ((144/120)^1 − 1)/(1·0.1) = 2.00 (trigger 000105, ref yang sama).
		now := time.Now()
		if _, err := pool.Exec(ctx, `
			INSERT INTO clinical.anthropometry
				(company_id, merchant_id, person_id, admission_id, measured_at, gender, age_days, bb_kg, tb_cm, extras)
			VALUES ($1, $2, $3, NULL, $4, 'laki_laki', 2557, 30, 144, '{}'::jsonb)`,
			companyID, merchantID, person, now); err != nil {
			t.Fatalf("insert anthropometry: %v", err)
		}
		data = growthSeries(t, person, "tbu")
		pts := data["points"].([]any)
		if len(pts) != 1 {
			t.Fatalf("points len = %d, want 1", len(pts))
		}
		p := pts[0].(map[string]any)
		if gzNum(t, p["zscore"]) != 2.0 {
			t.Fatalf("zscore = %v, want 2.0 (salin snapshot)", p["zscore"])
		}
	})

	t.Run("anak: bbu ter-cutoff 120 bln", func(t *testing.T) {
		// Baris gr am 121 (x 3683) DI LUAR domain bbu (cap 120 bln) → tidak
		// pernah tampil walau usia person ≥ 228.
		person := seedPerson(t, "anak-bbu", "male", time.Now().AddDate(-20, 0, 0).Format("2006-01-02"))
		data := growthSeries(t, person, "bbu")
		xs := curveXs(t, data)
		if len(xs) != 3 || xs[0] != 913 || xs[1] != 2557 || xs[2] != 3653 {
			t.Fatalf("bbu curve x = %v, want [913 2557 3653]", xs)
		}
		if data["source"].(map[string]any)["version"] != "who-gr-2007" {
			t.Fatalf("source = %v, want who-gr-2007", data["source"])
		}
	})

	t.Run("balita: domain lama tetap hijau", func(t *testing.T) {
		// DOB 548 hari (am 18) → tanpa baris who-gr-2007; hanya cgs.
		person := seedPerson(t, "balita", "male", time.Now().AddDate(0, 0, -548).Format("2006-01-02"))
		for _, ind := range []string{"tbu", "bbu"} {
			data := growthSeries(t, person, ind)
			xs := curveXs(t, data)
			if len(xs) != 1 || xs[0] != 913 {
				t.Fatalf("%s curve x = %v, want [913]", ind, xs)
			}
			if data["source"].(map[string]any)["version"] != "who-cgs-2006" {
				t.Fatalf("%s source = %v, want who-cgs-2006", ind, data["source"])
			}
		}
	})
}
