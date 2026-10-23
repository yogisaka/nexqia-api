// internal/server/visit_media_api.go
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

// validVisitMediaValues mirrors the media CHECK set (spec
// 2026-10-20-qr-visit-media-design.md §4.2) so a bad value is a 400, not a 500
// from the DB constraint.
var validVisitMediaValues = map[string]bool{
	"none": true, "slip": true, "wristband": true, "both": true,
}

// ReissueVisitTokenHandler closes the admission's active visit token and
// issues a fresh one (spec §4.2). No status gate: reissue works for any
// admission the caller can manage (print again mid-visit).
// ReissueVisitTokenHandler godoc
// @Summary Reissue the visit identity token of an admission
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Admission UUID"
// @Success 201 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id}/visit-token [post]
func ReissueVisitTokenHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, err := q.GetAdmissionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, admission.MerchantID) {
		return
	}
	// Close first (0 rows is fine — no active token yet), then issue the
	// replacement in the same request tx; a failure rolls both back.
	if _, err := q.CloseActiveVisitTokensByAdmission(c.Request.Context(), sqlcgen.CloseActiveVisitTokensByAdmissionParams{
		AdmissionID: admission.ID, ClosedBy: AuthUserID(c), CloseReason: pgtype.Text{String: "reissued", Valid: true},
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	vm := issueVisitMediaToken(c, q, admission.CompanyID, admission.MerchantID, admission.ID, admission.DepartmentID, admission.PersonID, admission.FallRisk)
	if vm == nil {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"visit_media": vm}, "meta": gin.H{}})
}

// issueVisitMediaToken runs the token pipeline shared with POST /admissions
// (media resolve + expiry in the merchant zone + QR). It writes the token row
// in the request tx; on failure it responds via respondInternalError (tx
// rollback) and returns nil.
func issueVisitMediaToken(c *gin.Context, q *sqlcgen.Queries, companyID, merchantID, admissionID, departmentID, personID pgtype.UUID, fallRisk bool) gin.H {
	ctx := c.Request.Context()
	loc, err := merchantLocation(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return nil
	}
	rows, err := q.ListVisitMediaConfig(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return nil
	}
	media := resolveVisitMedia(rows, departmentID)
	raw, hash, err := randomTokenCode("NQV1-")
	if err != nil {
		respondInternalError(c, err)
		return nil
	}
	if _, err := q.CreateVisitToken(ctx, sqlcgen.CreateVisitTokenParams{
		CompanyID: companyID, MerchantID: merchantID, AdmissionID: admissionID,
		TokenHash: hash, Media: media,
		ExpiresAt: pgtype.Timestamptz{Time: visitTokenExpiry(time.Now(), loc), Valid: true},
		CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return nil
	}
	allergy, err := q.CountActivePatientAllergies(ctx, personID)
	if err != nil {
		respondInternalError(c, err)
		return nil
	}
	qr, qrErr := QRPNGDataURI(raw)
	var nullableQR any
	if qrErr == nil {
		nullableQR = qr
	}
	return gin.H{"media": media, "token": raw, "qr_png": nullableQR, "allergy_alert": allergy > 0, "fall_risk": fallRisk}
}

// RegisterVisitMediaRoutes registers the visit media config endpoints
// (spec §4.2). The reissue route lives in RegisterAdmissionRoutes.
func RegisterVisitMediaRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/visit-media", GetVisitMediaHandler)
	rg.PUT("/visit-media", PutVisitMediaHandler)
	rg.DELETE("/visit-media/:department_id", DeleteVisitMediaOverrideHandler)
}

// GetVisitMediaHandler godoc
// @Summary Visit media default and per-department overrides
// @Tags queue
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id}/visit-media [get]
func GetVisitMediaHandler(c *gin.Context) {
	if !RequirePermission(c, PermQueueConfigure) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok || id != AuthMerchantID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListVisitMediaConfig(c.Request.Context(), id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	mediaDefault := "slip"
	overrides := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		if r.DepartmentID.Valid {
			overrides = append(overrides, gin.H{"department_id": r.DepartmentID.String(), "media": r.Media})
		} else {
			mediaDefault = r.Media
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"default": mediaDefault, "overrides": overrides}, "meta": gin.H{}})
}

type putVisitMediaRequest struct {
	DepartmentID *string `json:"department_id"`
	Media        string  `json:"media"`
}

// PutVisitMediaHandler godoc
// @Summary Upsert the visit media default (department_id null) or an override
// @Tags queue
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body putVisitMediaRequest true "Media config"
// @Success 200 {object} apiResponse
// @Router /visit-media [put]
func PutVisitMediaHandler(c *gin.Context) {
	if !RequirePermission(c, PermQueueConfigure) {
		return
	}
	merchantID := AuthMerchantID(c)
	if !merchantID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant context required"})
		return
	}
	var req putVisitMediaRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if !validVisitMediaValues[req.Media] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "media must be one of none, slip, wristband, both"})
		return
	}
	var departmentID pgtype.UUID
	if req.DepartmentID != nil {
		id, ok := parseUUID(*req.DepartmentID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		dept, err := q.GetDepartmentMerchant(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && dept.MerchantID != merchantID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		departmentID = id
	}
	q := sqlcgen.New(TxFromContext(c))
	row, err := q.UpsertVisitMediaConfig(c.Request.Context(), sqlcgen.UpsertVisitMediaConfigParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, DepartmentID: departmentID,
		Media: req.Media, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"department_id": uuidJSON(row.DepartmentID), "media": row.Media}, "meta": gin.H{}})
}

// DeleteVisitMediaOverrideHandler godoc
// @Summary Delete a per-department visit media override
// @Tags queue
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param department_id path string true "Department UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /visit-media/{department_id} [delete]
func DeleteVisitMediaOverrideHandler(c *gin.Context) {
	if !RequirePermission(c, PermQueueConfigure) {
		return
	}
	id, ok := parseUUID(c.Param("department_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	deleted, err := q.DeleteVisitMediaOverride(c.Request.Context(), sqlcgen.DeleteVisitMediaOverrideParams{
		MerchantID: AuthMerchantID(c), DepartmentID: id,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if deleted == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "visit media override not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
