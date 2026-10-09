// internal/server/person_merge.go
// Merge/unmerge of duplicate medical records (spec 2026-10-07-merge-medical-records
// §7, §8.1–8.3). Cross-merchant row movement only happens inside the
// SECURITY DEFINER functions core.person_merge_move / core.person_unmerge_move.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Merge permissions (migration 000065). merge_request guards the duplicate
// pair workflow (spec §8.4–8.5); merge/unmerge guard the endpoints here.
const (
	PermPersonMergeRequest = "core.person.merge_request"
	PermPersonMerge        = "core.person.merge"
	PermPersonUnmerge      = "core.person.unmerge"
)

// verifiedDocumentTypes — spec §4.2: verified_documents ⊆ this list.
var verifiedDocumentTypes = map[string]bool{
	"ktp": true, "kk": true, "akta_lahir": true,
	"kartu_bpjs": true, "paspor": true, "kitas": true,
}

// registerPersonMergeRoutes wires the merge endpoints. Static /persons/merge*
// routes must be registered BEFORE /persons/:id so gin keeps them distinct.
func registerPersonMergeRoutes(rg *gin.RouterGroup, cfg config.Config) {
	rg.GET("/persons/merge-preview", AccessLog("person", "merge_preview", ""), MergePreviewHandler(cfg))
	rg.POST("/persons/merge", AccessLog("person", "merge", ""), MergePersonsHandler(cfg))
	rg.GET("/persons/merge-logs", AccessLog("person", "merge_logs", ""), ListMergeLogsHandler)
	rg.GET("/persons/:id/merge-logs", AccessLog("person", "merge_logs", "id"), ListPersonMergeLogsHandler)
	rg.POST("/persons/merge-logs/:id/unmerge", AccessLog("person", "unmerge", ""), UnmergePersonHandler)
}

// loadMergeSide reads one person as field→value text (keys = column names) so
// the pure rules in person_merge_rules.go can compare both sides uniformly.
func loadMergeSide(ctx context.Context, q *sqlcgen.Queries, id pgtype.UUID) (map[string]string, sqlcgen.GetPersonMergeFieldsRow, error) {
	row, err := q.GetPersonMergeFields(ctx, id)
	if err != nil {
		return nil, row, err
	}
	return map[string]string{
		"medical_record_no":         row.MedicalRecordNo,
		"full_name":                 row.FullName,
		"birth_date":                row.BirthDate,
		"birth_place":               row.BirthPlace,
		"gender":                    row.Gender,
		"blood_type":                row.BloodType,
		"marital_status":            row.MaritalStatus,
		"religion_concept_id":       row.ReligionConceptID,
		"education_concept_id":      row.EducationConceptID,
		"ethnicity_concept_id":      row.EthnicityConceptID,
		"phone":                     row.Phone,
		"email":                     row.Email,
		"address":                   row.Address,
		"family_id":                 row.FamilyID,
		"photo_url":                 row.PhotoUrl,
		"region_village_concept_id": row.RegionVillageConceptID,
		"citizenship_concept_id":    row.CitizenshipConceptID,
		"job_concept_id":            row.JobConceptID,
		"marital_status_concept_id": row.MaritalStatusConceptID,
		"blood_type_concept_id":     row.BloodTypeConceptID,
		"nik":                       row.Nik,
		"nik_search_hash":           row.NikSearchHash,
		"identity_type_concept_id":  row.IdentityTypeConceptID,
	}, row, nil
}

// mergeFieldParams maps a field→value text map onto ApplyMergeFields params.
func mergeFieldParams(m map[string]string, actor, id pgtype.UUID) sqlcgen.ApplyMergeFieldsParams {
	return sqlcgen.ApplyMergeFieldsParams{
		FullName:               m["full_name"],
		BirthDate:              m["birth_date"],
		BirthPlace:             m["birth_place"],
		Gender:                 m["gender"],
		BloodType:              m["blood_type"],
		MaritalStatus:          m["marital_status"],
		ReligionConceptID:      m["religion_concept_id"],
		EducationConceptID:     m["education_concept_id"],
		EthnicityConceptID:     m["ethnicity_concept_id"],
		Phone:                  m["phone"],
		Email:                  m["email"],
		Address:                m["address"],
		FamilyID:               m["family_id"],
		PhotoUrl:               m["photo_url"],
		RegionVillageConceptID: m["region_village_concept_id"],
		CitizenshipConceptID:   m["citizenship_concept_id"],
		JobConceptID:           m["job_concept_id"],
		MaritalStatusConceptID: m["marital_status_concept_id"],
		BloodTypeConceptID:     m["blood_type_concept_id"],
		Nik:                    m["nik"],
		NikSearchHash:          m["nik_search_hash"],
		IdentityTypeConceptID:  m["identity_type_concept_id"],
		Actor:                  actor,
		ID:                     id,
	}
}

// maskNIK decrypts a stored ciphertext and returns only the last 4 digits.
func maskNIK(ctx context.Context, tx pgx.Tx, ciphertext, key string) (string, error) {
	plain, err := decryptNIK(ctx, tx, pgtype.Text{String: ciphertext, Valid: ciphertext != ""}, key)
	if err != nil || plain == "" {
		return "", err
	}
	last4 := plain
	if len(plain) > 4 {
		last4 = plain[len(plain)-4:]
	}
	return "************" + last4, nil
}

// mergeIdentityFromMap extracts what checkMergeIdentity needs.
func mergeIdentityFromMap(m map[string]string) mergeIdentity {
	return mergeIdentity{
		IdentityHash: m["nik_search_hash"],
		IdentityType: m["identity_type_concept_id"],
		BirthDate:    m["birth_date"],
		Gender:       m["gender"],
	}
}

// mergeSideJSON builds the preview side object for one record.
func mergeSideJSON(ctx context.Context, tx pgx.Tx, row sqlcgen.GetPersonMergeFieldsRow, key string) (gin.H, error) {
	nikMasked, err := maskNIK(ctx, tx, row.Nik, key)
	if err != nil {
		return nil, err
	}
	return gin.H{
		"id":                row.ID,
		"medical_record_no": row.MedicalRecordNo,
		"full_name":         row.FullName,
		"birth_date":        row.BirthDate,
		"gender":            row.Gender,
		"phone":             row.Phone,
		"address":           row.Address,
		"created_at":        row.CreatedAt,
		"updated_at":        row.UpdatedAt,
		"row_version":       row.RowVersion,
		"nik_masked":        nikMasked,
	}, nil
}

// MergePreviewHandler godoc
// @Summary Preview a person merge
// @Description Compares two duplicate records: identity check, conflicting fields, autofill, moved-row counts and blockers.
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param survivor_id query string true "Survivor person UUID"
// @Param merged_id query string true "Merged person UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 403 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /persons/merge-preview [get]
func MergePreviewHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonMerge) {
			return
		}
		survivorID, ok1 := parseUUID(c.Query("survivor_id"))
		mergedID, ok2 := parseUUID(c.Query("merged_id"))
		if !ok1 || !ok2 || survivorID == mergedID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		tx := TxFromContext(c)
		q := sqlcgen.New(tx)
		ctx := c.Request.Context()
		sMap, sRow, err := loadMergeSide(ctx, q, survivorID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		mMap, mRow, err := loadMergeSide(ctx, q, mergedID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if sRow.CompanyID != AuthCompanyID(c) || mRow.CompanyID != AuthCompanyID(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		survivorSide, err := mergeSideJSON(ctx, tx, sRow, cfg.NIKEncryptionKey)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		mergedSide, err := mergeSideJSON(ctx, tx, mRow, cfg.NIKEncryptionKey)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		check := checkMergeIdentity(mergeIdentityFromMap(sMap), mergeIdentityFromMap(mMap))
		similarity, err := q.NameSimilarity(ctx, sqlcgen.NameSimilarityParams{ID: survivorID, ID_2: mergedID})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if similarity < 0.5 {
			check.Warnings = append(check.Warnings, "name_differs")
		}
		var conflicts []gin.H
		var autofill []string
		for _, f := range mergeFields {
			if f == "identity" {
				sv, mv := identityKey(sMap), identityKey(mMap)
				if sv != "" && mv != "" && sv != mv {
					conflicts = append(conflicts, gin.H{"field": f, "survivor": survivorSide["nik_masked"], "merged": mergedSide["nik_masked"]})
				}
				if sv == "" && mv != "" {
					autofill = append(autofill, f)
				}
				continue
			}
			sv, mv := sMap[f], mMap[f]
			if sv != "" && mv != "" && sv != mv {
				conflicts = append(conflicts, gin.H{"field": f, "survivor": sv, "merged": mv})
			}
			if sv == "" && mv != "" {
				autofill = append(autofill, f)
			}
		}
		counts, err := q.PersonMergeCounts(ctx, mergedID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		blockers, err := q.PersonMergeBlockers(ctx, mergedID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if conflicts == nil {
			conflicts = []gin.H{}
		}
		if autofill == nil {
			autofill = []string{}
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"survivor":       survivorSide,
			"merged":         mergedSide,
			"identity_check": check,
			"conflicts":      conflicts,
			"autofill":       autofill,
			"counts":         json.RawMessage(counts),
			"blockers":       json.RawMessage(blockers),
		}})
	}
}

type mergeExpectedRowVersions struct {
	Survivor int32 `json:"survivor"`
	Merged   int32 `json:"merged"`
}

type mergePersonRequest struct {
	SurvivorID          string                   `json:"survivor_id"`
	MergedID            string                   `json:"merged_id"`
	ExpectedRowVersions mergeExpectedRowVersions `json:"expected_row_versions"`
	FieldChoices        map[string]string        `json:"field_choices"`
	Reason              string                   `json:"reason"`
	VerifiedDocuments   []string                 `json:"verified_documents"`
	ConfirmMRN          string                   `json:"confirm_mrn"`
}

// MergePersonsHandler godoc
// @Summary Merge two duplicate persons
// @Description Executes the merge in one transaction: identity + survivorship + blocker checks, row move, soft-delete of the merged record, merge log.
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body mergePersonRequest true "Merge request"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /persons/merge [post]
func MergePersonsHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPersonMerge) {
			return
		}
		var req mergePersonRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		survivorID, ok1 := parseUUID(req.SurvivorID)
		mergedID, ok2 := parseUUID(req.MergedID)
		if !ok1 || !ok2 || survivorID == mergedID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
			return
		}
		if req.VerifiedDocuments == nil {
			req.VerifiedDocuments = []string{}
		}
		for _, d := range req.VerifiedDocuments {
			if !verifiedDocumentTypes[d] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "unknown verified document type: " + d})
				return
			}
		}
		tx := TxFromContext(c)
		q := sqlcgen.New(tx)
		ctx := c.Request.Context()

		// 2. Lock both persons (company RLS: a foreign company row simply
		// does not come back → 404).
		locked, err := q.LockPersonsForMerge(ctx, []pgtype.UUID{survivorID, mergedID})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		byID := map[string]sqlcgen.LockPersonsForMergeRow{}
		for _, p := range locked {
			byID[p.ID.String()] = p
		}
		sp, sok := byID[survivorID.String()]
		mp, mok := byID[mergedID.String()]
		if !sok || !mok {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if live, _ := sp.IsLive.(bool); !live {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if live, _ := mp.IsLive.(bool); !live {
			c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
			return
		}
		if sp.RowVersion != req.ExpectedRowVersions.Survivor || mp.RowVersion != req.ExpectedRowVersions.Merged {
			c.JSON(http.StatusConflict, gin.H{"error": "patient data changed, reload the preview"})
			return
		}
		if req.ConfirmMRN != mp.MedicalRecordNo.String {
			c.JSON(http.StatusBadRequest, gin.H{"error": "confirmation does not match the medical record number"})
			return
		}

		// 3. Identity check (spec §5).
		sMap, _, err := loadMergeSide(ctx, q, survivorID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		mMap, _, err := loadMergeSide(ctx, q, mergedID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		check := checkMergeIdentity(mergeIdentityFromMap(sMap), mergeIdentityFromMap(mMap))
		if check.Result == "blocked" {
			c.JSON(http.StatusConflict, gin.H{"error": "identity check failed", "reasons": check.Reasons})
			return
		}
		if check.Result == "needs_documents" && len(req.VerifiedDocuments) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "verified documents are required"})
			return
		}

		// 4. Survivorship (spec §6).
		final, applied, missing := resolveSurvivorship(sMap, mMap, req.FieldChoices, survivorID.String(), mergedID.String())
		if len(missing) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "field choices required", "fields": missing})
			return
		}

		// 5. Blockers.
		blockersRaw, err := q.PersonMergeBlockers(ctx, mergedID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var blockers struct {
			ActiveVisits    int64 `json:"active_visits"`
			LinkedAccount   bool  `json:"linked_account"`
			LinkedPhysician bool  `json:"linked_physician"`
		}
		if err := json.Unmarshal(blockersRaw, &blockers); err != nil {
			respondInternalError(c, err)
			return
		}
		if blockers.ActiveVisits > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "merged record has an active visit"})
			return
		}
		if blockers.LinkedAccount || blockers.LinkedPhysician {
			c.JSON(http.StatusConflict, gin.H{"error": "merged record is linked to a user account or physician; keep it as the surviving record"})
			return
		}

		// 6. Snapshot both rows before anything changes.
		sSnap, err := q.PersonSnapshot(ctx, survivorID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		mSnap, err := q.PersonSnapshot(ctx, mergedID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		snapshot, err := json.Marshal(map[string]json.RawMessage{"survivor": sSnap, "merged": mSnap})
		if err != nil {
			respondInternalError(c, err)
			return
		}

		// 7. Move all B3 rows (SECURITY DEFINER, cross-merchant).
		moved, err := q.PersonMergeMove(ctx, sqlcgen.PersonMergeMoveParams{SurvivorID: survivorID, MergedID: mergedID})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				c.JSON(http.StatusConflict, gin.H{"error": pgErr.Message})
				return
			}
			respondInternalError(c, err)
			return
		}

		// 8. Soft-delete merged first (unique NIK index), then apply survivorship.
		if err := q.SoftDeleteMergedPerson(ctx, sqlcgen.SoftDeleteMergedPersonParams{Actor: AuthUserID(c), SurvivorID: survivorID, MergedID: mergedID}); err != nil {
			respondInternalError(c, err)
			return
		}
		if err := q.ApplyMergeFields(ctx, mergeFieldParams(final, AuthUserID(c), survivorID)); err != nil {
			respondInternalError(c, err)
			return
		}

		// 9. Merge log + duplicate pair.
		appliedJSON, err := json.Marshal(applied)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		logRow, err := q.InsertMergeLog(ctx, sqlcgen.InsertMergeLogParams{
			SurvivingPersonID: survivorID, MergedPersonID: mergedID,
			Reason: optText(strings.TrimSpace(req.Reason)), MergedBy: AuthUserID(c),
			IdentityCheck:     optText(check.Result),
			VerifiedDocuments: req.VerifiedDocuments,
			FieldChoices:      appliedJSON, Snapshot: snapshot, Moved: moved,
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if err := q.MarkDuplicatePairMerged(ctx, sqlcgen.MarkDuplicatePairMergedParams{Actor: AuthUserID(c), A: survivorID, B: mergedID}); err != nil {
			respondInternalError(c, err)
			return
		}

		// 10. Response.
		var movedMap map[string][]string
		if err := json.Unmarshal(moved, &movedMap); err != nil {
			respondInternalError(c, err)
			return
		}
		movedCounts := map[string]int{}
		for table, ids := range movedMap {
			movedCounts[table] = len(ids)
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"log_id":       logRow.ID,
			"survivor_id":  survivorID,
			"moved_counts": movedCounts,
		}})
	}
}

// mergeLogJSON renders a log row for list responses; the raw snapshot is never
// echoed back (it can contain the encrypted NIK ciphertext).
func mergeLogJSON(l sqlcgen.ListMergeLogsRow) gin.H {
	return gin.H{
		"id":                  l.ID,
		"surviving_person_id": l.SurvivingPersonID,
		"merged_person_id":    l.MergedPersonID,
		"reason":              l.Reason,
		"merged_at":           l.MergedAt,
		"merged_by":           l.MergedBy,
		"action":              l.Action,
		"merge_log_id":        l.MergeLogID,
		"identity_check":      l.IdentityCheck,
		"verified_documents":  l.VerifiedDocuments,
		"field_choices":       json.RawMessage(l.FieldChoices),
		"moved":               json.RawMessage(l.Moved),
		"reverted_at":         l.RevertedAt,
		"survivor_mrn":        l.SurvivorMrn,
		"survivor_name":       l.SurvivorName,
		"merged_mrn":          l.MergedMrn,
		"merged_name":         l.MergedName,
	}
}

// ListMergeLogsHandler godoc
// @Summary List person merge/unmerge logs
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Page size (default 50, max 200)"
// @Param offset query int false "Page offset"
// @Success 200 {object} apiResponse
// @Router /persons/merge-logs [get]
func ListMergeLogsHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonMerge) {
		return
	}
	limit, offset := paginationParams(c)
	q := sqlcgen.New(TxFromContext(c))
	logs, err := q.ListMergeLogs(c.Request.Context(), sqlcgen.ListMergeLogsParams{Limit: limit, Offset: offset})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		rows = append(rows, mergeLogJSON(l))
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "meta": gin.H{"limit": limit, "offset": offset}})
}

// ListPersonMergeLogsHandler godoc
// @Summary List merge logs involving one person
// @Tags person
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /persons/{id}/merge-logs [get]
func ListPersonMergeLogsHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonMerge) {
		return
	}
	id, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	logs, err := q.ListMergeLogsByPerson(c.Request.Context(), id)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	rows := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		rows = append(rows, gin.H{
			"id":                  l.ID,
			"surviving_person_id": l.SurvivingPersonID,
			"merged_person_id":    l.MergedPersonID,
			"reason":              l.Reason,
			"merged_at":           l.MergedAt,
			"merged_by":           l.MergedBy,
			"action":              l.Action,
			"merge_log_id":        l.MergeLogID,
			"identity_check":      l.IdentityCheck,
			"verified_documents":  l.VerifiedDocuments,
			"field_choices":       json.RawMessage(l.FieldChoices),
			"moved":               json.RawMessage(l.Moved),
			"reverted_at":         l.RevertedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "meta": gin.H{}})
}

type unmergePersonRequest struct {
	Reason string `json:"reason"`
}

// snapStr reads a snapshot JSON value; JSON null / absent → "".
func snapStr(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

// UnmergePersonHandler godoc
// @Summary Undo a person merge
// @Description Restores the merged record from the merge log snapshot. The survivor's identity is returned to the snapshot values first (unique NIK index), and for a same_identity merge the merged record's NIK is cleared.
// @Tags person
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Merge log UUID"
// @Param request body unmergePersonRequest true "Unmerge reason"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /persons/merge-logs/{id}/unmerge [post]
func UnmergePersonHandler(c *gin.Context) {
	if !RequirePermission(c, PermPersonUnmerge) {
		return
	}
	logID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req unmergePersonRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required"})
		return
	}
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()

	// 1. Lock the log row and both persons.
	logRow, err := q.GetMergeLogForUpdate(ctx, logID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "merge log not found"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if logRow.Action != "merge" || logRow.RevertedAt.Valid {
		c.JSON(http.StatusConflict, gin.H{"error": "merge already reverted or not a merge"})
		return
	}
	later, err := q.CountLaterOpenMerges(ctx, sqlcgen.CountLaterOpenMergesParams{
		LogID: logID, MergedAt: logRow.MergedAt,
		Ids: []pgtype.UUID{logRow.SurvivingPersonID, logRow.MergedPersonID},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if later > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "undo the later merge first"})
		return
	}
	locked, err := q.LockPersonsForMerge(ctx, []pgtype.UUID{logRow.SurvivingPersonID, logRow.MergedPersonID})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(locked) != 2 {
		c.JSON(http.StatusNotFound, gin.H{"error": "person not found"})
		return
	}
	for _, p := range locked {
		if p.ID == logRow.SurvivingPersonID {
			if live, _ := p.IsLive.(bool); !live {
				c.JSON(http.StatusConflict, gin.H{"error": "surviving record is no longer live"})
				return
			}
		}
	}

	// Snapshot is read into raw + decoded form.
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(logRow.Snapshot, &snapshot); err != nil {
		respondInternalError(c, err)
		return
	}
	var snapSurvivor, snapMerged map[string]any
	if err := json.Unmarshal(snapshot["survivor"], &snapSurvivor); err != nil {
		respondInternalError(c, err)
		return
	}
	if err := json.Unmarshal(snapshot["merged"], &snapMerged); err != nil {
		respondInternalError(c, err)
		return
	}
	var choices map[string]string
	if err := json.Unmarshal(logRow.FieldChoices, &choices); err != nil {
		respondInternalError(c, err)
		return
	}

	identityCleared := logRow.IdentityCheck.String == "same_identity"

	// 1b. NIK collision pre-check: the restore below writes snapshot NIK
	// hashes back onto live rows. If another live record has since taken
	// one of those hashes (uq_person_nik_hash), refuse BEFORE any write
	// with a 409 naming the colliding medical record number.
	exclude := []pgtype.UUID{logRow.SurvivingPersonID, logRow.MergedPersonID}
	checkNIKHash := func(hash string) bool {
		if hash == "" {
			return true
		}
		dup, err := q.FindPersonByNIKHashNotIn(ctx, sqlcgen.FindPersonByNIKHashNotInParams{
			CompanyID: AuthCompanyID(c), NikSearchHash: optText(hash), ExcludeIds: exclude,
		})
		if err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("nik already registered to medical record %s", dup.MedicalRecordNo)})
			return false
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			respondInternalError(c, err)
			return false
		}
		return true
	}
	if !identityCleared && !checkNIKHash(snapStr(snapMerged, "nik_search_hash")) {
		return
	}
	if !checkNIKHash(snapStr(snapSurvivor, "nik_search_hash")) {
		return
	}

	// 2. Identity of the survivor FIRST: if the survivor received the merged
	// record's identity during the merge, give it back the snapshot values so
	// the unique NIK index never sees two live rows with the same hash.
	if choices["identity"] == "autofill" || choices["identity"] == "merged" {
		cur, _, err := loadMergeSide(ctx, q, logRow.SurvivingPersonID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		cur["nik"] = snapStr(snapSurvivor, "nik")
		cur["nik_search_hash"] = snapStr(snapSurvivor, "nik_search_hash")
		cur["identity_type_concept_id"] = snapStr(snapSurvivor, "identity_type_concept_id")
		if err := q.ApplyMergeFields(ctx, mergeFieldParams(cur, AuthUserID(c), logRow.SurvivingPersonID)); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_person_nik_hash" {
				c.JSON(http.StatusConflict, gin.H{"error": "nik already registered to another medical record"})
				return
			}
			respondInternalError(c, err)
			return
		}
	}

	// 3. Restore the merged record and apply its snapshot fields.
	if err := q.RestoreMergedPerson(ctx, sqlcgen.RestoreMergedPersonParams{Actor: AuthUserID(c), ID: logRow.MergedPersonID}); err != nil {
		respondInternalError(c, err)
		return
	}
	mergedValues := map[string]string{}
	for k := range snapMerged {
		mergedValues[k] = snapStr(snapMerged, k)
	}
	if identityCleared {
		// The same NIK must not live on two records: the merged record is
		// restored without its identity number.
		mergedValues["nik"], mergedValues["nik_search_hash"], mergedValues["identity_type_concept_id"] = "", "", ""
	}
	if err := q.ApplyMergeFields(ctx, mergeFieldParams(mergedValues, AuthUserID(c), logRow.MergedPersonID)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_person_nik_hash" {
			c.JSON(http.StatusConflict, gin.H{"error": "nik already registered to another medical record"})
			return
		}
		respondInternalError(c, err)
		return
	}

	// 4. Move rows back, mark the merge reverted, write the unmerge log.
	result, err := q.PersonUnmergeMove(ctx, logID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.MarkMergeLogReverted(ctx, sqlcgen.MarkMergeLogRevertedParams{ID: logID, RevertedBy: AuthUserID(c)}); err != nil {
		respondInternalError(c, err)
		return
	}
	unmergeLog, err := q.InsertUnmergeLog(ctx, sqlcgen.InsertUnmergeLogParams{
		SurvivingPersonID: logRow.SurvivingPersonID, MergedPersonID: logRow.MergedPersonID,
		Reason: optText(strings.TrimSpace(req.Reason)), MergedBy: AuthUserID(c),
		MergeLogID: logID, Moved: result,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}

	// 5. Response.
	admissions, err := q.CountAdmissionsSince(ctx, sqlcgen.CountAdmissionsSinceParams{
		PersonID: logRow.SurvivingPersonID, CreatedAt: logRow.MergedAt,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Only the fields the history screen compares are echoed back; the raw
	// snapshot (encrypted NIK ciphertext) is never returned — same rule as
	// mergeLogJSON.
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"log_id":                 unmergeLog.ID,
		"result":                 json.RawMessage(result),
		"identity_cleared":       identityCleared,
		"admissions_after_merge": admissions,
		"survivor_snapshot": gin.H{
			"full_name":      snapStr(snapSurvivor, "full_name"),
			"birth_date":     snapStr(snapSurvivor, "birth_date"),
			"birth_place":    snapStr(snapSurvivor, "birth_place"),
			"gender":         snapStr(snapSurvivor, "gender"),
			"blood_type":     snapStr(snapSurvivor, "blood_type"),
			"marital_status": snapStr(snapSurvivor, "marital_status"),
			"phone":          snapStr(snapSurvivor, "phone"),
			"email":          snapStr(snapSurvivor, "email"),
			"address":        snapStr(snapSurvivor, "address"),
		},
	}})
}
