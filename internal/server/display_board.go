// internal/server/display_board.go
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

var validBoardLayouts = map[string]bool{"single": true, "grid": true, "split": true}
var validBoardVoices = map[string]bool{"off": true, "chime": true, "tts_id": true}
var validNameDisplays = map[string]bool{"hidden": true, "initials": true, "masked": true}

func RegisterDisplayBoardRoutes(rg *gin.RouterGroup) {
	rg.GET("/merchants/:id/display-boards", ListDisplayBoardsHandler)
	rg.POST("/display-boards", CreateDisplayBoardHandler)
	rg.PATCH("/display-boards/:id", UpdateDisplayBoardHandler)
	rg.DELETE("/display-boards/:id", DeleteDisplayBoardHandler)
	rg.POST("/display-boards/:id/token", CreateDisplayBoardTokenHandler)
	rg.DELETE("/display-boards/:id/token", RevokeDisplayBoardTokenHandler)
}

// boardJSON renders a board snake_case and NEVER includes token_hash — the
// raw device token is shown exactly once by CreateDisplayBoardTokenHandler.
func boardJSON(b sqlcgen.OperationsDisplayBoard) gin.H {
	return gin.H{
		"id":           b.ID,
		"merchant_id":  b.MerchantID,
		"name":         b.Name,
		"location_id":  boardUUIDOrNil(b.LocationID),
		"stage_ids":    b.StageIds,
		"counter_ids":  b.CounterIds,
		"layout":       b.Layout,
		"show_next_n":  b.ShowNextN,
		"voice":        b.Voice,
		"name_display": b.NameDisplay,
		"poll_seconds": b.PollSeconds,
		"is_active":    b.IsActive,
		"has_token":    b.TokenHash.Valid,
	}
}

func boardUUIDOrNil(id pgtype.UUID) any {
	if !id.Valid {
		return nil
	}
	return id
}

type displayBoardInput struct {
	Name        string   `json:"name" binding:"required"`
	LocationID  string   `json:"location_id"`
	StageIDs    []string `json:"stage_ids"`
	CounterIDs  []string `json:"counter_ids"`
	Layout      string   `json:"layout"`
	ShowNextN   *int32   `json:"show_next_n"`
	Voice       string   `json:"voice"`
	NameDisplay string   `json:"name_display"`
	PollSeconds *int32   `json:"poll_seconds"`
	IsActive    *bool    `json:"is_active"`
}

type createDisplayBoardRequest struct {
	displayBoardInput
	MerchantID string `json:"merchant_id" binding:"required"`
}

// normalizeBoardInput applies DB defaults and enum/range validation (400 on
// violation, so the CHECK constraints never surface as 500).
func normalizeBoardInput(req *displayBoardInput) bool {
	if req.Layout == "" {
		req.Layout = "grid"
	} else if !validBoardLayouts[req.Layout] {
		return false
	}
	if req.Voice == "" {
		req.Voice = "chime"
	} else if !validBoardVoices[req.Voice] {
		return false
	}
	if req.NameDisplay == "" {
		req.NameDisplay = "hidden"
	} else if !validNameDisplays[req.NameDisplay] {
		return false
	}
	if req.ShowNextN == nil {
		req.ShowNextN = new(int32)
		*req.ShowNextN = 5
	} else if *req.ShowNextN < 0 || *req.ShowNextN > 20 {
		return false
	}
	if req.PollSeconds == nil {
		req.PollSeconds = new(int32)
		*req.PollSeconds = 5
	} else if *req.PollSeconds < 2 || *req.PollSeconds > 60 {
		return false
	}
	return true
}

func boardUUIDList(raw []string) ([]pgtype.UUID, bool) {
	out := make([]pgtype.UUID, 0, len(raw))
	for _, r := range raw {
		id, ok := parseUUID(r)
		if !ok {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// CreateDisplayBoardHandler godoc
// @Summary Create a display board
// @Tags display-board
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body displayBoardInput true "Board data (merchant_id required)"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /display-boards [post]
func CreateDisplayBoardHandler(c *gin.Context) {
	var req createDisplayBoardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !normalizeBoardInput(&req.displayBoardInput) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid board settings (layout/voice/name_display enum, show_next_n 0..20, poll_seconds 2..60)"})
		return
	}
	merchantID, ok := parseUUID(req.MerchantID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid merchant_id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, merchantID) {
		return
	}
	stageIDs, ok := boardUUIDList(req.StageIDs)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage_ids entry"})
		return
	}
	counterIDs, ok := boardUUIDList(req.CounterIDs)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counter_ids entry"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	board, err := q.CreateDisplayBoard(c.Request.Context(), sqlcgen.CreateDisplayBoardParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Name: req.Name,
		LocationID: optUUID(req.LocationID), StageIds: stageIDs, CounterIds: counterIDs,
		Layout: req.Layout, ShowNextN: *req.ShowNextN, Voice: req.Voice,
		NameDisplay: req.NameDisplay, PollSeconds: *req.PollSeconds,
		IsActive: req.IsActive == nil || *req.IsActive, CreatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": boardJSON(board), "meta": gin.H{}})
}

// ListDisplayBoardsHandler godoc
// @Summary List a merchant's display boards
// @Tags display-board
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merchant UUID"
// @Success 200 {object} apiResponse
// @Router /merchants/{id}/display-boards [get]
func ListDisplayBoardsHandler(c *gin.Context) {
	merchantID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, merchantID) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	boards, err := q.ListDisplayBoardsByMerchant(c.Request.Context(), sqlcgen.ListDisplayBoardsByMerchantParams{
		MerchantID: merchantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	out := make([]gin.H, 0, len(boards))
	for _, b := range boards {
		out = append(out, boardJSON(b))
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{"limit": limit, "offset": offset}})
}

// UpdateDisplayBoardHandler godoc
// @Summary Update a display board
// @Tags display-board
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Board UUID"
// @Param request body displayBoardInput true "Board fields"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /display-boards/{id} [patch]
func UpdateDisplayBoardHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req displayBoardInput
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !normalizeBoardInput(&req) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid board settings (layout/voice/name_display enum, show_next_n 0..20, poll_seconds 2..60)"})
		return
	}
	stageIDs, ok := boardUUIDList(req.StageIDs)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stage_ids entry"})
		return
	}
	counterIDs, ok := boardUUIDList(req.CounterIDs)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid counter_ids entry"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetDisplayBoardByID(ctx, id)
	if err == pgx.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "display board not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, existing.MerchantID) {
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	board, err := q.UpdateDisplayBoard(ctx, sqlcgen.UpdateDisplayBoardParams{
		ID: id, Name: req.Name, LocationID: optUUID(req.LocationID),
		StageIds: stageIDs, CounterIds: counterIDs,
		Layout: req.Layout, ShowNextN: *req.ShowNextN, Voice: req.Voice,
		NameDisplay: req.NameDisplay, PollSeconds: *req.PollSeconds,
		IsActive: isActive, UpdatedBy: AuthUserID(c),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": boardJSON(board), "meta": gin.H{}})
}

// DeleteDisplayBoardHandler godoc
// @Summary Soft-delete a display board
// @Tags display-board
// @Security BearerAuth
// @Param id path string true "Board UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /display-boards/{id} [delete]
func DeleteDisplayBoardHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	existing, err := q.GetDisplayBoardByID(ctx, id)
	if err == pgx.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "display board not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, existing.MerchantID) {
		return
	}
	if err := q.SoftDeleteDisplayBoard(ctx, sqlcgen.SoftDeleteDisplayBoardParams{ID: id, DeletedBy: AuthUserID(c)}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// newDisplayToken returns (rawToken, sha256Hex). The raw token is returned to
// the admin exactly once; only the hash is persisted (spec §5).
func newDisplayToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// CreateDisplayBoardTokenHandler godoc
// @Summary Issue a device token for a display board
// @Description Returns the raw token ONCE; only its sha256 hex is stored.
// @Tags display-board
// @Produce json
// @Security BearerAuth
// @Param id path string true "Board UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /display-boards/{id}/token [post]
func CreateDisplayBoardTokenHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	board, err := q.GetDisplayBoardByID(ctx, id)
	if err == pgx.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "display board not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, board.MerchantID) {
		return
	}
	token, hash, err := newDisplayToken()
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.SetDisplayBoardToken(ctx, sqlcgen.SetDisplayBoardTokenParams{ID: id, TokenHash: pgtype.Text{String: hash, Valid: true}, UpdatedBy: AuthUserID(c)}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"token": token}, "meta": gin.H{}})
}

// RevokeDisplayBoardTokenHandler godoc
// @Summary Revoke a display board's device token
// @Tags display-board
// @Security BearerAuth
// @Param id path string true "Board UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /display-boards/{id}/token [delete]
func RevokeDisplayBoardTokenHandler(c *gin.Context) {
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	board, err := q.GetDisplayBoardByID(ctx, id)
	if err == pgx.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "display board not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if !RequirePermissionForMerchant(c, PermQueueConfigure, board.MerchantID) {
		return
	}
	if err := q.ClearDisplayBoardToken(ctx, sqlcgen.ClearDisplayBoardTokenParams{ID: id, UpdatedBy: AuthUserID(c)}); err != nil {
		c.Error(err)
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// patientLabel masks a patient name for the display per board.name_display:
// hidden → null; initials → first letter of each word ("B. S."); masked →
// first letter of each word + "***" ("B*** S***").
func patientLabel(fullName, mode string) any {
	switch mode {
	case "initials":
		var sb strings.Builder
		for i, word := range strings.Fields(fullName) {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteByte(word[0])
			sb.WriteString(".")
		}
		return sb.String()
	case "masked":
		var sb strings.Builder
		for i, word := range strings.Fields(fullName) {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteByte(word[0])
			sb.WriteString("***")
		}
		return sb.String()
	default: // "hidden"
		return nil
	}
}

// DisplayBoardFeedHandler godoc
// @Summary Device feed for a display board
// @Description Token-authenticated (X-Display-Token), rate limited; no patient
// @Description identifiers — only masked labels. 401 "invalid display token"
// @Description on missing/wrong token or inactive board.
// @Tags display
// @Produce json
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param X-Company-ID header string true "Company UUID"
// @Param id path string true "Board UUID"
// @Success 200 {object} apiResponse
// @Failure 401 {object} apiErrorResponse
// @Router /display/boards/{id}/feed [get]
func DisplayBoardFeedHandler(c *gin.Context) {
	boardID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid display token"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	board, err := q.GetDisplayBoardByID(c.Request.Context(), boardID)
	if err != nil {
		// Unknown board and wrong token are indistinguishable by design.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid display token"})
		return
	}
	token := c.GetHeader("X-Display-Token")
	if token == "" || !board.TokenHash.Valid ||
		subtle.ConstantTimeCompare([]byte(sha256Hex(token)), []byte(board.TokenHash.String)) != 1 ||
		!board.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid display token"})
		return
	}
	tickets, err := q.ListBoardFeedTickets(c.Request.Context(), sqlcgen.ListBoardFeedTicketsParams{
		MerchantID: board.MerchantID, Column2: board.StageIds, Column3: board.CounterIds,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	current := []gin.H{}
	waiting := []gin.H{}
	for _, t := range tickets {
		item := gin.H{
			"queue_number":   t.QueueNumber,
			"stage_name":     boardTextOrNil(t.StageName),
			"counter_name":   boardTextOrNil(t.CounterName),
			"called_at":      boardTimeOrNil(t.CalledAt),
			"patient_label":  patientLabel(t.PatientName, board.NameDisplay),
			"priority":       t.Priority,
			"physician_name": nil,
		}
		if t.StageKind.Valid && t.StageKind.String == "physician" && t.PhysicianName.Valid {
			item["physician_name"] = t.PhysicianName.String
		}
		switch t.Status {
		case "called", "in_progress":
			current = append(current, item)
		case "waiting":
			if int32(len(waiting)) < board.ShowNextN {
				waiting = append(waiting, item)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"board": gin.H{
			"id":           board.ID,
			"name":         board.Name,
			"layout":       board.Layout,
			"voice":        board.Voice,
			"poll_seconds": board.PollSeconds,
			"show_next_n":  board.ShowNextN,
			"name_display": board.NameDisplay,
		},
		"current": current,
		"waiting": waiting,
	}, "meta": gin.H{}})
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func boardTextOrNil(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

func boardTimeOrNil(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}
