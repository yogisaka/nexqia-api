// internal/server/clinical_summary.go
// Person-scoped clinical read models (plan
// 2026-11-04-cppt-2a-soap-editor-a-backend Task 4, spec
// 2026-11-04-cppt-2a-soap-editor-design §7): "Salin dari catatan lalu"
// person-forms and the Ringkasan Pasien drawer (#32) — five per-domain
// queries composed in Go, no giant SQL join (spec §7 K2).
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterClinicalSummaryRoutes(rg *gin.RouterGroup) {
	rg.GET("/clinical/person-forms", AccessLog("clinical_form", "view", ""), GetClinicalPersonFormsHandler)
	rg.GET("/clinical/patient-summary", AccessLog("clinical_form", "view", ""), GetClinicalPatientSummaryHandler)
}

// clinicalSectionText reads a sections entry as a non-empty string
// (spec §7 latest_recommendation): empty/absent/null → false.
func clinicalSectionText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || clinicalSectionEmpty(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

// GetClinicalPersonFormsHandler godoc
// @Summary Signed clinical form entries of one person ("Salin dari catatan lalu")
// @Description Signed non-gizi entries across the person's admissions, newest filled_at first, limit 10 (spec §7).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Param form_type query string false "Form type filter (default: all non-gizi)"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/person-forms [get]
func GetClinicalPersonFormsHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	personID, okP := parseUUID(c.Query("person_id"))
	if !okP {
		c.JSON(http.StatusBadRequest, gin.H{"error": "person_id query param is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	if _, err := q.GetPersonByID(ctx, personID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pasien tidak ditemukan"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	hasAdmission, err := q.PersonHasMerchantAdmission(ctx, sqlcgen.PersonHasMerchantAdmissionParams{
		PersonID: personID, MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !hasAdmission {
		c.JSON(http.StatusNotFound, gin.H{"error": "pasien tidak ditemukan"})
		return
	}
	var formType pgtype.Text
	if ft := c.Query("form_type"); ft != "" {
		formType = pgtype.Text{String: ft, Valid: true}
	}
	rows, err := q.ListClinicalFormInstancesByPerson(ctx, sqlcgen.ListClinicalFormInstancesByPersonParams{
		PersonID: personID, MerchantID: merchantID, FormType: formType, Limit: 10,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		items = append(items, clinicalFormJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetClinicalPatientSummaryHandler godoc
// @Summary Composed patient summary (drawer #32)
// @Description Person identity + allergies + deduped active diagnoses + recent outpatient visits + latest signed SOAP recommendation (spec §7).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/patient-summary [get]
func GetClinicalPatientSummaryHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	personID, okP := parseUUID(c.Query("person_id"))
	if !okP {
		c.JSON(http.StatusBadRequest, gin.H{"error": "person_id query param is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	person, err := q.GetPersonByID(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pasien tidak ditemukan"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	// core.person is company-scoped: a person of another company (= another
	// merchant's tenant) is invisible to this drawer (spec §7 beda merchant
	// → 404) without gating on admission existence (person tanpa riwayat
	// stays 200 with empty domains).
	if person.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pasien tidak ditemukan"})
		return
	}

	allergies, err := q.ListPatientAllergiesByPerson(ctx, person.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	allergyItems := make([]gin.H, 0, len(allergies))
	for _, a := range allergies {
		allergyItems = append(allergyItems, gin.H{
			"substance_name": a.SubstanceName,
			"allergy_type":   a.AllergyType,
			"reaction":       textJSON(a.Reaction),
			"severity":       textJSON(a.Severity),
		})
	}

	// Fetch generously, dedup by primary_system:primary_code keeping the
	// newest row, cap at 10 (spec §7 active_diagnoses).
	diagnoses, err := q.ListPersonDiagnoses(ctx, sqlcgen.ListPersonDiagnosesParams{
		PersonID: personID, MerchantID: merchantID, Limit: 100,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	dxItems := make([]gin.H, 0, len(diagnoses))
	seen := map[string]bool{}
	for _, d := range diagnoses {
		key := d.PrimarySystem + ":" + d.PrimaryCode
		if seen[key] || len(dxItems) >= 10 {
			continue
		}
		seen[key] = true
		dxItems = append(dxItems, gin.H{
			"dx_role":         d.DxRole,
			"primary_system":  d.PrimarySystem,
			"primary_code":    d.PrimaryCode,
			"primary_display": d.PrimaryDisplay,
			"icd10_code":      textJSON(d.Icd10Code),
			"last_at":         tsJSON(d.CreatedAt),
		})
	}

	visits, err := q.ListPersonRecentVisits(ctx, sqlcgen.ListPersonRecentVisitsParams{
		PersonID: personID, MerchantID: merchantID, Limit: 5,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	visitItems := make([]gin.H, 0, len(visits))
	for _, v := range visits {
		visitItems = append(visitItems, gin.H{
			"admission_id":    uuidJSON(v.ID),
			"visit_no":        v.VisitNo,
			"admission_at":    tsJSON(v.AdmissionAt),
			"department_name": v.DepartmentName,
			"physician_name":  textJSON(v.PhysicianName),
			"status":          v.AdmissionStatus,
		})
	}

	var latestRecommendation any
	soap, err := q.GetPersonLatestSignedSoap(ctx, sqlcgen.GetPersonLatestSignedSoapParams{
		PersonID: personID, MerchantID: merchantID,
	})
	if err == nil {
		// text = sections.rekomendasi (string) fallback sections.plan
		// (string); both empty/absent → null (spec §7).
		var payload struct {
			Sections map[string]json.RawMessage `json:"sections"`
		}
		if err := json.Unmarshal(soap.FormData, &payload); err == nil {
			text, okR := clinicalSectionText(payload.Sections["rekomendasi"])
			if !okR {
				text, okR = clinicalSectionText(payload.Sections["plan"])
			}
			if okR {
				latestRecommendation = gin.H{
					"admission_id": uuidJSON(soap.AdmissionID),
					"signed_at":    tsJSON(soap.SignedAt),
					"text":         text,
				}
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"person":                gin.H{"id": uuidJSON(person.ID), "mrn": textJSON(person.MedicalRecordNo), "name": person.FullName, "gender": textJSON(person.Gender), "birth_date": dateJSON(person.BirthDate)},
		"allergies":             allergyItems,
		"active_diagnoses":      dxItems,
		"recent_visits":         visitItems,
		"latest_recommendation": latestRecommendation,
	}, "meta": gin.H{}})
}
