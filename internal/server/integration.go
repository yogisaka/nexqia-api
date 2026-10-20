// internal/server/integration.go
// Integration endpoints (Permenkes 6/2026 Fase C2, spec
// 2026-10-20-permenkes-6-2026-compliance-design §5): SATUSEHAT configuration
// with encrypted credentials, explicit Organization/Location re-enqueue, and
// the resource-map sync status. The worker side lives in internal/connect.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/connect"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

const (
	PermIntegrationStatusView   = "integration.status.view"
	PermIntegrationConfigManage = "integration.config.manage"
)

// RegisterIntegrationRoutes wires the merchant SATUSEHAT endpoints (spec §5).
// Handlers run inside the ambient tenant transaction like payer_plan.go.
func RegisterIntegrationRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.GET("/merchants/:id/integration/satusehat", getSatusehatConfigHandler())
	rg.PUT("/merchants/:id/integration/satusehat", putSatusehatConfigHandler(cfg))
	rg.POST("/merchants/:id/integration/satusehat/sync", postSatusehatSyncHandler())
	rg.GET("/merchants/:id/integration/satusehat/status", getSatusehatStatusHandler())
}

// loadSatusehatMerchant resolves the path merchant (404 when missing) and
// checks `perm` at that merchant; returns false when the response was written.
func loadSatusehatMerchant(c *gin.Context, perm string) (sqlcgen.GetMerchantForSatusehatRow, bool) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.GetMerchantForSatusehatRow{}, false
	}
	q := sqlcgen.New(TxFromContext(c))
	merchant, err := q.GetMerchantForSatusehat(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merchant not found"})
		return sqlcgen.GetMerchantForSatusehatRow{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.GetMerchantForSatusehatRow{}, false
	}
	if !RequirePermissionForMerchant(c, perm, merchant.ID) {
		return sqlcgen.GetMerchantForSatusehatRow{}, false
	}
	return merchant, true
}

// satusehatConfigResponse reads the config row and renders the GET shape
// (configured true/false).
func satusehatConfigResponse(c *gin.Context, merchantID pgtype.UUID) bool {
	q := sqlcgen.New(TxFromContext(c))
	row, err := q.GetSatusehatConfigByMerchant(c.Request.Context(), merchantID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"configured": false}, "meta": gin.H{}})
		return true
	}
	if err != nil {
		respondInternalError(c, err)
		return false
	}
	var cfgJSON any
	if row.Config != nil {
		cfgJSON = json.RawMessage(row.Config)
	}
	var baseURL any
	if row.BaseUrl.Valid {
		baseURL = row.BaseUrl.String
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"configured":     true,
		"base_url":       baseURL,
		"config":         cfgJSON,
		"is_active":      row.IsActive,
		"credential_set": row.CredentialSet,
	}, "meta": gin.H{}})
	return true
}

// getSatusehatConfigHandler returns the merchant SATUSEHAT configuration
// (never the credentials themselves).
func getSatusehatConfigHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		merchant, ok := loadSatusehatMerchant(c, PermIntegrationStatusView)
		if !ok {
			return
		}
		satusehatConfigResponse(c, merchant.ID)
	}
}

type satusehatConfigInput struct {
	BaseURL      string         `json:"base_url"`
	ClientID     string         `json:"client_id"`
	ClientSecret string         `json:"client_secret"`
	IsActive     *bool          `json:"is_active"`
	Config       map[string]any `json:"config"`
}

// putSatusehatConfigHandler upserts the config with credentials encrypted via
// pgp_sym_encrypt using cfg.IntegrationCredentialKey.
func putSatusehatConfigHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		merchant, ok := loadSatusehatMerchant(c, PermIntegrationConfigManage)
		if !ok {
			return
		}
		var req satusehatConfigInput
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.BaseURL == "" || !(strings.HasPrefix(req.BaseURL, "http://") || strings.HasPrefix(req.BaseURL, "https://")) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_url"})
			return
		}
		if req.ClientID == "" || req.ClientSecret == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "client_id and client_secret are required"})
			return
		}
		if cfg.IntegrationCredentialKey == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "integration credential key not configured"})
			return
		}
		isActive := true
		if req.IsActive != nil {
			isActive = *req.IsActive
		}
		credential, err := json.Marshal(struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}{ClientID: req.ClientID, ClientSecret: req.ClientSecret})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var cfgParam []byte
		if req.Config != nil {
			cfgParam, err = json.Marshal(req.Config)
			if err != nil {
				respondInternalError(c, err)
				return
			}
		}
		q := sqlcgen.New(TxFromContext(c))
		if err := q.UpsertSatusehatConfig(c.Request.Context(), sqlcgen.UpsertSatusehatConfigParams{
			CompanyID: merchant.CompanyID, MerchantID: merchant.ID,
			BaseUrl: pgtype.Text{String: req.BaseURL, Valid: true},
			Config:  cfgParam, Column5: string(credential), Column6: cfg.IntegrationCredentialKey,
			IsActive: isActive, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		satusehatConfigResponse(c, merchant.ID)
	}
}

// satusehatMapFHIRID reads the known FHIR id for one local entity; a missing
// map row is "" (never created remotely yet), not an error (review temuan 3).
func satusehatMapFHIRID(c *gin.Context, q *sqlcgen.Queries, localTable string, localID pgtype.UUID, resourceType string) (string, bool) {
	row, err := q.GetSatusehatMapByLocal(c.Request.Context(), sqlcgen.GetSatusehatMapByLocalParams{
		LocalTable: localTable, LocalID: localID, FhirResourceType: resourceType,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", true
	}
	if err != nil {
		respondInternalError(c, err)
		return "", false
	}
	if !row.FhirResourceID.Valid {
		return "", true
	}
	return row.FhirResourceID.String, true
}

// satusehatNumericPointer converts a nullable numeric column to *float64.
func satusehatNumericPointer(n pgtype.Numeric) (*float64, error) {
	if !n.Valid {
		return nil, nil
	}
	f, err := n.Float64Value()
	if err != nil {
		return nil, err
	}
	if !f.Valid {
		return nil, nil
	}
	return &f.Float64, nil
}

// postSatusehatSyncHandler re-enqueues the merchant Organization + every
// non-bed Location as pending map rows (explicit sync button). FHIR ids known
// from previous runs are embedded BEFORE the payload is built so the worker
// issues PUT (update) instead of POST (create) (review temuan 2).
func postSatusehatSyncHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		merchant, ok := loadSatusehatMerchant(c, PermIntegrationConfigManage)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		cfgRow, err := q.GetSatusehatConfigByMerchant(c.Request.Context(), merchant.ID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cfgRow.IsActive) {
			c.JSON(http.StatusConflict, gin.H{"error": "satusehat integration not configured"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		orgFHIRID, ok := satusehatMapFHIRID(c, q, "core.merchant", merchant.ID, "Organization")
		if !ok {
			return
		}
		orgPayload := connect.BuildOrganizationPayload(connect.OrganizationInput{
			MerchantID:         merchant.ID.String(),
			Code:               merchant.Code,
			KemkesFacilityCode: merchant.KemkesFacilityCode.String,
			Name:               merchant.Name,
			FHIRID:             orgFHIRID,
		})
		if _, err := q.UpsertSatusehatResourceMap(c.Request.Context(), sqlcgen.UpsertSatusehatResourceMapParams{
			CompanyID: merchant.CompanyID, MerchantID: merchant.ID,
			LocalTable: "core.merchant", LocalID: merchant.ID,
			FhirResourceType: "Organization", Column6: orgPayload,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
		locations, err := q.ListSatusehatLocations(c.Request.Context(), merchant.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		for _, loc := range locations {
			locFHIRID, ok := satusehatMapFHIRID(c, q, "core.location", loc.ID, "Location")
			if !ok {
				return
			}
			lat, err := satusehatNumericPointer(loc.Latitude)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			lng, err := satusehatNumericPointer(loc.Longitude)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			payload := connect.BuildLocationPayload(connect.LocationInput{
				MerchantID:   merchant.ID.String(),
				LocationCode: loc.Code,
				Name:         loc.Name,
				Latitude:     lat,
				Longitude:    lng,
				OrgFHIRID:    orgFHIRID,
				FHIRID:       locFHIRID,
			})
			if _, err := q.UpsertSatusehatResourceMap(c.Request.Context(), sqlcgen.UpsertSatusehatResourceMapParams{
				CompanyID: merchant.CompanyID, MerchantID: merchant.ID,
				LocalTable: "core.location", LocalID: loc.ID,
				FhirResourceType: "Location", Column6: payload,
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"enqueued":     1 + len(locations),
			"organization": 1,
			"locations":    len(locations),
		}, "meta": gin.H{}})
	}
}

// getSatusehatStatusHandler lists the resource-map rows of the merchant
// (newest first, capped at 200).
func getSatusehatStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		merchant, ok := loadSatusehatMerchant(c, PermIntegrationStatusView)
		if !ok {
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		rows, err := q.ListSatusehatResourceStatusByMerchant(c.Request.Context(), merchant.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		resources := make([]gin.H, 0, len(rows))
		for _, r := range rows {
			var fhirID, lastError any
			if r.FhirResourceID.Valid {
				fhirID = r.FhirResourceID.String
			}
			if r.LastError.Valid {
				lastError = r.LastError.String
			}
			var lastSyncedAt any
			if r.LastSyncedAt.Valid {
				lastSyncedAt = r.LastSyncedAt.Time
			}
			resources = append(resources, gin.H{
				"id":                 r.ID,
				"local_table":        r.LocalTable,
				"local_id":           r.LocalID,
				"fhir_resource_type": r.FhirResourceType,
				"fhir_resource_id":   fhirID,
				"sync_status":        r.SyncStatus,
				"last_error":         lastError,
				"last_synced_at":     lastSyncedAt,
				"updated_at":         r.UpdatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"resources": resources}, "meta": gin.H{}})
	}
}
