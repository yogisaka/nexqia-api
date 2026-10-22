// internal/server/queue_flow_preview.go
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

// int2OrNil renders a nullable int2 column as int or nil (kecil, pelengkap
// textOrNil untuk render snake_case preview).
func int2OrNil(t pgtype.Int2) any {
	if !t.Valid {
		return nil
	}
	return t.Int16
}

// QueueFlowPreviewHandler godoc
// @Summary Preview queue flow: today's doctors and next numbers
// @Tags queue-flow
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue flow UUID"
// @Param department_id query string false "Department UUID (wajib bila tahap tanpa prefix ingin next_number)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /queue-flows/{id}/preview [get]
func QueueFlowPreviewHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	flow, err := q.GetQueueFlowByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue flow not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, flow.MerchantID) {
		return
	}
	loc, err := merchantLocation(c, q, flow.MerchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Read-only preview: no lockDailySequence, no DB writes.
	today := localDayStart(time.Now(), loc)
	todayDate := pgtype.Date{Time: today, Valid: true}
	dayOfWeek := int16(today.Weekday())

	var department gin.H
	var deptID pgtype.UUID
	deptCode := ""
	if raw := c.Query("department_id"); raw != "" {
		d, ok := parseUUID(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
			return
		}
		dept, err := q.GetDepartmentByID(ctx, d)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		deptID = d
		deptCode = dept.Code
		department = gin.H{"id": dept.ID, "code": dept.Code, "name": dept.Name}
	}

	stages, err := q.ListQueueStagesByFlow(ctx, flow.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	stageList := make([]gin.H, 0, len(stages))
	for _, st := range stages {
		n, err := q.CountTicketsForStageSince(ctx, sqlcgen.CountTicketsForStageSinceParams{
			StageID: st.ID, CreatedAt: pgtype.Timestamptz{Time: today, Valid: true},
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var next any
		switch {
		case st.NumberPrefix.Valid:
			next = fmt.Sprintf("%s-%03d", st.NumberPrefix.String, n+1)
		case deptID.Valid:
			next = fmt.Sprintf("%s-%03d", deptCode, n+1)
		default:
			next = nil
		}
		stageList = append(stageList, gin.H{
			"stage_id":         st.ID,
			"seq":              st.Seq,
			"name":             st.Name,
			"kind":             st.Kind,
			"skippable":        st.Skippable,
			"requires_checkin": st.RequiresCheckin,
			"bpjs_task_start":  int2OrNil(st.BpjsTaskStart),
			"bpjs_task_end":    int2OrNil(st.BpjsTaskEnd),
			"number_prefix":    textOrNil(st.NumberPrefix),
			"next_number":      next,
		})
	}

	bindings, err := q.ListTodayScheduleBindingsForFlow(ctx, sqlcgen.ListTodayScheduleBindingsForFlowParams{
		MerchantID: flow.MerchantID, DayOfWeek: dayOfWeek,
		SessionDate: todayDate, EffectiveFrom: todayDate,
		Column5: flow.DepartmentIds,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	doctors := make([]gin.H, 0, len(bindings))
	for _, b := range bindings {
		doctors = append(doctors, gin.H{
			"department_id":   b.DepartmentID,
			"department_name": b.DepartmentName,
			"physician_id":    b.PhysicianID,
			"physician_name":  b.PhysicianName,
			"room_name":       textOrNil(b.RoomName),
			"start_time":      formatTimeOfDay(b.StartTime),
			"end_time":        formatTimeOfDay(b.EndTime),
			"status":          textOrNil(b.SessionStatus),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"date":       today.Format("2006-01-02"),
			"department": department,
			"stages":     stageList,
			"doctors":    doctors,
		},
		"meta": gin.H{},
	})
}
