// internal/server/access_review.go
// Per-user access summary with review flags (spec
// docs/design/plans/2026-09-29-access-review.md, §6.1–§6.2, §7). The SQL
// (sqlcgen.AccessReviewCounts) returns raw counts; the flags and the median
// are computed here so they stay unit-testable.
package server

import (
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

type createAccessReviewRequest struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Notes string `json:"notes"`
}

// CreateAccessReviewHandler godoc
// @Summary Record a signed-off access review
// @Description Stores an append-only review record for the period. flagged_users
// @Description is computed server-side from the same counts and flags as the
// @Description summary endpoint; the caller only supplies the period and notes.
// @Tags audit
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body createAccessReviewRequest true "Review period (RFC3339) and notes (1-2000 chars)"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/reviews [post]
func CreateAccessReviewHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermAuditLogView) {
			return
		}
		var req createAccessReviewRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		from, err := time.Parse(time.RFC3339, req.From)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from timestamp"})
			return
		}
		to, err := time.Parse(time.RFC3339, req.To)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to timestamp"})
			return
		}
		if !to.After(from) || to.Sub(from) > auditMaxRangeDays*24*time.Hour {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid time range"})
			return
		}
		notes := strings.TrimSpace(req.Notes)
		if n := utf8.RuneCountInString(notes); n < 1 || n > 2000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "notes must be 1-2000 characters"})
			return
		}

		q := sqlcgen.New(TxFromContext(c))
		fromTS := pgtype.Timestamptz{Time: from, Valid: true}
		toTS := pgtype.Timestamptz{Time: to, Valid: true}
		counts, err := q.AccessReviewCounts(c.Request.Context(), sqlcgen.AccessReviewCountsParams{
			CompanyID: AuthCompanyID(c),
			FromTime:  fromTS,
			ToTime:    toTS,
			Tz:        cfg.AuditLocation.String(),
			StartHour: int32(cfg.AuditWorkHourStart),
			EndHour:   int32(cfg.AuditWorkHourEnd),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		flagged := 0
		rows, _ := ComputeReviewFlags(counts, cfg)
		for _, r := range rows {
			if len(r.Flags) > 0 {
				flagged++
			}
		}
		rec, err := q.CreateAccessReview(c.Request.Context(), sqlcgen.CreateAccessReviewParams{
			CompanyID:    AuthCompanyID(c),
			PeriodFrom:   fromTS,
			PeriodTo:     toTS,
			ReviewedBy:   AuthUserID(c),
			FlaggedUsers: int32(flagged),
			Notes:        notes,
		})
		if err != nil {
			abortInternalError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"id":            uuidOrNil(rec.ID),
			"period_from":   rfc3339OrNil(rec.PeriodFrom),
			"period_to":     rfc3339OrNil(rec.PeriodTo),
			"reviewed_by":   uuidOrNil(rec.ReviewedBy),
			"reviewed_at":   rfc3339OrNil(rec.ReviewedAt),
			"flagged_users": rec.FlaggedUsers,
			"notes":         rec.Notes,
		}})
	}
}

// ListAccessReviewsHandler godoc
// @Summary List recorded access reviews
// @Description Returns the caller's company review records, newest first.
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Rows to skip (default 0)"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiResponse
// @Router /audit/reviews [get]
func ListAccessReviewsHandler(c *gin.Context) {
	if !RequirePermission(c, PermAuditLogView) {
		return
	}
	limit, offset := auditLimitOffset(c)
	rows, err := sqlcgen.New(TxFromContext(c)).ListAccessReviews(c.Request.Context(), sqlcgen.ListAccessReviewsParams{
		CompanyID: AuthCompanyID(c),
		RowLimit:  limit,
		RowOffset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":               uuidOrNil(r.ID),
			"period_from":      rfc3339OrNil(r.PeriodFrom),
			"period_to":        rfc3339OrNil(r.PeriodTo),
			"reviewed_by":      uuidOrNil(r.ReviewedBy),
			"reviewed_by_name": textOrNil(r.ReviewedByName),
			"reviewed_at":      rfc3339OrNil(r.ReviewedAt),
			"flagged_users":    r.FlaggedUsers,
			"notes":            r.Notes,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}
