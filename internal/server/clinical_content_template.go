// internal/server/clinical_content_template.go
// CPPT fase 2a Task 3 (spec 2026-11-04-cppt-2a-soap-editor-design §7):
// content templates pribadi ("Jadikan template") — CRUD snapshot form_data
// milik satu user, migration 000091 tabel clinical_form.content_template.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterClinicalContentTemplateRoutes(rg *gin.RouterGroup) {
	rg.POST("/clinical/content-templates", CreateContentTemplateHandler)
	rg.GET("/clinical/content-templates", ListContentTemplatesHandler)
	rg.DELETE("/clinical/content-templates/:id", DeleteContentTemplateHandler)
}

// clinicalContentTemplateJSON maps a created template row to the snake_case
// contract shape (spec §7; pattern clinicalFormJSON).
func clinicalContentTemplateJSON(id pgtype.UUID, formType, name string, formData []byte, createdAt pgtype.Timestamptz) gin.H {
	return gin.H{
		"id":         uuidJSON(id),
		"form_type":  formType,
		"name":       name,
		"form_data":  json.RawMessage(formData),
		"created_at": tsJSON(createdAt),
	}
}

type createContentTemplateRequest struct {
	FormType string          `json:"form_type" binding:"required"`
	Name     string          `json:"name" binding:"required"`
	FormData json.RawMessage `json:"form_data" binding:"required"`
}

// CreateContentTemplateHandler godoc
// @Summary Save a personal content template from the SOAP editor
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createContentTemplateRequest true "Template data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/content-templates [post]
func CreateContentTemplateHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordWrite, merchantID) {
		return
	}
	var req createContentTemplateRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, AuthCompanyID(c), merchantID, req.FormType)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "form_type tidak dikenal"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) < 1 || len(name) > 80 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nama template wajib 1-80 karakter"})
		return
	}
	var defs clinicalSectionsDef
	if err := json.Unmarshal(tpl.Sections, &defs); err != nil {
		respondInternalError(c, err)
		return
	}
	// Content template tidak terikat admission → DOB invalid; gate
	// imt_kategori per-usia di-skip aman (plan 2026-11-18-anak-a1-1-be Task 2).
	if _, okV := validateClinicalFormData(c, req.FormType, defs, req.FormData, pgtype.Date{}); !okV {
		return
	}
	row, err := q.CreateContentTemplate(ctx, sqlcgen.CreateContentTemplateParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, OwnerUserID: AuthUserID(c),
		FormType: req.FormType, Name: name, FormData: req.FormData,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "nama template sudah ada"})
			return
		}
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": clinicalContentTemplateJSON(
		row.ID, row.FormType, row.Name, row.FormData, row.CreatedAt), "meta": gin.H{}})
}

// ListContentTemplatesHandler godoc
// @Summary List the caller's own content templates for one form_type
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param form_type query string true "Clinical form type"
// @Success 200 {object} apiResponse
// @Router /clinical/content-templates [get]
func ListContentTemplatesHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	// KEPUTUSAN (review 2026-11-04-cppt-2a): list sengaja ikut write perm —
	// panel template hanya dirender bagi pemegang clinical.record.write, jadi
	// view perm tidak menambah permukaan apa pun.
	if !RequirePermissionForMerchant(c, PermClinicalRecordWrite, merchantID) {
		return
	}
	formType := c.Query("form_type")
	if formType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "form_type query param is required"})
		return
	}
	rows, err := sqlcgen.New(TxFromContext(c)).ListContentTemplatesByOwner(c.Request.Context(),
		sqlcgen.ListContentTemplatesByOwnerParams{OwnerUserID: AuthUserID(c), FormType: formType})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, clinicalContentTemplateJSON(r.ID, r.FormType, r.Name, r.FormData, r.CreatedAt))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// DeleteContentTemplateHandler godoc
// @Summary Delete the caller's own content template
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Template UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/content-templates/{id} [delete]
func DeleteContentTemplateHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordWrite, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	row, err := q.GetContentTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OwnerUserID != AuthUserID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "template tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := q.DeleteContentTemplate(ctx, id); err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": uuidJSON(row.ID)}, "meta": gin.H{}})
}
