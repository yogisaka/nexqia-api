// internal/server/person.go
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermPersonManage guards all core.person CRUD — single manage permission,
// matches the RBAC convention already used by tenancy.go/rbac.go (no separate
// read/write split).
const PermPersonManage = "core.person.manage"

// RegisterPersonRoutes wires core.person CRUD (docs/07-core-ddl.md §4 "Person master").
// NIK is encrypted at rest via pgcrypto (pgp_sym_encrypt, key = NIK_ENCRYPTION_KEY —
// see .env.example) — never stored or logged as plaintext. To keep NIK out of list/search
// responses (PII minimization), only GetPersonHandler decrypts and returns it; list/search
// responses omit the field entirely.
func RegisterPersonRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.POST("/persons", CreatePersonHandler(cfg))
	rg.GET("/persons", AccessLog("person", "list", ""), ListPersonsHandler)
	rg.GET("/persons/search", AccessLog("person", "search", ""), SearchPersonsHandler(cfg))
	rg.GET("/persons/:id", AccessLog("person", "view", "id"), GetPersonHandler(cfg))
	rg.PATCH("/persons/:id", UpdatePersonHandler(cfg))
	rg.DELETE("/persons/:id", DeletePersonHandler)
}

type personRequest struct {
	NIK                    string `json:"nik"`
	MedicalRecordNo        string `json:"medical_record_no"`
	FullName               string `json:"full_name" binding:"required"`
	BirthDate              string `json:"birth_date"`
	BirthPlace             string `json:"birth_place"`
	Gender                 string `json:"gender" binding:"required"`
	BloodType              string `json:"blood_type"`
	MaritalStatus          string `json:"marital_status"`
	ReligionConceptID      string `json:"religion_concept_id"`
	EducationConceptID     string `json:"education_concept_id"`
	EthnicityConceptID     string `json:"ethnicity_concept_id"`
	RegionVillageConceptID string `json:"region_village_concept_id"`
	CitizenshipConceptID   string `json:"citizenship_concept_id"`
	IdentityTypeConceptID  string `json:"identity_type_concept_id"`
	JobConceptID           string `json:"job_concept_id"`
	MaritalStatusConceptID string `json:"marital_status_concept_id"`
	BloodTypeConceptID     string `json:"blood_type_concept_id"`
	Phone                  string `json:"phone"`
	Email                  string `json:"email"`
	Address                string `json:"address"`
	FamilyID               string `json:"family_id"`
}

func optText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func optUUID(s string) pgtype.UUID {
	id, ok := parseUUID(s)
	if !ok {
		return pgtype.UUID{}
	}
	return id
}

func optDate(s string) (pgtype.Date, error) {
	if s == "" {
		return pgtype.Date{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return pgtype.Date{}, err
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

// encryptNIK returns the pgcrypto ciphertext (base64) for a plaintext NIK, or an
// invalid pgtype.Text if plaintext is empty (NIK is optional).
func encryptNIK(ctx context.Context, tx pgx.Tx, plaintext, key string) (pgtype.Text, error) {
	if plaintext == "" {
		return pgtype.Text{}, nil
	}
	var ciphertext string
	err := tx.QueryRow(ctx, `SELECT encode(pgp_sym_encrypt($1::text, $2::text), 'base64')`, plaintext, key).Scan(&ciphertext)
	if err != nil {
		return pgtype.Text{}, err
	}
	return pgtype.Text{String: ciphertext, Valid: true}, nil
}

// hashNIK returns the hex HMAC-SHA256 of a plaintext NIK (pgcrypto hmac) for
// exact-match search via core.person.nik_search_hash. Empty plaintext yields "",
// which callers pass as a NULL pgtype.Text so the search never matches on it.
func hashNIK(ctx context.Context, tx pgx.Tx, plaintext, key string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	var hash string
	err := tx.QueryRow(ctx, `SELECT encode(hmac($1::bytea, $2::bytea, 'sha256'), 'hex')`, plaintext, key).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// decryptNIK reverses encryptNIK for a stored ciphertext. Only call this on a
// single-record detail response, never in a list/search loop.
func decryptNIK(ctx context.Context, tx pgx.Tx, ciphertext pgtype.Text, key string) (string, error) {
	if !ciphertext.Valid {
		return "", nil
	}
	var plaintext string
	err := tx.QueryRow(ctx, `SELECT pgp_sym_decrypt(decode($1, 'base64'), $2::text)`, ciphertext.String, key).Scan(&plaintext)
	return plaintext, err
}

// CreatePersonHandler godoc
// @Summary Create a person
// @Description NIK is encrypted at rest via pgcrypto — echoed back plaintext in this response only.
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body personRequest true "Person data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /persons [post]
func CreatePersonHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonManage) {
			return
		}
		var req personRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		birthDate, err := optDate(req.BirthDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
			return
		}
		tx := TxFromContext(c)
		nik, err := encryptNIK(c.Request.Context(), tx, req.NIK, cfg.NIKEncryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hash, err := hashNIK(c.Request.Context(), tx, req.NIK, cfg.NIKSearchKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		q := sqlcgen.New(tx)
		mrn := req.MedicalRecordNo
		if mrn == "" {
			// Merchant is optional here — person creation isn't merchant-bound
			// (core.person is company-level). When present it's used to resolve
			// a per-merchant MRN format override; absent just falls back to the
			// company default.
			merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
			mrn, err = generateMedicalRecordNo(c.Request.Context(), q, AuthCompanyID(c), merchantID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		person, err := q.CreatePerson(c.Request.Context(), sqlcgen.CreatePersonParams{
			CompanyID:          AuthCompanyID(c),
			Nik:                nik,
			MedicalRecordNo:    optText(mrn),
			FullName:           req.FullName,
			BirthDate:          birthDate,
			BirthPlace:         optText(req.BirthPlace),
			Gender:             optText(req.Gender),
			BloodType:          optText(req.BloodType),
			MaritalStatus:      optText(req.MaritalStatus),
			ReligionConceptID:  optUUID(req.ReligionConceptID),
			EducationConceptID: optUUID(req.EducationConceptID),
			EthnicityConceptID: optUUID(req.EthnicityConceptID),
			RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
			CitizenshipConceptID:   optUUID(req.CitizenshipConceptID),
			IdentityTypeConceptID:  optUUID(req.IdentityTypeConceptID),
			JobConceptID:           optUUID(req.JobConceptID),
			MaritalStatusConceptID: optUUID(req.MaritalStatusConceptID),
			BloodTypeConceptID:     optUUID(req.BloodTypeConceptID),
			Phone:              optText(req.Phone),
			Email:              optText(req.Email),
			Address:            optText(req.Address),
			FamilyID:           optUUID(req.FamilyID),
			NikSearchHash:      optText(hash),
			CreatedBy:          AuthUserID(c),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		person.Nik = optText(req.NIK) // echo the plaintext just submitted, avoid a decrypt round-trip
		c.JSON(http.StatusCreated, gin.H{"data": person, "meta": gin.H{}})
	}
}

// ListPersonsHandler godoc
// @Summary List persons in the caller's company
// @Description NIK is omitted from list responses (PII minimization).
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /persons [get]
func ListPersonsHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	persons, err := q.ListPersonsByCompany(c.Request.Context(), sqlcgen.ListPersonsByCompanyParams{
		CompanyID: AuthCompanyID(c), Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range persons {
		persons[i].Nik = pgtype.Text{} // list responses omit NIK, see RegisterPersonRoutes doc
	}
	c.JSON(http.StatusOK, gin.H{"data": persons, "meta": gin.H{"limit": limit, "offset": offset}})
}

// SearchPersonsHandler godoc
// @Summary Search persons by name
// @Description Searches name/MRN/phone plus exact NIK match via HMAC-SHA256 hash.
// @Description NIK is omitted from search responses (PII minimization).
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param q query string true "Name search query"
// @Param limit query int false "Page size"
// @Success 200 {object} apiResponse
// @Router /persons/search [get]
func SearchPersonsHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonManage) {
			return
		}
		query := c.Query("q")
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing query param q"})
			return
		}
		limit, _ := paginationParams(c)
		tx := TxFromContext(c)
		hash, err := hashNIK(c.Request.Context(), tx, query, cfg.NIKSearchKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		q := sqlcgen.New(tx)
		persons, err := q.SearchPersonsByName(c.Request.Context(), sqlcgen.SearchPersonsByNameParams{
			CompanyID: AuthCompanyID(c), Column2: pgtype.Text{String: query, Valid: true},
			NikSearchHash: optText(hash), Limit: limit,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for i := range persons {
			persons[i].Nik = pgtype.Text{}
		}
		c.JSON(http.StatusOK, gin.H{"data": persons, "meta": gin.H{}})
	}
}

// GetPersonHandler godoc
// @Summary Get a person by id
// @Description Decrypts and returns NIK — only this endpoint does, list/search omit it.
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id} [get]
func GetPersonHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonManage) {
			return
		}
		id, ok := parseUUID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		tx := TxFromContext(c)
		q := sqlcgen.New(tx)
		person, err := q.GetPersonByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if person.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		plainNIK, err := decryptNIK(c.Request.Context(), tx, person.Nik, cfg.NIKEncryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		person.Nik = optText(plainNIK)
		c.JSON(http.StatusOK, gin.H{"data": person, "meta": gin.H{}})
	}
}

// UpdatePersonHandler godoc
// @Summary Update a person
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param request body personRequest true "Person data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id} [patch]
func UpdatePersonHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonManage) {
			return
		}
		id, ok := parseUUID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		tx := TxFromContext(c)
		q := sqlcgen.New(tx)
		existing, err := q.GetPersonByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if existing.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		var req personRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		birthDate, err := optDate(req.BirthDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
			return
		}
		nik, err := encryptNIK(c.Request.Context(), tx, req.NIK, cfg.NIKEncryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hash, err := hashNIK(c.Request.Context(), tx, req.NIK, cfg.NIKSearchKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		person, err := q.UpdatePerson(c.Request.Context(), sqlcgen.UpdatePersonParams{
			ID:                 id,
			Nik:                nik,
			MedicalRecordNo:    optText(req.MedicalRecordNo),
			FullName:           req.FullName,
			BirthDate:          birthDate,
			BirthPlace:         optText(req.BirthPlace),
			Gender:             optText(req.Gender),
			BloodType:          optText(req.BloodType),
			MaritalStatus:      optText(req.MaritalStatus),
			ReligionConceptID:  optUUID(req.ReligionConceptID),
			EducationConceptID: optUUID(req.EducationConceptID),
			EthnicityConceptID: optUUID(req.EthnicityConceptID),
			RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
			CitizenshipConceptID:   optUUID(req.CitizenshipConceptID),
			IdentityTypeConceptID:  optUUID(req.IdentityTypeConceptID),
			JobConceptID:           optUUID(req.JobConceptID),
			MaritalStatusConceptID: optUUID(req.MaritalStatusConceptID),
			BloodTypeConceptID:     optUUID(req.BloodTypeConceptID),
			Phone:              optText(req.Phone),
			Email:              optText(req.Email),
			Address:            optText(req.Address),
			FamilyID:           optUUID(req.FamilyID),
			NikSearchHash:      optText(hash),
			UpdatedBy:          AuthUserID(c),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		person.Nik = optText(req.NIK)
		c.JSON(http.StatusOK, gin.H{"data": person, "meta": gin.H{}})
	}
}

// DeletePersonHandler godoc
// @Summary Soft-delete a person
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id} [delete]
func DeletePersonHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetPersonByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return
	}
	if err := q.SoftDeletePerson(c.Request.Context(), sqlcgen.SoftDeletePersonParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
