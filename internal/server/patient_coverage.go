// internal/server/patient_coverage.go
// Patient coverage (kepesertaan pasien), visit guarantors and guarantee
// letters (spec 2026-10-07-tariff-b1-billing-core §3.7–3.9, §6). Routes are
// wired in RegisterPayerRoutes (payer.go); kepesertaan is guarded by
// core.person.manage (input Pendaftaran), visit guarantors/GL by
// operations.visit.manage OR billing.invoice.create.
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// coverageInput is the shared create/update payload of a patient coverage.
type coverageInput struct {
	PayerID      string `json:"payer_id" binding:"required"`
	PlanID       string `json:"plan_id"`
	MemberNumber string `json:"member_number" binding:"required"`
	BpjsClass    string `json:"bpjs_class"`
	ValidFrom    string `json:"valid_from"`
	ValidTo      string `json:"valid_to"`
	TpaAnnual    string `json:"tpa_remaining_annual"`
	TpaAsOf      string `json:"tpa_remaining_as_of"`
}

// coverageFields holds the validated DB-shaped form of a coverageInput.
type coverageFields struct {
	payer     sqlcgen.CorePayer
	planID    pgtype.UUID
	member    string
	bpjsClass pgtype.Text
	validFrom pgtype.Date
	validTo   pgtype.Date
	tpaAnnual pgtype.Numeric
	tpaAsOf   pgtype.Date
}

// loadCoveragePayer loads a live payer of the active merchant (404 otherwise);
// an inactive payer is rejected with 400.
func loadCoveragePayer(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.CorePayer, bool) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return sqlcgen.CorePayer{}, false
	}
	payerID, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payer_id"})
		return sqlcgen.CorePayer{}, false
	}
	payer, err := q.GetPayerByID(c.Request.Context(), payerID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && payer.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "payer not found"})
		return sqlcgen.CorePayer{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePayer{}, false
	}
	if !payer.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payer is not active"})
		return sqlcgen.CorePayer{}, false
	}
	return payer, true
}

// resolveCoveragePlan validates an optional plan reference: it must belong to
// the same payer (400 "plan does not belong to payer" otherwise).
func resolveCoveragePlan(c *gin.Context, q *sqlcgen.Queries, payer sqlcgen.CorePayer, raw string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if raw == "" {
		return id, true
	}
	planID, ok := parseUUID(raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
		return id, false
	}
	plan, err := q.GetPayerPlanByID(c.Request.Context(), planID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && plan.PayerID != payer.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plan does not belong to payer"})
		return id, false
	}
	if err != nil {
		respondInternalError(c, err)
		return id, false
	}
	return planID, true
}

// resolveCoverageFields validates a coverageInput against its payer: bpjs
// class only on BPJS payers, optional dates (valid_to >= valid_from) and the
// paired TPA remaining fields.
func resolveCoverageFields(c *gin.Context, q *sqlcgen.Queries, in coverageInput) (coverageFields, bool) {
	var f coverageFields
	payer, ok := loadCoveragePayer(c, q, in.PayerID)
	if !ok {
		return f, false
	}
	f.payer = payer
	planID, ok := resolveCoveragePlan(c, q, payer, in.PlanID)
	if !ok {
		return f, false
	}
	f.planID = planID
	f.member = in.MemberNumber
	if in.BpjsClass != "" {
		if payer.PayerType != "bpjs" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bpjs_class applies only to a BPJS payer"})
			return f, false
		}
		if in.BpjsClass != "1" && in.BpjsClass != "2" && in.BpjsClass != "3" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bpjs_class"})
			return f, false
		}
		f.bpjsClass = pgtype.Text{String: in.BpjsClass, Valid: true}
	}
	if in.ValidFrom != "" {
		d, err := optDate(in.ValidFrom)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_from"})
			return f, false
		}
		f.validFrom = d
	}
	if in.ValidTo != "" {
		d, err := optDate(in.ValidTo)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_to"})
			return f, false
		}
		f.validTo = d
	}
	if f.validTo.Valid && f.validFrom.Valid && f.validTo.Time.Before(f.validFrom.Time) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid coverage period"})
		return f, false
	}
	if (in.TpaAnnual == "") != (in.TpaAsOf == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tpa_remaining_annual and tpa_remaining_as_of must be set together"})
		return f, false
	}
	if in.TpaAnnual != "" {
		cents, err := parseCents(in.TpaAnnual)
		if err != nil || cents < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tpa_remaining_annual"})
			return f, false
		}
		f.tpaAnnual = centsToNumeric(cents)
		asOf, err := optDate(in.TpaAsOf)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tpa_remaining_as_of"})
			return f, false
		}
		f.tpaAsOf = asOf
	}
	return f, true
}

// loadPersonForCoverage resolves the person path param for the kepesertaan
// endpoints: merchant header + core.person.manage, live person of the caller's
// company; a merged person is rejected with the trigger's 409 message.
func loadPersonForCoverage(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.CorePerson, pgtype.UUID, bool) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	if !RequirePermissionForMerchant(c, PermPersonManage, merchantID) {
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	personID, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	person, err := q.GetPersonByID(c.Request.Context(), personID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	if person.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	if person.MergedIntoPersonID.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "person has been merged"})
		return sqlcgen.CorePerson{}, pgtype.UUID{}, false
	}
	return person, merchantID, true
}

// patientCoverageListJSON renders a joined coverage row (snake_case).
func patientCoverageListJSON(r sqlcgen.ListPatientCoveragesByPersonRow) gin.H {
	var planID, tpaAnnual, tpaAsOf any
	if r.PlanID.Valid {
		planID = r.PlanID.String()
	}
	if r.TpaRemainingAnnual.Valid {
		tpaAnnual = numStr2(r.TpaRemainingAnnual)
	}
	if r.TpaRemainingAsOf.Valid {
		tpaAsOf = dateOrNil(r.TpaRemainingAsOf)
	}
	return gin.H{
		"id":                   r.ID.String(),
		"payer":                gin.H{"id": r.PayerID.String(), "code": r.PayerCode, "name": r.PayerName, "payer_type": r.PayerType},
		"plan_id":              planID,
		"member_number":        r.MemberNumber,
		"bpjs_class":           textJSON(r.BpjsClass),
		"valid_from":           dateOrNil(r.ValidFrom),
		"valid_to":             dateOrNil(r.ValidTo),
		"tpa_remaining_annual": tpaAnnual,
		"tpa_remaining_as_of":  tpaAsOf,
		"is_active":            r.IsActive,
		"row_version":          r.RowVersion,
	}
}

// patientCoverageDetailJSON is patientCoverageListJSON for the single-row
// query type (identical column list, distinct generated struct).
func patientCoverageDetailJSON(r sqlcgen.GetPatientCoverageDetailRow) gin.H {
	return patientCoverageListJSON(sqlcgen.ListPatientCoveragesByPersonRow{
		ID: r.ID, CompanyID: r.CompanyID, MerchantID: r.MerchantID, PersonID: r.PersonID,
		PayerID: r.PayerID, PlanID: r.PlanID, MemberNumber: r.MemberNumber, BpjsClass: r.BpjsClass,
		ValidFrom: r.ValidFrom, ValidTo: r.ValidTo,
		TpaRemainingAnnual: r.TpaRemainingAnnual, TpaRemainingAsOf: r.TpaRemainingAsOf,
		IsActive: r.IsActive, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy,
		UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy, DeletedAt: r.DeletedAt, DeletedBy: r.DeletedBy,
		RowVersion: r.RowVersion,
		PayerCode:  r.PayerCode, PayerName: r.PayerName, PayerType: r.PayerType,
	})
}

// ListPatientCoveragesHandler godoc
// @Summary List the coverage memberships (kepesertaan) of a person
// @Tags patient-coverage
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id}/coverages [get]
func ListPatientCoveragesHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	person, _, ok := loadPersonForCoverage(c, q, c.Param("id"))
	if !ok {
		return
	}
	rows, err := q.ListPatientCoveragesByPerson(c.Request.Context(), person.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, patientCoverageListJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// CreatePatientCoverageHandler godoc
// @Summary Register a coverage membership (kepesertaan) for a person
// @Tags patient-coverage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param request body coverageInput true "Coverage data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /persons/{id}/coverages [post]
func CreatePatientCoverageHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	person, merchantID, ok := loadPersonForCoverage(c, q, c.Param("id"))
	if !ok {
		return
	}
	var req coverageInput
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	f, ok := resolveCoverageFields(c, q, req)
	if !ok {
		return
	}
	cov, err := q.CreatePatientCoverage(c.Request.Context(), sqlcgen.CreatePatientCoverageParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: person.ID,
		PayerID: f.payer.ID, PlanID: f.planID, MemberNumber: f.member, BpjsClass: f.bpjsClass,
		ValidFrom: f.validFrom, ValidTo: f.validTo,
		TpaRemainingAnnual: f.tpaAnnual, TpaRemainingAsOf: f.tpaAsOf, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "member number already registered"})
			return
		}
		if strings.Contains(err.Error(), "person has been merged") {
			c.JSON(http.StatusConflict, gin.H{"error": "person has been merged"})
			return
		}
		respondInternalError(c, err)
		return
	}
	detail, err := q.GetPatientCoverageDetail(c.Request.Context(), cov.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": patientCoverageDetailJSON(detail), "meta": gin.H{}})
}

type updatePatientCoverageRequest struct {
	PayerID      *string `json:"payer_id"`
	PlanID       *string `json:"plan_id"`
	MemberNumber *string `json:"member_number"`
	BpjsClass    *string `json:"bpjs_class"`
	ValidFrom    *string `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	TpaAnnual    *string `json:"tpa_remaining_annual"`
	TpaAsOf      *string `json:"tpa_remaining_as_of"`
	IsActive     *bool   `json:"is_active"`
	RowVersion   *int64  `json:"row_version" binding:"required"`
}

// optTextStr is a pgtype.Text as an optional JSON string ("").
func optTextStr(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// optDateStr is a pgtype.Date as an optional JSON date string ("").
func optDateStr(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

// UpdatePatientCoverageHandler godoc
// @Summary Update a patient coverage (optimistic locking)
// @Tags patient-coverage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Patient coverage UUID"
// @Param request body updatePatientCoverageRequest true "Coverage data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /patient-coverages/{id} [patch]
func UpdatePatientCoverageHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermPersonManage, merchantID) {
		return
	}
	covID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	cov, err := q.GetPatientCoverageByID(c.Request.Context(), covID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "patient coverage not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var req updatePatientCoverageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion == nil || *req.RowVersion != int64(cov.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "row version mismatch"})
		return
	}
	// Effective payload: current values overlaid with the provided fields.
	in := coverageInput{
		PayerID:      cov.PayerID.String(),
		PlanID:       uuidStrOrEmpty(cov.PlanID),
		MemberNumber: cov.MemberNumber,
		BpjsClass:    optTextStr(cov.BpjsClass),
		ValidFrom:    optDateStr(cov.ValidFrom),
		ValidTo:      optDateStr(cov.ValidTo),
	}
	if cov.TpaRemainingAnnual.Valid {
		in.TpaAnnual = numStr2(cov.TpaRemainingAnnual)
		in.TpaAsOf = optDateStr(cov.TpaRemainingAsOf)
	}
	if req.PayerID != nil {
		in.PayerID = *req.PayerID
	}
	if req.PlanID != nil {
		in.PlanID = *req.PlanID
	}
	if req.MemberNumber != nil {
		in.MemberNumber = *req.MemberNumber
	}
	if req.BpjsClass != nil {
		in.BpjsClass = *req.BpjsClass
	}
	if req.ValidFrom != nil {
		in.ValidFrom = *req.ValidFrom
	}
	if req.ValidTo != nil {
		in.ValidTo = *req.ValidTo
	}
	if req.TpaAnnual != nil {
		in.TpaAnnual = *req.TpaAnnual
	}
	if req.TpaAsOf != nil {
		in.TpaAsOf = *req.TpaAsOf
	}
	f, ok := resolveCoverageFields(c, q, in)
	if !ok {
		return
	}
	isActive := cov.IsActive
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	updated, err := q.UpdatePatientCoverage(c.Request.Context(), sqlcgen.UpdatePatientCoverageParams{
		ID: cov.ID, PayerID: f.payer.ID, PlanID: f.planID, MemberNumber: f.member, BpjsClass: f.bpjsClass,
		ValidFrom: f.validFrom, ValidTo: f.validTo,
		TpaRemainingAnnual: f.tpaAnnual, TpaRemainingAsOf: f.tpaAsOf,
		IsActive: isActive, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "member number already registered"})
			return
		}
		respondInternalError(c, err)
		return
	}
	detail, err := q.GetPatientCoverageDetail(c.Request.Context(), updated.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": patientCoverageDetailJSON(detail), "meta": gin.H{}})
}

// uuidStrOrEmpty renders an optional UUID as a string ("" when NULL).
func uuidStrOrEmpty(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

// loadAdmissionForBilling resolves the admission path param for the guarantor/
// GL endpoints: merchant header + visit/invoice permission, live admission,
// not cancelled, billing account not finalized (contract §6).
func loadAdmissionForBilling(c *gin.Context, rawID string, q *sqlcgen.Queries) (sqlcgen.OperationsAdmission, pgtype.UUID, bool) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if !RequireAnyPermissionForMerchant(c, merchantID, PermVisitManage, permInvoiceCreate) {
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	admissionID, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	adm, err := q.GetAdmissionByID(c.Request.Context(), admissionID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if adm.Status == "cancelled" {
		c.JSON(http.StatusConflict, gin.H{"error": "admission is cancelled"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	acct, err := q.GetBillingAccountByAdmission(c.Request.Context(), adm.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	if err == nil && acct.Status == "finalized" {
		c.JSON(http.StatusConflict, gin.H{"error": "billing is finalized"})
		return sqlcgen.OperationsAdmission{}, pgtype.UUID{}, false
	}
	return adm, merchantID, true
}

type guarantorInput struct {
	PayerID       string `json:"payer_id" binding:"required"`
	CoverageID    string `json:"coverage_id"`
	PlanID        string `json:"plan_id"`
	PolicyNumber  string `json:"policy_number"`
	GuarantorName string `json:"guarantor_name"`
}

// guarantorRowsJSON renders admission_guarantor rows in sequence order.
func guarantorRowsJSON(rows []sqlcgen.OperationsAdmissionGuarantor) []gin.H {
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"payer_id":       r.PayerID.String(),
			"coverage_id":    uuidJSON(r.CoverageID),
			"plan_id":        uuidJSON(r.PlanID),
			"policy_number":  textJSON(r.PolicyNumber),
			"guarantor_name": textJSON(r.GuarantorName),
			"sequence":       r.Sequence,
		})
	}
	return data
}

// PutAdmissionGuarantorsHandler godoc
// @Summary Replace the guarantors of a visit (BPJS is moved to sequence 1)
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body putGuarantorsRequest true "Guarantor list"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /admissions/{id}/guarantors [put]
func PutAdmissionGuarantorsHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, merchantID, ok := loadAdmissionForBilling(c, c.Param("id"), q)
	if !ok {
		return
	}
	var req struct {
		Guarantors *[]guarantorInput `json:"guarantors" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchant, err := q.GetMerchantByID(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	type gItem struct {
		in         guarantorInput
		payer      sqlcgen.CorePayer
		coverageID pgtype.UUID
		planID     pgtype.UUID
	}
	items := make([]gItem, 0, len(*req.Guarantors))
	seen := map[string]bool{}
	for _, g := range *req.Guarantors {
		payer, ok := loadCoveragePayer(c, q, g.PayerID)
		if !ok {
			return
		}
		if seen[g.PayerID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate payer"})
			return
		}
		seen[g.PayerID] = true
		if payer.PayerType == "bpjs" && merchant.BpjsCooperation == "none" {
			c.JSON(http.StatusConflict, gin.H{"error": "merchant has no BPJS cooperation"})
			return
		}
		var coverageID pgtype.UUID
		if g.CoverageID != "" {
			covID, okID := parseUUID(g.CoverageID)
			if !okID {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid coverage_id"})
				return
			}
			cov, err := q.GetPatientCoverageByID(c.Request.Context(), covID)
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "coverage does not match"})
				return
			}
			if err != nil {
				respondInternalError(c, err)
				return
			}
			// The coverage must belong to the visit's person and this payer.
			if cov.PersonID != adm.PersonID || cov.PayerID != payer.ID {
				c.JSON(http.StatusBadRequest, gin.H{"error": "coverage does not match"})
				return
			}
			coverageID = covID
		}
		planID, okPlan := resolveCoveragePlan(c, q, payer, g.PlanID)
		if !okPlan {
			return
		}
		items = append(items, gItem{in: g, payer: payer, coverageID: coverageID, planID: planID})
	}
	// Order = array order with BPJS payers moved to sequence 1 (stable).
	ordered := make([]gItem, 0, len(items))
	for _, it := range items {
		if it.payer.PayerType == "bpjs" {
			ordered = append(ordered, it)
		}
	}
	for _, it := range items {
		if it.payer.PayerType != "bpjs" {
			ordered = append(ordered, it)
		}
	}
	if err := q.DeleteAdmissionGuarantors(c.Request.Context(), adm.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for i, it := range ordered {
		if err := q.InsertAdmissionGuarantor(c.Request.Context(), sqlcgen.InsertAdmissionGuarantorParams{
			AdmissionID: adm.ID, PayerID: it.payer.ID, CoverageID: it.coverageID, PlanID: it.planID,
			PolicyNumber:  pgtype.Text{String: it.in.PolicyNumber, Valid: it.in.PolicyNumber != ""},
			GuarantorName: pgtype.Text{String: it.in.GuarantorName, Valid: it.in.GuarantorName != ""},
			Sequence:      int16(i + 1), CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	var primary pgtype.UUID
	if len(ordered) > 0 {
		primary = ordered[0].payer.ID
	}
	if err := q.SetAdmissionPrimaryPayer(c.Request.Context(), sqlcgen.SetAdmissionPrimaryPayerParams{
		ID: adm.ID, PrimaryPayerID: primary, UpdatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListAdmissionGuarantors(c.Request.Context(), adm.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"guarantors": guarantorRowsJSON(rows)}, "meta": gin.H{}})
}

// putGuarantorsRequest is only referenced by the swagger annotation above.
type putGuarantorsRequest struct {
	Guarantors []guarantorInput `json:"guarantors"`
}

type guaranteeLetterInput struct {
	PayerID     string `json:"payer_id" binding:"required"`
	GLNumber    string `json:"gl_number" binding:"required"`
	AmountLimit string `json:"amount_limit" binding:"required"`
	ValidFrom   string `json:"valid_from" binding:"required"`
	ValidTo     string `json:"valid_to"`
	Note        string `json:"note"`
}

// guaranteeLettersJSON renders live guarantee_letter rows.
func guaranteeLettersJSON(rows []sqlcgen.OperationsGuaranteeLetter) []gin.H {
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		var validTo, note any
		if r.ValidTo.Valid {
			validTo = dateOrNil(r.ValidTo)
		}
		if r.Note.Valid {
			note = r.Note.String
		}
		data = append(data, gin.H{
			"id":           r.ID.String(),
			"payer_id":     r.PayerID.String(),
			"gl_number":    r.GlNumber,
			"amount_limit": numStr2(r.AmountLimit),
			"valid_from":   dateOrNil(r.ValidFrom),
			"valid_to":     validTo,
			"note":         note,
			"row_version":  r.RowVersion,
		})
	}
	return data
}

// PutAdmissionGuaranteeLettersHandler godoc
// @Summary Replace the guarantee letters (surat jaminan) of a visit
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body putGuaranteeLettersRequest true "Guarantee letter list"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /admissions/{id}/guarantee-letters [put]
func PutAdmissionGuaranteeLettersHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	adm, merchantID, ok := loadAdmissionForBilling(c, c.Param("id"), q)
	if !ok {
		return
	}
	var req struct {
		Letters *[]guaranteeLetterInput `json:"letters" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	guarantors, err := q.ListAdmissionGuarantors(c.Request.Context(), adm.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	guarantorPayers := map[string]bool{}
	for _, g := range guarantors {
		guarantorPayers[g.PayerID.String()] = true
	}
	type lItem struct {
		payer sqlcgen.CorePayer
		in    guaranteeLetterInput
		limit pgtype.Numeric
		from  pgtype.Date
		to    pgtype.Date
	}
	items := make([]lItem, 0, len(*req.Letters))
	seen := map[string]bool{}
	for _, l := range *req.Letters {
		payer, ok := loadCoveragePayer(c, q, l.PayerID)
		if !ok {
			return
		}
		if seen[l.PayerID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate payer"})
			return
		}
		seen[l.PayerID] = true
		// The payer must guarantee this visit.
		if !guarantorPayers[payer.ID.String()] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "payer is not a guarantor of this visit"})
			return
		}
		cents, perr := parseCents(l.AmountLimit)
		if perr != nil || cents <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount_limit"})
			return
		}
		from, ferr := optDate(l.ValidFrom)
		if ferr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_from"})
			return
		}
		var to pgtype.Date
		if l.ValidTo != "" {
			to, ferr = optDate(l.ValidTo)
			if ferr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_to"})
				return
			}
			if to.Time.Before(from.Time) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid valid_to"})
				return
			}
		}
		items = append(items, lItem{payer: payer, in: l, limit: centsToNumeric(cents), from: from, to: to})
	}
	// Full-set replace: the previous letters are soft-deleted, the new set
	// inserted in the given order.
	if err := q.SoftDeleteGuaranteeLetters(c.Request.Context(), sqlcgen.SoftDeleteGuaranteeLettersParams{
		AdmissionID: adm.ID, DeletedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, it := range items {
		if err := q.InsertGuaranteeLetter(c.Request.Context(), sqlcgen.InsertGuaranteeLetterParams{
			CompanyID: adm.CompanyID, MerchantID: merchantID, AdmissionID: adm.ID, PayerID: it.payer.ID,
			GlNumber: it.in.GLNumber, AmountLimit: it.limit, ValidFrom: it.from,
			ValidTo: it.to, Note: pgtype.Text{String: it.in.Note, Valid: it.in.Note != ""},
			CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	rows, err := q.ListGuaranteeLetters(c.Request.Context(), adm.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"letters": guaranteeLettersJSON(rows)}, "meta": gin.H{}})
}

// putGuaranteeLettersRequest is only referenced by the swagger annotation above.
type putGuaranteeLettersRequest struct {
	Letters []guaranteeLetterInput `json:"letters"`
}
