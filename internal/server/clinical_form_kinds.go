// internal/server/clinical_form_kinds.go
// CPPT SOAP v3 (spec 2026-11-10-cppt-pemeriksaan-v3 §Bentuk nilai): one
// validator per NEW section kind, dispatched from validateClinicalFormData.
// Old kinds keep their existing switch arms untouched.
package server

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

// clinicalInvalidKind writes the contract 400 for a v3 kind violation.
func clinicalInvalidKind(c *gin.Context, key string) bool {
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value for section '" + key + "'"})
	return false
}

// clinicalKindNumOpt reads an optional JSON number (null/absent → ok).
// Returns (value, filled, ok).
func clinicalKindNumOpt(m map[string]json.RawMessage, key string) (float64, bool, bool) {
	raw, present := m[key]
	if !present || string(raw) == "null" {
		return 0, false, true
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, false
	}
	v, err := n.Float64()
	if err != nil {
		return 0, false, false
	}
	return v, true, true
}

// clinicalKindIntOpt reads an optional JSON integer (null/absent → ok).
func clinicalKindIntOpt(m map[string]json.RawMessage, key string) (int64, bool, bool) {
	raw, present := m[key]
	if !present || string(raw) == "null" {
		return 0, false, true
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false, false
	}
	return v, true, true
}

// clinicalKindStrOpt reads an optional JSON string (null/absent → ok).
func clinicalKindStrOpt(m map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, present := m[key]
	if !present || string(raw) == "null" {
		return "", false, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, false
	}
	return s, true, true
}

// clinicalKindNumRange enforces lo < v < hi on a filled number value.
func clinicalKindNumRange(v, lo, hi float64) bool {
	return v > lo && v < hi
}

// clinicalKindStringEnum reports s ∈ values.
func clinicalKindStringEnum(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

// clinicalKindDateOpt validates an optional YYYY-MM-DD string (null/absent ok).
func clinicalKindDateOpt(m map[string]json.RawMessage, key string) (string, bool, bool) {
	s, filled, ok := clinicalKindStrOpt(m, key)
	if !filled || !ok {
		return s, filled, ok
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return s, true, false
	}
	return s, true, true
}

// ── radio_conditional (§Bentuk nilai #1, anamnesis_info) ──

// clinicalValidateRadioConditional: object {jenis,informan?,keandalan?};
// unknown keys → invalid; jenis must be one of the template options (empty
// options fall back to the spec enum); jenis=alloanamnesis requires
// informan.nama non-empty and informan.hubungan in enum (specific message).
func clinicalValidateRadioConditional(c *gin.Context, key string, raw json.RawMessage, d clinicalSectionDef) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	for k := range m {
		if k != "jenis" && k != "informan" && k != "keandalan" {
			return clinicalInvalidKind(c, key)
		}
	}
	jenis, filled, ok := clinicalKindStrOpt(m, "jenis")
	if !filled || !ok {
		return clinicalInvalidKind(c, key)
	}
	inOptions := len(d.Options) == 0 // fallback: spec enum below
	for _, o := range d.Options {
		if o.Value == jenis {
			inOptions = true
			break
		}
	}
	if !inOptions || !clinicalKindStringEnum(jenis, "autoanamnesis", "alloanamnesis") {
		return clinicalInvalidKind(c, key)
	}
	if jenis == "alloanamnesis" {
		rawInf, present := m["informan"]
		var inf map[string]json.RawMessage
		if !present || json.Unmarshal(rawInf, &inf) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "informan wajib diisi saat alloanamnesis"})
			return false
		}
		nama, nf, nok := clinicalKindStrOpt(inf, "nama")
		hubungan, hf, hok := clinicalKindStrOpt(inf, "hubungan")
		if !nf || !nok || nama == "" || !hf || !hok ||
			!clinicalKindStringEnum(hubungan, "orang_tua", "suami_istri", "anak", "saudara", "lainnya") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "informan wajib diisi saat alloanamnesis"})
			return false
		}
	}
	if keandalan, filled, ok := clinicalKindStrOpt(m, "keandalan"); filled &&
		(!ok || !clinicalKindStringEnum(keandalan, "andal", "kurang", "tidak_dapat_dinilai")) {
		return clinicalInvalidKind(c, key)
	}
	return true
}

// ── antropometri (§Bentuk nilai #11) ──

// clinicalValidateAntropometri: bb/tb/lk number 0<v<1000 or null; imt number
// or null; imt_kategori enum or null; var_tambahan array {variable,value}
// strings. Calc recompute is Task 2 — only shape/range here.
func clinicalValidateAntropometri(c *gin.Context, key string, raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	for _, f := range []string{"bb", "tb", "lk"} {
		if v, filled, ok := clinicalKindNumOpt(m, f); filled && (!ok || !clinicalKindNumRange(v, 0, 1000)) {
			return clinicalInvalidKind(c, key)
		}
	}
	if v, filled, ok := clinicalKindNumOpt(m, "imt"); filled && (!ok || v <= 0 || v >= 1000) {
		return clinicalInvalidKind(c, key)
	}
	if k, filled, ok := clinicalKindStrOpt(m, "imt_kategori"); filled &&
		(!ok || !clinicalKindStringEnum(k, "kurus", "normal", "bb_lebih", "obesitas")) {
		return clinicalInvalidKind(c, key)
	}
	if rawVar, present := m["var_tambahan"]; present && string(rawVar) != "null" {
		var vars []map[string]json.RawMessage
		if err := json.Unmarshal(rawVar, &vars); err != nil {
			return clinicalInvalidKind(c, key)
		}
		for _, v := range vars {
			for _, f := range []string{"variable", "value"} {
				if _, filled, ok := clinicalKindStrOpt(v, f); filled && !ok {
					return clinicalInvalidKind(c, key)
				}
			}
		}
	}
	return true
}

// ── physical_exam (§Bentuk nilai #12, pemeriksaan_fisik) ──

var clinicalPhysicalOrgans = []string{
	"kepala", "mata", "telinga", "hidung", "mulut", "leher", "thorax", "jantung",
	"paru", "abdomen", "genital_urogenital", "ekstremitas_atas", "ekstremitas_bawah",
	"kulit", "anus_rectum", "tulang_belakang", "saraf_pusat", "limfe",
}

var clinicalHexColorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// clinicalTimeRe matches the repeatable row field type `time` (HH:MM 24 jam).
var clinicalTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// clinicalValidatePhysicalExam: keadaan_umum enum|null; organ map limited to
// the 18 spec keys; per-organ status enum + optional keterangan + strokes
// (≤50 strokes/stroke ≤512 points, normalized coords, #RRGGBB, width 2..5,
// label "A"|"B"|null); genital_urogenital legacy flat strokes still
// rejected. The
// top-level key `pemeriksaan_lain` (FE-only free text) is intentionally
// ignored by this validator — the FE owns its shape (keputusan 2026-11-11).
func clinicalValidatePhysicalExam(c *gin.Context, key string, raw json.RawMessage) bool {
	var pe struct {
		KeadaanUmum json.RawMessage            `json:"keadaan_umum"`
		Organ       map[string]json.RawMessage `json:"organ"`
	}
	if err := json.Unmarshal(raw, &pe); err != nil {
		return clinicalInvalidKind(c, key)
	}
	if s, filled, ok := clinicalKindStrOpt(map[string]json.RawMessage{"keadaan_umum": pe.KeadaanUmum}, "keadaan_umum"); filled &&
		(!ok || !clinicalKindStringEnum(s, "baik", "ringan", "sedang", "buruk")) {
		return clinicalInvalidKind(c, key)
	}
	allowed := map[string]bool{}
	for _, o := range clinicalPhysicalOrgans {
		allowed[o] = true
	}
	for organ, rawOrg := range pe.Organ {
		if !allowed[organ] {
			return clinicalInvalidKind(c, key)
		}
		var om map[string]json.RawMessage
		if err := json.Unmarshal(rawOrg, &om); err != nil {
			return clinicalInvalidKind(c, key)
		}
		if status, filled, ok := clinicalKindStrOpt(om, "status"); filled &&
			(!ok || !clinicalKindStringEnum(status, "tidak_ada", "ada_kelainan")) {
			return clinicalInvalidKind(c, key)
		}
		if _, filled, ok := clinicalKindStrOpt(om, "keterangan"); filled && !ok {
			return clinicalInvalidKind(c, key)
		}
		// marks (baru, additif — spec 2026-11-20 §3.1 revisi 2026-11-21):
		// per-figureId strokes+pins; genital_urogenital tidak lagi diblokir
		// (keputusan user 2026-11-21) dan tunduk pada aturan yang sama dengan
		// organ lain (clinicalMarksOk). Legacy `strokes` flat di bawah TIDAK
		// diubah (union kompat). Batas pin total & keunikan nomor dinilai
		// per ORGAN lintas figur.
		if rawMarks, present := om["marks"]; present && string(rawMarks) != "null" {
			if err := clinicalMarksOk(rawMarks, organ); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return false
			}
		}
		if rawLegend, present := om["legend"]; present && string(rawLegend) != "null" {
			if err := clinicalLegendOk(rawLegend); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return false
			}
		}
		rawStrokes, present := om["strokes"]
		if !present || string(rawStrokes) == "null" {
			continue
		}
		if organ == "genital_urogenital" {
			return clinicalInvalidKind(c, key)
		}
		var strokes []json.RawMessage
		if err := json.Unmarshal(rawStrokes, &strokes); err != nil || len(strokes) > 50 {
			return clinicalInvalidKind(c, key)
		}
		for _, rawStroke := range strokes {
			if !clinicalStrokeOk(rawStroke) {
				return clinicalInvalidKind(c, key)
			}
		}
	}
	return true
}

// clinicalStrokeOk validates one normalized stroke (spec §Bentuk nilai #12).
func clinicalStrokeOk(raw json.RawMessage) bool {
	var st struct {
		Points []struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"points"`
		Color *string `json:"color"`
		Width *int    `json:"width"`
		Label *string `json:"label"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return false
	}
	if len(st.Points) == 0 || len(st.Points) > 512 {
		return false
	}
	for _, p := range st.Points {
		if p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
			return false
		}
	}
	if st.Color == nil || !clinicalHexColorRe.MatchString(*st.Color) {
		return false
	}
	if st.Width == nil || *st.Width < 2 || *st.Width > 5 {
		return false
	}
	if st.Label != nil && *st.Label != "A" && *st.Label != "B" {
		return false
	}
	return true
}

// clinicalFigureIDRe matches the figureId stem of marking assets (spec
// 2026-11-20 §2, contoh "pf-16", "pf-23-dorsum-kanan").
var clinicalFigureIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// clinicalMarksOk validates one organ's `marks` object (spec 2026-11-20
// §3.1): key figureId `^[a-z0-9][a-z0-9-]*$` ≤64 char; value object with
// optional `strokes` (via clinicalStrokeOk, ≤50/figure) and `pins` ({x,y,n}:
// x,y float 0..1; n int 1..9; ≤9 pins TOTAL per organ across figures; n
// unique per organ). Error messages are contract literals.
func clinicalMarksOk(raw json.RawMessage, organ string) error {
	invalid := errors.New("invalid marks for organ " + organ)
	var marks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &marks); err != nil {
		return invalid
	}
	pinTotal := 0
	pinNums := map[int]bool{}
	// Deterministic visit order (plan 2026-11-21 Task 4): sort figureIds so
	// that when ≥2 rules are violated the returned message does not depend on
	// Go's randomized map iteration. Per-figure rule order is unchanged
	// (pinTotal > 9 guard before duplicate, decision 2026-11-20).
	figIDs := make([]string, 0, len(marks))
	for figID := range marks {
		figIDs = append(figIDs, figID)
	}
	sort.Strings(figIDs)
	for _, figID := range figIDs {
		rawFig := marks[figID]
		if len(figID) > 64 || !clinicalFigureIDRe.MatchString(figID) {
			return errors.New("invalid figure id")
		}
		var fig map[string]json.RawMessage
		if err := json.Unmarshal(rawFig, &fig); err != nil {
			return invalid
		}
		if rawStrokes, present := fig["strokes"]; present && string(rawStrokes) != "null" {
			var strokes []json.RawMessage
			if err := json.Unmarshal(rawStrokes, &strokes); err != nil || len(strokes) > 50 {
				return invalid
			}
			for _, rawStroke := range strokes {
				if !clinicalStrokeOk(rawStroke) {
					return invalid
				}
			}
		}
		if rawPins, present := fig["pins"]; present && string(rawPins) != "null" {
			var pins []struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
				N *int    `json:"n"`
			}
			if err := json.Unmarshal(rawPins, &pins); err != nil {
				return invalid
			}
			for _, p := range pins {
				if p.N == nil || *p.N < 1 || *p.N > 9 ||
					p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
					return invalid
				}
				pinTotal++
				if pinTotal > 9 {
					return errors.New("too many pins")
				}
				if pinNums[*p.N] {
					return errors.New("duplicate pin number")
				}
				pinNums[*p.N] = true
			}
		}
	}
	return nil
}

// clinicalLegendOk validates one organ's organ-level `legend` object (spec
// 2026-11-20 §2): keys are string digits "1".."9", values strings ≤200
// characters. A pin without a legend entry is valid (soft). Key/value
// violations both use the contract literal `invalid legend key`.
func clinicalLegendOk(raw json.RawMessage) error {
	var legend map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legend); err != nil {
		return errors.New("invalid legend key")
	}
	for k, rawVal := range legend {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 || n > 9 {
			return errors.New("invalid legend key")
		}
		var s string
		if err := json.Unmarshal(rawVal, &s); err != nil || len([]rune(s)) > 200 {
			return errors.New("invalid legend key")
		}
	}
	return nil
}

// ── consciousness (§Bentuk nilai #13, kesadaran) ──

// clinicalValidateConsciousness: e/v/m string enum with uppercase "T" only
// ("t" rejected); any "T" forces gcs/kategori/gcs_p null; pupil mm 0<v<10,
// refleks enum, pupil_score 0..4, gcs_p int, respons ACVPU. Calc consistency
// (gcs/kategori/pupil_score/gcs_p recompute) is Task 2.
func clinicalValidateConsciousness(c *gin.Context, key string, raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	hasT := false
	// Per-field ranges (spec §Bentuk nilai #13): e "1".."4", v "1".."5",
	// m "1".."6"; the "T" marker is uppercase-only ("t" = v2, rejected).
	components := []struct {
		field string
		max   string
	}{
		{"e", "4"}, {"v", "5"}, {"m", "6"},
	}
	for _, comp := range components {
		s, filled, ok := clinicalKindStrOpt(m, comp.field)
		if !filled {
			continue
		}
		if !ok {
			return clinicalInvalidKind(c, key)
		}
		switch s {
		case "":
			// "" (legacy FE clear combobox) = kosong, keputusan user 2026-11-21.
			continue
		case "T":
			hasT = true
		case "1", "2", "3", "4", "5", "6":
			if s > comp.max {
				return clinicalInvalidKind(c, key)
			}
		default:
			return clinicalInvalidKind(c, key)
		}
	}
	// A filled gcs with any "T" input violates the T rule; otherwise gcs is
	// a number whose consistency is recomputed in Task 2.
	if _, filled, ok := clinicalKindNumOpt(m, "gcs"); filled && (!ok || hasT) {
		return clinicalInvalidKind(c, key)
	}
	if hasT {
		// gcs handled above; kategori and gcs_p must be null.
		for _, f := range []string{"kategori", "gcs_p"} {
			if raw, present := m[f]; present && string(raw) != "null" {
				return clinicalInvalidKind(c, key)
			}
		}
	}
	// "" (legacy FE clear combobox) = kosong untuk field opsional, keputusan
	// user 2026-11-21 — hanya di validator kesadaran ini.
	if k, filled, ok := clinicalKindStrOpt(m, "kategori"); filled &&
		(!ok || (k != "" && !clinicalKindStringEnum(k, "compos_mentis", "somnolen", "sopor", "koma"))) {
		return clinicalInvalidKind(c, key)
	}
	if rawPupil, present := m["pupil"]; present && string(rawPupil) != "null" {
		var p map[string]json.RawMessage
		if err := json.Unmarshal(rawPupil, &p); err != nil {
			return clinicalInvalidKind(c, key)
		}
		for _, f := range []string{"kiri_mm", "kanan_mm"} {
			// pupil mm is a JSON number (spec §Bentuk nilai #13); a quoted
			// string like "2.5" would slip through clinicalKindNumOpt
			// (json.Number accepts numeric strings) — reject it here.
			if raw, present := p[f]; present && string(raw) != "null" && len(raw) > 0 && raw[0] == '"' {
				return clinicalInvalidKind(c, key)
			}
			if v, filled, ok := clinicalKindNumOpt(p, f); filled && (!ok || v <= 0 || v >= 10) {
				return clinicalInvalidKind(c, key)
			}
		}
		for _, f := range []string{"refleks_kiri", "refleks_kanan"} {
			if s, filled, ok := clinicalKindStrOpt(p, f); filled &&
				(!ok || (s != "" && !clinicalKindStringEnum(s, "cepat", "lambat", "tidak_ada"))) {
				return clinicalInvalidKind(c, key)
			}
		}
	}
	if v, filled, ok := clinicalKindNumOpt(m, "pupil_score"); filled && (!ok || v < 0 || v > 4) {
		return clinicalInvalidKind(c, key)
	}
	if v, filled, ok := clinicalKindIntOpt(m, "gcs_p"); filled && (!ok || v < 0) {
		return clinicalInvalidKind(c, key)
	}
	if r, filled, ok := clinicalKindStrOpt(m, "respons"); filled &&
		(!ok || (r != "" && !clinicalKindStringEnum(r, "a", "c", "v", "p", "u"))) {
		return clinicalInvalidKind(c, key)
	}
	return true
}

// ── rtl (§Bentuk nilai #21, rencana_tindak_lanjut) ──

// clinicalValidateRTL: moda enum|null; target_tanggal YYYY-MM-DD|null;
// kriteria_rujukan_darurat string.
func clinicalValidateRTL(c *gin.Context, key string, raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	if moda, filled, ok := clinicalKindStrOpt(m, "moda"); filled &&
		(!ok || !clinicalKindStringEnum(moda, "kontrol_poliklinik", "panggil_pasien", "rujuk_fasilitas_lain", "rawat_inap_terencana", "lainnya")) {
		return clinicalInvalidKind(c, key)
	}
	if _, filled, ok := clinicalKindDateOpt(m, "target_tanggal"); filled && !ok {
		return clinicalInvalidKind(c, key)
	}
	if s, filled, ok := clinicalKindStrOpt(m, "kriteria_rujukan_darurat"); filled && !ok {
		return clinicalInvalidKind(c, key)
	} else if filled {
		_ = s
	}
	return true
}

// ── rekomendasi (§Bentuk nilai #22) ──

// clinicalValidateRekomendasi: teks string; tanggal_kontrol YYYY-MM-DD|null.
func clinicalValidateRekomendasi(c *gin.Context, key string, raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	if _, filled, ok := clinicalKindStrOpt(m, "teks"); filled && !ok {
		return clinicalInvalidKind(c, key)
	}
	if _, filled, ok := clinicalKindDateOpt(m, "tanggal_kontrol"); filled && !ok {
		return clinicalInvalidKind(c, key)
	}
	return true
}

// ── vitals STRICT v3 (§Bentuk nilai #10, section key tanda_vital only) ──

var clinicalVitalsV3Keys = []string{
	"td_sistolik", "td_diastolik", "nafas", "nadi", "suhu", "spo2", "spo2_scale",
	"oksigen", "waktu_asesmen", "kriteria", "news2_total",
}

// clinicalValidateVitalsV3: strict v3 validation for the tanda_vital section
// key; spo2 0..100 inclusive, other numbers 0<v<1000; spo2_scale {1,2};
// oksigen/kriteria enums; waktu_asesmen RFC3339; news2_total int 0..20|null.
// news2 recompute is Task 2. Keys OUTSIDE the v3 whitelist keep the legacy
// numeric path (0<v<1000, pain_score 0–10) — the v2 template (migration
// 000089:58) also uses key tanda_vital and legacy behavior must not change.
func clinicalValidateVitalsV3(c *gin.Context, key string, raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return clinicalInvalidKind(c, key)
	}
	allowed := map[string]bool{}
	for _, k := range clinicalVitalsV3Keys {
		allowed[k] = true
	}
	for _, f := range []string{"td_sistolik", "td_diastolik", "nafas", "nadi", "suhu"} {
		if v, filled, ok := clinicalKindNumOpt(m, f); filled && (!ok || !clinicalKindNumRange(v, 0, 1000)) {
			return clinicalInvalidKind(c, key)
		}
	}
	if v, filled, ok := clinicalKindNumOpt(m, "spo2"); filled && (!ok || v < 0 || v > 100) {
		return clinicalInvalidKind(c, key)
	}
	if v, filled, ok := clinicalKindNumOpt(m, "spo2_scale"); filled &&
		(!ok || !(v == 1 || v == 2)) {
		return clinicalInvalidKind(c, key)
	}
	if s, filled, ok := clinicalKindStrOpt(m, "oksigen"); filled &&
		(!ok || !clinicalKindStringEnum(s, "udara_ruang", "tambahan")) {
		return clinicalInvalidKind(c, key)
	}
	if s, filled, ok := clinicalKindStrOpt(m, "kriteria"); filled &&
		(!ok || !clinicalKindStringEnum(s, "rutin", "checkup", "perintah_dpjp", "pasca_tindakan", "reassessment_ews")) {
		return clinicalInvalidKind(c, key)
	}
	if s, filled, ok := clinicalKindStrOpt(m, "waktu_asesmen"); filled &&
		(!ok || func() bool { _, err := time.Parse(time.RFC3339, s); return err != nil }()) {
		return clinicalInvalidKind(c, key)
	}
	if v, filled, ok := clinicalKindIntOpt(m, "news2_total"); filled && (!ok || v < 0 || v > 20) {
		return clinicalInvalidKind(c, key)
	}
	// Legacy keys outside the v3 whitelist (pain_score, sistolic, …): keep
	// the legacy numeric range check (0<v<1000; pain_score 0–10 inclusive).
	for k, rawK := range m {
		if allowed[k] {
			continue
		}
		var v float64
		if err := json.Unmarshal(rawK, &v); err != nil {
			return clinicalInvalidKind(c, key)
		}
		if k == "pain_score" {
			if v < 0 || v > 10 {
				return clinicalInvalidKind(c, key)
			}
			continue
		}
		if v <= 0 || v >= 1000 {
			return clinicalInvalidKind(c, key)
		}
	}
	return true
}

// clinicalValidateKindV3 dispatches one v3 section value to its kind
// validator (called from the validateClinicalFormData switch).
func clinicalValidateKindV3(c *gin.Context, d clinicalSectionDef, raw json.RawMessage) bool {
	switch d.Kind {
	case "radio_conditional":
		return clinicalValidateRadioConditional(c, d.Key, raw, d)
	case "antropometri":
		return clinicalValidateAntropometri(c, d.Key, raw)
	case "physical_exam":
		return clinicalValidatePhysicalExam(c, d.Key, raw)
	case "consciousness":
		return clinicalValidateConsciousness(c, d.Key, raw)
	case "rtl":
		return clinicalValidateRTL(c, d.Key, raw)
	case "rekomendasi":
		return clinicalValidateRekomendasi(c, d.Key, raw)
	}
	return clinicalInvalidKind(c, d.Key)
}

// ── Calc consistency v3 (spec 2026-11-10-cppt-pemeriksaan-v3 §Validasi
// server, §NEWS2) ──
//
// clinicalValidateCalcV3 is called from validateClinicalFormData AFTER the
// per-kind shape validation: user-supplied calc values must equal the
// recompute (mismatch → 400 "kalkulasi <field> tidak konsisten", pattern
// clinical_form.go:349); partial inputs → the calc field must be null (null
// is always accepted — the server only rejects mismatch; with complete inputs
// a null value is FE's responsibility, spec: "server hanya menolak mismatch").

// clinicalImtKategori: <18.5 kurus | 18.5–22.9 normal | 23–24.9 bb_lebih |
// ≥25 obesitas (batas atas tiap rentang tertutup, spec §Bentuk nilai #11 /
// KMK 509/2025 via ledger).
func clinicalImtKategori(imt float64) string {
	switch {
	case imt < 18.5:
		return "kurus"
	case imt < 23:
		return "normal"
	case imt < 25:
		return "bb_lebih"
	default:
		return "obesitas"
	}
}

// clinicalGcsKategori: 15 compos_mentis | 13–14 somnolen | 9–12 sopor |
// ≤8 koma (spec §Bentuk nilai #13).
func clinicalGcsKategori(gcs int64) string {
	switch {
	case gcs >= 15:
		return "compos_mentis"
	case gcs >= 13:
		return "somnolen"
	case gcs >= 9:
		return "sopor"
	default:
		return "koma"
	}
}

// NEWS2 weights (spec §NEWS2 — satu-satunya sumber: ledger riset
// 2026-11-06-cppt-pemeriksaan-riset.md §dataset line 29; server recompute,
// TIDAK dikarang ulang).
//
// RR: ≤8:+3 · 9–11:+1 · 12–20:0 · 21–24:+2 · ≥25:+3
func clinicalNEWS2RespRate(v float64) int {
	switch {
	case v <= 8:
		return 3
	case v <= 11:
		return 1
	case v <= 20:
		return 0
	case v <= 24:
		return 2
	default:
		return 3
	}
}

// SpO₂ skala 1: ≥96:0 · 94–95:+1 · 92–93:+2 · ≤91:+3
// SpO₂ skala 2: ≥97:0 · 94–96:+1 · 92–93:+2 · ≤91:+3
func clinicalNEWS2SpO2(v float64, scale2 bool) int {
	if scale2 {
		switch {
		case v >= 97:
			return 0
		case v >= 94:
			return 1
		case v >= 92:
			return 2
		default:
			return 3
		}
	}
	switch {
	case v >= 96:
		return 0
	case v >= 94:
		return 1
	case v >= 92:
		return 2
	default:
		return 3
	}
}

// O₂: udara_ruang:0 · tambahan:+2
func clinicalNEWS2SupplementalO2(mode string) int {
	if mode == "tambahan" {
		return 2
	}
	return 0
}

// SBP: ≤90:+3 · 91–100:+2 · 101–199:0 · ≥200:+3
func clinicalNEWS2SBP(v float64) int {
	switch {
	case v <= 90:
		return 3
	case v <= 100:
		return 2
	case v <= 199:
		return 0
	default:
		return 3
	}
}

// Nadi: ≤40:+3 · 41–50:+1 · 51–90:0 · 91–110:+1 · 111–130:+2 · ≥131:+3
func clinicalNEWS2HeartRate(v float64) int {
	switch {
	case v <= 40:
		return 3
	case v <= 50:
		return 1
	case v <= 90:
		return 0
	case v <= 110:
		return 1
	case v <= 130:
		return 2
	default:
		return 3
	}
}

// Suhu: ≤35.0:+3 · 35.1–36.0:+1 · 36.1–38.0:0 · 38.1–39.0:+1 · ≥39.1:+3
func clinicalNEWS2Temp(v float64) int {
	switch {
	case v <= 35.0:
		return 3
	case v <= 36.0:
		return 1
	case v <= 38.0:
		return 0
	case v <= 39.0:
		return 1
	default:
		return 3
	}
}

// clinicalNEWS2Total recomputes the total from the 7 required inputs
// (RR, SpO₂+scale, O₂, SBP, nadi, suhu) + ACVPU kesadaran.respons
// (a=0, c|v|p|u=+3). Verified example (ledger): RR22=2, nadi96=1,
// suhu38.2=1, SpO₂ 95 sk1=1, TD118=0, Alert=0 → 5.
func clinicalNEWS2Total(rr, spo2, sbp, hr, temp float64, spo2Scale float64, oksigen, respons string) int {
	total := clinicalNEWS2RespRate(rr) +
		clinicalNEWS2SpO2(spo2, spo2Scale == 2) +
		clinicalNEWS2SupplementalO2(oksigen) +
		clinicalNEWS2SBP(sbp) +
		clinicalNEWS2HeartRate(hr) +
		clinicalNEWS2Temp(temp)
	if respons != "" && respons != "a" {
		total += 3
	}
	return total
}

// clinicalSectionMap parses one section value as a JSON object (nil bila
// absen/kosong/tak terparse — calc pass runs after shape validation, so a
// parse failure here cannot happen for validated kinds).
func clinicalSectionMap(sections map[string]json.RawMessage, key string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if raw, ok := sections[key]; ok && !clinicalSectionEmpty(raw) {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// clinicalAgeMonths: floor(age_days/30.4375) dari DOB person ke hari ini —
// basis persis sama dengan trigger fn_anthropometry_snapshot (plan
// 2026-11-18-anak-a1-1-be Task 2). DOB kosong → (0, false): gate
// imt_kategori di-skip aman (bukan error).
func clinicalAgeMonths(birth pgtype.Date) (int, bool) {
	if !birth.Valid {
		return 0, false
	}
	days := int(time.Since(birth.Time).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return int(math.Floor(float64(days) / 30.4375)), true
}

func clinicalValidateCalcV3(c *gin.Context, sections map[string]json.RawMessage, birthDate pgtype.Date) bool {
	mismatch := func(field string) bool {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kalkulasi " + field + " tidak konsisten"})
		return false
	}

	// antropometri: imt = round(bb/(tb/100)², 1) bila bb & tb ada;
	// imt_kategori dari imt recompute — check di-SKIP bila usia <= 228 bulan
	// (<19 th: trigger menulis imt_kategori NULL, D-KA-04; plan
	// 2026-11-18-anak-a1-1-be Task 2). Gate menyala HANYA bila DOB tersedia.
	if m := clinicalSectionMap(sections, "antropometri"); m != nil {
		bb, bbFilled, _ := clinicalKindNumOpt(m, "bb")
		tb, tbFilled, _ := clinicalKindNumOpt(m, "tb")
		complete := bbFilled && tbFilled
		expected := math.Round(bb/math.Pow(tb/100, 2)*10) / 10
		if imt, filled, _ := clinicalKindNumOpt(m, "imt"); filled &&
			(!complete || math.Abs(imt-expected) > 1e-9) {
			return mismatch("imt")
		}
		months, hasAge := clinicalAgeMonths(birthDate)
		if kat, filled, _ := clinicalKindStrOpt(m, "imt_kategori"); filled && hasAge &&
			months > 228 && (!complete || kat != clinicalImtKategori(expected)) {
			return mismatch("imt_kategori")
		}
	}

	// kesadaran: gcs = e+v+m (bila ketiganya numerik); kategori dari gcs;
	// pupil_score = skor refleks (cepat=0, lambat=1, tidak_ada=2) kiri+kanan
	// bila keduanya ada; gcs_p = gcs − pupil_score bila gcs ada.
	if m := clinicalSectionMap(sections, "kesadaran"); m != nil {
		numComp := func(field string) (int64, bool) {
			s, filled, ok := clinicalKindStrOpt(m, field)
			if !filled || !ok {
				return 0, false
			}
			n, err := strconv.Atoi(s)
			return int64(n), err == nil
		}
		e, eNum := numComp("e")
		v, vNum := numComp("v")
		mv, mNum := numComp("m")
		gcsComplete := eNum && vNum && mNum
		gcs := e + v + mv
		if gcsVal, filled, _ := clinicalKindNumOpt(m, "gcs"); filled &&
			(!gcsComplete || int64(gcsVal) != gcs) {
			return mismatch("gcs")
		}
		if kat, filled, _ := clinicalKindStrOpt(m, "kategori"); filled && kat != "" &&
			(!gcsComplete || kat != clinicalGcsKategori(gcs)) {
			return mismatch("kategori")
		}
		reflScore := func(s string) int64 {
			switch s {
			case "lambat":
				return 1
			case "tidak_ada":
				return 2
			}
			return 0 // "cepat"
		}
		expectedPS, psComplete := int64(0), false
		if p := clinicalSectionMap(map[string]json.RawMessage{"kesadaran": m["pupil"]}, "kesadaran"); p != nil {
			rl, rlFilled, _ := clinicalKindStrOpt(p, "refleks_kiri")
			rr, rrFilled, _ := clinicalKindStrOpt(p, "refleks_kanan")
			psComplete = rlFilled && rl != "" && rrFilled && rr != ""
			if psComplete {
				expectedPS = reflScore(rl) + reflScore(rr)
			}
		}
		if ps, filled, _ := clinicalKindNumOpt(m, "pupil_score"); filled &&
			(!psComplete || int64(ps) != expectedPS) {
			return mismatch("pupil_score")
		}
		// gcs_p = gcs − pupil_score, clamp minimum 1 — GCS-P min 1
		// (Teasdale 2014); keputusan user 2026-11-21.
		if gcsP, filled, _ := clinicalKindIntOpt(m, "gcs_p"); filled &&
			(!gcsComplete || !psComplete || gcsP != max(gcs-expectedPS, 1)) {
			return mismatch("gcs_p")
		}
	}

	// tanda_vital: news2_total dihitung HANYA bila SEMUA 7 input ada
	// (nafas, spo2, spo2_scale, oksigen, td_sistolik, nadi, suhu) +
	// kesadaran.respons dari section lain pada raw form_data.
	if m := clinicalSectionMap(sections, "tanda_vital"); m != nil {
		news2, news2Filled, _ := clinicalKindIntOpt(m, "news2_total")
		if news2Filled {
			num := func(field string) (float64, bool) {
				v, filled, ok := clinicalKindNumOpt(m, field)
				return v, filled && ok
			}
			rr, rrOK := num("nafas")
			spo2, spo2OK := num("spo2")
			scale, scaleOK := num("spo2_scale")
			sbp, sbpOK := num("td_sistolik")
			hr, hrOK := num("nadi")
			temp, tempOK := num("suhu")
			oksigen, oksFilled, oksOK := clinicalKindStrOpt(m, "oksigen")
			respons, respFilled, _ := clinicalKindStrOpt(clinicalSectionMap(sections, "kesadaran"), "respons")
			scaleValid := scaleOK && (scale == 1 || scale == 2)
			complete := rrOK && spo2OK && scaleValid && oksFilled && oksOK && sbpOK && hrOK && tempOK && respFilled
			if !complete || news2 != int64(clinicalNEWS2Total(rr, spo2, sbp, hr, temp, scale, oksigen, respons)) {
				return mismatch("news2_total")
			}
		}
	}
	return true
}
