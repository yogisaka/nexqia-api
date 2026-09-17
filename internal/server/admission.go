// internal/server/admission.go
package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func RegisterAdmissionRoutes(rg *gin.RouterGroup) {
	rg.POST("/admissions", CreateAdmissionHandler)
	rg.GET("/admissions", ListAdmissionsHandler)
	rg.GET("/admissions/:id", GetAdmissionHandler)
	rg.PATCH("/admissions/:id", UpdateAdmissionHandler)
}

type createAdmissionRequest struct {
	PersonID     string `json:"person_id" binding:"required"`
	DepartmentID string `json:"department_id" binding:"required"`
	PhysicianID  string `json:"physician_id"`
}

// CreateAdmissionHandler is FO check-in (spec §4 point 1): registers the
// kunjungan (operations.admission, admission_type='outpatient') and its first
// queue row (queue_type='pendaftaran') atomically. If the queue insert fails
// after the admission insert already succeeded, c.Error(err) signals
// TenantMiddleware to roll back the whole request instead of committing an
// admission with no queue ticket — see plan's Global Constraints.
// CreateAdmissionHandler godoc
// @Summary Check in a patient (FO)
// @Description Registers the kunjungan (admission, outpatient) and its first queue row (pendaftaran) atomically.
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createAdmissionRequest true "Admission data"
// @Success 201 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /admissions [post]
func CreateAdmissionHandler(c *gin.Context) {
	if !RequirePermission(c, PermVisitManage) {
		return
	}
	var req createAdmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	personID, ok := parseUUID(req.PersonID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	departmentID, ok := parseUUID(req.DepartmentID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid department_id"})
		return
	}
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))

	department, err := q.GetDepartmentByID(ctx, departmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "department not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	admissionSeq, err := q.CountTodayAdmissionsByDepartment(ctx, sqlcgen.CountTodayAdmissionsByDepartmentParams{
		MerchantID: merchantID, DepartmentID: departmentID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	visitNo := fmt.Sprintf("%s%s-%04d", department.Code, time.Now().Format("20060102"), admissionSeq+1)

	admission, err := q.CreateAdmission(ctx, sqlcgen.CreateAdmissionParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, VisitNo: visitNo, PersonID: personID,
		AdmissionType: "outpatient", DepartmentID: departmentID, PhysicianID: optUUID(req.PhysicianID),
		CreatedBy: AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	queueSeq, err := q.CountTodayQueueByType(ctx, sqlcgen.CountTodayQueueByTypeParams{
		MerchantID: merchantID, QueueType: "pendaftaran", DepartmentID: departmentID,
	})
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	queueNumber := fmt.Sprintf("%s-%03d", department.Code, queueSeq+1)
	queue, err := q.CreateQueue(ctx, sqlcgen.CreateQueueParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, QueueType: "pendaftaran", DepartmentID: departmentID,
		PersonID: personID, AdmissionID: pgtype.UUID{Bytes: admission.ID.Bytes, Valid: true},
		QueueNumber: queueNumber, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"admission": admission, "queue": queue}, "meta": gin.H{}})
}

// ListAdmissionsHandler godoc
// @Summary List admissions for a work queue
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param department_id query string true "Department UUID"
// @Param status query string true "Admission status"
// @Param limit query int false "Page size"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /admissions [get]
func ListAdmissionsHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, merchantID) {
		return
	}
	departmentID, ok := parseUUID(c.Query("department_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "department_id query param is required"})
		return
	}
	status := c.Query("status")
	if status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status query param is required"})
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	admissions, err := q.ListAdmissionsForWork(c.Request.Context(), sqlcgen.ListAdmissionsForWorkParams{
		MerchantID: merchantID, DepartmentID: departmentID, Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admissions, "meta": gin.H{"limit": limit, "offset": offset}})
}

// GetAdmissionHandler godoc
// @Summary Get an admission by id
// @Tags admission
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id} [get]
func GetAdmissionHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, err := q.GetAdmissionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, admission.MerchantID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admission, "meta": gin.H{}})
}

// updateAdmissionRequest is deliberately generic (physician reassignment,
// status, discharge_at) — "discharge" as a business action belongs to
// sub-project #3 (CPPT/resume), this endpoint just exposes the column writes
// it will need. See spec §2.
type updateAdmissionRequest struct {
	PhysicianID string `json:"physician_id"`
	Status      string `json:"status" binding:"required"`
	DischargeAt string `json:"discharge_at"`
}

// UpdateAdmissionHandler godoc
// @Summary Update an admission
// @Description Generic column writes (physician reassignment, status, discharge_at) — "discharge" as a business action belongs to a future CPPT/resume sub-project.
// @Tags admission
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Admission UUID"
// @Param request body updateAdmissionRequest true "Admission data"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /admissions/{id} [patch]
func UpdateAdmissionHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetAdmissionByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !RequirePermissionForMerchant(c, PermVisitManage, existing.MerchantID) {
		return
	}
	var req updateAdmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var dischargeAt pgtype.Timestamptz
	if req.DischargeAt != "" {
		t, err := time.Parse(time.RFC3339, req.DischargeAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid discharge_at, expected RFC3339"})
			return
		}
		dischargeAt = pgtype.Timestamptz{Time: t, Valid: true}
	}
	admission, err := q.UpdateAdmission(c.Request.Context(), sqlcgen.UpdateAdmissionParams{
		ID: id, PhysicianID: optUUID(req.PhysicianID), Status: req.Status, DischargeAt: dischargeAt, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "admission not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": admission, "meta": gin.H{}})
}
