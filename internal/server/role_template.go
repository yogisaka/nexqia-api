// internal/server/role_template.go
// Role templates (#7, spec 2026-09-30-role-templates-design §4): list platform
// role templates with a per-role status against the company's roles, and copy
// selected template roles into the company. Templates are copied, never linked.
package server

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// normalizeRoleName makes "  Kasir  Rajal" and "kasir rajal" compare equal.
func normalizeRoleName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// roleNameStatus compares a template role name with the company's roles (spec
// §4.1): exact match with an active role → "exists"; with a deleted role only →
// "deleted_exists" (UNIQUE (company_id, name) also binds deleted rows); else
// "new", with the original names of similar active roles. similar is never nil.
func roleNameStatus(name string, company []sqlcgen.ListCompanyRoleNamesRow) (status string, similar []string) {
	target := normalizeRoleName(name)
	deletedMatch := false
	for _, r := range company {
		if normalizeRoleName(r.Name) != target {
			continue
		}
		if !r.Deleted {
			return "exists", []string{}
		}
		deletedMatch = true
	}
	if deletedMatch {
		return "deleted_exists", []string{}
	}
	similar = []string{}
	for _, r := range company {
		n := normalizeRoleName(r.Name)
		if r.Deleted || n == target {
			continue
		}
		if (strings.Contains(n, target) || strings.Contains(target, n)) && !slices.Contains(similar, r.Name) {
			similar = append(similar, r.Name)
		}
	}
	sort.Strings(similar)
	return "new", similar
}

// roleRequiresMFA: holding any admin permission makes TOTP mandatory (mfa.go).
func roleRequiresMFA(codes []string) bool {
	for _, code := range codes {
		if slices.Contains(mfaAdminPermissions, code) {
			return true
		}
	}
	return false
}

// templateRole is one template role with its permissions, grouped from
// ListRoleTemplateRoles rows (ordered by template, sort_order, permission code).
type templateRole struct {
	ID                    pgtype.UUID
	TemplateID            pgtype.UUID
	Name                  string
	Description           pgtype.Text
	RequiresPhysicianData bool
	PermissionIDs         []pgtype.UUID
	PermissionCodes       []string
	Permissions           []gin.H
}

func groupTemplateRoles(rows []sqlcgen.ListRoleTemplateRolesRow) []*templateRole {
	var roles []*templateRole
	for _, r := range rows {
		if len(roles) == 0 || roles[len(roles)-1].ID != r.ID {
			roles = append(roles, &templateRole{
				ID:                    r.ID,
				TemplateID:            r.TemplateID,
				Name:                  r.Name,
				Description:           r.Description,
				RequiresPhysicianData: r.RequiresPhysicianData,
			})
		}
		tr := roles[len(roles)-1]
		tr.PermissionIDs = append(tr.PermissionIDs, r.PermissionID)
		tr.PermissionCodes = append(tr.PermissionCodes, r.PermissionCode)
		tr.Permissions = append(tr.Permissions, gin.H{"code": r.PermissionCode, "description": r.PermissionDescription})
	}
	return roles
}

// ListRoleTemplatesHandler godoc
// @Summary List role templates with per-role status against the company's roles
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /role-templates [get]
func ListRoleTemplatesHandler(c *gin.Context) {
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	templates, err := q.ListRoleTemplates(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListRoleTemplateRoles(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	companyRoles, err := q.ListCompanyRoleNames(ctx, AuthCompanyID(c))
	if err != nil {
		respondInternalError(c, err)
		return
	}
	roles := groupTemplateRoles(rows)

	data := make([]gin.H, 0, len(templates))
	for _, t := range templates {
		roleViews := []gin.H{}
		for _, r := range roles {
			if r.TemplateID != t.ID {
				continue
			}
			status, similar := roleNameStatus(r.Name, companyRoles)
			roleViews = append(roleViews, gin.H{
				"id":                      uuidOrNil(r.ID),
				"name":                    r.Name,
				"description":             textOrNil(r.Description),
				"requires_physician_data": r.RequiresPhysicianData,
				"requires_mfa":            roleRequiresMFA(r.PermissionCodes),
				"permissions":             r.Permissions,
				"status":                  status,
				"similar_to":              similar,
			})
		}
		data = append(data, gin.H{
			"id":          uuidOrNil(t.ID),
			"code":        t.Code,
			"name":        t.Name,
			"description": textOrNil(t.Description),
			"version":     t.Version,
			"roles":       roleViews,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

type applyRoleTemplateRequest struct {
	RoleIDs []string `json:"role_ids"`
}

type createdRole struct {
	RoleID string `json:"role_id"`
	Name   string `json:"name"`
}

type skippedRole struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ApplyRoleTemplateHandler godoc
// @Summary Copy selected roles of a role template into the company
// @Tags rbac
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Template ID"
// @Param body body applyRoleTemplateRequest true "Template role IDs to copy"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /role-templates/{id}/apply [post]
func ApplyRoleTemplateHandler(c *gin.Context) {
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	templateID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	template, err := q.GetRoleTemplate(ctx, templateID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}

	var req applyRoleTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.RoleIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role_ids is required"})
		return
	}
	// Keyed by canonical UUID text so case/format variants of one id count as a
	// repeat; an unparsable id keeps its raw text and fails the template check.
	selected := make(map[string]bool, len(req.RoleIDs))
	for _, raw := range req.RoleIDs {
		id := raw
		if parsed, ok := parseUUID(raw); ok {
			id = parsed.String()
		}
		if selected[id] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role_ids must not repeat"})
			return
		}
		selected[id] = true
	}

	rows, err := q.ListRoleTemplateRoles(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var chosen []*templateRole // template sort_order
	for _, r := range groupTemplateRoles(rows) {
		if r.TemplateID == templateID && selected[r.ID.String()] {
			chosen = append(chosen, r)
		}
	}
	// Invalid UUIDs and ids of other templates both fall out here.
	if len(chosen) != len(selected) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role_ids must belong to the template"})
		return
	}

	companyID := AuthCompanyID(c)
	companyRoles, err := q.ListCompanyRoleNames(ctx, companyID)
	if err != nil {
		respondInternalError(c, err)
		return
	}

	// From here on roles may already be written: every error must abort so the
	// request transaction rolls back instead of committing a partial apply.
	created := []createdRole{}
	skipped := []skippedRole{}
	for _, tr := range chosen {
		// Re-check on the server: the dialog's status may be stale.
		if status, _ := roleNameStatus(tr.Name, companyRoles); status != "new" {
			skipped = append(skipped, skippedRole{Name: tr.Name, Reason: status})
			continue
		}
		params := sqlcgen.CreateRoleParams{
			CompanyID:             companyID,
			Name:                  tr.Name,
			Description:           tr.Description,
			IsSystem:              false,
			RequiresPhysicianData: tr.RequiresPhysicianData,
			CreatedBy:             AuthUserID(c),
		}
		sp, err := tx.Begin(ctx) // nested pgx.Tx = SAVEPOINT
		if err != nil {
			abortInternalError(c, err)
			return
		}
		role, err := sqlcgen.New(sp).CreateRole(ctx, params)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isUniqueViolation(err) {
				skipped = append(skipped, skippedRole{Name: tr.Name, Reason: "exists"})
				continue
			}
			abortInternalError(c, err)
			return
		}
		for _, permissionID := range tr.PermissionIDs {
			if err := sqlcgen.New(sp).AddRolePermission(ctx, sqlcgen.AddRolePermissionParams{RoleID: role.ID, PermissionID: permissionID}); err != nil {
				_ = sp.Rollback(ctx)
				abortInternalError(c, err)
				return
			}
		}
		if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			abortInternalError(c, err)
			return
		}
		created = append(created, createdRole{RoleID: role.ID.String(), Name: role.Name})
	}

	var platformAdminID pgtype.UUID
	if by := AuthImpersonatedBy(c); by != "" {
		platformAdminID, _ = parseUUID(by)
	}
	createdNames := make([]string, 0, len(created))
	for _, r := range created {
		createdNames = append(createdNames, r.Name)
	}
	skippedNames := make([]string, 0, len(skipped))
	for _, r := range skipped {
		skippedNames = append(skippedNames, r.Name)
	}
	if err := q.CreateTemplateApplication(ctx, sqlcgen.CreateTemplateApplicationParams{
		CompanyID:       companyID,
		TemplateID:      templateID,
		TemplateVersion: template.Version,
		AppliedBy:       AuthUserID(c),
		PlatformAdminID: platformAdminID,
		RolesCreated:    createdNames,
		RolesSkipped:    skippedNames,
	}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"created": created, "skipped": skipped}})
}
