// internal/server/person_family.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterPersonFamilyRoutes wires core.person_family CRUD (migration 000028,
// spec §4.2). Company-scoped like person.go: list/create are keyed off
// AuthCompanyID and the :id routes verify the row's company before writing.
func RegisterPersonFamilyRoutes(rg *gin.RouterGroup) {
	rg.GET("/persons/:id/families", ListPersonFamiliesHandler)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	birthDate, err := optDate(req.BirthDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	var req personFamilyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	birthDate, err := optDate(req.BirthDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person family not found"})
		return
	}
	if err := q.SoftDeletePersonFamily(c.Request.Context(), sqlcgen.SoftDeletePersonFamilyParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
