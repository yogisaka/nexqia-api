// internal/server/person_family.go
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterPersonFamilyRoutes wires core.person_family CRUD (migration 000028,
// spec §4.2). Company-scoped like person.go: list/create are keyed off
// AuthCompanyID and the :id routes verify the row's company before writing.
func RegisterPersonFamilyRoutes(rg *gin.RouterGroup) {
	rg.GET("/persons/:id/families", AccessLog("person_family", "list", "id"), ListPersonFamiliesHandler)
	rg.POST("/persons/:id/families", CreatePersonFamilyHandler)
	rg.PATCH("/person-families/:id", UpdatePersonFamilyHandler)
	rg.DELETE("/person-families/:id", DeletePersonFamilyHandler)
}

type personFamilyRequest struct {
	FullName               string `json:"full_name" binding:"required"`
	Phone                  string `json:"phone"`
	Email                  string `json:"email"`
	BirthDate              string `json:"birth_date"`
	BirthPlace             string `json:"birth_place"`
	Gender                 string `json:"gender"`
	Address                string `json:"address"`
	RegionVillageConceptID string `json:"region_village_concept_id"`
	PostalCode             string `json:"postal_code"`
	RelationshipConceptID  string `json:"relationship_concept_id"`
	PositionConceptID      string `json:"position_concept_id"`
	JobConceptID           string `json:"job_concept_id"`
	IsResponsiblePerson    bool   `json:"is_responsible_person"`
}

// familyMerchantLocation resolves the caller's active merchant timezone
// (X-Merchant-ID header) for the birth_date future check — contract "zona
// merchant" (spec 2026-10-07-form-validation-stage2 §4.2). Missing or invalid
// header falls back to merchantTZ's default (Asia/Jakarta) like every other
// merchant-day computation.
func familyMerchantLocation(c *gin.Context) *time.Location {
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	loc, err := merchantLocation(c.Request.Context(), sqlcgen.New(TxFromContext(c)), merchantID)
	if err != nil {
		loc, _ = time.LoadLocation(defaultMerchantTimezone)
	}
	return loc
}

// validatePersonFamilyRules runs the stage-2 per-field rules (spec
// 2026-10-07-form-validation-stage2 §4.2) on a bound family payload.
// birthDate must already be parsed (optDate) — only non-empty dates are
// checked against today in the merchant's timezone (loc).
func validatePersonFamilyRules(req personFamilyRequest, birthDate pgtype.Date, loc *time.Location) map[string]string {
	fields := map[string]string{}
	if req.RelationshipConceptID == "" {
		fields["relationship_concept_id"] = "Hubungan keluarga wajib dipilih"
	}
	if req.IsResponsiblePerson && strings.TrimSpace(req.Phone) == "" {
		fields["phone"] = "Nomor HP penanggung jawab wajib diisi"
	} else if req.Phone != "" && !validPhoneID(req.Phone) {
		fields["phone"] = "Nomor HP tidak valid (contoh 081234567890)"
	}
	if req.Email != "" && !validEmail(req.Email) {
		fields["email"] = "Email tidak valid"
	}
	if birthDate.Valid {
		// Compare calendar dates: optDate parses to UTC midnight, so "today"
		// is the merchant-local calendar date rebuilt as a UTC midnight
		// instant (the merchant-local midnight instant itself would be up to
		// 9h behind and wrongly reject birth dates of today).
		y, m, d := time.Now().In(loc).Date()
		today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		if birthDate.Time.After(today) {
			fields["birth_date"] = "Tanggal lahir tidak boleh di masa depan"
		}
	}
	if req.Gender != "" && req.Gender != "male" && req.Gender != "female" {
		fields["gender"] = "Jenis kelamin tidak valid"
	}
	return fields
}

// ListPersonFamiliesHandler godoc
// @Summary List family members of a person
// @Tags person-family
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Router /persons/{id}/families [get]
func ListPersonFamiliesHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	families, err := q.ListPersonFamiliesByPerson(c.Request.Context(), personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": families, "meta": gin.H{}})
}

// CreatePersonFamilyHandler godoc
// @Summary Add a family member to a person
// @Tags person-family
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param request body personFamilyRequest true "Family member data"
// @Success 201 {object} apiResponse
// @Router /persons/{id}/families [post]
func CreatePersonFamilyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req personFamilyRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	birthDate, err := optDate(req.BirthDate)
	if err != nil {
		fieldErr(c, "invalid birth_date, expected YYYY-MM-DD", "birth_date", "Format tanggal YYYY-MM-DD")
		return
	}
	if fields := validatePersonFamilyRules(req, birthDate, familyMerchantLocation(c)); len(fields) > 0 {
		respondFieldErrors(c, "validation_failed", fields)
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	family, err := q.CreatePersonFamily(c.Request.Context(), sqlcgen.CreatePersonFamilyParams{
		CompanyID:              AuthCompanyID(c),
		PersonID:               personID,
		FullName:               req.FullName,
		Phone:                  optText(req.Phone),
		Email:                  optText(req.Email),
		BirthDate:              birthDate,
		BirthPlace:             optText(req.BirthPlace),
		Gender:                 optText(req.Gender),
		Address:                optText(req.Address),
		RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
		PostalCode:             optText(req.PostalCode),
		RelationshipConceptID:  optUUID(req.RelationshipConceptID),
		PositionConceptID:      optUUID(req.PositionConceptID),
		JobConceptID:           optUUID(req.JobConceptID),
		IsResponsiblePerson:    req.IsResponsiblePerson,
		CreatedBy:              AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": family, "meta": gin.H{}})
}

// UpdatePersonFamilyHandler godoc
// @Summary Update a family member
// @Tags person-family
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person family UUID"
// @Param request body personFamilyRequest true "Family member data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /person-families/{id} [patch]
func UpdatePersonFamilyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPersonFamilyByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	var req personFamilyRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	birthDate, err := optDate(req.BirthDate)
	if err != nil {
		fieldErr(c, "invalid birth_date, expected YYYY-MM-DD", "birth_date", "Format tanggal YYYY-MM-DD")
		return
	}
	if fields := validatePersonFamilyRules(req, birthDate, familyMerchantLocation(c)); len(fields) > 0 {
		respondFieldErrors(c, "validation_failed", fields)
		return
	}
	family, err := q.UpdatePersonFamily(c.Request.Context(), sqlcgen.UpdatePersonFamilyParams{
		ID:                     id,
		FullName:               req.FullName,
		Phone:                  optText(req.Phone),
		Email:                  optText(req.Email),
		BirthDate:              birthDate,
		BirthPlace:             optText(req.BirthPlace),
		Gender:                 optText(req.Gender),
		Address:                optText(req.Address),
		RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
		PostalCode:             optText(req.PostalCode),
		RelationshipConceptID:  optUUID(req.RelationshipConceptID),
		PositionConceptID:      optUUID(req.PositionConceptID),
		JobConceptID:           optUUID(req.JobConceptID),
		IsResponsiblePerson:    req.IsResponsiblePerson,
		UpdatedBy:              AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": family, "meta": gin.H{}})
}

// DeletePersonFamilyHandler godoc
// @Summary Soft-delete a family member
// @Tags person-family
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person family UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /person-families/{id} [delete]
func DeletePersonFamilyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPersonFamilyByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	if err := q.SoftDeletePersonFamily(c.Request.Context(), sqlcgen.SoftDeletePersonFamilyParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
