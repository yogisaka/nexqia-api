// internal/server/imunisasi_care.go
// Mesin status imunisasi anak per jenjang — plan 2026-11-24-anak-a2-imunisasi
// Task 2, spec §4 (aturan urutan) + §5 (respons snake_case). Fungsi murni
// computeImunStatus deterministik (param now time.Time); handler mengisi now
// dari merchantLocation + localDayStart (bukan time.Now() mentah). Tx ambient
// RunRequestTx: respons ≥400 otomatis rollback, 500 hanya respondInternalError.
package server

import (
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterImunisasiRoutes(r gin.IRouter) {
	r.GET("/clinical/immunization/status", GetImunisasiStatusHandler)
}

// imunRowStatus = satu baris jadwal + status hasil mesin §4.
type imunRowStatus struct {
	jadwal sqlcgen.RefImmunizationSchedule
	status string
	given  *sqlcgen.ClinicalPersonRiwayatVaksin // non-nil bila diberikan
}

// imunBerikutnya = dosis berikutnya yang belum jatuh tempo terlampaui (§4.7).
type imunBerikutnya struct {
	AntigenSlug        string
	Antigen            string // label jadwal (mis. "DPT-HB-Hib")
	DosisKe            int32
	UsiaTepatBln       int32
	TanggalRekomendasi string // YYYY-MM-DD, penambahan bulan kalender dari birth_date
}

// ImunStatusResult hasil mesin status — di-render snake_case oleh handler.
type ImunStatusResult struct {
	Rows       []imunRowStatus
	Agregat    string // telat | terjadwal | lengkap
	TelatCount int
	Berikutnya *imunBerikutnya
}

// imunPersonMonths = granularitas usia bulan bulat §4: floor(hari/30).
func imunPersonMonths(now, birth time.Time) int {
	v := math.Floor(now.Sub(birth).Hours() / 24 / 30)
	if v < 0 {
		return 0
	}
	return int(v)
}

// imunGenderMatch: schedule.gender memakai 'perempuan' (CHECK 000110),
// core.person.gender memakai 'male'/'female' (CHECK 000006) — normalisasi
// sebelum banding, bukan != mentah (laki-laki hpv → na; perempuan lolos).
func imunGenderMatch(jadwalGender, personGender string) bool {
	want := jadwalGender
	if want == "perempuan" {
		want = "female"
	}
	return want == personGender
}

// computeImunStatus — aturan spec §4 urutan persis per baris jadwal:
// 1 riwayat match → diberikan (menang atas na, area_khusus tetap diberikan);
// 2 gender mismatch → na; 3 area_khusus tanpa riwayat → na;
// 4 usia < min → terjadwal; 5 usia > min (dalam batas maks) → telat;
// 6 usia > maks → lewat_masa. usia == min (tepat) → terjadwal (belum telat —
// telat mensyaratkan strictly greater di §4.5). Agregat telat > terjadwal >
// lengkap; na/lewat_masa diabaikan. Berikutnya = baris belum-diberikan non-na
// dgn (usia_min − usia) terkecil ≥ 0, tie-break antigen asc lalu dosis asc
// (urutan input query sudah ORDER BY antigen_slug, dosis_ke — first-strict-less).
func computeImunStatus(jadwal []sqlcgen.RefImmunizationSchedule, riwayat []sqlcgen.ClinicalPersonRiwayatVaksin,
	birthDate pgtype.Date, gender string, now time.Time) ImunStatusResult {
	given := make(map[string]map[int32]*sqlcgen.ClinicalPersonRiwayatVaksin, len(riwayat))
	for i := range riwayat {
		r := &riwayat[i]
		m, ok := given[r.AntigenSlug]
		if !ok {
			m = map[int32]*sqlcgen.ClinicalPersonRiwayatVaksin{}
			given[r.AntigenSlug] = m
		}
		m[r.DosisKe] = r
	}
	usiaBln := 0
	if birthDate.Valid {
		usiaBln = imunPersonMonths(now, birthDate.Time)
	}
	res := ImunStatusResult{Rows: make([]imunRowStatus, 0, len(jadwal)), Agregat: "lengkap"}
	terjadwal := false
	for i := range jadwal {
		j := jadwal[i]
		row := imunRowStatus{jadwal: j, status: "terjadwal"}
		switch {
		case given[j.AntigenSlug][j.DosisKe] != nil: // 1: riwayat menang atas na
			row.status = "diberikan"
			row.given = given[j.AntigenSlug][j.DosisKe]
		case j.Gender.Valid && !imunGenderMatch(j.Gender.String, gender): // 2
			row.status = "na"
		case j.AreaKhusus.Valid: // 3: area fase lanjutan tanpa riwayat
			row.status = "na"
		case j.UsiaMinBln.Valid && usiaBln < int(j.UsiaMinBln.Int32): // 4
			row.status = "terjadwal"
		case (j.UsiaMaksBln.Valid && usiaBln > int(j.UsiaMaksBln.Int32)): // 6
			row.status = "lewat_masa"
		case j.UsiaMinBln.Valid: // 5: > usia_min, maks NULL atau ≤ maks → telat
			row.status = "telat"
		}
		switch row.status {
		case "telat":
			res.TelatCount++
		case "terjadwal":
			terjadwal = true
		}
		res.Rows = append(res.Rows, row)
	}
	switch {
	case res.TelatCount > 0:
		res.Agregat = "telat"
	case terjadwal:
		res.Agregat = "terjadwal"
	}
	// 7: berikutnya.
	var best *imunRowStatus
	bestDelta := 0
	for i := range res.Rows {
		r := &res.Rows[i]
		if r.status == "diberikan" || r.status == "na" || !r.jadwal.UsiaMinBln.Valid {
			continue
		}
		delta := int(r.jadwal.UsiaMinBln.Int32) - usiaBln
		if delta < 0 {
			continue
		}
		if best == nil || delta < bestDelta { // tie → pertama (antigen/dosis asc)
			best, bestDelta = r, delta
		}
	}
	if best != nil {
		rekom := ""
		if birthDate.Valid {
			rekom = birthDate.Time.AddDate(0, int(best.jadwal.UsiaMinBln.Int32), 0).Format("2006-01-02")
		}
		res.Berikutnya = &imunBerikutnya{
			AntigenSlug: best.jadwal.AntigenSlug, Antigen: best.jadwal.Label,
			DosisKe: best.jadwal.DosisKe, UsiaTepatBln: best.jadwal.UsiaMinBln.Int32,
			TanggalRekomendasi: rekom,
		}
	}
	return res
}

// imunRowJSON — respons baris snake_case persis spec §5.
func imunRowJSON(r imunRowStatus) gin.H {
	var given any
	if r.given != nil {
		given = gin.H{
			"given_date": dateJSON(r.given.GivenDate), "no_batch": textJSON(r.given.NoBatch),
			"tempat": textJSON(r.given.Tempat), "tempat_keterangan": textJSON(r.given.TempatKeterangan),
			"pemberi": textJSON(r.given.Pemberi),
		}
	}
	g := gin.H{
		"antigen_slug": r.jadwal.AntigenSlug, "antigen": r.jadwal.Label,
		"dosis_ke": r.jadwal.DosisKe, "kategori": r.jadwal.Kategori,
		"usia_min_bln": intText(r.jadwal.UsiaMinBln), "usia_maks_bln": intText(r.jadwal.UsiaMaksBln),
		"gender": textJSON(r.jadwal.Gender), "area_khusus": textJSON(r.jadwal.AreaKhusus),
		"status": r.status, "given_date": nil, "no_batch": nil, "tempat": nil,
		"tempat_keterangan": nil, "pemberi": nil,
	}
	if given != nil {
		gm := given.(gin.H)
		g["given_date"], g["no_batch"], g["tempat"] = gm["given_date"], gm["no_batch"], gm["tempat"]
		g["tempat_keterangan"], g["pemberi"] = gm["tempat_keterangan"], gm["pemberi"]
	}
	return g
}

func imunStatusJSON(res ImunStatusResult) gin.H {
	rows := make([]gin.H, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, imunRowJSON(r))
	}
	var berikutnya any
	if res.Berikutnya != nil {
		b := res.Berikutnya
		berikutnya = gin.H{
			"antigen": b.Antigen, "dosis_ke": b.DosisKe, "usia_tepat_bln": b.UsiaTepatBln,
			"tanggal_rekomendasi": b.TanggalRekomendasi,
		}
	}
	return gin.H{
		"agregat": res.Agregat, "telat_count": res.TelatCount,
		"berikutnya": berikutnya, "rows": rows,
	}
}

// GetImunisasiStatusHandler godoc
// @Summary Immunization schedule status of an admission's person (server-computed)
// @Description Mesin status spec §4: diberikan/na/terjadwal/telat/lewat_masa per dosis + agregat + berikutnya; status = properti person (episode gizi aktif tidak disyaratkan).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 422 {object} apiErrorResponse
// @Router /clinical/immunization/status [get]
func GetImunisasiStatusHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequireAnyPermissionForMerchant(c, merchantID, PermClinicalRecordView, PermClinicalNutritionWrite) {
		return
	}
	admissionID, okID := parseUUID(c.Query("admission_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okA := giziAdmission(c, q, merchantID, admissionID)
	if !okA {
		return
	}
	person, okP := kebPersonByCompany(c, q, admission.PersonID)
	if !okP {
		return
	}
	if !person.BirthDate.Valid {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "pasien tanpa tanggal lahir"})
		return
	}
	jadwal, err := q.ListProgramImmunizationSchedule(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	now := localDayStart(time.Now(), loc)
	// Riwayat semua antigen: ListPersonVaksin ber-filter antigen_slug — satu
	// panggilan per distinct antigen pada jadwal (kontrak: ListPersonVaksin).
	seen := map[string]bool{}
	var riwayat []sqlcgen.ClinicalPersonRiwayatVaksin
	for _, j := range jadwal {
		if seen[j.AntigenSlug] {
			continue
		}
		seen[j.AntigenSlug] = true
		rows, err := q.ListPersonVaksin(c.Request.Context(), sqlcgen.ListPersonVaksinParams{
			PersonID: admission.PersonID, AntigenSlug: j.AntigenSlug,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		riwayat = append(riwayat, rows...)
	}
	gender := ""
	if person.Gender.Valid {
		gender = person.Gender.String
	}
	res := computeImunStatus(jadwal, riwayat, person.BirthDate, gender, now)
	c.JSON(http.StatusOK, gin.H{"data": imunStatusJSON(res), "meta": gin.H{}})
}
