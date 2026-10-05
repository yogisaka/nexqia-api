// internal/server/rbac_matrix.go
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
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

type replaceRolePermissionsRequest struct {
	PermissionIDs []string `json:"permission_ids"`
	Reason        string   `json:"reason"`
}

// ReplaceRolePermissionsHandler godoc
// @Summary Replace the full permission set of a role
// @Description Bulk PUT: the submitted set becomes the role's exact permission set. Idempotent — an identical set returns 204 without writing or logging.
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Param request body replaceRolePermissionsRequest true "Permission ids + reason"
// @Success 204 "No Content"
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/permissions [put]
func ReplaceRolePermissionsHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if role.CompanyID != AuthCompanyID(c) || role.DeletedAt.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req replaceRolePermissionsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Reason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	if len(req.PermissionIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_ids"})
		return
	}
	seen := make(map[string]struct{}, len(req.PermissionIDs))
	ids := make([]pgtype.UUID, 0, len(req.PermissionIDs))
	for _, raw := range req.PermissionIDs {
		if _, dup := seen[raw]; dup {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate permission id"})
			return
		}
		id, valid := parseUUID(raw)
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_ids"})
			return
		}
		seen[raw] = struct{}{}
		ids = append(ids, id)
	}
	// Every submitted id must exist in the catalog: the query returns one row
	// per found id, so row count < submitted count means an unknown id.
	newCodes, err := q.ListPermissionCodesByIDs(c.Request.Context(), ids)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(newCodes) != len(ids) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_ids"})
		return
	}
	// Permission code is 1:1 with its id, so comparing code sets is equivalent
	// to comparing id sets.
	oldCodes, err := q.ListPermissionCodesByRole(c.Request.Context(), roleID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	oldSet := make(map[string]struct{}, len(oldCodes))
	for _, code := range oldCodes {
		oldSet[code] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(newCodes))
	for _, code := range newCodes {
		newSet[code] = struct{}{}
	}
	added := []string{}
	removed := []string{}
	identical := len(oldSet) == len(newSet)
	for _, code := range newCodes {
		if _, ok := oldSet[code]; !ok {
			identical = false
			added = append(added, code)
		}
	}
	for _, code := range oldCodes {
		if _, ok := newSet[code]; !ok {
			identical = false
			removed = append(removed, code)
		}
	}
	if identical {
		c.Status(http.StatusNoContent)
		return
	}
	sort.Strings(added)
	sort.Strings(removed)

	// Write 1: drop grants outside the new set.
	if _, err := q.DeleteRolePermissionsNotIn(c.Request.Context(), sqlcgen.DeleteRolePermissionsNotInParams{
		RoleID: roleID, Column2: ids,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	// Write 2: add the new grants (ids validated unique, no conflict after write 1).
	for _, id := range ids {
		if err := q.AddRolePermission(c.Request.Context(), sqlcgen.AddRolePermissionParams{
			RoleID: roleID, PermissionID: id,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
	}
	// Write 3: audit the diff using permission codes, not UUIDs.
	changes, err := json.Marshal(map[string]any{
		"permission": map[string]any{"added": added, "removed": removed},
	})
	if err != nil {
		abortInternalError(c, err)
		return
	}
	if err := q.InsertRoleChangeLog(c.Request.Context(), sqlcgen.InsertRoleChangeLogParams{
		CompanyID: role.CompanyID, RoleID: roleID, ChangedBy: AuthUserID(c),
		Reason: req.Reason, Changes: changes,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type roleWidgetOverrideInput struct {
	Key     string `json:"key"`
	Visible bool   `json:"visible"`
}

type replaceRoleWidgetsRequest struct {
	Widgets []roleWidgetOverrideInput `json:"widgets"`
	Reason  string                    `json:"reason"`
}

// ReplaceRoleWidgetsHandler godoc
// @Summary Replace the dashboard widget visibility overrides of a role
// @Description Bulk PUT: the submitted overrides become the role's exact widget override set. Idempotent — an identical set returns 204 without writing or logging.
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Param request body replaceRoleWidgetsRequest true "Widget overrides + reason"
// @Success 204 "No Content"
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/widgets [put]
func ReplaceRoleWidgetsHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if role.CompanyID != AuthCompanyID(c) || role.DeletedAt.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req replaceRoleWidgetsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Reason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	if len(req.Widgets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid widgets"})
		return
	}
	newOverrides := make(map[string]bool, len(req.Widgets))
	for _, w := range req.Widgets {
		if _, known := roleMatrixWidgetKeys[w.Key]; !known {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown widget key"})
			return
		}
		if _, dup := newOverrides[w.Key]; dup {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate widget key"})
			return
		}
		newOverrides[w.Key] = w.Visible
	}
	oldRows, err := q.ListRoleWidgets(c.Request.Context(), roleID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	oldOverrides := make(map[string]bool, len(oldRows))
	for _, row := range oldRows {
		oldOverrides[row.WidgetKey] = row.Visible
	}
	// Idempotent: same final override map → 204 without writes or log.
	if len(oldOverrides) == len(newOverrides) {
		same := true
		for key, visible := range newOverrides {
			if old, ok := oldOverrides[key]; !ok || old != visible {
				same = false
				break
			}
		}
		if same {
			c.Status(http.StatusNoContent)
			return
		}
	}
	changed := make([]roleWidgetOverrideInput, 0, len(newOverrides))
	keys := make([]string, 0, len(newOverrides))
	for key := range newOverrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// from defaults to false when no override row existed yet.
		if oldOverrides[key] != newOverrides[key] {
			changed = append(changed, roleWidgetOverrideInput{Key: key, Visible: newOverrides[key]})
		}
	}
	diff := make([]map[string]any, 0, len(changed))
	for _, w := range changed {
		diff = append(diff, map[string]any{"key": w.Key, "from": oldOverrides[w.Key], "to": w.Visible})
	}
	changes, err := json.Marshal(map[string]any{"widget": diff})
	if err != nil {
		abortInternalError(c, err)
		return
	}

	// Write 1: clear existing overrides.
	if _, err := q.DeleteRoleWidgets(c.Request.Context(), roleID); err != nil {
		abortInternalError(c, err)
		return
	}
	// Write 2: insert the new overrides (keys validated unique).
	for _, w := range req.Widgets {
		if err := q.AddRoleWidget(c.Request.Context(), sqlcgen.AddRoleWidgetParams{
			RoleID: roleID, WidgetKey: w.Key, Visible: w.Visible,
		}); err != nil {
			abortInternalError(c, err)
			return
		}
	}
	// Write 3: audit only the keys whose value changed.
	if err := q.InsertRoleChangeLog(c.Request.Context(), sqlcgen.InsertRoleChangeLogParams{
		CompanyID: role.CompanyID, RoleID: roleID, ChangedBy: AuthUserID(c),
		Reason: req.Reason, Changes: changes,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type duplicateRoleRequest struct {
	Name string `json:"name"`
}

// DuplicateRoleHandler godoc
// @Summary Duplicate a role with its permissions and widget overrides
// @Description Copies Description/RequiresPhysicianData from the source role (IsSystem=false), then copies permission grants and widget overrides. Does NOT write role_change_log.
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Source role UUID"
// @Param request body duplicateRoleRequest true "New role name"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /roles/{id}/duplicate [post]
func DuplicateRoleHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if role.CompanyID != AuthCompanyID(c) || role.DeletedAt.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req duplicateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	// Write 1: create the copy. UNIQUE(company_id, name) also binds soft-deleted
	// names — that collision is intentional and reported as 409.
	newRole, err := q.CreateRole(c.Request.Context(), sqlcgen.CreateRoleParams{
		CompanyID:             role.CompanyID,
		Name:                  req.Name,
		Description:           role.Description,
		IsSystem:              false,
		RequiresPhysicianData: role.RequiresPhysicianData,
		CreatedBy:             AuthUserID(c),
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "role name already exists"})
			return
		}
		abortInternalError(c, err)
		return
	}
	if _, err := q.CopyRolePermissions(c.Request.Context(), sqlcgen.CopyRolePermissionsParams{
		RoleID: newRole.ID, RoleID_2: roleID,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	if _, err := q.CopyRoleWidgets(c.Request.Context(), sqlcgen.CopyRoleWidgetsParams{
		RoleID: newRole.ID, RoleID_2: roleID,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": newRole.ID, "name": newRole.Name}, "meta": gin.H{}})
}

// loadMatrixRole resolves :id into a live role owned by the caller's company.
// Shared by the read endpoints: 400 on malformed id, 404 on missing /
// soft-deleted / cross-company roles, then RequirePermission(PermRoleManage).
func loadMatrixRole(c *gin.Context) (pgtype.UUID, sqlcgen.CoreRole, bool) {
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
	if !RequirePermission(c, PermRoleManage) {
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

// GetRoleMatrixHandler godoc
// @Summary Get the full permission + widget matrix of a role
// @Description Returns the role metadata, its permission ids, stored widget overrides (sorted by key) and the last change-log entry (null when never changed).
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/matrix [get]
func GetRoleMatrixHandler(c *gin.Context) {
	roleID, role, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	permRows, err := q.ListPermissionsByRole(c.Request.Context(), roleID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	permIDs := make([]string, 0, len(permRows))
	for _, perm := range permRows {
		permIDs = append(permIDs, perm.ID.String())
	}
	widgets, ok := roleWidgetOverrides(c, roleID)
	if !ok {
		return
	}
	var lastEdit any
	last, err := q.GetLastRoleChange(c.Request.Context(), roleID)
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
		},
		"permission_ids": permIDs,
		"widgets":        widgets,
		"last_edit":      lastEdit,
	}, "meta": gin.H{}})
}

// ListRoleChangeLogHandler godoc
// @Summary List the change history of a role
// @Description Newest-first change log entries (reason + jsonb diff). Query param limit defaults to 20.
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "Role UUID"
// @Param limit query int false "Max entries (default 20)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /roles/{id}/audit [get]
func ListRoleChangeLogHandler(c *gin.Context) {
	roleID, _, ok := loadMatrixRole(c)
	if !ok {
		return
	}
	limit := 20
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
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
