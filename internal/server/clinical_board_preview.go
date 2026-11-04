// internal/server/clinical_board_preview.go
// Fase 2a clinical board preview (plan 2026-11-04-clinical-board-fase2a-plan
// Task 2, spec 2026-11-04-clinical-board-fase2a-design §API-2): small read
// model per admission for the board sheet — person identity, last SOAP
// (author + excerpt), diagnoses and allergies. Composition reuses the
// existing per-domain queries; a single giant SQL join is forbidden (K2).
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// previewExcerpt returns the first non-empty string of
// form_data.sections.subjective, then .plan; truncated to 160 chars with an
// ellipsis when longer; not-a-string or empty → nil (spec §API-2). Parsing
// follows the form_data pattern of clinical_form.go.
func previewExcerpt(formData []byte) any {
	var payload struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(formData, &payload); err != nil {
		return nil
	}
	for _, key := range []string{"subjective", "plan"} {
		raw, ok := payload.Sections[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue
		}
		if len(s) > 160 {
			s = s[:160] + "…"
		}
		return s
	}
	return nil
}

// GetClinicalBoardPreviewHandler godoc
// @Summary Small clinical preview read model of one admission (board sheet)
// @Description Person + last SOAP (at/author_name/excerpt) + diagnoses + allergies (spec §API-2).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id}/clinical-preview [get]
func GetClinicalBoardPreviewHandler(c *gin.Context) {
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
	admission, err := q.GetAdmissionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && admission.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}

	person, err := q.GetPersonByID(ctx, admission.PersonID)
	if err != nil {
		respondInternalError(c, err)
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

	diagnoses, err := q.ListAdmissionDiagnoses(ctx, id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	dxItems := make([]gin.H, 0, len(diagnoses))
	for _, d := range diagnoses {
		dxItems = append(dxItems, gin.H{
			"dx_role":         d.DxRole,
			"primary_system":  d.PrimarySystem,
			"primary_code":    d.PrimaryCode,
			"primary_display": d.PrimaryDisplay,
			"icd10_code":      textJSON(d.Icd10Code),
			"icd10_display":   textJSON(d.Icd10Display),
			"icd10_source":    textJSON(d.Icd10Source),
		})
	}

	forms, err := q.ListClinicalFormInstancesByAdmission(ctx, sqlcgen.ListClinicalFormInstancesByAdmissionParams{
		AdmissionID: id, Column2: false,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Latest soap_dokter entry: filled_at NULLS LAST, created_at DESC (spec
	// §API-2; any status — drafts are informative in the preview).
	var last sqlcgen.ClinicalFormFormInstance
	found := false
	newer := func(a, b sqlcgen.ClinicalFormFormInstance) bool {
		if a.FilledAt.Valid != b.FilledAt.Valid {
			return a.FilledAt.Valid
		}
		if a.FilledAt.Valid && !a.FilledAt.Time.Equal(b.FilledAt.Time) {
			return a.FilledAt.Time.After(b.FilledAt.Time)
		}
		return a.CreatedAt.Time.After(b.CreatedAt.Time)
	}
	for _, f := range forms {
		if f.FormType != "soap_dokter" {
			continue
		}
		if !found || newer(f, last) {
			last, found = f, true
		}
	}
	var lastSOAP gin.H
	if found {
		at := last.FilledAt
		if !at.Valid {
			at = last.CreatedAt
		}
		var authorName any
		if last.FilledBy.Valid {
			author, err := q.GetUserDisplayName(ctx, last.FilledBy)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
			if err == nil {
				authorName = author
			}
		}
		lastSOAP = gin.H{
			"at":          tsJSON(at),
			"author_name": authorName,
			"excerpt":     previewExcerpt(last.FormData),
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"person": gin.H{
			"id":         uuidJSON(person.ID),
			"mrn":        textJSON(person.MedicalRecordNo),
			"name":       person.FullName,
			"gender":     textJSON(person.Gender),
			"birth_date": dateJSON(person.BirthDate),
		},
		"last_soap": lastSOAP,
		"diagnoses": dxItems,
		"allergies": allergyItems,
	}, "meta": gin.H{}})
}
