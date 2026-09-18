// internal/server/payer.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterPayerRoutes wires core.payer listing (pendaftaran form "Jaminan
// Pembayaran" section). No dedicated payer permission exists in the RBAC seed;
// the closest master-data manage permission is core.tariff.manage (tariff.go),
// which already covers payer-linked service rates — reused here rather than
// minting a new permission for a read-only list.
func RegisterPayerRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/payers", ListPayersHandler)
}

// ListPayersHandler godoc
// @Summary List payers (penjamin) under a merchant
// @Tags payer
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/payers [get]
func ListPayersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermTariffManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	payers, err := q.ListPayersByMerchant(c.Request.Context(), sqlcgen.ListPayersByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payers, "meta": gin.H{"limit": limit, "offset": offset}})
}
