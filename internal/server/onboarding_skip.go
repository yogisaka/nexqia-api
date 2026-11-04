// internal/server/onboarding_skip.go
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

type onboardingSkipPayload struct {
	Skipped           bool    `json:"skipped"`
	SkippedAt         *string `json:"skipped_at"`
	CanManageMerchant bool    `json:"can_manage_merchant"`
}

func RegisterOnboardingSelfRoutes(rg *gin.RouterGroup) {
	rg.GET("/onboarding-skip", OnboardingSkipGetHandler)
	rg.POST("/onboarding-skip", OnboardingSkipSetHandler)
}

// OnboardingSkipGetHandler godoc
// @Summary Status skip onboarding untuk user yang sedang login
// @Router /onboarding-skip [get]
func OnboardingSkipGetHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	userID, ok := onboardingSkipUserContext(c)
	if !ok {
		return
	}
	companyID := AuthCompanyID(c)
	if !companyID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "company context required"})
		return
	}
	skippedAt, err := q.GetAppUserOnboardingSkip(ctx, userID)
	if err != nil {
		if err == pgx.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		respondInternalError(c, err)
		return
	}
	payload, ok := onboardingSkipPermissionPayload(c, ctx, q, userID, companyID, skippedAt, skippedAt.Valid)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload})
}

// OnboardingSkipSetHandler godoc
// @Summary Tandai skip onboarding untuk user yang sedang login (idempoten)
// @Router /onboarding-skip [post]
func OnboardingSkipSetHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	userID, ok := onboardingSkipUserContext(c)
	if !ok {
		return
	}
	companyID := AuthCompanyID(c)
	if !companyID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "company context required"})
		return
	}
	skippedAt, err := q.SetAppUserOnboardingSkip(ctx, userID)
	if err != nil {
		if err == pgx.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		respondInternalError(c, err)
		return
	}
	payload, ok := onboardingSkipPermissionPayload(c, ctx, q, userID, companyID, skippedAt, true)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": payload})
}

// onboardingSkipUserContext returns the caller's user id, responding 400 when
// the auth context lacks it.
func onboardingSkipUserContext(c *gin.Context) (pgtype.UUID, bool) {
	userID := AuthUserID(c)
	if !userID.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user context required"})
		return pgtype.UUID{}, false
	}
	return userID, true
}

// onboardingSkipPermissionPayload probes the company-level permission and
// builds the snake_case payload; on probe error it responds 500 and returns
// ok=false so the caller stops instead of writing a second response.
func onboardingSkipPermissionPayload(c *gin.Context, ctx context.Context, q *sqlcgen.Queries, userID, companyID pgtype.UUID, skippedAt pgtype.Timestamptz, skipped bool) (onboardingSkipPayload, bool) {
	canManage, err := q.UserHasCompanyLevelPermission(ctx, sqlcgen.UserHasCompanyLevelPermissionParams{
		UserID:    userID,
		CompanyID: companyID,
		Codes:     []string{PermMerchantManage},
	})
	if err != nil {
		respondInternalError(c, err)
		return onboardingSkipPayload{}, false
	}
	var skippedAtStr *string
	if skippedAt.Valid {
		s := skippedAt.Time.Format(time.RFC3339)
		skippedAtStr = &s
	}
	return onboardingSkipPayload{Skipped: skipped, SkippedAt: skippedAtStr, CanManageMerchant: canManage}, true
}
