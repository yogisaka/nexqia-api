// internal/server/physician_schedule.go
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

// PermScheduleManage guards operations.physician_schedule CRUD — jadwal praktik
// dokter, template mingguan berulang (day_of_week + effective_from/to), lihat
// 2026-09-16-v1-operations-antrian-jadwal-design.md §2.
const PermScheduleManage = "operations.schedule.manage"

func RegisterPhysicianScheduleRoutes(rg *gin.RouterGroup) {
	rg.POST("/physician-schedules", CreatePhysicianScheduleHandler)
	rg.GET("/merchants/:id/physician-schedules", ListPhysicianSchedulesHandler)
	rg.GET("/physician-schedules/:id", GetPhysicianScheduleHandler)
	rg.PATCH("/physician-schedules/:id", UpdatePhysicianScheduleHandler)
	rg.DELETE("/physician-schedules/:id", DeletePhysicianScheduleHandler)
	// Calendar, summary, session overrides and settings (spec
	// 2026-10-01-b-physician-schedule-design §4).
	rg.GET("/merchants/:id/schedule/calendar", ScheduleCalendarHandler)
	rg.GET("/merchants/:id/schedule/summary", ScheduleSummaryHandler)
	rg.GET("/merchants/:id/schedule/settings", GetScheduleSettingsHandler)
	rg.PUT("/merchants/:id/schedule/settings", UpdateScheduleSettingsHandler)
	rg.PUT("/schedule/sessions/:schedule_id/:date", UpsertScheduleSessionHandler)
	rg.DELETE("/schedule/sessions/:schedule_id/:date", DeleteScheduleSessionHandler)
}

// parseTimeOfDay parses "HH:MM" into pgtype.Time (microseconds since midnight —
// pgtype.Time has no time.Time-based representation, see pgx/v5/pgtype/time.go).
func parseTimeOfDay(s string) (pgtype.Time, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return pgtype.Time{}, err
	}
	us := int64(t.Hour())*3600e6 + int64(t.Minute())*60e6
	return pgtype.Time{Microseconds: us, Valid: true}, nil
}

// formatTimeOfDay is parseTimeOfDay's inverse — pgtype.Time has no MarshalJSON
// (unlike pgtype.Date/UUID/Timestamptz), so responses must format it explicitly.
func formatTimeOfDay(t pgtype.Time) string {
	if !t.Valid {
		return ""
	}
	total := t.Microseconds / 1_000_000
	return fmt.Sprintf("%02d:%02d", total/3600, (total%3600)/60)
}

// checkScheduleRanges rejects inverted ranges before any write: end_time must
// be strictly after start_time and effective_to (when set) on or after
// effective_from. Shared by the create/update schedule handlers.
func checkScheduleRanges(c *gin.Context, start, end pgtype.Time, effectiveFrom, effectiveTo pgtype.Date) bool {
	if end.Microseconds <= start.Microseconds {
		c.JSON(http.StatusBadRequest, gin.H{"error": "end_time must be after start_time"})
		return false
	}
	if effectiveTo.Valid && effectiveTo.Time.Before(effectiveFrom.Time) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "effective_to must be on or after effective_from"})
		return false
	}
	return true
}

// toScheduleResponse maps a generated row to a JSON-friendly shape with
// start_time/end_time as "HH:MM" strings. Reused by display.go.
func toScheduleResponse(s sqlcgen.OperationsPhysicianSchedule) gin.H {
	return gin.H{
		"id":                  s.ID,
		"company_id":          s.CompanyID,
		"merchant_id":         s.MerchantID,
		"physician_id":        s.PhysicianID,
		"department_id":       s.DepartmentID,
		"day_of_week":         s.DayOfWeek,
		"start_time":          formatTimeOfDay(s.StartTime),
		"end_time":            formatTimeOfDay(s.EndTime),
		"slot_quota":          s.SlotQuota,
		"effective_from":      s.EffectiveFrom,
		"effective_to":        s.EffectiveTo,
		"is_active":           s.IsActive,
		"created_at":          s.CreatedAt,
		"updated_at":          s.UpdatedAt,
		"room_id":             s.RoomID,
		"quota_jkn":           s.QuotaJkn,
		"minutes_per_patient": s.MinutesPerPatient,
		"service_types":       s.ServiceTypes,
		"shift_id":            s.ShiftID,
		"notes":               s.Notes,
	}
}

// resolveScheduleRoom validates the pattern's room_id: it must be a
// core.location of the same merchant with kind='room' carrying the 'practice'
// function (spec §3). Writes 400 and returns false when invalid; an empty
// value (legacy data) passes as NULL.
func resolveScheduleRoom(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, raw string) (pgtype.UUID, bool) {
	if raw == "" {
		return pgtype.UUID{}, true
	}
	roomID, ok := parseUUID(raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid room_id"})
		return pgtype.UUID{}, false
	}
	loc, err := q.GetLocationByID(c.Request.Context(), roomID)
	if err != nil || loc.MerchantID != merchantID || loc.Kind != "room" || !locationHasFunction(loc, "practice") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "room must be a practice room"})
		return pgtype.UUID{}, false
	}
	return roomID, true
}

func locationHasFunction(loc sqlcgen.CoreLocation, fn string) bool {
	for _, f := range loc.Functions {
		if f == fn {
			return true
		}
	}
	return false
}

// scheduleConflict is one entry of the 409 conflicts list.
type scheduleConflict struct {
	ScheduleID string `json:"schedule_id"`
	Reason     string `json:"reason"` // room | physician
}

// checkPatternConflicts rejects a create/update whose candidate pattern
// overlaps another active pattern of the same merchant on the same weekday
// (schedule.PatternOverlap). excludeID skips the pattern being updated.
// Returns false when it already wrote the 409 response.
func checkPatternConflicts(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, candidate schedule.Pattern, excludeID string) bool {
	rows, err := q.ListSchedulesByMerchantAndDay(c.Request.Context(), sqlcgen.ListSchedulesByMerchantAndDayParams{
		MerchantID: merchantID,
		DayOfWeek:  int16(candidate.DayOfWeek),
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	for _, r := range rows {
		if excludeID != "" && r.ID.String() == excludeID {
			continue
		}
		other := schedule.Pattern{
			ID: r.ID.String(), PhysicianID: r.PhysicianID.String(), RoomID: r.RoomID.String(),
			DayOfWeek: time.Weekday(r.DayOfWeek),
			Start:     formatTimeOfDay(r.StartTime), End: formatTimeOfDay(r.EndTime),
			EffectiveFrom: r.EffectiveFrom.Time, EffectiveTo: effectiveToDate(r.EffectiveTo),
			Active: r.IsActive,
		}
		if !schedule.PatternOverlap(candidate, other) {
			continue
		}
		reason := "physician"
		if candidate.RoomID != "" && candidate.RoomID == other.RoomID {
			reason = "room"
		}
		c.JSON(http.StatusConflict, gin.H{
			"error": "schedule overlaps another schedule",
			"conflicts": []scheduleConflict{{
				ScheduleID: other.ID,
				Reason:     reason,
			}},
		})
		return false
	}
	return true
}

// scheduleWarnings computes the response warnings[] (spec §4):
//   - sip_expires_before_end: effective_to (or an open-ended pattern) passes
//     the physician's sip_valid_until;
//   - contract_ending: effective_to is within today + contract_warning_days
//     (merchant timezone).
func scheduleWarnings(c *gin.Context, q *sqlcgen.Queries, merchantID, physicianID pgtype.UUID, effectiveTo pgtype.Date) []string {
	warnings := []string{}
	sip, err := q.GetPhysicianSipValidUntil(c.Request.Context(), physicianID)
	if err == nil && sip.Valid &&
		(!effectiveTo.Valid || effectiveTo.Time.After(sip.Time)) {
		warnings = append(warnings, "sip_expires_before_end")
	}
	loc, err := merchantLocation(c, q, merchantID)
	if err != nil {
		return warnings
	}
	settings := loadScheduleSettings(c, q, merchantID)
	today := time.Now().In(loc)
	y, m, d := today.Date()
	deadline := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, settings.ContractWarningDays)
	if effectiveTo.Valid && !effectiveTo.Time.After(deadline) {
		warnings = append(warnings, "contract_ending")
	}
	return warnings
}

type createPhysicianScheduleRequest struct {
	MerchantID        string   `json:"merchant_id" binding:"required"`
	PhysicianID       string   `json:"physician_id" binding:"required"`
	DepartmentID      string   `json:"department_id" binding:"required"`
	DayOfWeek         int16    `json:"day_of_week" binding:"gte=0,lte=6"`
	StartTime         string   `json:"start_time" binding:"required"`
	EndTime           string   `json:"end_time" binding:"required"`
	SlotQuota         int32    `json:"slot_quota"`
	EffectiveFrom     string   `json:"effective_from" binding:"required"`
	EffectiveTo       string   `json:"effective_to"`
	RoomID            string   `json:"room_id"`
	QuotaJkn          int32    `json:"quota_jkn"`
	MinutesPerPatient int32    `json:"minutes_per_patient"`
	ServiceTypes      []string `json:"service_types"`
	ShiftID           string   `json:"shift_id"`
	Notes             string   `json:"notes"`
}

// CreatePhysicianScheduleHandler godoc
// @Summary Create a physician schedule (weekly recurring template)
// @Tags physician_schedule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createPhysicianScheduleRequest true "Schedule data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /physician-schedules [post]
func CreatePhysicianScheduleHandler(c *gin.Context) {
	var req createPhysicianScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	// Check permission against the body's merchant_id, not X-Merchant-ID —
	// they can differ, and creating under a merchant the caller only declared
	// in a header (without holding the permission there) is a cross-tenant
	// IDOR. See 2026-09-16-v1-operations-antrian-jadwal-plan.md.
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	physicianID, ok := parseUUID(req.PhysicianID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid physician_id"})
		return
	}
	departmentID, ok := parseUUID(req.DepartmentID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
		return
	}
	startTime, err := parseTimeOfDay(req.StartTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start_time, expected HH:MM"})
		return
	}
	endTime, err := parseTimeOfDay(req.EndTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end_time, expected HH:MM"})
		return
	}
	effectiveFrom, err := optDate(req.EffectiveFrom)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from, expected YYYY-MM-DD"})
		return
	}
	effectiveTo, err := optDate(req.EffectiveTo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_to, expected YYYY-MM-DD"})
		return
	}
	if !checkScheduleRanges(c, startTime, endTime, effectiveFrom, effectiveTo) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	roomID, ok := resolveScheduleRoom(c, q, merchantID, req.RoomID)
	if !ok {
		return
	}
	var shiftID pgtype.UUID
	if req.ShiftID != "" {
		var ok bool
		shiftID, ok = parseUUID(req.ShiftID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid shift_id"})
			return
		}
	}
	minutes := req.MinutesPerPatient
	if minutes <= 0 {
		minutes = defaultMinutesPerPatient
	}
	serviceTypes := req.ServiceTypes
	if len(serviceTypes) == 0 {
		serviceTypes = []string{"umum", "bpjs"}
	}
	candidate := schedule.Pattern{
		ID: "new", PhysicianID: physicianID.String(), RoomID: roomID.String(),
		DayOfWeek: time.Weekday(req.DayOfWeek),
		Start:     formatTimeOfDay(startTime), End: formatTimeOfDay(endTime),
		EffectiveFrom: effectiveFrom.Time, EffectiveTo: effectiveToDate(effectiveTo),
		Active: true,
	}
	if !checkPatternConflicts(c, q, merchantID, candidate, "") {
		return
	}
	schedule, err := q.CreatePhysicianSchedule(c.Request.Context(), sqlcgen.CreatePhysicianScheduleParams{
		CompanyID:         AuthCompanyID(c),
		MerchantID:        merchantID,
		PhysicianID:       physicianID,
		DepartmentID:      departmentID,
		DayOfWeek:         req.DayOfWeek,
		StartTime:         startTime,
		EndTime:           endTime,
		SlotQuota:         req.SlotQuota,
		EffectiveFrom:     effectiveFrom,
		EffectiveTo:       effectiveTo,
		CreatedBy:         AuthUserID(c),
		RoomID:            roomID,
		QuotaJkn:          req.QuotaJkn,
		MinutesPerPatient: minutes,
		ServiceTypes:      serviceTypes,
		ShiftID:           shiftID,
		Notes:             pgtype.Text{String: req.Notes, Valid: req.Notes != ""},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	warnings := scheduleWarnings(c, q, merchantID, physicianID, effectiveTo)
	c.JSON(http.StatusCreated, gin.H{"data": toScheduleResponse(schedule), "meta": gin.H{}, "warnings": warnings})
}

// ListPhysicianSchedulesHandler godoc
// @Summary List physician schedules under a merchant
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/physician-schedules [get]
func ListPhysicianSchedulesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	schedules, err := q.ListPhysicianSchedulesByMerchant(c.Request.Context(), sqlcgen.ListPhysicianSchedulesByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, len(schedules))
	for i, s := range schedules {
		out[i] = toScheduleResponse(s)
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetPhysicianScheduleHandler godoc
// @Summary Get a physician schedule by id
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician schedule UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physician-schedules/{id} [get]
func GetPhysicianScheduleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	schedule, err := q.GetPhysicianScheduleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, schedule.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toScheduleResponse(schedule), "meta": gin.H{}})
}

type updatePhysicianScheduleRequest struct {
	DayOfWeek         int16    `json:"day_of_week" binding:"gte=0,lte=6"`
	StartTime         string   `json:"start_time" binding:"required"`
	EndTime           string   `json:"end_time" binding:"required"`
	SlotQuota         int32    `json:"slot_quota"`
	EffectiveFrom     string   `json:"effective_from" binding:"required"`
	EffectiveTo       string   `json:"effective_to"`
	IsActive          bool     `json:"is_active"`
	RoomID            *string  `json:"room_id"`
	QuotaJkn          *int32   `json:"quota_jkn"`
	MinutesPerPatient *int32   `json:"minutes_per_patient"`
	ServiceTypes      []string `json:"service_types"`
	ShiftID           *string  `json:"shift_id"`
	Notes             *string  `json:"notes"`
}

// UpdatePhysicianScheduleHandler godoc
// @Summary Update a physician schedule
// @Tags physician_schedule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician schedule UUID"
// @Param request body updatePhysicianScheduleRequest true "Schedule data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /physician-schedules/{id} [patch]
func UpdatePhysicianScheduleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPhysicianScheduleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, existing.MerchantID) {
		return
	}
	var req updatePhysicianScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	startTime, err := parseTimeOfDay(req.StartTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start_time, expected HH:MM"})
		return
	}
	endTime, err := parseTimeOfDay(req.EndTime)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end_time, expected HH:MM"})
		return
	}
	effectiveFrom, err := optDate(req.EffectiveFrom)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from, expected YYYY-MM-DD"})
		return
	}
	effectiveTo, err := optDate(req.EffectiveTo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_to, expected YYYY-MM-DD"})
		return
	}
	if !checkScheduleRanges(c, startTime, endTime, effectiveFrom, effectiveTo) {
		return
	}
	roomID := existing.RoomID
	if req.RoomID != nil {
		parsed, ok := resolveScheduleRoom(c, q, existing.MerchantID, *req.RoomID)
		if !ok {
			return
		}
		roomID = parsed
	}
	shiftID := existing.ShiftID
	if req.ShiftID != nil {
		if *req.ShiftID == "" {
			shiftID = pgtype.UUID{}
		} else {
			parsed, ok := parseUUID(*req.ShiftID)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid shift_id"})
				return
			}
			shiftID = parsed
		}
	}
	quotaJkn := existing.QuotaJkn
	if req.QuotaJkn != nil {
		quotaJkn = *req.QuotaJkn
	}
	minutes := existing.MinutesPerPatient
	if req.MinutesPerPatient != nil && *req.MinutesPerPatient > 0 {
		minutes = *req.MinutesPerPatient
	}
	if minutes <= 0 {
		minutes = defaultMinutesPerPatient
	}
	serviceTypes := existing.ServiceTypes
	if len(req.ServiceTypes) > 0 {
		serviceTypes = req.ServiceTypes
	}
	notes := existing.Notes
	if req.Notes != nil {
		notes = pgtype.Text{String: *req.Notes, Valid: *req.Notes != ""}
	}
	candidate := schedule.Pattern{
		ID: id.String(), PhysicianID: existing.PhysicianID.String(), RoomID: roomID.String(),
		DayOfWeek: time.Weekday(req.DayOfWeek),
		Start:     formatTimeOfDay(startTime), End: formatTimeOfDay(endTime),
		EffectiveFrom: effectiveFrom.Time, EffectiveTo: effectiveToDate(effectiveTo),
		Active: req.IsActive,
	}
	if !checkPatternConflicts(c, q, existing.MerchantID, candidate, id.String()) {
		return
	}
	schedule, err := q.UpdatePhysicianSchedule(c.Request.Context(), sqlcgen.UpdatePhysicianScheduleParams{
		ID:                id,
		DayOfWeek:         req.DayOfWeek,
		StartTime:         startTime,
		EndTime:           endTime,
		SlotQuota:         req.SlotQuota,
		EffectiveFrom:     effectiveFrom,
		EffectiveTo:       effectiveTo,
		IsActive:          req.IsActive,
		UpdatedBy:         AuthUserID(c),
		RoomID:            roomID,
		QuotaJkn:          quotaJkn,
		MinutesPerPatient: minutes,
		ServiceTypes:      serviceTypes,
		ShiftID:           shiftID,
		Notes:             notes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	warnings := scheduleWarnings(c, q, existing.MerchantID, existing.PhysicianID, effectiveTo)
	c.JSON(http.StatusOK, gin.H{"data": toScheduleResponse(schedule), "meta": gin.H{}, "warnings": warnings})
}

// DeletePhysicianScheduleHandler godoc
// @Summary Soft-delete a physician schedule
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Physician schedule UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /physician-schedules/{id} [delete]
func DeletePhysicianScheduleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPhysicianScheduleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, existing.MerchantID) {
		return
	}
	if err := q.SoftDeletePhysicianSchedule(c.Request.Context(), sqlcgen.SoftDeletePhysicianScheduleParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
