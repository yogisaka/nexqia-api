// internal/server/audit_export.go
// CSV export of the audit trail (core.access_log, core.audit_log), see
// docs/design/specs/2026-09-29-audit-log-ui-design.md §4.5.
package server

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// auditExportTooManyRows is the 400 a caller gets when the filtered result is
// wider than cfg.AuditExportMaxRows.
const auditExportTooManyRows = "too many rows, narrow the filter"

// ExportAccessLogsHandler godoc
// @Summary Export access-log rows as CSV
// @Description Streams the same rows as GET /audit/access-logs (same filters,
// @Description newest first) as UTF-8 CSV with a BOM so Excel opens it
// @Description directly. Formula-like cells are neutralized with a leading
// @Description apostrophe. Capped at AUDIT_EXPORT_MAX_ROWS rows — a wider
// @Description result is refused with 400.
// @Tags audit
// @Produce text/csv
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param actor_id query string false "Filter by actor UUID"
// @Param person_id query string false "Filter by patient UUID"
// @Param resource query string false "Filter by resource name (e.g. person)"
// @Param resource_id query string false "Filter by resource UUID"
// @Param action query string false "Filter by action name (e.g. view)"
// @Success 200 {file} file
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/access-logs/export [get]
func ExportAccessLogsHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermAuditLogView) {
			return
		}
		from, to, actorID, resourceID, personID, ok := parseAccessLogFilters(c)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		// One extra row detects an over-cap result without a separate count.
		rows, err := q.ListAccessLogs(c.Request.Context(), sqlcgen.ListAccessLogsParams{
			CompanyID:  AuthCompanyID(c),
			FromTime:   pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:     pgtype.Timestamptz{Time: to, Valid: true},
			ActorID:    actorID,
			Resource:   optText(c.Query("resource")),
			ResourceID: resourceID,
			PersonID:   personID,
			Action:     optText(c.Query("action")),
			RowOffset:  0,
			RowLimit:   int32(cfg.AuditExportMaxRows) + 1,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if len(rows) > cfg.AuditExportMaxRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": auditExportTooManyRows})
			return
		}
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"audit-akses-%s.csv\"",
			time.Now().In(cfg.AuditLocation).Format("20060102-1504")))
		writeAccessCSV(c, cfg.AuditLocation, rows)
	}
}

// writeAccessCSV streams the BOM + CSV body: Waktu, User, Admin platform,
// Aksi, Data, Pasien, No. RM, Hasil, IP.
func writeAccessCSV(c *gin.Context, loc *time.Location, rows []sqlcgen.ListAccessLogsRow) {
	_, _ = c.Writer.WriteString("\xEF\xBB\xBF") // UTF-8 BOM so Excel autodetects
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Waktu", "User", "Admin platform", "Aksi", "Data", "Pasien", "No. RM", "Hasil", "IP"})
	for _, r := range rows {
		ip := ""
		if r.IpAddress != nil {
			ip = r.IpAddress.String()
		}
		_ = w.Write([]string{
			csvSafe(auditTime(r.CreatedAt, loc)),
			csvSafe(auditActor(r.ActorName, r.ActorID)),
			csvSafe(auditAdminYes(r.PlatformAdminID)),
			csvSafe(r.Action),
			csvSafe(r.Resource),
			csvSafe(auditText(r.PatientName)),
			csvSafe(auditText(r.PatientMrn)),
			csvSafe(strconv.Itoa(int(r.StatusCode))),
			csvSafe(ip),
		})
	}
	w.Flush()
}

// ExportChangeLogsHandler godoc
// @Summary Export change-log rows as CSV
// @Description Streams the same rows as GET /audit/change-logs (same filters,
// @Description newest first) as UTF-8 CSV with a BOM so Excel opens it
// @Description directly. Formula-like cells are neutralized with a leading
// @Description apostrophe. Capped at AUDIT_EXPORT_MAX_ROWS rows — a wider
// @Description result is refused with 400.
// @Tags audit
// @Produce text/csv
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param table_name query string false "Filter by table name (e.g. core.person)"
// @Param record_id query string false "Filter by record UUID"
// @Param person_id query string false "Filter by patient UUID"
// @Param changed_by query string false "Filter by changed_by user UUID"
// @Param action query string false "Filter by action name (e.g. insert)"
// @Success 200 {file} file
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/change-logs/export [get]
func ExportChangeLogsHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermAuditLogView) {
			return
		}
		from, to, recordID, changedBy, personID, ok := parseChangeLogFilters(c)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		// One extra row detects an over-cap result without a separate count.
		rows, err := q.ListChangeLogs(c.Request.Context(), sqlcgen.ListChangeLogsParams{
			CompanyID: AuthCompanyID(c),
			FromTime:  pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:    pgtype.Timestamptz{Time: to, Valid: true},
			TableName: optText(c.Query("table_name")),
			RecordID:  recordID,
			PersonID:  personID,
			ChangedBy: changedBy,
			Action:    optText(c.Query("action")),
			RowOffset: 0,
			RowLimit:  int32(cfg.AuditExportMaxRows) + 1,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if len(rows) > cfg.AuditExportMaxRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": auditExportTooManyRows})
			return
		}
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"audit-perubahan-%s.csv\"",
			time.Now().In(cfg.AuditLocation).Format("20060102-1504")))
		writeChangeCSV(c, cfg.AuditLocation, rows)
	}
}

// writeChangeCSV streams the BOM + CSV body: Waktu, User, Admin platform,
// Aksi, Data, Pasien, No. RM, Field berubah.
func writeChangeCSV(c *gin.Context, loc *time.Location, rows []sqlcgen.ListChangeLogsRow) {
	_, _ = c.Writer.WriteString("\xEF\xBB\xBF") // UTF-8 BOM so Excel autodetects
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Waktu", "User", "Admin platform", "Aksi", "Data", "Pasien", "No. RM", "Field berubah"})
	for _, r := range rows {
		user := auditActor(r.ChangedByName, r.ChangedBy)
		if user == "" {
			// No changed_by: the change was made by the database itself.
			user = "Sistem"
		}
		_ = w.Write([]string{
			csvSafe(auditTime(r.ChangedAt, loc)),
			csvSafe(user),
			csvSafe(auditAdminYes(r.PlatformAdminID)),
			csvSafe(r.Action),
			csvSafe(r.TableName),
			csvSafe(auditText(r.PatientName)),
			csvSafe(auditText(r.PatientMrn)),
			csvSafe(strings.Join(r.ChangedFields, ", ")),
		})
	}
	w.Flush()
}

// csvSafe neutralizes spreadsheet formula injection (OWASP CSV injection): a
// cell that Excel would interpret as a formula gets a leading apostrophe.
// Applied to every text cell of the exports.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// auditTime renders a timestamp as "YYYY-MM-DD HH:MM:SS" in loc (spec §4.5).
func auditTime(ts pgtype.Timestamptz, loc *time.Location) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.In(loc).Format("2006-01-02 15:04:05")
}

// auditText renders an optional text column as a plain CSV cell value.
func auditText(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// auditActor prefers the resolved person name; without one it falls back to
// the raw actor UUID so exported rows stay attributable (spec §4.5).
func auditActor(name pgtype.Text, id pgtype.UUID) string {
	if name.Valid {
		return name.String
	}
	if id.Valid {
		return id.String()
	}
	return ""
}

// auditAdminYes marks rows performed by an impersonating platform admin.
func auditAdminYes(id pgtype.UUID) string {
	if id.Valid {
		return "ya"
	}
	return ""
}
