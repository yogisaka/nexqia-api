// internal/server/physician_leave.go
// Physician leave (cuti) with per-session marking, affected patients and
// per-patient resolution — spec 2026-10-01-b-physician-schedule-design §3/§4.
package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/schedule"
)

// RegisterPhysicianLeaveRoutes registers the physician-leave endpoints.
// Registered from RegisterAdmissionRoutes (admission.go, inside this task's
// Files list) because server.go is outside it — plan Task 4.
func RegisterPhysicianLeaveRoutes(rg *gin.RouterGroup) {
	rg.POST("/physician-leaves", CreatePhysicianLeaveHandler)
	rg.GET("/physician-leaves", ListPhysicianLeavesHandler)
	rg.GET("/physician-leaves/:id/affected", ListPhysicianLeaveAffectedHandler)
	rg.DELETE("/physician-leaves/:id", DeletePhysicianLeaveHandler)
	rg.POST("/physician-leaves/:id/affected/:admission_id/resolve", ResolveLeaveAffectedHandler)
}

type createPhysicianLeaveRequest struct {
	MerchantID            string `json:"merchant_id" binding:"required"`
	PhysicianID           string `json:"physician_id" binding:"required"`
	DateFrom              string `json:"date_from" binding:"required"`
	DateTo                string `json:"date_to" binding:"required"`
	Reason                string `json:"reason"`
	SubstitutePhysicianID string `json:"substitute_physician_id"`
}

// CreatePhysicianLeaveHandler godoc
// @Summary Create a physician leave, mark its sessions and list affected patients
// @Tags physician_leave
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPhysicianLeaveRequest true "Leave data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /physician-leaves [post]
func CreatePhysicianLeaveHandler(c *gin.Context) {
	var req createPhysicianLeaveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	physicianID, ok := parseUUID(req.PhysicianID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid physician_id"})
		return
	}
	dateFrom, ok := parseScheduleDate(req.DateFrom)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date_from, expected YYYY-MM-DD"})
		return
	}
	dateTo, ok := parseScheduleDate(req.DateTo)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date_to, expected YYYY-MM-DD"})
		return
	}
	if dateTo.Before(dateFrom) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "date_to must be on or after date_from"})
		return
	}
	var substituteID pgtype.UUID
	if req.SubstitutePhysicianID != "" {
		id, ok := parseUUID(req.SubstitutePhysicianID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid substitute_physician_id"})
			return
		}
		substituteID = id
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	if substituteID.Valid {
		if _, err := q.GetMerchantPhysician(ctx, sqlcgen.GetMerchantPhysicianParams{
			ID: substituteID, MerchantID: merchantID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "substitute physician not found in merchant"})
				return
			}
			respondInternalError(c, err)
			return
		}
		// The substitute must not have overlapping sessions on the sessions
		// being replaced (spec §4) — 409 with the list of conflict dates.
		if !checkSubstituteConflicts(c, q, merchantID, physicianID, substituteID, dateFrom, dateTo) {
			return
		}
	}

	leave, err := q.CreatePhysicianLeave(ctx, sqlcgen.CreatePhysicianLeaveParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		PhysicianID: physicianID,
		DateFrom:    pgtype.Date{Time: dateFrom, Valid: true},
		DateTo:      pgtype.Date{Time: dateTo, Valid: true},
		Reason:      optText(req.Reason), SubstitutePhysicianID: substituteID,
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}

	// Mark every session of the physician in the range (projected + stored):
	// substituted (+ physician = substitute) when there is a substitute, else
	// leave. A write already happened, so any failure aborts the transaction.
	status := schedule.StatusLeave
	effPhysician := physicianID
	if substituteID.Valid {
		status = schedule.StatusSubstituted
		effPhysician = substituteID
	}
	data, err := computeCalendarData(c, q, merchantID, dateFrom, dateTo)
	if err != nil {
		abortInternalError(c, err)
		return
	}
	for _, s := range data.sessions {
		if s.PhysicianID != physicianID.String() || s.Status == schedule.StatusCancelled {
			continue
		}
		sessionScheduleID, ok := parseUUID(s.ScheduleID)
		if !ok {
			abortInternalError(c, errors.New("invalid schedule id in projection"))
			return
		}
		if _, err := q.UpsertScheduleSessionStatus(ctx, sqlcgen.UpsertScheduleSessionStatusParams{
			SessionDate:          pgtype.Date{Time: s.Date, Valid: true},
			EffectivePhysicianID: effPhysician,
			Status:               status,
			LeaveID:              pgtype.UUID{Bytes: leave.ID.Bytes, Valid: true},
			UserID:               AuthUserID(c),
			ScheduleID:           sessionScheduleID,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
	}

	affected, err := listLeaveAffected(c, q, merchantID, physicianID, dateFrom, dateTo)
	if err != nil {
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": leaveResponse(leave, affected), "meta": gin.H{}})
}

// checkSubstituteConflicts rejects a substitute who already has sessions
// overlapping the leave physician's sessions in the range. Returns false when
// it already wrote the 409 response.
func checkSubstituteConflicts(c *gin.Context, q *sqlcgen.Queries, merchantID, physicianID, substituteID pgtype.UUID, from, to time.Time) bool {
	data, err := computeCalendarData(c, q, merchantID, from, to)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	conflictDates := map[string]bool{}
	for _, s := range data.sessions {
		if s.PhysicianID != physicianID.String() || s.Status == schedule.StatusCancelled {
			continue
		}
		candidate := s
		candidate.PhysicianID = substituteID.String()
		candidate.Status = schedule.StatusAvailable
		for _, other := range data.sessions {
			if other.ScheduleID == s.ScheduleID || other.PhysicianID != substituteID.String() {
				continue
			}
			if schedule.Overlaps(candidate, other) {
				conflictDates[s.Date.Format("2006-01-02")] = true
			}
		}
	}
	if len(conflictDates) == 0 {
		return true
	}
	dates := make([]string, 0, len(conflictDates))
	for d := range conflictDates {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	c.JSON(http.StatusConflict, gin.H{
		"error": "substitute physician has overlapping sessions",
		"dates": dates,
	})
	return false
}

// listLeaveAffected builds the "pasien terdampak" list for a physician over a
// date range (spec §4: id, patient, date, session, payer).
func listLeaveAffected(c *gin.Context, q *sqlcgen.Queries, merchantID, physicianID pgtype.UUID, from, to time.Time) ([]gin.H, error) {
	rows, err := q.ListLeaveAffectedAdmissions(c.Request.Context(), sqlcgen.ListLeaveAffectedAdmissionsParams{
		MerchantID:  merchantID,
		PhysicianID: physicianID,
		Tz:          tzName(c, q, merchantID),
		DateFrom:    pgtype.Date{Time: from, Valid: true},
		DateTo:      pgtype.Date{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":      r.ID,
			"patient": gin.H{"id": r.PersonID, "name": r.PatientName},
			"date":    r.VisitDate.Time.Format("2006-01-02"),
			"session": gin.H{
				"start_time": formatTimeOfDay(r.SessionStart),
				"end_time":   formatTimeOfDay(r.SessionEnd),
			},
			"payer": gin.H{"name": r.PayerName, "type": r.PayerType},
		})
	}
	return out, nil
}

func leaveResponse(l sqlcgen.OperationsPhysicianLeave, affected []gin.H) gin.H {
	return gin.H{
		"id":                      l.ID,
		"merchant_id":             l.MerchantID,
		"physician_id":            l.PhysicianID,
		"date_from":               l.DateFrom,
		"date_to":                 l.DateTo,
		"reason":                  l.Reason,
		"substitute_physician_id": l.SubstitutePhysicianID,
		"created_at":              l.CreatedAt,
		"affected":                affected,
	}
}

// ListPhysicianLeavesHandler godoc
// @Summary List physician leaves of a merchant in a date range
// @Tags physician_leave
// @Produce json
// @Security BearerAuth
// @Param merchant_id query string true "Merchant UUID"
// @Param from query string false "Range start YYYY-MM-DD"
// @Param to query string false "Range end YYYY-MM-DD"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /physician-leaves [get]
func ListPhysicianLeavesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Query("merchant_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant_id query param is required"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	var fromFilter, toFilter pgtype.Date
	if raw := c.Query("from"); raw != "" {
		d, ok := parseScheduleDate(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from, expected YYYY-MM-DD"})
			return
		}
		fromFilter = pgtype.Date{Time: d, Valid: true}
	}
	if raw := c.Query("to"); raw != "" {
		d, ok := parseScheduleDate(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to, expected YYYY-MM-DD"})
			return
		}
		toFilter = pgtype.Date{Time: d, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	leaves, err := q.ListPhysicianLeaves(c.Request.Context(), sqlcgen.ListPhysicianLeavesParams{
		MerchantID: merchantID, Column2: fromFilter, Column3: toFilter,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, len(leaves))
	for i, l := range leaves {
		out[i] = leaveResponse(l, nil)
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}

// loadLeave fetches the leave and enforces operations.schedule.manage on its
// merchant. Returns false when it already wrote the response.
func loadLeave(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.OperationsPhysicianLeave, bool) {
	leave, err := q.GetPhysicianLeaveByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician leave not found"})
		return sqlcgen.OperationsPhysicianLeave{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsPhysicianLeave{}, false
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, leave.MerchantID) {
		return sqlcgen.OperationsPhysicianLeave{}, false
	}
	return leave, true
}

// ListPhysicianLeaveAffectedHandler godoc
// @Summary List the patients affected by a leave
// @Tags physician_leave
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician leave UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physician-leaves/{id}/affected [get]
func ListPhysicianLeaveAffectedHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	leave, ok := loadLeave(c, q, id)
	if !ok {
		return
	}
	affected, err := listLeaveAffected(c, q, leave.MerchantID, leave.PhysicianID,
		leave.DateFrom.Time, leave.DateTo.Time)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"leave_id": leave.ID, "affected": affected}, "meta": gin.H{}})
}

// DeletePhysicianLeaveHandler godoc
// @Summary Cancel a leave and restore its sessions to the pattern physician
// @Tags physician_leave
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician leave UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /physician-leaves/{id} [delete]
func DeletePhysicianLeaveHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	leave, ok := loadLeave(c, q, id)
	if !ok {
		return
	}
	sessions, err := q.ListLeaveScheduleSessions(c.Request.Context(), leave.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Restore first; every failure from here on aborts the request
	// transaction (a write already happened in it).
	if _, err := q.RestoreLeaveSessions(c.Request.Context(), sqlcgen.RestoreLeaveSessionsParams{
		LeaveID: leave.ID, UpdatedBy: AuthUserID(c),
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	tz := tzName(c, q, leave.MerchantID)
	for _, ss := range sessions {
		if _, err := q.DeleteSessionIfMatchesPattern(c.Request.Context(), sqlcgen.DeleteSessionIfMatchesPatternParams{
			SessionID: ss.ID, Tz: tz,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
	}
	if err := q.SoftDeletePhysicianLeave(c.Request.Context(), sqlcgen.SoftDeletePhysicianLeaveParams{
		ID: leave.ID, DeletedBy: AuthUserID(c),
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type resolveLeaveAffectedRequest struct {
	Action     string `json:"action" binding:"required"`
	TargetDate string `json:"target_date"`
}

// ResolveLeaveAffectedHandler godoc
// @Summary Resolve one affected admission of a leave (substitute/reschedule/cancel)
// @Tags physician_leave
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician leave UUID"
// @Param admission_id path string true "Admission UUID"
// @Param request body resolveLeaveAffectedRequest true "Action"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physician-leaves/{id}/affected/{admission_id}/resolve [post]
func ResolveLeaveAffectedHandler(c *gin.Context) {
	leaveID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	admissionID, ok := parseUUID(c.Param("admission_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	var req resolveLeaveAffectedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	leave, ok := loadLeave(c, q, leaveID)
	if !ok {
		return
	}
	admission, err := q.GetAdmissionByID(c.Request.Context(), admissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// The admission must be part of this leave: same physician's pattern and
	// a visit date inside the leave range, else 404 (spec §4).
	loc, err := merchantLocation(c, q, leave.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	partOf := admission.ScheduleID.Valid
	if partOf {
		schedule, err := q.GetPhysicianScheduleByID(c.Request.Context(), admission.ScheduleID)
		if errors.Is(err, pgx.ErrNoRows) {
			partOf = false
		} else if err != nil {
			respondInternalError(c, err)
			return
		} else {
			partOf = schedule.PhysicianID == leave.PhysicianID
		}
	}
	if partOf {
		visitDate := admission.AdmissionAt.Time.In(loc)
		y, m, d := visitDate.Date()
		visitDate = time.Date(y, m, d, 0, 0, 0, 0, loc)
		// The leave bounds are midnight-UTC pgtype.Date values; normalize them
		// to merchant-local midnights so the range matches the local visit
		// date (2026-10-05T00:00Z is 2026-10-04 17:00 WIB).
		from := leave.DateFrom.Time.In(loc)
		y, m, d = from.Date()
		from = time.Date(y, m, d, 0, 0, 0, 0, loc)
		to := leave.DateTo.Time.In(loc)
		y, m, d = to.Date()
		to = time.Date(y, m, d, 0, 0, 0, 0, loc)
		partOf = !visitDate.Before(from) && !visitDate.After(to)
	}
	if !partOf {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission is not part of this leave"})
		return
	}

	var physician pgtype.UUID
	var note pgtype.Text
	status := ""
	switch req.Action {
	case "substitute":
		if !leave.SubstitutePhysicianID.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "leave has no substitute physician"})
			return
		}
		physician = leave.SubstitutePhysicianID
	case "reschedule":
		target, ok := parseScheduleDate(req.TargetDate)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_date is required for reschedule, expected YYYY-MM-DD"})
			return
		}
		status = "cancelled"
		note = pgtype.Text{String: "dijadwal ulang ke " + target.Format("2006-01-02"), Valid: true}
	case "cancel":
		status = "cancelled"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be substitute, reschedule or cancel"})
		return
	}
	updated, err := q.ResolveLeaveAdmission(c.Request.Context(), sqlcgen.ResolveLeaveAdmissionParams{
		AdmissionID: admissionID, PhysicianID: physician, Status: status, Note: note,
		UserID: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": updated, "meta": gin.H{}})
}
