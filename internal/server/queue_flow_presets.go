// internal/server/queue_flow_presets.go
package server

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// presetPerm constants for served_by_permission of preset stages (spec §5):
// farmasi keeps operations.visit.manage until the pharmacy permission exists (#13).
const permInvoiceCreate = "billing.invoice.create"

type presetStage struct {
	Seq             int32
	Name            string
	Kind            string
	Prefix          *string
	Skippable       bool
	RequiresCheckin bool
	BpjsTaskStart   *int32
	BpjsTaskEnd     *int32
	Perm            string
}

func int32Ptr(v int32) *int32 { return &v }

// (strPtr already exists in stock_ops.go — reused for preset prefixes.)

// queueFlowPresets — the built-in templates (plan 2026-10-21 task 1 kontrak):
// simple = Admisi → Dokter → Kasir & Apotek;
// outpatient_full = Admisi(task 1→3) → Check-in Poli → Perawat(3→4) →
// Dokter(4→5) → Kasir → Farmasi(5→7), check-in required before Kasir/Farmasi;
// hemodialisa / mcu / fast_track = non-rawat-jalan presets (spec §4.1);
// igd is deferred to Fase 2.
var queueFlowPresets = map[string]struct {
	defaultName string
	description string
	stages      []presetStage
}{
	"simple": {
		defaultName: "Rawat Jalan Sederhana",
		description: "Admisi → Dokter → Kasir & Apotek",
		stages: []presetStage{
			{Seq: 1, Name: "Pendaftaran", Kind: "admission", Perm: PermCounterManage},
			{Seq: 2, Name: "Dokter", Kind: "physician", Perm: PermVisitManage},
			{Seq: 3, Name: "Kasir & Apotek", Kind: "cashier", Perm: permInvoiceCreate},
		},
	},
	"outpatient_full": {
		defaultName: "Rawat Jalan Lengkap",
		description: "Admisi → Check-in → Perawat → Dokter → Kasir → Farmasi",
		stages: []presetStage{
			{Seq: 1, Name: "Admisi", Kind: "admission", Perm: PermCounterManage, BpjsTaskStart: int32Ptr(1), BpjsTaskEnd: int32Ptr(3)},
			{Seq: 2, Name: "Check-in Poli", Kind: "checkin", Perm: PermVisitManage, RequiresCheckin: true},
			{Seq: 3, Name: "Perawat", Kind: "nurse", Perm: PermVisitManage, BpjsTaskStart: int32Ptr(3), BpjsTaskEnd: int32Ptr(4)},
			{Seq: 4, Name: "Dokter", Kind: "physician", Perm: PermVisitManage, BpjsTaskStart: int32Ptr(4), BpjsTaskEnd: int32Ptr(5)},
			{Seq: 5, Name: "Kasir", Kind: "cashier", Perm: permInvoiceCreate, RequiresCheckin: true},
			{Seq: 6, Name: "Farmasi", Kind: "pharmacy", Perm: PermVisitManage, RequiresCheckin: true, BpjsTaskStart: int32Ptr(5), BpjsTaskEnd: int32Ptr(7)},
		},
	},
	"hemodialisa": {
		defaultName: "Hemodialisa",
		description: "Admisi → Check-in → Perawat Pre-HD → Dokter → Hemodialisa → Kasir → Farmasi",
		stages: []presetStage{
			{Seq: 1, Name: "Admisi", Kind: "admission", Perm: PermCounterManage},
			{Seq: 2, Name: "Check-in", Kind: "checkin", Perm: PermVisitManage, RequiresCheckin: true},
			{Seq: 3, Name: "Perawat Pre-HD", Kind: "nurse", Perm: PermVisitManage},
			{Seq: 4, Name: "Dokter", Kind: "physician", Perm: PermVisitManage, Skippable: true},
			{Seq: 5, Name: "Hemodialisa", Kind: "custom", Perm: PermVisitManage},
			{Seq: 6, Name: "Kasir", Kind: "cashier", Perm: permInvoiceCreate},
			{Seq: 7, Name: "Farmasi", Kind: "pharmacy", Perm: PermVisitManage},
		},
	},
	"mcu": {
		defaultName: "MCU",
		description: "Admisi → Perawat/Vitals → Penunjang Lab & Radiologi → Dokter → Konsultasi Hasil → Kasir",
		stages: []presetStage{
			{Seq: 1, Name: "Admisi MCU", Kind: "admission", Perm: PermCounterManage},
			{Seq: 2, Name: "Perawat/Vitals", Kind: "nurse", Perm: PermVisitManage},
			{Seq: 3, Name: "Penunjang Lab & Radiologi", Kind: "support", Perm: PermVisitManage},
			{Seq: 4, Name: "Pemeriksaan Dokter", Kind: "physician", Perm: PermVisitManage},
			{Seq: 5, Name: "Konsultasi Hasil", Kind: "custom", Perm: PermVisitManage, Skippable: true},
			{Seq: 6, Name: "Kasir", Kind: "cashier", Perm: permInvoiceCreate},
		},
	},
	"fast_track": {
		defaultName: "Fast Track / Eksekutif",
		description: "Admisi → Dokter → Kasir & Farmasi (nomor prefix FT)",
		stages: []presetStage{
			{Seq: 1, Name: "Admisi", Kind: "admission", Perm: PermCounterManage, Prefix: strPtr("FT")},
			{Seq: 2, Name: "Dokter", Kind: "physician", Perm: PermVisitManage, Prefix: strPtr("FT")},
			{Seq: 3, Name: "Kasir & Farmasi", Kind: "cashier", Perm: permInvoiceCreate, Prefix: strPtr("FT")},
		},
	},
}

// ListQueueFlowPresetsHandler godoc
// @Summary List built-in queue flow presets
// @Tags queue-flow
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /queue-flows/presets [get]
func ListQueueFlowPresetsHandler(c *gin.Context) {
	if !RequirePermission(c, PermQueueConfigure) {
		return
	}
	keys := make([]string, 0, len(queueFlowPresets))
	for k := range queueFlowPresets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		p := queueFlowPresets[k]
		out = append(out, gin.H{"key": k, "name": p.defaultName, "description": p.description})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{}})
}

type createQueueFlowFromPresetRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
	Name       string `json:"name"`
}

// CreateQueueFlowFromPresetHandler godoc
// @Summary Create a queue flow from a built-in preset
// @Param preset path string true "Preset key (simple | outpatient_full)"
// @Tags queue-flow
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body createQueueFlowFromPresetRequest true "Target merchant"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Router /queue-flows/from-preset/{preset} [post]
func CreateQueueFlowFromPresetHandler(c *gin.Context) {
	preset, ok := queueFlowPresets[c.Param("preset")]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown preset"})
		return
	}
	var req createQueueFlowFromPresetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	ctx := c.Request.Context()
	q := sqlcgen.New(TxFromContext(c))
	name := preset.defaultName
	if req.Name != "" {
		name = req.Name
	}
	flow, err := q.CreateQueueFlow(ctx, sqlcgen.CreateQueueFlowParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, Name: name,
		ServiceTypes: []string{"umum", "bpjs", "asuransi"}, DepartmentIds: []pgtype.UUID{},
		IsDefault: false, IsActive: true, CreatedBy: AuthUserID(c),
	})
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "flow name already exists"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	stages := make([]gin.H, 0, len(preset.stages))
	for _, s := range preset.stages {
		st, err := q.CreateQueueStageFull(ctx, sqlcgen.CreateQueueStageFullParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID, FlowID: flow.ID,
			Seq: s.Seq, Name: s.Name, Kind: s.Kind,
			NumberPrefix: optTextPtr(s.Prefix), Skippable: s.Skippable,
			RequiresCheckin: s.RequiresCheckin,
			BpjsTaskStart:   optInt2Ptr(s.BpjsTaskStart), BpjsTaskEnd: optInt2Ptr(s.BpjsTaskEnd),
			ServedByPermission: s.Perm,
		})
		if err != nil {
			c.Error(err)
			respondInternalError(c, err)
			return
		}
		stages = append(stages, queueStageJSON(st))
	}
	out := queueFlowJSON(flow)
	out["stages"] = stages
	c.JSON(http.StatusCreated, gin.H{"data": out, "meta": gin.H{}})
}
