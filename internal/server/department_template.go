// internal/server/department_template.go
// Department templates (#26, spec 2026-10-01-department-templates): list
// platform department templates with a per-poli status against the merchant's
// departments, and copy selected template departments (with their external
// code mappings) into the merchant. Templates are copied, never linked.
package server

import (
	"context"
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

// normalizeDepartmentName makes "  Poli  Umum" and "poli umum" compare equal.
func normalizeDepartmentName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// departmentStatus compares a template department with the merchant's
// departments, including soft-deleted ones (spec §5): the same code
// (case-insensitive) or the same normalized name in an active department →
// "exists"; in a deleted department only → "deleted_exists"; else "new", with
// the original names of similar active departments (mutual substring).
// similar is never nil.
func departmentStatus(code, name string, departments []sqlcgen.ListDepartmentNamesCodesByMerchantRow) (status string, similar []string) {
	targetCode := strings.ToLower(strings.TrimSpace(code))
	targetName := normalizeDepartmentName(name)
	deletedMatch := false
	for _, d := range departments {
		codeMatch := strings.ToLower(strings.TrimSpace(d.Code)) == targetCode
		nameMatch := normalizeDepartmentName(d.Name) == targetName
		if !codeMatch && !nameMatch {
			continue
		}
		if !d.Deleted {
			return "exists", []string{}
		}
		deletedMatch = true
	}
	if deletedMatch {
		return "deleted_exists", []string{}
	}
	similar = []string{}
	for _, d := range departments {
		n := normalizeDepartmentName(d.Name)
		if d.Deleted || n == targetName {
			continue
		}
		if (strings.Contains(n, targetName) || strings.Contains(targetName, n)) && !slices.Contains(similar, d.Name) {
			similar = append(similar, d.Name)
		}
	}
	sort.Strings(similar)
	return "new", similar
}

// ListDepartmentTemplatesHandler godoc
// @Summary List department templates with per-department status against the merchant's departments
// @Tags department
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /department-templates [get]
func ListDepartmentTemplatesHandler(c *gin.Context) {
	if !RequirePermission(c, PermDepartmentManage) {
		return
	}
	// Same merchant RequirePermission just checked: the X-Merchant-ID header,
	// not the token's mid claim (impersonation and fresh sessions carry none).
	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	templates, err := q.ListDepartmentTemplates(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	templateDepartments, err := q.ListTemplateDepartments(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	codeMaps, err := q.ListTemplateDepartmentCodeMaps(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	departments, err := q.ListDepartmentNamesCodesByMerchant(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}

	mapsByDepartment := make(map[pgtype.UUID][]gin.H)
	for _, m := range codeMaps {
		mapsByDepartment[m.TemplateDepartmentID] = append(mapsByDepartment[m.TemplateDepartmentID], gin.H{"system": m.System, "code": m.Code})
	}

	data := make([]gin.H, 0, len(templates))
	for _, t := range templates {
		departmentViews := []gin.H{}
		for _, td := range templateDepartments {
			if td.TemplateID != t.ID {
				continue
			}
			status, similar := departmentStatus(td.Code, td.Name, departments)
			maps := mapsByDepartment[td.ID]
			if maps == nil {
				maps = []gin.H{}
			}
			departmentViews = append(departmentViews, gin.H{
				"id":             uuidOrNil(td.ID),
				"code":           td.Code,
				"name":           td.Name,
				"specialty_code": textOrNil(td.SpecialtyCode),
				"code_maps":      maps,
				"status":         status,
				"similar_to":     similar,
			})
		}
		data = append(data, gin.H{
			"id":          uuidOrNil(t.ID),
			"code":        t.Code,
			"name":        t.Name,
			"description": textOrNil(t.Description),
			"version":     t.Version,
			"departments": departmentViews,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

type applyDepartmentTemplateRequest struct {
	DepartmentIDs []string `json:"department_ids"`
}

type createdDepartment struct {
	DepartmentID string `json:"department_id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
}

type skippedDepartment struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// specialtyConceptID resolves a template specialty_code to a terminology
// concept id; an unknown code leaves the poli without a specialty (NULL)
// instead of failing the apply.
func specialtyConceptID(ctx context.Context, q *sqlcgen.Queries, code string, cache map[string]pgtype.UUID) (pgtype.UUID, error) {
	if id, ok := cache[code]; ok {
		return id, nil
	}
	id, err := q.GetConceptIDByCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		var zero pgtype.UUID
		cache[code] = zero
		return zero, nil
	}
	if err != nil {
		var zero pgtype.UUID
		return zero, err
	}
	cache[code] = id
	return id, nil
}

// ApplyDepartmentTemplateHandler godoc
// @Summary Copy selected departments of a department template into the merchant
// @Tags department
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Template ID"
// @Param body body applyDepartmentTemplateRequest true "Template department IDs to copy"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /department-templates/{id}/apply [post]
func ApplyDepartmentTemplateHandler(c *gin.Context) {
	if !RequirePermission(c, PermDepartmentManage) {
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
	template, err := q.GetDepartmentTemplate(ctx, templateID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}

	var req applyDepartmentTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.DepartmentIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department_ids is required"})
		return
	}
	// Keyed by canonical UUID text so case/format variants of one id count as a
	// repeat; an unparsable id keeps its raw text and fails the template check.
	selected := make(map[string]bool, len(req.DepartmentIDs))
	for _, raw := range req.DepartmentIDs {
		id := raw
		if parsed, ok := parseUUID(raw); ok {
			id = parsed.String()
		}
		if selected[id] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "department_ids must not repeat"})
			return
		}
		selected[id] = true
	}

	rows, err := q.ListTemplateDepartments(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var chosen []sqlcgen.ListTemplateDepartmentsRow // template sort_order
	for _, r := range rows {
		if r.TemplateID == templateID && selected[r.ID.String()] {
			chosen = append(chosen, r)
		}
	}
	// Invalid UUIDs and ids of other templates both fall out here.
	if len(chosen) != len(selected) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department_ids must belong to the template"})
		return
	}

	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return
	}
	companyID := AuthCompanyID(c)
	existing, err := q.ListDepartmentNamesCodesByMerchant(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	templateMaps, err := q.ListTemplateDepartmentCodeMaps(ctx)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	mapsByDepartment := make(map[pgtype.UUID][]sqlcgen.CoreTemplateDepartmentCodeMap)
	for _, m := range templateMaps {
		mapsByDepartment[m.TemplateDepartmentID] = append(mapsByDepartment[m.TemplateDepartmentID], m)
	}

	// From here on departments may already be written: every error must abort
	// so the request transaction rolls back instead of committing a partial
	// apply.
	created := []createdDepartment{}
	skipped := []skippedDepartment{}
	concepts := make(map[string]pgtype.UUID)
	for _, td := range chosen {
		// Re-check on the server: the dialog's status may be stale.
		if status, _ := departmentStatus(td.Code, td.Name, existing); status != "new" {
			skipped = append(skipped, skippedDepartment{Code: td.Code, Name: td.Name, Reason: status})
			continue
		}
		var specialtyID pgtype.UUID
		if td.SpecialtyCode.Valid {
			specialtyID, err = specialtyConceptID(ctx, q, td.SpecialtyCode.String, concepts)
			if err != nil {
				abortInternalError(c, err)
				return
			}
		}
		sp, err := tx.Begin(ctx) // nested pgx.Tx = SAVEPOINT
		if err != nil {
			abortInternalError(c, err)
			return
		}
		department, err := sqlcgen.New(sp).CreateDepartment(ctx, sqlcgen.CreateDepartmentParams{
			CompanyID:          companyID,
			MerchantID:         merchantID,
			Code:               td.Code,
			Name:               td.Name,
			SpecialtyConceptID: specialtyID,
			CreatedBy:          AuthUserID(c),
		})
		if err != nil {
			_ = sp.Rollback(ctx)
			if isUniqueViolation(err) {
				skipped = append(skipped, skippedDepartment{Code: td.Code, Name: td.Name, Reason: "exists"})
				continue
			}
			abortInternalError(c, err)
			return
		}
		for _, m := range mapsByDepartment[td.ID] {
			if err := sqlcgen.New(sp).CreateDepartmentCodeMap(ctx, sqlcgen.CreateDepartmentCodeMapParams{
				DepartmentID: department.ID,
				System:       m.System,
				Code:         m.Code,
				ActorID:      AuthUserID(c),
			}); err != nil {
				_ = sp.Rollback(ctx)
				if isUniqueViolation(err) {
					skipped = append(skipped, skippedDepartment{Code: td.Code, Name: td.Name, Reason: "exists"})
					break
				}
				abortInternalError(c, err)
				return
			}
		}
		if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			abortInternalError(c, err)
			return
		}
		created = append(created, createdDepartment{DepartmentID: department.ID.String(), Code: department.Code, Name: department.Name})
	}

	var platformAdminID pgtype.UUID
	if by := AuthImpersonatedBy(c); by != "" {
		platformAdminID, _ = parseUUID(by)
	}
	// core.template_application names its history columns roles_created /
	// roles_skipped; for department templates they carry the department names.
	createdNames := make([]string, 0, len(created))
	for _, d := range created {
		createdNames = append(createdNames, d.Name)
	}
	skippedNames := make([]string, 0, len(skipped))
	for _, d := range skipped {
		skippedNames = append(skippedNames, d.Name)
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
