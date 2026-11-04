// internal/server/clinical_completeness.go
// Soft daily clinical record completeness indicator (plan
// 2026-10-21-cppt-rajal-b Task 3, spec 2026-10-21-cppt-rajal-fase1-design
// §11 / keputusan user 7). Pure read model: no blocking, no mutation. Fase 2a
// (plan 2026-11-04-clinical-board-fase2a-design §API-1) adds visit metadata
// columns, q/department_id/status filters and pagination; the per-check
// composition stays in Go via per-domain BATCHED queries (one uuid[] query
// per domain — a giant SQL join is forbidden).
package server

import (
	"errors"
	"net/http"
	"strings"
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

// completenessRow is one board row before check composition. physicianName /
// payerName stay pgtype.Text so a NULL id maps to JSON null naturally;
// departmentName keeps the fase-1 COALESCE(”) behavior.
type completenessRow struct {
	id              pgtype.UUID
	visitNo         string
	personID        pgtype.UUID
	personName      string
	mrn             pgtype.Text
	gender          pgtype.Text
	birthDate       pgtype.Date
	departmentID    pgtype.UUID
	departmentName  string
	physicianName   pgtype.Text
	admissionAt     pgtype.Timestamptz
	admissionStatus string
	payerName       pgtype.Text
}

// computeCompletenessChecks holds the UNCHANGED fase-1 per-check logic
// (spec 2026-10-21-cppt-rajal-fase1-design §11): forms per form_type,
// diagnosis presence, counter-sign pending on counter-sign categories.
func computeCompletenessChecks(forms []sqlcgen.ClinicalFormFormInstance, diagnoses []sqlcgen.ClinicalDiagnosis, category map[string]string) ([]gin.H, string) {
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
	return checks, overall
}

// GetClinicalCompletenessHandler godoc
// @Summary Daily clinical record completeness board
// @Description Per-outpatient-visit soft completeness checks (nurse assessment, SOAP, diagnosis, resume, counter-sign) with visit metadata. ?date=YYYY-MM-DD (merchant-local day) or ?admission_id=, one required; date mode supports ?q=, ?department_id=, ?status=lengkap|belum_lengkap|pending and ?limit/?offset (spec fase-2a §API-1).
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param date query string false "Merchant-local day, YYYY-MM-DD"
// @Param admission_id query string false "Admission UUID"
// @Param q query string false "Contains case-insensitive match on person name or MRN"
// @Param department_id query string false "Department UUID filter"
// @Param status query string false "lengkap | belum_lengkap | pending"
// @Param limit query int false "Page size (default 20, max 200)"
// @Param offset query int false "Page offset"
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

	limit, offset := paginationParamsDefault(c, 20)

	// Filters only apply to date mode; admission_id mode ignores them (spec §API-1).
	textFilter := strings.TrimSpace(c.Query("q"))
	var departmentFilter pgtype.UUID
	statusFilter := ""
	if admRaw == "" {
		if raw := strings.TrimSpace(c.Query("department_id")); raw != "" {
			var okDept bool
			departmentFilter, okDept = parseUUID(raw)
			if !okDept {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
				return
			}
		}
		statusFilter = c.Query("status")
		switch statusFilter {
		case "", "lengkap", "belum_lengkap", "pending":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
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
		// physician_name via core.physician → core.person.full_name; null when
		// physician_id NULL (guard pattern of GetDepartmentByID above).
		var physicianName pgtype.Text
		if admission.PhysicianID.Valid {
			physician, err := q.GetPhysicianByID(ctx, admission.PhysicianID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
			if err == nil {
				dok, err := q.GetPersonByID(ctx, physician.PersonID)
				if err != nil {
					respondInternalError(c, err)
					return
				}
				physicianName = pgtype.Text{String: dok.FullName, Valid: true}
			}
		}
		var payerName pgtype.Text
		if admission.PrimaryPayerID.Valid {
			payer, err := q.GetPayerByID(ctx, admission.PrimaryPayerID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
			if err == nil {
				payerName = pgtype.Text{String: payer.Name, Valid: true}
			}
		}
		rows = []completenessRow{{
			id: admission.ID, visitNo: admission.VisitNo, personID: person.ID,
			personName: person.FullName, mrn: person.MedicalRecordNo,
			gender: person.Gender, birthDate: person.BirthDate,
			departmentID: admission.DepartmentID, departmentName: departmentName,
			physicianName: physicianName, admissionAt: admission.AdmissionAt,
			admissionStatus: admission.Status, payerName: payerName,
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
			// q: contains case-insensitive over person_name OR mrn.
			if textFilter != "" {
				needle := strings.ToLower(textFilter)
				if !strings.Contains(strings.ToLower(a.PersonName), needle) &&
					!(a.Mrn.Valid && strings.Contains(strings.ToLower(a.Mrn.String), needle)) {
					continue
				}
			}
			if departmentFilter.Valid && a.DepartmentID != departmentFilter {
				continue
			}
			rows = append(rows, completenessRow{
				id: a.ID, visitNo: a.VisitNo, personID: a.PersonID,
				personName: a.PersonName, mrn: a.Mrn, gender: a.Gender,
				birthDate: a.BirthDate, departmentID: a.DepartmentID,
				departmentName: a.DepartmentName, physicianName: a.PhysicianName,
				admissionAt: a.AdmissionAt, admissionStatus: a.AdmissionStatus,
				payerName: a.PayerName,
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

	// Batched reads per domain (anti-N+1): forms and diagnoses of ALL filtered
	// rows in one uuid[] query each; grouping per admission stays in Go.
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.id)
	}
	formsAll, err := q.ListClinicalFormInstancesByAdmissionIDs(ctx, ids)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	formsBy := map[pgtype.UUID][]sqlcgen.ClinicalFormFormInstance{}
	for _, f := range formsAll {
		formsBy[f.AdmissionID] = append(formsBy[f.AdmissionID], f)
	}
	diagnosesAll, err := q.ListAdmissionDiagnosesByAdmissionIDs(ctx, ids)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	diagnosesBy := map[pgtype.UUID][]sqlcgen.ClinicalDiagnosis{}
	for _, dx := range diagnosesAll {
		diagnosesBy[dx.AdmissionID] = append(diagnosesBy[dx.AdmissionID], dx)
	}

	type composedRow struct {
		row     completenessRow
		checks  []gin.H
		overall string
	}
	composed := make([]composedRow, 0, len(rows))
	for _, row := range rows {
		checks, overall := computeCompletenessChecks(formsBy[row.id], diagnosesBy[row.id], category)
		composed = append(composed, composedRow{row: row, checks: checks, overall: overall})
	}

	// status semantics: lengkap/belum_lengkap by overall; pending = any check
	// pending — pending rows REMAIN part of belum_lengkap (pending ⊂ belum_lengkap).
	if statusFilter != "" {
		filtered := composed[:0]
		for _, cr := range composed {
			switch statusFilter {
			case "lengkap":
				if cr.overall != "lengkap" {
					continue
				}
			case "belum_lengkap":
				if cr.overall != "belum_lengkap" {
					continue
				}
			case "pending":
				pending := false
				for _, chk := range cr.checks {
					if chk["status"].(string) == "pending" {
						pending = true
						break
					}
				}
				if !pending {
					continue
				}
			}
			filtered = append(filtered, cr)
		}
		composed = filtered
	}

	total := len(composed)
	// Pagination is date-mode only (spec §API-1: admission_id ignores
	// filters/pagination) — admission_id mode always returns its single row.
	if admRaw == "" {
		if int(offset) < total {
			end := int(offset) + int(limit)
			if end > total {
				end = total
			}
			composed = composed[offset:end]
		} else {
			composed = nil
		}
	}

	// queue_number only for the ids on the page (batched DISTINCT ON; poli
	// beats loket, cancelled/skipped ignored, no ticket → null).
	queueBy := map[pgtype.UUID]string{}
	if len(composed) > 0 {
		pageIDs := make([]pgtype.UUID, 0, len(composed))
		for _, cr := range composed {
			pageIDs = append(pageIDs, cr.row.id)
		}
		tickets, err := q.ListLatestQueueTicketsByAdmissionIDs(ctx, pageIDs)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for _, t := range tickets {
			queueBy[t.AdmissionID] = t.QueueNumber
		}
	}

	out := make([]gin.H, 0, len(composed))
	for _, cr := range composed {
		queueNumber := any(nil)
		if n, okTicket := queueBy[cr.row.id]; okTicket {
			queueNumber = n
		}
		out = append(out, gin.H{
			"admission_id":     uuidJSON(cr.row.id),
			"visit_no":         cr.row.visitNo,
			"person_id":        uuidJSON(cr.row.personID),
			"person_name":      cr.row.personName,
			"mrn":              textJSON(cr.row.mrn),
			"gender":           textJSON(cr.row.gender),
			"birth_date":       dateJSON(cr.row.birthDate),
			"department_id":    uuidJSON(cr.row.departmentID),
			"department_name":  cr.row.departmentName,
			"physician_name":   textJSON(cr.row.physicianName),
			"admission_at":     tsJSON(cr.row.admissionAt),
			"admission_status": cr.row.admissionStatus,
			"payer_name":       textJSON(cr.row.payerName),
			"queue_number":     queueNumber,
			"checks":           cr.checks,
			"overall":          cr.overall,
		})
	}

	meta := gin.H{"limit": limit, "offset": offset, "total": total}
	if admRaw != "" {
		meta = gin.H{"limit": 1, "offset": 0, "total": 1}
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": meta})
}
