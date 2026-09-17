// internal/server/display.go
package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterDisplayRoutes wires read-only queue/schedule board endpoints
// (docs/breakdown/screen.md, no real-time push in v1 — see
// docs/design/specs/2026-09-16-v1-operations-antrian-jadwal-design.md §9).
func RegisterDisplayRoutes(rg *gin.RouterGroup) {
	rg.GET("/display/queue", DisplayQueueHandler)
	rg.GET("/display/schedule", DisplayScheduleHandler)
}

// DisplayQueueHandler godoc
// @Summary Poll-based queue board for a merchant
// @Description Read-only, no real-time push in v1.
// @Tags display
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param department_id query string false "Filter by department UUID"
// @Success 200 {object} apiResponse
// @Router /display/queue [get]
func DisplayQueueHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	var departmentID pgtype.UUID
	if v := c.Query("department_id"); v != "" {
		id, ok := parseUUID(v)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
			return
		}
		departmentID = id
	}
	q := sqlcgen.New(TxFromContext(c))
	queue, err := q.ListActiveQueueForDisplay(c.Request.Context(), sqlcgen.ListActiveQueueForDisplayParams{
		MerchantID: merchantID, DepartmentID: departmentID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": queue, "meta": gin.H{}})
}

// DisplayScheduleHandler godoc
// @Summary Today's active physician schedules for a department
// @Tags display
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param department_id query string true "Department UUID"
// @Success 200 {object} apiResponse
// @Router /display/schedule [get]
func DisplayScheduleHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermScheduleManage, merchantID) {
		return
	}
	departmentID, ok := parseUUID(c.Query("department_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department_id query param is required"})
		return
	}
	today := time.Now()
	dayOfWeek := int16(today.Weekday())
	q := sqlcgen.New(TxFromContext(c))
	schedules, err := q.ListActiveSchedulesForDepartmentToday(c.Request.Context(), sqlcgen.ListActiveSchedulesForDepartmentTodayParams{
		MerchantID: merchantID, DepartmentID: departmentID, DayOfWeek: dayOfWeek,
		EffectiveFrom: pgtype.Date{Time: today, Valid: true},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, len(schedules))
	for i, s := range schedules {
		out[i] = toScheduleResponse(s)
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}
