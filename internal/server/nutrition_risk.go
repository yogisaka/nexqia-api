// internal/server/nutrition_risk.go
// Rules engine risiko gizi D5 — plan 2026-11-13-gizi-nutrition-care chunk 3
// Task 2 (spec 2026-11-13-gizi-nutrition-care-spec §6, FR-GZ-08c): trigger
// fase 1 → level rendah/sedang/tinggi, snapshot instrument 'rules'
// (score = jumlah pemicu, max_score 10 — 10 pemicu sejak rules anak plan
// 2026-11-18-anak-a1-1-be Task 3) ditulis SETELAH write utama sukses
// dalam tx ambient yang sama (POST antropometri, PATCH episode, hook
// skrining MST/MNA-SF/STAMP).
package server

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// giziBandStatus bentuk item jsonb clinical.anthropometry.status yang ditulis
// trigger snapshot (migration 000097): {"bbu":{"code","label","risk"}, ...}.
type giziBandStatus struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	Risk  string `json:"risk"`
}

// giziNumVal → (float64, ok) dari kolom numeric (NULL → false).
func giziNumVal(n pgtype.Numeric) (float64, bool) {
	v, err := n.Float64Value()
	if err != nil || !v.Valid {
		return 0, false
	}
	return v.Float64, true
}

// giziFmtNum format angka desimal koma untuk reason (kontrak: "BB/U -2,4").
func giziFmtNum(f float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(f, 'f', prec, 64), ".", ",", 1)
}

// giziAnakIndLabel label pendek indikator anak untuk reason zscore_anak.
var giziAnakIndLabel = map[string]string{
	"bbu": "BB/U", "tbu": "TB/U", "bbtb": "BB/TB", "imtu": "IMT/U", "lku": "LK/U",
}

// giziAnakIndZ urutan prioritas band anak: BB/U dulu (spec §6).
var giziAnakIndZ = []string{"bbu", "bbtb", "imtu", "tbu", "lku"}

// Konstanta rules anak (plan 2026-11-18-anak-a1-1-be Task 3, D-KA-12) —
// nilai klinis tunable via konstanta (bukan env — bukan ops deployment):
// tbu_menurun / imtu_buruk pakai delta z dua pengukuran terakhir; domain
// usia anak = 61–228 bln (floor(age_days/30.4375), sinkron trigger 000105).
const (
	tbuMenurunDelta      = -0.5
	imtuBurukDelta       = -0.5
	imtuBurukZ           = -2.0
	giziAnakMinAgeMonths = 61
	giziAnakMaxAgeMonths = 228
)

// evaluateGiziRisk menghitung rules fase 1 spec §6 untuk episode dengan
// profil `profile`: (a) seri antropometri admission (bb/imt, extras lila,
// band status), (b) hasil skrining mst/mnasf terbaru, (c) Hb + warna tinja
// dari form asuhan_gizi terbaru (sections.biokimia/tinja, item terakhir
// menang). Gate zscore_anak: HANYA dievaluasi bila profile == 'balita'
// (review fix #1 — dewasa/lansia/bumil/menyusui TIDAK PERNAH dipicu rules
// anak). riskJSON berisi {level, triggers, reason}; handler GET menambahkan
// "override" dari episode.risk_override.
func evaluateGiziRisk(ctx context.Context, q *sqlcgen.Queries, personID, admissionID pgtype.UUID, profile string) (gin.H, []gin.H, error) {
	screen, err := q.ListLatestNutritionScreeningByAdmission(ctx, admissionID)
	if err != nil {
		return nil, nil, err
	}
	mstScore, mnasfScore := 0, 0
	var hasMst, hasMnasf bool
	for _, r := range screen {
		switch r.Instrument {
		case "mst":
			mstScore, hasMst = int(r.Score), true
		case "mnasf":
			mnasfScore, hasMnasf = int(r.Score), true
		}
	}
	rows, err := q.ListAnthropometryByAdmission(ctx, admissionID)
	if err != nil {
		return nil, nil, err
	}
	type giziMeas struct {
		at                 time.Time
		ageDays            int
		hasAge             bool
		bb, imt, lila, mgg float64
		hasBB, hasIMT      bool
		hasLila, hasMgg    bool
		z                  map[string]float64
		bands              map[string]giziBandStatus
	}
	meas := make([]giziMeas, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // query DESC → kronologis ASC
		r := rows[i]
		m := giziMeas{at: r.MeasuredAt.Time, z: map[string]float64{}, bands: map[string]giziBandStatus{}}
		if r.AgeDays.Valid {
			m.ageDays, m.hasAge = int(r.AgeDays.Int32), true
		}
		if v, ok := giziNumVal(r.BbKg); ok {
			m.bb, m.hasBB = v, true
		}
		if v, ok := giziNumVal(r.Imt); ok {
			m.imt, m.hasIMT = v, true
		}
		if len(r.Extras) > 0 {
			var ex map[string]json.RawMessage
			if json.Unmarshal(r.Extras, &ex) == nil {
				if raw, ok := ex["lila_cm"]; ok {
					var v float64
					if json.Unmarshal(raw, &v) == nil {
						m.lila, m.hasLila = v, true
					}
				}
				if raw, ok := ex["mgg"]; ok {
					var v float64
					if json.Unmarshal(raw, &v) == nil {
						m.mgg, m.hasMgg = v, true
					}
				}
			}
		}
		zcols := map[string]pgtype.Numeric{
			"bbu": r.ZscoreBbu, "tbu": r.ZscoreTbu, "bbtb": r.ZscoreBbtb,
			"imtu": r.ZscoreImtu, "lku": r.ZscoreLku,
		}
		for ind, col := range zcols {
			if v, ok := giziNumVal(col); ok {
				m.z[ind] = v
			}
		}
		if len(r.Status) > 0 {
			var st map[string]giziBandStatus
			if json.Unmarshal(r.Status, &st) == nil {
				m.bands = st
			}
		}
		meas = append(meas, m)
	}
	// Hb + warna tinja dari form asuhan_gizi terbaru (draft/signed).
	forms, err := q.ListAsuhanGiziByAdmission(ctx, admissionID)
	if err != nil {
		return nil, nil, err
	}
	var hb float64
	var hasHb bool
	tinjaNomor := 0
	if len(forms) > 0 {
		var fd struct {
			Sections map[string]json.RawMessage `json:"sections"`
		}
		if err := json.Unmarshal(forms[0].FormData, &fd); err == nil {
			if raw, ok := fd.Sections["biokimia"]; ok {
				var bio []map[string]json.RawMessage
				if json.Unmarshal(raw, &bio) == nil {
					for _, row := range bio { // item hb TERAKHIR menang
						var item string
						if json.Unmarshal(row["item"], &item) != nil || item != "hb" {
							continue
						}
						var n json.Number
						if json.Unmarshal(row["nilai"], &n) == nil {
							if v, err := n.Float64(); err == nil {
								hb, hasHb = v, true
							}
						}
					}
				}
			}
			if raw, ok := fd.Sections["tinja"]; ok {
				var tj []map[string]json.RawMessage
				if json.Unmarshal(raw, &tj) == nil && len(tj) > 0 {
					var n json.Number
					if json.Unmarshal(tj[len(tj)-1]["nomor"], &n) == nil {
						if v, err := n.Int64(); err == nil {
							tinjaNomor = int(v)
						}
					}
				}
			}
		}
	}

	triggers := []gin.H{}
	// mst (>=2; 4–5 → level tinggi).
	if hasMst && mstScore >= 2 {
		risk := "sedang"
		if mstScore >= 4 {
			risk = "tinggi"
		}
		triggers = append(triggers, gin.H{"code": "mst", "risk": risk, "label": "MST " + strconv.Itoa(mstScore)})
	}
	// mnasf (<12; <8 → level tinggi).
	if hasMnasf && mnasfScore < 12 {
		risk := "sedang"
		if mnasfScore < 8 {
			risk = "tinggi"
		}
		triggers = append(triggers, gin.H{"code": "mnasf", "risk": risk, "label": "MNA-SF " + strconv.Itoa(mnasfScore)})
	}
	// bb_stagnan / bb_turun: seri bb usia <= 60 bln (1826 hari).
	var bbSeries []giziMeas
	var imtSeries []giziMeas
	for _, m := range meas {
		if m.hasBB && m.hasAge && m.ageDays <= 1826 {
			bbSeries = append(bbSeries, m)
		}
		if m.hasIMT {
			imtSeries = append(imtSeries, m)
		}
	}
	if len(bbSeries) >= 2 {
		delta := bbSeries[len(bbSeries)-1].bb - bbSeries[len(bbSeries)-2].bb
		if delta <= 0 {
			triggers = append(triggers, gin.H{"code": "bb_stagnan", "risk": "sedang",
				"label": "BB stagnan (" + giziFmtNum(delta, 1) + " kg)"})
		}
		latest := bbSeries[len(bbSeries)-1]
		for i := len(bbSeries) - 2; i >= 0; i-- {
			dt := latest.at.Sub(bbSeries[i].at)
			if dt >= 30*24*time.Hour && dt <= 183*24*time.Hour {
				if pct := (latest.bb - bbSeries[i].bb) / bbSeries[i].bb * 100; pct <= -5 {
					triggers = append(triggers, gin.H{"code": "bb_turun", "risk": "sedang",
						"label": "BB turun " + giziFmtNum(-pct, 1) + "%"})
				}
				break // kandidat terbaru dalam jendela 1–6 bulan
			}
		}
	}
	// imt_flatten: >= 2 imt rutin |Δ|<=0,1 DAN MST >= 2.
	if hasMst && mstScore >= 2 {
		for i := 1; i < len(imtSeries); i++ {
			d := imtSeries[i].imt - imtSeries[i-1].imt
			if d < 0 {
				d = -d
			}
			if d <= 0.1 {
				triggers = append(triggers, gin.H{"code": "imt_flatten", "risk": "sedang", "label": "IMT mendatar"})
				break
			}
		}
	}
	// lila_kek (bumil, lila terakhir < 23,5) + hb_rendah (bumil, Hb < cutoff
	// trimester — lihat blok hb_rendah di bawah).
	var lastLila float64
	var hasLilaAny bool
	for i := len(meas) - 1; i >= 0; i-- {
		if meas[i].hasLila {
			lastLila, hasLilaAny = meas[i].lila, true
			break
		}
	}
	lilaKek := false
	if profile == "bumil" && hasLilaAny && lastLila < 23.5 {
		lilaKek = true
		triggers = append(triggers, gin.H{"code": "lila_kek", "risk": "sedang",
			"label": "LiLA " + giziFmtNum(lastLila, 1) + " cm"})
	}
	// hb_rendah (bumil) trimester-aware (plan Task 3, spec §5): cutoff
	// <10,5 bila mgg antropometri terbaru 14–27 (trimester t2), tetap <11
	// bila t1/t3/tanpa mgg.
	var lastMgg float64
	var hasMggAny bool
	for i := len(meas) - 1; i >= 0; i-- {
		if meas[i].hasMgg {
			lastMgg, hasMggAny = meas[i].mgg, true
			break
		}
	}
	hbCutoff := 11.0
	if hasMggAny && lastMgg >= 14 && lastMgg <= 27 {
		hbCutoff = 10.5
	}
	hbRendah := false
	if profile == "bumil" && hasHb && hb < hbCutoff {
		hbRendah = true
		triggers = append(triggers, gin.H{"code": "hb_rendah", "risk": "sedang",
			"label": "Hb " + giziFmtNum(hb, 1)})
	}
	// hb_rendah (balita, plan 2026-11-19-gizi-balita-1-be Task 3, D-BT-02):
	// Hb < 11 pada usia efektif 6–59 bln (am kronologis baris antropometri
	// terbaru) → level sedang. Usia tak diketahui / di luar 6–59 → tanpa
	// pemicu.
	if profile == "balita" && hasHb && hb < 11.0 {
		for i := len(meas) - 1; i >= 0; i-- {
			if !meas[i].hasAge {
				continue
			}
			if am := math.Floor(float64(meas[i].ageDays) / 30.4375); am >= 6 && am <= 59 {
				hbRendah = true
				triggers = append(triggers, gin.H{"code": "hb_rendah", "risk": "sedang",
					"label": "Hb < 11 g/dL (balita 6–59 bln)"})
			}
			break
		}
	}
	// zscore_anak — GATE profil balita|anak (plan 2026-11-18-anak-a1-1-be
	// Task 3: profil 'anak' ikut; review fix #1 tetap: dewasa/lansia/bumil/
	// menyusui TIDAK PERNAH dipicu rules anak): band risk sedang/tinggi,
	// prioritas BB/U, baris antropometri terbaru dulu.
	zscoreAnakTinggi := false
	if profile == "balita" || profile == "anak" {
	out:
		for i := len(meas) - 1; i >= 0; i-- {
			for _, ind := range giziAnakIndZ {
				b, ok := meas[i].bands[ind]
				if !ok || (b.Risk != "sedang" && b.Risk != "tinggi") {
					continue
				}
				label := giziAnakIndLabel[ind]
				if z, ok := meas[i].z[ind]; ok {
					label += " " + giziFmtNum(z, 1)
				}
				triggers = append(triggers, gin.H{"code": "zscore_anak", "risk": b.Risk, "label": label})
				if b.Risk == "tinggi" {
					zscoreAnakTinggi = true
				}
				break out
			}
		}
	}
	// tbu_menurun (D-KA-12) & imtu_buruk (plan 2026-11-18-anak-a1-1-be
	// Task 3): rules anak usia 61–228 bln dari seri z snapshot, level
	// sedang. tbu_menurun: Δ zscore_tbu ≤ −0,5 dua pengukuran terakhir.
	// imtu_buruk: Δ zscore_imtu ≤ −0,5 ATAU z IMT/U terbaru < −2.
	anakZSeries := func(ind string) []float64 {
		zs := make([]float64, 0, 2)
		for _, m := range meas {
			if !m.hasAge {
				continue
			}
			am := math.Floor(float64(m.ageDays) / 30.4375)
			if am < giziAnakMinAgeMonths || am > giziAnakMaxAgeMonths {
				continue
			}
			if z, ok := m.z[ind]; ok {
				zs = append(zs, z)
			}
		}
		return zs
	}
	if tbu := anakZSeries("tbu"); len(tbu) >= 2 {
		if delta := tbu[len(tbu)-1] - tbu[len(tbu)-2]; delta <= tbuMenurunDelta {
			triggers = append(triggers, gin.H{"code": "tbu_menurun", "risk": "sedang",
				"label": "TB/U menurun " + giziFmtNum(delta, 2)})
		}
	}
	if imtu := anakZSeries("imtu"); len(imtu) >= 1 && imtu[len(imtu)-1] < imtuBurukZ {
		triggers = append(triggers, gin.H{"code": "imtu_buruk", "risk": "sedang",
			"label": "IMT/U " + giziFmtNum(imtu[len(imtu)-1], 2)})
	} else if len(imtu) >= 2 {
		if delta := imtu[len(imtu)-1] - imtu[len(imtu)-2]; delta <= imtuBurukDelta {
			triggers = append(triggers, gin.H{"code": "imtu_buruk", "risk": "sedang",
				"label": "IMT/U menurun " + giziFmtNum(delta, 2)})
		}
	}
	// tinja_pucat: display-only warna 1–3 (tidak memengaruhi level).
	if tinjaNomor >= 1 && tinjaNomor <= 3 {
		triggers = append(triggers, gin.H{"code": "tinja_pucat", "display_only": true,
			"label": "Warna tinja " + strconv.Itoa(tinjaNomor)})
	}
	// Level: tinggi bila MST 4–5 / MNA-SF <8 / band anak tinggi /
	// (lila_kek DAN hb_rendah); sedang bila >= 1 pemicu lain; rendah nihil.
	level := "rendah"
	if (hasMst && mstScore >= 4) || (hasMnasf && mnasfScore < 8) || zscoreAnakTinggi {
		level = "tinggi"
	}
	if level != "tinggi" && lilaKek && hbRendah {
		level = "tinggi"
	}
	if level == "rendah" && len(triggers) > 0 {
		level = "sedang"
	}
	reasons := make([]string, 0, len(triggers))
	for _, t := range triggers {
		if l, ok := t["label"].(string); ok {
			reasons = append(reasons, l)
		}
	}
	return gin.H{"level": level, "triggers": triggers, "reason": strings.Join(reasons, "; ")}, triggers, nil
}

// upsertGiziRulesSnapshot menulis hasil rules ke
// clinical.nutrition_screening_result (instrument 'rules', max_score 10 —
// literal SQL queries/gizi.sql ikut 8→10, plan 2026-11-18-anak-a1-1-be
// Task 3; score = jumlah pemicu, payload {triggers, reason}). Idempoten per
// admission: UPDATE dulu; 0 baris → INSERT (tanpa form_instance — di luar
// index partial uq_nsr_form_instrument).
func upsertGiziRulesSnapshot(ctx context.Context, q *sqlcgen.Queries, companyID, merchantID, personID, admissionID, episodeID pgtype.UUID, risk gin.H, triggers []gin.H, userID pgtype.UUID) error {
	level, _ := risk["level"].(string)
	pl, err := json.Marshal(map[string]any{"triggers": triggers, "reason": risk["reason"]})
	if err != nil {
		return err
	}
	n, err := q.UpdateGiziRulesSnapshot(ctx, sqlcgen.UpdateGiziRulesSnapshotParams{
		AdmissionID: admissionID, Score: int32(len(triggers)), Interpretation: level,
		Payload: pl, EpisodeID: episodeID, CalculatedBy: userID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return q.InsertGiziRulesSnapshot(ctx, sqlcgen.InsertGiziRulesSnapshotParams{
			CompanyID: companyID, MerchantID: merchantID, PersonID: personID,
			AdmissionID: admissionID, EpisodeID: episodeID, Score: int32(len(triggers)),
			Interpretation: level, Payload: pl, CalculatedBy: userID,
		})
	}
	return nil
}

// refreshGiziRules evaluasi ulang rules + tulis snapshot SETELAH write
// utama sukses (tx ambient; error → respondInternalError → rollback ambient
// via RunRequestTx). Profil dibaca ulang dari baris episode agar selalu
// profil efektif terbaru.
func refreshGiziRules(c *gin.Context, q *sqlcgen.Queries, companyID, merchantID, personID, admissionID, episodeID pgtype.UUID) bool {
	ctx := c.Request.Context()
	episode, err := q.GetNutritionEpisodeByID(ctx, episodeID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	risk, triggers, err := evaluateGiziRisk(ctx, q, personID, admissionID, episode.Profile)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if err := upsertGiziRulesSnapshot(ctx, q, companyID, merchantID, personID, admissionID, episodeID, risk, triggers, AuthUserID(c)); err != nil {
		respondInternalError(c, err)
		return false
	}
	return true
}
