// internal/server/audit.go
// Read API for the audit trail (core.access_log, core.audit_log), see
// docs/design/specs/2026-09-29-audit-trail-design.md §5.
package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// auditMaxRangeDays caps how far back one list call may reach (spec §5).
const auditMaxRangeDays = 93

// RegisterAuditRoutes mounts the audit-trail read endpoints (spec §5) and the
// CSV exports (spec §4.5). Every route records one access_log row for itself
// (spec §4.4).
func RegisterAuditRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.GET("/audit/access-logs", AccessLog("audit", "list_access", ""), ListAccessLogsHandler)
	rg.GET("/audit/change-logs", AccessLog("audit", "list_change", ""), ListChangeLogsHandler)
	rg.GET("/audit/patients", AccessLog("audit", "search_patient", ""), SearchAuditPatientsHandler)
	rg.GET("/audit/users", AccessLog("audit", "search_user", ""), SearchAuditUsersHandler)
	rg.GET("/audit/access-logs/export", AccessLog("audit", "export_access", ""), ExportAccessLogsHandler(cfg))
	rg.GET("/audit/change-logs/export", AccessLog("audit", "export_change", ""), ExportChangeLogsHandler(cfg))
	rg.GET("/audit/review/summary", AccessLog("audit", "review_summary", ""), AccessReviewSummaryHandler(cfg))
	rg.POST("/audit/reviews", AccessLog("audit", "review_create", ""), CreateAccessReviewHandler(cfg))
	rg.GET("/audit/reviews", AccessLog("audit", "review_list", ""), ListAccessReviewsHandler)
}

// ListAccessLogsHandler godoc
// @Summary List access-log rows (who read which patient data)
// @Description Returns core.access_log rows for the caller's company, newest
// @Description first. Only rows the authenticated company owns are ever returned.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param actor_id query string false "Filter by actor UUID"
// @Param person_id query string false "Filter by patient UUID"
// @Param resource query string false "Filter by resource name (e.g. person)"
// @Param resource_id query string false "Filter by resource UUID"
// @Param action query string false "Filter by action name (e.g. view)"
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/access-logs [get]
func ListAccessLogsHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	from, to, actorID, resourceID, personID, ok := parseAccessLogFilters(c)
	if !ok {
		return
	}
	limit, offset := auditLimitOffset(c)
	q := sqlcgen.New(TxFromContext(c))
	// CompanyID is an explicit filter — RLS alone is bypassed by owner roles.
	rows, err := q.ListAccessLogs(c.Request.Context(), sqlcgen.ListAccessLogsParams{
		CompanyID:  AuthCompanyID(c),
		FromTime:   pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:     pgtype.Timestamptz{Time: to, Valid: true},
		ActorID:    actorID,
		Resource:   optText(c.Query("resource")),
		ResourceID: resourceID,
		PersonID:   personID,
		Action:     optText(c.Query("action")),
		RowOffset:  offset,
		RowLimit:   limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load logs"})
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		row := gin.H{
			"id":                uuidOrNil(r.ID),
			"merchant_id":       uuidOrNil(r.MerchantID),
			"actor_id":          uuidOrNil(r.ActorID),
			"platform_admin_id": uuidOrNil(r.PlatformAdminID),
			"resource":          r.Resource,
			"resource_id":       uuidOrNil(r.ResourceID),
			"person_id":         uuidOrNil(r.PersonID),
			"action":            r.Action,
			"status_code":       r.StatusCode,
			"actor_name":        textOrNil(r.ActorName),
			"patient_name":      textOrNil(r.PatientName),
			"patient_mrn":       textOrNil(r.PatientMrn),
			"ip_address":        nil,
			"created_at":        rfc3339OrNil(r.CreatedAt),
		}
		if r.IpAddress != nil {
			row["ip_address"] = r.IpAddress.String()
		}
		data = append(data, row)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// ListChangeLogsHandler godoc
// @Summary List change-log rows (who changed which patient-data fields)
// @Description Returns core.audit_log rows for the caller's company, newest
// @Description first. Field names only — old/new values are never stored or returned.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param table_name query string false "Filter by table name (e.g. core.person)"
// @Param record_id query string false "Filter by record UUID"
// @Param person_id query string false "Filter by patient UUID"
// @Param changed_by query string false "Filter by changed_by user UUID"
// @Param action query string false "Filter by action name (e.g. insert)"
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/change-logs [get]
func ListChangeLogsHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	from, to, recordID, changedBy, personID, ok := parseChangeLogFilters(c)
	if !ok {
		return
	}
	limit, offset := auditLimitOffset(c)
	q := sqlcgen.New(TxFromContext(c))
	// CompanyID is an explicit filter — RLS alone is bypassed by owner roles.
	rows, err := q.ListChangeLogs(c.Request.Context(), sqlcgen.ListChangeLogsParams{
		CompanyID: AuthCompanyID(c),
		FromTime:  pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:    pgtype.Timestamptz{Time: to, Valid: true},
		TableName: optText(c.Query("table_name")),
		RecordID:  recordID,
		PersonID:  personID,
		ChangedBy: changedBy,
		Action:    optText(c.Query("action")),
		RowOffset: offset,
		RowLimit:  limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load logs"})
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":                uuidOrNil(r.ID),
			"merchant_id":       uuidOrNil(r.MerchantID),
			"table_name":        r.TableName,
			"record_id":         uuidOrNil(r.RecordID),
			"person_id":         uuidOrNil(r.PersonID),
			"action":            r.Action,
			"changed_fields":    r.ChangedFields,
			"changed_by":        uuidOrNil(r.ChangedBy),
			"changed_by_name":   textOrNil(r.ChangedByName),
			"patient_name":      textOrNil(r.PatientName),
			"patient_mrn":       textOrNil(r.PatientMrn),
			"platform_admin_id": uuidOrNil(r.PlatformAdminID),
			"changed_at":        rfc3339OrNil(r.ChangedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// SearchAuditPatientsHandler godoc
// @Summary Search patients for the audit-log person filter
// @Description Finds patients of the caller's company by full-name substring
// @Description or exact medical-record number. Soft-deleted patients stay
// @Description searchable (flagged deleted) so old log rows stay readable.
// @Description Max 20 rows. Wildcards in q are matched literally.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param q query string true "Full-name substring or exact medical_record_no (2-100 characters)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/patients [get]
func SearchAuditPatientsHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	search, ok := auditSearchQuery(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.SearchAuditPatients(c.Request.Context(), sqlcgen.SearchAuditPatientsParams{
		CompanyID: AuthCompanyID(c),
		Pattern:   escapeLike(search),
		Q:         search,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search patients"})
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		var birthDate any
		if r.BirthDate.Valid {
			birthDate = r.BirthDate.Time.Format("2006-01-02")
		}
		data = append(data, gin.H{
			"id":                uuidOrNil(r.ID),
			"full_name":         r.FullName,
			"medical_record_no": textOrNil(r.MedicalRecordNo),
			"birth_date":        birthDate,
			"deleted":           r.Deleted,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// SearchAuditUsersHandler godoc
// @Summary Search users for the audit-log actor filter
// @Description Finds users of the caller's company by username or person
// @Description full-name substring, including soft-deleted ones (flagged
// @Description deleted). Max 20 rows. Wildcards in q are matched literally.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param q query string true "Username or full-name substring (2-100 characters)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/users [get]
func SearchAuditUsersHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	search, ok := auditSearchQuery(c)
	if !ok {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.SearchAuditUsers(c.Request.Context(), sqlcgen.SearchAuditUsersParams{
		CompanyID: AuthCompanyID(c),
		Pattern:   escapeLike(search),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search users"})
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":        uuidOrNil(r.ID),
			"username":  r.Username,
			"full_name": textOrNil(r.FullName),
			"active":    r.IsActive,
			"deleted":   r.Deleted,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// auditRange parses the from/to RFC3339 window: to defaults to now, from to
// to−7 days; a reversed window or one wider than auditMaxRangeDays is rejected
// with a 400.
func auditRange(c *gin.Context) (from, to time.Time, ok bool) {
	to = time.Now()
	if v := c.Query("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to timestamp"})
			return time.Time{}, time.Time{}, false
		}
		to = parsed
	}
	from = to.Add(-7 * 24 * time.Hour)
	if v := c.Query("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from timestamp"})
			return time.Time{}, time.Time{}, false
		}
		from = parsed
	}
	if !to.After(from) || to.Sub(from) > auditMaxRangeDays*24*time.Hour {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid time range"})
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// auditUUIDFilter parses an optional UUID query param; a malformed value is a 400.
func auditUUIDFilter(c *gin.Context, name string) (pgtype.UUID, bool) {
	v := c.Query(name)
	if v == "" {
		return pgtype.UUID{}, true
	}
	id, ok := parseUUID(v)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid UUID filter"})
		return pgtype.UUID{}, false
	}
	return id, true
}

// parseAccessLogFilters parses the query filters shared verbatim by the
// access-log list and its CSV export: the from/to window plus the actor,
// resource, resource-id and person UUID filters. A filtered patient is also
// recorded on the access_log row of the call itself. ok=false means a 400 was
// already written.
func parseAccessLogFilters(c *gin.Context) (from, to time.Time, actorID, resourceID, personID pgtype.UUID, ok bool) {
	from, to, ok = auditRange(c)
	if !ok {
		return
	}
	if actorID, ok = auditUUIDFilter(c, "actor_id"); !ok {
		return
	}
	if resourceID, ok = auditUUIDFilter(c, "resource_id"); !ok {
		return
	}
	if personID, ok = auditUUIDFilter(c, "person_id"); !ok {
		return
	}
	if personID.Valid {
		// The audit list call itself is logged against the filtered patient.
		setAccessLogPersonID(c, personID)
	}
	return from, to, actorID, resourceID, personID, true
}

// parseChangeLogFilters parses the query filters shared verbatim by the
// change-log list and its CSV export: the from/to window plus the table,
// record, changed-by and person UUID filters. ok=false means a 400 was
// already written.
func parseChangeLogFilters(c *gin.Context) (from, to time.Time, recordID, changedBy, personID pgtype.UUID, ok bool) {
	from, to, ok = auditRange(c)
	if !ok {
		return
	}
	if recordID, ok = auditUUIDFilter(c, "record_id"); !ok {
		return
	}
	if changedBy, ok = auditUUIDFilter(c, "changed_by"); !ok {
		return
	}
	if personID, ok = auditUUIDFilter(c, "person_id"); !ok {
		return
	}
	if personID.Valid {
		// The audit list call itself is logged against the filtered patient.
		setAccessLogPersonID(c, personID)
	}
	return from, to, recordID, changedBy, personID, true
}

// auditSearchQuery reads and validates the shared q search param: trimmed,
// 2-100 runes, otherwise a 400.
func auditSearchQuery(c *gin.Context) (string, bool) {
	q := strings.TrimSpace(c.Query("q"))
	if n := len([]rune(q)); n < 2 || n > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q must be 2-100 characters"})
		return "", false
	}
	return q, true
}

// auditLimitOffset parses limit/offset: limit defaults to 50 and clamps to 200,
// offset defaults to 0 and invalid or negative values fall back to the default.
func auditLimitOffset(c *gin.Context) (limit, offset int32) {
	limit, offset = 50, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		if v > 200 {
			v = 200
		}
		limit = int32(v)
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v >= 0 {
		offset = int32(v)
	}
	return limit, offset
}

// uuidOrNil renders a pgtype.UUID as string or nil (no repo helper existed).
func uuidOrNil(id pgtype.UUID) any {
	if !id.Valid {
		return nil
	}
	return id.String()
}

// rfc3339OrNil renders a timestamp as an RFC3339 string or nil.
func rfc3339OrNil(ts pgtype.Timestamptz) any {
	if !ts.Valid {
		return nil
	}
	return ts.Time.Format(time.RFC3339)
}
