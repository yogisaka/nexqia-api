// internal/server/schedule_calendar.go
// Schedule calendar, summary, per-date session overrides and merchant schedule
// settings — spec 2026-10-01-b-physician-schedule-design §3-§4.
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
	"github.com/yogisaka/nexqia-api/internal/schedule"
)

// Feature-flag keys holding the merchant schedule settings (spec §3). A
// missing or unparsable flag falls back to the default in loadScheduleSettings.
const (
	scheduleQuotaModeFlagKey       = "schedule.quota_mode"
	scheduleNearFullFlagKey        = "schedule.near_full_threshold"
	scheduleContractWarningFlagKey = "schedule.contract_warning_days"

	// scheduleCalendarMaxRangeDays caps the calendar/summary range (spec §4).
	scheduleCalendarMaxRangeDays = 62

	// defaultMinutesPerPatient is the operations.physician_schedule column
	// default, applied when a create/update omits minutes_per_patient.
	defaultMinutesPerPatient = 10
)

// callerHasPermission is RequirePermissionForMerchant without the error/403
// side effects — used when a handler accepts ANY of several permissions and
// only the final fallback may write the response (the calendar also accepts
// operations.visit.manage so FO can pick a session at registration).
func callerHasPermission(c *gin.Context, code string, merchantID pgtype.UUID) bool {
	q := sqlcgen.New(TxFromContext(c))
	roleID := AuthRoleID(c)
	if !roleID.Valid {
		def, err := q.DefaultUserRole(c.Request.Context(), sqlcgen.DefaultUserRoleParams{
			UserID: AuthUserID(c), MerchantID: merchantID,
		})
		if err != nil || !def.Valid {
			return false
		}
		roleID = def
	}
	has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: code, RoleID: roleID,
	})
	return err == nil && has
}

// scheduleSettings mirrors the three merchant feature flags with their spec
// defaults (combined, 80, 30).
type scheduleSettings struct {
	QuotaMode           string
	NearFullThreshold   int
	ContractWarningDays int
}

func defaultScheduleSettings() scheduleSettings {
	return scheduleSettings{QuotaMode: schedule.ModeCombined, NearFullThreshold: 80, ContractWarningDays: 30}
}

// loadScheduleSettings reads the merchant's schedule feature flags; a missing
// flag or one that fails to parse/validate keeps the default — never an error
// (same treatment as getPinLockFlag in applock.go).
func loadScheduleSettings(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) scheduleSettings {
	s := defaultScheduleSettings()
	if flag, err := q.GetFeatureFlag(c.Request.Context(), sqlcgen.GetFeatureFlagParams{MerchantID: merchantID, FlagKey: scheduleQuotaModeFlagKey}); err == nil {
		var mode string
		if json.Unmarshal(flag.FlagValue, &mode) == nil && (mode == schedule.ModeCombined || mode == schedule.ModeSplit) {
			s.QuotaMode = mode
		}
	}
	if flag, err := q.GetFeatureFlag(c.Request.Context(), sqlcgen.GetFeatureFlagParams{MerchantID: merchantID, FlagKey: scheduleNearFullFlagKey}); err == nil {
		var pct int
		if json.Unmarshal(flag.FlagValue, &pct) == nil && pct >= 1 && pct <= 100 {
			s.NearFullThreshold = pct
		}
	}
	if flag, err := q.GetFeatureFlag(c.Request.Context(), sqlcgen.GetFeatureFlagParams{MerchantID: merchantID, FlagKey: scheduleContractWarningFlagKey}); err == nil {
		var days int
		if json.Unmarshal(flag.FlagValue, &days) == nil && days >= 1 && days <= 365 {
			s.ContractWarningDays = days
		}
	}
	return s
}

// tzName resolves the merchant timezone string (spec §3: session dates and
// "today" follow core.merchant.timezone), falling back to the spec default
// when the merchant row cannot be loaded.
func tzName(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) string {
	tz, err := q.GetMerchantTimezone(c.Request.Context(), merchantID)
	if err != nil || tz == "" {
		return "Asia/Jakarta"
	}
	return tz
}

// merchantLocation loads the merchant timezone as *time.Location.
func merchantLocation(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) (*time.Location, error) {
	return time.LoadLocation(tzName(c, q, merchantID))
}

// parseScheduleDate parses a YYYY-MM-DD path/query parameter.
func parseScheduleDate(s string) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

// scheduleRegistration holds the per-payer admission counts of one session.
type scheduleRegistration struct {
	JKN   int64
	Other int64
}

func registrationKey(scheduleID string, date time.Time) string {
	return scheduleID + "|" + date.Format("2006-01-02")
}

func effectiveToDate(d pgtype.Date) time.Time {
	if !d.Valid {
		return time.Time{}
	}
	return d.Time
}

// calendarData bundles everything the calendar/summary handlers need.
type calendarData struct {
	sessions      []schedule.Session
	patterns      map[string]sqlcgen.ListSchedulePatternsForCalendarRow
	roomPaths     map[pgtype.UUID]string
	registrations map[string]scheduleRegistration
	stored        map[string]sqlcgen.ListScheduleSessionsBetweenRow
}

func computeCalendarData(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, from, to time.Time) (*calendarData, error) {
	ctx := c.Request.Context()
	patternRows, err := q.ListSchedulePatternsForCalendar(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	patterns := make([]schedule.Pattern, 0, len(patternRows))
	patternByID := make(map[string]sqlcgen.ListSchedulePatternsForCalendarRow, len(patternRows))
	for _, r := range patternRows {
		patterns = append(patterns, schedule.Pattern{
			ID:            r.ID.String(),
			PhysicianID:   r.PhysicianID.String(),
			DepartmentID:  r.DepartmentID.String(),
			RoomID:        r.RoomID.String(),
			DayOfWeek:     time.Weekday(r.DayOfWeek),
			Start:         formatTimeOfDay(r.StartTime),
			End:           formatTimeOfDay(r.EndTime),
			QuotaJKN:      int(r.QuotaJkn),
			SlotQuota:     int(r.SlotQuota),
			EffectiveFrom: r.EffectiveFrom.Time,
			EffectiveTo:   effectiveToDate(r.EffectiveTo),
			Active:        r.IsActive,
		})
		patternByID[r.ID.String()] = r
	}

	storedRows, err := q.ListScheduleSessionsBetween(ctx, sqlcgen.ListScheduleSessionsBetweenParams{
		MerchantID:    merchantID,
		SessionDate:   pgtype.Date{Time: from, Valid: true},
		SessionDate_2: pgtype.Date{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	stored := make([]schedule.StoredSession, 0, len(storedRows))
	storedByKey := make(map[string]sqlcgen.ListScheduleSessionsBetweenRow, len(storedRows))
	for _, r := range storedRows {
		stored = append(stored, schedule.StoredSession{
			ScheduleID: r.ScheduleID.String(), Date: r.SessionDate.Time,
			PhysicianID: r.PhysicianID.String(), RoomID: r.RoomID.String(),
			Start: formatTimeOfDay(r.StartTime), End: formatTimeOfDay(r.EndTime),
			QuotaJKN: int(r.QuotaJkn), SlotQuota: int(r.SlotQuota),
			Status: r.Status, LeaveID: r.LeaveID.String(), Notes: r.Notes.String,
		})
		storedByKey[registrationKey(r.ScheduleID.String(), r.SessionDate.Time)] = r
	}

	allLocs, err := q.ListAllLocationsByMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	paths := locationPaths(allLocs)

	tz := tzName(c, q, merchantID)
	regRows, err := q.CountScheduleRegistrations(ctx, sqlcgen.CountScheduleRegistrationsParams{
		Column1:    tz,
		MerchantID: merchantID,
		Column3:    pgtype.Date{Time: from, Valid: true},
		Column4:    pgtype.Date{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	regs := make(map[string]scheduleRegistration, len(regRows))
	for _, r := range regRows {
		regs[registrationKey(r.ScheduleID.String(), r.SessionDate.Time)] =
			scheduleRegistration{JKN: r.RegisteredJkn, Other: r.RegisteredOther}
	}

	return &calendarData{
		sessions:      schedule.Project(patterns, stored, from, to),
		patterns:      patternByID,
		roomPaths:     paths,
		registrations: regs,
		stored:        storedByKey,
	}, nil
}

// scheduleSessionItem renders one projected/stored session for the calendar
// response (spec §4 field list).
func scheduleSessionItem(data *calendarData, s schedule.Session, settings scheduleSettings) gin.H {
	pattern := data.patterns[s.ScheduleID]
	regKey := registrationKey(s.ScheduleID, s.Date)
	reg := data.registrations[regKey]

	status := schedule.Status(int(reg.JKN), int(reg.Other), s, settings.QuotaMode, settings.NearFullThreshold)

	var room any
	if id, ok := parseUUID(s.RoomID); ok {
		room = gin.H{"id": s.RoomID, "path": data.roomPaths[id]}
	}

	var leave any
	bpjsSync := "not_synced"
	if row, ok := data.stored[regKey]; ok {
		bpjsSync = row.BpjsSyncStatus
		if s.Status == schedule.StatusLeave || s.Status == schedule.StatusSubstituted {
			leave = gin.H{
				"reason":                  row.LeaveReason,
				"substitute_physician_id": row.LeaveSubstitutePhysicianID,
			}
		}
	}

	return gin.H{
		"schedule_id": s.ScheduleID,
		"date":        s.Date.Format("2006-01-02"),
		"projected":   s.Projected,
		"physician": gin.H{
			"id":         pattern.PhysicianID.String(),
			"name":       pattern.PhysicianName,
			"sip_number": pattern.PhysicianSipNumber,
		},
		"department": gin.H{
			"id":   pattern.DepartmentID.String(),
			"name": pattern.DepartmentName,
		},
		"room":             room,
		"start_time":       s.Start,
		"end_time":         s.End,
		"quota_jkn":        s.QuotaJKN,
		"slot_quota":       s.SlotQuota,
		"registered_jkn":   reg.JKN,
		"registered_other": reg.Other,
		"status":           status,
		"leave":            leave,
		"notes":            s.Notes,
		"bpjs_sync_status": bpjsSync,
	}
}

// parseCalendarRange reads from/to (YYYY-MM-DD) and enforces the 62-day cap.
func parseCalendarRange(c *gin.Context) (time.Time, time.Time, bool) {
	from, ok := parseScheduleDate(c.Query("from"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from, expected YYYY-MM-DD"})
		return time.Time{}, time.Time{}, false
	}
	to, ok := parseScheduleDate(c.Query("to"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to, expected YYYY-MM-DD"})
		return time.Time{}, time.Time{}, false
	}
	if to.Before(from) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to must be on or after from"})
		return time.Time{}, time.Time{}, false
	}
	if daysBetween(from, to) > scheduleCalendarMaxRangeDays {
		c.JSON(http.StatusBadRequest, gin.H{"error": "date range must not exceed 62 days"})
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours()/24) + 1
}

// ScheduleCalendarHandler godoc
// @Summary Merchant schedule calendar (projected + stored sessions)
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param from query string true "Range start YYYY-MM-DD"
// @Param to query string true "Range end YYYY-MM-DD"
// @Param department_id query string false "Filter by department UUID"
// @Param physician_id query string false "Filter by physician UUID"
// @Param room_id query string false "Filter by room UUID"
// @Param shift_id query string false "Filter by shift UUID"
// @Param status query string false "Filter by computed status"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/schedule/calendar [get]
func ScheduleCalendarHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	// FO registration also reads the calendar via operations.visit.manage; the
	// final RequirePermissionForMerchant call writes the 403 when neither holds.
	if !callerHasPermission(c, PermScheduleManage, merchantID) &&
		!RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	from, to, ok := parseCalendarRange(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	data, err := computeCalendarData(c, q, merchantID, from, to)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	settings := loadScheduleSettings(c, q, merchantID)

	deptFilter, hasDept := parseOptionalFilter(c, "department_id")
	physFilter, hasPhys := parseOptionalFilter(c, "physician_id")
	roomFilter, hasRoom := parseOptionalFilter(c, "room_id")
	shiftFilter, hasShift := parseOptionalFilter(c, "shift_id")
	statusFilter := c.Query("status")

	items := make([]gin.H, 0, len(data.sessions))
	for _, s := range data.sessions {
		pattern := data.patterns[s.ScheduleID]
		if hasDept && pattern.DepartmentID.String() != deptFilter {
			continue
		}
		if hasPhys && s.PhysicianID != physFilter {
			continue
		}
		if hasRoom && s.RoomID != roomFilter {
			continue
		}
		if hasShift && pattern.ShiftID.String() != shiftFilter {
			continue
		}
		item := scheduleSessionItem(data, s, settings)
		if statusFilter != "" && item["status"] != statusFilter {
			continue
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"from": from.Format("2006-01-02"), "to": to.Format("2006-01-02")}})
}

// parseOptionalFilter returns the query param (validated UUID) and whether it
// was provided; a malformed UUID fails the request.
func parseOptionalFilter(c *gin.Context, name string) (string, bool) {
	raw := c.Query(name)
	if raw == "" {
		return "", false
	}
	id, ok := parseUUID(raw)
	if !ok {
		return "", false
	}
	return id.String(), true
}

// ScheduleSummaryHandler godoc
// @Summary Merchant schedule summary for a range
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param from query string true "Range start YYYY-MM-DD"
// @Param to query string true "Range end YYYY-MM-DD"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/schedule/summary [get]
func ScheduleSummaryHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	from, to, ok := parseCalendarRange(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	data, err := computeCalendarData(c, q, merchantID, from, to)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	physicians := make(map[string]bool)
	departments := make(map[string]bool)
	leaveSessions := 0
	for _, s := range data.sessions {
		pattern := data.patterns[s.ScheduleID]
		physicians[s.PhysicianID+"|"+pattern.DepartmentID.String()] = true
		departments[pattern.DepartmentID.String()] = true
		if s.Status == schedule.StatusLeave || s.Status == schedule.StatusSubstituted {
			leaveSessions++
		}
	}
	conflicts := 0
	for i := 0; i < len(data.sessions); i++ {
		for j := i + 1; j < len(data.sessions); j++ {
			if schedule.Overlaps(data.sessions[i], data.sessions[j]) {
				conflicts++
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"active_physicians": len(physicians),
		"departments":       len(departments),
		"sessions":          len(data.sessions),
		"leave_sessions":    leaveSessions,
		"room_conflicts":    conflicts,
	}, "meta": gin.H{}})
}

type upsertScheduleSessionRequest struct {
	RoomID    *string `json:"room_id"`
	StartTime *string `json:"start_time"`
	EndTime   *string `json:"end_time"`
	QuotaJkn  *int32  `json:"quota_jkn"`
	SlotQuota *int32  `json:"slot_quota"`
	Notes     *string `json:"notes"`
}

// UpsertScheduleSessionHandler godoc
// @Summary Override (or create) the stored session of a schedule for one date
// @Tags physician_schedule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param schedule_id path string true "Physician schedule UUID"
// @Param date path string true "Session date YYYY-MM-DD"
// @Param request body upsertScheduleSessionRequest true "Override values (empty = from pattern)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /schedule/sessions/{schedule_id}/{date} [put]
func UpsertScheduleSessionHandler(c *gin.Context) {
	scheduleID, ok := parseUUID(c.Param("schedule_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid schedule_id"})
		return
	}
	date, ok := parseScheduleDate(c.Param("date"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, expected YYYY-MM-DD"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	pattern, err := q.GetPhysicianScheduleByID(c.Request.Context(), scheduleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, pattern.MerchantID) {
		return
	}
	// The date must fall on the pattern's weekday, inside its effective range,
	// on an active pattern — otherwise there is no session to override.
	if !pattern.IsActive || int16(date.Weekday()) != pattern.DayOfWeek ||
		date.Before(pattern.EffectiveFrom.Time) ||
		(pattern.EffectiveTo.Valid && date.After(pattern.EffectiveTo.Time)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "date does not match schedule pattern"})
		return
	}
	var req upsertScheduleSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	roomID := pattern.RoomID
	if req.RoomID != nil && *req.RoomID != "" {
		parsed, ok := resolveScheduleRoom(c, q, pattern.MerchantID, *req.RoomID)
		if !ok {
			return
		}
		roomID = parsed
	}
	startTime := pattern.StartTime
	if req.StartTime != nil {
		startTime, err = parseTimeOfDay(*req.StartTime)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start_time, expected HH:MM"})
			return
		}
	}
	endTime := pattern.EndTime
	if req.EndTime != nil {
		endTime, err = parseTimeOfDay(*req.EndTime)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end_time, expected HH:MM"})
			return
		}
	}
	if endTime.Microseconds <= startTime.Microseconds {
		c.JSON(http.StatusBadRequest, gin.H{"error": "end_time must be after start_time"})
		return
	}
	quotaJkn := pattern.QuotaJkn
	if req.QuotaJkn != nil {
		quotaJkn = *req.QuotaJkn
	}
	slotQuota := pattern.SlotQuota
	if req.SlotQuota != nil {
		slotQuota = *req.SlotQuota
	}
	var notes pgtype.Text
	if req.Notes != nil {
		notes = pgtype.Text{String: *req.Notes, Valid: true}
	}

	// Conflict check for this date only: project every other pattern into the
	// date and compare with schedule.Overlaps.
	if ok := checkSessionConflicts(c, q, pattern, date, roomID, startTime, endTime); !ok {
		return
	}

	session, err := q.UpsertScheduleSession(c.Request.Context(), sqlcgen.UpsertScheduleSessionParams{
		CompanyID:    pattern.CompanyID,
		MerchantID:   pattern.MerchantID,
		ScheduleID:   pattern.ID,
		SessionDate:  pgtype.Date{Time: date, Valid: true},
		PhysicianID:  pattern.PhysicianID,
		DepartmentID: pattern.DepartmentID,
		RoomID:       roomID,
		StartTime:    startTime,
		EndTime:      endTime,
		QuotaJkn:     quotaJkn,
		SlotQuota:    slotQuota,
		Notes:        notes,
		CreatedBy:    AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": scheduleSessionResponse(session), "meta": gin.H{}})
}

// checkSessionConflicts rejects an override that overlaps another session on
// the same date (same room or same physician). Returns false when it already
// wrote the 409 response.
func checkSessionConflicts(c *gin.Context, q *sqlcgen.Queries, pattern sqlcgen.OperationsPhysicianSchedule, date time.Time, roomID pgtype.UUID, start, end pgtype.Time) bool {
	data, err := computeCalendarData(c, q, pattern.MerchantID, date, date)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	candidate := schedule.Session{
		ScheduleID:  pattern.ID.String(),
		Date:        date,
		PhysicianID: pattern.PhysicianID.String(),
		RoomID:      roomID.String(),
		Start:       formatTimeOfDay(start),
		End:         formatTimeOfDay(end),
		Status:      schedule.StatusAvailable,
	}
	for _, other := range data.sessions {
		if other.ScheduleID == candidate.ScheduleID {
			continue
		}
		if schedule.Overlaps(candidate, other) {
			c.JSON(http.StatusConflict, gin.H{"error": "session overlaps another session"})
			return false
		}
	}
	return true
}

// scheduleSessionResponse renders a stored schedule_session row.
func scheduleSessionResponse(s sqlcgen.OperationsScheduleSession) gin.H {
	return gin.H{
		"id":               s.ID,
		"schedule_id":      s.ScheduleID,
		"session_date":     s.SessionDate,
		"physician_id":     s.PhysicianID,
		"department_id":    s.DepartmentID,
		"room_id":          s.RoomID,
		"start_time":       formatTimeOfDay(s.StartTime),
		"end_time":         formatTimeOfDay(s.EndTime),
		"quota_jkn":        s.QuotaJkn,
		"slot_quota":       s.SlotQuota,
		"status":           s.Status,
		"notes":            s.Notes,
		"bpjs_sync_status": s.BpjsSyncStatus,
	}
}

// DeleteScheduleSessionHandler godoc
// @Summary Delete a per-date session override (back to the pattern)
// @Tags physician_schedule
// @Security BearerAuth
// @Param schedule_id path string true "Physician schedule UUID"
// @Param date path string true "Session date YYYY-MM-DD"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /schedule/sessions/{schedule_id}/{date} [delete]
func DeleteScheduleSessionHandler(c *gin.Context) {
	scheduleID, ok := parseUUID(c.Param("schedule_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid schedule_id"})
		return
	}
	date, ok := parseScheduleDate(c.Param("date"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, expected YYYY-MM-DD"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	pattern, err := q.GetPhysicianScheduleByID(c.Request.Context(), scheduleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, pattern.MerchantID) {
		return
	}
	session, err := q.GetScheduleSessionByScheduleAndDate(c.Request.Context(), sqlcgen.GetScheduleSessionByScheduleAndDateParams{
		ScheduleID:  scheduleID,
		SessionDate: pgtype.Date{Time: date, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule session not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Only an active stored row can be removed; leave/substituted/cancelled
	// rows carry clinical meaning (spec §4: else 409).
	if session.Status != "active" {
		c.JSON(http.StatusConflict, gin.H{"error": "session has registered patients"})
		return
	}
	count, err := q.CountSessionAdmissions(c.Request.Context(), sqlcgen.CountSessionAdmissionsParams{
		ScheduleID: scheduleID,
		Column2:    tzName(c, q, pattern.MerchantID),
		Column3:    pgtype.Date{Time: date, Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "session has registered patients"})
		return
	}
	if err := q.DeleteScheduleSession(c.Request.Context(), session.ID); err != nil {
		abortInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// GetScheduleSettingsHandler godoc
// @Summary Read merchant schedule settings (defaults when unset)
// @Tags physician_schedule
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/schedule/settings [get]
func GetScheduleSettingsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	s := loadScheduleSettings(c, q, merchantID)
	c.JSON(http.StatusOK, gin.H{"data": scheduleSettingsResponse(s), "meta": gin.H{}})
}

func scheduleSettingsResponse(s scheduleSettings) gin.H {
	return gin.H{
		"quota_mode":            s.QuotaMode,
		"near_full_threshold":   s.NearFullThreshold,
		"contract_warning_days": s.ContractWarningDays,
	}
}

type updateScheduleSettingsRequest struct {
	QuotaMode           string `json:"quota_mode" binding:"required"`
	NearFullThreshold   int    `json:"near_full_threshold" binding:"required,gte=1,lte=100"`
	ContractWarningDays int    `json:"contract_warning_days" binding:"required,gte=1,lte=365"`
}

// UpdateScheduleSettingsHandler godoc
// @Summary Update merchant schedule settings (three feature flags)
// @Tags physician_schedule
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body updateScheduleSettingsRequest true "Settings"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/schedule/settings [put]
func UpdateScheduleSettingsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	var req updateScheduleSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.QuotaMode != schedule.ModeCombined && req.QuotaMode != schedule.ModeSplit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota_mode must be combined or split"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	modeJSON, _ := json.Marshal(req.QuotaMode)
	if _, err := q.UpsertFeatureFlag(c.Request.Context(), sqlcgen.UpsertFeatureFlagParams{
		MerchantID: merchantID, FlagKey: scheduleQuotaModeFlagKey, FlagValue: modeJSON,
		CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	thresholdJSON, _ := json.Marshal(req.NearFullThreshold)
	if _, err := q.UpsertFeatureFlag(c.Request.Context(), sqlcgen.UpsertFeatureFlagParams{
		MerchantID: merchantID, FlagKey: scheduleNearFullFlagKey, FlagValue: thresholdJSON,
		CreatedBy: AuthUserID(c),
	}); err != nil {
		// A write already happened in this transaction — abort, do not commit.
		abortInternalError(c, err)
		return
	}
	daysJSON, _ := json.Marshal(req.ContractWarningDays)
	if _, err := q.UpsertFeatureFlag(c.Request.Context(), sqlcgen.UpsertFeatureFlagParams{
		MerchantID: merchantID, FlagKey: scheduleContractWarningFlagKey, FlagValue: daysJSON,
		CreatedBy: AuthUserID(c),
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": scheduleSettingsResponse(scheduleSettings{
		QuotaMode:           req.QuotaMode,
		NearFullThreshold:   req.NearFullThreshold,
		ContractWarningDays: req.ContractWarningDays,
	}), "meta": gin.H{}})
}
