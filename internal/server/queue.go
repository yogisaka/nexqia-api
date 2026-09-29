// internal/server/queue.go
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermVisitManage guards operations.admission + operations.queue — the FO/nurse/
// doctor daily-use surface, kept as one permission since both move together as
// "the kunjungan" in v1's flow. See
// docs/design/specs/2026-09-16-v1-operations-antrian-jadwal-design.md §6.
const PermVisitManage = "operations.visit.manage"

// queueStagePipeline is the fixed v1 Poli Umum stage order. Sub-project #4/#5
// append "kasir"/"farmasi" here when they're implemented — no migration needed,
// operations.queue already supports arbitrary queue_type values. See spec §9.
var queueStagePipeline = []string{"pendaftaran", "perawat", "dokter"}

// nextQueueStage returns the stage after current, or ("", false) if current is
// the last stage in the pipeline (or not in it).
func nextQueueStage(current string) (string, bool) {
	for i, s := range queueStagePipeline {
		if s == current && i+1 < len(queueStagePipeline) {
			return queueStagePipeline[i+1], true
		}
	}
	return "", false
}

// queueValidTransitions is the linear queue status state machine — see spec §7.
var queueValidTransitions = map[string][]string{
	"waiting":     {"called", "cancelled"},
	"called":      {"in_progress", "cancelled"},
	"in_progress": {"done", "cancelled"},
}

func RegisterQueueRoutes(rg *gin.RouterGroup) {
	rg.GET("/queue", AccessLog("queue", "list", ""), ListQueueHandler)
	rg.GET("/queue/board", ListQueueBoardHandler)
	rg.GET("/queue/:id", AccessLog("queue", "view", "id"), GetQueueHandler)
	rg.PATCH("/queue/:id", UpdateQueueStatusHandler)
}

// ListQueueHandler godoc
// @Summary List queue entries for a work stage
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param queue_type query string true "Queue stage (pendaftaran/perawat/dokter)"
// @Param status query string true "Queue status"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /queue [get]
func ListQueueHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	queueType := c.Query("queue_type")
	status := c.Query("status")
	if queueType == "" || status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "queue_type and status query params are required"})
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	queue, err := q.ListQueueForWork(c.Request.Context(), sqlcgen.ListQueueForWorkParams{
		MerchantID: merchantID, QueueType: queueType, Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": queue, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetQueueHandler godoc
// @Summary Get a queue entry by id
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /queue/{id} [get]
func GetQueueHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	queue, err := q.GetQueueByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, queue.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": queue, "meta": gin.H{}})
}

type updateQueueStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// UpdateQueueStatusHandler drives the queue state machine (spec §7). On a
// transition into "done" it creates the next pipeline stage's queue row
// (spec §4 point 4) — this handler does 3 writes in one request
// (UpdateQueueStatus, CreateQueueStatusHistory, optionally CreateQueue for the
// next stage); every write after the first calls c.Error(err) on failure so
// TenantMiddleware rolls back instead of committing a partial pipeline step
// (see plan's Global Constraints).
// UpdateQueueStatusHandler godoc
// @Summary Advance a queue entry's status
// @Description Drives the queue state machine. On a transition into "done" it also creates the next pipeline stage's queue row — response includes both queue and next_stage_queue.
// @Tags queue
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue UUID"
// @Param request body updateQueueStatusRequest true "New status"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse "invalid status transition"
// @Router /queue/{id} [patch]
func UpdateQueueStatusHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetQueueByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "queue not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	var req updateQueueStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	allowed, ok := queueValidTransitions[existing.Status]
	valid := false
	if ok {
		for _, s := range allowed {
			if s == req.Status {
				valid = true
			}
		}
	}
	if !valid {
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition from " + existing.Status + " to " + req.Status})
		return
	}
	updated, err := q.UpdateQueueStatus(ctx, sqlcgen.UpdateQueueStatusParams{
		ID: id, Status: req.Status, CounterID: existing.CounterID, CalledAt: existing.CalledAt, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
		QueueID: id, CounterID: existing.CounterID, FromStatus: optText(existing.Status), ToStatus: req.Status, ChangedBy: AuthUserID(c),
	}); err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var nextQueue *sqlcgen.OperationsQueue
	if req.Status == "done" {
		if nextType, has := nextQueueStage(existing.QueueType); has {
			department, err := q.GetDepartmentByID(ctx, existing.DepartmentID)
			if err != nil {
				c.Error(err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			seq, err := q.CountTodayQueueByType(ctx, sqlcgen.CountTodayQueueByTypeParams{
				MerchantID: existing.MerchantID, QueueType: nextType, DepartmentID: existing.DepartmentID,
			})
			if err != nil {
				c.Error(err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			queueNumber := fmt.Sprintf("%s-%03d", department.Code, seq+1)
			created, err := q.CreateQueue(ctx, sqlcgen.CreateQueueParams{
				CompanyID: existing.CompanyID, MerchantID: existing.MerchantID, QueueType: nextType,
				DepartmentID: existing.DepartmentID, PersonID: existing.PersonID, AdmissionID: existing.AdmissionID,
				QueueNumber: queueNumber, CreatedBy: AuthUserID(c),
			})
			if err != nil {
				c.Error(err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			nextQueue = &created
		}
	}

	resp := gin.H{"data": gin.H{"queue": updated, "next_stage_queue": nextQueue}, "meta": gin.H{}}
	c.JSON(http.StatusOK, resp)
}

// ListQueueBoardHandler godoc
// @Summary List queue entries with person/physician names joined (staff board/dashboard)
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param queue_type query string true "Queue stage (pendaftaran/perawat/dokter)"
// @Param date query string false "Filter by check-in date, YYYY-MM-DD (omit for all dates)"
// @Param status query string false "Comma-separated statuses (e.g. waiting,called,in_progress); omit for all"
// @Param search query string false "Filter by patient name or medical record no (partial match)"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /queue/board [get]
func ListQueueBoardHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	queueType := c.Query("queue_type")
	if queueType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "queue_type query param is required"})
		return
	}
	date, err := optDate(c.Query("date"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, expected YYYY-MM-DD"})
		return
	}
	var statuses []string
	if raw := c.Query("status"); raw != "" {
		statuses = strings.Split(raw, ",")
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	board, err := q.ListQueueBoard(c.Request.Context(), sqlcgen.ListQueueBoardParams{
		MerchantID: merchantID, QueueType: queueType, Date: date, Statuses: statuses,
		Search: optText(c.Query("search")), Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": board, "meta": gin.H{"limit": limit, "offset": offset}})
}
