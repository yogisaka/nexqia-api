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
		"id":             s.ID,
		"company_id":     s.CompanyID,
		"merchant_id":    s.MerchantID,
		"physician_id":   s.PhysicianID,
		"department_id":  s.DepartmentID,
		"day_of_week":    s.DayOfWeek,
		"start_time":     formatTimeOfDay(s.StartTime),
		"end_time":       formatTimeOfDay(s.EndTime),
		"slot_quota":     s.SlotQuota,
		"effective_from": s.EffectiveFrom,
		"effective_to":   s.EffectiveTo,
		"is_active":      s.IsActive,
		"created_at":     s.CreatedAt,
		"updated_at":     s.UpdatedAt,
	}
}

type createPhysicianScheduleRequest struct {
	MerchantID    string `json:"merchant_id" binding:"required"`
	PhysicianID   string `json:"physician_id" binding:"required"`
	DepartmentID  string `json:"department_id" binding:"required"`
	DayOfWeek     int16  `json:"day_of_week" binding:"gte=0,lte=6"`
	StartTime     string `json:"start_time" binding:"required"`
	EndTime       string `json:"end_time" binding:"required"`
	SlotQuota     int32  `json:"slot_quota"`
	EffectiveFrom string `json:"effective_from" binding:"required"`
	EffectiveTo   string `json:"effective_to"`
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
	schedule, err := q.CreatePhysicianSchedule(c.Request.Context(), sqlcgen.CreatePhysicianScheduleParams{
		CompanyID:     AuthCompanyID(c),
		MerchantID:    merchantID,
		PhysicianID:   physicianID,
		DepartmentID:  departmentID,
		DayOfWeek:     req.DayOfWeek,
		StartTime:     startTime,
		EndTime:       endTime,
		SlotQuota:     req.SlotQuota,
		EffectiveFrom: effectiveFrom,
		EffectiveTo:   effectiveTo,
		CreatedBy:     AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toScheduleResponse(schedule), "meta": gin.H{}})
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
	DayOfWeek     int16  `json:"day_of_week" binding:"gte=0,lte=6"`
	StartTime     string `json:"start_time" binding:"required"`
	EndTime       string `json:"end_time" binding:"required"`
	SlotQuota     int32  `json:"slot_quota"`
	EffectiveFrom string `json:"effective_from" binding:"required"`
	EffectiveTo   string `json:"effective_to"`
	IsActive      bool   `json:"is_active"`
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
	schedule, err := q.UpdatePhysicianSchedule(c.Request.Context(), sqlcgen.UpdatePhysicianScheduleParams{
		ID:            id,
		DayOfWeek:     req.DayOfWeek,
		StartTime:     startTime,
		EndTime:       endTime,
		SlotQuota:     req.SlotQuota,
		EffectiveFrom: effectiveFrom,
		EffectiveTo:   effectiveTo,
		IsActive:      req.IsActive,
		UpdatedBy:     AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "physician schedule not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toScheduleResponse(schedule), "meta": gin.H{}})
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
