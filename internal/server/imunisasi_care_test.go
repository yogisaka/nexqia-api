//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Imunisasi anak per jenjang (plan 2026-11-24-anak-a2-imunisasi Task 3,
// spec §4 mesin status + §5.2 guard POST). Fixture pola kebidanan_care_test.go
// TestKebidananVaksin: helpers newTestPostgresPool/gzSeedEnv/cfSeedUserWithPerms/
// cfDo/seedAuditPersonAdmission; jadwal dari migration 000110 (25 baris
// kia-2024-kmk35-2025). `now` handler = localDayStart(time.Now(), tz merchant
// Asia/Jakarta default) — ekspektasi kontrak dihitung utk now = 2026-11-24.

var imCounter int64

func imNext() int64 { return atomic.AddInt64(&imCounter, 1) }

// imSeedPerson inserts a person with a fixed birth_date + gender directly
// (core.person.gender CHECK 'male'/'female', migration 000006).
func imSeedPerson(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, gender, birth, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender, birth_date) VALUES ($1, $2, $3, $4) RETURNING id::text",
		companyID, "Anak Imunisasi "+label, gender, birth).Scan(&id); err != nil {
		t.Fatalf("seed person %s: %v", label, err)
	}
	return id
}

// imSeedRiwayat inserts vaccination history rows (tempat CHECK
// faskes_ini/luar, migration 000109:166).
func imSeedRiwayat(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID, personID string, rows []struct {
	antigen string
	dosis   int
	given   string
}) {
	t.Helper()
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `
			INSERT INTO clinical.person_riwayat_vaksin
				(company_id, merchant_id, person_id, antigen_slug, dosis_ke, given_date, tempat)
			VALUES ($1, $2, $3, $4, $5, $6, 'faskes_ini')`,
			companyID, merchantID, personID, r.antigen, r.dosis, r.given); err != nil {
			t.Fatalf("seed riwayat %s-%d: %v", r.antigen, r.dosis, err)
		}
	}
}

// imRowByAntigen indexes data.rows of the status endpoint by
// "<antigen_slug>-<dosis_ke>".
func imRowByAntigen(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range data["rows"].([]any) {
		r := raw.(map[string]any)
		key := fmt.Sprintf("%s-%d", r["antigen_slug"], int(r["dosis_ke"].(float64)))
		out[key] = r
	}
	return out
}

// TestImunisasiStatus — mesin status spec §4 pada kasus kontrak "Bara":
// lahir 2025-09-10, now = 2026-11-24 (WIB) → usia floor(440/30) = 14 bln.
// Riwayat 16 dosis; telat = opv-4 (usia 14 > min 4 ≤ maks 59) + pcv-3
// (14 > 12 ≤ 59) → agregat telat, telat_count 2. dpt_hb_hib-4 min 18 > 14 →
// terjadwal dan berikutnya (delta 4, tie antigen asc dpt_hb_hib < mr).
// tanggal_rekomendasi = 2025-09-10 AddDate(0,18,0) = 2027-03-10 kalender.
// rv-1..3/ipv-1..2 ber-area_khusus tapi punya riwayat → diberikan (rule 1
// menang atas na). Laki-laki 2020-01-01 (usia 83): hpv na (gender), bcg-1 &
// opv-1 lewat_masa (> maks). Perempuan sama: hpv terjadwal (83 < min 132).
func TestImunisasiStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, fmt.Sprintf("imst%d", imNext()))

	personID := imSeedPerson(t, ctx, pool, companyID, "male", "2025-09-10", fmt.Sprintf("bara%d", imNext()))
	admissionID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "imbara")
	// 16 dosis Bara (kontrak plan Task 3).
	imSeedRiwayat(t, ctx, pool, companyID, merchantID, personID, []struct {
		antigen string
		dosis   int
		given   string
	}{
		{"hb", 1, "2025-09-10"},
		{"bcg", 1, "2025-10-15"}, {"opv", 1, "2025-10-15"},
		{"dpt_hb_hib", 1, "2025-11-12"}, {"pcv", 1, "2025-11-12"}, {"rv", 1, "2025-11-12"},
		{"dpt_hb_hib", 2, "2025-12-10"}, {"pcv", 2, "2025-12-10"}, {"rv", 2, "2025-12-10"}, {"opv", 2, "2025-12-10"},
		{"dpt_hb_hib", 3, "2026-01-10"}, {"opv", 3, "2026-01-10"}, {"rv", 3, "2026-01-10"}, {"ipv", 1, "2026-01-10"},
		{"mr", 1, "2026-07-10"}, {"ipv", 2, "2026-07-10"},
	})

	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/immunization/status?admission_id="+admissionID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)
	if data["agregat"] != "telat" || int(data["telat_count"].(float64)) != 2 {
		t.Fatalf("agregat = %v %v", data["agregat"], data["telat_count"])
	}
	b := data["berikutnya"].(map[string]any)
	// 2025-09-10 + 18 bulan kalender (AddDate) = 2027-03-10 — bukan 18×30 hr.
	if b["antigen"] != "DPT-HB-Hib" || int(b["dosis_ke"].(float64)) != 4 ||
		int(b["usia_tepat_bln"].(float64)) != 18 || b["tanggal_rekomendasi"] != "2027-03-10" {
		t.Fatalf("berikutnya = %v", b)
	}
	rows := imRowByAntigen(t, data)
	want := map[string]struct{ status, area string }{
		"hb-1":  {"diberikan", ""},
		"bcg-1": {"diberikan", ""},
		"opv-1": {"diberikan", ""}, "opv-2": {"diberikan", ""}, "opv-3": {"diberikan", ""},
		"opv-4":        {"telat", ""},
		"dpt_hb_hib-1": {"diberikan", ""}, "dpt_hb_hib-2": {"diberikan", ""}, "dpt_hb_hib-3": {"diberikan", ""},
		"dpt_hb_hib-4": {"terjadwal", ""},
		"pcv-1":        {"diberikan", ""}, "pcv-2": {"diberikan", ""},
		"pcv-3": {"telat", ""},
		"rv-1":  {"diberikan", "rv"}, "rv-2": {"diberikan", "rv"}, "rv-3": {"diberikan", "rv"},
		"ipv-1": {"diberikan", "ipv"}, "ipv-2": {"diberikan", "ipv"},
		"mr-1":  {"diberikan", ""},
		"mr-2":  {"terjadwal", ""},
		"je-1":  {"na", "je"},
		"hpv-1": {"na", ""},
	}
	for key, w := range want {
		r, ok := rows[key]
		if !ok {
			t.Fatalf("row %s hilang (rows=%d)", key, len(rows))
		}
		if r["status"] != w.status {
			t.Fatalf("row %s status = %v, want %s", key, r["status"], w.status)
		}
		if w.area != "" && r["area_khusus"] != w.area {
			t.Fatalf("row %s area_khusus = %v, want %s (badge area tetap tampil)", key, r["area_khusus"], w.area)
		}
	}

	// Kasus kedua: laki-laki 2020-01-01 tanpa riwayat (usia 2519/30 → 83 bln).
	personB := imSeedPerson(t, ctx, pool, companyID, "male", "2020-01-01", fmt.Sprintf("laki%d", imNext()))
	admB, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personB, "imlaki")
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/immunization/status?admission_id="+admB, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status B = %d: %s", rec.Code, rec.Body.String())
	}
	rowsB := imRowByAntigen(t, resp["data"].(map[string]any))
	for key, wantStatus := range map[string]string{"hpv-1": "na", "bcg-1": "lewat_masa", "opv-1": "lewat_masa"} {
		if rowsB[key]["status"] != wantStatus {
			t.Fatalf("kasus2 %s = %v, want %s", key, rowsB[key]["status"], wantStatus)
		}
	}

	// Kasus ketiga: perempuan sama → hpv lolos gender, 83 < min 132 → terjadwal.
	personC := imSeedPerson(t, ctx, pool, companyID, "female", "2020-01-01", fmt.Sprintf("perempuan%d", imNext()))
	admC, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personC, "impr")
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/immunization/status?admission_id="+admC, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status C = %d: %s", rec.Code, rec.Body.String())
	}
	rowsC := imRowByAntigen(t, resp["data"].(map[string]any))
	if rowsC["hpv-1"]["status"] != "terjadwal" {
		t.Fatalf("kasus3 hpv-1 = %v, want terjadwal", rowsC["hpv-1"]["status"])
	}
}

// TestPersonVaccinationGuard — guard A2 POST person-vaccination (spec §5.2,
// gate td-only F1 dihapus): (a) antigen ngarang `zzz` (riwayat kosong →
// ordinal 1, tidak ada di jadwal program) → 422 "antigen/dosis tidak ada di
// jadwal program"; (b) `opv` dgn 3 riwayat → ordinal 4 (ada di jadwal) → 201
// dgn sesi clinical.nutrition.write saja; (c) `mr` saat usia 5 bln (< min 9)
// → 422 "imunisasi tidak boleh diberikan lebih cepat dari jadwal". Alur F1
// Td (person tanpa birth_date → lolos guard lebih-cepat) diregresi
// TestKebidananVaksin (tetap hijau).
func TestPersonVaccinationGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, fmt.Sprintf("imgd%d", imNext()))
	personID := imSeedPerson(t, ctx, pool, companyID, "male", "2025-01-01", fmt.Sprintf("guard%d", imNext()))

	// (a) antigen ngarang → 422 persis.
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		map[string]any{"person_id": personID, "antigen_slug": "zzz", "given_date": "2026-11-24", "tempat": "faskes_ini"})
	if rec.Code != http.StatusUnprocessableEntity || resp["error"] != "antigen/dosis tidak ada di jadwal program" {
		t.Fatalf("zzz = %d %v", rec.Code, resp)
	}

	// (b) opv 3 riwayat → ordinal 4 → 201 dengan izin clinical.nutrition.write.
	imSeedRiwayat(t, ctx, pool, companyID, merchantID, personID, []struct {
		antigen string
		dosis   int
		given   string
	}{
		{"opv", 1, "2025-02-10"}, {"opv", 2, "2025-03-10"}, {"opv", 3, "2025-04-10"},
	})
	nutWrite := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, fmt.Sprintf("imgdnut%d", imNext()), "clinical.nutrition.write")
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", nutWrite, companyID, merchantID,
		map[string]any{"person_id": personID, "antigen_slug": "opv", "given_date": "2026-10-01", "tempat": "faskes_ini", "no_batch": "OPV-004"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("opv = %d: %s", rec.Code, rec.Body.String())
	}
	if d := resp["data"].(map[string]any); d["antigen_slug"] != "opv" || d["dosis_ke"].(float64) != 4 || d["given_date"] != "2026-10-01" {
		t.Fatalf("opv data = %v", d)
	}

	// (c) guard lebih-cepat: person lahir 2026-06-01, given 2026-11-01 →
	// usia floor(153/30) = 5 bln < usia_min mr = 9 → 422 persis (respons ≥400
	// auto-rollback ambient tx — tidak tersimpan).
	personMR := imSeedPerson(t, ctx, pool, companyID, "male", "2026-06-01", fmt.Sprintf("mrcepat%d", imNext()))
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/person-vaccination", token, companyID, merchantID,
		map[string]any{"person_id": personMR, "antigen_slug": "mr", "given_date": "2026-11-01", "tempat": "faskes_ini"})
	if rec.Code != http.StatusUnprocessableEntity || resp["error"] != "imunisasi tidak boleh diberikan lebih cepat dari jadwal" {
		t.Fatalf("mr lebih-cepat = %d %v", rec.Code, resp)
	}
}
