// internal/server/clinical_form.go
// CPPT Fase 1 (spec 2026-10-21-cppt-rajal-fase1-design §5, §9): clinical form
// template resolution and draft entry CRUD with soft validation. Sign/lock/
// addendum/counter-sign live in clinical_form_sign.go (plan A Task 3).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Clinical module permissions (migration 000089; pattern queue.go).
const (
	PermClinicalRecordView        = "clinical.record.view"
	PermClinicalRecordWrite       = "clinical.record.write"
	PermClinicalRecordCountersign = "clinical.record.countersign"
	PermClinicalNutritionWrite    = "clinical.nutrition.write"
)

func RegisterClinicalRoutes(rg *gin.RouterGroup) {
	rg.GET("/clinical/form-templates", ListClinicalFormTemplatesHandler)
	rg.POST("/clinical/forms", CreateClinicalFormHandler)
	rg.GET("/clinical/forms", AccessLog("clinical_form", "list", ""), ListClinicalFormsHandler)
	rg.GET("/clinical/forms/:id", AccessLog("clinical_form", "view", "id"), GetClinicalFormHandler)
	rg.PATCH("/clinical/forms/:id", UpdateClinicalFormHandler)
	rg.DELETE("/clinical/forms/:id", DeleteClinicalFormHandler)
	// Sign/counter-sign/addendum (plan A Task 3, spec §9:184-186).
	RegisterClinicalSignRoutes(rg)
	// Diet catalog + nutrition orders (plan B Task 1).
	RegisterClinicalNutritionRoutes(rg)
	// Composed timeline per admission (plan B Task 2). Registered here because
	// server.go is outside the task's Files list; gin allows the static child
	// /admissions/:id/clinical-timeline next to /admissions/:id.
	rg.GET("/admissions/:id/clinical-timeline", AccessLog("clinical_form", "view", "id"), GetClinicalTimelineHandler)
	// Board sheet preview (fase 2a plan Task 2, spec §API-2) — small read
	// model per admission, same guard shape as the timeline.
	rg.GET("/admissions/:id/clinical-preview", AccessLog("clinical_form", "view", "id"), GetClinicalBoardPreviewHandler)
	// Daily completeness board (plan B Task 3, spec §11) — pure read.
	rg.GET("/clinical/completeness", AccessLog("clinical_form", "view", ""), GetClinicalCompletenessHandler)
	// Gizi: antropometri + episode + guard profil (plan 2026-11-13-gizi chunk 1 Task 3).
	RegisterGiziCareRoutes(rg)
	// Gizi: series growth KMS + band bumil (plan 2026-11-13-gizi chunk 3 Task 1).
	RegisterGrowthSeriesRoutes(rg)
}

type clinicalSectionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// clinicalRepeatableFieldDef is one column of a repeatable section (spec
// 2026-11-04-cppt-2a-soap-editor-design §4: alergi rows).
type clinicalRepeatableFieldDef struct {
	Key     string                  `json:"key"`
	Label   string                  `json:"label"`
	Type    string                  `json:"type"`
	Options []clinicalSectionOption `json:"options"`
}

// clinicalCalcDef is the declarative recompute metadata of a calc section
// (spec §1 A2, pattern MST): formula "gcs" reads select section keys,
// "imt" reads "<section>.<field>" paths (objective.weight_kg/height_cm).
type clinicalCalcDef struct {
	Formula string   `json:"formula"`
	Inputs  []string `json:"inputs"`
}

// clinicalSectionDef mirrors one entry of core.template_clinical_form.sections
// (spec §5: {"sections":[{"key","label","kind","soft_required"?,"options"?}]}).
type clinicalSectionDef struct {
	Key          string                       `json:"key"`
	Label        string                       `json:"label"`
	Kind         string                       `json:"kind"`
	SoftRequired bool                         `json:"soft_required"`
	Options      []clinicalSectionOption      `json:"options"`
	Fields       []clinicalRepeatableFieldDef `json:"fields"`
	Calc         *clinicalCalcDef             `json:"calc"`
}

type clinicalSectionsDef struct {
	Sections []clinicalSectionDef `json:"sections"`
}

// clinicalSectionKinds are the kinds the server validates in v1 (spec §5);
// any other kind in template data is rejected instead of silently ignored.
var clinicalSectionKinds = map[string]bool{
	"textarea": true, "text": true, "number": true, "select": true,
	"vitals": true, "diagnosis": true, "date": true,
	"repeatable": true, "calc": true,
	// SOAP v3 kinds (spec 2026-11-10-cppt-pemeriksaan-v3 §Kontrak data).
	"radio_conditional": true, "antropometri": true, "physical_exam": true,
	"consciousness": true, "rtl": true, "rekomendasi": true,
}

// resolveClinicalFormTemplate picks the active clinical_form template for
// formType with scope priority merchant > company > platform (spec §9:178).
func resolveClinicalFormTemplate(ctx context.Context, q *sqlcgen.Queries, companyID, merchantID pgtype.UUID, formType string) (sqlcgen.ListClinicalFormTemplatesRow, bool, error) {
	rows, err := q.ListClinicalFormTemplates(ctx, sqlcgen.ListClinicalFormTemplatesParams{CompanyID: companyID, MerchantID: merchantID})
	if err != nil {
		return sqlcgen.ListClinicalFormTemplatesRow{}, false, err
	}
	scopeRank := map[string]int{"platform": 1, "company": 2, "merchant": 3}
	var best sqlcgen.ListClinicalFormTemplatesRow
	found := false
	for _, r := range rows {
		if r.FormType != formType {
			continue
		}
		if !found || scopeRank[r.Scope] > scopeRank[best.Scope] {
			best, found = r, true
		}
	}
	return best, found, nil
}

// clinicalWritePerm maps a form category to its write permission (spec §9:179):
// gizi forms — and the gizi-only assessment skrining — need
// clinical.nutrition.write, everything else clinical.record.write.
func clinicalWritePerm(formType, category string) string {
	if category == "gizi" || formType == "ases_gizi_skrining" {
		return PermClinicalNutritionWrite
	}
	return PermClinicalRecordWrite
}

func clinicalIsGiziForm(formType, category string) bool {
	return category == "gizi" || formType == "ases_gizi_skrining"
}

// clinicalUserHasPerm reports whether the caller's active role holds code at
// merchantID — used for read-path filtering where a 403 is not wanted.
func clinicalUserHasPerm(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, code string) bool {
	roleID, err := effectiveRoleID(c, q, AuthUserID(c), merchantID)
	if err != nil || !roleID.Valid {
		return false
	}
	has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: code, RoleID: roleID,
	})
	return err == nil && has
}

func clinicalInvalidSection(c *gin.Context, key string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value for section '" + key + "'"})
}

// clinicalSectionEmpty reports a JSON value as "kosong" (null, empty string,
// empty object/array) — empty sections never block saving (user decision 7).
func clinicalSectionEmpty(raw json.RawMessage) bool {
	s := string(raw)
	return s == "" || s == "null" || s == `""` || s == "{}" || s == "[]"
}

// validateClinicalFormData enforces spec §5.2 against the pinned template
// sections: exact key set, per-kind value types, physiological vitals range,
// MST score consistency. Soft warnings (empty sections) are returned without
// failing; validation failures write the 400 and return false.
func validateClinicalFormData(c *gin.Context, formType string, defs clinicalSectionsDef, raw json.RawMessage) ([]string, bool) {
	warnings := []string{}
	byKey := make(map[string]clinicalSectionDef, len(defs.Sections))
	for _, d := range defs.Sections {
		if !clinicalSectionKinds[d.Kind] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported section kind: " + d.Kind})
			return nil, false
		}
		// A calc section without recompute metadata is a malformed template —
		// reject the shape instead of silently skipping validation (spec §6.3).
		if d.Kind == "calc" && (d.Calc == nil || d.Calc.Formula == "" || len(d.Calc.Inputs) == 0) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported section kind: calc"})
			return nil, false
		}
		byKey[d.Key] = d
	}
	var payload struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid form_data"})
			return nil, false
		}
	}
	for key := range payload.Sections {
		if _, ok := byKey[key]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown section key: " + key})
			return nil, false
		}
	}
	mst := map[string]int{}
	// MNA-SF: skor per pertanyaan diambil dari suffix "<huruf>_<skor>" nilai
	// select (spec §5 hook, template migration 000098).
	mnasf := map[string]int{}
	for _, d := range defs.Sections {
		rawVal, present := payload.Sections[d.Key]
		if !present || clinicalSectionEmpty(rawVal) {
			warnings = append(warnings, "section '"+d.Key+"' kosong")
			continue
		}
		switch d.Kind {
		case "textarea", "text", "select":
			var s string
			if err := json.Unmarshal(rawVal, &s); err != nil || s == "" {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
			// MST question answers carry their score in the select value.
			if n, err := strconv.Atoi(s); err == nil {
				mst[d.Key] = n
			}
			// MNA-SF question answers carry their score as the select value
			// suffix "<huruf>_<skor>" (mnasf_a.."f_3", spec §5 hook).
			if i := strings.IndexByte(s, '_'); i > 0 {
				if n, err := strconv.Atoi(s[i+1:]); err == nil {
					mnasf[s[:i]] = n
				}
			}
		case "number":
			var n json.Number
			if err := json.Unmarshal(rawVal, &n); err != nil {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
			if d.Key == "skor_mst" || d.Key == "skor_mnasf" {
				if v, err := n.Float64(); err == nil {
					mst[d.Key] = int(v)
				}
			}
		case "date":
			var s string
			if err := json.Unmarshal(rawVal, &s); err != nil {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
			if _, err := time.Parse("2006-01-02", s); err != nil {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
		case "vitals":
			// STRICT v3 whitelist only for the tanda_vital section key (spec
			// 2026-11-10-cppt-pemeriksaan-v3 §Bentuk nilai #10); other vitals
			// sections (e.g. objective) keep the legacy path unchanged.
			if d.Key == "tanda_vital" {
				if !clinicalValidateVitalsV3(c, d.Key, rawVal) {
					return nil, false
				}
				continue
			}
			var vitals map[string]float64
			if err := json.Unmarshal(rawVal, &vitals); err != nil {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
			for k, v := range vitals {
				if k == "pain_score" {
					// NRS 0–10 inclusive (0 = no pain; review A4).
					if v < 0 || v > 10 {
						clinicalInvalidSection(c, d.Key)
						return nil, false
					}
					continue
				}
				if v <= 0 || v >= 1000 {
					clinicalInvalidSection(c, d.Key)
					return nil, false
				}
			}
		case "repeatable":
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal(rawVal, &rows); err != nil {
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
			fieldDefs := make(map[string]clinicalRepeatableFieldDef, len(d.Fields))
			for _, f := range d.Fields {
				fieldDefs[f.Key] = f
			}
			for _, row := range rows {
				for fk, fv := range row {
					fd, known := fieldDefs[fk]
					if !known {
						c.JSON(http.StatusBadRequest, gin.H{"error": "unknown field '" + fk + "' di section '" + d.Key + "'"})
						return nil, false
					}
					if !clinicalRepeatableFieldOk(fv, fd) {
						clinicalInvalidSection(c, d.Key)
						return nil, false
					}
				}
			}
		case "diagnosis":
			var dx struct {
				Narrative *string           `json:"narrative"`
				Diagnoses []json.RawMessage `json:"diagnoses"`
			}
			if err := json.Unmarshal(rawVal, &dx); err != nil || dx.Narrative == nil {
				// narrative wajib; diagnoses hilang = kosong (soft_required,
				// warning FE — review A3). Item shape/suspect divalidasi saat
				// sign (persistSignDiagnoses — remedial 6536088, semantik
				// fase 1: create/update menerima diagnoses bentuk apa pun).
				clinicalInvalidSection(c, d.Key)
				return nil, false
			}
		case "radio_conditional", "antropometri", "physical_exam", "consciousness", "rtl", "rekomendasi":
			// SOAP v3 kinds (clinical_form_kinds.go) — dispatch per kind.
			// Other kinds (calc) keep their legacy fall-through below.
			if !clinicalValidateKindV3(c, d, rawVal) {
				return nil, false
			}
		}
	}
	// MST consistency (spec §5.1/§5.2): skor_mst must equal the sum of the
	// three question scores and risiko_gizi must follow skor >= 2.
	if formType == "ases_gizi_skrining" {
		if skor, ok := mst["skor_mst"]; ok {
			var risiko string
			if r, okR := payload.Sections["risiko_gizi"]; okR && !clinicalSectionEmpty(r) {
				_ = json.Unmarshal(r, &risiko)
			}
			sum := mst["mst_penurunan_bb"] + mst["mst_nafsu_makan"] + mst["mst_sakit_akut"]
			want := "tidak_berisiko"
			if skor >= 2 {
				want = "berisiko"
			}
			if len(mst) < 4 || sum != skor || risiko != want {
				c.JSON(http.StatusBadRequest, gin.H{"error": "skor MST tidak konsisten"})
				return nil, false
			}
		}
	}
	// MNA-SF consistency (spec §5 hook, pola MST): skor_mnasf must equal the
	// sum of the six "<huruf>_<skor>" answers (total 0..14) and
	// interpretasi_mnasf must follow the band of the total. Partial drafts
	// without skor_mnasf pass (same policy as MST).
	if formType == "ases_gizi_mnasf" {
		if skor, ok := mst["skor_mnasf"]; ok {
			var interp string
			if r, okR := payload.Sections["interpretasi_mnasf"]; okR && !clinicalSectionEmpty(r) {
				_ = json.Unmarshal(r, &interp)
			}
			sum := mnasf["a"] + mnasf["b"] + mnasf["c"] + mnasf["d"] + mnasf["e"] + mnasf["f"]
			want := "malnutrisi"
			if skor >= 12 {
				want = "normal"
			} else if skor >= 8 {
				want = "berisiko"
			}
			if len(mnasf) < 6 || sum != skor || sum < 0 || sum > 14 || interp != want {
				c.JSON(http.StatusBadRequest, gin.H{"error": "skor MNA-SF tidak konsisten"})
				return nil, false
			}
		}
	}
	// Calc consistency (spec 2a §6.3, pattern MST recompute): a calc value may
	// only be filled when its inputs allow the recomputation. All inputs empty
	// → calc must be empty/absent (filled = soft warning, not an error);
	// partial inputs → calc filled = 400, calc empty = pass (FE inline warning).
	for _, d := range defs.Sections {
		if d.Kind != "calc" {
			continue
		}
		rawVal, present := payload.Sections[d.Key]
		calcFilled := present && !clinicalSectionEmpty(rawVal)
		complete, anyFilled, texts, nums := clinicalCalcInputs(payload.Sections, d.Calc.Inputs)
		if !complete {
			if !calcFilled {
				continue
			}
			// GCS "t": any filled input "t" pins the total to "T" even before
			// the remaining inputs are known (spec 2a Task 2 test c).
			if d.Calc.Formula == "gcs" && clinicalCalcIsT(rawVal, texts) {
				continue
			}
			if anyFilled {
				c.JSON(http.StatusBadRequest, gin.H{"error": "kalkulasi " + d.Key + " tidak konsisten"})
				return nil, false
			}
			warnings = append(warnings, "kalkulasi '"+d.Key+"' terisi tanpa input lengkap")
			continue
		}
		if !calcFilled {
			continue
		}
		inconsistent := false
		switch d.Calc.Formula {
		case "gcs":
			t := false
			sum := 0
			for _, s := range texts {
				if s == "t" {
					t = true
					continue
				}
				n, err := strconv.Atoi(s)
				if err != nil {
					inconsistent = true
					break
				}
				sum += n
			}
			if t {
				inconsistent = !clinicalCalcIsT(rawVal, texts)
			} else {
				v, ok := clinicalCalcNumber(rawVal)
				inconsistent = !ok || int(v) != sum
			}
		case "imt":
			expected := math.Round(nums["objective.weight_kg"]/math.Pow(nums["objective.height_cm"]/100, 2)*10) / 10
			v, ok := clinicalCalcNumber(rawVal)
			inconsistent = !ok || math.Abs(v-expected) > 0.1+1e-9
		default:
			// Unknown formula = malformed template shape.
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported section kind: calc"})
			return nil, false
		}
		if inconsistent {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kalkulasi " + d.Key + " tidak konsisten"})
			return nil, false
		}
	}
	// SOAP v3 calc consistency (spec 2026-11-10-cppt-pemeriksaan-v3 §Validasi
	// server): imt/imt_kategori, gcs/kategori/pupil_score/gcs_p,
	// news2_total — recompute vs user value after shape validation.
	if !clinicalValidateCalcV3(c, payload.Sections) {
		return nil, false
	}
	return warnings, true
}

// validateTindakanConcepts checks every sections.tindakan row with a
// non-empty concept_id against the ICD-9-CM code system (spec
// 2026-11-10-cppt-pemeriksaan-v3 §Bentuk nilai #18; query EXISTS persis
// tariff.go:82-86). Called from Create/Update handlers AFTER
// validateClinicalFormData succeeded — the validator has no tx, the handler
// runs ambient RunRequestTx so a 400 auto-rolls back. Rows without
// concept_id (partial draft) are accepted. The returned error message is the
// contract 400 message; DB errors are written as 500 inside (responded → the
// caller must not overwrite, see c.Writer.Written()).
func validateTindakanConcepts(c *gin.Context, raw json.RawMessage) error {
	var payload struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	rawT, ok := payload.Sections["tindakan"]
	if !ok || clinicalSectionEmpty(rawT) {
		return nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(rawT, &rows); err != nil {
		return errors.New("invalid value for section 'tindakan'")
	}
	for _, row := range rows {
		rawCID, present := row["concept_id"]
		if !present || string(rawCID) == "null" {
			continue
		}
		var conceptIDStr string
		if err := json.Unmarshal(rawCID, &conceptIDStr); err != nil {
			return errors.New("invalid value for section 'tindakan'")
		}
		if conceptIDStr == "" {
			continue
		}
		id, okID := parseUUID(conceptIDStr)
		if !okID {
			return errors.New("invalid value for section 'tindakan'")
		}
		var exists bool
		err := TxFromContext(c).QueryRow(c.Request.Context(), `
			SELECT EXISTS(SELECT 1 FROM terminology.concept ct
			JOIN terminology.code_system cs ON cs.id = ct.code_system_id
			WHERE ct.id = $1 AND ct.is_active AND ct.is_selectable AND cs.system_uri = $2)`,
			id, icd9cmSystemURI).Scan(&exists)
		if err != nil {
			respondInternalError(c, err)
			return err
		}
		if !exists {
			return errors.New("tindakan harus konsep ICD-9-CM aktif")
		}
	}
	return nil
}

// clinicalCalcInputs reads the current values of a calc definition's inputs
// (spec 2a §6.3): bare section keys resolve to select strings ("gcs" E/V/M),
// "<section>.<field>" paths resolve to numbers of that section object
// ("imt" ← objective.weight_kg / objective.height_cm). complete = every input
// filled; anyFilled = at least one filled (partial inputs).
func clinicalCalcInputs(sections map[string]json.RawMessage, inputs []string) (complete, anyFilled bool, texts []string, nums map[string]float64) {
	complete, anyFilled = true, false
	texts = []string{}
	nums = map[string]float64{}
	for _, in := range inputs {
		sec, field, isPath := strings.Cut(in, ".")
		filled := false
		if isPath {
			var m map[string]json.Number
			if r, ok := sections[sec]; ok && !clinicalSectionEmpty(r) {
				_ = json.Unmarshal(r, &m)
			}
			if n, ok := m[field]; ok {
				if v, err := n.Float64(); err == nil {
					nums[in] = v
					filled = true
				}
			}
		} else {
			var s string
			if r, ok := sections[in]; ok && !clinicalSectionEmpty(r) {
				if err := json.Unmarshal(r, &s); err == nil && s != "" {
					texts = append(texts, s)
					filled = true
				}
			}
		}
		if filled {
			anyFilled = true
		} else {
			complete = false
		}
	}
	return complete, anyFilled, texts, nums
}

// clinicalCalcIsT reports a gcs calc value consistent with a "t" input: at
// least one filled input is "t" and the calc value is exactly the string "T".
func clinicalCalcIsT(raw json.RawMessage, texts []string) bool {
	hasT := false
	for _, s := range texts {
		if s == "t" {
			hasT = true
		}
	}
	if !hasT {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	return s == "T"
}

// clinicalCalcNumber reads a calc section value as a number (JSON number or
// numeric string); the "T" string form is handled by the gcs branch.
func clinicalCalcNumber(raw json.RawMessage) (float64, bool) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if v, err := n.Float64(); err == nil {
			return v, true
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// clinicalRepeatableFieldOk validates one repeatable row value against its
// field def (spec 2a §6.2: text/select/date; v3 §Bentuk nilai #18: number =
// json.Number, time = HH:MM, boolean = bool).
func clinicalRepeatableFieldOk(raw json.RawMessage, fd clinicalRepeatableFieldDef) bool {
	switch fd.Type {
	case "number":
		// json.Number would also accept a JSON string (it has string kind) —
		// reject quoted values so `tarif_snapshot:"150000"` fails.
		if len(raw) > 0 && raw[0] == '"' {
			return false
		}
		var n json.Number
		return json.Unmarshal(raw, &n) == nil
	case "boolean":
		var b bool
		return json.Unmarshal(raw, &b) == nil
	case "time":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return false
		}
		return clinicalTimeRe.MatchString(s)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	switch fd.Type {
	case "text":
		return true
	case "select":
		for _, o := range fd.Options {
			if o.Value == s {
				return true
			}
		}
		return false
	case "date":
		_, err := time.Parse("2006-01-02", s)
		return err == nil
	}
	return false
}

// clinicalFormJSON maps a sqlcgen row to the snake_case contract shape (spec
// §9; the sqlcgen struct has no json tags — pattern admissionJSON).
func clinicalFormJSON(e sqlcgen.ClinicalFormFormInstance) gin.H {
	return gin.H{
		"id":                uuidJSON(e.ID),
		"company_id":        uuidJSON(e.CompanyID),
		"merchant_id":       uuidJSON(e.MerchantID),
		"admission_id":      uuidJSON(e.AdmissionID),
		"form_type":         e.FormType,
		"template_id":       uuidJSON(e.TemplateID),
		"template_version":  e.TemplateVersion,
		"form_data":         json.RawMessage(e.FormData),
		"status":            e.Status,
		"filled_by":         uuidJSON(e.FilledBy),
		"filled_at":         tsJSON(e.FilledAt),
		"signed_by":         uuidJSON(e.SignedBy),
		"signed_at":         tsJSON(e.SignedAt),
		"signature_hash":    textJSON(e.SignatureHash),
		"counter_signed_by": uuidJSON(e.CounterSignedBy),
		"counter_signed_at": tsJSON(e.CounterSignedAt),
		"addendum_to_id":    uuidJSON(e.AddendumToID),
		"addendum_reason":   textJSON(e.AddendumReason),
		"created_at":        tsJSON(e.CreatedAt),
		"updated_at":        tsJSON(e.UpdatedAt),
		"deleted_at":        tsJSON(e.DeletedAt),
		"deleted_by":        uuidJSON(e.DeletedBy),
		"row_version":       e.RowVersion,
	}
}

// ListClinicalFormTemplatesHandler godoc
// @Summary List active clinical form templates (best scope per form_type)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Router /clinical/form-templates [get]
func ListClinicalFormTemplatesHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequireAnyPermissionForMerchant(c, merchantID, PermClinicalRecordView, PermClinicalNutritionWrite) {
		return
	}
	rows, err := sqlcgen.New(TxFromContext(c)).ListClinicalFormTemplates(c.Request.Context(),
		sqlcgen.ListClinicalFormTemplatesParams{CompanyID: AuthCompanyID(c), MerchantID: merchantID})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	scopeRank := map[string]int{"platform": 1, "company": 2, "merchant": 3}
	best := map[string]sqlcgen.ListClinicalFormTemplatesRow{}
	for _, r := range rows {
		if cur, seen := best[r.FormType]; !seen || scopeRank[r.Scope] > scopeRank[cur.Scope] {
			best[r.FormType] = r
		}
	}
	data := make([]gin.H, 0, len(best))
	for _, r := range best {
		data = append(data, gin.H{
			"id": uuidJSON(r.ID), "code": r.Code, "form_type": r.FormType,
			"category": r.Category, "name": r.Name, "scope": r.Scope,
			"version": r.Version,
			// sections as a JSON object, not a string.
			"sections": json.RawMessage(r.Sections),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type createClinicalFormRequest struct {
	AdmissionID string          `json:"admission_id" binding:"required"`
	FormType    string          `json:"form_type" binding:"required"`
	FormData    json.RawMessage `json:"form_data"`
}

// CreateClinicalFormHandler godoc
// @Summary Create a draft clinical form entry (CPPT fase 1 = rawat jalan only)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createClinicalFormRequest true "Form entry data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/forms [post]
func CreateClinicalFormHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	var req createClinicalFormRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, AuthCompanyID(c), merchantID, req.FormType)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "template form tidak ditemukan"})
		return
	}
	if !RequirePermissionForMerchant(c, clinicalWritePerm(req.FormType, tpl.Category), merchantID) {
		return
	}
	admissionID, okID := parseUUID(req.AdmissionID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	admission, err := q.GetAdmissionByID(ctx, admissionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && admission.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if admission.Status == "cancelled" || admission.AdmissionType != "outpatient" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cppt fase 1 hanya rawat jalan"})
		return
	}
	var defs clinicalSectionsDef
	if err := json.Unmarshal(tpl.Sections, &defs); err != nil {
		respondInternalError(c, err)
		return
	}
	warnings, okV := validateClinicalFormData(c, req.FormType, defs, req.FormData)
	if !okV {
		return
	}
	formData := req.FormData
	if len(formData) == 0 {
		formData = json.RawMessage(`{"sections":{}}`)
	}
	if err := validateTindakanConcepts(c, formData); err != nil && !c.Writer.Written() {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	} else if err != nil {
		return
	}
	entry, err := q.CreateClinicalFormInstance(ctx, sqlcgen.CreateClinicalFormInstanceParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, AdmissionID: admissionID,
		FormType: req.FormType, TemplateID: tpl.TemplateID, TemplateVersion: tpl.Version,
		FormData: formData, FilledBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Hook hasil skrining MST (FR-GZ-02, spec §5): write form sukses +
	// ases_gizi_skrining dengan skor_mst terisi → upsert
	// clinical.nutrition_screening_result (tx ambient; gagal = 500 rollback).
	if !persistGiziScreeningResult(c, entry.ID, admissionID, admission.PersonID) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"entry": clinicalFormJSON(entry), "warnings": warnings}, "meta": gin.H{}})
}

// ListClinicalFormsHandler godoc
// @Summary List clinical form entries of one admission (desc filled_at)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Router /clinical/forms [get]
func ListClinicalFormsHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	admissionID, okID := parseUUID(c.Query("admission_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "admission_id query param is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	if _, err := q.GetAdmissionByID(ctx, admissionID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	seeGizi := clinicalUserHasPerm(c, q, merchantID, PermClinicalNutritionWrite)
	templates, err := q.ListClinicalFormTemplates(ctx, sqlcgen.ListClinicalFormTemplatesParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	category := map[string]string{}
	for _, r := range templates {
		if _, seen := category[r.FormType]; !seen {
			category[r.FormType] = r.Category
		}
	}
	rows, err := q.ListClinicalFormInstancesByAdmission(ctx, sqlcgen.ListClinicalFormInstancesByAdmissionParams{
		AdmissionID: admissionID, Column2: false,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Gizi forms need clinical.nutrition.write to be visible (spec §9:180);
	// RLS already scopes everything to the merchant.
	items := []gin.H{}
	for _, r := range rows {
		if !seeGizi && clinicalIsGiziForm(r.FormType, category[r.FormType]) {
			continue
		}
		items = append(items, clinicalFormJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetClinicalFormHandler godoc
// @Summary Get a clinical form entry by id (with addendum chain ids)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Form entry UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/forms/{id} [get]
func GetClinicalFormHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermission(c, PermClinicalRecordView) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	entry, err := q.GetClinicalFormInstance(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	addendums, err := q.ListClinicalFormAddendums(c.Request.Context(), id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ids := make([]string, 0, len(addendums))
	for _, a := range addendums {
		ids = append(ids, a.ID.String())
	}
	data := clinicalFormJSON(entry)
	data["addendums"] = ids
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type updateClinicalFormRequest struct {
	FormData json.RawMessage `json:"form_data" binding:"required"`
}

// UpdateClinicalFormHandler godoc
// @Summary Update form_data of a draft entry (author only; signed = locked)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Form entry UUID"
// @Param request body updateClinicalFormRequest true "New form data"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/forms/{id} [patch]
func UpdateClinicalFormHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	// FOR UPDATE so a concurrent sign cannot slip between the checks and write.
	entry, err := q.GetClinicalFormInstanceForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, entry.CompanyID, merchantID, entry.FormType)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "template form tidak ditemukan"})
		return
	}
	if !RequirePermissionForMerchant(c, clinicalWritePerm(entry.FormType, tpl.Category), merchantID) {
		return
	}
	if entry.FilledBy != AuthUserID(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "hanya penulis entri"})
		return
	}
	if entry.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	var req updateClinicalFormRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	var defs clinicalSectionsDef
	if err := json.Unmarshal(tpl.Sections, &defs); err != nil {
		respondInternalError(c, err)
		return
	}
	warnings, okV := validateClinicalFormData(c, entry.FormType, defs, req.FormData)
	if !okV {
		return
	}
	if err := validateTindakanConcepts(c, req.FormData); err != nil && !c.Writer.Written() {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	} else if err != nil {
		return
	}
	updated, err := q.UpdateClinicalFormInstanceDraft(ctx, sqlcgen.UpdateClinicalFormInstanceDraftParams{
		ID: id, FormData: req.FormData,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Hook hasil skrining MST (FR-GZ-02, spec §5): idempoten per (form,
	// instrument) — PATCH berulang tetap 1 baris, nilai ter-UPDATE.
	if admission, err := q.GetAdmissionByID(ctx, updated.AdmissionID); err != nil {
		respondInternalError(c, err)
		return
	} else if !persistGiziScreeningResult(c, updated.ID, updated.AdmissionID, admission.PersonID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entry": clinicalFormJSON(updated), "warnings": warnings}, "meta": gin.H{}})
}

// DeleteClinicalFormHandler godoc
// @Summary Soft-delete a draft clinical form entry (author only)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Form entry UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/forms/{id} [delete]
func DeleteClinicalFormHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	entry, err := q.GetClinicalFormInstanceForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, entry.CompanyID, merchantID, entry.FormType)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "template form tidak ditemukan"})
		return
	}
	if !RequirePermissionForMerchant(c, clinicalWritePerm(entry.FormType, tpl.Category), merchantID) {
		return
	}
	if entry.FilledBy != AuthUserID(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "hanya penulis entri"})
		return
	}
	if entry.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	if _, err := q.SoftDeleteClinicalFormInstance(ctx, sqlcgen.SoftDeleteClinicalFormInstanceParams{
		ID: id, DeletedBy: AuthUserID(c),
	}); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id.String(), "deleted": true}, "meta": gin.H{}})
}
