// internal/server/onboarding.go
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

type onboardingStatusItem struct {
	Key  string `json:"key"`
	Done bool   `json:"done"`
}

func RegisterOnboardingRoutes(rg *gin.RouterGroup) {
	rg.GET("/onboarding-status", OnboardingStatusHandler)
}

// OnboardingStatusHandler godoc
// @Summary Status checklist onboarding untuk merchant aktif
// @Router /onboarding-status [get]
func OnboardingStatusHandler(c *gin.Context) {
	if !RequirePermission(c, PermMerchantManage) {
		return
	}
	merchantID := AuthMerchantID(c)
	if !merchantID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant context required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	deps, err := q.CheckOnboardingDepartments(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	locs, err := q.CheckOnboardingLocations(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	phys, err := q.CheckOnboardingPhysicians(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	scheds, err := q.CheckOnboardingSchedules(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	staff, err := q.CheckOnboardingStaffRoles(ctx, sqlcgen.CheckOnboardingStaffRolesParams{MerchantID: merchantID, UserID: AuthUserID(c)})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	clin, err := q.CheckOnboardingClinicalConfig(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	payers, err := q.CheckOnboardingPayers(ctx, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := []onboardingStatusItem{
		{Key: "departments", Done: deps},
		{Key: "locations", Done: locs},
		{Key: "physicians_schedules", Done: phys && scheds},
		{Key: "staff_roles", Done: staff},
		{Key: "clinical_config", Done: clin},
		{Key: "payers", Done: payers},
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}
