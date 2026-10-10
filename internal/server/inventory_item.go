// internal/server/inventory_item.go
// Company-level item master API (spec 2026-10-07-tariff-c-drug-master-pricing
// §3.3-§3.5): units dictionary, items with packaging and active ingredients.
package server

import (
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermInventoryItemManage guards the company-level inventory item master.
const PermInventoryItemManage = "inventory.item.manage"

// trackDefaultKinds are the item kinds whose track_batch/track_expiry default
// to true when the field is not sent (spec §3.3 "diisi API, bukan DB").
var trackDefaultKinds = map[string]bool{
	"drug": true, "vaccine": true, "medical_device": true, "reagent": true,
}

// parseScaled parses a non-negative decimal string ("100", "0.5", "1.25")
// with at most maxFrac fraction digits into a scaled integer (value·10^maxFrac).
func parseScaled(s string, maxFrac int) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") || len(frac) > maxFrac {
		return 0, false
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	for len(frac) < maxFrac {
		frac += "0"
	}
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// formatScaled renders a value scaled by 10^frac with exactly frac digits
// ("1000"@3 → "1.000", "100000"@3 → "100.000").
func formatScaled(v int64, frac int) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	s := strconv.FormatInt(v, 10)
	if frac == 0 {
		return sign + s
	}
	for len(s) <= frac {
		s = "0" + s
	}
	return sign + s[:len(s)-frac] + "." + s[len(s)-frac:]
}

// formatTrimDecimal renders a scaled value with trailing zeros trimmed
// ("100" → "100", "0.50" → "0.5").
func formatTrimDecimal(v int64, frac int) string {
	s := formatScaled(v, frac)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// scaledToNumeric builds an exact numeric from a value scaled by 10^frac.
func scaledToNumeric(v int64, frac int) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(v), Exp: int32(-frac), Valid: true}
}

// numericToScaled converts a numeric exactly into a value scaled by 10^frac.
func numericToScaled(n pgtype.Numeric, frac int) (int64, error) {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return 0, errors.New("numeric is not a finite value")
	}
	v := new(big.Int).Set(n.Int)
	exp := n.Exp + int32(frac)
	ten := big.NewInt(10)
	for ; exp > 0; exp-- {
		v.Mul(v, ten)
	}
	for ; exp < 0; exp++ {
		q, r := new(big.Int).QuoRem(v, ten, new(big.Int))
		if r.Sign() != 0 {
			return 0, errors.New("numeric has non-zero digits beyond the target scale")
		}
		v = q
	}
	if !v.IsInt64() {
		return 0, errors.New("numeric out of range")
	}
	return v.Int64(), nil
}

// respondItemError maps insert/update Postgres errors for the item master:
// unique → 409 uniqueMsg; trigger-raised check_violation (no constraint name)
// → 409 with the trigger message; named CHECK constraints → the contract
// messages; other CHECKs → 400 "invalid item field".
func respondItemError(c *gin.Context, err error, uniqueMsg string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "23505":
		c.JSON(http.StatusConflict, gin.H{"error": uniqueMsg})
		return true
	case "23514":
		switch pgErr.ConstraintName {
		case "":
			// RAISE EXCEPTION ... USING ERRCODE = 'check_violation' (triggers).
			c.JSON(http.StatusConflict, gin.H{"error": pgErr.Message})
		case "chk_item_kfa":
			c.JSON(http.StatusBadRequest, gin.H{"error": "drugs, vaccines and medical devices need a KFA code or a non-KFA reason"})
		case "chk_item_drug_class":
			c.JSON(http.StatusBadRequest, gin.H{"error": "drug_class is required for drugs and vaccines only"})
		case "chk_item_het_pair":
			c.JSON(http.StatusBadRequest, gin.H{"error": "het_amount and het_effective_from go together"})
		case "chk_item_lasa":
			c.JSON(http.StatusBadRequest, gin.H{"error": "LASA items need a note"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item field"})
		}
		return true
	}
	return false
}

// nargText/nargBool/nargInt4 convert optional request fields into pgtype
// narg values (invalid = leave the column unchanged).
func nargText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func nargBool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func nargInt4(i *int32) pgtype.Int4 {
	if i == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *i, Valid: true}
}

// textOrNull converts an optional request string: "" → NULL.
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func tsOrNil(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format("2006-01-02T15:04:05Z07:00")
}

func dateOrNil(d pgtype.Date) any {
	if !d.Valid {
		return nil
	}
	return d.Time.Format("2006-01-02")
}

func hetJSON(n pgtype.Numeric) any {
	cents, err := numericToCents(n)
	if err != nil {
		return nil
	}
	return formatCents(cents)
}

func minFractionJSON(n pgtype.Numeric) any {
	v, err := numericToScaled(n, 2)
	if err != nil {
		return nil
	}
	return formatTrimDecimal(v, 2)
}

// ingredientJSON renders one active ingredient; strength has 4 fraction
// digits, per_quantity 3.
func ingredientJSON(ing sqlcgen.InventoryItemIngredient) gin.H {
	var strength, perQuantity any
	if v, err := numericToScaled(ing.Strength, 4); err == nil {
		strength = formatScaled(v, 4)
	}
	if v, err := numericToScaled(ing.PerQuantity, 3); err == nil {
		perQuantity = formatScaled(v, 3)
	}
	return gin.H{
		"id":              ing.ID.String(),
		"ingredient_code": textOrNil(ing.IngredientCode),
		"ingredient_name": ing.IngredientName,
		"strength":        strength,
		"strength_unit":   textOrNil(ing.StrengthUnit),
		"per_quantity":    perQuantity,
		"per_unit":        textOrNil(ing.PerUnit),
	}
}

// itemDetailJSON renders the full item detail shape: all item columns plus
// base_unit, active packaging units (ordered by factor) and ingredients.
func itemDetailJSON(c *gin.Context, q *sqlcgen.Queries, item sqlcgen.InventoryItem) (gin.H, bool) {
	units, err := q.ListInventoryItemUnits(c.Request.Context(), item.ID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	ings, err := q.ListInventoryItemIngredients(c.Request.Context(), item.ID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	base, err := q.GetInventoryUnitByID(c.Request.Context(), item.BaseUnitID)
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	unitList := make([]gin.H, 0, len(units))
	for _, u := range units {
		unitList = append(unitList, gin.H{
			"id":                  u.ID.String(),
			"unit":                gin.H{"id": u.UnitID.String(), "code": u.UnitCode, "name": u.UnitName},
			"factor":              formatScaled(mustNumericScaled(u.Factor, 3), 3),
			"kfa_packaging_code":  textOrNil(u.KfaPackagingCode),
			"is_default_purchase": u.IsDefaultPurchase,
			"is_default_issue":    u.IsDefaultIssue,
			"is_active":           u.IsActive,
		})
	}
	ingredientList := make([]gin.H, 0, len(ings))
	for _, ing := range ings {
		ingredientList = append(ingredientList, ingredientJSON(ing))
	}
	return gin.H{
		"id":                 item.ID.String(),
		"company_id":         item.CompanyID.String(),
		"code":               item.Code,
		"name":               item.Name,
		"item_kind":          item.ItemKind,
		"pricing_category":   item.PricingCategory,
		"base_unit_id":       item.BaseUnitID.String(),
		"base_unit":          gin.H{"id": base.ID.String(), "code": base.Code, "name": base.Name},
		"kfa_code":           textOrNil(item.KfaCode),
		"kfa_pov_code":       textOrNil(item.KfaPovCode),
		"non_kfa":            item.NonKfa,
		"non_kfa_reason":     textOrNil(item.NonKfaReason),
		"nie":                textOrNil(item.Nie),
		"manufacturer":       textOrNil(item.Manufacturer),
		"dosage_form":        textOrNil(item.DosageForm),
		"drug_class":         textOrNil(item.DrugClass),
		"is_oot":             item.IsOot,
		"is_fornas":          item.IsFornas,
		"fornas_period":      textOrNil(item.FornasPeriod),
		"is_high_alert":      item.IsHighAlert,
		"is_lasa":            item.IsLasa,
		"lasa_note":          textOrNil(item.LasaNote),
		"is_generic":         item.IsGeneric,
		"het_amount":         hetJSON(item.HetAmount),
		"het_effective_from": dateOrNil(item.HetEffectiveFrom),
		"min_fraction":       minFractionJSON(item.MinFraction),
		"no_crush":           item.NoCrush,
		"bud_after_open_hours": func() any {
			if !item.BudAfterOpenHours.Valid {
				return nil
			}
			return item.BudAfterOpenHours.Int32
		}(),
		"bud_after_reconstitution_hours": func() any {
			if !item.BudAfterReconstitutionHours.Valid {
				return nil
			}
			return item.BudAfterReconstitutionHours.Int32
		}(),
		"is_single_dose": item.IsSingleDose,
		"storage_temp":   item.StorageTemp,
		"track_batch":    item.TrackBatch,
		"track_expiry":   item.TrackExpiry,
		"is_active":      item.IsActive,
		"created_at":     tsOrNil(item.CreatedAt),
		"created_by":     uuidOrNil(item.CreatedBy),
		"updated_at":     tsOrNil(item.UpdatedAt),
		"updated_by":     uuidOrNil(item.UpdatedBy),
		"deleted_at":     tsOrNil(item.DeletedAt),
		"deleted_by":     uuidOrNil(item.DeletedBy),
		"units":          unitList,
		"ingredients":    ingredientList,
		"row_version":    item.RowVersion,
	}, true
}

// mustNumericScaled is numericToScaled with a 0 fallback for render paths.
func mustNumericScaled(n pgtype.Numeric, frac int) int64 {
	v, err := numericToScaled(n, frac)
	if err != nil {
		return 0
	}
	return v
}

// loadInventoryItem fetches a live item (404 when missing under RLS).
func loadInventoryItem(c *gin.Context, q *sqlcgen.Queries, rawID string) (sqlcgen.InventoryItem, bool) {
	id, ok := parseUUID(rawID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return sqlcgen.InventoryItem{}, false
	}
	item, err := q.GetInventoryItemByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return sqlcgen.InventoryItem{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.InventoryItem{}, false
	}
	return item, true
}

type itemIngredientInput struct {
	IngredientCode string `json:"ingredient_code"`
	IngredientName string `json:"ingredient_name"`
	Strength       string `json:"strength"`
	StrengthUnit   string `json:"strength_unit"`
	PerQuantity    string `json:"per_quantity"`
	PerUnit        string `json:"per_unit"`
}

// parseItemIngredients validates the ingredient array (strength ≤ 4 fraction
// digits and > 0, per_quantity ≤ 3 fraction digits and > 0, default "1").
func parseItemIngredients(c *gin.Context, companyID, itemID pgtype.UUID, ings []itemIngredientInput) ([]sqlcgen.InsertInventoryItemIngredientParams, bool) {
	rows := make([]sqlcgen.InsertInventoryItemIngredientParams, 0, len(ings))
	for i, ing := range ings {
		if strings.TrimSpace(ing.IngredientName) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ingredient_name is required"})
			return nil, false
		}
		var strength pgtype.Numeric
		if ing.Strength != "" {
			v, ok := parseScaled(ing.Strength, 4)
			if !ok || v <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ingredient field"})
				return nil, false
			}
			strength = scaledToNumeric(v, 4)
		}
		pq := "1"
		if ing.PerQuantity != "" {
			pq = ing.PerQuantity
		}
		v, ok := parseScaled(pq, 3)
		if !ok || v <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ingredient field"})
			return nil, false
		}
		var code, strengthUnit, perUnit pgtype.Text
		if ing.IngredientCode != "" {
			code = pgtype.Text{String: ing.IngredientCode, Valid: true}
		}
		if ing.StrengthUnit != "" {
			strengthUnit = pgtype.Text{String: ing.StrengthUnit, Valid: true}
		}
		if ing.PerUnit != "" {
			perUnit = pgtype.Text{String: ing.PerUnit, Valid: true}
		}
		rows = append(rows, sqlcgen.InsertInventoryItemIngredientParams{
			CompanyID: companyID, ItemID: itemID,
			IngredientCode: code, IngredientName: ing.IngredientName,
			Strength: strength, StrengthUnit: strengthUnit,
			PerQuantity: scaledToNumeric(v, 3), PerUnit: perUnit,
			SortOrder: int32(i),
		})
	}
	return rows, true
}

// RegisterInventoryRoutes wires the inventory item master endpoints under the
// locked (authed) group; everything is company-level and guarded by
// inventory.item.manage except the read-only units dictionary.
func RegisterInventoryRoutes(rg *gin.RouterGroup) {
	rg.GET("/inventory/units", ListInventoryUnitsHandler)
	rg.POST("/inventory/items", CreateInventoryItemHandler)
	rg.GET("/inventory/items", ListInventoryItemsHandler)
	rg.GET("/inventory/items/:id", GetInventoryItemHandler)
	rg.PATCH("/inventory/items/:id", UpdateInventoryItemHandler)
	rg.DELETE("/inventory/items/:id", DeleteInventoryItemHandler)
	rg.PUT("/inventory/items/:id/ingredients", PutInventoryItemIngredientsHandler)
	rg.POST("/inventory/items/:id/units", CreateInventoryItemUnitHandler)
	rg.PATCH("/inventory/item-units/:id", UpdateInventoryItemUnitHandler)
	rg.GET("/merchants/:id/formulary", ListMerchantFormularyHandler)
	rg.POST("/merchants/:id/formulary", AddMerchantFormularyHandler)
	rg.PATCH("/formulary/:id", UpdateFormularyHandler)
	rg.PUT("/formulary/:id/reference-price", SetFormularyReferencePriceHandler)
}

// ListInventoryUnitsHandler godoc
// @Summary List platform units
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /inventory/units [get]
func ListInventoryUnitsHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	units, err := q.ListInventoryUnits(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(units))
	for _, u := range units {
		data = append(data, gin.H{
			"id":        u.ID.String(),
			"code":      u.Code,
			"name":      u.Name,
			"ucum_code": textOrNil(u.UcumCode),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type createInventoryItemRequest struct {
	Code                        string                `json:"code" binding:"required"`
	Name                        string                `json:"name" binding:"required"`
	ItemKind                    string                `json:"item_kind" binding:"required"`
	PricingCategory             string                `json:"pricing_category" binding:"required"`
	BaseUnitID                  string                `json:"base_unit_id" binding:"required"`
	KfaCode                     string                `json:"kfa_code"`
	KfaPovCode                  string                `json:"kfa_pov_code"`
	NonKfa                      *bool                 `json:"non_kfa"`
	NonKfaReason                string                `json:"non_kfa_reason"`
	Nie                         string                `json:"nie"`
	Manufacturer                string                `json:"manufacturer"`
	DosageForm                  string                `json:"dosage_form"`
	DrugClass                   string                `json:"drug_class"`
	IsOot                       *bool                 `json:"is_oot"`
	IsFornas                    *bool                 `json:"is_fornas"`
	FornasPeriod                string                `json:"fornas_period"`
	IsHighAlert                 *bool                 `json:"is_high_alert"`
	IsLasa                      *bool                 `json:"is_lasa"`
	LasaNote                    string                `json:"lasa_note"`
	IsGeneric                   *bool                 `json:"is_generic"`
	HetAmount                   string                `json:"het_amount"`
	HetEffectiveFrom            string                `json:"het_effective_from"`
	MinFraction                 string                `json:"min_fraction"`
	NoCrush                     *bool                 `json:"no_crush"`
	BudAfterOpenHours           *int32                `json:"bud_after_open_hours"`
	BudAfterReconstitutionHours *int32                `json:"bud_after_reconstitution_hours"`
	IsSingleDose                *bool                 `json:"is_single_dose"`
	StorageTemp                 string                `json:"storage_temp"`
	TrackBatch                  *bool                 `json:"track_batch"`
	TrackExpiry                 *bool                 `json:"track_expiry"`
	IsActive                    *bool                 `json:"is_active"`
	Ingredients                 []itemIngredientInput `json:"ingredients"`
}

// CreateInventoryItemHandler godoc
// @Summary Create a company-level item with base unit and ingredients
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createInventoryItemRequest true "Item data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inventory/items [post]
func CreateInventoryItemHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	var req createInventoryItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	baseUnitID, ok := parseUUID(req.BaseUnitID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_unit_id"})
		return
	}
	if _, err := q.GetInventoryUnitByID(c.Request.Context(), baseUnitID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid base_unit_id"})
			return
		}
		respondInternalError(c, err)
		return
	}
	var hetAmount pgtype.Numeric
	if req.HetAmount != "" {
		cents, err := parseCents(req.HetAmount)
		if err != nil || cents <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid het_amount"})
			return
		}
		hetAmount = centsToNumeric(cents)
	}
	var hetFrom pgtype.Date
	if req.HetEffectiveFrom != "" {
		t, ok := parseAPIDate(req.HetEffectiveFrom)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid het_effective_from"})
			return
		}
		hetFrom = pgtype.Date{Time: t, Valid: true}
	}
	minFraction := "1"
	if req.MinFraction != "" {
		minFraction = req.MinFraction
	}
	mf, ok := parseScaled(minFraction, 2)
	if !ok || (mf != 100 && mf != 50 && mf != 25) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid min_fraction"})
		return
	}
	storageTemp := req.StorageTemp
	if storageTemp == "" {
		storageTemp = "room"
	}
	// API default: stock-tracking kinds track batch/expiry unless sent.
	trackBatch := trackDefaultKinds[req.ItemKind]
	if req.TrackBatch != nil {
		trackBatch = *req.TrackBatch
	}
	trackExpiry := trackDefaultKinds[req.ItemKind]
	if req.TrackExpiry != nil {
		trackExpiry = *req.TrackExpiry
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	var drugClass pgtype.Text
	if req.DrugClass != "" {
		drugClass = pgtype.Text{String: req.DrugClass, Valid: true}
	}
	nonKfa := false
	if req.NonKfa != nil {
		nonKfa = *req.NonKfa
	}
	item, err := q.CreateInventoryItem(c.Request.Context(), sqlcgen.CreateInventoryItemParams{
		CompanyID: AuthCompanyID(c), Code: req.Code, Name: req.Name,
		ItemKind: req.ItemKind, PricingCategory: req.PricingCategory, BaseUnitID: baseUnitID,
		KfaCode: textOrNull(req.KfaCode), KfaPovCode: textOrNull(req.KfaPovCode),
		NonKfa: nonKfa, NonKfaReason: textOrNull(req.NonKfaReason), Nie: textOrNull(req.Nie),
		Manufacturer: textOrNull(req.Manufacturer), DosageForm: textOrNull(req.DosageForm),
		DrugClass: drugClass, IsOot: boolArg(req.IsOot), IsFornas: boolArg(req.IsFornas),
		FornasPeriod: textOrNull(req.FornasPeriod), IsHighAlert: boolArg(req.IsHighAlert),
		IsLasa: boolArg(req.IsLasa), LasaNote: textOrNull(req.LasaNote),
		IsGeneric: boolArg(req.IsGeneric), HetAmount: hetAmount, HetEffectiveFrom: hetFrom,
		MinFraction: scaledToNumeric(mf, 2), NoCrush: boolArg(req.NoCrush),
		BudAfterOpenHours:           nargInt4(req.BudAfterOpenHours),
		BudAfterReconstitutionHours: nargInt4(req.BudAfterReconstitutionHours),
		IsSingleDose:                boolArg(req.IsSingleDose), StorageTemp: storageTemp,
		TrackBatch: trackBatch, TrackExpiry: trackExpiry, IsActive: isActive,
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		if respondItemError(c, err, "item code already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	// Base unit packaging row: factor 1, default purchase and issue.
	if _, err := q.InsertInventoryItemUnit(c.Request.Context(), sqlcgen.InsertInventoryItemUnitParams{
		CompanyID: AuthCompanyID(c), ItemID: item.ID, UnitID: baseUnitID,
		Factor: scaledToNumeric(1000, 3), CreatedBy: AuthUserID(c),
		IsDefaultPurchase: true, IsDefaultIssue: true,
	}); err != nil {
		if respondItemError(c, err, "item code already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	ings, ok := parseItemIngredients(c, AuthCompanyID(c), item.ID, req.Ingredients)
	if !ok {
		return
	}
	for _, row := range ings {
		if _, err := q.InsertInventoryItemIngredient(c.Request.Context(), row); err != nil {
			if respondItemError(c, err, "item code already exists") {
				return
			}
			respondInternalError(c, err)
			return
		}
	}
	detail, ok := itemDetailJSON(c, q, item)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": detail, "meta": gin.H{}})
}

func boolArg(b *bool) bool {
	return b != nil && *b
}

// ListInventoryItemsHandler godoc
// @Summary List company items (ringkas)
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param q query string false "Filter by code or name"
// @Param item_kind query string false "Filter by item kind"
// @Param is_active query bool false "Filter by active flag"
// @Param limit query int false "Page size (max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /inventory/items [get]
func ListInventoryItemsHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	limit, offset := paginationParams(c)
	var itemKind pgtype.Text
	if v := c.Query("item_kind"); v != "" {
		itemKind = pgtype.Text{String: v, Valid: true}
	}
	var q pgtype.Text
	if v := c.Query("q"); v != "" {
		q = pgtype.Text{String: v, Valid: true}
	}
	var isActive pgtype.Bool
	switch c.Query("is_active") {
	case "true":
		isActive = pgtype.Bool{Bool: true, Valid: true}
	case "false":
		isActive = pgtype.Bool{Bool: false, Valid: true}
	}
	qry := sqlcgen.New(TxFromContext(c))
	items, err := qry.ListInventoryItems(c.Request.Context(), sqlcgen.ListInventoryItemsParams{
		CompanyID: AuthCompanyID(c), ItemKind: itemKind, IsActive: isActive, Q: q,
		RowOffset: offset, RowLimit: limit,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(items))
	for _, it := range items {
		data = append(data, gin.H{
			"id":               it.ID.String(),
			"code":             it.Code,
			"name":             it.Name,
			"item_kind":        it.ItemKind,
			"pricing_category": it.PricingCategory,
			"drug_class":       textOrNil(it.DrugClass),
			"base_unit_code":   it.BaseUnitCode,
			"kfa_code":         textOrNil(it.KfaCode),
			"non_kfa":          it.NonKfa,
			"het_amount":       hetJSON(it.HetAmount),
			"is_active":        it.IsActive,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetInventoryItemHandler godoc
// @Summary Get an item detail
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /inventory/items/{id} [get]
func GetInventoryItemHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadInventoryItem(c, q, c.Param("id"))
	if !ok {
		return
	}
	detail, ok := itemDetailJSON(c, q, item)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail, "meta": gin.H{}})
}

type updateInventoryItemRequest struct {
	Name                        *string `json:"name"`
	KfaCode                     *string `json:"kfa_code"`
	KfaPovCode                  *string `json:"kfa_pov_code"`
	NonKfa                      *bool   `json:"non_kfa"`
	NonKfaReason                *string `json:"non_kfa_reason"`
	Nie                         *string `json:"nie"`
	Manufacturer                *string `json:"manufacturer"`
	DosageForm                  *string `json:"dosage_form"`
	DrugClass                   *string `json:"drug_class"`
	IsOot                       *bool   `json:"is_oot"`
	IsFornas                    *bool   `json:"is_fornas"`
	FornasPeriod                *string `json:"fornas_period"`
	IsHighAlert                 *bool   `json:"is_high_alert"`
	IsLasa                      *bool   `json:"is_lasa"`
	LasaNote                    *string `json:"lasa_note"`
	IsGeneric                   *bool   `json:"is_generic"`
	HetAmount                   *string `json:"het_amount"`
	HetEffectiveFrom            *string `json:"het_effective_from"`
	MinFraction                 *string `json:"min_fraction"`
	NoCrush                     *bool   `json:"no_crush"`
	BudAfterOpenHours           *int32  `json:"bud_after_open_hours"`
	BudAfterReconstitutionHours *int32  `json:"bud_after_reconstitution_hours"`
	IsSingleDose                *bool   `json:"is_single_dose"`
	StorageTemp                 *string `json:"storage_temp"`
	TrackBatch                  *bool   `json:"track_batch"`
	TrackExpiry                 *bool   `json:"track_expiry"`
	IsActive                    *bool   `json:"is_active"`
	BaseUnitID                  *string `json:"base_unit_id"`
	RowVersion                  int64   `json:"row_version"`
}

// UpdateInventoryItemHandler godoc
// @Summary Update an item (optimistic locking on row_version)
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item UUID"
// @Param request body updateInventoryItemRequest true "Item fields"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inventory/items/{id} [patch]
func UpdateInventoryItemHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	var req updateInventoryItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RowVersion <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadInventoryItem(c, q, c.Param("id"))
	if !ok {
		return
	}
	if req.BaseUnitID != nil {
		newBase, okID := parseUUID(*req.BaseUnitID)
		if !okID || newBase != item.BaseUnitID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "base unit cannot be changed"})
			return
		}
	}
	if req.RowVersion != int64(item.RowVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "item changed, reload"})
		return
	}
	var hetAmount pgtype.Numeric
	if req.HetAmount != nil {
		cents, err := parseCents(*req.HetAmount)
		if err != nil || cents <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid het_amount"})
			return
		}
		hetAmount = centsToNumeric(cents)
	}
	var hetFrom pgtype.Date
	if req.HetEffectiveFrom != nil {
		t, okDate := parseAPIDate(*req.HetEffectiveFrom)
		if !okDate {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid het_effective_from"})
			return
		}
		hetFrom = pgtype.Date{Time: t, Valid: true}
	}
	var minFraction pgtype.Numeric
	if req.MinFraction != nil {
		mf, okScale := parseScaled(*req.MinFraction, 2)
		if !okScale || (mf != 100 && mf != 50 && mf != 25) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid min_fraction"})
			return
		}
		minFraction = scaledToNumeric(mf, 2)
	}
	updated, err := q.UpdateInventoryItem(c.Request.Context(), sqlcgen.UpdateInventoryItemParams{
		Name: nargText(req.Name), KfaCode: nargText(req.KfaCode), KfaPovCode: nargText(req.KfaPovCode),
		NonKfa: nargBool(req.NonKfa), NonKfaReason: nargText(req.NonKfaReason), Nie: nargText(req.Nie),
		Manufacturer: nargText(req.Manufacturer), DosageForm: nargText(req.DosageForm),
		DrugClass: nargText(req.DrugClass), IsOot: nargBool(req.IsOot), IsFornas: nargBool(req.IsFornas),
		FornasPeriod: nargText(req.FornasPeriod), IsHighAlert: nargBool(req.IsHighAlert),
		IsLasa: nargBool(req.IsLasa), LasaNote: nargText(req.LasaNote), IsGeneric: nargBool(req.IsGeneric),
		HetAmount: hetAmount, HetEffectiveFrom: hetFrom, MinFraction: minFraction,
		NoCrush: nargBool(req.NoCrush), BudAfterOpenHours: nargInt4(req.BudAfterOpenHours),
		BudAfterReconstitutionHours: nargInt4(req.BudAfterReconstitutionHours),
		IsSingleDose:                nargBool(req.IsSingleDose), StorageTemp: nargText(req.StorageTemp),
		TrackBatch: nargBool(req.TrackBatch), TrackExpiry: nargBool(req.TrackExpiry),
		IsActive: nargBool(req.IsActive), UpdatedBy: AuthUserID(c),
		ItemID: item.ID, ExpectedRowVersion: item.RowVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "item changed, reload"})
			return
		}
		if respondItemError(c, err, "item code already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	detail, ok := itemDetailJSON(c, q, updated)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail, "meta": gin.H{}})
}

// DeleteInventoryItemHandler godoc
// @Summary Soft-delete an item
// @Tags inventory
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inventory/items/{id} [delete]
func DeleteInventoryItemHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadInventoryItem(c, q, c.Param("id"))
	if !ok {
		return
	}
	n, err := q.SoftDeleteInventoryItem(c.Request.Context(), sqlcgen.SoftDeleteInventoryItemParams{
		ItemID: item.ID, DeletedBy: AuthUserID(c),
	})
	if err != nil {
		if respondItemError(c, err, "item code already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": item.ID.String()}, "meta": gin.H{}})
}

type putInventoryItemIngredientsRequest struct {
	Ingredients []itemIngredientInput `json:"ingredients" binding:"required"`
}

// PutInventoryItemIngredientsHandler godoc
// @Summary Replace all active ingredients of an item
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item UUID"
// @Param request body putInventoryItemIngredientsRequest true "Ingredients"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /inventory/items/{id}/ingredients [put]
func PutInventoryItemIngredientsHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	var req putInventoryItemIngredientsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadInventoryItem(c, q, c.Param("id"))
	if !ok {
		return
	}
	ings, ok := parseItemIngredients(c, AuthCompanyID(c), item.ID, req.Ingredients)
	if !ok {
		return
	}
	if _, err := q.DeleteInventoryItemIngredients(c.Request.Context(), item.ID); err != nil {
		respondInternalError(c, err)
		return
	}
	for _, row := range ings {
		if _, err := q.InsertInventoryItemIngredient(c.Request.Context(), row); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	detail, ok := itemDetailJSON(c, q, item)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail, "meta": gin.H{}})
}

type createInventoryItemUnitRequest struct {
	UnitID            string `json:"unit_id" binding:"required"`
	Factor            string `json:"factor" binding:"required"`
	KfaPackagingCode  string `json:"kfa_packaging_code"`
	IsDefaultPurchase *bool  `json:"is_default_purchase"`
	IsDefaultIssue    *bool  `json:"is_default_issue"`
}

// CreateInventoryItemUnitHandler godoc
// @Summary Add a packaging unit to an item
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item UUID"
// @Param request body createInventoryItemUnitRequest true "Packaging unit"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inventory/items/{id}/units [post]
func CreateInventoryItemUnitHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	var req createInventoryItemUnitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	factor, ok := parseScaled(req.Factor, 3)
	if !ok || factor <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factor"})
		return
	}
	unitID, okID := parseUUID(req.UnitID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unit_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	item, ok := loadInventoryItem(c, q, c.Param("id"))
	if !ok {
		return
	}
	if _, err := q.GetInventoryUnitByID(c.Request.Context(), unitID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unit_id"})
			return
		}
		respondInternalError(c, err)
		return
	}
	// Requested defaults release the item's previous defaults first.
	if req.IsDefaultPurchase != nil && *req.IsDefaultPurchase {
		if _, err := q.ClearInventoryItemDefaultPurchase(c.Request.Context(), item.ID); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if req.IsDefaultIssue != nil && *req.IsDefaultIssue {
		if _, err := q.ClearInventoryItemDefaultIssue(c.Request.Context(), item.ID); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if _, err := q.InsertInventoryItemUnit(c.Request.Context(), sqlcgen.InsertInventoryItemUnitParams{
		CompanyID: AuthCompanyID(c), ItemID: item.ID, UnitID: unitID,
		Factor: scaledToNumeric(factor, 3), KfaPackagingCode: textOrNull(req.KfaPackagingCode),
		IsDefaultPurchase: boolArg(req.IsDefaultPurchase), IsDefaultIssue: boolArg(req.IsDefaultIssue),
		CreatedBy: AuthUserID(c),
	}); err != nil {
		if respondItemError(c, err, "packaging unit already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	detail, ok := itemDetailJSON(c, q, item)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": detail, "meta": gin.H{}})
}

type updateInventoryItemUnitRequest struct {
	IsActive          *bool   `json:"is_active"`
	IsDefaultPurchase *bool   `json:"is_default_purchase"`
	IsDefaultIssue    *bool   `json:"is_default_issue"`
	KfaPackagingCode  *string `json:"kfa_packaging_code"`
}

// UpdateInventoryItemUnitHandler godoc
// @Summary Update a packaging unit's flags
// @Tags inventory
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Item unit UUID"
// @Param request body updateInventoryItemUnitRequest true "Unit fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /inventory/item-units/{id} [patch]
func UpdateInventoryItemUnitHandler(c *gin.Context) {
	if !RequireCompanyLevelPermission(c, PermInventoryItemManage) {
		return
	}
	var req updateInventoryItemUnitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	unitID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	unit, err := q.GetInventoryItemUnitByID(c.Request.Context(), unitID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item unit not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Requested defaults release the item's previous defaults first.
	if req.IsDefaultPurchase != nil && *req.IsDefaultPurchase {
		if _, err := q.ClearInventoryItemDefaultPurchase(c.Request.Context(), unit.ItemID); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if req.IsDefaultIssue != nil && *req.IsDefaultIssue {
		if _, err := q.ClearInventoryItemDefaultIssue(c.Request.Context(), unit.ItemID); err != nil {
			respondInternalError(c, err)
			return
		}
	}
	if _, err := q.UpdateInventoryItemUnit(c.Request.Context(), sqlcgen.UpdateInventoryItemUnitParams{
		IsActive: nargBool(req.IsActive), IsDefaultPurchase: nargBool(req.IsDefaultPurchase),
		IsDefaultIssue: nargBool(req.IsDefaultIssue), KfaPackagingCode: nargText(req.KfaPackagingCode),
		ItemUnitID: unitID,
	}); err != nil {
		if respondItemError(c, err, "packaging unit already exists") {
			return
		}
		respondInternalError(c, err)
		return
	}
	item, ok := loadInventoryItem(c, q, unit.ItemID.String())
	if !ok {
		return
	}
	detail, ok := itemDetailJSON(c, q, item)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail, "meta": gin.H{}})
}
