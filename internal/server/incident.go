// internal/server/incident.go
// Patient safety incident core API (spec
// 2026-10-20-permenkes-6-2026-compliance-design §4 Fase B / §6): report-only
// incident reporting over quality.incident plus the append-only kronologi
// quality.incident_entry, data-driven NCC MERP A–I gradings and 10 incident
// type categories (overridable per merchant) — migration 000079. No DELETE:
// kronologi is append-only and incidents are never deleted in B1.
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Incident permissions seeded by migration 000079
// (quality.incident.view/manage/report).
const (
	PermQualityIncidentView   = "quality.incident.view"
	PermQualityIncidentManage = "quality.incident.manage"
	PermQualityIncidentReport = "quality.incident.report"
)

// RegisterIncidentRoutes wires the incident endpoints (spec §4 Fase B).
func RegisterIncidentRoutes(rg *gin.RouterGroup) {
	rg.GET("/incident-types", ListIncidentTypesHandler)
	rg.GET("/incident-gradings", ListIncidentGradingsHandler)
	rg.POST("/incidents", CreateIncidentHandler)
	rg.GET("/incidents", ListIncidentsHandler)
	rg.GET("/incidents/:id", AccessLog("incident", "view", "id"), GetIncidentHandler)
	rg.POST("/incidents/:id/entries", AddIncidentEntryHandler)
	rg.PATCH("/incidents/:id/status", UpdateIncidentStatusHandler)
}

// incidentNextStatus maps the target status to the required current one
// (spec §6 loop tutup: feedback always precedes closed; closed has no exit
// in B1 — pattern claimBatchNextStatus).
var incidentNextStatus = map[string]string{
	"feedback": "dilaporkan",
	"closed":   "feedback",
}

// incidentCatalog holds the per-request catalog lookups: effective (override
// aware) incident type names + active flags and the grading name/outcome maps.
type incidentCatalog struct {
	typeNames       map[string]string
	typeActive      map[string]bool
	gradingNames    map[string]string
	gradingOutcomes map[string]string
}

// loadIncidentCatalogs loads the catalogs once per request (no hard-coded
// enum in Go — Global constraints).
func loadIncidentCatalogs(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) (incidentCatalog, error) {
	types, err := q.ListEffectiveIncidentTypes(ctx, merchantID)
	if err != nil {
		return incidentCatalog{}, err
	}
	gradings, err := q.ListIncidentGradings(ctx)
	if err != nil {
		return incidentCatalog{}, err
	}
	cat := incidentCatalog{
		typeNames:       make(map[string]string, len(types)),
		typeActive:      make(map[string]bool, len(types)),
		gradingNames:    make(map[string]string, len(gradings)),
		gradingOutcomes: make(map[string]string, len(gradings)),
	}
	for _, t := range types {
		cat.typeNames[t.Code] = t.Name
		cat.typeActive[t.Code] = t.IsActive
	}
	for _, g := range gradings {
		cat.gradingNames[g.Code] = g.Name
		cat.gradingOutcomes[g.Code] = g.Outcome
	}
	return cat, nil
}

// fetchIncident loads an incident with the reporter name and applies the 404
// contract when it is missing or belongs to another merchant (isolation is
// enforced in the handler too — pattern fetchPersonDocument).
func fetchIncident(c *gin.Context, q *sqlcgen.Queries, id, merchantID pgtype.UUID) (sqlcgen.GetIncidentByIDRow, bool) {
	incident, err := q.GetIncidentByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && incident.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "insiden tidak ditemukan"})
		return incident, false
	}
	if err != nil {
		respondInternalError(c, err)
		return incident, false
	}
	return incident, true
}

// callerHasIncidentManage reports whether the caller holds
// quality.incident.manage at merchantID (pattern:
// effectiveRoleID + UserHasPermission in RequirePermissionForMerchant).
func callerHasIncidentManage(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) bool {
	roleID, err := effectiveRoleID(c, q, AuthUserID(c), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	if !roleID.Valid {
		return false
	}
	has, err := q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: PermQualityIncidentManage, RoleID: roleID,
	})
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	return has
}

// redactReporter hides the reporter identity on anonymous incidents from
// callers without quality.incident.manage (spec §6 anonimisasi opsional).
func redactReporter(payload gin.H, anonymous bool, hasManage bool) {
	if anonymous && !hasManage {
		payload["reported_by"] = nil
		payload["reported_by_name"] = nil
	}
}

// redactEntryAuthor hides the author of the `report` kronologi entry on
// anonymous incidents from callers without manage (the report row is the
// reporter's own writing); follow-up entries (comment/correction/feedback/
// status) stay visible — kronologi tindak lanjut is visible work.
func redactEntryAuthor(entry gin.H, incidentAnonymous bool, hasManage bool, kind string) {
	if incidentAnonymous && !hasManage && kind == "report" {
		entry["created_by_name"] = nil
	}
}

// incidentJSON maps incident fields to the snake_case response shape; type
// and grading names plus the grading outcome come from the catalogs loaded
// once per request. reportedByName is the LEFT JOIN person name (null when
// the user has no person row).
func incidentJSON(id, personID, admissionID, reportedBy pgtype.UUID,
	incidentType, grading, location, narrative, patientImpact string,
	occurredAt, createdAt, updatedAt pgtype.Timestamptz,
	reportedByName pgtype.Text, anonymous bool, status string, rowVersion int32,
	typeName, gradingName, gradingOutcome string) gin.H {
	return gin.H{
		"id":                 uuidJSON(id),
		"incident_type":      incidentType,
		"incident_type_name": typeName,
		"grading":            grading,
		"grading_name":       gradingName,
		"grading_outcome":    gradingOutcome,
		"occurred_at":        tsJSON(occurredAt),
		"location":           location,
		"narrative":          narrative,
		"patient_impact":     patientImpact,
		"person_id":          uuidJSON(personID),
		"admission_id":       uuidJSON(admissionID),
		"reported_by":        uuidJSON(reportedBy),
		"reported_by_name":   textJSON(reportedByName),
		"anonymous":          anonymous,
		"status":             status,
		"created_at":         tsJSON(createdAt),
		"updated_at":         tsJSON(updatedAt),
		"row_version":        rowVersion,
	}
}

// entryJSON maps one kronologi entry to the snake_case response shape.
func entryJSON(id, incidentID pgtype.UUID, kind, body string, createdByName pgtype.Text, createdAt pgtype.Timestamptz) gin.H {
	return gin.H{
		"id":              uuidJSON(id),
		"incident_id":     uuidJSON(incidentID),
		"kind":            kind,
		"body":            body,
		"created_by_name": textJSON(createdByName),
		"created_at":      tsJSON(createdAt),
	}
}

// CreateIncidentHandler godoc
// @Summary Report a patient safety incident (incident + report entry, one tx)
// @Tags incident
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createIncidentRequest true "Incident report"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /incidents [post]
func CreateIncidentHandler(c *gin.Context) {
	if !RequirePermission(c, PermQualityIncidentReport) {
		return
	}
	var req createIncidentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	occurredAt, err := time.Parse(time.RFC3339, req.OccurredAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "waktu kejadian wajib dan harus RFC3339"})
		return
	}
	if strings.TrimSpace(req.Location) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "lokasi wajib"})
		return
	}
	if strings.TrimSpace(req.Narrative) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "narasi wajib"})
		return
	}
	if strings.TrimSpace(req.PatientImpact) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dampak pasien wajib"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	cat, err := loadIncidentCatalogs(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !cat.typeActive[req.IncidentType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jenis insiden tidak dikenal"})
		return
	}
	if _, known := cat.gradingNames[req.Grading]; !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": "grading tidak dikenal"})
		return
	}
	var personID, admissionID pgtype.UUID
	if req.PersonID != nil && *req.PersonID != "" {
		id, ok := parseUUID(*req.PersonID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		if _, err := q.PersonBelongsToCompany(ctx, sqlcgen.PersonBelongsToCompanyParams{
			ID: id, CompanyID: AuthCompanyID(c),
		}); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pasien tidak ditemukan"})
			return
		} else if err != nil {
			respondInternalError(c, err)
			return
		}
		personID = id
	}
	if req.AdmissionID != nil && *req.AdmissionID != "" {
		id, ok := parseUUID(*req.AdmissionID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		if _, err := q.AdmissionBelongsToMerchant(ctx, sqlcgen.AdmissionBelongsToMerchantParams{
			ID: id, MerchantID: merchantID,
		}); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kunjungan tidak ditemukan"})
			return
		} else if err != nil {
			respondInternalError(c, err)
			return
		}
		admissionID = id
	}
	// One ambient request tx: incident row + the `report` kronologi entry are
	// written together (RunRequestTx rolls both back on any ≥400 response).
	incident, err := q.CreateIncident(ctx, sqlcgen.CreateIncidentParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		IncidentType: req.IncidentType, Grading: req.Grading,
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		Location:   req.Location, Narrative: req.Narrative, PatientImpact: req.PatientImpact,
		PersonID: personID, AdmissionID: admissionID,
		ReportedBy: AuthUserID(c), Anonymous: req.Anonymous,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if _, err := q.CreateIncidentEntry(ctx, sqlcgen.CreateIncidentEntryParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, IncidentID: incident.ID,
		Kind: "report", Body: req.Narrative, CreatedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	payload := incidentJSON(
		incident.ID, incident.PersonID, incident.AdmissionID, incident.ReportedBy,
		incident.IncidentType, incident.Grading, incident.Location, incident.Narrative, incident.PatientImpact,
		incident.OccurredAt, incident.CreatedAt, incident.UpdatedAt,
		pgtype.Text{}, incident.Anonymous, incident.Status, incident.RowVersion,
		cat.typeNames[incident.IncidentType], cat.gradingNames[incident.Grading], cat.gradingOutcomes[incident.Grading])
	c.JSON(http.StatusCreated, gin.H{"incident": payload})
}

type createIncidentRequest struct {
	IncidentType  string  `json:"incident_type"`
	Grading       string  `json:"grading"`
	OccurredAt    string  `json:"occurred_at"`
	Location      string  `json:"location"`
	Narrative     string  `json:"narrative"`
	PatientImpact string  `json:"patient_impact"`
	PersonID      *string `json:"person_id"`
	AdmissionID   *string `json:"admission_id"`
	Anonymous     bool    `json:"anonymous"`
}

// ListIncidentsHandler godoc
// @Summary List patient safety incidents of the active merchant (newest first)
// @Tags incident
// @Produce json
// @Security BearerAuth
// @Param status query string false "dilaporkan|feedback|closed"
// @Param incident_type query string false "Incident type code"
// @Param grading query string false "Grading code"
// @Success 200 {object} apiResponse
// @Router /incidents [get]
func ListIncidentsHandler(c *gin.Context) {
	if !RequirePermission(c, PermQualityIncidentView) {
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	cat, err := loadIncidentCatalogs(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListIncidents(ctx, sqlcgen.ListIncidentsParams{
		Column1: c.Query("status"), Column2: c.Query("incident_type"), Column3: c.Query("grading"),
		MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	hasManage := callerHasIncidentManage(c, q, merchantID)
	items := make([]gin.H, len(rows))
	for i, r := range rows {
		payload := incidentJSON(
			r.ID, r.PersonID, r.AdmissionID, r.ReportedBy,
			r.IncidentType, r.Grading, r.Location, r.Narrative, r.PatientImpact,
			r.OccurredAt, r.CreatedAt, r.UpdatedAt,
			r.ReportedByName, r.Anonymous, r.Status, r.RowVersion,
			cat.typeNames[r.IncidentType], cat.gradingNames[r.Grading], cat.gradingOutcomes[r.Grading])
		redactReporter(payload, r.Anonymous, hasManage)
		items[i] = payload
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetIncidentHandler godoc
// @Summary Get one incident with its append-only kronologi
// @Tags incident
// @Produce json
// @Security BearerAuth
// @Param id path string true "Incident UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /incidents/{id} [get]
func GetIncidentHandler(c *gin.Context) {
	if !RequirePermission(c, PermQualityIncidentView) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	incident, ok := fetchIncident(c, q, id, merchantID)
	if !ok {
		return
	}
	cat, err := loadIncidentCatalogs(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	hasManage := callerHasIncidentManage(c, q, merchantID)
	payload := incidentJSON(
		incident.ID, incident.PersonID, incident.AdmissionID, incident.ReportedBy,
		incident.IncidentType, incident.Grading, incident.Location, incident.Narrative, incident.PatientImpact,
		incident.OccurredAt, incident.CreatedAt, incident.UpdatedAt,
		incident.ReportedByName, incident.Anonymous, incident.Status, incident.RowVersion,
		cat.typeNames[incident.IncidentType], cat.gradingNames[incident.Grading], cat.gradingOutcomes[incident.Grading])
	redactReporter(payload, incident.Anonymous, hasManage)
	entries, err := q.ListIncidentEntries(ctx, incident.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	entryItems := make([]gin.H, len(entries))
	for i, e := range entries {
		item := entryJSON(e.ID, e.IncidentID, e.Kind, e.Body, e.CreatedByName, e.CreatedAt)
		redactEntryAuthor(item, incident.Anonymous, hasManage, e.Kind)
		entryItems[i] = item
	}
	c.JSON(http.StatusOK, gin.H{"incident": payload, "entries": entryItems})
}

type updateIncidentStatusRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// UpdateIncidentStatusHandler godoc
// @Summary Advance the incident status (dilaporkan → feedback → closed)
// @Description closed requires the feedback loop first; feedback requires a note (spec §6 loop tutup).
// @Tags incident
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Incident UUID"
// @Param request body updateIncidentStatusRequest true "Target status + optional note"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /incidents/{id}/status [patch]
func UpdateIncidentStatusHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	incident, ok := fetchIncident(c, q, id, merchantID)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermQualityIncidentManage, incident.MerchantID) {
		return
	}
	var req updateIncidentStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	required, ok := incidentNextStatus[req.Status]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if incident.Status != required {
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition"})
		return
	}
	if req.Status == "feedback" && strings.TrimSpace(req.Note) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "catatan umpan balik wajib"})
		return
	}
	// One ambient request tx: the feedback/status kronologi entry (when there
	// is a note) and the status UPDATE are written together.
	if strings.TrimSpace(req.Note) != "" {
		kind := "status"
		if req.Status == "feedback" {
			kind = "feedback"
		}
		if _, err := q.CreateIncidentEntry(ctx, sqlcgen.CreateIncidentEntryParams{
			CompanyID: incident.CompanyID, MerchantID: incident.MerchantID, IncidentID: incident.ID,
			Kind: kind, Body: req.Note, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	updated, err := q.UpdateIncidentStatus(ctx, sqlcgen.UpdateIncidentStatusParams{
		ID: incident.ID, Status: req.Status,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// reported_by_name diambil via LEFT JOIN person seperti GET detail
	// (UpdateIncidentStatus RETURNING * tidak membawa nama).
	refreshed, ok := fetchIncident(c, q, updated.ID, merchantID)
	if !ok {
		return
	}
	cat, err := loadIncidentCatalogs(ctx, q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	payload := incidentJSON(
		refreshed.ID, refreshed.PersonID, refreshed.AdmissionID, refreshed.ReportedBy,
		refreshed.IncidentType, refreshed.Grading, refreshed.Location, refreshed.Narrative, refreshed.PatientImpact,
		refreshed.OccurredAt, refreshed.CreatedAt, refreshed.UpdatedAt,
		refreshed.ReportedByName, refreshed.Anonymous, refreshed.Status, refreshed.RowVersion,
		cat.typeNames[refreshed.IncidentType], cat.gradingNames[refreshed.Grading], cat.gradingOutcomes[refreshed.Grading])
	c.JSON(http.StatusOK, gin.H{"incident": payload})
}

type addIncidentEntryRequest struct {
	Kind string `json:"kind"`
	Body string `json:"body"`
}

// AddIncidentEntryHandler godoc
// @Summary Append a comment/correction to the incident kronologi (append-only)
// @Description kind is limited to comment|correction — feedback/status entries are written only by PATCH /status.
// @Tags incident
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Incident UUID"
// @Param request body addIncidentEntryRequest true "Kronologi entry"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /incidents/{id}/entries [post]
func AddIncidentEntryHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	incident, ok := fetchIncident(c, q, id, merchantID)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermQualityIncidentManage, incident.MerchantID) {
		return
	}
	var req addIncidentEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Kind != "comment" && req.Kind != "correction" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jenis entri tidak dikenal"})
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "catatan wajib"})
		return
	}
	entry, err := q.CreateIncidentEntry(c.Request.Context(), sqlcgen.CreateIncidentEntryParams{
		CompanyID: incident.CompanyID, MerchantID: incident.MerchantID, IncidentID: incident.ID,
		Kind: req.Kind, Body: req.Body, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// created_by_name diambil via LEFT JOIN app_user→person seperti GET detail
	// (CreateIncidentEntry RETURNING * tidak membawa nama).
	entries, err := q.ListIncidentEntries(c.Request.Context(), incident.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var createdByName pgtype.Text
	for _, e := range entries {
		if e.ID == entry.ID {
			createdByName = e.CreatedByName
			break
		}
	}
	c.JSON(http.StatusCreated, gin.H{"entry": entryJSON(
		entry.ID, entry.IncidentID, entry.Kind, entry.Body, createdByName, entry.CreatedAt,
	)})
}

// ListIncidentTypesHandler godoc
// @Summary List the effective incident type catalog for the active merchant
// @Description Override-aware; only active categories are returned (data-driven — no hard-coded enum).
// @Tags incident
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /incident-types [get]
func ListIncidentTypesHandler(c *gin.Context) {
	if !RequirePermission(c, PermQualityIncidentView) {
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListEffectiveIncidentTypes(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		if !r.IsActive {
			continue
		}
		items = append(items, gin.H{"code": r.Code, "name": r.Name, "is_active": true})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// ListIncidentGradingsHandler godoc
// @Summary List the incident grading catalog (NCC MERP A–I with ICPS outcomes)
// @Tags incident
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /incident-gradings [get]
func ListIncidentGradingsHandler(c *gin.Context) {
	if !RequirePermission(c, PermQualityIncidentView) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListIncidentGradings(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, len(rows))
	for i, r := range rows {
		items[i] = gin.H{"code": r.Code, "name": r.Name, "outcome": r.Outcome, "description": r.Description, "sort_order": r.SortOrder}
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}
