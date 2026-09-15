// internal/server/params.go
package server

import (
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
