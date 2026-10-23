// internal/server/clinical_nutrition.go
// CPPT Fase 1 (spec 2026-10-21-cppt-rajal-fase1-design §4, §9): per-merchant
// diet type catalog and revisable nutrition orders (rawat jalan only).
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

func RegisterClinicalNutritionRoutes(rg *gin.RouterGroup) {
	rg.GET("/clinical/diet-types", ListDietTypesHandler)
	rg.POST("/clinical/diet-types", CreateDietTypeHandler)
	rg.POST("/clinical/nutrition-orders", CreateNutritionOrderHandler)
	rg.GET("/clinical/nutrition-orders", AccessLog("nutrition_order", "view", "admission_id"), ListNutritionOrdersHandler)
	rg.PATCH("/clinical/nutrition-orders/:id", UpdateNutritionOrderHandler)
}

var validNutritionRoutes = map[string]bool{"oral": true, "ngt": true, "parenteral": true}
var validNutritionTextures = map[string]bool{"biasa": true, "lunak": true, "lunak_saring": true, "cair": true, "pantang": true}

func nutritionEnumInvalid(c *gin.Context, field string) bool {
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value for " + field})
	return false
}

// dietTypeJSON maps a created diet type row to the snake_case contract shape.
func dietTypeJSON(d sqlcgen.CoreDietType) gin.H {
	return gin.H{
		"id":              uuidJSON(d.ID),
		"code":            d.Code,
		"name":            d.Name,
		"default_calorie": numericJSON(d.DefaultCalorie),
		"is_active":       d.IsActive,
	}
}

// ListDietTypesHandler godoc
// @Summary List active diet types of the merchant
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Success 200 {object} apiResponse
// @Router /clinical/diet-types [get]
func ListDietTypesHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	rows, err := sqlcgen.New(TxFromContext(c)).ListActiveDietTypes(c.Request.Context(), merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, gin.H{
			"id":              uuidJSON(r.ID),
			"code":            r.Code,
			"name":            r.Name,
			"default_calorie": numericJSON(r.DefaultCalorie),
			"is_active":       r.IsActive,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type createDietTypeRequest struct {
	Code           string   `json:"code"`
	Name           string   `json:"name"`
	DefaultCalorie *float64 `json:"default_calorie"`
}

// CreateDietTypeHandler godoc
// @Summary Create a diet type in the merchant catalog
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createDietTypeRequest true "Diet type data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/diet-types [post]
func CreateDietTypeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	var req createDietTypeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	code, name := strings.TrimSpace(req.Code), strings.TrimSpace(req.Name)
	if code == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kode dan nama diet wajib"})
		return
	}
	created, err := sqlcgen.New(TxFromContext(c)).CreateDietType(c.Request.Context(),
		sqlcgen.CreateDietTypeParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, Code: code, Name: name,
			DefaultCalorie: numericFromFloat(req.DefaultCalorie), CreatedBy: AuthUserID(c),
		})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "kode diet sudah ada"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": dietTypeJSON(created), "meta": gin.H{}})
}

type createNutritionOrderRequest struct {
	AdmissionID         string   `json:"admission_id" binding:"required"`
	DietTypeID          string   `json:"diet_type_id" binding:"required"`
	Texture             *string  `json:"texture"`
	Route               string   `json:"route" binding:"required"`
	CalorieTarget       *float64 `json:"calorie_target"`
	RestrictionNote     *string  `json:"restriction_note"`
	OrderingPhysicianID *string  `json:"ordering_physician_id"`
}

// nutritionOrderJSON maps the order row (with diet join) to the snake_case
// contract shape (pattern admissionJSON). The single-order sqlcgen row is
// structurally identical, so it converts directly.
func nutritionOrderJSON(o sqlcgen.ListNutritionOrdersByAdmissionWithDietRow) gin.H {
	return gin.H{
		"id":                    uuidJSON(o.ID),
		"company_id":            uuidJSON(o.CompanyID),
		"merchant_id":           uuidJSON(o.MerchantID),
		"admission_id":          uuidJSON(o.AdmissionID),
		"ordering_physician_id": uuidJSON(o.OrderingPhysicianID),
		"ordered_by":            uuidJSON(o.OrderedBy),
		"diet_type_id":          uuidJSON(o.DietTypeID),
		"diet_type_code":        o.DietTypeCode,
		"diet_type_name":        o.DietTypeName,
		"texture":               textJSON(o.Texture),
		"route":                 o.Route,
		"calorie_target":        numericJSON(o.CalorieTarget),
		"restriction_note":      textJSON(o.RestrictionNote),
		"status":                o.Status,
		"started_at":            tsJSON(o.StartedAt),
		"ended_at":              tsJSON(o.EndedAt),
		"created_at":            tsJSON(o.CreatedAt),
		"created_by":            uuidJSON(o.CreatedBy),
		"updated_at":            tsJSON(o.UpdatedAt),
		"updated_by":            uuidJSON(o.UpdatedBy),
		"row_version":           o.RowVersion,
	}
}

// nutritionOrderWithDiet re-fetches an order with its diet join for responses.
func nutritionOrderWithDiet(c *gin.Context, q *sqlcgen.Queries, id pgtype.UUID) (sqlcgen.GetNutritionOrderWithDietRow, bool) {
	o, err := q.GetNutritionOrderWithDiet(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "order diet tidak ditemukan"})
		return sqlcgen.GetNutritionOrderWithDietRow{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.GetNutritionOrderWithDietRow{}, false
	}
	return o, true
}

// CreateNutritionOrderHandler godoc
// @Summary Order a diet for an outpatient admission (status active)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createNutritionOrderRequest true "Order data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/nutrition-orders [post]
func CreateNutritionOrderHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	var req createNutritionOrderRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if !validNutritionRoutes[req.Route] {
		nutritionEnumInvalid(c, "route")
		return
	}
	var texture pgtype.Text
	if req.Texture != nil {
		if !validNutritionTextures[*req.Texture] {
			nutritionEnumInvalid(c, "texture")
			return
		}
		texture = pgtype.Text{String: *req.Texture, Valid: true}
	}
	admissionID, okID := parseUUID(req.AdmissionID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	dietTypeID, okDiet := parseUUID(req.DietTypeID)
	if !okDiet {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid diet_type_id"})
		return
	}
	var physicianID pgtype.UUID
	if req.OrderingPhysicianID != nil {
		if physicianID, okID = parseUUID(*req.OrderingPhysicianID); !okID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ordering_physician_id"})
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	admission, err := q.GetAdmissionByID(ctx, admissionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && admission.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if admission.Status == "cancelled" || admission.AdmissionType != "outpatient" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cppt fase 1 hanya rawat jalan"})
		return
	}
	diet, err := q.GetDietType(ctx, dietTypeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && diet.MerchantID != merchantID) || (err == nil && !diet.IsActive) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jenis diet tidak aktif"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	var note pgtype.Text
	if req.RestrictionNote != nil {
		note = pgtype.Text{String: *req.RestrictionNote, Valid: true}
	}
	created, err := q.CreateNutritionOrder(ctx, sqlcgen.CreateNutritionOrderParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, AdmissionID: admissionID,
		OrderingPhysicianID: physicianID, OrderedBy: AuthUserID(c),
		DietTypeID: dietTypeID, Texture: texture, Route: req.Route,
		CalorieTarget: numericFromFloat(req.CalorieTarget), RestrictionNote: note,
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	withDiet, okJSON := nutritionOrderWithDiet(c, q, created.ID)
	if !okJSON {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": nutritionOrderJSON(sqlcgen.ListNutritionOrdersByAdmissionWithDietRow(withDiet)), "meta": gin.H{}})
}

// ListNutritionOrdersHandler godoc
// @Summary List nutrition orders of one admission (with diet type join)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Router /clinical/nutrition-orders [get]
func ListNutritionOrdersHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	admissionID, okID := parseUUID(c.Query("admission_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "admission_id query param is required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	if _, err := q.GetAdmissionByID(ctx, admissionID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission tidak ditemukan"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	rows, err := q.ListNutritionOrdersByAdmissionWithDiet(ctx, admissionID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		items = append(items, nutritionOrderJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

type updateNutritionOrderRequest struct {
	DietTypeID          *string  `json:"diet_type_id"`
	Texture             *string  `json:"texture"`
	Route               *string  `json:"route"`
	CalorieTarget       *float64 `json:"calorie_target"`
	RestrictionNote     *string  `json:"restriction_note"`
	OrderingPhysicianID *string  `json:"ordering_physician_id"`
	Status              *string  `json:"status"`
	// No binding: a missing row_version must yield the 409 below, not a 400.
	RowVersion *int64 `json:"row_version"`
}

// UpdateNutritionOrderHandler godoc
// @Summary Revise a nutrition order (optimistic locking; active -> completed|cancelled only)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Nutrition order UUID"
// @Param request body updateNutritionOrderRequest true "Order revision"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/nutrition-orders/{id} [patch]
func UpdateNutritionOrderHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalNutritionWrite, merchantID) {
		return
	}
	var req updateNutritionOrderRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if req.RowVersion == nil || *req.RowVersion < 0 || *req.RowVersion > int64(^uint32(0)>>1) {
		// row_version is mandatory and must match the stored value.
		c.JSON(http.StatusConflict, gin.H{"error": "data sudah berubah"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	// FOR UPDATE so a concurrent revision cannot slip between checks and write.
	o, err := q.GetNutritionOrderForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && o.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "order diet tidak ditemukan"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Status transition: only active -> completed|cancelled (spec §9);
	// completed/cancelled are terminal, so any status field on them → 409.
	status := o.Status
	if req.Status != nil {
		if o.Status != "active" || (*req.Status != "completed" && *req.Status != "cancelled") {
			c.JSON(http.StatusConflict, gin.H{"error": "status order tidak bisa diubah"})
			return
		}
		status = *req.Status
	}
	dietTypeID := o.DietTypeID
	if req.DietTypeID != nil {
		parsed, okDiet := parseUUID(*req.DietTypeID)
		if !okDiet {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid diet_type_id"})
			return
		}
		diet, err := q.GetDietType(ctx, parsed)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && diet.MerchantID != merchantID) || (err == nil && !diet.IsActive) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "jenis diet tidak aktif"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		dietTypeID = parsed
	}
	texture := o.Texture
	if req.Texture != nil {
		if !validNutritionTextures[*req.Texture] {
			nutritionEnumInvalid(c, "texture")
			return
		}
		texture = pgtype.Text{String: *req.Texture, Valid: true}
	}
	route := o.Route
	if req.Route != nil {
		if !validNutritionRoutes[*req.Route] {
			nutritionEnumInvalid(c, "route")
			return
		}
		route = *req.Route
	}
	calorieTarget := o.CalorieTarget
	if req.CalorieTarget != nil {
		calorieTarget = numericFromFloat(req.CalorieTarget)
	}
	restrictionNote := o.RestrictionNote
	if req.RestrictionNote != nil {
		restrictionNote = pgtype.Text{String: *req.RestrictionNote, Valid: true}
	}
	physicianID := o.OrderingPhysicianID
	if req.OrderingPhysicianID != nil {
		parsed, okPhy := parseUUID(*req.OrderingPhysicianID)
		if !okPhy {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ordering_physician_id"})
			return
		}
		physicianID = parsed
	}
	// ended_at is set only when the order leaves the active state.
	var endedAt pgtype.Timestamptz
	if status == "completed" || status == "cancelled" {
		endedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	if _, err := q.UpdateNutritionOrder(ctx, sqlcgen.UpdateNutritionOrderParams{
		ID: id, OrderingPhysicianID: physicianID, DietTypeID: dietTypeID,
		Texture: texture, Route: route, CalorieTarget: calorieTarget,
		RestrictionNote: restrictionNote, Status: status, EndedAt: endedAt,
		UpdatedBy: AuthUserID(c), RowVersion: int32(*req.RowVersion),
	}); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "data sudah berubah"})
		return
	} else if err != nil {
		respondInternalError(c, err)
		return
	}
	withDiet, okJSON := nutritionOrderWithDiet(c, q, id)
	if !okJSON {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": nutritionOrderJSON(sqlcgen.ListNutritionOrdersByAdmissionWithDietRow(withDiet)), "meta": gin.H{}})
}
