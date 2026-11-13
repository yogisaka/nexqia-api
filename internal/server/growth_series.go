// internal/server/growth_series.go
// Gizi chunk 3 Task 1 (plan 2026-11-13-gizi-nutrition-care-chunk-3-growth-
// early-warning, spec 2026-11-13-gizi-nutrition-care-spec §5): endpoint
// GET /clinical/growth-series — series kurva LMS z=-3..+3 dari ref.growth_curve
// + titik pasien dari snapshot antropometri (FR-GZ-14) + band berat badan ibu
// hamil ICM/IOM 2009 (FR-GZ-08b). Kurva LMS di-cache in-proc 5 menit per
// (indicator, gender, source_version) / band; titik pasien TIDAK di-cache.
// Semua handler jalan di tx ambient RunRequestTx (500 → rollback otomatis).
package server

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterGrowthSeriesRoutes(rg *gin.RouterGroup) {
	rg.GET("/clinical/growth-series", AccessLog("growth_series", "view", ""), GetGrowthSeriesHandler)
}

var validGrowthIndicators = map[string]bool{
	"bbu": true, "tbu": true, "bbtb": true, "imtu": true, "lku": true, "lila": true, "bumil_bb": true,
}
var validBumilBands = map[string]bool{"kurus": true, "normal": true, "gemuk": true, "obes": true}

// Unit per indikator KMS — map LITERAL kontrak.
var growthIndicatorUnits = map[string]string{
	"bbu": "kg", "tbu": "cm", "bbtb": "kg", "imtu": "kg/m²", "lku": "cm", "lila": "cm",
}

// growthCurveZ adalah urutan z baris kurva (kunci JSON fixed-order).
var growthCurveZ = []float64{-3, -2, -1, 0, 1, 2, 3}
var growthCurveZKeys = []string{"-3", "-2", "-1", "0", "+1", "+2", "+3"}

// growthCacheTTL kontrak: cache in-proc 5 menit.
const growthCacheTTL = 5 * time.Minute

// growthCacheEntry menyimpan kurva LMS terkomputasi (+ sumber) atau band
// bumil. Titik pasien tidak pernah masuk cache.
type growthCacheEntry struct {
	at      time.Time
	curve   []gin.H
	version string
	url     string
	bands   gin.H
}

var (
	growthCacheMu sync.RWMutex
	growthCache   = map[string]growthCacheEntry{}
)

// bumilBands — band ICM/IOM 2009, nilai LITERAL kontrak (R:54).
func bumilBands() gin.H {
	return gin.H{
		"kurus":  gin.H{"min": 12.5, "max": 18.0},
		"normal": gin.H{"min": 11.5, "max": 16.0},
		"gemuk":  gin.H{"min": 7.0, "max": 11.5},
		"obes":   gin.H{"min": 5.0, "max": 9.0},
	}
}

// growthLMSValue menghitung satu titik kurva dari definisi LMS (spec chunk 1
// Task 1): v = round(M·(1+L·S·z)^(1/L), 3); L=0 → round(M·exp(S·z), 3).
func growthLMSValue(l, m, s, z float64) float64 {
	var v float64
	if l == 0 {
		v = m * math.Exp(s*z)
	} else {
		v = m * math.Pow(1+l*s*z, 1/l)
	}
	return math.Round(v*1000) / 1000
}

// numericFloat melonggarkan pgtype.Numeric ke float64 (NULL → 0; pemanggil
// wajib cek .Valid sebelum memakai).
func numericFloat(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}

// giziExtrasFloat membaca satu angka dari extras jsonb pengukuran.
func giziExtrasFloat(raw []byte, key string) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var ex map[string]any
	if err := json.Unmarshal(raw, &ex); err != nil {
		return 0, false
	}
	v, ok := ex[key].(float64)
	return v, ok
}

// buildGrowthCurve memetakan baris ref (x ASC) ke seri {x, lines{z→v}} —
// interpolasi DILARANG: hanya baris tabel. Mengembalikan juga sumber
// (source_version maksimum + url baris versi itu).
func buildGrowthCurve(rows []sqlcgen.ListGrowthCurveByIndicatorGenderRow) ([]gin.H, string, string) {
	curve := make([]gin.H, 0, len(rows))
	version, url := "", ""
	for i, r := range rows {
		if i == 0 || r.SourceVersion > version {
			version, url = r.SourceVersion, r.SourceUrl
		}
		l, m, s := numericFloat(r.L), numericFloat(r.M), numericFloat(r.S)
		lines := gin.H{}
		for j, z := range growthCurveZ {
			lines[growthCurveZKeys[j]] = growthLMSValue(l, m, s, z)
		}
		curve = append(curve, gin.H{"x": numericFloat(r.X), "lines": lines})
	}
	return curve, version, url
}

// growthRefRange mengembalikan rentang tampilan zscore titik: [minX-step,
// maxX+step] dengan step = selisih x terkecil antar baris ref (1 bila < 2
// baris). Titik di luar rentang ini TAMPIL zscore:null (aturan tampilan
// spec §4 (g): endpoint MENYALIN z snapshot tanpa menghitung ulang).
func growthRefRange(rows []sqlcgen.ListGrowthCurveByIndicatorGenderRow) (low, high float64, ok bool) {
	if len(rows) == 0 {
		return 0, 0, false
	}
	low = numericFloat(rows[0].X)
	high = numericFloat(rows[len(rows)-1].X)
	step := 1.0
	for i := 1; i < len(rows); i++ {
		d := numericFloat(rows[i].X) - numericFloat(rows[i-1].X)
		if d > 0 && d < step {
			step = d
		}
	}
	return low - step, high + step, true
}

// growthKMSPoints memetakan pengukuran person (measured_at ASC) ke titik
// {measured_at, x, value, zscore, status} per indikator: z MENYALIN snapshot
// (tanpa hitung ulang), dinilai null di luar rentang ref ±1 langkah. flag_t
// hanya untuk bbu: titik i bertanda bila bb[i]-bb[i-1] ≤ 0. Baris tanpa nilai
// indikator (bb/tb/extras kosong) tidak menjadi titik.
func growthKMSPoints(indicator string, rows []sqlcgen.ListAnthropometryByPersonRow, low, high float64, hasRef bool) []gin.H {
	pts := []gin.H{}
	prevBB := 0.0
	for i, r := range rows {
		ageDays := 0.0
		hasAge := r.AgeDays.Valid
		if hasAge {
			ageDays = float64(r.AgeDays.Int32)
		}
		var x, value float64
		ok := false
		switch indicator {
		case "bbu":
			value, ok = numericFloat(r.BbKg), r.BbKg.Valid && hasAge
			x = ageDays
		case "tbu":
			value, ok = numericFloat(r.TbCm), r.TbCm.Valid && hasAge
			x = ageDays
		case "bbtb":
			value, x, ok = numericFloat(r.BbKg), numericFloat(r.TbCm), r.BbKg.Valid && r.TbCm.Valid
		case "imtu":
			value, ok = numericFloat(r.Imt), r.Imt.Valid && hasAge
			x = ageDays
		case "lku":
			v, hasV := giziExtrasFloat(r.Extras, "lk_cm")
			value, x, ok = v, ageDays, hasV && hasAge
		case "lila":
			v, hasV := giziExtrasFloat(r.Extras, "lila_cm")
			value, x, ok = v, ageDays, hasV && hasAge
		}
		if !ok {
			continue
		}
		z := pgtype.Numeric{}
		if hasRef && x >= low && x <= high {
			switch indicator {
			case "bbu":
				z = r.ZscoreBbu
			case "tbu":
				z = r.ZscoreTbu
			case "bbtb":
				z = r.ZscoreBbtb
			case "imtu":
				z = r.ZscoreImtu
			case "lku":
				z = r.ZscoreLku
			case "lila":
				z = r.ZscoreLila
			}
		}
		p := gin.H{
			"measured_at": tsJSON(r.MeasuredAt),
			"x":           x,
			"value":       value,
			"zscore":      numericOrNil(z),
			"status":      rawJSONOrNil(r.Status),
		}
		if indicator == "bbu" {
			p["flag_t"] = i > 0 && value-prevBB <= 0
			prevBB = value
		}
		pts = append(pts, p)
	}
	return pts
}

// bumilSeries membangun respons indikator bumil_bb: band ICM literal + titik
// berat (hanya baris ber-extras.mgg), delta vs anchor (bb pengukuran mgg
// terkecil), in_band = delta antara band.min·mgg/40 dan band.max·mgg/40
// (interpolasi linear, pola band diagonal KIA cetak 108).
func bumilSeries(band string, rows []sqlcgen.ListAnthropometryByPersonRow) gin.H {
	// Cache band literal (kontrak: key band; isi statis — cache memenuhi bentuk).
	key := "bumil_bb|" + band
	growthCacheMu.RLock()
	ent, hit := growthCache[key]
	growthCacheMu.RUnlock()
	if !hit || time.Since(ent.at) > growthCacheTTL {
		ent = growthCacheEntry{at: time.Now(), bands: bumilBands()}
		growthCacheMu.Lock()
		growthCache[key] = ent
		growthCacheMu.Unlock()
	}
	bands := ent.bands
	sel := bands[band].(gin.H)
	selMin, selMax := sel["min"].(float64), sel["max"].(float64)

	type mggRow struct {
		measuredAt pgtype.Timestamptz
		mgg, bb    float64
	}
	var mrows []mggRow
	anchor, haveAnchor := 0.0, false
	minMgg := 0.0
	for _, r := range rows {
		mgg, ok := giziExtrasFloat(r.Extras, "mgg")
		if !ok || !r.BbKg.Valid {
			continue
		}
		bb := numericFloat(r.BbKg)
		mrows = append(mrows, mggRow{r.MeasuredAt, mgg, bb})
		if !haveAnchor || mgg < minMgg {
			minMgg, anchor, haveAnchor = mgg, bb, true
		}
	}
	pts := make([]gin.H, 0, len(mrows))
	for _, m := range mrows {
		delta := m.bb - anchor
		lo := selMin * m.mgg / 40
		hi := selMax * m.mgg / 40
		pts = append(pts, gin.H{
			"measured_at": tsJSON(m.measuredAt),
			"mgg":         m.mgg,
			"bb_kg":       m.bb,
			"delta_kg":    delta,
			"in_band":     delta >= lo && delta <= hi,
		})
	}
	var anchorJSON any
	if haveAnchor {
		anchorJSON = anchor
	}
	return gin.H{
		"indicator":     "bumil_bb",
		"bands":         bands,
		"band_selected": band,
		"anchor_bb_kg":  anchorJSON,
		"points":        pts,
		"source":        gin.H{"version": "icm-2009", "url": "https://nap.nationalacademies.org/catalog/12584/"},
	}
}

// GetGrowthSeriesHandler godoc
// @Summary Growth series: WHO LMS curve + patient points, or bumil weight bands
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Param indicator query string true "bbu|tbu|bbtb|imtu|lku|lila|bumil_bb"
// @Param imt_pra_hamil query string false "kurus|normal|gemuk|obes (bumil_bb; default normal)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/growth-series [get]
func GetGrowthSeriesHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	personID, okID := parseUUID(c.Query("person_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	indicator := c.Query("indicator")
	if !validGrowthIndicators[indicator] {
		giziJSONHelperError(c, "indicator")
		return
	}
	band := c.Query("imt_pra_hamil")
	if band == "" {
		band = "normal"
	}
	if !validBumilBands[band] {
		giziJSONHelperError(c, "imt_pra_hamil")
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	person, err := q.GetPersonByID(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person tidak ditemukan"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	gender, okG := personGenderToGizi(person.Gender)
	if !okG {
		giziJSONHelperError(c, "gender")
		return
	}
	rows, err := q.ListAnthropometryByPerson(ctx, personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if indicator == "bumil_bb" {
		c.JSON(http.StatusOK, gin.H{"data": bumilSeries(band, rows), "meta": gin.H{}})
		return
	}
	curveRows, err := q.ListGrowthCurveByIndicatorGender(ctx, sqlcgen.ListGrowthCurveByIndicatorGenderParams{
		Indicator: indicator, Gender: gender,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	_, version, url := buildGrowthCurve(curveRows)
	cacheKey := indicator + "|" + gender + "|" + version
	var curve []gin.H
	growthCacheMu.RLock()
	ent, hit := growthCache[cacheKey]
	growthCacheMu.RUnlock()
	if hit && time.Since(ent.at) <= growthCacheTTL {
		curve = ent.curve
	} else {
		curve, version, url = buildGrowthCurve(curveRows)
		growthCacheMu.Lock()
		growthCache[cacheKey] = growthCacheEntry{at: time.Now(), curve: curve, version: version, url: url}
		growthCacheMu.Unlock()
	}
	low, high, hasRef := growthRefRange(curveRows)
	var source any
	if hasRef {
		source = gin.H{"version": version, "url": url}
	}
	data := gin.H{
		"indicator": indicator,
		"gender":    gender,
		"unit":      growthIndicatorUnits[indicator],
		"available": hasRef,
		"curve":     curve,
		"points":    growthKMSPoints(indicator, rows, low, high, hasRef),
		"source":    source,
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
