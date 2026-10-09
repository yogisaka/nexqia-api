// internal/server/person_duplicate.go
// Duplicate-candidate detection and merge-request workflow
// (spec 2026-10-07-merge-medical-records §8.4, §8.5).
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// registerPersonDuplicateRoutes wires the duplicate-pair endpoints. Static
// /persons/duplicates* routes must be registered BEFORE /persons/:id (same
// reason as registerPersonMergeRoutes).
func registerPersonDuplicateRoutes(rg *gin.RouterGroup) {
	rg.GET("/persons/duplicates", AccessLog("person", "duplicate_list", ""), ListDuplicateCandidatesHandler)
	rg.GET("/persons/duplicates/check", AccessLog("person", "duplicate_check", ""), CheckNewPatientDuplicatesHandler)
	rg.POST("/persons/duplicates", AccessLog("person", "duplicate_request", ""), RequestDuplicatePairHandler)
	rg.POST("/persons/duplicates/dismiss", AccessLog("person", "duplicate_dismiss", ""), DismissDuplicatePairHandler)
}

// dupPersonSide renders one side of a candidate pair
// {id, mrn, name, birth_date, gender, created_at}.
func dupPersonSide(id pgtype.UUID, mrn pgtype.Text, name string, birthDate pgtype.Date, gender pgtype.Text, createdAt pgtype.Timestamptz) gin.H {
	return gin.H{
		"id":         id,
		"mrn":        mrn,
		"name":       name,
		"birth_date": birthDate,
		"gender":     gender,
		"created_at": createdAt,
	}
}

// ListDuplicateCandidatesHandler godoc
// @Summary List duplicate person candidate pairs
// @Description Scored pairs of possibly-duplicate medical records; dismissed/merged pairs are hidden, requested pairs sort first.
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /persons/duplicates [get]
func ListDuplicateCandidatesHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonMergeRequest) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListDuplicateCandidates(c.Request.Context(), sqlcgen.ListDuplicateCandidatesParams{
		CompanyID: AuthCompanyID(c), RowLimit: limit, RowOffset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		score, _ := r.Score.(int32)
		reason, _ := r.Reason.(string)
		items = append(items, gin.H{
			"person_a": dupPersonSide(r.AID, r.AMrn, r.AName, r.ABirthDate, r.AGender, r.ACreatedAt),
			"person_b": dupPersonSide(r.BID, r.BMrn, r.BName, r.BBirthDate, r.BGender, r.BCreatedAt),
			"score":    score,
			"reason":   reason,
			"status":   r.Status,
			"note":     r.Note,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"limit": limit, "offset": offset}})
}

// CheckNewPatientDuplicatesHandler godoc
// @Summary Check a prospective patient for duplicates
// @Description Scores existing live records against the registration form values before creating a new person.
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param full_name query string true "Candidate full name"
// @Param birth_date query string false "Birth date (YYYY-MM-DD)"
// @Param gender query string false "Gender"
// @Param phone query string false "Phone"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /persons/duplicates/check [get]
func CheckNewPatientDuplicatesHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	fullName := c.Query("full_name")
	if fullName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "full_name is required"})
		return
	}
	birthDate, err := optDate(c.Query("birth_date"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.CheckNewPatientDuplicates(c.Request.Context(), sqlcgen.CheckNewPatientDuplicatesParams{
		CompanyID: AuthCompanyID(c),
		FullName:  fullName,
		BirthDate: birthDate,
		Gender:    optText(c.Query("gender")),
		Phone:     optText(c.Query("phone")),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		items = append(items, gin.H{
			"id":                r.ID,
			"medical_record_no": r.MedicalRecordNo,
			"full_name":         r.FullName,
			"birth_date":        r.BirthDate,
			"gender":            r.Gender,
			"score":             r.Score,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

type duplicatePairRequest struct {
	PersonAID string `json:"person_a_id"`
	PersonBID string `json:"person_b_id"`
	Note      string `json:"note"`
}

// upsertDuplicatePair validates the pair request, checks both persons live in
// the caller's company and applies the status upsert. Returns the upserted
// row; ok=false when the response has already been written.
func upsertDuplicatePair(c *gin.Context, status string) (sqlcgen.UpsertDuplicatePairRow, bool) {
	var req duplicatePairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return sqlcgen.UpsertDuplicatePairRow{}, false
	}
	aID, okA := parseUUID(req.PersonAID)
	bID, okB := parseUUID(req.PersonBID)
	if !okA || !okB || aID == bID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.UpsertDuplicatePairRow{}, false
	}
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()
	for _, id := range []pgtype.UUID{aID, bID} {
		person, err := q.GetPersonByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return sqlcgen.UpsertDuplicatePairRow{}, false
		}
		if err != nil {
			respondInternalError(c, err)
			return sqlcgen.UpsertDuplicatePairRow{}, false
		}
		if person.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return sqlcgen.UpsertDuplicatePairRow{}, false
		}
	}
	row, err := q.UpsertDuplicatePair(ctx, sqlcgen.UpsertDuplicatePairParams{
		A: aID, B: bID, Status: status,
		Note: optText(req.Note), Actor: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO UPDATE … WHERE status <> 'merged' skipped the row.
		c.JSON(http.StatusConflict, gin.H{"error": "pair already merged"})
		return sqlcgen.UpsertDuplicatePairRow{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.UpsertDuplicatePairRow{}, false
	}
	return row, true
}

// RequestDuplicatePairHandler godoc
// @Summary Request a merge of a duplicate pair
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body duplicatePairRequest true "Pair ids and note"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /persons/duplicates [post]
func RequestDuplicatePairHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonMergeRequest) {
		return
	}
	row, ok := upsertDuplicatePair(c, "requested")
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": row.ID, "status": row.Status}, "meta": gin.H{}})
}

// DismissDuplicatePairHandler godoc
// @Summary Dismiss a duplicate pair
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body duplicatePairRequest true "Pair ids and note"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /persons/duplicates/dismiss [post]
func DismissDuplicatePairHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonMerge) {
		return
	}
	row, ok := upsertDuplicatePair(c, "dismissed")
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": row.ID, "status": row.Status}, "meta": gin.H{}})
}
