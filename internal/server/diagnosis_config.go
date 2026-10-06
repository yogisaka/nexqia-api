// internal/server/diagnosis_config.go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// defaultDiagnosisCodeSystem is used when a company has never configured
// core.diagnosis_config (no row at all — not even the company default).
// Matches terminology.code_system.name (see cmd/migrate-data/diagnosis.go).
const defaultDiagnosisCodeSystem = "ICD-10"

// diagnosisCodeSystemTag marks terminology.code_system rows that are valid
// diagnosis standards (migration 000042).
const diagnosisCodeSystemTag = "diagnosis"

// diagnosisConfigData is the single response shape of every diagnosis-config
// endpoint — same contract as mrnConfigData in mrn_config.go.
func diagnosisConfigData(companyID, merchantID pgtype.UUID, codeSystem string, isDefault, isOverride bool) gin.H {
	return gin.H{"company_id": companyID, "merchant_id": merchantID, "code_system": codeSystem, "is_default": isDefault, "is_override": isOverride}
}

// RegisterDiagnosisConfigRoutes wires the configurable diagnosis coding
// standard (migrations/000033): different merchants under the same company
// may use different terminology.code_system values (ICD-10, ICD-9-CM, or
// whatever else gets ETL'd/seeded later) — company-level default + optional
// per-merchant override, same shape as mrn_config.go.
func RegisterDiagnosisConfigRoutes(rg *gin.RouterGroup) {
	rg.GET("/terminology/code-systems", ListCodeSystemsHandler)
	rg.GET("/companies/:id/diagnosis-config", GetCompanyDiagnosisConfigHandler)
	rg.PUT("/companies/:id/diagnosis-config", UpsertCompanyDiagnosisConfigHandler)
	rg.GET("/merchants/:id/diagnosis-config", GetMerchantDiagnosisConfigHandler)
	rg.PUT("/merchants/:id/diagnosis-config", UpsertMerchantDiagnosisConfigHandler)
	rg.DELETE("/merchants/:id/diagnosis-config", DeleteMerchantDiagnosisConfigHandler)
}

type diagnosisConfigRequest struct {
	CodeSystem string `json:"code_system" binding:"required"`
}

// validateCodeSystem requires the value to be an active terminology.code_system
// tagged "diagnosis" (migration 000042) — any other system (religion, region,
// ...) is not a diagnosis standard, and an unknown name would leave the
// diagnosis combobox empty.
func validateCodeSystem(ctx context.Context, q *sqlcgen.Queries, codeSystem string) error {
	systems, err := q.ListCodeSystemsByTag(ctx, diagnosisCodeSystemTag)
	if err != nil {
		return err
	}
	for _, s := range systems {
		if s.Name == codeSystem {
			return nil
		}
	}
	return errors.New("code_system is not an active diagnosis standard: " + codeSystem)
}

// ListCodeSystemsHandler godoc
// @Summary List active terminology code systems (for diagnosis-config admin)
// @Param tag query string false "Only systems carrying this tag, e.g. diagnosis"
// @Tags diagnosis-config
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Description Profile is a JSON object (spec 2026-10-01-terminology-import §5); Attribution must be shown with ICD-11.
// @Router /terminology/code-systems [get]
func ListCodeSystemsHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	var systems []sqlcgen.TerminologyCodeSystem
	var err error
	if tag := c.Query("tag"); tag != "" {
		systems, err = q.ListCodeSystemsByTag(c.Request.Context(), tag)
	} else {
		systems, err = q.ListCodeSystems(c.Request.Context())
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Explicit response (spec §12 R6): sending the sqlc struct raw would
	// base64-encode the jsonb profile column.
	items := make([]gin.H, 0, len(systems))
	for _, s := range systems {
		profile := s.Profile
		if len(profile) == 0 {
			profile = []byte("{}")
		}
		items = append(items, gin.H{
			"ID": s.ID, "Name": s.Name, "Version": s.Version, "SystemURI": s.SystemUri,
			"License":     s.License.String,
			"Attribution": s.Attribution.String,
			"Profile":     json.RawMessage(profile),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetCompanyDiagnosisConfigHandler godoc
// @Summary Get a company's default diagnosis code system
// @Tags diagnosis-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company ID"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/diagnosis-config [get]
func GetCompanyDiagnosisConfigHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	cfg, err := q.GetCompanyDiagnosisConfig(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(id, pgtype.UUID{}, defaultDiagnosisCodeSystem, true, false), "meta": gin.H{"is_default": true}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(cfg.CompanyID, cfg.MerchantID, cfg.CodeSystem, false, false), "meta": gin.H{}})
}

// UpsertCompanyDiagnosisConfigHandler godoc
// @Summary Set a company's default diagnosis code system
// @Tags diagnosis-config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company ID"
// @Param request body diagnosisConfigRequest true "Diagnosis code system"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/diagnosis-config [put]
func UpsertCompanyDiagnosisConfigHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	var req diagnosisConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := validateCodeSystem(c.Request.Context(), q, req.CodeSystem); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg, err := q.UpsertCompanyDiagnosisConfig(c.Request.Context(), sqlcgen.UpsertCompanyDiagnosisConfigParams{
		CompanyID: id, CodeSystem: req.CodeSystem, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(cfg.CompanyID, cfg.MerchantID, cfg.CodeSystem, false, false), "meta": gin.H{}})
}

// GetMerchantDiagnosisConfigHandler godoc
// @Summary Get a merchant's diagnosis code system override (falls back to company default if unset)
// @Tags diagnosis-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/diagnosis-config [get]
func GetMerchantDiagnosisConfigHandler(c *gin.Context) {
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && merchant.CompanyID != AuthCompanyID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	cfg, err := q.GetMerchantDiagnosisConfig(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		codeSystem, err := resolveDiagnosisCodeSystem(c.Request.Context(), q, AuthCompanyID(c), pgtype.UUID{})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(AuthCompanyID(c), id, codeSystem, false, false), "meta": gin.H{"is_override": false}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(cfg.CompanyID, cfg.MerchantID, cfg.CodeSystem, false, true), "meta": gin.H{"is_override": true}})
}

// UpsertMerchantDiagnosisConfigHandler godoc
// @Summary Set a merchant-specific diagnosis code system override
// @Tags diagnosis-config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Param request body diagnosisConfigRequest true "Diagnosis code system"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/diagnosis-config [put]
func UpsertMerchantDiagnosisConfigHandler(c *gin.Context) {
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req diagnosisConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := validateCodeSystem(c.Request.Context(), q, req.CodeSystem); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchant, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && merchant.CompanyID != AuthCompanyID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	cfg, err := q.UpsertMerchantDiagnosisConfig(c.Request.Context(), sqlcgen.UpsertMerchantDiagnosisConfigParams{
		CompanyID: AuthCompanyID(c), MerchantID: id, CodeSystem: req.CodeSystem, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": diagnosisConfigData(cfg.CompanyID, cfg.MerchantID, cfg.CodeSystem, false, true), "meta": gin.H{}})
}

// DeleteMerchantDiagnosisConfigHandler godoc
// @Summary Remove a merchant's diagnosis code system override (reverts to company default)
// @Tags diagnosis-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Success 204
// @Router /merchants/{id}/diagnosis-config [delete]
func DeleteMerchantDiagnosisConfigHandler(c *gin.Context) {
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && merchant.CompanyID != AuthCompanyID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.DeleteMerchantDiagnosisConfig(c.Request.Context(), id); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// resolveDiagnosisCodeSystem: merchant override, else company default, else
// defaultDiagnosisCodeSystem.
func resolveDiagnosisCodeSystem(ctx context.Context, q *sqlcgen.Queries, companyID, merchantID pgtype.UUID) (string, error) {
	codeSystem, err := q.GetDiagnosisCodeSystem(ctx, sqlcgen.GetDiagnosisCodeSystemParams{MerchantID: merchantID, CompanyID: companyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultDiagnosisCodeSystem, nil
	}
	return codeSystem, err
}
