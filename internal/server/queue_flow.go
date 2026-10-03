// internal/server/queue_flow.go
package server

import (
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermQueueConfigure guards queue flow/service-point/display-board setup
// (spec 2026-10-01-c-queue-flow-display §5, permission id …031, migration 000057).
const PermQueueConfigure = "operations.queue.configure"

// validStageKinds mirrors the queue_stage.kind CHECK constraint — validated in
// Go so a bad kind is a 400, not a 500 from the DB check.
var validStageKinds = map[string]bool{
	"admission": true, "checkin": true, "nurse": true, "physician": true,
	"cashier": true, "pharmacy": true, "support": true, "custom": true,
}

var stagePrefixFormat = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)

func RegisterQueueFlowRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/queue-flows", ListQueueFlowsHandler)
	rg.POST("/queue-flows", CreateQueueFlowHandler)
	rg.PATCH("/queue-flows/:id", UpdateQueueFlowHandler)
	rg.DELETE("/queue-flows/:id", DeleteQueueFlowHandler)
	rg.PUT("/queue-flows/:id/stages", ReplaceQueueFlowStagesHandler)
	rg.POST("/queue-flows/from-preset/:preset", CreateQueueFlowFromPresetHandler)
}

// RequireAnyPermissionForMerchant grants access when the caller holds ANY of
// codes at merchantID (active-role scoped, same semantics as
// RequirePermissionForMerchant). Writes the 403 and returns false otherwise.
func RequireAnyPermissionForMerchant(c *gin.Context, merchantID pgtype.UUID, codes ...string) bool {
	q := sqlcgen.New(TxFromContext(c))
	roleID := AuthRoleID(c)
	if !roleID.Valid {
		def, err := q.DefaultUserRole(c.Request.Context(), sqlcgen.DefaultUserRoleParams{
			UserID: AuthUserID(c), MerchantID: merchantID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if !def.Valid {
			c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + codes[0]})
			return false
		}
		roleID = def
	}
	for _, code := range codes {
		has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
			UserID: AuthUserID(c), MerchantID: merchantID, Code: code, RoleID: roleID,
		})
		if err != nil {
			respondInternalError(c, err)
			return false
		}
		if has {
			return true
		}
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "missing permission: " + codes[0]})
	return false
}

// queueFlowJSON renders a flow snake_case — sqlcgen structs are never returned
// raw (their fields marshal PascalCase).
func queueFlowJSON(f sqlcgen.OperationsQueueFlow) gin.H {
	return gin.H{
		"id":             f.ID,
		"merchant_id":    f.MerchantID,
		"name":           f.Name,
		"service_types":  f.ServiceTypes,
		"department_ids": f.DepartmentIds,
		"is_default":     f.IsDefault,
		"is_active":      f.IsActive,
	}
}

func queueStageJSON(s sqlcgen.OperationsQueueStage) gin.H {
	out := gin.H{
		"id":                   s.ID,
		"flow_id":              s.FlowID,
		"seq":                  s.Seq,
		"name":                 s.Name,
		"kind":                 s.Kind,
		"skippable":            s.Skippable,
		"requires_checkin":     s.RequiresCheckin,
		"served_by_permission": s.ServedByPermission,
	}
	if s.NumberPrefix.Valid {
		out["number_prefix"] = s.NumberPrefix.String
	} else {
		out["number_prefix"] = nil
	}
	if s.BpjsTaskStart.Valid {
		out["bpjs_task_start"] = s.BpjsTaskStart.Int16
	} else {
		out["bpjs_task_start"] = nil
	}
	if s.BpjsTaskEnd.Valid {
		out["bpjs_task_end"] = s.BpjsTaskEnd.Int16
	} else {
		out["bpjs_task_end"] = nil
	}
	return out
}

type stageInput struct {
	ID                 string  `json:"id"`
	Seq                int32   `json:"seq"`
	Name               string  `json:"name"`
	Kind               string  `json:"kind"`
	NumberPrefix       *string `json:"number_prefix"`
	Skippable          bool    `json:"skippable"`
	RequiresCheckin    bool    `json:"requires_checkin"`
	BpjsTaskStart      *int32  `json:"bpjs_task_start"`
	BpjsTaskEnd        *int32  `json:"bpjs_task_end"`
	ServedByPermission string  `json:"served_by_permission" binding:"required"`
}

// validateStageInput enforces the contract rules that must be 400 (not DB
// errors): known kind, prefix format, BPJS task range/order, and a
// served_by_permission code that exists in core.permission.
func validateStageInput(c *gin.Context, q *sqlcgen.Queries, s stageInput) bool {
	if s.Name == "" || s.Seq < 1 || !validStageKinds[s.Kind] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage: name, seq >= 1 and a known kind are required"})
		return false
	}
	if s.NumberPrefix != nil && !stagePrefixFormat.MatchString(*s.NumberPrefix) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "number_prefix must match ^[A-Z0-9]{1,4}$"})
		return false
	}
	if (s.BpjsTaskStart == nil) != (s.BpjsTaskEnd == nil) ||
		(s.BpjsTaskStart != nil && (*s.BpjsTaskStart < 1 || *s.BpjsTaskStart > 7 || *s.BpjsTaskEnd < 1 || *s.BpjsTaskEnd > 7 || *s.BpjsTaskStart > *s.BpjsTaskEnd)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bpjs_task_start/end must both be set, within 1..7, start <= end"})
		return false
	}
	if _, err := q.GetPermissionByCode(c.Request.Context(), s.ServedByPermission); err != nil {
		if err == pgx.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown served_by_permission: " + s.ServedByPermission})
			return false
		}
		respondInternalError(c, err)
		return false
	}
	return true
}

func stageInputToCreateParams(companyID, merchantID, flowID pgtype.UUID, s stageInput) sqlcgen.CreateQueueStageFullParams {
	return sqlcgen.CreateQueueStageFullParams{
		CompanyID: companyID, MerchantID: merchantID, FlowID: flowID,
		Seq: s.Seq, Name: s.Name, Kind: s.Kind,
		NumberPrefix: optTextPtr(s.NumberPrefix),
		Skippable:    s.Skippable, RequiresCheckin: s.RequiresCheckin,
		BpjsTaskStart:      optInt2Ptr(s.BpjsTaskStart),
		BpjsTaskEnd:        optInt2Ptr(s.BpjsTaskEnd),
		ServedByPermission: s.ServedByPermission,
	}
}

// optTextPtr/optInt2Ptr — small pgtype converters for pointer request fields.
func optTextPtr(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: *p != ""}
}

func optInt2Ptr(p *int32) pgtype.Int2 {
	if p == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: int16(*p), Valid: true}
}

type createQueueFlowRequest struct {
	MerchantID    string       `json:"merchant_id" binding:"required"`
	Name          string       `json:"name" binding:"required"`
	ServiceTypes  []string     `json:"service_types"`
	DepartmentIDs []string     `json:"department_ids"`
	IsDefault     bool         `json:"is_default"`
	IsActive      *bool        `json:"is_active"`
	Stages        []stageInput `json:"stages"`
}

// CreateQueueFlowHandler godoc
// @Summary Create a queue flow, optionally with its stages
// @Tags queue-flow
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createQueueFlowRequest true "Flow data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /queue-flows [post]
func CreateQueueFlowHandler(c *gin.Context) {
	var req createQueueFlowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, merchantID) {
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	serviceTypes := req.ServiceTypes
	if len(serviceTypes) == 0 {
		serviceTypes = []string{"umum", "bpjs", "asuransi"}
	}
	deptIDs := make([]pgtype.UUID, 0, len(req.DepartmentIDs))
	for _, raw := range req.DepartmentIDs {
		id, ok := parseUUID(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_ids entry"})
			return
		}
		deptIDs = append(deptIDs, id)
	}
	for _, s := range req.Stages {
		if !validateStageInput(c, q, s) {
			return
		}
	}
	flow, err := q.CreateQueueFlow(ctx, sqlcgen.CreateQueueFlowParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Name: req.Name,
		ServiceTypes: serviceTypes, DepartmentIds: deptIDs,
		IsDefault: req.IsDefault, IsActive: isActive, CreatedBy: AuthUserID(c),
	})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "flow name already exists or default flow already set"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	stages := []gin.H{}
	for _, s := range req.Stages {
		st, err := q.CreateQueueStageFull(ctx, stageInputToCreateParams(AuthCompanyID(c), merchantID, flow.ID, s))
		if err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
		stages = append(stages, queueStageJSON(st))
	}
	out := queueFlowJSON(flow)
	out["stages"] = stages
	c.JSON(http.StatusCreated, gin.H{"data": out, "meta": gin.H{}})
}

// ListQueueFlowsHandler godoc
// @Summary List a merchant's queue flows with their stages
// @Tags queue-flow
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/queue-flows [get]
func ListQueueFlowsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, merchantID) {
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	flows, err := q.ListMerchantQueueFlows(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, 0, len(flows))
	for _, f := range flows {
		stages, err := q.ListQueueStagesByFlow(ctx, f.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		entry := queueFlowJSON(f)
		stageList := make([]gin.H, 0, len(stages))
		for _, s := range stages {
			stageList = append(stageList, queueStageJSON(s))
		}
		entry["stages"] = stageList
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}

type updateQueueFlowRequest struct {
	Name      *string `json:"name"`
	IsDefault *bool   `json:"is_default"`
	IsActive  *bool   `json:"is_active"`
}

// UpdateQueueFlowHandler godoc
// @Summary Update a queue flow
// @Tags queue-flow
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Flow UUID"
// @Param request body updateQueueFlowRequest true "Flow fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /queue-flows/{id} [patch]
func UpdateQueueFlowHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req updateQueueFlowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	flow, err := q.GetQueueFlowByID(ctx, id)
	if err == pgx.ErrNoRows {
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
	name := flow.Name
	if req.Name != nil {
		name = *req.Name
	}
	updated, err := q.UpdateQueueFlow(ctx, sqlcgen.UpdateQueueFlowParams{
		ID: id, Name: name,
		IsDefault: req.IsDefault != nil && *req.IsDefault,
		IsActive:  req.IsActive == nil || *req.IsActive,
		UpdatedBy: AuthUserID(c),
	})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "flow name already exists or default flow already set"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": queueFlowJSON(updated), "meta": gin.H{}})
}

// DeleteQueueFlowHandler godoc
// @Summary Soft-delete a queue flow
// @Description Refused with 409 while journeys of the flow are in_progress/awaiting_checkin.
// @Tags queue-flow
// @Security BearerAuth
// @Param id path string true "Flow UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /queue-flows/{id} [delete]
func DeleteQueueFlowHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	flow, err := q.GetQueueFlowByID(ctx, id)
	if err == pgx.ErrNoRows {
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
	active, err := q.CountActiveJourneysForFlow(ctx, id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if active > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "flow has active journeys"})
		return
	}
	if err := q.SoftDeleteQueueFlow(ctx, sqlcgen.SoftDeleteQueueFlowParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type replaceStagesRequest struct {
	Stages []stageInput `json:"stages" binding:"required,min=1,dive"`
}

// ReplaceQueueFlowStagesHandler godoc
// @Summary Replace the full stage list of a flow
// @Description Entries with id update existing stages, entries without id are
// @Description inserted, existing stages missing from the list are deleted —
// @Description all in one transaction. Deleting a stage still referenced by
// @Description any ticket is refused with 409.
// @Tags queue-flow
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Flow UUID"
// @Param request body replaceStagesRequest true "Stage list"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /queue-flows/{id}/stages [put]
func ReplaceQueueFlowStagesHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req replaceStagesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	flow, err := q.GetQueueFlowByID(ctx, id)
	if err == pgx.ErrNoRows {
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
	kept := map[pgtype.UUID]bool{}
	for _, s := range req.Stages {
		if !validateStageInput(c, q, s) {
			return
		}
		if s.ID != "" {
			sid, ok := parseUUID(s.ID)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage id"})
				return
			}
			kept[sid] = true
		}
	}
	existing, err := q.ListQueueStagesByFlow(ctx, id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Stages dropped from the list must not be referenced by any ticket — the
	// FK from operations.queue.stage_id would abort the transaction.
	var removed []pgtype.UUID
	for _, ex := range existing {
		if !kept[ex.ID] {
			removed = append(removed, ex.ID)
		}
	}
	if len(removed) > 0 {
		inUse, err := q.CountTicketsForStages(ctx, removed)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if inUse > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "stage in use by existing tickets"})
			return
		}
	}
	out := make([]gin.H, 0, len(req.Stages))
	for _, s := range req.Stages {
		if s.ID == "" {
			st, err := q.CreateQueueStageFull(ctx, stageInputToCreateParams(flow.CompanyID, flow.MerchantID, flow.ID, s))
			if err != nil {
				c.Error(err)
				respondInternalError(c, err)
				return
			}
			out = append(out, queueStageJSON(st))
			continue
		}
		sid, _ := parseUUID(s.ID)
		st, err := q.UpdateQueueStageFull(ctx, sqlcgen.UpdateQueueStageFullParams{
			ID: sid, Seq: s.Seq, Name: s.Name, Kind: s.Kind,
			NumberPrefix: optTextPtr(s.NumberPrefix), Skippable: s.Skippable,
			RequiresCheckin: s.RequiresCheckin,
			BpjsTaskStart:   optInt2Ptr(s.BpjsTaskStart), BpjsTaskEnd: optInt2Ptr(s.BpjsTaskEnd),
			ServedByPermission: s.ServedByPermission,
		})
		if err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
		out = append(out, queueStageJSON(st))
	}
	keptIDs := make([]pgtype.UUID, 0, len(kept))
	for sid := range kept {
		keptIDs = append(keptIDs, sid)
	}
	if err := q.DeleteQueueStagesNotIn(ctx, sqlcgen.DeleteQueueStagesNotInParams{FlowID: id, Column2: keptIDs}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"stages": out}, "meta": gin.H{}})
}
