// internal/server/visit_scan.go
// POST /queue/scan — check-in by scanning the visit identity QR token
// (NQV1-…), see 2026-10-20-qr-visit-media-design.md §4.2/§4.4.
package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

type scanQueueRequest struct {
	Token     string `json:"token"`
	CounterID string `json:"counter_id"`
}

// ScanQueueHandler godoc
// @Summary Check a patient in by scanning their visit QR token
// @Description Resolves the NQV1- visit token (404 unknown, 409 closed/expired),
// @Description checks merchant permission, then runs the same check-in core as
// @Description POST /queue/checkin and returns {queue, journey, patient}.
// @Tags queue
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body scanQueueRequest true "Visit token + counter"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse "invalid token / invalid counter_id"
// @Failure 404 {object} apiErrorResponse "visit token not found"
// @Failure 409 {object} apiErrorResponse "visit token is no longer active / nothing to check in"
// @Router /queue/scan [post]
func ScanQueueHandler(c *gin.Context) {
	var req scanQueueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}
	if !isValidTokenFormat(req.Token, "NQV1-") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))

	token, err := q.GetVisitTokenByHash(ctx, hashTokenCode(req.Token))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "visit token not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if token.ClosedAt.Valid || !token.ExpiresAt.Time.After(time.Now()) {
		c.JSON(http.StatusConflict, gin.H{"error": "visit token is no longer active"})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, token.MerchantID) {
		return
	}
	ctxRow, err := q.GetAdmissionVisitContext(ctx, token.AdmissionID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Audit: only the person id — never name/NIK (spec §5).
	setAccessLogPersonID(c, ctxRow.PersonID)

	journey, err := q.GetQueueJourneyByAdmission(ctx, token.AdmissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "nothing to check in"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	counterID, ok := parseUUID(req.CounterID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counter_id"})
		return
	}
	payload, ok := checkInJourney(c, q, journey, counterID)
	if !ok {
		return
	}
	allergy, err := q.CountActivePatientAllergies(ctx, ctxRow.PersonID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var birthDate any
	if ctxRow.BirthDate.Valid {
		birthDate = ctxRow.BirthDate.Time.Format("2006-01-02")
	}
	var mrn any
	if ctxRow.MedicalRecordNo.Valid {
		mrn = ctxRow.MedicalRecordNo.String
	}
	payload["patient"] = gin.H{
		"full_name":         ctxRow.FullName,
		"birth_date":        birthDate,
		"medical_record_no": mrn,
		"allergy_alert":     allergy > 0,
		"fall_risk":         ctxRow.FallRisk,
	}
	c.JSON(http.StatusOK, gin.H{"data": payload, "meta": gin.H{}})
}
