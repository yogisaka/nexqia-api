// internal/server/coldchain.go
// Cold chain API (spec 2026-10-20-inventory-17b-procurement-special-cases §7,
// migration 000083): opened containers with beyond-use dating (BUD), VVM
// stage checks, vaccine fridges and their temperature logs. Routes are
// registered here but wired from server.go in the emergency task; every
// handler runs inside the request transaction. Single-write 403 rule:
// RequirePermissionForMerchant writes the response itself, so a handler may
// call it AT MOST ONCE — membership checks go through userHasPermissionNoWrite,
// which never writes a response.
package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

const (
	// PermColdChainLog guards cold chain records: fridge temperature entries,
	// VVM checks and opened-container open/waste (permission
	// inventory.coldchain.log, migration 000083).
	PermColdChainLog = "inventory.coldchain.log"
	// PermEmergencyUse guards emergency kit seal lifecycle (permission
	// inventory.emergency.use, migration 000083) — used by emergency.go.
	PermEmergencyUse = "inventory.emergency.use"

	// Acceptable vaccine fridge temperature range in °C (spec §7.1): entries
	// outside +2..+8 are flagged out_of_range; quarantine stays manual.
	fridgeTempMin = 2.0
	fridgeTempMax = 8.0
)

// errNoBudRule marks an item without the BUD attribute needed for the chosen
// branch (spec §7.2 #1: mapped to 400 "no bud rule").
var errNoBudRule = errors.New("no bud rule")

// userHasPermissionNoWrite mirrors RequirePermissionForMerchant WITHOUT
// writing a response: it resolves the session's active role and reports
// whether it holds `code` at merchantID. Errors are returned for the caller
// to map (respondInternalError); a caller without a role simply reads false.
// Use it before the single RequirePermissionForMerchant call whenever a
// handler may pass with either of two permissions.
func userHasPermissionNoWrite(c *gin.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, code string) (bool, error) {
	roleID, err := effectiveRoleID(c, q, AuthUserID(c), merchantID)
	if err != nil {
		return false, err
	}
	if !roleID.Valid {
		return false, nil
	}
	return q.UserHasPermission(c.Request.Context(), sqlcgen.UserHasPermissionParams{
		UserID: AuthUserID(c), MerchantID: merchantID, Code: code, RoleID: roleID,
	})
}

// fridgeTempOutOfRange reports whether a temperature reading falls outside
// the +2..+8 °C vaccine range (spec §7.1).
func fridgeTempOutOfRange(t float64) bool {
	return t < fridgeTempMin || t > fridgeTempMax
}

// computeBudAt derives the beyond-use date of an opened container (spec
// §7.2 #1): reconstituted containers follow the reconstitution rule, others
// the open rule; an item without the attribute for the chosen branch fails
// with errNoBudRule.
func computeBudAt(openedAt time.Time, reconstituted bool, openHours, reconstHours *int32) (time.Time, error) {
	hours := openHours
	if reconstituted {
		hours = reconstHours
	}
	if hours == nil {
		return time.Time{}, errNoBudRule
	}
	return openedAt.Add(time.Duration(*hours) * time.Hour), nil
}

// int4Hours maps a nullable item BUD attribute to the pure helper's input.
func int4Hours(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	h := v.Int32
	return &h
}

// RegisterColdChainRoutes registers the cold chain endpoints under the
// authenticated router group (wired in server.go).
func RegisterColdChainRoutes(rg *gin.RouterGroup) {
	rg.POST("/merchants/:id/opened-containers", CreateOpenedContainerHandler)
	rg.GET("/merchants/:id/opened-containers", ListOpenedContainersHandler)
	rg.POST("/opened-containers/:id/waste", WasteOpenedContainerHandler)

	rg.POST("/merchants/:id/fridges", CreateFridgeHandler)
	rg.GET("/merchants/:id/fridges", ListFridgesHandler)
	rg.GET("/fridges/:id", GetFridgeHandler)
	rg.PATCH("/fridges/:id", UpdateFridgeHandler)

	rg.POST("/merchants/:id/fridge-temp-logs", CreateFridgeTempLogHandler)
	rg.GET("/merchants/:id/fridge-temp-logs", ListFridgeTempLogsHandler)
}

// openedContainerJSON renders an opened container for responses.
func openedContainerJSON(r sqlcgen.InventoryOpenedContainer) gin.H {
	return gin.H{
		"id":                 r.ID.String(),
		"merchant_id":        r.MerchantID.String(),
		"item_id":            r.ItemID.String(),
		"lot_id":             r.LotID.String(),
		"location_id":        r.LocationID.String(),
		"opened_at":          tsOrNil(r.OpenedAt),
		"reconstituted":      r.Reconstituted,
		"bud_at":             tsOrNil(r.BudAt),
		"remaining_base_qty": numericExact(r.RemainingBaseQty),
		"status":             r.Status,
		"note":               textOrNil(r.Note),
	}
}

func fridgeJSON(r sqlcgen.InventoryFridge) gin.H {
	return gin.H{
		"id":          r.ID.String(),
		"merchant_id": r.MerchantID.String(),
		"name":        r.Name,
		"location_id": uuidOrNil(r.LocationID),
		"note":        textOrNil(r.Note),
		"is_active":   r.IsActive,
	}
}

// fridgeTempLogJSON renders one temperature entry; out_of_range is computed
// server-side from the numeric reading (spec §7.1, +2..+8 °C).
func fridgeTempLogJSON(r sqlcgen.InventoryFridgeTempLog) gin.H {
	out := gin.H{
		"id":          r.ID.String(),
		"merchant_id": r.MerchantID.String(),
		"fridge_id":   r.FridgeID.String(),
		"logged_at":   tsOrNil(r.LoggedAt),
		"temperature": numericExact(r.Temperature),
		"taken_by":    uuidOrNil(r.TakenBy),
		"note":        textOrNil(r.Note),
	}
	if f, err := strconv.ParseFloat(numericExact(r.Temperature), 64); err == nil {
		out["out_of_range"] = fridgeTempOutOfRange(f)
	}
	return out
}

// §7.2 #1 — opened containers with BUD.

type openedContainerRequest struct {
	ItemID           string `json:"item_id" binding:"required"`
	LotID            string `json:"lot_id" binding:"required"`
	LocationID       string `json:"location_id" binding:"required"`
	OpenedAt         string `json:"opened_at" binding:"required"`
	Reconstituted    bool   `json:"reconstituted"`
	RemainingBaseQty string `json:"remaining_base_qty" binding:"required"`
	Note             string `json:"note"`
}

// CreateOpenedContainerHandler godoc
// @Summary Open a multidose vial and stamp its beyond-use date (no balance change)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body openedContainerRequest true "Opened container"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/opened-containers [post]
func CreateOpenedContainerHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, merchantID) {
		return
	}
	var req openedContainerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	itemID, okI := parseUUID(req.ItemID)
	lotID, okL := parseUUID(req.LotID)
	locID, okR := parseUUID(req.LocationID)
	if !okI || !okL || !okR {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reference"})
		return
	}
	openedAt, err := time.Parse(time.RFC3339, req.OpenedAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid opened_at"})
		return
	}
	qty, err := parseNumeric(req.RemainingBaseQty)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, err := q.GetInventoryItemByID(c.Request.Context(), itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	budAt, err := computeBudAt(openedAt, req.Reconstituted,
		int4Hours(item.BudAfterOpenHours), int4Hours(item.BudAfterReconstitutionHours))
	if errors.Is(err, errNoBudRule) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no bud rule"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	container, err := q.InsertOpenedContainer(c.Request.Context(), sqlcgen.InsertOpenedContainerParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		ItemID: itemID, LotID: lotID, LocationID: locID,
		OpenedAt:         pgtype.Timestamptz{Time: openedAt, Valid: true},
		Reconstituted:    req.Reconstituted,
		BudAt:            pgtype.Timestamptz{Time: budAt, Valid: true},
		RemainingBaseQty: qty,
		Note:             nargText(&req.Note),
		CreatedBy:        AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// The vial is still an asset: NO balance move is written here (spec §7.2 #1).
	c.JSON(http.StatusCreated, gin.H{"data": openedContainerJSON(container), "meta": gin.H{}})
}

// ListOpenedContainersHandler godoc
// @Summary List the merchant's opened containers (optionally by status, BUD order)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param status query string false "Filter by status (open|finished|expired|wasted)"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/opened-containers [get]
func ListOpenedContainersHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Single-write 403: cold chain members pass without any response write;
	// non-members get exactly one RequirePermissionForMerchant attempt.
	has, err := userHasPermissionNoWrite(c, q, merchantID, PermColdChainLog)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !has && !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	rows, err := q.ListOpenedContainers(c.Request.Context(), sqlcgen.ListOpenedContainersParams{
		MerchantID: merchantID,
		Status:     optTextValue(c.Query("status")),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, openedContainerJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type wasteOpenedContainerRequest struct {
	Reason string `json:"reason"`
}

// WasteOpenedContainerHandler godoc
// @Summary Waste an open container: one negative issue move plus status wasted
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Opened container UUID"
// @Param request body wasteOpenedContainerRequest true "Reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /opened-containers/{id}/waste [post]
func WasteOpenedContainerHandler(c *gin.Context) {
	containerID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	container, err := q.GetOpenedContainerByID(c.Request.Context(), containerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "container not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, container.MerchantID) {
		return
	}
	var req wasteOpenedContainerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason required"})
		return
	}
	// Only an OPEN container may be wasted; an expired BUD is exactly the
	// case waste exists for, so the date is NOT a guard here (spec §7.2 #1).
	if container.Status != "open" {
		c.JSON(http.StatusConflict, gin.H{"error": "container not open"})
		return
	}
	txTime, err := q.TransactionTime(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Waste = the remaining quantity leaves the balance through the locked
	// ledger as one negative issue referencing the container.
	if _, err := issueStock(c.Request.Context(), q, container.MerchantID, container.CompanyID, AuthUserID(c),
		container.LocationID, container.ItemID, container.RemainingBaseQty, txTime.Time,
		stockRef{RefType: "opened_container", RefID: container.ID}, pgtype.UUID{}, ""); err != nil {
		if !respondStockError(c, err) {
			respondInternalError(c, err)
		}
		return
	}
	wasted, err := q.SetOpenedContainerStatus(c.Request.Context(), sqlcgen.SetOpenedContainerStatusParams{
		ID: container.ID, Status: "wasted", UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": openedContainerJSON(wasted), "meta": gin.H{}})
}

// §7.2 #3 — fridges.

type fridgeRequest struct {
	Name       string  `json:"name" binding:"required"`
	LocationID *string `json:"location_id"`
	Note       *string `json:"note"`
	IsActive   *bool   `json:"is_active"`
}

// CreateFridgeHandler godoc
// @Summary Register a vaccine fridge
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body fridgeRequest true "Fridge"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/fridges [post]
func CreateFridgeHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, merchantID) {
		return
	}
	var req fridgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var locID pgtype.UUID
	if req.LocationID != nil && *req.LocationID != "" {
		var okL bool
		if locID, okL = parseUUID(*req.LocationID); !okL {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	if locID.Valid {
		if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
			return
		}
	}
	fridge, err := q.InsertFridge(c.Request.Context(), sqlcgen.InsertFridgeParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		Name: req.Name, LocationID: locID, Note: nargText(req.Note),
		IsActive: nargBool(req.IsActive), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": fridgeJSON(fridge), "meta": gin.H{}})
}

// ListFridgesHandler godoc
// @Summary List the merchant's vaccine fridges
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/fridges [get]
func ListFridgesHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListFridges(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, fridgeJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// GetFridgeHandler godoc
// @Summary Read one vaccine fridge
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fridge UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /fridges/{id} [get]
func GetFridgeHandler(c *gin.Context) {
	fridgeID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	fridge, err := q.GetFridgeByID(c.Request.Context(), fridgeID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fridge not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, fridge.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": fridgeJSON(fridge), "meta": gin.H{}})
}

// UpdateFridgeHandler godoc
// @Summary Update a vaccine fridge (partial)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Fridge UUID"
// @Param request body fridgeRequest true "Fridge fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /fridges/{id} [patch]
func UpdateFridgeHandler(c *gin.Context) {
	fridgeID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	fridge, err := q.GetFridgeByID(c.Request.Context(), fridgeID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fridge not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, fridge.MerchantID) {
		return
	}
	var req fridgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var locID pgtype.UUID
	if req.LocationID != nil && *req.LocationID != "" {
		var okL bool
		if locID, okL = parseUUID(*req.LocationID); !okL {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
			return
		}
		if _, ok := loadMerchantRoom(c, q, locID, fridge.MerchantID); !ok {
			return
		}
	}
	updated, err := q.UpdateFridge(c.Request.Context(), sqlcgen.UpdateFridgeParams{
		ID:         fridgeID,
		Name:       nargText(&req.Name),
		LocationID: locID,
		Note:       nargText(req.Note),
		IsActive:   nargBool(req.IsActive),
		UpdatedBy:  AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": fridgeJSON(updated), "meta": gin.H{}})
}

// §7.2 #3 — temperature logs.

type fridgeTempLogRequest struct {
	FridgeID    string  `json:"fridge_id" binding:"required"`
	LoggedAt    string  `json:"logged_at"`
	Temperature string  `json:"temperature" binding:"required"`
	Note        *string `json:"note"`
}

// CreateFridgeTempLogHandler godoc
// @Summary Record a fridge temperature reading (flagged outside +2..+8 °C)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body fridgeTempLogRequest true "Reading"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /merchants/{id}/fridge-temp-logs [post]
func CreateFridgeTempLogHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermColdChainLog, merchantID) {
		return
	}
	var req fridgeTempLogRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fridgeID, okF := parseUUID(req.FridgeID)
	if !okF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid fridge_id"})
		return
	}
	temperature, err := parseNumeric(req.Temperature)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid temperature"})
		return
	}
	tempFloat, errF := strconv.ParseFloat(strings.TrimSpace(req.Temperature), 64)
	if errF != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid temperature"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loggedAt, errT := q.TransactionTime(c.Request.Context())
	if errT != nil {
		respondInternalError(c, errT)
		return
	}
	if req.LoggedAt != "" {
		parsed, errP := time.Parse(time.RFC3339, req.LoggedAt)
		if errP != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid logged_at"})
			return
		}
		loggedAt = pgtype.Timestamptz{Time: parsed, Valid: true}
	}
	fridge, err := q.GetFridgeByID(c.Request.Context(), fridgeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && fridge.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fridge not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	entry, err := q.InsertFridgeTempLog(c.Request.Context(), sqlcgen.InsertFridgeTempLogParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		FridgeID: fridgeID, LoggedAt: loggedAt, Temperature: temperature,
		TakenBy: AuthUserID(c), Note: nargText(req.Note),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := fridgeTempLogJSON(entry)
	data["out_of_range"] = fridgeTempOutOfRange(tempFloat)
	c.JSON(http.StatusCreated, gin.H{"data": data, "meta": gin.H{}})
}

// ListFridgeTempLogsHandler godoc
// @Summary List fridge temperature readings (date range, newest first)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param from query string false "From date YYYY-MM-DD (inclusive)"
// @Param to query string false "To date YYYY-MM-DD (inclusive)"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/fridge-temp-logs [get]
func ListFridgeTempLogsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	// Single-write 403: cold chain members pass without any response write;
	// non-members get exactly one RequirePermissionForMerchant attempt.
	has, err := userHasPermissionNoWrite(c, q, merchantID, PermColdChainLog)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !has && !RequirePermissionForMerchant(c, PermStockView, merchantID) {
		return
	}
	params := sqlcgen.ListFridgeTempLogsParams{MerchantID: merchantID}
	if raw := c.Query("from"); raw != "" {
		d, okD := parseDateStr(raw)
		if !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date range"})
			return
		}
		params.FromDate = d
	}
	if raw := c.Query("to"); raw != "" {
		d, okD := parseDateStr(raw)
		if !okD {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date range"})
			return
		}
		params.ToDate = d
	}
	rows, err := q.ListFridgeTempLogs(c.Request.Context(), params)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, fridgeTempLogJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
