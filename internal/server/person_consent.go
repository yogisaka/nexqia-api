// internal/server/person_consent.go
// Patient data-processing consent API (spec 2026-10-17-patient-documents-design
// §5.3): append-only consent recording over core.person_consent plus the
// active consent_text feed (migration 000074). Rows are immutable — app_runtime
// has no UPDATE/DELETE grant on the table.
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermPatientConsentRecord guards every consent endpoint (migration 000074).
const PermPatientConsentRecord = "patient.consent.record"

// allowedConsentMethods mirrors the core.person_consent method CHECK.
var allowedConsentMethods = map[string]bool{"form_signed": true, "verbal": true}

// RegisterPersonConsentRoutes wires the consent endpoints. The person path
// param is `:id` (not the spec's `:personId`) — same gin wildcard-conflict
// constraint as RegisterPersonDocumentRoutes: RegisterPersonRoutes already
// owns `/persons/:id`.
func RegisterPersonConsentRoutes(rg *gin.RouterGroup) {
	rg.POST("/persons/:id/consents", CreatePersonConsentHandler)
	rg.GET("/persons/:id/consents", ListPersonConsentsHandler)
	rg.GET("/persons/:id/consents/latest", GetLatestPersonConsentHandler)
	rg.GET("/consent-text", GetConsentTextHandler)
}

// consentPayload renders one consent row in the snake_case shape of spec §5.3;
// granted_at is RFC3339 in the merchant's timezone.
func consentPayload(id, personID pgtype.UUID, consentType, textVersion, method string, personDocumentID pgtype.UUID, grantedAt pgtype.Timestamptz, grantedBy pgtype.UUID, grantedByName string, loc *time.Location) gin.H {
	var documentID any
	if personDocumentID.Valid {
		documentID = personDocumentID.String()
	}
	return gin.H{
		"id":                 id.String(),
		"person_id":          personID.String(),
		"consent_type":       consentType,
		"text_version":       textVersion,
		"method":             method,
		"person_document_id": documentID,
		"granted_at":         grantedAt.Time.In(loc).Format(time.RFC3339),
		"granted_by":         grantedBy.String(),
		"granted_by_name":    grantedByName,
	}
}

// loadConsentPerson resolves the person path param and the consent permission.
func loadConsentPerson(c *gin.Context) (pgtype.UUID, bool) {
	if !RequirePermission(c, PermPatientConsentRecord) {
		return pgtype.UUID{}, false
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return pgtype.UUID{}, false
	}
	return id, true
}

type createPersonConsentRequest struct {
	ConsentType      string `json:"consent_type" binding:"required"`
	TextVersion      string `json:"text_version" binding:"required"`
	Method           string `json:"method" binding:"required"`
	PersonDocumentID string `json:"person_document_id"`
}

// CreatePersonConsentHandler godoc
// @Summary Record a data-processing consent for a person (append-only)
// @Tags patient-consent
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param request body createPersonConsentRequest true "Consent data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /persons/{id}/consents [post]
func CreatePersonConsentHandler(c *gin.Context) {
	personID, ok := loadConsentPerson(c)
	if !ok {
		return
	}
	var req createPersonConsentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.ConsentType != "data_processing" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jenis persetujuan tidak dikenal"})
		return
	}
	if !allowedConsentMethods[req.Method] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "metode persetujuan tidak dikenal"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	// The submitted text_version must match the currently active consent_text
	// version for this code (spec §5.3).
	text, err := q.GetActiveConsentText(ctx, req.ConsentType)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && text.Version != req.TextVersion) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "versi teks persetujuan tidak dikenal"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var documentID pgtype.UUID
	if req.PersonDocumentID != "" {
		id, okDoc := parseUUID(req.PersonDocumentID)
		if !okDoc {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_document_id"})
			return
		}
		// The referenced document must exist and belong to this person
		// (pattern: fetchPersonDocument in person_document.go — isolation is
		// enforced in the handler, not just RLS).
		doc, err := q.GetPersonDocumentByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && doc.PersonID != personID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dokumen tidak valid untuk pasien ini"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		documentID = id
	}
	if _, err := q.CreatePersonConsent(ctx, sqlcgen.CreatePersonConsentParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: personID,
		ConsentType: req.ConsentType, TextVersion: req.TextVersion, Method: req.Method,
		PersonDocumentID: documentID, GrantedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	// Echo the stored row with the recorder's name (contract shape includes
	// granted_by_name); the just-inserted row is the latest for this person.
	row, err := q.GetLatestPersonConsent(ctx, personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"consent": consentPayload(
		row.ID, row.PersonID, row.ConsentType, row.TextVersion, row.Method,
		row.PersonDocumentID, row.GrantedAt, row.GrantedBy, row.GrantedByName, loc,
	)})
}

// ListPersonConsentsHandler godoc
// @Summary List a person's consents (newest first)
// @Tags patient-consent
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Router /persons/{id}/consents [get]
func ListPersonConsentsHandler(c *gin.Context) {
	personID, ok := loadConsentPerson(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListPersonConsents(ctx, personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, len(rows))
	for i, r := range rows {
		items[i] = consentPayload(r.ID, r.PersonID, r.ConsentType, r.TextVersion, r.Method,
			r.PersonDocumentID, r.GrantedAt, r.GrantedBy, r.GrantedByName, loc)
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetLatestPersonConsentHandler godoc
// @Summary Get the person's latest consent (null when none)
// @Tags patient-consent
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Router /persons/{id}/consents/latest [get]
func GetLatestPersonConsentHandler(c *gin.Context) {
	personID, ok := loadConsentPerson(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	row, err := q.GetLatestPersonConsent(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"consent": nil, "meta": gin.H{}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"consent": consentPayload(
		row.ID, row.PersonID, row.ConsentType, row.TextVersion, row.Method,
		row.PersonDocumentID, row.GrantedAt, row.GrantedBy, row.GrantedByName, loc,
	), "meta": gin.H{}})
}

// GetConsentTextHandler godoc
// @Summary Get the active consent text version for a code
// @Tags patient-consent
// @Produce json
// @Security BearerAuth
// @Param code query string true "Consent text code"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /consent-text [get]
func GetConsentTextHandler(c *gin.Context) {
	if !RequirePermission(c, PermPatientConsentRecord) {
		return
	}
	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing query param code"})
		return
	}
	text, err := sqlcgen.New(TxFromContext(c)).GetActiveConsentText(c.Request.Context(), code)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "teks persetujuan tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": text.Code, "version": text.Version, "body": text.Body})
}
