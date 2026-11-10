// internal/server/clinical_form_sign.go
// CPPT Fase 1 sign/lock/addendum/counter-sign (spec
// 2026-10-21-cppt-rajal-fase1-design §6, §9; plan A Task 3). All handlers run
// inside the ambient RunRequestTx — errors after the first write go through
// respondInternalError, which rolls back automatically.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterClinicalSignRoutes wires the sign/counter-sign/addendum endpoints
// (called from RegisterClinicalRoutes, spec §9:184-186).
func RegisterClinicalSignRoutes(rg *gin.RouterGroup) {
	rg.POST("/clinical/forms/:id/sign", SignClinicalFormHandler)
	rg.POST("/clinical/forms/:id/countersign", CountersignClinicalFormHandler)
	rg.POST("/clinical/forms/:id/addendum", AddendumClinicalFormHandler)
}

// entryPasswordHasher verifies re-auth passwords with the same mechanism as
// login (auth.PasswordHasher.Verify — Argon2 parameters ride the stored hash,
// so a verifier built with the config defaults confirms every hash produced
// by the configured hasher).
var entryPasswordHasher = auth.NewPasswordHasher(8, 5*time.Second, auth.Argon2Params{
	MemoryKiB: 19456, Iterations: 2, Parallelism: 1,
})

// verifyEntryUserPassword re-authenticates the active user without issuing a
// token (spec §6.1): wrong password → 401 "password salah" and no writes.
func verifyEntryUserPassword(c *gin.Context, q *sqlcgen.Queries, userID pgtype.UUID, password string) bool {
	user, err := q.GetAppUserByID(c.Request.Context(), userID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	valid, err := entryPasswordHasher.Verify(c.Request.Context(), user.PasswordHash, password)
	if err != nil {
		if errors.Is(err, auth.ErrHashQueueTimeout) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
			return false
		}
		respondInternalError(c, err)
		return false
	}
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "password salah"})
		return false
	}
	return true
}

type signPasswordRequest struct {
	Password string `json:"password" binding:"required"`
}

// persistSignDiagnoses lives in clinical_diagnosis.go (plan A Task 4): it
// writes clinical.diagnosis rows for entries whose assessment section carries
// diagnoses (spec §6.1, soap_dokter only). Client-side diagnosis errors come
// back as errInvalidDiagnosis → 400; anything else is internal.

// SignClinicalFormHandler godoc
// @Summary Sign (TTE) a draft clinical form entry; locks it
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Form entry UUID"
// @Param request body signPasswordRequest true "Re-auth password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/forms/{id}/sign [post]
func SignClinicalFormHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	var req signPasswordRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	// FOR UPDATE so a concurrent sign cannot slip between checks and write.
	entry, err := q.GetClinicalFormInstanceForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, entry.CompanyID, merchantID, entry.FormType)
	if err != nil || !found {
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "template form tidak ditemukan"})
		return
	}
	if !RequirePermissionForMerchant(c, clinicalWritePerm(entry.FormType, tpl.Category), merchantID) {
		return
	}
	if entry.FilledBy != AuthUserID(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "hanya penulis entri"})
		return
	}
	if entry.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	if !verifyEntryUserPassword(c, q, AuthUserID(c), req.Password) {
		return
	}
	// Guard TTE sign (spec 2026-11-10-cppt-pemeriksaan-v3 §Guard TTE sign):
	// soap_dokter v3 wajib mengisi diagnosa primer (maks 1), keluhan utama,
	// dan jenis anamnesis sebelum TTE. Dijalankan SEBELUM tulis pertama
	// (q.SignClinicalFormInstance) — 400 di sini tidak menulis apa pun dan
	// ambient RunRequestTx tidak perlu rollback eksplisit. Moda RTL TIDAK
	// dicek server (guard lunak — FE).
	if entry.FormType == "soap_dokter" {
		if msg, blocked := soapSignGuardMessage(entry.FormData); blocked {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
	}
	var formData map[string]any
	if err := json.Unmarshal(entry.FormData, &formData); err != nil {
		respondInternalError(c, err)
		return
	}
	hash, err := entrySigner.Signature(id.String(), entry.AdmissionID.String(), entry.FormType,
		int(entry.TemplateVersion), entry.FilledBy.String(), entry.FilledAt.Time, formData)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	signed, err := q.SignClinicalFormInstance(ctx, sqlcgen.SignClinicalFormInstanceParams{
		ID: id, SignedBy: AuthUserID(c),
		SignedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		SignatureHash: pgtype.Text{String: hash, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Diagnosis write (soap_dokter only, addendum included; resume never
	// writes diagnosis — spec §6.1). Invalid payload/unresolvable concept →
	// 400 invalid diagnosis entry; RunRequestTx rolls the sign write back so
	// the entry stays draft (spec §6.1). Other errors are internal.
	if entry.FormType == "soap_dokter" {
		if err := persistSignDiagnoses(c, q, entry, AuthUserID(c), formData); err != nil {
			if errors.Is(err, errInvalidDiagnosis) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid diagnosis entry"})
				return
			}
			if errors.Is(err, errSuspectNotBoolean) {
				// Spec 2a §6.4/A1 (remedial 6536088): 400 after the sign
				// write → RunRequestTx rolls back; entry stays draft.
				c.JSON(http.StatusBadRequest, gin.H{"error": "suspect harus boolean"})
				return
			}
			respondInternalError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entry": clinicalFormJSON(signed)}, "meta": gin.H{}})
}

// soapSignGuardMessage evaluates the v3 sign-time mandatory sections on the
// stored form_data (spec 2026-11-10-cppt-pemeriksaan-v3 §Guard TTE sign,
// soap_dokter only). Unparseable/missing shapes count as empty — never
// panics. Order is fixed: diagnosa primer → keluhan utama → jenis anamnesis.
// More than one primary diagnosis returns immediately with its own message.
func soapSignGuardMessage(formData json.RawMessage) (string, bool) {
	var wrapper struct {
		Sections map[string]any `json:"sections"`
	}
	_ = json.Unmarshal(formData, &wrapper)
	sections := wrapper.Sections
	primary := 0
	if dx, ok := sections["diagnosa"].(map[string]any); ok {
		if rows, ok := dx["diagnoses"].([]any); ok {
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					if role, _ := m["role"].(string); role == "primary" {
						primary++
					}
				}
			}
		}
	}
	if primary > 1 {
		return "diagnosa primer maksimal 1", true
	}
	var missing []string
	if primary == 0 {
		missing = append(missing, "diagnosa primer")
	}
	if ku, _ := sections["keluhan_utama"].(string); strings.TrimSpace(ku) == "" {
		missing = append(missing, "keluhan utama")
	}
	jenis := ""
	if ai, ok := sections["anamnesis_info"].(map[string]any); ok {
		jenis, _ = ai["jenis"].(string)
	}
	if jenis != "autoanamnesis" && jenis != "alloanamnesis" {
		missing = append(missing, "jenis anamnesis")
	}
	if len(missing) == 0 {
		return "", false
	}
	return "bagian wajib belum terisi: " + strings.Join(missing, ", "), true
}

// CountersignClinicalFormHandler godoc
// @Summary Counter-sign (DPJP) a signed clinical form entry
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Form entry UUID"
// @Param request body signPasswordRequest true "Re-auth password"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/forms/{id}/countersign [post]
func CountersignClinicalFormHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	var req signPasswordRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	if !RequirePermissionForMerchant(c, PermClinicalRecordCountersign, merchantID) {
		return
	}
	entry, err := q.GetClinicalFormInstanceForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if entry.Status != "signed" {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah ditandatangani dan terkunci"})
		return
	}
	if !verifyEntryUserPassword(c, q, AuthUserID(c), req.Password) {
		return
	}
	updated, err := q.CountersignClinicalFormInstance(ctx, sqlcgen.CountersignClinicalFormInstanceParams{
		ID: id, CounterSignedBy: AuthUserID(c),
		CounterSignedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "entri sudah di-counter-sign"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"entry": clinicalFormJSON(updated)}, "meta": gin.H{}})
}

type addendumClinicalFormRequest struct {
	FormData    json.RawMessage `json:"form_data" binding:"required"`
	Reason      string          `json:"reason"`
	AdmissionID string          `json:"admission_id"`
	FormType    string          `json:"form_type"`
}

// AddendumClinicalFormHandler godoc
// @Summary Create an addendum draft referencing a signed entry
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Target (signed) form entry UUID"
// @Param request body addendumClinicalFormRequest true "Addendum form data + reason"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/forms/{id}/addendum [post]
func AddendumClinicalFormHandler(c *gin.Context) {
	targetID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	var req addendumClinicalFormRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "alasan addendum wajib"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	target, err := q.GetClinicalFormInstanceForUpdate(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "entri tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	tpl, found, err := resolveClinicalFormTemplate(ctx, q, target.CompanyID, merchantID, target.FormType)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "template form tidak ditemukan"})
		return
	}
	if !RequirePermissionForMerchant(c, clinicalWritePerm(target.FormType, tpl.Category), merchantID) {
		return
	}
	// Addendum must stay within the same admission and form type (spec §9:186).
	if req.FormType != "" && req.FormType != target.FormType {
		c.JSON(http.StatusBadRequest, gin.H{"error": "addendum harus sesama kunjungan dan jenis form"})
		return
	}
	if req.AdmissionID != "" {
		if admID, okID := parseUUID(req.AdmissionID); !okID || admID != target.AdmissionID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "addendum harus sesama kunjungan dan jenis form"})
			return
		}
	}
	if target.Status != "signed" {
		c.JSON(http.StatusConflict, gin.H{"error": "entri asal belum ditandatangani"})
		return
	}
	var defs clinicalSectionsDef
	if err := json.Unmarshal(tpl.Sections, &defs); err != nil {
		respondInternalError(c, err)
		return
	}
	warnings, okV := validateClinicalFormData(c, target.FormType, defs, req.FormData)
	if !okV {
		return
	}
	entry, err := q.CreateClinicalFormInstance(ctx, sqlcgen.CreateClinicalFormInstanceParams{
		CompanyID: target.CompanyID, MerchantID: target.MerchantID,
		AdmissionID: target.AdmissionID, FormType: target.FormType,
		TemplateID: target.TemplateID, TemplateVersion: target.TemplateVersion,
		FormData: req.FormData, FilledBy: AuthUserID(c),
		AddendumToID:   target.ID,
		AddendumReason: pgtype.Text{String: strings.TrimSpace(req.Reason), Valid: true},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"entry": clinicalFormJSON(entry), "warnings": warnings}, "meta": gin.H{}})
}
