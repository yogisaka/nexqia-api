// internal/server/params.go
package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

// parseUUID parses a path/query/body UUID string into pgtype.UUID.
// ok is false on empty or malformed input.
func parseUUID(s string) (id pgtype.UUID, ok bool) {
	if err := id.Scan(s); err != nil {
		return pgtype.UUID{}, false
	}
	return id, id.Valid
}

// paginationParams reads ?limit=&offset= query params with sane defaults/caps.
func paginationParams(c *gin.Context) (limit, offset int32) {
	limit, offset = 50, 0
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 200 {
		limit = int32(v)
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v >= 0 {
		offset = int32(v)
	}
	return limit, offset
}

// requireMerchantHeader parses X-Merchant-ID and writes a 400 response if
// missing/invalid. Use for list/display endpoints that need the merchant id
// itself (not just a permission check) but have no :id path param to source it
// from — mirrors the header-parsing RequirePermission already does internally.
func requireMerchantHeader(c *gin.Context) (pgtype.UUID, bool) {
	merchantID, ok := parseUUID(c.GetHeader("X-Merchant-ID"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid X-Merchant-ID"})
		return pgtype.UUID{}, false
	}
	return merchantID, true
}
