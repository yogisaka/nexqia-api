// internal/server/tenancy.go
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/session"
)

var companyCodeFormat = regexp.MustCompile(`^[A-Z0-9]{6}$`)

const codeCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const codeGenerateMaxAttempts = 5

// generateCode returns a random 6-char uppercase alphanumeric code (company/merchant identifier).
func generateCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i, v := range b {
		b[i] = codeCharset[int(v)%len(codeCharset)]
	}
	return string(b), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// InsertWithUniqueCode runs insert with a fresh code from gen, retrying on a
// unique violation up to codeGenerateMaxAttempts times. Each attempt runs inside
// a SAVEPOINT (pgx nested transaction): without it, a failed INSERT aborts the
// caller's whole transaction and every retry fails with 25P02. Set any session
// GUC (set_config(..., true)) BEFORE calling this — a set_config issued inside a
// rolled-back savepoint is undone with it.
func InsertWithUniqueCode[T any](ctx context.Context, tx pgx.Tx, gen func() (string, error), insert func(q *sqlcgen.Queries, code string) (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		code, err := gen()
		if err != nil {
			return zero, err
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return zero, err
		}
		row, err := insert(sqlcgen.New(sp), code)
		if err == nil {
			return row, sp.Commit(ctx)
		}
		_ = sp.Rollback(ctx)
		if !isUniqueViolation(err) || attempt >= codeGenerateMaxAttempts-1 {
			return zero, err
		}
	}
}

// RegisterTenancyRoutes wires the PLATFORM-WIDE-only tenancy routes — company
// creation/listing spans tenants by nature (sub-project 0d, NEXQIA's own
// platform-admin scope), gated by the unsplit PermCompanyManage. Everything
// company-scoped-to-self or merchant-scoped lives in RegisterCompanyLevelRoutes
// instead (docs/design/specs/2026-09-23-saas-registration-owner-bootstrap-design.md §3).
func RegisterTenancyRoutes(rg *gin.RouterGroup) {
	rg.POST("/companies", CreateCompanyHandler)
	rg.GET("/companies", ListCompaniesHandler)
}

// RegisterCompanyLevelRoutes wires company-self-management + merchant CRUD —
// deliberately registered under companyOnlyAuthed (CompanyOnlyMiddleware, no
// X-Merchant-ID needed), not the merchant-required `locked` group, so a user
// with zero merchants (a freshly-registered Owner) can still reach them. See
// §3 for why TenantMiddleware can't be used here.
func RegisterCompanyLevelRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.GET("/companies/:id", GetCompanyHandler)
	rg.PATCH("/companies/:id", UpdateCompanyHandler)
	rg.DELETE("/companies/:id", DeleteCompanyHandler)

	rg.POST("/merchants", CreateMerchantHandler(cfg))
	rg.GET("/companies/:id/merchants", ListMerchantsHandler)
	rg.GET("/merchants/:id", GetMerchantHandler)
	rg.PATCH("/merchants/:id", UpdateMerchantHandler)
	rg.DELETE("/merchants/:id", DeleteMerchantHandler)
}

type createCompanyRequest struct {
	Name string `json:"name" binding:"required"`
}

// CreateCompanyHandler godoc
// @Summary Create a company
// @Description Platform-admin scope — company creation spans tenants by nature.
// @Tags tenancy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createCompanyRequest true "Company data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /companies [post]
func CreateCompanyHandler(c *gin.Context) {
	if !RequirePermission(c, PermCompanyManage) {
		return
	}
	var req createCompanyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	company, err := InsertWithUniqueCode(c.Request.Context(), TxFromContext(c), generateCode, func(q *sqlcgen.Queries, code string) (sqlcgen.CoreCompany, error) {
		return q.CreateCompany(c.Request.Context(), sqlcgen.CreateCompanyParams{
			Code: code, Name: req.Name, CreatedBy: AuthUserID(c),
		})
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": company, "meta": gin.H{}})
}

// ListCompaniesHandler godoc
// @Summary List companies
// @Description Lists across ALL tenants — platform-admin only.
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /companies [get]
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

// GetCompanyHandler godoc
// @Summary Get a company by id
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /companies/{id} [get]
func GetCompanyHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if id != AuthCompanyID(c) && !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
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

// UpdateCompanyHandler godoc
// @Summary Update a company
// @Tags tenancy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Param request body updateCompanyRequest true "Company data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /companies/{id} [patch]
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
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
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

// DeleteCompanyHandler godoc
// @Summary Soft-delete a company
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /companies/{id} [delete]
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
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
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
	Name               string `json:"name" binding:"required"`
	KemkesFacilityCode string `json:"kemkes_facility_code"`
	BpjsPpkCode        string `json:"bpjs_ppk_code"`
	Timezone           string `json:"timezone" binding:"required"`
}

// CreateMerchantHandler godoc
// @Summary Create a merchant
// @Tags tenancy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createMerchantRequest true "Merchant data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants [post]
func CreateMerchantHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		if !RequireCompanyLevelPermission(c, PermMerchantManage) {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		merchant, err := InsertWithUniqueCode(c.Request.Context(), TxFromContext(c), generateCode, func(q *sqlcgen.Queries, code string) (sqlcgen.CoreMerchant, error) {
			return q.CreateMerchant(c.Request.Context(), sqlcgen.CreateMerchantParams{
				CompanyID:          companyID,
				Code:               code,
				Name:               req.Name,
				KemkesFacilityCode: pgtype.Text{String: req.KemkesFacilityCode, Valid: req.KemkesFacilityCode != ""},
				BpjsPpkCode:        pgtype.Text{String: req.BpjsPpkCode, Valid: req.BpjsPpkCode != ""},
				Timezone:           req.Timezone,
				CreatedBy:          AuthUserID(c),
			})
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// §3 poin 6: if the creator has a company-wide role (the Owner-bootstrap
		// case — zero merchants until now), give them that SAME role at the
		// merchant they just created, so every ordinary merchant-scoped
		// RequirePermission call works for them from here on without falling
		// back through RequireCompanyLevelPermission every time.
		//
		// [2026-09-24, bug ketemu live] core.user_merchant_role RLS requires
		// current_setting('app.current_merchant_id') (migration 000004), but this
		// handler runs under companyOnlyAuthed (CompanyOnlyMiddleware) -- which
		// only ever sets app.current_company_id, never app.current_merchant_id
		// (no merchant context exists yet at the point this middleware runs,
		// chicken-and-egg same as company_id in RegisterHandler). The INSERT
		// below failed 42704 every single time, but the error was silently
		// discarded (`_, _ = ...`), so nobody noticed -- Postgres aborts the
		// WHOLE transaction after that failed statement, and the NEXT query
		// (session.SwitchMerchant a few lines down) surfaced a completely
		// unrelated-looking 25P02 "current transaction is aborted" instead of the
		// real cause. Fixed the same way RegisterHandler solves the identical
		// problem for app.current_company_id: set_config the GUC to the value
		// the new row needs to match BEFORE the insert, transaction-scoped
		// (true). Also stopped swallowing the error -- if this fails now, the
		// whole request fails loudly (AbortWithStatusJSON), not silently leaves
		// the Owner locked out of their own new merchant.
		if _, err := TxFromContext(c).Exec(c.Request.Context(), "SELECT set_config('app.current_merchant_id', $1, true)", merchant.ID.String()); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set merchant context: " + err.Error()})
			return
		}
		ucr, err := q.ListUserCompanyRoles(c.Request.Context(), AuthUserID(c))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, r := range ucr {
			if r.CompanyID == AuthCompanyID(c) {
				if _, err := q.AddUserMerchantRole(c.Request.Context(), sqlcgen.AddUserMerchantRoleParams{
					UserID: AuthUserID(c), MerchantID: merchant.ID, RoleID: r.RoleID, CreatedBy: AuthUserID(c),
				}); err != nil {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
			}
		}

		// Issue a merchant-scoped token directly — /auth/switch-merchant can't
		// be used here, it's registered behind TenantMiddleware which
		// hard-requires an X-Merchant-ID header the caller doesn't have yet
		// (they may have zero merchants, e.g. right after registration). See
		// docs/design/specs/2026-09-24-merchant-management-ui-design.md §3.5.
		rawToken, err := c.Cookie(refreshCookieName)
		if err != nil || rawToken == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing refresh token"})
			return
		}
		newToken, err := session.SwitchMerchant(c.Request.Context(), q, sessionConfig(cfg), rawToken, AuthUserID(c), merchant.ID)
		if err != nil {
			// [2026-09-24] Real cause diketel di pesan ini — sempat cuma "failed to
			// activate new merchant" generik pas bug ini dilaporin live, gak bisa
			// dibedain error mana (ErrNoSession/ErrSessionRevoked/ErrSessionMismatch/
			// DB error lain) tanpa akses log server langsung.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to activate new merchant: " + err.Error()})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"merchant": merchant,
			"token":    newToken,
		}, "meta": gin.H{}})
	}
}

// ListMerchantsHandler godoc
// @Summary List merchants under a company
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/merchants [get]
func ListMerchantsHandler(c *gin.Context) {
	companyID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if companyID != AuthCompanyID(c) && !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
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

// GetMerchantHandler godoc
// @Summary Get a merchant by id
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id} [get]
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

// UpdateMerchantHandler godoc
// @Summary Update a merchant
// @Tags tenancy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body updateMerchantRequest true "Merchant data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id} [patch]
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
	if !RequireCompanyLevelPermission(c, PermMerchantManage) {
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

// DeleteMerchantHandler godoc
// @Summary Soft-delete a merchant
// @Tags tenancy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id} [delete]
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
	if !RequireCompanyLevelPermission(c, PermMerchantManage) {
		return
	}
	if err := q.SoftDeleteMerchant(c.Request.Context(), sqlcgen.SoftDeleteMerchantParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
