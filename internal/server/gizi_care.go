// internal/server/gizi_care.go
// Gizi (nutrition care) endpoints — plan 2026-11-13-gizi-nutrition-care
// chunk 1 Task 3 (spec 2026-11-13-gizi-nutrition-care-spec §4.1, §5):
// antropometri (FR-GZ-13 snapshot), episode gizi + guard 3 jalur profil
// (FR-GZ-01: close_active + PATCH audit + revert), riwayat per person
// (FR-GZ-15). Semua handler jalan di tx ambient RunRequestTx.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterGiziCareRoutes(rg *gin.RouterGroup) {
	rg.POST("/clinical/anthropometry", CreateGiziAnthropometryHandler)
	rg.GET("/clinical/anthropometry", AccessLog("anthropometry", "view", "admission_id"), ListGiziAnthropometryHandler)
	rg.POST("/clinical/nutrition-episode", CreateGiziNutritionEpisodeHandler)
	rg.GET("/clinical/nutrition-episode", GetGiziNutritionEpisodeHandler)
	rg.PATCH("/clinical/nutrition-episode/:id", UpdateGiziNutritionEpisodeHandler)
	rg.POST("/clinical/nutrition-episode/:id/revert-profile", RevertGiziNutritionProfileHandler)
	rg.GET("/clinical/nutrition-episodes", ListGiziNutritionEpisodesHandler)
}

var validGiziProfiles = map[string]bool{"dewasa": true, "lansia": true, "bumil": true, "menyusui": true, "balita": true}
var validGiziRisks = map[string]bool{"rendah": true, "sedang": true, "tinggi": true}

// personGenderToGizi maps core.person.gender (male/female) to the
// clinical.anthropometry CHECK domain (laki_laki/perempuan).
func personGenderToGizi(g pgtype.Text) (string, bool) {
	switch g.String {
	case "male":
		return "laki_laki", true
	case "female":
		return "perempuan", true
	default:
		return "", false
	}
}

// giziMstScore ekstrak skor_mst dari form_data (hook TIDAK menghitung ulang —
// validator clinical_form.go §5.1/§5.2 sudah menjamin konsistensi jumlah
// jawaban). Non-ases_gizi_skrining atau skor kosong/tak-valid → (0, false)
// = hook dilewati (draft parsial).
func giziMstScore(formType string, formData json.RawMessage) (int, bool) {
	if formType != "ases_gizi_skrining" {
		return 0, false
	}
	var payload struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(formData, &payload); err != nil {
		return 0, false
	}
	rawVal, ok := payload.Sections["skor_mst"]
	if !ok || clinicalSectionEmpty(rawVal) {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(rawVal, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil || v < 0 || v > 5 {
		return 0, false
	}
	return int(v), true
}

// giziMstInterpretation STD-31 OV000298–300 R:77: 0→rendah; 1–3→sedang; 4–5→tinggi.
func giziMstInterpretation(score int) string {
	switch {
	case score == 0:
		return "rendah"
	case score <= 3:
		return "sedang"
	default:
		return "tinggi"
	}
}

// persistGiziScreeningResult menulis hasil skrining MST ke
// clinical.nutrition_screening_result (FR-GZ-02, spec §5 hook teknis C).
// Dipanggil dari Create/UpdateClinicalFormHandler SETELAH write form sukses,
// dalam tx ambient RunRequestTx (error hook → respondInternalError → rollback).
// Idempoten: form di-PATCH berkali → tetap 1 baris per (form_instance, 'mst')
// (UPDATE dulu; 0 baris → INSERT). Form lain / skor kosong → dilewati.
func persistGiziScreeningResult(c *gin.Context, formID, admissionID, personID pgtype.UUID) bool {
	merchantID, okM := requireMerchantHeader(c)
	if !okM {
		return false
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	// episode aktif admission bila ada; tidak ada → NULL (bukan error).
	var episodeID pgtype.UUID
	episode, err := q.GetActiveNutritionEpisodeByAdmission(ctx, admissionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		respondInternalError(c, err)
		return false
	default:
		episodeID = episode.ID
	}
	// lintas data form terbaru untuk skor (validator existing sudah validasi).
	entry, err := q.GetClinicalFormInstance(ctx, formID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	score, okS := giziMstScore(entry.FormType, entry.FormData)
	if !okS {
		return true // hook dilewati (draft parsial / form lain)
	}
	bands := json.RawMessage(`[{"low":0,"high":1,"interpretation":"rendah"},{"low":1,"high":4,"interpretation":"sedang"},{"low":4,"high":6,"interpretation":"tinggi"}]`)
	pl := json.RawMessage(`{"berisiko_mst":true}`)
	if score < 2 {
		pl = json.RawMessage(`{"berisiko_mst":false}`)
	}
	userID := AuthUserID(c)
	n, err := q.UpdateGiziScreeningResultByForm(ctx, sqlcgen.UpdateGiziScreeningResultByFormParams{
		FormInstanceID: formID, Score: int32(score),
		Interpretation: giziMstInterpretation(score),
		Bands:          bands, Payload: pl, EpisodeID: episodeID, CalculatedBy: userID,
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if n == 0 {
		if _, err := q.InsertGiziScreeningResult(ctx, sqlcgen.InsertGiziScreeningResultParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: personID,
			AdmissionID: admissionID, EpisodeID: episodeID, FormInstanceID: formID,
			Score: int32(score), Interpretation: giziMstInterpretation(score),
			Bands: bands, Payload: pl, CalculatedBy: userID, CreatedBy: userID,
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

func giziJSONHelperError(c *gin.Context, field string) bool {
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value for " + field})
	return false
}

// anthropometryJSON maps an anthropometry row (insert RETURNING or latest-row
// query — both expose the same snapshot column set) to the snake_case contract
// shape with the trigger-computed snapshot (FR-GZ-13).
func anthropometryJSON(r sqlcgen.InsertAnthropometryRow) gin.H {
	return gin.H{
		"id":            uuidJSON(r.ID),
		"person_id":     uuidJSON(r.PersonID),
		"admission_id":  uuidJSON(r.AdmissionID),
		"measured_at":   tsJSON(r.MeasuredAt),
		"gender":        r.Gender,
		"age_days":      intOrNil(r.AgeDays),
		"bb_kg":         numericJSON(r.BbKg),
		"tb_cm":         numericJSON(r.TbCm),
		"source_ref":    r.SourceRef,
		"extras":        rawJSONOrNil(r.Extras),
		"zscore_bbu":    numericJSON(r.ZscoreBbu),
		"zscore_tbu":    numericJSON(r.ZscoreTbu),
		"zscore_bbtb":   numericJSON(r.ZscoreBbtb),
		"zscore_imtu":   numericJSON(r.ZscoreImtu),
		"zscore_lku":    numericJSON(r.ZscoreLku),
		"zscore_lila":   numericJSON(r.ZscoreLila),
		"imt":           numericJSON(r.Imt),
		"imt_kategori":  textJSON(r.ImtKategori),
		"status":        rawJSONOrNil(r.Status),
		"curve_version": textJSON(r.CurveVersion),
		"created_at":    tsJSON(r.CreatedAt),
		"created_by":    uuidJSON(r.CreatedBy),
		"row_version":   r.RowVersion,
	}
}

// latestAnthropometryJSON converts the by-person latest row (structurally the
// same columns) into the shared shape.
func latestAnthropometryJSON(r sqlcgen.GetLatestAnthropometryByPersonRow) gin.H {
	return anthropometryJSON(sqlcgen.InsertAnthropometryRow{
		ID: r.ID, CompanyID: r.CompanyID, MerchantID: r.MerchantID,
		PersonID: r.PersonID, AdmissionID: r.AdmissionID, MeasuredAt: r.MeasuredAt,
		Gender: r.Gender, AgeDays: r.AgeDays, BbKg: r.BbKg, TbCm: r.TbCm,
		SourceRef: r.SourceRef, Extras: r.Extras, ZscoreBbu: r.ZscoreBbu,
		ZscoreTbu: r.ZscoreTbu, ZscoreBbtb: r.ZscoreBbtb, ZscoreImtu: r.ZscoreImtu,
		ZscoreLku: r.ZscoreLku, ZscoreLila: r.ZscoreLila, Imt: r.Imt,
		ImtKategori: r.ImtKategori, Status: r.Status, CurveVersion: r.CurveVersion,
		CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, RowVersion: r.RowVersion,
	})
}

func episodeJSON(e sqlcgen.ClinicalNutritionEpisode) gin.H {
	return gin.H{
		"id":            uuidJSON(e.ID),
		"person_id":     uuidJSON(e.PersonID),
		"admission_id":  uuidJSON(e.AdmissionID),
		"profile":       e.Profile,
		"risk_override": textJSON(e.RiskOverride),
		"status":        e.Status,
		"extras":        rawJSONOrNil(e.Extras),
		"started_at":    tsJSON(e.StartedAt),
		"closed_at":     tsJSON(e.ClosedAt),
		"row_version":   e.RowVersion,
	}
}

func nsrJSON(r sqlcgen.ListLatestNutritionScreeningByAdmissionRow) gin.H {
	return gin.H{
		"id":             uuidJSON(r.ID),
		"instrument":     r.Instrument,
		"score":          r.Score,
		"max_score":      r.MaxScore,
		"interpretation": r.Interpretation,
		"bands":          rawJSONOrNil(r.Bands),
		"payload":        rawJSONOrNil(r.Payload),
		"calculated_at":  tsJSON(r.CalculatedAt),
	}
}

type createGiziAnthropometryRequest struct {
	AdmissionID string          `json:"admission_id" binding:"required"`
	MeasuredAt  string          `json:"measured_at" binding:"required"`
	BbKg        float64         `json:"bb_kg" binding:"required"`
	TbCm        *float64        `json:"tb_cm"`
	Extras      json.RawMessage `json:"extras"`
}

// validateGiziExtras enforces spec §5 extras ranges (mgg 1..45, lila/lk > 0).
func validateGiziExtras(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var ex map[string]any
	if err := json.Unmarshal(raw, &ex); err != nil {
		return false
	}
	if v, ok := ex["mgg"]; ok {
		n, isNum := v.(float64)
		if !isNum || n < 1 || n > 45 {
			return false
		}
	}
	for _, key := range []string{"lila_cm", "lk_cm"} {
		if v, ok := ex[key]; ok {
			n, isNum := v.(float64)
			if !isNum || n <= 0 {
				return false
			}
		}
	}
	return true
}

// giziAdmission resolves the admission + person context for gizi endpoints;
// a foreign-merchant or unknown admission is a 404 (RLS pattern).
func giziAdmission(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, admissionID pgtype.UUID) (sqlcgen.GetAdmissionForGiziRow, bool) {
	admission, err := q.GetAdmissionForGizi(c.Request.Context(), admissionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && admission.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return sqlcgen.GetAdmissionForGiziRow{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.GetAdmissionForGiziRow{}, false
	}
	return admission, true
}

// giziAgeDays derives age in days from birth_date to measured_at in the
// merchant timezone (pola merchant_day.go); birth_date NULL → NULL.
func giziAgeDays(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, birth pgtype.Date, measured time.Time) pgtype.Int4 {
	if !birth.Valid {
		return pgtype.Int4{}
	}
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		return pgtype.Int4{}
	}
	birthMidnight := time.Date(birth.Time.Year(), birth.Time.Month(), birth.Time.Day(), 0, 0, 0, 0, loc)
	days := int(measured.In(loc).Sub(birthMidnight).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return pgtype.Int4{Int32: int32(days), Valid: true}
}

// CreateGiziAnthropometryHandler godoc
// @Summary Record an anthropometry measurement (snapshot via trigger)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createGiziAnthropometryRequest true "Measurement data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/anthropometry [post]
func CreateGiziAnthropometryHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	var req createGiziAnthropometryRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	admissionID, okID := parseUUID(req.AdmissionID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	measured, err := time.Parse(time.RFC3339, req.MeasuredAt)
	if err != nil {
		giziJSONHelperError(c, "measured_at")
		return
	}
	if measured.After(time.Now()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "measured_at tidak boleh di masa depan"})
		return
	}
	if req.BbKg <= 0 || req.BbKg >= 500 {
		giziJSONHelperError(c, "bb_kg")
		return
	}
	var tb pgtype.Numeric
	if req.TbCm != nil {
		if *req.TbCm <= 0 || *req.TbCm >= 250 {
			giziJSONHelperError(c, "tb_cm")
			return
		}
		tb = numericFromFloat(req.TbCm)
	}
	if !validateGiziExtras(req.Extras) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value for extras"})
		return
	}
	extras := req.Extras
	if len(extras) == 0 {
		extras = json.RawMessage(`{}`)
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okA := giziAdmission(c, q, merchantID, admissionID)
	if !okA {
		return
	}
	gender, okG := personGenderToGizi(admission.Gender)
	if !okG {
		giziJSONHelperError(c, "gender")
		return
	}
	ageDays := giziAgeDays(c, q, merchantID, admission.BirthDate, measured)
	row, err := q.InsertAnthropometry(c.Request.Context(), sqlcgen.InsertAnthropometryParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: admission.PersonID,
		AdmissionID: admissionID, MeasuredAt: pgtype.Timestamptz{Time: measured, Valid: true},
		Gender: gender, AgeDays: ageDays, BbKg: numericFromFloat(&req.BbKg), TbCm: tb,
		SourceRef: "gizi_tab", Extras: extras, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": anthropometryJSON(row), "meta": gin.H{}})
}

// ListGiziAnthropometryHandler godoc
// @Summary List anthropometry measurements of an admission (measured_at DESC)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/anthropometry [get]
func ListGiziAnthropometryHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	admissionID, okID := parseUUID(c.Query("admission_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okA := giziAdmission(c, q, merchantID, admissionID); !okA {
		return
	}
	rows, err := q.ListAnthropometryByAdmission(c.Request.Context(), admissionID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		item := anthropometryJSON(sqlcgen.InsertAnthropometryRow{
			ID: r.ID, PersonID: r.PersonID, AdmissionID: r.AdmissionID,
			MeasuredAt: r.MeasuredAt, Gender: r.Gender, AgeDays: r.AgeDays,
			BbKg: r.BbKg, TbCm: r.TbCm, SourceRef: r.SourceRef, Extras: r.Extras,
			ZscoreBbu: r.ZscoreBbu, ZscoreTbu: r.ZscoreTbu, ZscoreBbtb: r.ZscoreBbtb,
			ZscoreImtu: r.ZscoreImtu, ZscoreLku: r.ZscoreLku, ZscoreLila: r.ZscoreLila,
			Imt: r.Imt, ImtKategori: r.ImtKategori, Status: r.Status,
			CurveVersion: r.CurveVersion, RowVersion: r.RowVersion,
		})
		item["person_name"] = r.PersonName
		data = append(data, item)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type createGiziNutritionEpisodeRequest struct {
	AdmissionID string          `json:"admission_id" binding:"required"`
	Profile     string          `json:"profile" binding:"required"`
	Extras      json.RawMessage `json:"extras"`
	CloseActive *bool           `json:"close_active"`
}

// CreateGiziNutritionEpisodeHandler godoc
// @Summary Open a nutrition episode for an admission (guard close_active)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createGiziNutritionEpisodeRequest true "Episode data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/nutrition-episode [post]
func CreateGiziNutritionEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	var req createGiziNutritionEpisodeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if !validGiziProfiles[req.Profile] {
		giziJSONHelperError(c, "profile")
		return
	}
	admissionID, okID := parseUUID(req.AdmissionID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okA := giziAdmission(c, q, merchantID, admissionID)
	if !okA {
		return
	}
	// Guard jalur 1 (FR-GZ-01): close_active=true menutup episode aktif lama
	// dan membuka yang baru dalam SATU tx (tx ambient; respons ≥400 rollback).
	if req.CloseActive != nil && *req.CloseActive {
		if _, err := q.CloseActiveNutritionEpisodeByAdmission(c.Request.Context(), sqlcgen.CloseActiveNutritionEpisodeByAdmissionParams{
			AdmissionID: admissionID, UpdatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	} else if _, err := q.GetActiveNutritionEpisodeByAdmission(c.Request.Context(), admissionID); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "episode gizi aktif sudah ada"})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	extras := req.Extras
	if len(extras) == 0 {
		extras = json.RawMessage(`{}`)
	}
	created, err := q.CreateNutritionEpisode(c.Request.Context(), sqlcgen.CreateNutritionEpisodeParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: admission.PersonID,
		AdmissionID: admissionID, Profile: req.Profile, Extras: extras, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": episodeJSON(created), "meta": gin.H{}})
}

// GetGiziNutritionEpisodeHandler godoc
// @Summary Current nutrition episode of an admission + screening summary
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/nutrition-episode [get]
func GetGiziNutritionEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
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
	resp := gin.H{
		// Placeholder risiko chunk 3 (rules §6) mengisi nilai ini.
		"risk": nil,
	}
	episode, err := q.GetActiveNutritionEpisodeByAdmission(c.Request.Context(), admissionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		resp["episode"] = nil
	case err != nil:
		respondInternalError(c, err)
		return
	default:
		resp["episode"] = episodeJSON(episode)
	}
	screening, err := q.ListLatestNutritionScreeningByAdmission(c.Request.Context(), admissionID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	scr := make([]gin.H, 0, len(screening))
	for _, r := range screening {
		scr = append(scr, nsrJSON(r))
	}
	resp["screening"] = scr
	latest, err := q.GetLatestAnthropometryByPerson(c.Request.Context(), admission.PersonID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		resp["anthropometry_latest"] = nil
	case err != nil:
		respondInternalError(c, err)
		return
	default:
		resp["anthropometry_latest"] = latestAnthropometryJSON(latest)
	}
	kunjungan, err := q.CountKunjunganByPerson(c.Request.Context(), admission.PersonID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	resp["kunjungan_count"] = kunjungan
	c.JSON(http.StatusOK, resp)
}

type updateGiziNutritionEpisodeRequest struct {
	RowVersion   int32           `json:"row_version" binding:"required"`
	Profile      *string         `json:"profile"`
	Extras       json.RawMessage `json:"extras"`
	RiskOverride *string         `json:"risk_override"`
	Status       *string         `json:"status"`
}

// giziEpisodeForWrite resolves an episode for PATCH/revert; foreign-merchant
// or unknown id is a 404 (RLS pattern).
func giziEpisodeForWrite(c *gin.Context, q *sqlcgen.Queries, merchantID, id pgtype.UUID) (sqlcgen.ClinicalNutritionEpisode, bool) {
	episode, err := q.GetNutritionEpisodeByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && episode.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode gizi tidak ditemukan"})
		return sqlcgen.ClinicalNutritionEpisode{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.ClinicalNutritionEpisode{}, false
	}
	return episode, true
}

// UpdateGiziNutritionEpisodeHandler godoc
// @Summary Patch a nutrition episode (optimistic lock; profile change audited)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Episode UUID"
// @Param request body updateGiziNutritionEpisodeRequest true "Patch data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/nutrition-episode/{id} [patch]
func UpdateGiziNutritionEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Param("id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}
	var req updateGiziNutritionEpisodeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if req.Profile != nil && !validGiziProfiles[*req.Profile] {
		giziJSONHelperError(c, "profile")
		return
	}
	if req.RiskOverride != nil && !validGiziRisks[*req.RiskOverride] {
		giziJSONHelperError(c, "risk_override")
		return
	}
	if req.Status != nil && *req.Status != "active" && *req.Status != "closed" {
		giziJSONHelperError(c, "status")
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	episode, okE := giziEpisodeForWrite(c, q, merchantID, episodeID)
	if !okE {
		return
	}
	// Guard jalur 2 (FR-GZ-01/15): ganti profil tercatat ke tabel audit
	// nutrition_profile_change dalam tx yang sama; data kunjungan tak disentuh.
	newProfile := episode.Profile
	if req.Profile != nil && *req.Profile != episode.Profile {
		newProfile = *req.Profile
		if _, err := q.InsertNutritionProfileChange(c.Request.Context(), sqlcgen.InsertNutritionProfileChangeParams{
			CompanyID: episode.CompanyID, MerchantID: episode.MerchantID, PersonID: episode.PersonID,
			EpisodeID: episode.ID, FromProfile: episode.Profile, ToProfile: newProfile,
			ChangedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	extras := episode.Extras
	if req.Extras != nil {
		extras = req.Extras
	}
	risk := episode.RiskOverride
	if req.RiskOverride != nil {
		risk = pgtype.Text{String: *req.RiskOverride, Valid: true}
	}
	status := episode.Status
	if req.Status != nil {
		status = *req.Status
	}
	closedAt := episode.ClosedAt
	if status == "closed" && !closedAt.Valid {
		closedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	updated, err := q.UpdateNutritionEpisode(c.Request.Context(), sqlcgen.UpdateNutritionEpisodeParams{
		ID: episodeID, Profile: newProfile, Extras: extras, RiskOverride: risk,
		Status: status, ClosedAt: closedAt, UpdatedBy: AuthUserID(c),
		RowVersion: req.RowVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "row_version konflik"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": episodeJSON(updated), "meta": gin.H{}})
}

// RevertGiziNutritionProfileHandler godoc
// @Summary Undo the last unreverted profile change of an episode (FR-GZ-01)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Episode UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/nutrition-episode/{id}/revert-profile [post]
func RevertGiziNutritionProfileHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Param("id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	episode, okE := giziEpisodeForWrite(c, q, merchantID, episodeID)
	if !okE {
		return
	}
	change, err := q.GetLastUnrevertedProfileChange(c.Request.Context(), episodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "tidak ada perubahan profil untuk dibatalkan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// SATU tx: balikkan profil + tandai baris audit reverted (tx ambient).
	updated, err := q.UpdateNutritionEpisode(c.Request.Context(), sqlcgen.UpdateNutritionEpisodeParams{
		ID: episodeID, Profile: change.FromProfile, Extras: episode.Extras,
		RiskOverride: episode.RiskOverride, Status: episode.Status,
		ClosedAt: episode.ClosedAt, UpdatedBy: AuthUserID(c),
		// RowVersion internal dari baris yang baru dibaca — bukan body klien.
		RowVersion: episode.RowVersion,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := q.MarkProfileChangeReverted(c.Request.Context(), sqlcgen.MarkProfileChangeRevertedParams{
		ID: change.ID, RevertedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		RevertedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": episodeJSON(updated), "reverted": true, "meta": gin.H{}})
}

// ListGiziNutritionEpisodesHandler godoc
// @Summary List a person's nutrition episodes + profile changes (FR-GZ-15)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Success 200 {object} apiResponse
// @Router /clinical/nutrition-episodes [get]
func ListGiziNutritionEpisodesHandler(c *gin.Context) {
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
	q := sqlcgen.New(TxFromContext(c))
	episodes, err := q.ListNutritionEpisodesByPerson(c.Request.Context(), personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(episodes))
	kunjungan := int64(0)
	for _, e := range episodes {
		if e.MerchantID != merchantID {
			continue
		}
		if kunjungan == 0 {
			if kunjungan, err = q.CountKunjunganByPerson(c.Request.Context(), personID); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		changes, err := q.ListProfileChangesByEpisode(c.Request.Context(), e.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		changeList := make([]gin.H, 0, len(changes))
		for _, ch := range changes {
			changeList = append(changeList, gin.H{
				"from_profile":    ch.FromProfile,
				"to_profile":      ch.ToProfile,
				"changed_at":      tsJSON(ch.ChangedAt),
				"changed_by_name": ch.ChangedByName,
				"reverted_at":     tsJSON(ch.RevertedAt),
			})
		}
		item := episodeJSON(e)
		item["kunjungan_count"] = kunjungan
		item["profile_changes"] = changeList
		data = append(data, item)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
