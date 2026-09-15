// internal/server/rbac.go
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// RegisterRBACRoutes wires core.app_user, core.role, core.permission and their
// join tables (docs/07-core-ddl.md §2 "RBAC"). Permission catalog is seed/migration-managed,
// only exposed here as read-only. Every handler scopes to AuthCompanyID(c) and gates
// mutations behind RequirePermission — see internal/server/auth.go.
func RegisterRBACRoutes(rg *gin.RouterGroup, hasher *auth.PasswordHasher) {
	rg.POST("/users", CreateAppUserHandler(hasher))
	rg.GET("/users", ListAppUsersHandler)
	rg.GET("/users/:id", GetAppUserHandler)
	rg.PATCH("/users/:id", UpdateAppUserHandler)
	rg.PUT("/users/:id/password", UpdateAppUserPasswordHandler(hasher))
	rg.DELETE("/users/:id", DeleteAppUserHandler)

	rg.GET("/permissions", ListPermissionsHandler)

	rg.POST("/roles", CreateRoleHandler)
	rg.GET("/roles", ListRolesHandler)
	rg.GET("/roles/:id", GetRoleHandler)
	rg.PATCH("/roles/:id", UpdateRoleHandler)
	rg.DELETE("/roles/:id", DeleteRoleHandler)
	rg.GET("/roles/:id/permissions", ListRolePermissionsHandler)
	rg.POST("/roles/:id/permissions", AddRolePermissionHandler)
	rg.DELETE("/roles/:id/permissions/:permission_id", RemoveRolePermissionHandler)

	rg.POST("/users/:id/merchant-roles", AddUserMerchantRoleHandler)
	rg.GET("/users/:id/merchant-roles", ListUserMerchantRolesHandler)
	rg.GET("/users/:id/roles", ListUserRolesInMerchantHandler)
	rg.DELETE("/merchant-roles/:id", RemoveUserMerchantRoleHandler)
}

type createAppUserRequest struct {
	CompanyID string `json:"company_id" binding:"required"`
	PersonID  string `json:"person_id" binding:"required"`
	Username  string `json:"username" binding:"required"`
	Email     string `json:"email"`
	Password  string `json:"password" binding:"required,min=8"`
}

func CreateAppUserHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createAppUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		companyID, ok := parseUUID(req.CompanyID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid company_id"})
			return
		}
		if companyID != AuthCompanyID(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "cannot create user outside your own company"})
			return
		}
		if !RequirePermission(c, PermUserManage) {
			return
		}
		personID, ok := parseUUID(req.PersonID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
			return
		}
		hash, err := hasher.Hash(c.Request.Context(), req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		user, err := q.CreateAppUser(c.Request.Context(), sqlcgen.CreateAppUserParams{
			CompanyID:    companyID,
			PersonID:     personID,
			Username:     req.Username,
			Email:        pgtype.Text{String: req.Email, Valid: req.Email != ""},
			PasswordHash: hash,
			CreatedBy:    AuthUserID(c),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		user.PasswordHash = ""
		c.JSON(http.StatusCreated, gin.H{"data": user, "meta": gin.H{}})
	}
}

func ListAppUsersHandler(c *gin.Context) {
	if !RequirePermission(c, PermUserManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	users, err := q.ListAppUsersByCompany(c.Request.Context(), sqlcgen.ListAppUsersByCompanyParams{
		CompanyID: AuthCompanyID(c), Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range users {
		users[i].PasswordHash = ""
	}
	c.JSON(http.StatusOK, gin.H{"data": users, "meta": gin.H{"limit": limit, "offset": offset}})
}

func GetAppUserHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	user, err := q.GetAppUserByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if user.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if id != AuthUserID(c) && !RequirePermission(c, PermUserManage) {
		return
	}
	user.PasswordHash = ""
	c.JSON(http.StatusOK, gin.H{"data": user, "meta": gin.H{}})
}

type updateAppUserRequest struct {
	PersonID string `json:"person_id" binding:"required"`
	Email    string `json:"email"`
	IsActive bool   `json:"is_active"`
}

func UpdateAppUserHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetAppUserByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if !RequirePermission(c, PermUserManage) {
		return
	}
	var req updateAppUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	personID, ok := parseUUID(req.PersonID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	user, err := q.UpdateAppUser(c.Request.Context(), sqlcgen.UpdateAppUserParams{
		ID: id, PersonID: personID, Email: pgtype.Text{String: req.Email, Valid: req.Email != ""},
		IsActive: req.IsActive, UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	user.PasswordHash = ""
	c.JSON(http.StatusOK, gin.H{"data": user, "meta": gin.H{}})
}

type updateAppUserPasswordRequest struct {
	Password string `json:"password" binding:"required,min=8"`
}

// UpdateAppUserPasswordHandler allows a user to change their own password, or an
// admin (PermUserManage) to reset someone else's — never a bare client-supplied actor.
func UpdateAppUserPasswordHandler(hasher *auth.PasswordHasher) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUUID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		q := sqlcgen.New(TxFromContext(c))
		existing, err := q.GetAppUserByID(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if existing.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if id != AuthUserID(c) && !RequirePermission(c, PermUserManage) {
			return
		}
		var req updateAppUserPasswordRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		hash, err := hasher.Hash(c.Request.Context(), req.Password)
		if err != nil {
			if errors.Is(err, auth.ErrHashQueueTimeout) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service temporarily unavailable"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}
		if err := q.UpdateAppUserPassword(c.Request.Context(), sqlcgen.UpdateAppUserPasswordParams{
			ID: id, PasswordHash: hash, UpdatedBy: AuthUserID(c),
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func DeleteAppUserHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetAppUserByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if !RequirePermission(c, PermUserManage) {
		return
	}
	if err := q.SoftDeleteAppUser(c.Request.Context(), sqlcgen.SoftDeleteAppUserParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// ListPermissionsHandler exposes the global permission catalog — not company-scoped,
// no elevated permission required (read-only, needed by any admin UI to build role forms).
func ListPermissionsHandler(c *gin.Context) {
	q := sqlcgen.New(TxFromContext(c))
	permissions, err := q.ListPermissions(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": permissions, "meta": gin.H{}})
}

type createRoleRequest struct {
	CompanyID   string `json:"company_id" binding:"required"`
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

func CreateRoleHandler(c *gin.Context) {
	var req createRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	companyID, ok := parseUUID(req.CompanyID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid company_id"})
		return
	}
	if companyID != AuthCompanyID(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cannot create role outside your own company"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.CreateRole(c.Request.Context(), sqlcgen.CreateRoleParams{
		CompanyID:   companyID,
		Name:        req.Name,
		Description: pgtype.Text{String: req.Description, Valid: req.Description != ""},
		IsSystem:    false,
		CreatedBy:   AuthUserID(c),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": role, "meta": gin.H{}})
}

func ListRolesHandler(c *gin.Context) {
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	roles, err := q.ListRolesByCompany(c.Request.Context(), sqlcgen.ListRolesByCompanyParams{
		CompanyID: AuthCompanyID(c), Limit: limit, Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": roles, "meta": gin.H{"limit": limit, "offset": offset}})
}

func GetRoleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if role.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": role, "meta": gin.H{}})
}

type updateRoleRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

func UpdateRoleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetRoleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req updateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role, err := q.UpdateRole(c.Request.Context(), sqlcgen.UpdateRoleParams{
		ID: id, Name: req.Name, Description: pgtype.Text{String: req.Description, Valid: req.Description != ""},
		UpdatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found (or is a system role)"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": role, "meta": gin.H{}})
}

func DeleteRoleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetRoleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if existing.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	if err := q.SoftDeleteRole(c.Request.Context(), sqlcgen.SoftDeleteRoleParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func ListRolePermissionsHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if role.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	permissions, err := q.ListPermissionsByRole(c.Request.Context(), roleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": permissions, "meta": gin.H{}})
}

type addRolePermissionRequest struct {
	PermissionID string `json:"permission_id" binding:"required"`
}

func AddRolePermissionHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if role.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req addRolePermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	permissionID, ok := parseUUID(req.PermissionID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_id"})
		return
	}
	if err := q.AddRolePermission(c.Request.Context(), sqlcgen.AddRolePermissionParams{
		RoleID: roleID, PermissionID: permissionID,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func RemoveRolePermissionHandler(c *gin.Context) {
	roleID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	permissionID, ok := parseUUID(c.Param("permission_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if role.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "role not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	if err := q.RemoveRolePermission(c.Request.Context(), sqlcgen.RemoveRolePermissionParams{
		RoleID: roleID, PermissionID: permissionID,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

type addUserMerchantRoleRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
	RoleID     string `json:"role_id" binding:"required"`
}

func AddUserMerchantRoleHandler(c *gin.Context) {
	userID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	targetUser, err := q.GetAppUserByID(c.Request.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if targetUser.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	var req addUserMerchantRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	merchant, err := q.GetMerchantByID(c.Request.Context(), merchantID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if merchant.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant not found"})
		return
	}
	roleID, ok := parseUUID(req.RoleID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role_id"})
		return
	}
	role, err := q.GetRoleByID(c.Request.Context(), roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if role.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role not found"})
		return
	}
	assignment, err := q.AddUserMerchantRole(c.Request.Context(), sqlcgen.AddUserMerchantRoleParams{
		UserID: userID, MerchantID: merchantID, RoleID: roleID, CreatedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"already_assigned": true}, "meta": gin.H{}})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": assignment, "meta": gin.H{}})
}

func ListUserMerchantRolesHandler(c *gin.Context) {
	userID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	targetUser, err := q.GetAppUserByID(c.Request.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if targetUser.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if userID != AuthUserID(c) && !RequirePermission(c, PermUserManage) {
		return
	}
	assignments, err := q.ListUserMerchantRolesByUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": assignments, "meta": gin.H{}})
}

func ListUserRolesInMerchantHandler(c *gin.Context) {
	userID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, ok := parseUUID(c.Query("merchant_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "merchant_id query param required"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	targetUser, err := q.GetAppUserByID(c.Request.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if targetUser.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if userID != AuthUserID(c) && !RequirePermission(c, PermUserManage) {
		return
	}
	roles, err := q.ListRolesByUserMerchant(c.Request.Context(), sqlcgen.ListRolesByUserMerchantParams{
		UserID: userID, MerchantID: merchantID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": roles, "meta": gin.H{}})
}

func RemoveUserMerchantRoleHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	assignment, err := q.GetUserMerchantRoleByID(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "assignment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	targetUser, err := q.GetAppUserByID(c.Request.Context(), assignment.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if targetUser.CompanyID != AuthCompanyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "assignment not found"})
		return
	}
	if !RequirePermission(c, PermRoleManage) {
		return
	}
	if err := q.RemoveUserMerchantRole(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
