// internal/server/audit_role_change.go
// Read API for the role change history (core.role_change_log), see
// 2026-10-08-role-change-history-design.md §3.
package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// ListRoleChangeAuditHandler godoc
// @Summary List role change history rows
// @Description Returns core.role_change_log rows for the caller's company,
// @Description newest first, with the current role name and deletion flag.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param role_id query string false "Filter by role UUID"
// @Param changed_by query string false "Filter by changed_by user UUID"
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/role-changes [get]
func ListRoleChangeAuditHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	from, to, roleID, changedBy, ok := parseRoleChangeFilters(c)
	if !ok {
		return
	}
	limit, offset := auditLimitOffset(c)
	q := sqlcgen.New(TxFromContext(c))
	// CompanyID is an explicit filter — RLS alone is bypassed by owner roles.
	rows, err := q.ListRoleChangeAudit(c.Request.Context(), sqlcgen.ListRoleChangeAuditParams{
		CompanyID: AuthCompanyID(c),
		FromTime:  pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:    pgtype.Timestamptz{Time: to, Valid: true},
		RoleID:    roleID,
		ChangedBy: changedBy,
		RowOffset: offset,
		RowLimit:  limit,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":              uuidOrNil(r.ID),
			"role_id":         uuidOrNil(r.RoleID),
			"role_name":       r.RoleName,
			"role_deleted":    r.RoleDeleted,
			"changed_by":      uuidOrNil(r.ChangedBy),
			"changed_by_name": textOrNil(r.ChangedByName),
			"changed_at":      rfc3339OrNil(r.ChangedAt),
			"reason":          r.Reason,
			"changes":         json.RawMessage(r.Changes),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// ExportRoleChangeAuditHandler godoc
// @Summary Export role change history rows as CSV
// @Description Streams the same rows as GET /audit/role-changes (same filters,
// @Description newest first) as UTF-8 CSV with a BOM so Excel opens it
// @Description directly. Formula-like cells are neutralized with a leading
// @Description apostrophe. Capped at AUDIT_EXPORT_MAX_ROWS rows — a wider
// @Description result is refused with 400.
// @Tags audit
// @Produce text/csv
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Param role_id query string false "Filter by role UUID"
// @Param changed_by query string false "Filter by changed_by user UUID"
// @Success 200 {file} file
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/role-changes/export [get]
func ExportRoleChangeAuditHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermAuditLogView) {
			return
		}
		from, to, roleID, changedBy, ok := parseRoleChangeFilters(c)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		// One extra row detects an over-cap result without a separate count.
		rows, err := q.ListRoleChangeAudit(c.Request.Context(), sqlcgen.ListRoleChangeAuditParams{
			CompanyID: AuthCompanyID(c),
			FromTime:  pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:    pgtype.Timestamptz{Time: to, Valid: true},
			RoleID:    roleID,
			ChangedBy: changedBy,
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
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"audit-perubahan-role-%s.csv\"",
			time.Now().In(cfg.AuditLocation).Format("20060102-1504")))
		writeRoleChangeCSV(c, cfg.AuditLocation, rows)
	}
}

// writeRoleChangeCSV streams the BOM + CSV body: Waktu, User, Role, Alasan,
// Perubahan (spec §3.2).
func writeRoleChangeCSV(c *gin.Context, loc *time.Location, rows []sqlcgen.ListRoleChangeAuditRow) {
	_, _ = c.Writer.WriteString("\xEF\xBB\xBF") // UTF-8 BOM so Excel autodetects
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"Waktu", "User", "Role", "Alasan", "Perubahan"})
	for _, r := range rows {
		user := auditActor(r.ChangedByName, r.ChangedBy)
		if user == "" {
			// No changed_by name or id resolved: attribute the row to the system.
			user = "Sistem"
		}
		role := r.RoleName
		if r.RoleDeleted {
			role += " (dihapus)"
		}
		_ = w.Write([]string{
			csvSafe(auditTime(r.ChangedAt, loc)),
			csvSafe(user),
			csvSafe(role),
			csvSafe(r.Reason),
			csvSafe(formatRoleChangesText(r.Changes)),
		})
	}
	w.Flush()
}

// roleMetaLabels fixes the label and order of the known meta keys (spec §3.4);
// unknown keys follow alphabetically with their key as label.
var roleMetaLabels = []struct{ key, label string }{
	{"name", "Nama"},
	{"description", "Deskripsi"},
	{"requires_physician_data", "Kredensial STR/SIP"},
}

// formatRoleChangesText renders a role_change_log.changes JSON document as the
// compact CSV text (spec §3.4): sections separated by "; " in the order meta →
// added permissions → removed permissions → widgets. A document that does not
// parse falls back to the raw JSON text.
func formatRoleChangesText(raw []byte) string {
	var parsed struct {
		Meta       map[string]map[string]any `json:"meta"`
		Permission struct {
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		} `json:"permission"`
		Widget []struct {
			Key  string `json:"key"`
			From any    `json:"from"`
			To   any    `json:"to"`
		} `json:"widget"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return string(raw)
	}
	parts := make([]string, 0,
		len(parsed.Meta)+len(parsed.Permission.Added)+len(parsed.Permission.Removed)+len(parsed.Widget))
	seen := make(map[string]bool, len(parsed.Meta))
	for _, m := range roleMetaLabels {
		if change, ok := parsed.Meta[m.key]; ok {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", m.label,
				roleMetaValue(change["from"]), roleMetaValue(change["to"])))
			seen[m.key] = true
		}
	}
	unknown := make([]string, 0, len(parsed.Meta))
	for key := range parsed.Meta {
		if !seen[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		change := parsed.Meta[key]
		parts = append(parts, fmt.Sprintf("%s: %s → %s", key,
			roleMetaValue(change["from"]), roleMetaValue(change["to"])))
	}
	for _, code := range parsed.Permission.Added {
		parts = append(parts, "+ "+code)
	}
	for _, code := range parsed.Permission.Removed {
		parts = append(parts, "− "+code)
	}
	for _, wgt := range parsed.Widget {
		parts = append(parts, fmt.Sprintf("Widget %s: %s → %s", wgt.Key,
			roleWidgetValue(wgt.From), roleWidgetValue(wgt.To)))
	}
	return strings.Join(parts, "; ")
}

// roleMetaValue renders a meta from/to value (spec §3.4): booleans as
// ya/tidak, empty strings as —, anything else verbatim.
func roleMetaValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case bool:
		if t {
			return "ya"
		}
		return "tidak"
	case string:
		if t == "" {
			return "—"
		}
		return t
	default:
		return fmt.Sprint(t)
	}
}

// roleWidgetValue renders a widget from/to value (spec §3.4): null as default,
// true/false as tampil/sembunyi, other values verbatim.
func roleWidgetValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "default"
	case bool:
		if t {
			return "tampil"
		}
		return "sembunyi"
	case string:
		if t == "" {
			return "—"
		}
		return t
	default:
		return fmt.Sprint(t)
	}
}

// ListAuditRolesHandler godoc
// @Summary List every role of the company for the audit filter
// @Description Returns all roles of the caller's company, soft-deleted ones
// @Description included (flagged deleted), sorted by name — the dropdown source
// @Description for the role change audit filter.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/roles [get]
func ListAuditRolesHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListAuditRoles(c.Request.Context(), AuthCompanyID(c))
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":      uuidOrNil(r.ID),
			"name":    r.Name,
			"deleted": r.Deleted,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// parseRoleChangeFilters parses the query filters shared verbatim by the role
// change list and its CSV export: the from/to window plus the role and
// changed-by UUID filters. ok=false means a 400 was already written.
func parseRoleChangeFilters(c *gin.Context) (from, to time.Time, roleID, changedBy pgtype.UUID, ok bool) {
	from, to, ok = auditRange(c)
	if !ok {
		return
	}
	if roleID, ok = auditUUIDFilter(c, "role_id"); !ok {
		return
	}
	if changedBy, ok = auditUUIDFilter(c, "changed_by"); !ok {
		return
	}
	return from, to, roleID, changedBy, true
}
