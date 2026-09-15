// internal/server/tenancy.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterTenancyRoutes wires core.company and core.merchant CRUD (docs/07-core-ddl.md §1).
// All routes require AuthMiddleware; company-level mutation/list requires PermCompanyManage
// (platform-admin scope — company creation/listing spans tenants by nature), merchant
// mutation requires PermMerchantManage, and every handler enforces the caller can only
// see/touch their own company (AuthCompanyID) unless they hold PermCompanyManage.
func RegisterTenancyRoutes(rg *gin.RouterGroup) {
	rg.POST("/companies", CreateCompanyHandler)
	rg.GET("/companies", ListCompaniesHandler)
	rg.GET("/companies/:id", GetCompanyHandler)
	rg.PATCH("/companies/:id", UpdateCompanyHandler)
	rg.DELETE("/companies/:id", DeleteCompanyHandler)

	rg.POST("/merchants", CreateMerchantHandler)
	rg.GET("/companies/:id/merchants", ListMerchantsHandler)
	rg.GET("/merchants/:id", GetMerchantHandler)
	rg.PATCH("/merchants/:id", UpdateMerchantHandler)
	rg.DELETE("/merchants/:id", DeleteMerchantHandler)
}

type createCompanyRequest struct {
	Code string `json:"code" binding:"required"`
	Name string `json:"name" binding:"required"`
}

func CreateCompanyHandler(c *gin.Context) {
	if !RequirePermission(c, PermCompanyManage) {
		return
	}
	var req createCompanyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	company, err := q.CreateCompany(c.Request.Context(), sqlcgen.CreateCompanyParams{
		Code: req.Code, Name: req.Name, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": company, "meta": gin.H{}})
}

// ListCompaniesHandler lists across ALL tenants — platform-admin only.
func ListCompaniesHandler(c *gin.Context) {
	if !RequirePermission(c, PermCompanyManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	companies, err := q.ListCompanies(c.Request.Context(), sqlcgen.ListCompaniesParams{Limit: limit, Offset: offset})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": companies, "meta": gin.H{"limit": limit, "offset": offset}})
}

func GetCompanyHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if id != AuthCompanyID(c) && !RequirePermission(c, PermCompanyManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	company, err := q.GetCompanyByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": company, "meta": gin.H{}})
}

type updateCompanyRequest struct {
	Name     string `json:"name" binding:"required"`
	IsActive bool   `json:"is_active"`
}

func UpdateCompanyHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	if !RequirePermission(c, PermCompanyManage) {
		return
	}
	var req updateCompanyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	company, err := q.UpdateCompany(c.Request.Context(), sqlcgen.UpdateCompanyParams{
		ID: id, Name: req.Name, IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": company, "meta": gin.H{}})
}

func DeleteCompanyHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	if !RequirePermission(c, PermCompanyManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if err := q.SoftDeleteCompany(c.Request.Context(), sqlcgen.SoftDeleteCompanyParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

type createMerchantRequest struct {
	CompanyID          string `json:"company_id" binding:"required"`
	Code               string `json:"code" binding:"required"`
	Name               string `json:"name" binding:"required"`
	KemkesFacilityCode string `json:"kemkes_facility_code"`
	BpjsPpkCode        string `json:"bpjs_ppk_code"`
	Timezone           string `json:"timezone" binding:"required"`
}

func CreateMerchantHandler(c *gin.Context) {
	var req createMerchantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	companyID, ok := parseUUID(req.CompanyID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid company_id"})
		return
	}
	if companyID != AuthCompanyID(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cannot create merchant outside your own company"})
		return
	}
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.CreateMerchant(c.Request.Context(), sqlcgen.CreateMerchantParams{
		CompanyID:          companyID,
		Code:               req.Code,
		Name:               req.Name,
		KemkesFacilityCode: pgtype.Text{String: req.KemkesFacilityCode, Valid: req.KemkesFacilityCode != ""},
		BpjsPpkCode:        pgtype.Text{String: req.BpjsPpkCode, Valid: req.BpjsPpkCode != ""},
		Timezone:           req.Timezone,
		CreatedBy:          AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": merchant, "meta": gin.H{}})
}

func ListMerchantsHandler(c *gin.Context) {
	companyID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if companyID != AuthCompanyID(c) && !RequirePermission(c, PermCompanyManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	merchants, err := q.ListMerchantsByCompany(c.Request.Context(), sqlcgen.ListMerchantsByCompanyParams{
		CompanyID: companyID, Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": merchants, "meta": gin.H{"limit": limit, "offset": offset}})
}

func GetMerchantHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if merchant.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": merchant, "meta": gin.H{}})
}

type updateMerchantRequest struct {
	Name               string `json:"name" binding:"required"`
	KemkesFacilityCode string `json:"kemkes_facility_code"`
	BpjsPpkCode        string `json:"bpjs_ppk_code"`
	Timezone           string `json:"timezone" binding:"required"`
	IsActive           bool   `json:"is_active"`
}

func UpdateMerchantHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if !RequirePermissionForMerchant(c, PermMerchantManage, existing.ID) {
		return
	}
	var req updateMerchantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchant, err := q.UpdateMerchant(c.Request.Context(), sqlcgen.UpdateMerchantParams{
		ID:                 id,
		Name:               req.Name,
		KemkesFacilityCode: pgtype.Text{String: req.KemkesFacilityCode, Valid: req.KemkesFacilityCode != ""},
		BpjsPpkCode:        pgtype.Text{String: req.BpjsPpkCode, Valid: req.BpjsPpkCode != ""},
		Timezone:           req.Timezone,
		IsActive:           req.IsActive,
		UpdatedBy:          AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": merchant, "meta": gin.H{}})
}

func DeleteMerchantHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetMerchantByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	if !RequirePermissionForMerchant(c, PermMerchantManage, existing.ID) {
		return
	}
	if err := q.SoftDeleteMerchant(c.Request.Context(), sqlcgen.SoftDeleteMerchantParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
