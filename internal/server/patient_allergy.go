// internal/server/patient_allergy.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterPatientAllergyRoutes wires core.patient_allergy CRUD (migration
// 000006). Company-scoped via AuthCompanyID; :id routes verify the row's
// company before writing. sqlc queries live in person.sql.
func RegisterPatientAllergyRoutes(rg *gin.RouterGroup) {
	rg.GET("/persons/:id/allergies", ListPatientAllergiesHandler)
	rg.POST("/persons/:id/allergies", CreatePatientAllergyHandler)
	rg.PATCH("/patient-allergies/:id", UpdatePatientAllergyHandler)
	rg.DELETE("/patient-allergies/:id", DeletePatientAllergyHandler)
}

type patientAllergyRequest struct {
	AllergyType   string `json:"allergy_type" binding:"required"`
	SubstanceName string `json:"substance_name" binding:"required"`
	Reaction      string `json:"reaction"`
	Severity      string `json:"severity"`
}

type updatePatientAllergyRequest struct {
	Reaction string `json:"reaction"`
	Severity string `json:"severity"`
}

// ListPatientAllergiesHandler godoc
// @Summary List active allergies of a person
// @Tags patient-allergy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Router /persons/{id}/allergies [get]
func ListPatientAllergiesHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	allergies, err := q.ListPatientAllergiesByPerson(c.Request.Context(), personID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": allergies, "meta": gin.H{}})
}

// CreatePatientAllergyHandler godoc
// @Summary Record an allergy for a person
// @Tags patient-allergy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param request body patientAllergyRequest true "Allergy data"
// @Success 201 {object} apiResponse
// @Router /persons/{id}/allergies [post]
func CreatePatientAllergyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req patientAllergyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	allergy, err := q.CreatePatientAllergy(c.Request.Context(), sqlcgen.CreatePatientAllergyParams{
		CompanyID:     AuthCompanyID(c),
		PersonID:      personID,
		AllergyType:   req.AllergyType,
		SubstanceName: req.SubstanceName,
		Reaction:      optText(req.Reaction),
		Severity:      optText(req.Severity),
		RecordedBy:    AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": allergy, "meta": gin.H{}})
}

// UpdatePatientAllergyHandler godoc
// @Summary Update an allergy record
// @Tags patient-allergy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Patient allergy UUID"
// @Param request body updatePatientAllergyRequest true "Allergy data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /patient-allergies/{id} [patch]
func UpdatePatientAllergyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPatientAllergyByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "allergy not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "allergy not found"})
		return
	}
	var req updatePatientAllergyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	allergy, err := q.UpdatePatientAllergy(c.Request.Context(), sqlcgen.UpdatePatientAllergyParams{
		ID: id, Reaction: optText(req.Reaction), Severity: optText(req.Severity), UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "allergy not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": allergy, "meta": gin.H{}})
}

// DeletePatientAllergyHandler godoc
// @Summary Deactivate an allergy record
// @Tags patient-allergy
// @Produce json
// @Security BearerAuth
// @Param id path string true "Patient allergy UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /patient-allergies/{id} [delete]
func DeletePatientAllergyHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPatientAllergyByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "allergy not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "allergy not found"})
		return
	}
	if err := q.DeactivatePatientAllergy(c.Request.Context(), sqlcgen.DeactivatePatientAllergyParams{ID: id, UpdatedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
