// internal/server/audit.go
// Read API for the audit trail (core.access_log, core.audit_log), see
// docs/design/specs/2026-09-29-audit-trail-design.md §5.
package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// auditMaxRangeDays caps how far back one list call may reach (spec §5).
const auditMaxRangeDays = 93

// RegisterAuditRoutes mounts the audit-trail read endpoints (spec §5).
func RegisterAuditRoutes(rg *gin.RouterGroup) {
	rg.GET("/audit/access-logs", ListAccessLogsHandler)
	rg.GET("/audit/change-logs", ListChangeLogsHandler)
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
// @Param resource query string false "Filter by resource name (e.g. person)"
// @Param resource_id query string false "Filter by resource UUID"
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
	from, to, ok := auditRange(c)
	if !ok {
		return
	}
	actorID, ok := auditUUIDFilter(c, "actor_id")
	if !ok {
		return
	}
	resourceID, ok := auditUUIDFilter(c, "resource_id")
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
			"action":            r.Action,
			"status_code":       r.StatusCode,
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
// @Param changed_by query string false "Filter by changed_by user UUID"
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
	from, to, ok := auditRange(c)
	if !ok {
		return
	}
	recordID, ok := auditUUIDFilter(c, "record_id")
	if !ok {
		return
	}
	changedBy, ok := auditUUIDFilter(c, "changed_by")
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
		ChangedBy: changedBy,
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
			"action":            r.Action,
			"changed_fields":    r.ChangedFields,
			"changed_by":        uuidOrNil(r.ChangedBy),
			"platform_admin_id": uuidOrNil(r.PlatformAdminID),
			"changed_at":        rfc3339OrNil(r.ChangedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
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
