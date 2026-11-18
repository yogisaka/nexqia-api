//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestGiziAnthropometrySnapshot — migration 000097_gizi_fondasi: trigger
// clinical.fn_anthropometry_snapshot mengisi imt/imt_kategori, zscore_*, status,
// dan curve_version saat INSERT. Referensi kurva dipakai SINTETIS (bukan WHO)
// supaya nilai z bisa dihitung tangan dari definisi LMS.
//
// Fixture tenant disemai langsung dengan pool owner (pola seedChildRLSTenant):
// ref.growth_curve tidak ber-RLS, sedangkan INSERT antropometri dilakukan
// sebagai app_runtime dengan kedua GUC tenant (pola runtimeTenantTx) supaya
// RLS + trg_check_tenant_consistency aktif seperti produksi.
func TestGiziAnthropometrySnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	// --- Tenant minimal: company -> merchant -> department -> person -> admission.
	var companyID, merchantID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ('GZI', 'Gizi Co', 10) RETURNING id::text",
	).Scan(&companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.merchant (company_id, code, name) VALUES ($1, 'gizi', 'Gizi Merchant') RETURNING id::text",
		companyID).Scan(&merchantID); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	var deptID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, 'GZI-DEP', 'Poli Gizi') RETURNING id::text",
		companyID, merchantID).Scan(&deptID); err != nil {
		t.Fatalf("seed department: %v", err)
	}

	seedPerson := func(t *testing.T, label, gender string, birthDate any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, $3, $4) RETURNING id::text",
			companyID, "Gizi Person "+label, gender, birthDate).Scan(&id); err != nil {
			t.Fatalf("seed person %s: %v", label, err)
		}
		return id
	}

	// --- Fixture ref sintetis (bukan WHO): (indicator, gender, x_kind, x, l, m, s).
	// source_version='test-sintetis' — nilai z dihitung tangan dari rumus LMS.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at) VALUES
			-- sintetis (bukan WHO)
			('bbu',  'laki_laki', 'age_days',  548, 1, 10, 0.1,  'sintetis', 'test-sintetis', 'http://test.local/sintetis', now()),
			-- sintetis (bukan WHO)
			('bbtb', 'laki_laki', 'height_cm', 75,  1, 10, 0.1,  'sintetis', 'test-sintetis', 'http://test.local/sintetis', now()),
			-- sintetis (bukan WHO)
			('imtu', 'laki_laki', 'age_days',  548, 1, 15, 0.05, 'sintetis', 'test-sintetis', 'http://test.local/sintetis', now()),
			-- sintetis (bukan WHO)
			('lila', 'laki_laki', 'age_days',  548, 0, 13, 0.1,  'sintetis', 'test-sintetis', 'http://test.local/sintetis', now())`); err != nil {
		t.Fatalf("seed ref growth_curve: %v", err)
	}

	// giziSnap holds the snapshot columns written by the trigger for one row.
	type giziSnap struct {
		bbu, tbu, bbtb, imtu, lku, lila *float64
		imt                             *float64
		imtKategori                     *string
		status                          map[string]any
		curveVersion                    *string
	}

	insertAnthro := func(t *testing.T, tx pgx.Tx, personID, gender string, ageDays int, bb, tb float64, extras string) giziSnap {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO clinical.anthropometry
				(company_id, merchant_id, person_id, admission_id, measured_at, gender, age_days, bb_kg, tb_cm, extras)
			VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, $8, $9::jsonb)
			RETURNING id::text`,
			companyID, merchantID, personID, time.Now(), gender, ageDays, bb, tb, extras).Scan(&id); err != nil {
			t.Fatalf("insert anthropometry: %v", err)
		}
		var s giziSnap
		var statusRaw []byte
		if err := tx.QueryRow(ctx, `
			SELECT zscore_bbu, zscore_tbu, zscore_bbtb, zscore_imtu, zscore_lku, zscore_lila,
			       imt, imt_kategori, status, curve_version
			FROM clinical.anthropometry WHERE id = $1::uuid`, id).
			Scan(&s.bbu, &s.tbu, &s.bbtb, &s.imtu, &s.lku, &s.lila, &s.imt, &s.imtKategori, &statusRaw, &s.curveVersion); err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		if err := json.Unmarshal(statusRaw, &s.status); err != nil {
			t.Fatalf("unmarshal status: %v", err)
		}
		return s
	}

	// expectNil asserts a z-score column is NULL.
	expectNil := func(t *testing.T, name string, v *float64) {
		t.Helper()
		if v != nil {
			t.Fatalf("%s: expected NULL, got %v", name, *v)
		}
	}

	// expectVal asserts a z-score column equals want (round 2 by the trigger).
	expectVal := func(t *testing.T, name string, got *float64, want float64) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: expected %v, got NULL", name, want)
		}
		if *got != want {
			t.Fatalf("%s: expected %v, got %v", name, want, *got)
		}
	}

	t.Run("dewasa imt", func(t *testing.T) {
		person := seedPerson(t, "dewasa", "male", "1974-06-15")
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// imt = round(62/1.58^2, 1) = 24.8 (kategori bb_lebih: 23 <= imt < 25).
		// age_days 19000 > 2192 -> semua zscore_* NULL, status {}, curve_version NULL.
		s := insertAnthro(t, tx, person, "laki_laki", 19000, 62, 158, "{}")
		if s.imt == nil || *s.imt != 24.8 {
			t.Fatalf("imt: expected 24.8, got %v", s.imt)
		}
		if s.imtKategori == nil || *s.imtKategori != "bb_lebih" {
			t.Fatalf("imt_kategori: expected bb_lebih, got %v", s.imtKategori)
		}
		expectNil(t, "zscore_bbu", s.bbu)
		expectNil(t, "zscore_tbu", s.tbu)
		expectNil(t, "zscore_bbtb", s.bbtb)
		expectNil(t, "zscore_imtu", s.imtu)
		expectNil(t, "zscore_lku", s.lku)
		expectNil(t, "zscore_lila", s.lila)
		if len(s.status) != 0 {
			t.Fatalf("status: expected {}, got %v", s.status)
		}
		if s.curveVersion != nil {
			t.Fatalf("curve_version: expected NULL, got %v", *s.curveVersion)
		}
	})

	t.Run("balita z + status", func(t *testing.T) {
		person := seedPerson(t, "balita", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// bbu: l=1 -> (7.9/10-1)/0.1 = -2.10 (band gizi_kurang/sedang).
		// tbu: tidak ada baris tbu -> NULL.
		// bbtb: (7.9/10-1)/0.1 = -2.10 (band wasting/sedang).
		// imt = round(7.9/0.75^2, 1) = 14.0; imtu: (14.0/15-1)/0.05 = -1.33 (gizi_baik/rendah).
		// lila: l=0 -> ln(12/13)/0.1 = round(-0.80,2) = -0.80; band cm [11.5,12.5) -> gizi_kurang/sedang.
		s := insertAnthro(t, tx, person, "laki_laki", 548, 7.9, 75, `{"lila_cm":12.0}`)
		expectVal(t, "zscore_bbu", s.bbu, -2.10)
		expectNil(t, "zscore_tbu", s.tbu)
		expectVal(t, "zscore_bbtb", s.bbtb, -2.10)
		if s.imt == nil || *s.imt != 14.0 {
			t.Fatalf("imt: expected 14.0, got %v", s.imt)
		}
		expectVal(t, "zscore_imtu", s.imtu, -1.33)
		expectVal(t, "zscore_lila", s.lila, -0.80)
		if s.curveVersion == nil || *s.curveVersion != "test-sintetis" {
			t.Fatalf("curve_version: expected test-sintetis, got %v", s.curveVersion)
		}
		for _, key := range []string{"bbu", "bbtb", "imtu", "lila_cm"} {
			band, ok := s.status[key].(map[string]any)
			if !ok {
				t.Fatalf("status[%s] hilang: %v", key, s.status)
			}
			code, _ := band["code"].(string)
			want := map[string]string{"bbu": "gizi_kurang", "bbtb": "wasting", "imtu": "gizi_baik", "lila_cm": "gizi_kurang"}[key]
			if code != want {
				t.Fatalf("status[%s].code: expected %s, got %s", key, want, code)
			}
		}
		if _, ok := s.status["tbu"]; ok {
			t.Fatalf("status[tbu] tidak boleh ada tanpa baris ref tbu: %v", s.status)
		}
	})

	t.Run("nearest row tie-break", func(t *testing.T) {
		person := seedPerson(t, "tie", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// Ref kedua 550 hari: jarak |549-548| = |549-550| = 1 -> tie, ORDER BY x
		// memilih baris 548 (x lebih kecil): z = (10/10-1)/0.1 = 0, BUKAN -5.
		// age 550 tepat kena baris 550: z = (10/20-1)/0.1 = -5.
		// Baris ref disisipkan via pool owner (app_runtime hanya SELECT) dan
		// dihapus lagi setelah sub selesai supaya tidak mengganggu sub lain.
		if _, err := pool.Exec(ctx, `
			-- sintetis (bukan WHO)
			INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at)
			VALUES ('bbu', 'laki_laki', 'age_days', 550, 1, 20, 0.1, 'sintetis', 'test-sintetis', 'http://test.local/sintetis', now())`); err != nil {
			t.Fatalf("seed ref kedua: %v", err)
		}
		defer func() {
			if _, err := pool.Exec(ctx,
				"DELETE FROM ref.growth_curve WHERE indicator='bbu' AND gender='laki_laki' AND x=550 AND source_version='test-sintetis'"); err != nil {
				t.Fatalf("cleanup ref kedua: %v", err)
			}
		}()
		s := insertAnthro(t, tx, person, "laki_laki", 549, 10, 75, "{}")
		expectVal(t, "zscore_bbu@549", s.bbu, 0)
		s = insertAnthro(t, tx, person, "laki_laki", 550, 10, 75, "{}")
		expectVal(t, "zscore_bbu@550", s.bbu, -5.00)
	})

	t.Run("jendela usia 2192", func(t *testing.T) {
		// (a) Balita 2000 hari (masih < 2192): z anak tetap dihitung.
		person := seedPerson(t, "balita2000", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		s := insertAnthro(t, tx, person, "laki_laki", 2000, 10, 80, `{"lk_cm":45.0,"lila_cm":12.0}`)
		expectVal(t, "zscore_bbu", s.bbu, 0) // (10/10-1)/0.1
		expectVal(t, "zscore_bbtb", s.bbtb, 0)
		if s.imtu == nil {
			t.Fatalf("zscore_imtu: expected non-NULL at 2000 days")
		}
		if s.lila == nil {
			t.Fatalf("zscore_lila: expected non-NULL at 2000 days")
		}
		expectNil(t, "zscore_tbu", s.tbu) // tidak ada ref tbu
		expectNil(t, "zscore_lku", s.lku) // tidak ada ref lku
		if s.curveVersion == nil {
			t.Fatalf("curve_version: expected non-NULL")
		}

		// (b) Dewasa age_days besar: imt tetap, SEMUA z NULL, status {}, curve_version NULL.
		person2 := seedPerson(t, "dewasa19000", "male", "1974-06-15")
		s2 := insertAnthro(t, tx, person2, "laki_laki", 19000, 62, 158, "{}")
		if s2.imt == nil || *s2.imt != 24.8 || s2.imtKategori == nil || *s2.imtKategori != "bb_lebih" {
			t.Fatalf("imt dewasa: expected 24.8/bb_lebih, got %v/%v", s2.imt, s2.imtKategori)
		}
		expectNil(t, "zscore_bbu", s2.bbu)
		expectNil(t, "zscore_tbu", s2.tbu)
		expectNil(t, "zscore_bbtb", s2.bbtb) // bbtb/imtu termasuk yang NULL
		expectNil(t, "zscore_imtu", s2.imtu)
		expectNil(t, "zscore_lku", s2.lku)
		expectNil(t, "zscore_lila", s2.lila)
		if len(s2.status) != 0 {
			t.Fatalf("status dewasa: expected {}, got %v", s2.status)
		}
		if s2.curveVersion != nil {
			t.Fatalf("curve_version dewasa: expected NULL, got %v", *s2.curveVersion)
		}
	})

	t.Run("ref kosong tidak error", func(t *testing.T) {
		person := seedPerson(t, "perempuan", "female", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// Tidak ada baris ref perempuan -> semua z NULL, INSERT tetap sukses.
		s := insertAnthro(t, tx, person, "perempuan", 400, 8, 70, "{}")
		expectNil(t, "zscore_bbu", s.bbu)
		expectNil(t, "zscore_tbu", s.tbu)
		expectNil(t, "zscore_bbtb", s.bbtb)
		expectNil(t, "zscore_imtu", s.imtu)
		expectNil(t, "zscore_lku", s.lku)
		expectNil(t, "zscore_lila", s.lila)
	})

	t.Run("lila anak di luar usia", func(t *testing.T) {
		person := seedPerson(t, "lila100", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// age 100 hari: zscore_lila tetap dihitung dari tabel lila (<= 2192),
		// ln(12/13)/0.1 = -0.80; band lila_cm TIDAK diisi (hanya 183..1795 hari).
		s := insertAnthro(t, tx, person, "laki_laki", 100, 4.5, 60, `{"lila_cm":12.0}`)
		expectVal(t, "zscore_lila", s.lila, -0.80)
		if _, ok := s.status["lila_cm"]; ok {
			t.Fatalf("status[lila_cm] tidak boleh diisi di luar 6-59 bulan: %v", s.status)
		}
	})

	// seedRef sintetis per-subtest: baris ref dihapus lagi saat subtest selesai
	// (defer) supaya nearest-x subtest lain tidak terganggu.
	seedRef := func(t *testing.T, indicator, gender, xKind, sourceVersion string, x int, l, m, s float64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ref.growth_curve (indicator, gender, x_kind, x, l, m, s, source_name, source_version, source_url, downloaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'sintetis', $8, 'http://test.local/sintetis', now())`,
			indicator, gender, xKind, x, l, m, s, sourceVersion); err != nil {
			t.Fatalf("seed ref %s x=%d: %v", indicator, x, err)
		}
	}
	cleanupRef := func(t *testing.T, indicator, gender, sourceVersion string, x int) {
		t.Helper()
		t.Cleanup(func() {
			if _, err := pool.Exec(ctx,
				"DELETE FROM ref.growth_curve WHERE indicator=$1 AND gender=$2 AND source_version=$3 AND x=$4",
				indicator, gender, sourceVersion, x); err != nil {
				t.Fatalf("cleanup ref %s x=%d: %v", indicator, x, err)
			}
		})
	}

	// (a) anak 84 bln (2557 hari; floor(2557/30.4375)=84): z bbu/tbu/imtu
	// terisi dari sumber who-gr-2007, band domain ANAK (bbu z=1.2 → anak
	// 'gizi_lebih'; balita di rentang sama = 'risiko_gizi_lebih').
	t.Run("anak 84 bln sumber gr + band anak", func(t *testing.T) {
		for _, rc := range []struct {
			ind, xk string
			x       int
			l, m, s float64
		}{
			{"bbu", "age_days", 2557, 1, 20, 0.1},
			{"tbu", "age_days", 2557, 1, 100, 0.1},
			{"imtu", "age_days", 2557, 1, 20, 0.05},
		} {
			seedRef(t, rc.ind, "laki_laki", rc.xk, "who-gr-2007", rc.x, rc.l, rc.m, rc.s)
			cleanupRef(t, rc.ind, "laki_laki", "who-gr-2007", rc.x)
		}
		person := seedPerson(t, "anak84", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// bbu: (22.4/20-1)/0.1 = 1.20; tbu: (100/100-1)/0.1 = 0;
		// imt = round(22.4/1^2,1) = 22.4; imtu: (22.4/20-1)/0.05 = 2.40.
		s := insertAnthro(t, tx, person, "laki_laki", 2557, 22.4, 100, "{}")
		expectVal(t, "zscore_bbu", s.bbu, 1.20)
		expectVal(t, "zscore_tbu", s.tbu, 0)
		expectVal(t, "zscore_imtu", s.imtu, 2.40)
		for _, key := range []string{"bbu", "tbu", "imtu"} {
			band, ok := s.status[key].(map[string]any)
			if !ok {
				t.Fatalf("status[%s] hilang: %v", key, s.status)
			}
			code, _ := band["code"].(string)
			want := map[string]string{"bbu": "gizi_lebih", "tbu": "normal", "imtu": "obesitas"}[key]
			if code != want {
				t.Fatalf("status[%s].code: expected %s (band anak), got %s", key, want, code)
			}
		}
		if s.curveVersion == nil || *s.curveVersion != "who-gr-2007" {
			t.Fatalf("curve_version: expected who-gr-2007, got %v", s.curveVersion)
		}
	})

	// (b) 121 bln (3683 hari) & 130 bln (3957 hari): bbu NULL (batas 120 bln),
	// tbu/imtu tetap terisi (batas 228 bln).
	t.Run("anak 121 dan 130 bln bbu null", func(t *testing.T) {
		for _, rc := range []struct {
			ind     string
			x       int
			l, m, s float64
		}{
			{"tbu", 3683, 1, 120, 0.1},
			{"imtu", 3683, 1, 20, 0.05},
		} {
			seedRef(t, rc.ind, "laki_laki", "age_days", "who-gr-2007", rc.x, rc.l, rc.m, rc.s)
			cleanupRef(t, rc.ind, "laki_laki", "who-gr-2007", rc.x)
		}
		person := seedPerson(t, "anak121", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// tbu: (120/120-1)/0.1 = 0; imt = round(20/1.2^2,1) = 13.9;
		// imtu: (13.9/20-1)/0.05 = -6.10.
		s := insertAnthro(t, tx, person, "laki_laki", 3683, 20, 120, "{}")
		expectNil(t, "zscore_bbu@121bln", s.bbu)
		expectVal(t, "zscore_tbu@121bln", s.tbu, 0)
		expectVal(t, "zscore_imtu@121bln", s.imtu, -6.10)
		if s.curveVersion == nil || *s.curveVersion != "who-gr-2007" {
			t.Fatalf("curve_version 121: expected who-gr-2007, got %v", s.curveVersion)
		}
		// 130 bln: baris ref terdekat tetap x=3683 → nilai z sama.
		s = insertAnthro(t, tx, person, "laki_laki", 3957, 20, 120, "{}")
		expectNil(t, "zscore_bbu@130bln", s.bbu)
		expectVal(t, "zscore_tbu@130bln", s.tbu, 0)
		expectVal(t, "zscore_imtu@130bln", s.imtu, -6.10)
	})

	// (c) anak → imt_kategori NULL, imt tetap dihitung (D-KA-04); sudah
	// dicek di subtest 84/121/130; di sini eksplisit bersama (d).
	t.Run("imt kategori per usia", func(t *testing.T) {
		person := seedPerson(t, "kat84", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		s := insertAnthro(t, tx, person, "laki_laki", 2557, 22.4, 100, "{}")
		if s.imt == nil || *s.imt != 22.4 {
			t.Fatalf("imt anak: expected 22.4, got %v", s.imt)
		}
		if s.imtKategori != nil {
			t.Fatalf("imt_kategori anak: expected NULL, got %v", *s.imtKategori)
		}
		// (d) dewasa 30 th (11000 hari; 361 bln > 228): kategori terisi (regresi).
		personD := seedPerson(t, "katdewasa", "male", "1974-06-15")
		// imt = round(62/1.58^2,1) = 24.8 → bb_lebih.
		s2 := insertAnthro(t, tx, personD, "laki_laki", 11000, 62, 158, "{}")
		if s2.imtKategori == nil || *s2.imtKategori != "bb_lebih" {
			t.Fatalf("imt_kategori dewasa: expected bb_lebih, got %v", s2.imtKategori)
		}
	})

	// (e) balita 30 bln (913 hari; <= 60 bln): sumber who-cgs-2006 (regresi).
	t.Run("balita 30 bln sumber cgs", func(t *testing.T) {
		seedRef(t, "bbu", "laki_laki", "age_days", "who-cgs-2006", 913, 1, 10, 0.1)
		cleanupRef(t, "bbu", "laki_laki", "who-cgs-2006", 913)
		seedRef(t, "imtu", "laki_laki", "age_days", "who-cgs-2006", 913, 1, 15, 0.05)
		cleanupRef(t, "imtu", "laki_laki", "who-cgs-2006", 913)
		person := seedPerson(t, "balita30", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		// bbu: (10/10-1)/0.1 = 0; imt = round(10/0.75^2,1) = 17.8;
		// imtu: (17.8/15-1)/0.05 = 3.73 (blok terakhir → curve_version cgs).
		s := insertAnthro(t, tx, person, "laki_laki", 913, 10, 75, "{}")
		expectVal(t, "zscore_bbu", s.bbu, 0)
		expectVal(t, "zscore_imtu", s.imtu, 3.73)
		if s.imtKategori != nil {
			t.Fatalf("imt_kategori balita: expected NULL, got %v", *s.imtKategori)
		}
		if s.curveVersion == nil || *s.curveVersion != "who-cgs-2006" {
			t.Fatalf("curve_version: expected who-cgs-2006, got %v", s.curveVersion)
		}
	})

	// (f) extras lk_cm tersimpan tanpa z/band (D-KA-13).
	t.Run("extras lk_cm tanpa z", func(t *testing.T) {
		person := seedPerson(t, "lkcm", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		s := insertAnthro(t, tx, person, "laki_laki", 2000, 12, 82, `{"lk_cm":45.5}`)
		expectNil(t, "zscore_lku", s.lku) // tidak ada baris ref lku
		if _, ok := s.status["lku"]; ok {
			t.Fatalf("status[lku] tidak boleh ada tanpa ref: %v", s.status)
		}
		var lk string
		if err := tx.QueryRow(ctx,
			"SELECT extras->>'lk_cm' FROM clinical.anthropometry WHERE person_id=$1::uuid ORDER BY created_at DESC LIMIT 1",
			person).Scan(&lk); err != nil || lk != "45.5" {
			t.Fatalf("extras lk_cm tersimpan = %q (%v), want 45.5", lk, err)
		}
	})

	// (g) guard lama 2192 hari tetap untuk bbtb/lku/lila: 2200 hari
	// (72 bln, <= 228) → tbu/imtu masih bisa terisi, tapi bbtb/lila NULL.
	t.Run("guard 2192 bbtb lila", func(t *testing.T) {
		person := seedPerson(t, "guard2200", "male", nil)
		tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
		s := insertAnthro(t, tx, person, "laki_laki", 2200, 20, 120, `{"lila_cm":25.0}`)
		expectNil(t, "zscore_bbtb", s.bbtb)
		expectNil(t, "zscore_lku", s.lku)
		expectNil(t, "zscore_lila", s.lila)
		if _, ok := s.status["lila_cm"]; ok {
			t.Fatalf("status[lila_cm] tidak boleh diisi > 1795 hari: %v", s.status)
		}
	})

	// (h) POST episode profile 'anak' via handler + PATCH anak→dewasa
	// (validGiziProfiles dipakai Create/Update handler).
	t.Run("episode profile anak via handler", func(t *testing.T) {
		router := gzNewRouter(t, ctx, pool)
		envCompany, envMerchant, _, token := gzSeedEnv(t, ctx, pool, router, "anthroanak")
		_, adm := cfSeedAdmission(t, ctx, pool, router, token, envCompany, envMerchant, "gzanthroanak")
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, envCompany, envMerchant, map[string]any{
			"admission_id": adm, "profile": "anak",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST episode anak = %d: %s", rec.Code, rec.Body.String())
		}
		epID := resp["data"].(map[string]any)["id"].(string)
		rec, resp = cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+epID, token, envCompany, envMerchant, map[string]any{
			"row_version": 1, "profile": "dewasa",
		})
		if rec.Code != http.StatusOK || resp["data"].(map[string]any)["profile"] != "dewasa" {
			t.Fatalf("PATCH anak→dewasa = %d: %s", rec.Code, rec.Body.String())
		}
	})
}
