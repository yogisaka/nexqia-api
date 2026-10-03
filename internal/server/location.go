// internal/server/location.go
package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermLocationManage guards core.location CRUD (physical tree: site → building
// → wing/level → room → bed). Beds live inside this tree (spec
// 2026-10-01-a-physical-location-design §3), unlike the legacy core.bed table.
const PermLocationManage = "core.location.manage"

// RegisterLocationRoutes wires core.location CRUD, same locked group as
// RegisterDepartmentRoutes. List/create use the merchant from the path/body;
// get/update/delete use the existing row's merchant_id (department.go pattern).
func RegisterLocationRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/locations", ListLocationsHandler)
	rg.POST("/locations", CreateLocationHandler)
	rg.GET("/locations/:id", GetLocationHandler)
	rg.PATCH("/locations/:id", UpdateLocationHandler)
	rg.DELETE("/locations/:id", DeleteLocationHandler)
}

type createLocationRequest struct {
	MerchantID   string   `json:"merchant_id" binding:"required"`
	ParentID     string   `json:"parent_id"`
	Kind         string   `json:"kind" binding:"required"`
	Code         string   `json:"code" binding:"required"`
	Name         string   `json:"name" binding:"required"`
	Functions    []string `json:"functions"`
	ServiceClass string   `json:"service_class"`
	Capacity     *int32   `json:"capacity"`
	Latitude     *float64 `json:"latitude"`
	Longitude    *float64 `json:"longitude"`
	Status       string   `json:"status"`
	SortOrder    *int32   `json:"sort_order"`
}

// CreateLocationHandler godoc
// @Summary Create a location in the physical tree
// @Tags locations
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createLocationRequest true "Location data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /locations [post]
func CreateLocationHandler(c *gin.Context) {
	var req createLocationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	// Same cross-tenant IDOR rule as departments: check against the body's
	// merchant_id, not the X-Merchant-ID header.
	if !RequirePermissionForMerchant(c, PermLocationManage, merchantID) {
		return
	}
	parentID := pgtype.UUID{}
	if req.ParentID != "" {
		p, ok := parseUUID(req.ParentID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parent_id"})
			return
		}
		parentID = p
	}
	if req.Status == "" {
		req.Status = "active"
	}
	// functions is NOT NULL DEFAULT '{}' — an absent list must insert '{}',
	// not NULL.
	functions := req.Functions
	if functions == nil {
		functions = []string{}
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := q.CreateLocation(c.Request.Context(), sqlcgen.CreateLocationParams{
		CompanyID:    AuthCompanyID(c),
		MerchantID:   merchantID,
		ParentID:     parentID,
		Kind:         req.Kind,
		Code:         req.Code,
		Name:         req.Name,
		Functions:    functions,
		ServiceClass: optTextValue(req.ServiceClass),
		Capacity:     optInt4(req.Capacity),
		Latitude:     numericFromFloat(req.Latitude),
		Longitude:    numericFromFloat(req.Longitude),
		Status:       req.Status,
		SortOrder:    optInt32(req.SortOrder),
		CreatedBy:    AuthUserID(c),
	})
	if err != nil {
		if respondLocationError(c, err) {
			return
		}
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": locationEnriched(c, q, loc), "meta": gin.H{}})
}

// ListLocationsHandler godoc
// @Summary List a merchant's locations
// @Tags locations
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Param kind query string false "Filter by kind (site/building/wing/level/room/bed)"
// @Param function query string false "Filter by function (practice/nurse_station/counter/ward_room/treatment/support)"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /merchants/{id}/locations [get]
func ListLocationsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermLocationManage, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	var (
		rows []sqlcgen.CoreLocation
		err  error
	)
	// kind / function filters are mutually exclusive single-value filters.
	switch {
	case c.Query("kind") != "":
		rows, err = q.ListLocationsByMerchantKind(ctx, sqlcgen.ListLocationsByMerchantKindParams{
			MerchantID: merchantID, Kind: c.Query("kind"), Limit: limit, Offset: offset,
		})
	case c.Query("function") != "":
		rows, err = q.ListLocationsByMerchantFunction(ctx, sqlcgen.ListLocationsByMerchantFunctionParams{
			MerchantID: merchantID, Column2: c.Query("function"), Limit: limit, Offset: offset,
		})
	default:
		rows, err = q.ListLocationsByMerchant(ctx, sqlcgen.ListLocationsByMerchantParams{
			MerchantID: merchantID, Limit: limit, Offset: offset,
		})
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Paths and children counts come from the same full merchant list, computed
	// in Go — no recursive query per item.
	items := locationEnrichedList(ctx, q, merchantID, rows)
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetLocationHandler godoc
// @Summary Get a location by id
// @Tags locations
// @Produce json
// @Security BearerAuth
// @Param id path string true "Location UUID"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /locations/{id} [get]
func GetLocationHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := q.GetLocationByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermLocationManage, loc.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": locationEnriched(c, q, loc), "meta": gin.H{}})
}

type updateLocationRequest struct {
	ParentID     *string   `json:"parent_id"`
	Code         *string   `json:"code"`
	Name         *string   `json:"name"`
	Functions    *[]string `json:"functions"`
	ServiceClass *string   `json:"service_class"`
	Capacity     *int32    `json:"capacity"`
	Latitude     *float64  `json:"latitude"`
	Longitude    *float64  `json:"longitude"`
	Status       *string   `json:"status"`
	SortOrder    *int32    `json:"sort_order"`
}

// UpdateLocationHandler godoc
// @Summary Update a location (kind is immutable)
// @Tags locations
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Location UUID"
// @Param request body updateLocationRequest true "Location data"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /locations/{id} [patch]
func UpdateLocationHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetLocationByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermLocationManage, existing.MerchantID) {
		return
	}
	var req updateLocationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// kind is intentionally absent from the UPDATE — it cannot be changed.
	// nil fields keep the existing value; empty strings become NULL.
	parentID := existing.ParentID
	if req.ParentID != nil {
		if *req.ParentID == "" {
			parentID = pgtype.UUID{}
		} else {
			p, ok := parseUUID(*req.ParentID)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parent_id"})
				return
			}
			parentID = p
		}
	}
	functions := existing.Functions
	if req.Functions != nil {
		functions = *req.Functions
		// Draining a ward room that still holds active beds would orphan them.
		if existing.Kind == "room" &&
			containsString(existing.Functions, "ward_room") && !containsString(functions, "ward_room") {
			beds, err := q.CountActiveBedsByParent(c.Request.Context(), id)
			if err != nil {
				respondInternalError(c, err)
				return
			}
			if beds > 0 {
				c.JSON(http.StatusConflict, gin.H{"error": "room still has beds"})
				return
			}
		}
	}
	serviceClass := existing.ServiceClass
	if req.ServiceClass != nil {
		serviceClass = optTextValue(*req.ServiceClass)
	}
	capacity := existing.Capacity
	if req.Capacity != nil {
		capacity = pgtype.Int4{Int32: *req.Capacity, Valid: true}
	}
	latitude, longitude := existing.Latitude, existing.Longitude
	if req.Latitude != nil {
		latitude = numericFromFloat(req.Latitude)
	}
	if req.Longitude != nil {
		longitude = numericFromFloat(req.Longitude)
	}
	status := existing.Status
	if req.Status != nil {
		status = *req.Status
	}
	sortOrder := existing.SortOrder
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	loc, err := q.UpdateLocation(c.Request.Context(), sqlcgen.UpdateLocationParams{
		ID:           id,
		ParentID:     parentID,
		Code:         derefString(req.Code, existing.Code),
		Name:         derefString(req.Name, existing.Name),
		Functions:    functions,
		ServiceClass: serviceClass,
		Capacity:     capacity,
		Latitude:     latitude,
		Longitude:    longitude,
		Status:       status,
		SortOrder:    sortOrder,
		UpdatedBy:    AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		if respondLocationError(c, err) {
			return
		}
		abortInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": locationEnriched(c, q, loc), "meta": gin.H{}})
}

// DeleteLocationHandler godoc
// @Summary Soft-delete a location
// @Tags locations
// @Produce json
// @Security BearerAuth
// @Param id path string true "Location UUID"
// @Success 204 "No Content"
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /locations/{id} [delete]
func DeleteLocationHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetLocationByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "location not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermLocationManage, existing.MerchantID) {
		return
	}
	children, err := q.CountActiveChildrenByParent(c.Request.Context(), id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if children > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "location still has child locations"})
		return
	}
	if err := q.SoftDeleteLocation(c.Request.Context(), sqlcgen.SoftDeleteLocationParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		abortInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// respondLocationError maps location-specific Postgres errors to their API
// responses. It returns true when it wrote a response (409/400); otherwise the
// caller falls through to respondInternalError. Every branch uses
// AbortWithStatusJSON — a failed statement leaves the request transaction in
// Postgres' aborted state, so TenantMiddleware must roll back, not commit
// (otherwise it appends a second "failed to commit transaction" JSON body).
func respondLocationError(c *gin.Context, err error) bool {
	if isUniqueViolation(err) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "location code already exists"})
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		switch pgErr.ConstraintName {
		case "chk_location_functions_room":
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "functions are only for rooms"})
		case "chk_location_site_position":
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "site requires latitude and longitude"})
		default:
			if strings.HasPrefix(pgErr.ConstraintName, "chk_location_") {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid location data"})
			} else {
				// Trigger RAISE EXCEPTION messages ("bed must be inside a ward
				// room", "parent location not found", ...) carry no constraint
				// name — surface them verbatim.
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": pgErr.Message})
			}
		}
		return true
	}
	return false
}

// locationEnrichedList fetches the merchant's whole active tree once and
// annotates the page rows with path + children_count.
func locationEnrichedList(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, rows []sqlcgen.CoreLocation) []gin.H {
	all, err := q.ListAllLocationsByMerchant(ctx, merchantID)
	if err != nil {
		// Paths/counts are presentation data; fall back to blanks rather than
		// failing the whole listing.
		items := make([]gin.H, 0, len(rows))
		for _, loc := range rows {
			items = append(items, locationItem(loc, "", 0))
		}
		return items
	}
	paths := locationPaths(all)
	counts := locationChildCounts(all)
	items := make([]gin.H, 0, len(rows))
	for _, loc := range rows {
		items = append(items, locationItem(loc, paths[loc.ID], counts[loc.ID]))
	}
	return items
}

// locationEnriched is the single-item variant of locationEnrichedList.
func locationEnriched(c *gin.Context, q *sqlcgen.Queries, loc sqlcgen.CoreLocation) gin.H {
	items := locationEnrichedList(c.Request.Context(), q, loc.MerchantID, []sqlcgen.CoreLocation{loc})
	if len(items) == 1 {
		return items[0]
	}
	return locationItem(loc, "", 0)
}

// locationPaths computes each location's name path ("A › B › C") from a single
// in-memory merchant list — no recursive SQL per item. Reused by plan B for
// room names on the calendar.
func locationPaths(rows []sqlcgen.CoreLocation) map[pgtype.UUID]string {
	names := make(map[pgtype.UUID]string, len(rows))
	parents := make(map[pgtype.UUID]pgtype.UUID, len(rows))
	for _, r := range rows {
		names[r.ID] = r.Name
		if r.ParentID.Valid {
			parents[r.ID] = r.ParentID
		}
	}
	paths := make(map[pgtype.UUID]string, len(rows))
	for _, r := range rows {
		var parts []string
		cur, depth := r.ID, 0
		for {
			name, ok := names[cur]
			if !ok || depth > len(rows) {
				break
			}
			parts = append([]string{name}, parts...)
			next, ok := parents[cur]
			if !ok {
				break
			}
			cur = next
			depth++
		}
		paths[r.ID] = strings.Join(parts, " › ")
	}
	return paths
}

// locationChildCounts counts active children per parent from the same list.
func locationChildCounts(rows []sqlcgen.CoreLocation) map[pgtype.UUID]int64 {
	counts := make(map[pgtype.UUID]int64, len(rows))
	for _, r := range rows {
		if r.ParentID.Valid {
			counts[r.ParentID]++
		}
	}
	return counts
}

// locationItem renders one location row: every column plus the derived path
// and children_count.
func locationItem(loc sqlcgen.CoreLocation, path string, childrenCount int64) gin.H {
	return gin.H{
		"id":                    loc.ID,
		"company_id":            loc.CompanyID,
		"merchant_id":           loc.MerchantID,
		"parent_id":             uuidOrNil(loc.ParentID),
		"kind":                  loc.Kind,
		"code":                  loc.Code,
		"name":                  loc.Name,
		"functions":             loc.Functions,
		"service_class":         textOrNil(loc.ServiceClass),
		"capacity":              intOrNil(loc.Capacity),
		"latitude":              numericOrNil(loc.Latitude),
		"longitude":             numericOrNil(loc.Longitude),
		"status":                loc.Status,
		"sort_order":            loc.SortOrder,
		"satusehat_location_id": textOrNil(loc.SatusehatLocationID),
		"created_at":            loc.CreatedAt,
		"created_by":            uuidOrNil(loc.CreatedBy),
		"updated_at":            loc.UpdatedAt,
		"updated_by":            uuidOrNil(loc.UpdatedBy),
		"path":                  path,
		"children_count":        childrenCount,
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func derefString(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func optTextValue(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func optInt4(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
}

// numericFromFloat converts an optional JSON float into numeric(9,6). A nil
// pointer becomes NULL — required so a site without coordinates still trips
// chk_location_site_position (a ::float8-cast param would coerce 0 and hide
// the violation).
func numericFromFloat(p *float64) pgtype.Numeric {
	if p == nil {
		return pgtype.Numeric{}
	}
	var n pgtype.Numeric
	if err := n.Scan(strconv.FormatFloat(*p, 'f', -1, 64)); err != nil {
		return pgtype.Numeric{}
	}
	return n
}

func optInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func intOrNil(v pgtype.Int4) any {
	if !v.Valid {
		return nil
	}
	return v.Int32
}

// numericOrNil renders numeric(9,6) as float64 or nil.
func numericOrNil(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return nil
	}
	return f.Float64
}
