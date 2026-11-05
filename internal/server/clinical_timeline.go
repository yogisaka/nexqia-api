// internal/server/clinical_timeline.go
// Composed clinical timeline read model per admission (plan
// 2026-10-21-cppt-rajal-b Task 2, spec 2026-10-21-cppt-rajal-fase1-design §9):
// one admission's person identity, allergies, diagnoses, nutrition orders and
// clinical form entries merged into a newest-first item feed. Composition runs
// per-domain queries — a single giant SQL join is forbidden (spec §9).
package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// timelineItem is one merged entry of the newest-first `items` feed.
type timelineItem struct {
	at   time.Time
	data gin.H
}

// GetClinicalTimelineHandler godoc
// @Summary Composed clinical timeline of one admission
// @Description Admission + person + active allergies + diagnoses + nutrition orders + form entries, items merged newest first (spec §9).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id}/clinical-timeline [get]
func GetClinicalTimelineHandler(c *gin.Context) {
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
			"created_at":      tsJSON(d.CreatedAt),
		})
	}

	orders, err := q.ListNutritionOrdersByAdmissionWithDiet(ctx, id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	orderJSONs := make([]gin.H, 0, len(orders))
	for _, o := range orders {
		orderJSONs = append(orderJSONs, nutritionOrderJSON(o))
	}

	// Form entries with their category from the resolved templates (pattern
	// ListClinicalFormsHandler). Gizi-category entries are NOT filtered here:
	// the timeline viewer must hold clinical.record.view only, and gizi forms
	// stay visible per the composed read-model contract.
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
	forms, err := q.ListClinicalFormInstancesByAdmission(ctx, sqlcgen.ListClinicalFormInstancesByAdmissionParams{
		AdmissionID: id, Column2: false,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Person documents linked to this admission (berkas RM revisi Task 3,
	// spec 2026-11-05-berkas-rm-revisi-design Q4): media_link rows with
	// purpose 'berkas_rm' resolve to the person's active documents.
	docs, err := q.ListAdmissionDocuments(ctx, sqlcgen.ListAdmissionDocumentsParams{
		OwnerID: id, PersonID: admission.PersonID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]timelineItem, 0, len(forms)+len(orders)+len(docs))
	for _, f := range forms {
		at := f.FilledAt
		if !at.Valid {
			at = f.CreatedAt
		}
		items = append(items, timelineItem{at: at.Time, data: gin.H{
			"kind":              "form",
			"id":                uuidJSON(f.ID),
			"form_type":         f.FormType,
			"category":          category[f.FormType],
			"status":            f.Status,
			"filled_at":         tsJSON(f.FilledAt),
			"signed_at":         tsJSON(f.SignedAt),
			"filled_by":         uuidJSON(f.FilledBy),
			"counter_signed_at": tsJSON(f.CounterSignedAt),
			"addendum_to_id":    uuidJSON(f.AddendumToID),
		}})
	}
	for _, o := range orders {
		items = append(items, timelineItem{at: o.CreatedAt.Time, data: gin.H{
			"kind":           "nutrition_order",
			"id":             uuidJSON(o.ID),
			"created_at":     tsJSON(o.CreatedAt),
			"status":         o.Status,
			"diet_type_name": o.DietTypeName,
		}})
	}
	for _, d := range docs {
		items = append(items, timelineItem{at: d.CreatedAt.Time, data: gin.H{
			"kind":               "person_document",
			"id":                 uuidJSON(d.ID),
			"document_type":      d.DocumentType,
			"document_type_name": d.DocumentTypeName,
			"label":              d.Label,
			"filename":           d.Filename,
			"size_bytes":         d.SizeBytes,
			"created_by_name":    d.CreatedByName,
			"created_at":         tsJSON(d.CreatedAt),
		}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	feed := make([]gin.H, 0, len(items))
	for _, it := range items {
		feed = append(feed, it.data)
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"admission":        admissionJSON(admission),
		"person":           gin.H{"id": uuidJSON(person.ID), "mrn": textJSON(person.MedicalRecordNo), "name": person.FullName, "gender": textJSON(person.Gender), "birth_date": dateJSON(person.BirthDate)},
		"allergies":        allergyItems,
		"diagnoses":        dxItems,
		"nutrition_orders": orderJSONs,
		"items":            feed,
	}, "meta": gin.H{}})
}
