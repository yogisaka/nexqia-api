// internal/server/clinical_completeness.go
// Soft daily clinical record completeness indicator (plan
// 2026-10-21-cppt-rajal-b Task 3, spec 2026-10-21-cppt-rajal-fase1-design
// §11 / keputusan user 7). Pure read model: no blocking, no mutation. The
// board composes EXISTING per-domain queries (pattern clinical_timeline.go);
// only the admission-by-day list is new SQL.
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// completenessCountersignCategories — entries whose category demands a DPJP
// counter-sign after signing (spec §6.5: category-derived, no column).
var completenessCountersignCategories = map[string]bool{
	"keperawatan": true,
	"assessment":  true,
	"gizi":        true,
}

// Register route lives in RegisterClinicalRoutes (clinical_form.go).

// GetClinicalCompletenessHandler godoc
// @Summary Daily clinical record completeness board
// @Description Per-outpatient-visit soft completeness checks (nurse assessment, SOAP, diagnosis, resume, counter-sign). ?date=YYYY-MM-DD (merchant-local day) or ?admission_id=, one required (spec §11).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param date query string false "Merchant-local day, YYYY-MM-DD"
// @Param admission_id query string false "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/completeness [get]
func GetClinicalCompletenessHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()

	dateRaw := c.Query("date")
	admRaw := c.Query("admission_id")
	if dateRaw == "" && admRaw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "parameter date atau admission_id wajib"})
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}

	type completenessRow struct {
		id             pgtype.UUID
		visitNo        string
		personName     string
		departmentName string
	}
	var rows []completenessRow

	if admRaw != "" {
		id, okID := parseUUID(admRaw)
		if !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
			return
		}
		admission, err := q.GetAdmissionByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && admission.MerchantID != merchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		person, err := q.GetPersonByID(ctx, admission.PersonID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		departmentName := ""
		if admission.DepartmentID.Valid {
			department, err := q.GetDepartmentByID(ctx, admission.DepartmentID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
			departmentName = department.Name
		}
		rows = []completenessRow{{
			id: admission.ID, visitNo: admission.VisitNo,
			personName: person.FullName, departmentName: departmentName,
		}}
	} else {
		day, err := time.Parse("2006-01-02", dateRaw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date"})
			return
		}
		loc, err := merchantLocation(ctx, q, merchantID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		y, m, d := day.Date()
		dayStart := time.Date(y, m, d, 0, 0, 0, 0, loc)
		admissions, err := q.ListOutpatientAdmissionsByDay(ctx, sqlcgen.ListOutpatientAdmissionsByDayParams{
			MerchantID: merchantID,
			Column2:    pgtype.Timestamptz{Time: dayStart, Valid: true},
			Column3:    pgtype.Timestamptz{Time: dayStart.Add(24 * time.Hour), Valid: true},
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		rows = rows[:0]
		for _, a := range admissions {
			rows = append(rows, completenessRow{
				id: a.ID, visitNo: a.VisitNo,
				personName: a.PersonName, departmentName: a.DepartmentName,
			})
		}
	}

	// Category resolution per form_type comes from the resolved templates
	// (pattern clinical_timeline.go / ListClinicalFormsHandler).
	templates, err := q.ListClinicalFormTemplates(ctx, sqlcgen.ListClinicalFormTemplatesParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	category := map[string]string{}
	for _, t := range templates {
		if _, seen := category[t.FormType]; !seen {
			category[t.FormType] = t.Category
		}
	}

	out := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		forms, err := q.ListClinicalFormInstancesByAdmission(ctx, sqlcgen.ListClinicalFormInstancesByAdmissionParams{
			AdmissionID: row.id, Column2: false,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		diagnoses, err := q.ListAdmissionDiagnoses(ctx, row.id)
		if err != nil {
			respondInternalError(c, err)
			return
		}

		formCheck := func(formType, label string) gin.H {
			hasDraft := false
			for _, f := range forms {
				if f.FormType != formType {
					continue
				}
				if f.Status == "signed" {
					return gin.H{"key": formType, "label": label, "status": "complete"}
				}
				if f.Status == "draft" {
					hasDraft = true
				}
			}
			if hasDraft {
				return gin.H{"key": formType, "label": "belum sign", "status": "incomplete"}
			}
			return gin.H{"key": formType, "label": label, "status": "incomplete"}
		}

		dxStatus := "incomplete"
		if len(diagnoses) >= 1 {
			dxStatus = "complete"
		}

		// Counter-sign: any signed entry in a counter-sign category still
		// waiting (counter_signed_at NULL) → pending; otherwise complete.
		counterStatus := "complete"
		for _, f := range forms {
			if f.Status == "signed" && !f.CounterSignedAt.Valid &&
				completenessCountersignCategories[category[f.FormType]] {
				counterStatus = "pending"
				break
			}
		}

		checks := []gin.H{
			formCheck("ases_awal_perawat", "Asesmen awal perawat"),
			formCheck("soap_dokter", "SOAP dokter"),
			{"key": "diagnosis", "label": "Diagnosis", "status": dxStatus},
			formCheck("resume_rajal", "Resume rawat jalan"),
			{"key": "countersign", "label": "Counter-sign DPJP", "status": counterStatus},
		}
		overall := "lengkap"
		for _, chk := range checks {
			if s := chk["status"].(string); s != "complete" {
				overall = "belum_lengkap"
				break
			}
		}
		out = append(out, gin.H{
			"admission_id":    uuidJSON(row.id),
			"visit_no":        row.visitNo,
			"person_name":     row.personName,
			"department_name": row.departmentName,
			"checks":          checks,
			"overall":         overall,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}
