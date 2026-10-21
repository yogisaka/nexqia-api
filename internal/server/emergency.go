// internal/server/emergency.go
// Emergency kit API (spec 2026-10-20-inventory-17b-procurement-special-cases
// §7, migration 000083): sealed emergency kits (troli emergensi) with the
// periodic check report (§7.2 #4), the single-active-seal lifecycle and the
// break wizard that issues stock through the locked ledger. Every handler
// runs inside the request transaction; the permission is inventory.emergency.
// use (PermEmergencyUse) for the whole kit area.
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterEmergencyRoutes registers the emergency kit endpoints under the
// authenticated router group (wired in server.go).
func RegisterEmergencyRoutes(rg *gin.RouterGroup) {
	rg.POST("/merchants/:id/emergency-kits", CreateEmergencyKitHandler)
	rg.GET("/merchants/:id/emergency-kits", ListEmergencyKitsHandler)
	rg.GET("/emergency-kits/:id", GetEmergencyKitHandler)
	rg.PATCH("/emergency-kits/:id", UpdateEmergencyKitHandler)
	rg.POST("/emergency-kits/:id/seals", CreateSealHandler)
	rg.POST("/emergency-kits/:id/seals/:sid/break", BreakSealHandler)
}

// kitJSON renders an emergency kit for responses.
func kitJSON(r sqlcgen.InventoryEmergencyKit) gin.H {
	return gin.H{
		"id":          r.ID.String(),
		"merchant_id": r.MerchantID.String(),
		"name":        r.Name,
		"location_id": r.LocationID.String(),
		"note":        textOrNil(r.Note),
		"is_active":   r.IsActive,
	}
}

// sealJSON renders an emergency kit seal for responses.
func sealJSON(r sqlcgen.InventoryEmergencyKitSeal) gin.H {
	return gin.H{
		"id":        r.ID.String(),
		"kit_id":    r.KitID.String(),
		"seal_no":   r.SealNo,
		"sealed_at": tsOrNil(r.SealedAt),
		"sealed_by": uuidOrNil(r.SealedBy),
		"broken_at": tsOrNil(r.BrokenAt),
		"broken_by": uuidOrNil(r.BrokenBy),
		"note":      textOrNil(r.Note),
	}
}

// loadEmergencyKit resolves one kit of the path and enforces the merchant
// permission exactly once (single-write 403 rule); ok=false when the response
// is already written.
func loadEmergencyKit(c *gin.Context, q *sqlcgen.Queries, kitID pgtype.UUID) (sqlcgen.InventoryEmergencyKit, bool) {
	kit, err := q.GetEmergencyKitByID(c.Request.Context(), kitID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "kit not found"})
		return kit, false
	}
	if err != nil {
		respondInternalError(c, err)
		return kit, false
	}
	if !RequirePermissionForMerchant(c, PermEmergencyUse, kit.MerchantID) {
		return kit, false
	}
	return kit, true
}

type emergencyKitItemRequest struct {
	ItemID  string `json:"item_id" binding:"required"`
	BaseQty string `json:"base_qty" binding:"required"`
}

type emergencyKitRequest struct {
	Name       string                     `json:"name" binding:"required"`
	LocationID string                     `json:"location_id" binding:"required"`
	Note       *string                    `json:"note"`
	IsActive   *bool                      `json:"is_active"`
	Items      *[]emergencyKitItemRequest `json:"items"`
}

// insertKitItems replaces the kit's contents (delete + insert) inside the
// request transaction; ok=false when a response is already written.
func insertKitItems(c *gin.Context, q *sqlcgen.Queries, kit sqlcgen.InventoryEmergencyKit, items []emergencyKitItemRequest) bool {
	if err := q.DeleteEmergencyKitItems(c.Request.Context(), kit.ID); err != nil {
		respondInternalError(c, err)
		return false
	}
	for _, it := range items {
		itemID, okI := parseUUID(it.ItemID)
		if !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item"})
			return false
		}
		qty, err := parseNumeric(it.BaseQty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
			return false
		}
		if _, err := q.GetInventoryItemByID(c.Request.Context(), itemID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item"})
				return false
			}
			respondInternalError(c, err)
			return false
		}
		if _, err := q.InsertEmergencyKitItem(c.Request.Context(), sqlcgen.InsertEmergencyKitItemParams{
			CompanyID: kit.CompanyID, MerchantID: kit.MerchantID,
			KitID: kit.ID, ItemID: itemID, BaseQty: qty, CreatedBy: AuthUserID(c),
		}); err != nil {
			respondInternalError(c, err)
			return false
		}
	}
	return true
}

// CreateEmergencyKitHandler godoc
// @Summary Create an emergency kit with its standard contents
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param request body emergencyKitRequest true "Kit"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /merchants/{id}/emergency-kits [post]
func CreateEmergencyKitHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermEmergencyUse, merchantID) {
		return
	}
	var req emergencyKitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	locID, okL := parseUUID(req.LocationID)
	if !okL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid location_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, ok := loadMerchantRoom(c, q, locID, merchantID); !ok {
		return
	}
	kit, err := q.InsertEmergencyKit(c.Request.Context(), sqlcgen.InsertEmergencyKitParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		Name: req.Name, LocationID: locID, Note: nargText(req.Note),
		IsActive: nargBool(req.IsActive), CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if req.Items != nil && !insertKitItems(c, q, kit, *req.Items) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kitJSON(kit), "meta": gin.H{}})
}

// ListEmergencyKitsHandler godoc
// @Summary List the merchant's emergency kits
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/emergency-kits [get]
func ListEmergencyKitsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermEmergencyUse, merchantID) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListEmergencyKits(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, kitJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// emergencyKitReportItem is one row of the periodic check report: the kit's
// standard quantity, the available saldo and the nearest expiry in stock.
type emergencyKitReportItem struct {
	ItemID        pgtype.UUID
	ItemName      string
	BaseQty       pgtype.Numeric
	OnHand        pgtype.Numeric
	NearestExpiry pgtype.Date
}

// emergencyKitReport builds the §7.2 #4 periodic check report body.
func emergencyKitReport(c *gin.Context, q *sqlcgen.Queries, kit sqlcgen.InventoryEmergencyKit) (gin.H, bool) {
	items, err := q.ListEmergencyKitItems(c.Request.Context(), kit.ID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	ids := make([]pgtype.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ItemID)
	}
	expiries := map[pgtype.UUID]pgtype.Date{}
	if len(ids) > 0 {
		rows, err := q.ListKitItemExpiry(c.Request.Context(), sqlcgen.ListKitItemExpiryParams{
			MerchantID: kit.MerchantID, LocationID: kit.LocationID, Column3: ids,
		})
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		for _, r := range rows {
			// A date scanned into interface{} arrives as pgtype.Date or
			// time.Time depending on the driver description path — accept both.
			switch d := r.NearestExpiry.(type) {
			case pgtype.Date:
				expiries[r.ItemID] = d
			case time.Time:
				expiries[r.ItemID] = pgtype.Date{Time: d, Valid: true}
			}
		}
	}
	data := make([]gin.H, 0, len(items))
	for _, it := range items {
		sum, err := q.SumAvailableByItemLocation(c.Request.Context(), sqlcgen.SumAvailableByItemLocationParams{
			MerchantID: kit.MerchantID, LocationID: kit.LocationID, ItemID: it.ItemID,
		})
		if err != nil {
			respondInternalError(c, err)
			return nil, false
		}
		data = append(data, gin.H{
			"item_id":        it.ItemID.String(),
			"item_name":      it.ItemName,
			"base_qty":       numericExact(it.BaseQty),
			"on_hand":        numericExact(sum),
			"nearest_expiry": dateOrNil(expiries[it.ItemID]),
		})
	}
	var activeSeal gin.H
	if seal, err := q.GetActiveSealByKit(c.Request.Context(), kit.ID); err == nil {
		txTime, errT := q.TransactionTime(c.Request.Context())
		if errT != nil {
			respondInternalError(c, errT)
			return nil, false
		}
		ageDays := int64(txTime.Time.Sub(seal.SealedAt.Time) / (24 * time.Hour))
		if ageDays < 0 {
			ageDays = 0
		}
		activeSeal = sealJSON(seal)
		activeSeal["age_days"] = ageDays
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return nil, false
	}
	body := kitJSON(kit)
	body["items"] = data
	body["active_seal"] = activeSeal
	return body, true
}

// GetEmergencyKitHandler godoc
// @Summary Periodic check report of one emergency kit (contents, on-hand, nearest expiry, active seal age)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Kit UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /emergency-kits/{id} [get]
func GetEmergencyKitHandler(c *gin.Context) {
	kitID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	kit, ok := loadEmergencyKit(c, q, kitID)
	if !ok {
		return
	}
	body, ok := emergencyKitReport(c, q, kit)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": body, "meta": gin.H{}})
}

// UpdateEmergencyKitHandler godoc
// @Summary Update an emergency kit (partial; items replace the contents when present)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Kit UUID"
// @Param request body emergencyKitRequest true "Kit fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /emergency-kits/{id} [patch]
func UpdateEmergencyKitHandler(c *gin.Context) {
	kitID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	kit, ok := loadEmergencyKit(c, q, kitID)
	if !ok {
		return
	}
	var req emergencyKitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := q.UpdateEmergencyKit(c.Request.Context(), sqlcgen.UpdateEmergencyKitParams{
		ID:        kitID,
		Name:      nargText(&req.Name),
		Note:      nargText(req.Note),
		IsActive:  nargBool(req.IsActive),
		UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if req.Items != nil && !insertKitItems(c, q, kit, *req.Items) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": kitJSON(updated), "meta": gin.H{}})
}

type createSealRequest struct {
	SealNo         string  `json:"seal_no" binding:"required"`
	Note           *string `json:"note"`
	OverrideReason *string `json:"override_reason"`
}

// CreateSealHandler godoc
// @Summary Seal an emergency kit after refill (blocks while a seal is active or the refill is incomplete without an override reason)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Kit UUID"
// @Param request body createSealRequest true "Seal"
// @Success 201 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /emergency-kits/{id}/seals [post]
func CreateSealHandler(c *gin.Context) {
	kitID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	kit, ok := loadEmergencyKit(c, q, kitID)
	if !ok {
		return
	}
	var req createSealRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// A kit holds at most one active seal (partial UNIQUE index in the
	// migration guards the same rule).
	if _, err := q.GetActiveSealByKit(c.Request.Context(), kit.ID); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "kit already sealed"})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		respondInternalError(c, err)
		return
	}
	// Refill validation: every kit item needs its standard quantity available
	// at the kit location; continuing with a shortage requires a reason, which
	// is recorded in the seal note (spec §7.2 #4c).
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	override := ""
	if req.OverrideReason != nil {
		override = strings.TrimSpace(*req.OverrideReason)
	}
	items, err := q.ListEmergencyKitItems(c.Request.Context(), kit.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, it := range items {
		sum, err := q.SumAvailableByItemLocation(c.Request.Context(), sqlcgen.SumAvailableByItemLocationParams{
			MerchantID: kit.MerchantID, LocationID: kit.LocationID, ItemID: it.ItemID,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		have, errH := numericToMilli(sum)
		want, errW := numericToMilli(it.BaseQty)
		if errH != nil || errW != nil {
			respondInternalError(c, errors.New("kit quantity outside numeric(14,3) scale"))
			return
		}
		if have < want {
			if override == "" {
				c.JSON(http.StatusConflict, gin.H{"error": "kit refill incomplete"})
				return
			}
			note = strings.TrimSpace(note + " | override: " + override)
			break
		}
	}
	txTime, err := q.TransactionTime(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	seal, err := q.InsertEmergencyKitSeal(c.Request.Context(), sqlcgen.InsertEmergencyKitSealParams{
		CompanyID: kit.CompanyID, MerchantID: kit.MerchantID,
		KitID: kit.ID, SealNo: req.SealNo, SealedAt: txTime,
		SealedBy: AuthUserID(c), Note: pgText(note),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": sealJSON(seal), "meta": gin.H{}})
}

type breakSealLine struct {
	ItemID    string   `json:"item_id" binding:"required"`
	Qty       string   `json:"qty" binding:"required"`
	SerialIDs []string `json:"serial_ids"`
}

type breakSealRequest struct {
	Reason string          `json:"reason" binding:"required"`
	Lines  []breakSealLine `json:"lines"`
}

// BreakSealHandler godoc
// @Summary Break the active seal and issue stock per line through the wizard (ref_type emergency_kit)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Kit UUID"
// @Param sid path string true "Seal UUID"
// @Param request body breakSealRequest true "Reason and issued lines"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /emergency-kits/{id}/seals/{sid}/break [post]
func BreakSealHandler(c *gin.Context) {
	kitID, okK := parseUUID(c.Param("id"))
	sealID, okS := parseUUID(c.Param("sid"))
	if !okK || !okS {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	kit, ok := loadEmergencyKit(c, q, kitID)
	if !ok {
		return
	}
	seal, err := q.GetEmergencyKitSealByID(c.Request.Context(), sealID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && seal.KitID != kit.ID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "seal not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if seal.BrokenAt.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "seal already broken"})
		return
	}
	var req breakSealRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason required"})
		return
	}
	// Parse every line BEFORE the first write: unknown items and bad
	// quantities reject the whole wizard with 400 (RunRequestTx would also
	// roll back, but the guards stay ahead of the ledger).
	type parsedLine struct {
		itemID  pgtype.UUID
		qty     pgtype.Numeric
		serials []string
		item    sqlcgen.InventoryItem
	}
	lines := make([]parsedLine, 0, len(req.Lines))
	for _, l := range req.Lines {
		itemID, okI := parseUUID(l.ItemID)
		if !okI {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item"})
			return
		}
		qty, err := parseNumeric(l.Qty)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quantity"})
			return
		}
		item, err := q.GetInventoryItemByID(c.Request.Context(), itemID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item"})
				return
			}
			respondInternalError(c, err)
			return
		}
		serials := []string{}
		if item.TrackSerial {
			cleaned, okS := cleanSerialNos(c, l.SerialIDs)
			if !okS {
				return
			}
			if len(cleaned) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "serial required"})
				return
			}
			serials = cleaned
		}
		lines = append(lines, parsedLine{itemID: itemID, qty: qty, serials: serials, item: item})
	}
	txTime, err := q.TransactionTime(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// The break moves stock through the locked ledger; every issue references
	// the seal (ref_type emergency_kit). A shortage rolls the whole request
	// back (RunRequestTx) after respondStockError wrote the 409.
	for _, l := range lines {
		if _, err := issueStock(c.Request.Context(), q, kit.MerchantID, kit.CompanyID, AuthUserID(c),
			kit.LocationID, l.itemID, l.qty, txTime.Time,
			stockRef{RefType: "emergency_kit", RefID: seal.ID}, pgtype.UUID{}, req.Reason); err != nil {
			if !respondStockError(c, err) {
				respondInternalError(c, err)
			}
			return
		}
		if l.item.TrackSerial {
			if err := enforceSerialIssue(c.Request.Context(), q, kit.MerchantID, kit.LocationID,
				l.itemID, l.serials, "issued", stockRef{RefType: "emergency_kit", RefID: seal.ID}); err != nil {
				if !respondIdentityError(c, err) {
					respondInternalError(c, err)
				}
				return
			}
		}
	}
	broken, err := q.BreakEmergencyKitSeal(c.Request.Context(), sqlcgen.BreakEmergencyKitSealParams{
		ID: seal.ID, BrokenAt: txTime, BrokenBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sealJSON(broken), "meta": gin.H{}})
}
