// internal/server/access_log.go
// Access log for patient-data reads, see docs/design/specs/2026-09-29-audit-trail-design.md §4.
package server

import (
	"log"
	"net/netip"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// AccessLog records one core.access_log row per request, AFTER the handler ran, inside
// the request transaction (an aborted request rolls back and is not recorded; a 403
// written with c.JSON is recorded with its status). It never records the query string,
// headers or body. idParam names the route param holding resource_id ("" = none).
func AccessLog(resource, action, idParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.IsAborted() {
			return
		}
		var resourceID pgtype.UUID
		if idParam != "" {
			resourceID, _ = parseUUID(c.Param(idParam))
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		var platformAdminID pgtype.UUID
		if by := AuthImpersonatedBy(c); by != "" {
			platformAdminID, _ = parseUUID(by)
		}
		var ip *netip.Addr
		if addr, err := netip.ParseAddr(c.ClientIP()); err == nil {
			ip = &addr
		}
		q := sqlcgen.New(TxFromContext(c))
		if err := q.CreateAccessLog(c.Request.Context(), sqlcgen.CreateAccessLogParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, ActorID: AuthUserID(c),
			PlatformAdminID: platformAdminID, Resource: resource, ResourceID: resourceID,
			Action: action, StatusCode: int16(c.Writer.Status()), IpAddress: ip,
		}); err != nil {
			// The response is already written; surface the gap in server logs.
			log.Printf("access log write failed: resource=%s action=%s: %v", resource, action, err)
		}
	}
}
