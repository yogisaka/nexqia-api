// internal/server/mrn_config.go
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// defaultMRNFormat is used when a company has never configured core.mrn_config
// (no row at all — not even the company default).
const defaultMRNFormat = "RM-{YYYY}-{SEQ:5}"

var mrnSeqToken = regexp.MustCompile(`\{SEQ:(\d+)\}`)

// RegisterMRNConfigRoutes wires the configurable medical_record_no format
// (migrations/000032): one company-level default plus an optional per-merchant
// override, resolved by generateMedicalRecordNo on every CreatePersonHandler
// call. Company config reuses PermCompanyManage, merchant override reuses
// PermMerchantManage — same RBAC split tenancy.go already uses for these two levels.
func RegisterMRNConfigRoutes(rg *gin.RouterGroup) {
	rg.GET("/companies/:id/mrn-config", GetCompanyMRNConfigHandler)
	rg.PUT("/companies/:id/mrn-config", UpsertCompanyMRNConfigHandler)
	rg.GET("/merchants/:id/mrn-config", GetMerchantMRNConfigHandler)
	rg.PUT("/merchants/:id/mrn-config", UpsertMerchantMRNConfigHandler)
	rg.DELETE("/merchants/:id/mrn-config", DeleteMerchantMRNConfigHandler)
}

type mrnConfigRequest struct {
	Format string `json:"format" binding:"required"`
}

// validateMRNFormat requires exactly one {SEQ:N} token — generateMedicalRecordNo
// depends on that to split the template into prefix/suffix.
func validateMRNFormat(format string) error {
	matches := mrnSeqToken.FindAllString(format, -1)
	if len(matches) != 1 {
		return fmt.Errorf("format must contain exactly one {SEQ:N} token, found %d", len(matches))
	}
	return nil
}

// mrnConfigData is the single response shape of every mrn-config endpoint:
// snake_case keys with the flags INSIDE data, because the frontend's apiFetch
// returns only `data` and drops `meta`. is_default is true only on the company
// endpoint when no row exists (built-in format in effect); is_override is true
// only on the merchant endpoint when the merchant has its own row.
func mrnConfigData(companyID, merchantID pgtype.UUID, format string, isDefault, isOverride bool) gin.H {
	return gin.H{"company_id": companyID, "merchant_id": merchantID, "format": format, "is_default": isDefault, "is_override": isOverride}
}

// GetCompanyMRNConfigHandler godoc
// @Summary Get a company's default MRN format
// @Tags mrn-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company ID"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/mrn-config [get]
func GetCompanyMRNConfigHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	cfg, err := q.GetCompanyMRNConfig(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(id, pgtype.UUID{}, defaultMRNFormat, true, false), "meta": gin.H{"is_default": true}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(cfg.CompanyID, cfg.MerchantID, cfg.Format, false, false), "meta": gin.H{}})
}

// UpsertCompanyMRNConfigHandler godoc
// @Summary Set a company's default MRN format
// @Tags mrn-config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Company ID"
// @Param request body mrnConfigRequest true "MRN format template"
// @Success 200 {object} apiResponse
// @Router /companies/{id}/mrn-config [put]
func UpsertCompanyMRNConfigHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermCompanyManageOwn, PermCompanyManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "company not found"})
		return
	}
	var req mrnConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateMRNFormat(req.Format); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	cfg, err := q.UpsertCompanyMRNConfig(c.Request.Context(), sqlcgen.UpsertCompanyMRNConfigParams{
		CompanyID: id, Format: req.Format, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(cfg.CompanyID, cfg.MerchantID, cfg.Format, false, false), "meta": gin.H{}})
}

// GetMerchantMRNConfigHandler godoc
// @Summary Get a merchant's MRN format override (falls back to company default if unset)
// @Tags mrn-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/mrn-config [get]
func GetMerchantMRNConfigHandler(c *gin.Context) {
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
	cfg, err := q.GetMerchantMRNConfig(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No override — resolve what generateMedicalRecordNo would actually use.
		format, err := q.GetMRNFormat(c.Request.Context(), sqlcgen.GetMRNFormatParams{MerchantID: pgtype.UUID{}, CompanyID: AuthCompanyID(c)})
		if errors.Is(err, pgx.ErrNoRows) {
			format = defaultMRNFormat
		} else if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(AuthCompanyID(c), id, format, false, false), "meta": gin.H{"is_override": false}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(cfg.CompanyID, cfg.MerchantID, cfg.Format, false, true), "meta": gin.H{"is_override": true}})
}

// UpsertMerchantMRNConfigHandler godoc
// @Summary Set a merchant-specific MRN format override
// @Tags mrn-config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Param request body mrnConfigRequest true "MRN format template"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/mrn-config [put]
func UpsertMerchantMRNConfigHandler(c *gin.Context) {
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req mrnConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateMRNFormat(req.Format); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	cfg, err := q.UpsertMerchantMRNConfig(c.Request.Context(), sqlcgen.UpsertMerchantMRNConfigParams{
		CompanyID: AuthCompanyID(c), MerchantID: id, Format: req.Format, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mrnConfigData(cfg.CompanyID, cfg.MerchantID, cfg.Format, false, true), "meta": gin.H{}})
}

// DeleteMerchantMRNConfigHandler godoc
// @Summary Remove a merchant's MRN format override (reverts to company default)
// @Tags mrn-config
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant ID"
// @Success 204
// @Router /merchants/{id}/mrn-config [delete]
func DeleteMerchantMRNConfigHandler(c *gin.Context) {
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
	if err := q.DeleteMerchantMRNConfig(c.Request.Context(), id); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// generateMedicalRecordNo resolves the effective format (merchant override,
// else company default, else defaultMRNFormat), renders its {YYYY}/{YY}/{MM}/
// {MERCHANT_CODE} tokens, and assigns the next {SEQ:N} value.
//
// Must run inside the caller's request transaction: the tx-scoped advisory
// lock (LockSequence, keyed by company + resolved prefix) serializes
// concurrent check-ins so two requests in the same period/merchant can't
// compute the same next sequence number.
// Date tokens render in the merchant timezone (company-level formats without a merchant use the default zone).
func generateMedicalRecordNo(ctx context.Context, q *sqlcgen.Queries, companyID, merchantID pgtype.UUID) (string, error) {
	format, err := q.GetMRNFormat(ctx, sqlcgen.GetMRNFormatParams{MerchantID: merchantID, CompanyID: companyID})
	if errors.Is(err, pgx.ErrNoRows) {
		format = defaultMRNFormat
	} else if err != nil {
		return "", err
	}

	loc := mrnSeqToken.FindStringSubmatchIndex(format)
	if loc == nil {
		return "", fmt.Errorf("mrn format %q missing {SEQ:N} token", format)
	}
	width, err := strconv.Atoi(format[loc[2]:loc[3]])
	if err != nil || width <= 0 {
		return "", fmt.Errorf("mrn format %q has invalid {SEQ:N} width", format)
	}
	prefixTpl, suffixTpl := format[:loc[0]], format[loc[1]:]

	var merchantCode string
	if merchantID.Valid && (strings.Contains(prefixTpl, "{MERCHANT_CODE}") || strings.Contains(suffixTpl, "{MERCHANT_CODE}")) {
		merchant, err := q.GetMerchantByID(ctx, merchantID)
		if err != nil {
			return "", err
		}
		merchantCode = merchant.Code
	}

	tzLoc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		return "", err
	}
	now := time.Now().In(tzLoc)
	prefix := renderMRNTokens(prefixTpl, now, merchantCode)
	suffix := renderMRNTokens(suffixTpl, now, merchantCode)

	lockKey := prefix + "|" + suffix
	if err := q.LockSequence(ctx, sqlcgen.LockSequenceParams{Column1: companyID.String(), Column2: lockKey}); err != nil {
		return "", err
	}

	likePattern := escapeLike(prefix) + strings.Repeat("_", width) + escapeLike(suffix)
	maxSeq, err := q.MaxPersonMRNSeqAt(ctx, sqlcgen.MaxPersonMRNSeqAtParams{
		CompanyID:   companyID,
		SeqStart:    int32(len(prefix)) + 1, // SUBSTRING is 1-based
		SeqWidth:    int32(width),
		LikePattern: likePattern,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%0*d%s", prefix, width, maxSeq+1, suffix), nil
}

func renderMRNTokens(s string, now time.Time, merchantCode string) string {
	r := strings.NewReplacer(
		"{YYYY}", fmt.Sprintf("%04d", now.Year()),
		"{YY}", fmt.Sprintf("%02d", now.Year()%100),
		"{MM}", fmt.Sprintf("%02d", int(now.Month())),
		"{MERCHANT_CODE}", merchantCode,
	)
	return r.Replace(s)
}

// escapeLike escapes LIKE metacharacters in a literal fragment so it can be
// concatenated with '_' wildcards (the {SEQ:N} slot) in a single pattern —
// paired with `ESCAPE '\'` on the query side.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
