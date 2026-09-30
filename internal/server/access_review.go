// internal/server/access_review.go
// Per-user access summary with review flags (spec
// docs/design/plans/2026-09-29-access-review.md, §6.1–§6.2, §7). The SQL
// (sqlcgen.AccessReviewCounts) returns raw counts; the flags and the median
// are computed here so they stay unit-testable.
package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// ReviewRow is one actor's raw period counts plus the computed review flags.
// Flags is always non-nil (empty when nothing is flagged) so JSON never renders
// it as null.
type ReviewRow struct {
	sqlcgen.AccessReviewCountsRow
	Flags []string
}

// ComputeReviewFlags attaches the review flags to each count row (spec §6.2)
// and sorts the output by flag count desc, then views desc, then actor_id asc.
// Flag order within a row is fixed: after_hours, denied, self_family, volume,
// platform_admin. medianViews is the median of the per-actor views > 0 (even
// count → mean of the two middle values; no positive views → 0). The volume
// flag fires only when the median > 0 and views strictly exceed multiplier ×
// median.
func ComputeReviewFlags(rows []sqlcgen.AccessReviewCountsRow, cfg config.Config) (out []ReviewRow, medianViews float64) {
	views := []float64{}
	for _, r := range rows {
		if r.Views > 0 {
			views = append(views, float64(r.Views))
		}
	}
	sort.Float64s(views)
	if n := len(views); n > 0 {
		if n%2 == 1 {
			medianViews = views[n/2]
		} else {
			medianViews = (views[n/2-1] + views[n/2]) / 2
		}
	}

	out = make([]ReviewRow, 0, len(rows))
	for _, r := range rows {
		flags := []string{}
		if r.AfterHours > 0 {
			flags = append(flags, "after_hours")
		}
		if r.Denied >= int64(cfg.AuditReviewDeniedThreshold) {
			flags = append(flags, "denied")
		}
		if r.SelfFamily > 0 {
			flags = append(flags, "self_family")
		}
		if medianViews > 0 && float64(r.Views) > cfg.AuditReviewVolumeMultiplier*medianViews {
			flags = append(flags, "volume")
		}
		if r.PlatformAdmin > 0 {
			flags = append(flags, "platform_admin")
		}
		out = append(out, ReviewRow{AccessReviewCountsRow: r, Flags: flags})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if len(a.Flags) != len(b.Flags) {
			return len(a.Flags) > len(b.Flags)
		}
		if a.Views != b.Views {
			return a.Views > b.Views
		}
		return uuidLess(a.ActorID, b.ActorID)
	})
	return out, medianViews
}

// uuidLess orders two pgtype.UUIDs byte-wise (invalid sorts as all-zero).
func uuidLess(a, b pgtype.UUID) bool {
	for i := range a.Bytes {
		if a.Bytes[i] != b.Bytes[i] {
			return a.Bytes[i] < b.Bytes[i]
		}
	}
	return false
}

// AccessReviewSummaryHandler godoc
// @Summary Summarize per-user access with review flags
// @Description Aggregates access_log/audit_log activity per user for the
// @Description caller's company over the given window and computes review
// @Description flags: after_hours (access outside the configured working
// @Description hours), denied (>= AUDIT_REVIEW_DENIED_THRESHOLD 403s),
// @Description self_family (access involving the actor's own/family person),
// @Description volume (views above the configured multiplier × the period
// @Description median) and platform_admin. Rows are ordered by flag count,
// @Description then views, then actor_id.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param from query string false "Range start, RFC3339 (default: to − 7 days)"
// @Param to query string false "Range end, RFC3339 (default: now)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/review/summary [get]
func AccessReviewSummaryHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermAuditLogView) {
			return
		}
		from, to, ok := auditRange(c)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		rows, err := q.AccessReviewCounts(c.Request.Context(), sqlcgen.AccessReviewCountsParams{
			CompanyID: AuthCompanyID(c),
			FromTime:  pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:    pgtype.Timestamptz{Time: to, Valid: true},
			Tz:        cfg.AuditLocation.String(),
			StartHour: int32(cfg.AuditWorkHourStart),
			EndHour:   int32(cfg.AuditWorkHourEnd),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		flagged, medianViews := ComputeReviewFlags(rows, cfg)
		data := make([]gin.H, 0, len(flagged))
		for _, r := range flagged {
			data = append(data, gin.H{
				"actor_id":       uuidOrNil(r.ActorID),
				"actor_name":     textOrNil(r.ActorName),
				"views":          r.Views,
				"lists":          r.Lists,
				"changes":        r.Changes,
				"denied":         r.Denied,
				"after_hours":    r.AfterHours,
				"self_family":    r.SelfFamily,
				"platform_admin": r.PlatformAdmin,
				"audit_actions":  r.AuditActions,
				"flags":          r.Flags,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"data": data,
			"meta": gin.H{
				"median_views": medianViews,
				"from":         from.Format(time.RFC3339),
				"to":           to.Format(time.RFC3339),
			},
		})
	}
}
