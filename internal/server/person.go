// internal/server/person.go
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Static /persons/merge* routes must come before /persons/:id (see
	// registerPersonMergeRoutes).
	registerPersonMergeRoutes(rg, cfg)
	// Static /persons/duplicates* routes must also come before /persons/:id.
	registerPersonDuplicateRoutes(rg)
	rg.GET("/persons/:id", AccessLog("person", "view", "id"), GetPersonHandler(cfg))
	rg.PATCH("/persons/:id", UpdatePersonHandler(cfg))
	rg.DELETE("/persons/:id", DeletePersonHandler)
}

type personRequest struct {
	Profile                 string        `json:"profile"`
	NIKExemption            string        `json:"nik_exemption"`
	NIKExemptionNote        string        `json:"nik_exemption_note"`
	DuplicateOverrideReason string        `json:"duplicate_override_reason"`
	NIK                     string        `json:"nik"`
	MedicalRecordNo         string        `json:"medical_record_no"`
	FullName                string        `json:"full_name" binding:"required"`
	BirthDate               string        `json:"birth_date"`
	BirthPlace              string        `json:"birth_place"`
	Gender                  string        `json:"gender" binding:"required"`
	BloodType               string        `json:"blood_type"`
	MaritalStatus           string        `json:"marital_status"`
	ReligionConceptID       string        `json:"religion_concept_id"`
	EducationConceptID      string        `json:"education_concept_id"`
	EthnicityConceptID      string        `json:"ethnicity_concept_id"`
	RegionVillageConceptID  string        `json:"region_village_concept_id"`
	CitizenshipConceptID    string        `json:"citizenship_concept_id"`
	IdentityTypeConceptID   string        `json:"identity_type_concept_id"`
	JobConceptID            string        `json:"job_concept_id"`
	MaritalStatusConceptID  string        `json:"marital_status_concept_id"`
	BloodTypeConceptID      string        `json:"blood_type_concept_id"`
	Phone                   string        `json:"phone"`
	Email                   string        `json:"email"`
	Address                 string        `json:"address"`
	FamilyID                string        `json:"family_id"`
	Consent                 *consentInput `json:"consent"`
}

// consentInput is the data-processing consent block required on POST /persons
// (plan C Task 3); PATCH /persons stays consent-free for legacy patients.
type consentInput struct {
	Method           string  `json:"method"`
	TextVersion      string  `json:"text_version"`
	PersonDocumentID *string `json:"person_document_id"`
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

// uuidMustParse parses a compile-time constant UUID literal (panics on typo).
func uuidMustParse(s string) pgtype.UUID {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		panic("uuidMustParse: " + err.Error())
	}
	return u
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

// personProfile normalizes the submitted profile: empty defaults to "staff"
// (spec 2026-10-07-registration-validation §7.1); any value outside
// patient/staff returns "" so the caller responds 400 invalid profile.
func personProfile(s string) string {
	if s == "" {
		return "staff"
	}
	if s == "patient" || s == "staff" {
		return s
	}
	return ""
}

// setNIKExemption stores the NIK-exemption reason/note chosen at create/update
// time; empty values are stored as NULL (matches migration 000072 CHECKs).
func setNIKExemption(ctx context.Context, tx pgx.Tx, id pgtype.UUID, exemption, note string) error {
	_, err := tx.Exec(ctx,
		`UPDATE core.person SET nik_exemption = NULLIF(btrim($1), ''), nik_exemption_note = NULLIF(btrim($2), '') WHERE id = $3`,
		exemption, note, id)
	return err
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
		profile := personProfile(req.Profile)
		if profile == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid profile"})
			return
		}
		// Consent gating (plan C Task 3): rejected before any write so the
		// transaction leaves no rows behind.
		if req.Consent == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "consent wajib saat mendaftarkan pasien baru"})
			return
		}
		if !allowedConsentMethods[req.Consent.Method] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "metode persetujuan tidak dikenal"})
			return
		}
		fields, warnings := validatePerson(req, time.Now())
		if len(fields) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "validation_failed", "fields": fields})
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
			respondInternalError(c, err)
			return
		}
		hash, err := hashNIK(c.Request.Context(), tx, req.NIK, cfg.NIKSearchKey)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		q := sqlcgen.New(tx)
		// The submitted text_version must match the active consent_text (spec
		// §5.3) — still read-only, so a 400 here writes nothing.
		consentText, err := q.GetActiveConsentText(c.Request.Context(), "data_processing")
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && consentText.Version != req.Consent.TextVersion) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "versi teks persetujuan tidak dikenal"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		mrn := req.MedicalRecordNo
		if mrn == "" {
			// Merchant is optional here — person creation isn't merchant-bound
			// (core.person is company-level). When present it's used to resolve
			// a per-merchant MRN format override; absent just falls back to the
			// company default.
			mrn, err = generateMedicalRecordNo(c.Request.Context(), q, AuthCompanyID(c), merchantID)
			if err != nil {
				respondInternalError(c, err)
				return
			}
		}
		if hash != "" {
			// Spec §4.7: NIK uniqueness enforced per company via the deterministic
			// HMAC hash (unique index uq_person_nik_hash) — pre-check here to give
			// a 409 naming the colliding MRN instead of a bare constraint error.
			dup, err := q.FindPersonByNIKHash(c.Request.Context(), sqlcgen.FindPersonByNIKHashParams{
				CompanyID: AuthCompanyID(c), NikSearchHash: optText(hash),
				ExcludeID: uuidMustParse("00000000-0000-0000-0000-000000000000"),
			})
			if err == nil {
				c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("nik already registered to medical record %s", dup.MedicalRecordNo)})
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
		}
		// Spec §4/M6(a): duplicate gate only on CREATE with profile patient.
		var strongDuplicates []sqlcgen.CheckNewPatientDuplicatesRow
		if profile == "patient" && birthDate.Valid {
			dups, err := q.CheckNewPatientDuplicates(c.Request.Context(), sqlcgen.CheckNewPatientDuplicatesParams{
				CompanyID: AuthCompanyID(c), FullName: req.FullName, BirthDate: birthDate,
				Gender: optText(req.Gender), Phone: optText(req.Phone),
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			for _, d := range dups {
				if d.Score >= 70 {
					strongDuplicates = append(strongDuplicates, d)
				}
			}
			if len(strongDuplicates) > 0 && req.DuplicateOverrideReason == "" {
				candidates := make([]gin.H, 0, len(strongDuplicates))
				for _, d := range strongDuplicates {
					candidates = append(candidates, gin.H{
						"id":                d.ID,
						"medical_record_no": d.MedicalRecordNo,
						"full_name":         d.FullName,
						"birth_date":        d.BirthDate,
						"gender":            d.Gender,
						"score":             d.Score,
					})
				}
				c.JSON(http.StatusConflict, gin.H{"error": "possible_duplicate", "candidates": candidates})
				return
			}
		}
		person, err := q.CreatePerson(c.Request.Context(), sqlcgen.CreatePersonParams{
			CompanyID:              AuthCompanyID(c),
			Nik:                    nik,
			MedicalRecordNo:        optText(mrn),
			FullName:               req.FullName,
			BirthDate:              birthDate,
			BirthPlace:             optText(req.BirthPlace),
			Gender:                 optText(req.Gender),
			BloodType:              optText(req.BloodType),
			MaritalStatus:          optText(req.MaritalStatus),
			ReligionConceptID:      optUUID(req.ReligionConceptID),
			EducationConceptID:     optUUID(req.EducationConceptID),
			EthnicityConceptID:     optUUID(req.EthnicityConceptID),
			RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
			CitizenshipConceptID:   optUUID(req.CitizenshipConceptID),
			IdentityTypeConceptID:  optUUID(req.IdentityTypeConceptID),
			JobConceptID:           optUUID(req.JobConceptID),
			MaritalStatusConceptID: optUUID(req.MaritalStatusConceptID),
			BloodTypeConceptID:     optUUID(req.BloodTypeConceptID),
			Phone:                  optText(req.Phone),
			Email:                  optText(req.Email),
			Address:                optText(req.Address),
			FamilyID:               optUUID(req.FamilyID),
			NikSearchHash:          optText(hash),
			CreatedBy:              AuthUserID(c),
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_person_nik_hash" {
				c.JSON(http.StatusConflict, gin.H{"error": "nik already registered"})
				return
			}
			respondInternalError(c, err)
			return
		}
		person.Nik = optText(req.NIK) // echo the plaintext just submitted, avoid a decrypt round-trip
		var documentID pgtype.UUID
		if req.Consent.PersonDocumentID != nil {
			id, okDoc := parseUUID(*req.Consent.PersonDocumentID)
			if !okDoc {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_document_id"})
				return
			}
			documentID = id
		}
		// Same-transaction consent insert (plan C Task 3): granted_by = context
		// user, granted_at = now, merchant from context; a failure here rolls
		// the person insert back with it.
		if _, err := q.CreatePersonConsent(c.Request.Context(), sqlcgen.CreatePersonConsentParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: person.ID,
			ConsentType: "data_processing", TextVersion: req.Consent.TextVersion, Method: req.Consent.Method,
			PersonDocumentID: documentID, GrantedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		if err := setNIKExemption(c.Request.Context(), tx, person.ID, req.NIKExemption, req.NIKExemptionNote); err != nil {
			respondInternalError(c, err)
			return
		}
		// Override accepted: mark every strong candidate as a dismissed pair in
		// the same transaction (spec §4 M6(a)).
		for _, d := range strongDuplicates {
			if _, err := q.UpsertDuplicatePair(c.Request.Context(), sqlcgen.UpsertDuplicatePairParams{
				A: person.ID, B: d.ID, Status: "dismissed",
				Note: optText(req.DuplicateOverrideReason), Actor: AuthUserID(c),
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		consentRow, err := q.GetLatestPersonConsent(c.Request.Context(), person.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		loc, err := merchantLocation(c.Request.Context(), q, merchantID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": person, "consent": consentPayload(
			consentRow.ID, consentRow.PersonID, consentRow.ConsentType, consentRow.TextVersion,
			consentRow.Method, consentRow.PersonDocumentID, consentRow.GrantedAt, consentRow.GrantedBy,
			consentRow.GrantedByName, loc), "meta": gin.H{}, "warnings": warnings})
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
		respondInternalError(c, err)
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
			respondInternalError(c, err)
			return
		}
		q := sqlcgen.New(tx)
		persons, err := q.SearchPersonsByName(c.Request.Context(), sqlcgen.SearchPersonsByNameParams{
			CompanyID: AuthCompanyID(c), Column2: pgtype.Text{String: query, Valid: true},
			NikSearchHash: optText(hash), Limit: limit,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for i := range persons {
			persons[i].Nik = pgtype.Text{}
		}
		// Spec §8.6: surface the medical record numbers of merged (absorbed)
		// persons on the survivor so searches for the old MRN still resolve.
		ids := make([]pgtype.UUID, len(persons))
		for i := range persons {
			ids[i] = persons[i].ID
		}
		mergedRows, err := q.MergedMRNsBySurvivor(c.Request.Context(), ids)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		mergedBySurvivor := map[pgtype.UUID][]string{}
		for _, row := range mergedRows {
			mrn := ""
			if row.MedicalRecordNo.Valid {
				mrn = row.MedicalRecordNo.String
			}
			mergedBySurvivor[row.SurvivorID] = append(mergedBySurvivor[row.SurvivorID], mrn)
		}
		items := make([]personSearchItem, len(persons))
		for i := range persons {
			mrnList := mergedBySurvivor[persons[i].ID]
			if mrnList == nil {
				mrnList = []string{}
			}
			items[i] = personSearchItem{CorePerson: persons[i], MergedFromMRN: mrnList}
		}
		c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
	}
}

// personSearchItem wraps a CorePerson row with the MRNs of persons merged into
// it (spec §8.6) — all other fields keep the existing serialization shape.
type personSearchItem struct {
	sqlcgen.CorePerson
	MergedFromMRN []string `json:"merged_from_mrn"`
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
			// Spec §8.6: a merged person no longer resolves directly — point the
			// caller at the surviving record instead.
			if target, mErr := q.GetMergedPersonTarget(c.Request.Context(), id); mErr == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "person merged", "merged_into_person_id": target.String()})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if person.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		plainNIK, err := decryptNIK(c.Request.Context(), tx, person.Nik, cfg.NIKEncryptionKey)
		if err != nil {
			respondInternalError(c, err)
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
			respondInternalError(c, err)
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
		if personProfile(req.Profile) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid profile"})
			return
		}
		fields, warnings := validatePerson(req, time.Now())
		if len(fields) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "validation_failed", "fields": fields})
			return
		}
		birthDate, err := optDate(req.BirthDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date, expected YYYY-MM-DD"})
			return
		}
		nik, err := encryptNIK(c.Request.Context(), tx, req.NIK, cfg.NIKEncryptionKey)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		hash, err := hashNIK(c.Request.Context(), tx, req.NIK, cfg.NIKSearchKey)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if hash != "" {
			// Spec §4.7: block re-pointing this person's NIK at another live
			// record's NIK (exclude self so a no-op update passes).
			dup, err := q.FindPersonByNIKHash(c.Request.Context(), sqlcgen.FindPersonByNIKHashParams{
				CompanyID: AuthCompanyID(c), NikSearchHash: optText(hash), ExcludeID: id,
			})
			if err == nil {
				c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("nik already registered to medical record %s", dup.MedicalRecordNo)})
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				respondInternalError(c, err)
				return
			}
		}
		person, err := q.UpdatePerson(c.Request.Context(), sqlcgen.UpdatePersonParams{
			ID:                     id,
			Nik:                    nik,
			MedicalRecordNo:        optText(req.MedicalRecordNo),
			FullName:               req.FullName,
			BirthDate:              birthDate,
			BirthPlace:             optText(req.BirthPlace),
			Gender:                 optText(req.Gender),
			BloodType:              optText(req.BloodType),
			MaritalStatus:          optText(req.MaritalStatus),
			ReligionConceptID:      optUUID(req.ReligionConceptID),
			EducationConceptID:     optUUID(req.EducationConceptID),
			EthnicityConceptID:     optUUID(req.EthnicityConceptID),
			RegionVillageConceptID: optUUID(req.RegionVillageConceptID),
			CitizenshipConceptID:   optUUID(req.CitizenshipConceptID),
			IdentityTypeConceptID:  optUUID(req.IdentityTypeConceptID),
			JobConceptID:           optUUID(req.JobConceptID),
			MaritalStatusConceptID: optUUID(req.MaritalStatusConceptID),
			BloodTypeConceptID:     optUUID(req.BloodTypeConceptID),
			Phone:                  optText(req.Phone),
			Email:                  optText(req.Email),
			Address:                optText(req.Address),
			FamilyID:               optUUID(req.FamilyID),
			NikSearchHash:          optText(hash),
			UpdatedBy:              AuthUserID(c),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_person_nik_hash" {
				c.JSON(http.StatusConflict, gin.H{"error": "nik already registered"})
				return
			}
			respondInternalError(c, err)
			return
		}
		person.Nik = optText(req.NIK)
		if err := setNIKExemption(c.Request.Context(), tx, person.ID, req.NIKExemption, req.NIKExemptionNote); err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": person, "meta": gin.H{}, "warnings": warnings})
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
		respondInternalError(c, err)
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return
	}
	if err := q.SoftDeletePerson(c.Request.Context(), sqlcgen.SoftDeletePersonParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
