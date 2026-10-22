// internal/server/empi.go
// EMPI G2 endpoints (spec 2026-10-20-empi-cross-company §6.3): resolve
// read-only dan master detail lintas company (izin empi.master.view,
// migrasi 000085 — katalog saja, operator menugaskan manual via Role Matrix).
package server

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// EMPI permissions (migrasi 000085).
const (
	PermEmpiMasterView   = "empi.master.view"
	PermEmpiMasterManage = "empi.master.manage"
)

// RegisterEmpiRoutes wires the EMPI cross-company endpoints (base path
// /api/v1/empi/...). Semua handler membuka dengan RequirePermission
// (pola person_merge.go); respons 500 hanya respondInternalError.
func RegisterEmpiRoutes(rg *gin.RouterGroup) {
	rg.GET("/empi/resolve", AccessLog("empi", "resolve", ""), ResolvePersonHandler)
	rg.GET("/empi/master-persons/:id", AccessLog("empi", "master_detail", "id"), MasterDetailHandler)
	rg.GET("/empi/candidate-pairs", AccessLog("empi", "candidate_pairs", ""), ListEmpiCandidatePairsHandler)
	rg.POST("/empi/candidate-pairs/:id/dismiss", AccessLog("empi", "candidate_dismiss", "id"), DismissEmpiCandidatePairHandler)
}

// empiPersonMrn membaca MRN satu person DI DALAM tx request SETELAH memindah
// GUC company ke company milik link (person RLS = company-level, 000003;
// jangan JOIN core.person lintas company). Person tidak terbaca → "".
func empiPersonMrn(c *gin.Context, tx pgx.Tx, companyID, personID pgtype.UUID) (string, error) {
	if _, err := tx.Exec(c.Request.Context(),
		"SELECT set_config('app.current_company_id', $1, true)", companyID.String()); err != nil {
		return "", err
	}
	p, err := sqlcgen.New(tx).GetPersonMrn(c.Request.Context(), personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !p.MedicalRecordNo.Valid {
		return "", nil
	}
	return p.MedicalRecordNo.String, nil
}

// empiLinkJSON renders one master link (snake_case, pola admissionJSON).
// TANPA nik_search_hash/NIK (PDP, spec §7).
func empiLinkJSON(l sqlcgen.EmpiMasterLinkCompany, mrn string) gin.H {
	return gin.H{
		"company_id":        l.CompanyID,
		"company_name":      l.CompanyName,
		"person_id":         l.PersonID,
		"medical_record_no": mrn,
		"full_name":         l.FullName,
		"status":            l.Status,
	}
}

// empiMasterJSON membangun isi data respons master (links + pending count)
// untuk resolve maupun master detail. Error internal di-bubble ke pemanggil
// untuk respondInternalError.
func empiMasterJSON(c *gin.Context, tx pgx.Tx, q *sqlcgen.Queries, masterID pgtype.UUID,
	fullName string, birthDate pgtype.Date, gender pgtype.Text, mergedInto pgtype.UUID) (gin.H, error) {
	ctx := c.Request.Context()
	linkRows, err := q.ListEmpiLinksByMaster(ctx, masterID)
	if err != nil {
		return nil, err
	}
	links := make([]gin.H, 0, len(linkRows))
	for _, l := range linkRows {
		mrn, err := empiPersonMrn(c, tx, l.CompanyID, l.PersonID)
		if err != nil {
			return nil, err
		}
		links = append(links, empiLinkJSON(l, mrn))
	}
	pending, err := q.CountEmpiPendingCandidatesByMaster(ctx, masterID)
	if err != nil {
		return nil, err
	}
	return gin.H{
		"master_person_id":             masterID,
		"full_name":                    fullName,
		"birth_date":                   birthDate,
		"gender":                       gender,
		"merged_into_master_person_id": mergedInto,
		"links":                        links,
		"candidate_pairs_pending":      pending,
	}, nil
}

// ResolvePersonHandler godoc
// @Summary Resolve a person to its EMPI master with all cross-company links
// @Description Returns the master identity and every active link (all companies) with per-company medical record numbers.
// @Tags empi
// @Produce json
// @Security BearerAuth
// @Param person_id query string true "Person UUID (must belong to the caller's company)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /empi/resolve [get]
func ResolvePersonHandler(c *gin.Context) {
	if !RequirePermission(c, PermEmpiMasterView) {
		return
	}
	personID, ok := parseUUID(c.Query("person_id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()
	link, err := q.ResolveEmpiLinkByPerson(ctx, sqlcgen.ResolveEmpiLinkByPersonParams{
		CompanyID: AuthCompanyID(c), PersonID: personID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 404 tanpa detail (person company lain / tanpa link — sama).
		c.JSON(http.StatusNotFound, gin.H{"error": "person not linked"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data, err := empiMasterJSON(c, tx, q, link.MasterPersonID,
		link.FullName, link.BirthDate, link.Gender, link.MergedIntoMasterPersonID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// MasterDetailHandler godoc
// @Summary EMPI master person detail with links and pending candidate count
// @Tags empi
// @Produce json
// @Security BearerAuth
// @Param id path string true "Master person UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /empi/master-persons/{id} [get]
func MasterDetailHandler(c *gin.Context) {
	if !RequirePermission(c, PermEmpiMasterView) {
		return
	}
	masterID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	master, err := q.GetEmpiMaster(c.Request.Context(), masterID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "master person not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data, err := empiMasterJSON(c, tx, q, master.ID,
		master.FullName, master.BirthDate, master.Gender, master.MergedIntoMasterPersonID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// empiCandidateSide renders one side of a candidate pair (snake_case); nil
// bila link sisi itu sudah tidak ada (LEFT JOIN person_link kosong).
func empiCandidateSide(companyID pgtype.UUID, companyName string, personID pgtype.UUID,
	fullName pgtype.Text, birthDate pgtype.Date, gender pgtype.Text, hasNik any, masterPersonID pgtype.UUID) gin.H {
	if !personID.Valid {
		return nil
	}
	return gin.H{
		"company_id":       companyID,
		"company_name":     companyName,
		"person_id":        personID,
		"full_name":        fullName,
		"birth_date":       birthDate,
		"gender":           gender,
		"has_nik":          hasNik,
		"master_person_id": masterPersonID,
	}
}

// empiCandidateJSON renders one candidate pair with its a/b sides.
func empiCandidateJSON(r sqlcgen.ListEmpiCandidatePairsRow) gin.H {
	return gin.H{
		"id":     r.ID,
		"score":  r.Score,
		"reason": r.Reason,
		"status": r.Status,
		"note":   r.Note,
		"a": empiCandidateSide(r.ACompanyID, r.ACompanyName, r.APersonID,
			r.AFullName, r.ABirthDate, r.AGender, r.AHasNik, r.AMasterPersonID),
		"b": empiCandidateSide(r.BCompanyID, r.BCompanyName, r.BPersonID,
			r.BFullName, r.BBirthDate, r.BGender, r.BHasNik, r.BMasterPersonID),
	}
}

// ListEmpiCandidatePairsHandler godoc
// @Summary List pending EMPI candidate pairs across companies
// @Description Scored cross-company pairs awaiting a merge/dismiss decision, highest score first.
// @Tags empi
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Failure 403 {object} apiErrorResponse
// @Router /empi/candidate-pairs [get]
func ListEmpiCandidatePairsHandler(c *gin.Context) {
	if !RequirePermission(c, PermEmpiMasterManage) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListEmpiCandidatePairs(c.Request.Context(), sqlcgen.ListEmpiCandidatePairsParams{
		Limit: limit, Offset: offset,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		items = append(items, empiCandidateJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

type empiCandidateDismissRequest struct {
	Note string `json:"note"`
}

// DismissEmpiCandidatePairHandler godoc
// @Summary Dismiss a pending EMPI candidate pair
// @Tags empi
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Candidate pair UUID"
// @Param request body empiCandidateDismissRequest false "Optional note"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /empi/candidate-pairs/{id}/dismiss [post]
func DismissEmpiCandidatePairHandler(c *gin.Context) {
	if !RequirePermission(c, PermEmpiMasterManage) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req empiCandidateDismissRequest
	// Body opsional: tanpa body (ContentLength 0) tetap valid tanpa note.
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	q := sqlcgen.New(TxFromContext(c))
	row, err := q.DismissEmpiCandidatePair(c.Request.Context(), sqlcgen.DismissEmpiCandidatePairParams{
		ID: id, Note: optText(req.Note), DecidedBy: AuthUserID(c),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "candidate pair is not pending"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": row.ID, "status": row.Status}, "meta": gin.H{}})
}
