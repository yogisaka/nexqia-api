// internal/server/queue.go
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

// PermVisitManage guards operations.admission + operations.queue — the FO/nurse/
// doctor daily-use surface, kept as one permission since both move together as
// "the kunjungan" in v1's flow. See
// 2026-09-16-v1-operations-antrian-jadwal-design.md §6.
const PermVisitManage = "operations.visit.manage"

// queueValidTransitions is the linear queue status state machine — see spec §7.
// "skipped" is only reachable through POST /queue/:id/skip (stage.skippable).
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
	rg.POST("/queue/:id/skip", SkipQueueHandler)
	rg.POST("/queue/:id/recall", RecallQueueHandler)
	rg.POST("/queue/checkin", CheckInQueueHandler)
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
		respondInternalError(c, err)
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
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, queue.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": queue, "meta": gin.H{}})
}

type updateQueueRequest struct {
	Status   string `json:"status"`
	Priority *bool  `json:"priority"`
}

// UpdateQueueStatusHandler drives the queue state machine (spec §7). On a
// transition into "done" it finishes the ticket and advances its journey to
// the next flow stage (queue_engine.go) — this handler does several writes in
// one request; every write after the first calls c.Error(err) on failure so
// TenantMiddleware rolls back instead of committing a partial pipeline step
// (see plan's Global Constraints). A "priority" field without status just
// re-queues the ticket ahead of same-stage waiters.
// UpdateQueueStatusHandler godoc
// @Summary Advance a queue entry's status (or set priority)
// @Description Drives the queue state machine. On a transition into "done" it finishes the ticket and advances its journey to the next flow stage (or awaits check-in / completes the journey) — response includes queue, next_stage_queue and journey. A "priority" field (no status) moves the ticket ahead in its stage.
// @Tags queue
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue UUID"
// @Param request body updateQueueRequest true "New status and/or priority"
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
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	var req updateQueueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Status == "" && req.Priority == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status or priority is required"})
		return
	}
	if req.Priority != nil {
		existing, err = q.UpdateQueuePriority(ctx, sqlcgen.UpdateQueuePriorityParams{
			ID: id, Priority: *req.Priority, UpdatedBy: AuthUserID(c),
		})
		if err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
	}

	res := stageAdvance{Ticket: &existing}
	if req.Status != "" {
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
		switch req.Status {
		case "done":
			adv, err := advanceStageTicket(ctx, q, existing, req.Status, AuthUserID(c))
			if err != nil {
				c.Error(err)
				respondInternalError(c, err)
				return
			}
			res = adv
		case "in_progress":
			started, err := q.StartQueueTicket(ctx, sqlcgen.StartQueueTicketParams{ID: id, ServedBy: AuthUserID(c)})
			if err != nil {
				c.Error(err)
				respondInternalError(c, err)
				return
			}
			res.Ticket = &started
		default:
			updated, err := q.UpdateQueueStatus(ctx, sqlcgen.UpdateQueueStatusParams{
				ID: id, Status: req.Status, CounterID: existing.CounterID, CalledAt: existing.CalledAt, UpdatedBy: AuthUserID(c),
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			res.Ticket = &updated
		}
		if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
			QueueID: id, CounterID: existing.CounterID, FromStatus: optText(existing.Status), ToStatus: req.Status, ChangedBy: AuthUserID(c),
		}); err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"queue": res.Ticket, "next_stage_queue": res.Next, "journey": res.Journey,
	}, "meta": gin.H{}})
}

// SkipQueueHandler marks a skippable stage's ticket as skipped (spec §4) and
// advances the journey exactly like a "done" transition. Only flow tickets
// (with journey_id and stage) whose stage allows skipping can be skipped.
// SkipQueueHandler godoc
// @Summary Skip a queue entry's stage
// @Description Marks the ticket 'skipped' (finished_at set) and advances the journey to the next flow stage; only allowed when the ticket's stage has skippable=true.
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse "stage not skippable / invalid transition"
// @Router /queue/{id}/skip [post]
func SkipQueueHandler(c *gin.Context) {
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
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	if !existing.JourneyID.Valid || !existing.StageID.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "stage not skippable"})
		return
	}
	stage, err := q.GetQueueStageByID(ctx, existing.StageID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !stage.Skippable {
		c.JSON(http.StatusConflict, gin.H{"error": "stage not skippable"})
		return
	}
	switch existing.Status {
	case "waiting", "called", "in_progress":
	default:
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition from " + existing.Status + " to skipped"})
		return
	}
	res, err := advanceStageTicket(ctx, q, existing, "skipped", AuthUserID(c))
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
		QueueID: id, CounterID: existing.CounterID, FromStatus: optText(existing.Status), ToStatus: "skipped", ChangedBy: AuthUserID(c),
	}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"queue": res.Ticket, "next_stage_queue": res.Next, "journey": res.Journey,
	}, "meta": gin.H{}})
}

// RecallQueueHandler re-announces the ticket as called (spec §4): the queue
// row returns to 'called' keeping its counter, and the transition is logged
// in queue_status_history with from=to='called' when already called.
// RecallQueueHandler godoc
// @Summary Recall a called/in-progress queue entry
// @Description Sets the ticket back to 'called' (keeps its counter) and logs the transition in queue_status_history.
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param id path string true "Queue UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse "cannot recall"
// @Router /queue/{id}/recall [post]
func RecallQueueHandler(c *gin.Context) {
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
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	if existing.Status != "called" && existing.Status != "in_progress" {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot recall from " + existing.Status})
		return
	}
	updated, err := q.UpdateQueueStatus(ctx, sqlcgen.UpdateQueueStatusParams{
		ID: id, Status: "called", CounterID: existing.CounterID,
		CalledAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
		QueueID: id, CounterID: existing.CounterID, FromStatus: optText(existing.Status), ToStatus: "called", ChangedBy: AuthUserID(c),
	}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": updated, "meta": gin.H{}})
}

type checkInQueueRequest struct {
	JourneyID   string `json:"journey_id"`
	AdmissionID string `json:"admission_id"`
	CounterID   string `json:"counter_id" binding:"required"`
}

// CheckInQueueHandler issues the ticket for a journey awaiting check-in
// (spec §4): the journey must be 'awaiting_checkin' and the counter must
// belong to the journey's current stage; otherwise 409 "nothing to check in".
// CheckInQueueHandler godoc
// @Summary Check a patient in at a stage (issues the stage ticket)
// @Description For a journey in 'awaiting_checkin': validates that the counter belongs to the journey's current stage, creates the stage ticket and moves the journey back to 'in_progress'.
// @Tags queue
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body checkInQueueRequest true "Journey (or admission) + counter"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse "nothing to check in"
// @Router /queue/checkin [post]
func CheckInQueueHandler(c *gin.Context) {
	var req checkInQueueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	counterID, ok := parseUUID(req.CounterID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counter_id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))

	var journey sqlcgen.OperationsQueueJourney
	switch {
	case req.JourneyID != "":
		jid, ok := parseUUID(req.JourneyID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid journey_id"})
			return
		}
		j, err := q.GetQueueJourneyByID(ctx, jid)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "journey not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		journey = j
	case req.AdmissionID != "":
		aid, ok := parseUUID(req.AdmissionID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
			return
		}
		j, err := q.GetQueueJourneyByAdmission(ctx, aid)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "journey not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		journey = j
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "journey_id or admission_id is required"})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, journey.MerchantID) {
		return
	}
	counter, err := q.GetCounterByID(ctx, counterID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "counter not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if journey.Status != "awaiting_checkin" || !counter.StageID.Valid ||
		!journey.CurrentStageID.Valid || counter.StageID != journey.CurrentStageID ||
		counter.MerchantID != journey.MerchantID {
		c.JSON(http.StatusConflict, gin.H{"error": "nothing to check in"})
		return
	}
	stage, err := q.GetQueueStageByID(ctx, journey.CurrentStageID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ticket, err := issueTicket(ctx, q, &journey, stage, pgtype.UUID{})
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	updated, err := q.UpdateQueueJourneyStage(ctx, sqlcgen.UpdateQueueJourneyStageParams{
		ID: journey.ID, CurrentStageID: stage.ID, Status: "in_progress", UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"queue": ticket, "journey": updated}, "meta": gin.H{}})
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
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": board, "meta": gin.H{"limit": limit, "offset": offset}})
}
