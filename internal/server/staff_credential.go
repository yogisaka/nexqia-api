// internal/server/staff_credential.go
// Staff credential (STR/SIP/SIPA) status + CRUD — spec
// 2026-10-20-staff-credential-expiry-design §4.1-§4.2.
package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// credentialWarnDaysFallback is the "expiring soon" window in days. Plan A2
// reads the real value from config; handlers pass this constant for now.
const credentialWarnDaysFallback = 30

func RegisterStaffCredentialRoutes(rg *gin.RouterGroup) {
	rg.GET("/staff-credential-status", ListStaffCredentialStatusHandler)
	rg.POST("/staff-credentials", CreateStaffCredentialHandler)
	rg.PATCH("/staff-credentials/:id", UpdateStaffCredentialHandler)
	rg.DELETE("/staff-credentials/:id", DeleteStaffCredentialHandler)
}

// credentialStatusInfo classifies one credential row against the merchant's
// local today: unknown when valid_until is NULL, expired when days < 0,
// expiring within warnDays, valid otherwise.
func credentialStatusInfo(validUntil pgtype.Date, today time.Time, warnDays int) (days *int, status string) {
	if !validUntil.Valid {
		return nil, "unknown"
	}
	vy, vm, vd := validUntil.Time.Date()
	ty, tm, td := today.Date()
	from := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
	to := time.Date(vy, vm, vd, 0, 0, 0, 0, time.UTC)
	d := int(to.Sub(from).Hours() / 24)
	switch {
	case d < 0:
		return &d, "expired"
	case d <= warnDays:
		return &d, "expiring"
	default:
		return &d, "valid"
	}
}

// credentialStatusSeverity orders the status list: worst first.
func credentialStatusSeverity(status string) int {
	switch status {
	case "expired":
		return 0
	case "expiring":
		return 1
	case "unknown":
		return 2
	default:
		return 3
	}
}

// staffCredentialJSON serializes a staff_credential row into its snake_case
// response shape.
func staffCredentialJSON(row sqlcgen.CoreStaffCredential) gin.H {
	var validUntil any
	if row.ValidUntil.Valid {
		validUntil = row.ValidUntil.Time.Format("2006-01-02")
	}
	return gin.H{
		"id":              row.ID.String(),
		"user_id":         row.UserID.String(),
		"credential_kind": row.CredentialKind,
		"license_number":  row.LicenseNumber,
		"valid_until":     validUntil,
		"row_version":     row.RowVersion,
	}
}

// requireCredentialMerchant guards the caller's merchant context: the token
// must carry an active merchant (spec §4.1 — credentials are merchant-scoped).
func requireCredentialMerchant(c *gin.Context) (pgtype.UUID, bool) {
	merchantID := AuthMerchantID(c)
	if !merchantID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant context required"})
		return pgtype.UUID{}, false
	}
	return merchantID, true
}

// ListStaffCredentialStatusHandler godoc
// @Summary List staff credential expiry status (physicians + staff)
// @Tags staff_credential
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /staff-credential-status [get]
func ListStaffCredentialStatusHandler(c *gin.Context) {
	if !RequirePermission(c, PermUserManage) {
		return
	}
	merchantID, ok := requireCredentialMerchant(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListStaffCredentialStatus(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	type entry struct {
		item gin.H
		rank int
		name string
	}
	entries := make([]entry, 0, len(rows))
	for _, row := range rows {
		days, status := credentialStatusInfo(row.ValidUntil, time.Now().In(loc), credentialWarnDaysFallback)
		var validUntil any
		if row.ValidUntil.Valid {
			validUntil = row.ValidUntil.Time.Format("2006-01-02")
		}
		var userID any
		if row.UserID.Valid {
			userID = row.UserID.String()
		}
		entries = append(entries, entry{
			item: gin.H{
				"staff_kind":      row.StaffKind,
				"user_id":         userID,
				"name":            row.StaffName,
				"credential_kind": row.CredentialKind,
				"license_number":  row.LicenseNumber.String,
				"valid_until":     validUntil,
				"days_remaining":  days,
				"status":          status,
			},
			rank: credentialStatusSeverity(status),
			name: row.StaffName,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		return entries[i].name < entries[j].name
	})
	data := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		data = append(data, e.item)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"count": len(data)}})
}

type createStaffCredentialRequest struct {
	UserID         string `json:"user_id"`
	CredentialKind string `json:"credential_kind"`
	LicenseNumber  string `json:"license_number"`
	ValidUntil     string `json:"valid_until"`
}

// CreateStaffCredentialHandler godoc
// @Summary Create a staff credential
// @Tags staff_credential
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createStaffCredentialRequest true "Credential data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /staff-credentials [post]
func CreateStaffCredentialHandler(c *gin.Context) {
	if !RequirePermission(c, PermUserManage) {
		return
	}
	merchantID, ok := requireCredentialMerchant(c)
	if !ok {
		return
	}
	var req createStaffCredentialRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	userID, valid := parseUUID(req.UserID)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Same company AND active member of this merchant; a same-company
	// non-member is indistinguishable from a missing user (404 — never leak
	// membership, pattern rbac.go scoped-out).
	if _, err := q.GetUserForCredential(c.Request.Context(), sqlcgen.GetUserForCredentialParams{
		ID: userID, CompanyID: AuthCompanyID(c), MerchantID: merchantID,
	}); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	switch req.CredentialKind {
	case "str", "sip", "sipa":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential_kind must be one of str, sip, sipa"})
		return
	}
	license := strings.TrimSpace(req.LicenseNumber)
	if license == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "license_number is required"})
		return
	}
	validUntil, ok := parseDateStr(req.ValidUntil)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_until"})
		return
	}
	row, err := q.CreateStaffCredential(c.Request.Context(), sqlcgen.CreateStaffCredentialParams{
		CompanyID:      AuthCompanyID(c),
		MerchantID:     merchantID,
		UserID:         userID,
		CredentialKind: req.CredentialKind,
		LicenseNumber:  license,
		ValidUntil:     validUntil,
		CreatedBy:      AuthUserID(c),
	})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "staff credential already exists"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": staffCredentialJSON(row), "meta": gin.H{}})
}

type updateStaffCredentialRequest struct {
	LicenseNumber string `json:"license_number"`
	ValidUntil    string `json:"valid_until"`
}

// UpdateStaffCredentialHandler godoc
// @Summary Replace a staff credential's license number and valid_until
// @Tags staff_credential
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Credential UUID"
// @Param request body updateStaffCredentialRequest true "Credential data"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /staff-credentials/{id} [patch]
func UpdateStaffCredentialHandler(c *gin.Context) {
	if !RequirePermission(c, PermUserManage) {
		return
	}
	merchantID, ok := requireCredentialMerchant(c)
	if !ok {
		return
	}
	id, valid := parseUUID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req updateStaffCredentialRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// RLS makes another merchant's id look like a missing row.
	existing, err := q.GetStaffCredentialByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "staff credential not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if existing.MerchantID != merchantID {
		c.JSON(http.StatusNotFound, gin.H{"error": "staff credential not found"})
		return
	}
	license := strings.TrimSpace(req.LicenseNumber)
	if license == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "license_number is required"})
		return
	}
	// Full replace: "" (or absent) clears valid_until.
	validUntil, ok := parseDateStr(req.ValidUntil)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_until"})
		return
	}
	row, err := q.UpdateStaffCredential(c.Request.Context(), sqlcgen.UpdateStaffCredentialParams{
		ID: id, LicenseNumber: license, ValidUntil: validUntil,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": staffCredentialJSON(row), "meta": gin.H{}})
}

// DeleteStaffCredentialHandler godoc
// @Summary Soft-delete a staff credential
// @Tags staff_credential
// @Produce json
// @Security BearerAuth
// @Param id path string true "Credential UUID"
// @Success 204 "No Content"
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /staff-credentials/{id} [delete]
func DeleteStaffCredentialHandler(c *gin.Context) {
	if !RequirePermission(c, PermUserManage) {
		return
	}
	merchantID, ok := requireCredentialMerchant(c)
	if !ok {
		return
	}
	id, valid := parseUUID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetStaffCredentialByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "staff credential not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if existing.MerchantID != merchantID {
		c.JSON(http.StatusNotFound, gin.H{"error": "staff credential not found"})
		return
	}
	if err := q.SoftDeleteStaffCredential(c.Request.Context(), sqlcgen.SoftDeleteStaffCredentialParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
