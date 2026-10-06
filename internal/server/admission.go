// internal/server/admission.go
package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/schedule"
)

func RegisterAdmissionRoutes(rg *gin.RouterGroup) {
	rg.POST("/admissions", CreateAdmissionHandler)
	rg.GET("/admissions", AccessLog("admission", "list", ""), ListAdmissionsHandler)
	rg.GET("/admissions/payer-summary", PayerSummaryHandler)
	rg.GET("/admissions/:id", AccessLog("admission", "view", "id"), GetAdmissionHandler)
	rg.PATCH("/admissions/:id", UpdateAdmissionHandler)
	// Physician leave (cuti) endpoints (spec 2026-10-01-b §3/§4) plus the
	// calendar CSV export and copy-week endpoints. server.go is outside this
	// task's Files list, so these register here; each handler enforces its own
	// permission via RequirePermissionForMerchant (schedule.manage for leave
	// and copy-week, visit.manage for the CSV/FO export).
	RegisterPhysicianLeaveRoutes(rg)
	RegisterScheduleCalendarExportRoutes(rg)
}

// uuidJSON renders an optional pgtype.UUID as a string (nil when NULL).
func uuidJSON(u pgtype.UUID) any {
	if !u.Valid {
		return nil
	}
	return u.String()
}

// tsJSON renders an optional pgtype.Timestamptz as RFC3339 (nil when NULL).
func tsJSON(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}

// textJSON renders an optional pgtype.Text as a string (nil when NULL).
func textJSON(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

// admissionJSON maps a raw sqlcgen.OperationsAdmission to a snake_case JSON
// object — the sqlcgen struct has no json tags, so returning it directly
// produced PascalCase keys.
func admissionJSON(a sqlcgen.OperationsAdmission) gin.H {
	return gin.H{
		"id":                   uuidJSON(a.ID),
		"company_id":           uuidJSON(a.CompanyID),
		"merchant_id":          uuidJSON(a.MerchantID),
		"visit_no":             a.VisitNo,
		"person_id":            uuidJSON(a.PersonID),
		"admission_type":       a.AdmissionType,
		"department_id":        uuidJSON(a.DepartmentID),
		"physician_id":         uuidJSON(a.PhysicianID),
		"ward_id":              uuidJSON(a.WardID),
		"bed_id":               uuidJSON(a.BedID),
		"primary_payer_id":     uuidJSON(a.PrimaryPayerID),
		"bpjs_sep_number":      textJSON(a.BpjsSepNumber),
		"status":               a.Status,
		"admission_at":         tsJSON(a.AdmissionAt),
		"discharge_at":         tsJSON(a.DischargeAt),
		"created_at":           tsJSON(a.CreatedAt),
		"created_by":           uuidJSON(a.CreatedBy),
		"updated_at":           tsJSON(a.UpdatedAt),
		"updated_by":           uuidJSON(a.UpdatedBy),
		"deleted_at":           tsJSON(a.DeletedAt),
		"deleted_by":           uuidJSON(a.DeletedBy),
		"row_version":          a.RowVersion,
		"complaint":            textJSON(a.Complaint),
		"referral_source":      textJSON(a.ReferralSource),
		"note":                 textJSON(a.Note),
		"diagnosis_text":       textJSON(a.DiagnosisText),
		"treatment_barriers":   textJSON(a.TreatmentBarriers),
		"special_patient_type": textJSON(a.SpecialPatientType),
		"needs_companion":      a.NeedsCompanion,
		"companion_name":       textJSON(a.CompanionName),
		"referral_origin":      textJSON(a.ReferralOrigin),
		"schedule_id":          uuidJSON(a.ScheduleID),
	}
}

// PayerSummaryHandler godoc
// @Summary Today's admission count grouped by payer type (dashboard "Jenis Pasien" widget)
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Router /admissions/payer-summary [get]
func PayerSummaryHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	summary, err := q.SummarizeAdmissionsByPayerTypeSince(c.Request.Context(), sqlcgen.SummarizeAdmissionsByPayerTypeSinceParams{
		MerchantID: merchantID, Since: pgtype.Timestamptz{Time: localDayStart(time.Now(), loc), Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": summary, "meta": gin.H{}})
}

type createAdmissionRequest struct {
	PersonID           string `json:"person_id" binding:"required"`
	DepartmentID       string `json:"department_id" binding:"required"`
	PhysicianID        string `json:"physician_id"`
	PrimaryPayerID     string `json:"primary_payer_id"`
	Complaint          string `json:"complaint"`
	ReferralSource     string `json:"referral_source"`
	Note               string `json:"note"`
	PolicyNumber       string `json:"policy_number"`
	GuarantorName      string `json:"guarantor_name"`
	DiagnosisText      string `json:"diagnosis_text"`
	TreatmentBarriers  string `json:"treatment_barriers"`
	SpecialPatientType string `json:"special_patient_type"`
	NeedsCompanion     bool   `json:"needs_companion"`
	CompanionName      string `json:"companion_name"`
	ReferralOrigin     string `json:"referral_origin"`
	ScheduleID         string `json:"schedule_id"`
	FlowID             string `json:"flow_id"`
}

// CreateAdmissionHandler is FO check-in (spec §4 point 1): registers the
// kunjungan (operations.admission, admission_type='outpatient') and its first
// queue row (queue_type='pendaftaran') atomically. If the queue insert fails
// after the admission insert already succeeded, c.Error(err) signals
// TenantMiddleware to roll back the whole request instead of committing an
// admission with no queue ticket — see plan's Global Constraints.
// CreateAdmissionHandler godoc
// @Summary Check in a patient (FO)
// @Description Registers the kunjungan (admission, outpatient) and its first queue row (pendaftaran) atomically.
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createAdmissionRequest true "Admission data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /admissions [post]
func CreateAdmissionHandler(c *gin.Context) {
	if !RequirePermission(c, PermVisitManage) {
		return
	}
	var req createAdmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	personID, ok := parseUUID(req.PersonID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	departmentID, ok := parseUUID(req.DepartmentID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
		return
	}
	var primaryPayerID pgtype.UUID
	if req.PrimaryPayerID != "" {
		id, ok := parseUUID(req.PrimaryPayerID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid primary_payer_id"})
			return
		}
		primaryPayerID = id
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))

	// Optional session pick (spec 2026-10-01-b §4): validate the schedule
	// pattern, replace the physician with the substitute when the session was
	// substituted, and enforce the payer quota. The pattern row is locked
	// FOR UPDATE so two concurrent registrations cannot both pass the quota
	// check. Without schedule_id the legacy behavior is unchanged.
	admissionScheduleID := pgtype.UUID{}
	effectivePhysician := optUUID(req.PhysicianID)
	if req.ScheduleID != "" {
		scheduleID, ok := parseUUID(req.ScheduleID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid schedule_id"})
			return
		}
		pattern, err := q.GetPhysicianScheduleForUpdate(ctx, scheduleID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "schedule does not match"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		reqPhysician := optUUID(req.PhysicianID)
		if pattern.MerchantID != merchantID || !pattern.IsActive ||
			pattern.DepartmentID != departmentID ||
			(reqPhysician.Valid && reqPhysician != pattern.PhysicianID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "schedule does not match"})
			return
		}
		loc, err := merchantLocation(c, q, merchantID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		now := time.Now().In(loc)
		y, m, d := now.Date()
		today := time.Date(y, m, d, 0, 0, 0, 0, loc)
		if int16(today.Weekday()) != pattern.DayOfWeek ||
			today.Before(pattern.EffectiveFrom.Time) ||
			(pattern.EffectiveTo.Valid && today.After(pattern.EffectiveTo.Time)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "schedule does not match"})
			return
		}
		session, err := q.GetScheduleSessionByScheduleAndDate(ctx, sqlcgen.GetScheduleSessionByScheduleAndDateParams{
			ScheduleID: scheduleID, SessionDate: pgtype.Date{Time: today, Valid: true},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			respondInternalError(c, err)
			return
		}
		quotaJkn, slotQuota := pattern.QuotaJkn, pattern.SlotQuota
		if err == nil {
			if session.Status == schedule.StatusLeave || session.Status == schedule.StatusCancelled {
				c.JSON(http.StatusConflict, gin.H{"error": "session quota full"})
				return
			}
			quotaJkn, slotQuota = session.QuotaJkn, session.SlotQuota
		}
		counts, err := q.CountScheduleAdmissionsForQuota(ctx, sqlcgen.CountScheduleAdmissionsForQuotaParams{
			ScheduleID: scheduleID, Column2: loc.String(),
			Column3: pgtype.Date{Time: today, Valid: true},
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		payerIsJKN := false
		if primaryPayerID.Valid {
			payerType, err := q.GetPayerType(ctx, primaryPayerID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
			payerIsJKN = payerType == "bpjs"
		}
		// Full = this admission would exceed the payer's pool. A pattern with
		// quota_jkn > 0 uses split pools (JKN against quota_jkn, others against
		// slot_quota); otherwise one combined pool (slot_quota). Zero quota =
		// unlimited (schedule.Status).
		full := false
		if quotaJkn > 0 {
			if payerIsJKN && counts.RegisteredJkn >= int64(quotaJkn) {
				full = true
			}
			if !payerIsJKN && slotQuota > 0 && counts.RegisteredOther >= int64(slotQuota) {
				full = true
			}
		} else if slotQuota > 0 && counts.RegisteredJkn+counts.RegisteredOther >= int64(slotQuota) {
			full = true
		}
		if full {
			c.JSON(http.StatusConflict, gin.H{"error": "session quota full"})
			return
		}
		admissionScheduleID = scheduleID
		effectivePhysician = pattern.PhysicianID
		if err == nil && session.Status == schedule.StatusSubstituted {
			effectivePhysician = session.PhysicianID
		}
	}

	department, err := q.GetDepartmentByID(ctx, departmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}

	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	dayStart, err := txDayStart(ctx, q, loc)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := lockDailySequence(ctx, q, merchantID, "visit:"+departmentID.String(), dayStart); err != nil {
		respondInternalError(c, err)
		return
	}
	admissionSeq, err := q.CountAdmissionsByDepartmentSince(ctx, sqlcgen.CountAdmissionsByDepartmentSinceParams{
		MerchantID: merchantID, DepartmentID: departmentID, Since: pgtype.Timestamptz{Time: dayStart, Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	visitNo := fmt.Sprintf("%s%s-%04d", department.Code, dayStart.Format("20060102"), admissionSeq+1)

	admission, err := q.CreateAdmission(ctx, sqlcgen.CreateAdmissionParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, VisitNo: visitNo, PersonID: personID,
		AdmissionType: "outpatient", DepartmentID: departmentID, PhysicianID: effectivePhysician,
		PrimaryPayerID: primaryPayerID, Complaint: optText(req.Complaint),
		ReferralSource: optText(req.ReferralSource), Note: optText(req.Note),
		CreatedBy: AuthUserID(c), ScheduleID: admissionScheduleID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}

	// Second write in the request: admission already inserted, so any failure
	// here must c.Error(err) to make TenantMiddleware roll back the whole tx
	// rather than commit an admission without its guarantor row.
	if primaryPayerID.Valid {
		if _, err := q.CreateAdmissionGuarantor(ctx, sqlcgen.CreateAdmissionGuarantorParams{
			AdmissionID: admission.ID, PayerID: primaryPayerID,
			PolicyNumber: optText(req.PolicyNumber), GuarantorName: optText(req.GuarantorName),
			CreatedBy: AuthUserID(c),
		}); err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
	}

	// Flow-driven first ticket (spec 2026-10-01-c §4): resolve the merchant's
	// queue flow for this payer/department (auto-provisioning the built-in
	// default flow when the merchant has none), create the journey, and issue
	// the first stage's ticket. These writes come after the admission insert,
	// so every failure must c.Error(err) to roll the whole request back.
	payerType := ""
	if primaryPayerID.Valid {
		payerType, err = q.GetPayerType(ctx, primaryPayerID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
	}
	var explicitFlowID pgtype.UUID
	if req.FlowID != "" {
		fid, ok := parseUUID(req.FlowID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid flow_id"})
			return
		}
		explicitFlowID = fid
	}
	flow, stages, err := resolveFlow(ctx, q, merchantID, departmentID, payerType, explicitFlowID)
	if errors.Is(err, errFlowNotApplies) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, errNoQueueFlow) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	journey, err := q.CreateQueueJourney(ctx, sqlcgen.CreateQueueJourneyParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, AdmissionID: admission.ID, PersonID: personID,
		FlowID: flow.ID, CurrentStageID: stages[0].ID, Status: "in_progress", CreatedBy: AuthUserID(c),
	})
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	queue, err := issueTicket(ctx, q, &journey, stages[0], pgtype.UUID{})
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"admission": admissionJSON(admission), "queue": queue, "journey": journey}, "meta": gin.H{}})
}

// ListAdmissionsHandler godoc
// @Summary List admissions for a work queue
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param department_id query string true "Department UUID"
// @Param status query string true "Admission status"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /admissions [get]
func ListAdmissionsHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	departmentID, ok := parseUUID(c.Query("department_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department_id query param is required"})
		return
	}
	status := c.Query("status")
	if status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status query param is required"})
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	admissions, err := q.ListAdmissionsForWork(c.Request.Context(), sqlcgen.ListAdmissionsForWorkParams{
		MerchantID: merchantID, DepartmentID: departmentID, Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admissions, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetAdmissionHandler godoc
// @Summary Get an admission by id
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id} [get]
func GetAdmissionHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, err := q.GetAdmissionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, admission.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admissionJSON(admission), "meta": gin.H{}})
}

// updateAdmissionRequest is deliberately generic (physician reassignment,
// status, discharge_at) — "discharge" as a business action belongs to
// sub-project #3 (CPPT/resume), this endpoint just exposes the column writes
// it will need. See spec §2.
type updateAdmissionRequest struct {
	PhysicianID string `json:"physician_id"`
	Status      string `json:"status" binding:"required"`
	DischargeAt string `json:"discharge_at"`
}

// UpdateAdmissionHandler godoc
// @Summary Update an admission
// @Description Generic column writes (physician reassignment, status, discharge_at) — "discharge" as a business action belongs to a future CPPT/resume sub-project.
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body updateAdmissionRequest true "Admission data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id} [patch]
func UpdateAdmissionHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetAdmissionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	var req updateAdmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var dischargeAt pgtype.Timestamptz
	if req.DischargeAt != "" {
		t, err := time.Parse(time.RFC3339, req.DischargeAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid discharge_at, expected RFC3339"})
			return
		}
		dischargeAt = pgtype.Timestamptz{Time: t, Valid: true}
	}
	admission, err := q.UpdateAdmission(c.Request.Context(), sqlcgen.UpdateAdmissionParams{
		ID: id, PhysicianID: optUUID(req.PhysicianID), Status: req.Status, DischargeAt: dischargeAt, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admissionJSON(admission), "meta": gin.H{}})
}
