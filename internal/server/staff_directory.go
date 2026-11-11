// internal/server/staff_directory.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterStaffDirectoryRoutes exposes the read-only staff directory used by
// clinical pickers (e.g. "Petugas yang mengorder" combobox). It is guarded by
// the clinical read permission instead of user.manage so Dokter/Perawat can
// resolve colleague names without holding user management rights.
func RegisterStaffDirectoryRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/staff", ListStaffDirectoryHandler)
}

// staffLimit caps the directory listing. The directory is a picker over a
// single merchant's active staff; paging is not needed at this scale.
const staffLimit = 1000

// ListStaffDirectoryHandler godoc
// @Summary List active staff of a merchant (read-only directory)
// @Description Minimal read-only directory for clinical pickers: id, username,
// full_name, is_active, role names. No management fields (email, phone,
// person detail). Guarded by clinical.record.view, NOT user.manage.
// @Tags rbac
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/staff [get]
func ListStaffDirectoryHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	db := sqlcgen.New(TxFromContext(c))
	rows, err := db.ListAppUsersDirectory(c.Request.Context(), sqlcgen.ListAppUsersDirectoryParams{
		CompanyID:  AuthCompanyID(c),
		MerchantID: merchantID,
		IsActive:   pgtype.Bool{Bool: true, Valid: true},
		RowLimit:   staffLimit,
		RowOffset:  0,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		items = append(items, gin.H{
			"id":        uuidJSON(r.ID),
			"username":  r.Username,
			"full_name": textJSON(r.FullName),
			"is_active": r.IsActive,
			"roles":     r.Roles,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"total": len(items)}})
}
