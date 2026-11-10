// internal/server/clinical_form_kinds.go
// CPPT SOAP v3 (spec 2026-11-10-cppt-pemeriksaan-v3 §Bentuk nilai): one
// validator per NEW section kind, dispatched from validateClinicalFormData.
// Old kinds keep their existing switch arms untouched.
package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
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
// label "A"|"B"|null); genital_urogenital may not carry strokes.
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
	if k, filled, ok := clinicalKindStrOpt(m, "kategori"); filled &&
		(!ok || !clinicalKindStringEnum(k, "compos_mentis", "somnolen", "sopor", "koma")) {
		return clinicalInvalidKind(c, key)
	}
	if rawPupil, present := m["pupil"]; present && string(rawPupil) != "null" {
		var p map[string]json.RawMessage
		if err := json.Unmarshal(rawPupil, &p); err != nil {
			return clinicalInvalidKind(c, key)
		}
		for _, f := range []string{"kiri_mm", "kanan_mm"} {
			if v, filled, ok := clinicalKindNumOpt(p, f); filled && (!ok || v <= 0 || v >= 10) {
				return clinicalInvalidKind(c, key)
			}
		}
		for _, f := range []string{"refleks_kiri", "refleks_kanan"} {
			if s, filled, ok := clinicalKindStrOpt(p, f); filled &&
				(!ok || !clinicalKindStringEnum(s, "cepat", "lambat", "tidak_ada")) {
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
		(!ok || !clinicalKindStringEnum(r, "a", "c", "v", "p", "u")) {
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
