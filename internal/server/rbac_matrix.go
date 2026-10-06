// internal/server/rbac_matrix.go
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// roleMatrixWidgetKeys mirrors DashboardWidgetKey in nexqia-his
// src/lib/dashboardConfig.ts — keep both lists in sync.
var roleMatrixWidgetKeys = map[string]struct{}{
	"kpi": {}, "queue": {}, "counterCall": {}, "quickActions": {},
	"doctorSchedule": {}, "visitAnalytics": {}, "patientType": {}, "notifications": {},
}

// roleMatrixConflictMessage is the optimistic-lock failure of PUT
// /roles/:id/matrix; nexqia-his matches it (ROLE_MATRIX_CONFLICT_MESSAGE) to
// offer a reload.
const roleMatrixConflictMessage = "role was changed by someone else, reload and try again"

type roleWidgetOverrideInput struct {
	Key     string `json:"key"`
	Visible bool   `json:"visible"`
}

type saveRoleMatrixRequest struct {
	RowVersion            *int32                    `json:"row_version" binding:"required"`
	Name                  string                    `json:"name"`
	Description           string                    `json:"description"`
	RequiresPhysicianData bool                      `json:"requires_physician_data"`
	PermissionIDs         []string                  `json:"permission_ids" binding:"required"`
	Widgets               []roleWidgetOverrideInput `json:"widgets" binding:"required"`
	Reason                string                    `json:"reason"`
}

// SaveRoleMatrixHandler godoc
// @Summary Save a role's details, permission set and widget overrides atomically
// @Description One request = one transaction = at most one role_change_log row (spec 2026-10-06-role-matrix-fixes §2.1). permission_ids and widgets are the complete final sets (empty arrays allowed, absent/null rejected). row_version is the optimistic lock from GET /roles/{id}/matrix (stale → 409). System roles: details must stay unchanged (409). No change → 200 without writing or logging. Response = the GET /roles/{id}/matrix body.
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Param request body saveRoleMatrixRequest true "Final role matrix + reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /roles/{id}/matrix [put]
func SaveRoleMatrixHandler(c *gin.Context) {
	roleID, role, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	var req saveRoleMatrixRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	if *req.RowVersion != role.RowVersion {
		c.JSON(http.StatusConflict, gin.H{"error": roleMatrixConflictMessage})
		return
	}

	// Details: compared trimmed; an empty description means NULL.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	description := strings.TrimSpace(req.Description)
	oldDescription := ""
	if role.Description.Valid {
		oldDescription = role.Description.String
	}
	meta := map[string]any{}
	if name != strings.TrimSpace(role.Name) {
		meta["name"] = map[string]any{"from": role.Name, "to": name}
	}
	if description != strings.TrimSpace(oldDescription) {
		meta["description"] = map[string]any{"from": oldDescription, "to": description}
	}
	if req.RequiresPhysicianData != role.RequiresPhysicianData {
		meta["requires_physician_data"] = map[string]any{"from": role.RequiresPhysicianData, "to": req.RequiresPhysicianData}
	}
	if role.IsSystem && len(meta) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "system role details cannot be changed"})
		return
	}

	// Permissions: ids must be unique UUIDs from the catalog. ids is
	// non-nil so an empty set is sent as '{}' — a nil slice would encode as
	// NULL and DeleteRolePermissionsNotIn would delete nothing.
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	ids := make([]pgtype.UUID, 0, len(req.PermissionIDs))
	seen := make(map[pgtype.UUID]struct{}, len(req.PermissionIDs))
	for _, raw := range req.PermissionIDs {
		id, valid := parseUUID(raw)
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_ids"})
			return
		}
		if _, dup := seen[id]; dup {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate permission id"})
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	newCodes, err := q.ListPermissionCodesByIDs(ctx, ids)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(newCodes) != len(ids) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_ids"})
		return
	}
	oldCodes, err := q.ListPermissionCodesByRole(ctx, roleID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	added, removed := codeSetDiff(oldCodes, newCodes)
	permChanged := len(added)+len(removed) > 0

	// Widgets: the complete override set (absent key = permission default).
	newWidgets := make(map[string]bool, len(req.Widgets))
	for _, w := range req.Widgets {
		if _, known := roleMatrixWidgetKeys[w.Key]; !known {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown widget key"})
			return
		}
		if _, dup := newWidgets[w.Key]; dup {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate widget key"})
			return
		}
		newWidgets[w.Key] = w.Visible
	}
	oldRows, err := q.ListRoleWidgets(ctx, roleID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	oldWidgets := make(map[string]bool, len(oldRows))
	for _, row := range oldRows {
		oldWidgets[row.WidgetKey] = row.Visible
	}
	widgetChanges := roleWidgetDiff(oldWidgets, newWidgets)

	if len(meta) == 0 && !permChanged && len(widgetChanges) == 0 {
		respondRoleMatrix(c, role)
		return
	}

	// Write 1: details + row touch under the optimistic lock. Unchanged
	// details are written back exactly as stored (no trim).
	params := sqlcgen.TouchRoleForMatrixSaveParams{
		ID: roleID, RowVersion: role.RowVersion, Name: role.Name, Description: role.Description,
		RequiresPhysicianData: req.RequiresPhysicianData, UpdatedBy: AuthUserID(c),
	}
	if _, changed := meta["name"]; changed {
		params.Name = name
	}
	if _, changed := meta["description"]; changed {
		params.Description = pgtype.Text{String: description, Valid: description != ""}
	}
	updated, err := q.TouchRoleForMatrixSave(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": roleMatrixConflictMessage})
		return
	}
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "role name already exists"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Write 2: the exact permission set.
	if permChanged {
		if _, err := q.DeleteRolePermissionsNotIn(ctx, sqlcgen.DeleteRolePermissionsNotInParams{
			RoleID: roleID, Column2: ids,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		for _, id := range ids {
			if err := q.AddRolePermission(ctx, sqlcgen.AddRolePermissionParams{
				RoleID: roleID, PermissionID: id,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
	}
	// Write 3: the exact widget override set.
	if len(widgetChanges) > 0 {
		if _, err := q.DeleteRoleWidgets(ctx, roleID); err != nil {
			respondInternalError(c, err)
			return
		}
		keys := make([]string, 0, len(newWidgets))
		for key := range newWidgets {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := q.AddRoleWidget(ctx, sqlcgen.AddRoleWidgetParams{
				RoleID: roleID, WidgetKey: key, Visible: newWidgets[key],
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
	}
	// Write 4: one change-log row with only the parts that changed.
	changes := map[string]any{}
	if len(meta) > 0 {
		changes["meta"] = meta
	}
	if permChanged {
		changes["permission"] = map[string]any{"added": added, "removed": removed}
	}
	if len(widgetChanges) > 0 {
		changes["widget"] = widgetChanges
	}
	raw, err := json.Marshal(changes)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.InsertRoleChangeLog(ctx, sqlcgen.InsertRoleChangeLogParams{
		CompanyID: role.CompanyID, RoleID: roleID, ChangedBy: AuthUserID(c),
		Reason: reason, Changes: raw,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	respondRoleMatrix(c, updated)
}

// codeSetDiff returns the permission codes added to and removed from old,
// each sorted (permission codes are 1:1 with ids).
func codeSetDiff(old, next []string) (added, removed []string) {
	oldSet := make(map[string]struct{}, len(old))
	for _, code := range old {
		oldSet[code] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, code := range next {
		nextSet[code] = struct{}{}
	}
	added, removed = []string{}, []string{}
	for _, code := range next {
		if _, ok := oldSet[code]; !ok {
			added = append(added, code)
		}
	}
	for _, code := range old {
		if _, ok := nextSet[code]; !ok {
			removed = append(removed, code)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// roleWidgetDiff lists every key whose override changed, sorted by key —
// including new overrides (from nil) and removed ones (to nil); nil = no
// override, the permission-derived default applies.
func roleWidgetDiff(old, next map[string]bool) []map[string]any {
	keys := make([]string, 0, len(old)+len(next))
	for key := range old {
		keys = append(keys, key)
	}
	for key := range next {
		if _, ok := old[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		oldVal, hadOld := old[key]
		newVal, hasNew := next[key]
		if hadOld == hasNew && oldVal == newVal {
			continue
		}
		var from, to any
		if hadOld {
			from = oldVal
		}
		if hasNew {
			to = newVal
		}
		out = append(out, map[string]any{"key": key, "from": from, "to": to})
	}
	return out
}

type duplicateRoleRequest struct {
	Name string `json:"name"`
}

// DuplicateRoleHandler godoc
// @Summary Duplicate a role with its permissions and widget overrides
// @Description Copies Description/RequiresPhysicianData from the source role (IsSystem=false), then copies permission grants and widget overrides. Does NOT write role_change_log. The name is trimmed.
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Source role UUID"
// @Param request body duplicateRoleRequest true "New role name"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /roles/{id}/duplicate [post]
func DuplicateRoleHandler(c *gin.Context) {
	roleID, role, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	var req duplicateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// UNIQUE(company_id, name) also binds soft-deleted names — that
	// collision is intentional and reported as 409.
	newRole, err := q.CreateRole(c.Request.Context(), sqlcgen.CreateRoleParams{
		CompanyID:             role.CompanyID,
		Name:                  name,
		Description:           role.Description,
		IsSystem:              false,
		RequiresPhysicianData: role.RequiresPhysicianData,
		CreatedBy:             AuthUserID(c),
	})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "role name already exists"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := q.CopyRolePermissions(c.Request.Context(), sqlcgen.CopyRolePermissionsParams{
		RoleID: newRole.ID, RoleID_2: roleID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := q.CopyRoleWidgets(c.Request.Context(), sqlcgen.CopyRoleWidgetsParams{
		RoleID: newRole.ID, RoleID_2: roleID,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": newRole.ID, "name": newRole.Name}, "meta": gin.H{}})
}

// loadMatrixRole checks PermRoleManage FIRST — a caller without it gets 403
// whether or not the role exists (spec 2026-10-06-role-matrix-fixes A9) —
// then resolves :id into a live role owned by the caller's company: 400 on a
// malformed id, 404 on missing / soft-deleted / cross-company roles.
func loadMatrixRole(c *gin.Context) (pgtype.UUID, sqlcgen.CoreRole, bool) {
	if !RequirePermission(c, PermRoleManage) {
		return pgtype.UUID{}, sqlcgen.CoreRole{}, false
	}
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return pgtype.UUID{}, sqlcgen.CoreRole{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return pgtype.UUID{}, sqlcgen.CoreRole{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return pgtype.UUID{}, sqlcgen.CoreRole{}, false
	}
	if role.CompanyID != AuthCompanyID(c) || role.DeletedAt.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return pgtype.UUID{}, sqlcgen.CoreRole{}, false
	}
	return roleID, role, true
}

// roleWidgetOverrides returns the stored overrides of a role sorted by key.
func roleWidgetOverrides(c *gin.Context, roleID pgtype.UUID) ([]gin.H, bool) {
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListRoleWidgets(c.Request.Context(), roleID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].WidgetKey < rows[j].WidgetKey })
	widgets := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		widgets = append(widgets, gin.H{"key": row.WidgetKey, "visible": row.Visible})
	}
	return widgets, true
}

// roleChangeLogEntry converts a role_change_log row into its snake_case JSON
// shape; changes is passed through as raw jsonb.
func roleChangeLogEntry(changedAt pgtype.Timestamptz, changedByName pgtype.Text, reason string, changes []byte) gin.H {
	name := ""
	if changedByName.Valid {
		name = changedByName.String
	}
	return gin.H{
		"changed_at":      changedAt.Time.Format(time.RFC3339),
		"changed_by_name": name,
		"reason":          reason,
		"changes":         json.RawMessage(changes),
	}
}

// respondRoleMatrix writes the GET /roles/:id/matrix body for role; PUT
// /roles/:id/matrix answers with it too so the client takes it as its new
// snapshot (row_version included).
func respondRoleMatrix(c *gin.Context, role sqlcgen.CoreRole) {
	q := sqlcgen.New(TxFromContext(c))
	permRows, err := q.ListPermissionsByRole(c.Request.Context(), role.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	permIDs := make([]string, 0, len(permRows))
	for _, perm := range permRows {
		permIDs = append(permIDs, perm.ID.String())
	}
	widgets, ok := roleWidgetOverrides(c, role.ID)
	if !ok {
		return
	}
	var lastEdit any
	last, err := q.GetLastRoleChange(c.Request.Context(), role.ID)
	if err == nil {
		lastEdit = gin.H{
			"by_name": last.ChangedByName.String,
			"at":      last.ChangedAt.Time.Format(time.RFC3339),
			"reason":  last.Reason,
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	description := ""
	if role.Description.Valid {
		description = role.Description.String
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"role": gin.H{
			"id":                      role.ID.String(),
			"name":                    role.Name,
			"description":             description,
			"is_system":               role.IsSystem,
			"requires_physician_data": role.RequiresPhysicianData,
			"row_version":             role.RowVersion,
		},
		"permission_ids": permIDs,
		"widgets":        widgets,
		"last_edit":      lastEdit,
	}, "meta": gin.H{}})
}

// GetRoleMatrixHandler godoc
// @Summary Get the full permission + widget matrix of a role
// @Description Returns the role details (with row_version for PUT /roles/{id}/matrix), its permission ids, stored widget overrides (sorted by key) and the last change-log entry (null when never changed).
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/matrix [get]
func GetRoleMatrixHandler(c *gin.Context) {
	_, role, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	respondRoleMatrix(c, role)
}

// ListRoleChangeLogHandler godoc
// @Summary List the change history of a role
// @Description Newest-first change log entries (reason + jsonb diff). Query param limit defaults to 20 and is clamped to 1–100.
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Param limit query int false "Max entries (default 20, 1–100)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/audit [get]
func ListRoleChangeLogHandler(c *gin.Context) {
	roleID, _, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	limit := 20
	if v, err := strconv.Atoi(c.Query("limit")); err == nil {
		limit = min(max(v, 1), 100)
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListRoleChanges(c.Request.Context(), sqlcgen.ListRoleChangesParams{
		RoleID: roleID, Limit: int32(limit),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		data = append(data, roleChangeLogEntry(row.ChangedAt, row.ChangedByName, row.Reason, row.Changes))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit}})
}

// ListUserRoleWidgetsHandler godoc
// @Summary List the dashboard widget visibility overrides of the caller's active role
// @Description Self-access allowed without PermRoleManage (pattern of ListUserPermissionsHandler). Role is taken from the token's active role id; legacy tokens without a role id get an empty list.
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "User UUID"
// @Param merchant_id query string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /users/{id}/widgets [get]
func ListUserRoleWidgetsHandler(c *gin.Context) {
	userID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, ok := parseUUID(c.Query("merchant_id")); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant_id query param required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	targetUser, err := q.GetAppUserByID(c.Request.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if targetUser.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if userID != AuthUserID(c) && !RequirePermission(c, PermUserManage) {
		return
	}
	// Override of the caller's ACTIVE role only; legacy tokens without a
	// role id get an empty list (frontend falls back to defaults).
	data := []gin.H{}
	roleID := AuthRoleID(c)
	if roleID.Valid {
		widgets, ok := roleWidgetOverrides(c, roleID)
		if !ok {
			return
		}
		data = widgets
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
