// internal/server/counter.go
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermCounterManage guards operations.counter CRUD and the call-next operator
// action — see 2026-09-16-v1-operations-antrian-jadwal-design.md §2.
const PermCounterManage = "operations.counter.manage"

func RegisterCounterRoutes(rg *gin.RouterGroup) {
	rg.POST("/counters", CreateCounterHandler)
	rg.GET("/merchants/:id/counters", ListCountersHandler)
	rg.GET("/counters/:id", GetCounterHandler)
	rg.PATCH("/counters/:id", UpdateCounterHandler)
	rg.POST("/counters/:id/call-next", CallNextQueueHandler)
}

type createCounterRequest struct {
	MerchantID   string `json:"merchant_id" binding:"required"`
	QueueType    string `json:"queue_type" binding:"required"`
	DepartmentID string `json:"department_id"`
	Code         string `json:"code" binding:"required"`
	Name         string `json:"name"`
}

// CreateCounterHandler godoc
// @Summary Create a counter (loket)
// @Tags counter
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createCounterRequest true "Counter data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /counters [post]
func CreateCounterHandler(c *gin.Context) {
	var req createCounterRequest
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
	if !RequirePermissionForMerchant(c, PermCounterManage, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	counter, err := q.CreateCounter(c.Request.Context(), sqlcgen.CreateCounterParams{
		CompanyID:    AuthCompanyID(c),
		MerchantID:   merchantID,
		QueueType:    req.QueueType,
		DepartmentID: optUUID(req.DepartmentID),
		Code:         req.Code,
		Name:         optText(req.Name),
		CreatedBy:    AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": counter, "meta": gin.H{}})
}

// ListCountersHandler godoc
// @Summary List counters under a merchant
// @Tags counter
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/counters [get]
func ListCountersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermCounterManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	counters, err := q.ListCountersByMerchant(c.Request.Context(), sqlcgen.ListCountersByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": counters, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetCounterHandler godoc
// @Summary Get a counter by id
// @Tags counter
// @Produce json
// @Security BearerAuth
// @Param id path string true "Counter UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /counters/{id} [get]
func GetCounterHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	counter, err := q.GetCounterByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "counter not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermCounterManage, counter.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": counter, "meta": gin.H{}})
}

type updateCounterRequest struct {
	Code     string `json:"code" binding:"required"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
}

// UpdateCounterHandler godoc
// @Summary Update a counter
// @Tags counter
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Counter UUID"
// @Param request body updateCounterRequest true "Counter data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /counters/{id} [patch]
func UpdateCounterHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetCounterByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "counter not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermCounterManage, existing.MerchantID) {
		return
	}
	var req updateCounterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	counter, err := q.UpdateCounter(c.Request.Context(), sqlcgen.UpdateCounterParams{
		ID: id, Code: req.Code, Name: optText(req.Name), IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "counter not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": counter, "meta": gin.H{}})
}

// CallNextQueueHandler pulls the oldest 'waiting' queue row matching this
// counter's queue_type/department scope, marks it 'called', and logs the
// transition. See 2026-09-16-v1-operations-antrian-jadwal-design.md §4.
// CallNextQueueHandler godoc
// @Summary Call the next waiting queue entry at this counter
// @Description Pulls the oldest 'waiting' queue row matching this counter's queue_type/department scope, marks it 'called', and logs the transition.
// @Tags counter
// @Produce json
// @Security BearerAuth
// @Param id path string true "Counter UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse "no waiting queue for this counter"
// @Router /counters/{id}/call-next [post]
func CallNextQueueHandler(c *gin.Context) {
	counterID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	counter, err := q.GetCounterByID(c.Request.Context(), counterID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "counter not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermCounterManage, counter.MerchantID) {
		return
	}
	ctx := c.Request.Context()
	// Counter bound to a flow stage (spec 2026-10-01-c §4): the stage's
	// served_by_permission governs who may call, and the candidate pool is the
	// stage's waiting tickets addressed to this counter or unaddressed,
	// priority first, then check-in/creation order, with SKIP LOCKED so two
	// counters cannot pull the same ticket.
	if counter.StageID.Valid {
		stage, err := q.GetQueueStageByID(ctx, counter.StageID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if !RequirePermissionForMerchant(c, stage.ServedByPermission, counter.MerchantID) {
			return
		}
		next, err := q.CallNextFlowTicket(ctx, sqlcgen.CallNextFlowTicketParams{
			StageID: counter.StageID, Column2: pgtype.UUID{Bytes: counterID.Bytes, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no waiting queue for this counter"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		calledAt := pgtype.Timestamptz{Time: time.Now(), Valid: true}
		updated, err := q.UpdateQueueStatus(ctx, sqlcgen.UpdateQueueStatusParams{
			ID: next.ID, Status: "called", CounterID: pgtype.UUID{Bytes: counterID.Bytes, Valid: true},
			CalledAt: calledAt, UpdatedBy: AuthUserID(c),
		})
		if err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
		if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
			QueueID: next.ID, CounterID: pgtype.UUID{Bytes: counterID.Bytes, Valid: true},
			FromStatus: optText(next.Status), ToStatus: "called", ChangedBy: AuthUserID(c),
		}); err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": updated, "meta": gin.H{}})
		return
	}
	next, err := q.GetOldestWaitingQueue(ctx, sqlcgen.GetOldestWaitingQueueParams{
		MerchantID: counter.MerchantID, QueueType: counter.QueueType, DepartmentID: counter.DepartmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no waiting queue for this counter"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	calledAt := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	updated, err := q.UpdateQueueStatus(ctx, sqlcgen.UpdateQueueStatusParams{
		ID: next.ID, Status: "called", CounterID: pgtype.UUID{Bytes: counterID.Bytes, Valid: true},
		CalledAt: calledAt, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.CreateQueueStatusHistory(ctx, sqlcgen.CreateQueueStatusHistoryParams{
		QueueID: next.ID, CounterID: pgtype.UUID{Bytes: counterID.Bytes, Valid: true},
		FromStatus: optText(next.Status), ToStatus: "called", ChangedBy: AuthUserID(c),
	}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": updated, "meta": gin.H{}})
}
