// internal/server/kebidanan_care.go
// Kebidanan (ANC F1): episode kehamilan per person — plan
// 2026-11-22-kebidanan-anc-f1 Task 2, spec §5. Pola gizi_care.go: tx ambient
// RunRequestTx (respons ≥400 rollback), semua error path setelah write pertama
// = respondInternalError (rollback ambient, profil).
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// PermClinicalMidwiferyWrite didefinisikan di clinical_form.go (blok konstanta
// clinical module permissions; kode dari migration 000109 — view memakai
// PermClinicalRecordView existing, persis pola gizi; TIDAK ada view baru).

// kebKind is the care_episode kind of a pregnancy episode (CHECK 'kehamilan').
const kebKind = "kehamilan"

func RegisterKebidananCareRoutes(rg *gin.RouterGroup) {
	rg.POST("/clinical/midwifery/episodes", CreateKebidananEpisodeHandler)
	rg.GET("/clinical/midwifery/episode", GetKebidananEpisodeHandler)
	rg.PATCH("/clinical/midwifery/episode/:id", UpdateKebidananEpisodeHandler)
	rg.POST("/clinical/midwifery/episode/:id/close", CloseKebidananEpisodeHandler)
	rg.GET("/clinical/midwifery/episodes", AccessLog("midwifery_episode", "view", "person_id"), ListKebidananEpisodesHandler)
}

type createKebidananEpisodeRequest struct {
	AdmissionID       string  `json:"admission_id" binding:"required"`
	Gravida           *int    `json:"gravida"`
	Para              *int    `json:"para"`
	Abortus           *int    `json:"abortus"`
	RiwayatPersalinan *string `json:"riwayat_persalinan"`
	JarakKehamilanTh  *int    `json:"jarak_kehamilan_th"`
	Hpht              string  `json:"hpht" binding:"required"`
	CloseActive       *bool   `json:"close_active"`
}

// newPregnancyRequest = payload nested close (step-2).
type newPregnancyRequest struct {
	Gravida           *int    `json:"gravida"`
	Para              *int    `json:"para"`
	Abortus           *int    `json:"abortus"`
	RiwayatPersalinan *string `json:"riwayat_persalinan"`
	JarakKehamilanTh  *int    `json:"jarak_kehamilan_th"`
	Hpht              string  `json:"hpht" binding:"required"`
}

// kebCoreFields validates gravida/para/abortus/hpht (wajib) shared by create
// and close-with-new-pregnancy; returns the parsed HPHT date.
func kebCoreFields(c *gin.Context, gravida, para, abortus *int, hpht string) (time.Time, bool) {
	if gravida == nil || para == nil || abortus == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gravida, para, abortus wajib"})
		return time.Time{}, false
	}
	h, err := time.Parse("2006-01-02", hpht)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hpht tidak valid"})
		return time.Time{}, false
	}
	return h, true
}

func kebTextPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func kebIntPtr(i *int) pgtype.Int4 {
	if i == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*i), Valid: true}
}

// kebHPL = HPHT + 280 hari (tanggal murni; kontrak Task 2 step 3).
func kebHPL(hpht time.Time) pgtype.Date {
	return pgtype.Date{Time: hpht.AddDate(0, 0, 280), Valid: true}
}

// kebCreateEpisode writes care_episode + midwifery_pregnancy in the ambient
// tx: ordinal = Count+1 (server, dalam tx create — spec §4.1), UK/HPL server.
func kebCreateEpisode(c *gin.Context, q *sqlcgen.Queries, companyID, merchantID, personID pgtype.UUID,
	gravida, para, abortus int, riwayat *string, jarak *int, hpht time.Time) (sqlcgen.OperationsCareEpisode, sqlcgen.ClinicalMidwiferyPregnancy, bool) {
	ctx := c.Request.Context()
	count, err := q.CountCareEpisodePersonKind(ctx, sqlcgen.CountCareEpisodePersonKindParams{PersonID: personID, Kind: kebKind})
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsCareEpisode{}, sqlcgen.ClinicalMidwiferyPregnancy{}, false
	}
	ep, err := q.CreateCareEpisode(ctx, sqlcgen.CreateCareEpisodeParams{
		CompanyID: companyID, MerchantID: merchantID, PersonID: personID,
		Kind: kebKind, OrdinalNo: int32(count + 1),
	})
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsCareEpisode{}, sqlcgen.ClinicalMidwiferyPregnancy{}, false
	}
	preg, err := q.CreateMidwiferyPregnancy(ctx, sqlcgen.CreateMidwiferyPregnancyParams{
		CompanyID: companyID, MerchantID: merchantID, CareEpisodeID: ep.ID,
		Gravida: int32(gravida), Para: int32(para), Abortus: int32(abortus),
		RiwayatPersalinan: kebTextPtr(riwayat), JarakKehamilanTh: kebIntPtr(jarak),
		Hpht: pgtype.Date{Time: hpht, Valid: true}, UkSource: "hpht", Hpl: kebHPL(hpht),
	})
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsCareEpisode{}, sqlcgen.ClinicalMidwiferyPregnancy{}, false
	}
	return ep, preg, true
}

// kebEpisodeByID reads one care_episode row inline (Task 1 queries have no
// GetByID; pola raw SQL validateTindakanConcepts). Merchant mismatch = 404 —
// cek eksplisit pola giziAdmission (RLS bukan satu-satunya guard).
func kebEpisodeByID(c *gin.Context, merchantID pgtype.UUID, id pgtype.UUID) (sqlcgen.OperationsCareEpisode, bool) {
	var ep sqlcgen.OperationsCareEpisode
	err := TxFromContext(c).QueryRow(c.Request.Context(), `
		SELECT id, company_id, merchant_id, person_id, kind, ordinal_no, status,
		       started_at, closed_at, close_context, row_version, created_at, updated_at
		FROM operations.care_episode WHERE id = $1`, id).Scan(
		&ep.ID, &ep.CompanyID, &ep.MerchantID, &ep.PersonID, &ep.Kind, &ep.OrdinalNo,
		&ep.Status, &ep.StartedAt, &ep.ClosedAt, &ep.CloseContext, &ep.RowVersion,
		&ep.CreatedAt, &ep.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ep.MerchantID != merchantID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode tidak ditemukan"})
		return sqlcgen.OperationsCareEpisode{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.OperationsCareEpisode{}, false
	}
	return ep, true
}

// kebLatestVisit loads the newest ANC visit of an episode (freshness header);
// no visit yet → (nil, true). DB error → 500 + (nil, false): caller must stop.
func kebLatestVisit(c *gin.Context, q *sqlcgen.Queries, episodeID pgtype.UUID) (*sqlcgen.ClinicalMidwiferyAncVisit, bool) {
	v, err := q.GetLatestAncVisit(c.Request.Context(), episodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true
	}
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	return &v, true
}

// kebUKWeeks: usg override bila uk_source='usg_t1' + nilai terisi, else
// hitung dari hpht vs now (zona merchant), clamp ≥ 0.
func kebUKWeeks(preg sqlcgen.ClinicalMidwiferyPregnancy, loc *time.Location) any {
	if preg.UkSource == "usg_t1" && preg.UkUsgT1Weeks.Valid {
		if v, err := preg.UkUsgT1Weeks.Float64Value(); err == nil {
			return v
		}
	}
	if !preg.Hpht.Valid {
		return nil
	}
	weeks := int(time.Now().In(loc).Sub(preg.Hpht.Time).Hours() / (7 * 24))
	if weeks < 0 {
		weeks = 0
	}
	return weeks
}

// episodeJSON — kontrak Task 2 step 3 (snake_case).
func kebEpisodeJSON(ep sqlcgen.OperationsCareEpisode, preg sqlcgen.ClinicalMidwiferyPregnancy, loc *time.Location, latest *sqlcgen.ClinicalMidwiferyAncVisit) gin.H {
	var latestVisit any
	if latest != nil {
		latestVisit = gin.H{"visited_at": tsJSON(latest.VisitedAt), "tfu_cm": numericJSON(latest.TfuCm), "djj": intText(latest.Djj)}
	}
	return gin.H{
		"id":                 uuidJSON(ep.ID),
		"ordinal_no":         ep.OrdinalNo,
		"status":             ep.Status,
		"gravida":            preg.Gravida,
		"para":               preg.Para,
		"abortus":            preg.Abortus,
		"riwayat_persalinan": textJSON(preg.RiwayatPersalinan),
		"jarak_kehamilan_th": int4Text(preg.JarakKehamilanTh),
		"hpht":               dateJSON(preg.Hpht),
		"hpl":                dateJSON(preg.Hpl),
		"uk_weeks":           kebUKWeeks(preg, loc),
		"uk_source":          preg.UkSource,
		"row_version":        preg.RowVersion,
		"latest_visit":       latestVisit,
		"person_id":          uuidJSON(ep.PersonID),
		"started_at":         tsJSON(ep.StartedAt),
		"closed_at":          tsJSON(ep.ClosedAt),
	}
}

// intText/int4Text render optional integer columns as JSON-friendly strings
// or nil (pola textJSON untuk pgtype.Int4).
func intText(v pgtype.Int4) any {
	if !v.Valid {
		return nil
	}
	return v.Int32
}

func int4Text(v pgtype.Int4) any { return intText(v) }

// CreateKebidananEpisodeHandler godoc
// @Summary Open a pregnancy (kehamilan) episode for an admission's person
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createKebidananEpisodeRequest true "Episode data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/midwifery/episodes [post]
func CreateKebidananEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	var req createKebidananEpisodeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	admissionID, okID := parseUUID(req.AdmissionID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	hpht, okH := kebCoreFields(c, req.Gravida, req.Para, req.Abortus, req.Hpht)
	if !okH {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okA := giziAdmission(c, q, merchantID, admissionID)
	if !okA {
		return
	}
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	// Guard episode aktif person (spec §5, pola gizi :1011-1026): tanpa
	// close_active → 409; dengan close_active → tutup aktif (close_context
	// {"reason":"ganti_episode"}) lalu create baru dalam tx yang sama.
	active, err := q.GetActiveCareEpisodeByPerson(c.Request.Context(), admission.PersonID)
	switch {
	case err == nil && req.CloseActive != nil && *req.CloseActive:
		ctx := []byte(`{"reason":"ganti_episode"}`)
		if _, err := q.CloseCareEpisode(c.Request.Context(), sqlcgen.CloseCareEpisodeParams{
			ID: active.ID, Status: "closed", ClosedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
			CloseContext: ctx, RowVersion: active.RowVersion,
		}); err != nil {
			respondInternalError(c, err)
			return
		}
	case err == nil:
		c.JSON(http.StatusConflict, gin.H{"error": "episode aktif masih ada"})
		return
	case !errors.Is(err, pgx.ErrNoRows):
		respondInternalError(c, err)
		return
	}
	ep, preg, okC := kebCreateEpisode(c, q, AuthCompanyID(c), merchantID, admission.PersonID,
		*req.Gravida, *req.Para, *req.Abortus, req.RiwayatPersalinan, req.JarakKehamilanTh, hpht)
	if !okC {
		return
	}
	latest, okL := kebLatestVisit(c, q, ep.ID)
	if !okL {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kebEpisodeJSON(ep, preg, loc, latest), "meta": gin.H{}})
}

// GetKebidananEpisodeHandler godoc
// @Summary Active pregnancy episode of an admission's person (null bila kosong)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param admission_id query string true "Admission UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/episode [get]
func GetKebidananEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	admissionID, okID := parseUUID(c.Query("admission_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okA := giziAdmission(c, q, merchantID, admissionID)
	if !okA {
		return
	}
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ep, err := q.GetActiveCareEpisodeByPerson(c.Request.Context(), admission.PersonID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"data": nil, "meta": gin.H{}})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	preg, err := q.GetMidwiferyPregnancy(c.Request.Context(), ep.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	latest, okL := kebLatestVisit(c, q, ep.ID)
	if !okL {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": kebEpisodeJSON(ep, preg, loc, latest), "meta": gin.H{}})
}

type updateKebidananEpisodeRequest struct {
	// Pointer + cek manual: row_version awal baris baru = 0 (trg_touch_row
	// hanya menaikkan pada UPDATE) — binding "required" akan menolak 0.
	RowVersion        *int32   `json:"row_version"`
	Gravida           *int     `json:"gravida"`
	Para              *int     `json:"para"`
	Abortus           *int     `json:"abortus"`
	RiwayatPersalinan *string  `json:"riwayat_persalinan"`
	JarakKehamilanTh  *int     `json:"jarak_kehamilan_th"`
	Hpht              *string  `json:"hpht"`
	UkSource          *string  `json:"uk_source"`
	UkUsgT1Weeks      *float64 `json:"uk_usg_t1_weeks"`
}

// UpdateKebidananEpisodeHandler godoc
// @Summary Patch pregnancy fields (row_version optimistic; HPL recompute)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Care episode UUID"
// @Param request body updateKebidananEpisodeRequest true "Pregnancy patch"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/midwifery/episode/{id} [patch]
func UpdateKebidananEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Param("id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}
	var req updateKebidananEpisodeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if req.RowVersion == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "row_version wajib"})
		return
	}
	if req.UkSource != nil && *req.UkSource != "hpht" && *req.UkSource != "usg_t1" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "uk_source tidak valid"})
		return
	}
	var hphtTime time.Time
	if req.Hpht != nil {
		t, err := time.Parse("2006-01-02", *req.Hpht)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hpht tidak valid"})
			return
		}
		hphtTime = t
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ep, okE := kebEpisodeByID(c, merchantID, episodeID)
	if !okE {
		return
	}
	preg, err := q.GetMidwiferyPregnancy(c.Request.Context(), episodeID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	gravida, para, abortus := int(preg.Gravida), int(preg.Para), int(preg.Abortus)
	if req.Gravida != nil {
		gravida = *req.Gravida
	}
	if req.Para != nil {
		para = *req.Para
	}
	if req.Abortus != nil {
		abortus = *req.Abortus
	}
	riwayat := preg.RiwayatPersalinan
	if req.RiwayatPersalinan != nil {
		riwayat = kebTextPtr(req.RiwayatPersalinan)
	}
	jarak := preg.JarakKehamilanTh
	if req.JarakKehamilanTh != nil {
		jarak = kebIntPtr(req.JarakKehamilanTh)
	}
	hpht := preg.Hpht
	if req.Hpht != nil {
		hpht = pgtype.Date{Time: hphtTime, Valid: true}
	}
	ukSource := preg.UkSource
	if req.UkSource != nil {
		ukSource = *req.UkSource
	}
	usgWeeks := preg.UkUsgT1Weeks
	if req.UkUsgT1Weeks != nil {
		usgWeeks = numericFromFloat(req.UkUsgT1Weeks)
	}
	// Recompute HPL bila hpht/uk_source berubah (hpl = hpht + 280 hari).
	hpl := preg.Hpl
	if req.Hpht != nil || req.UkSource != nil {
		hpl = kebHPL(hpht.Time)
	}
	updated, err := q.UpdateMidwiferyPregnancy(c.Request.Context(), sqlcgen.UpdateMidwiferyPregnancyParams{
		CareEpisodeID: episodeID, Gravida: int32(gravida), Para: int32(para), Abortus: int32(abortus),
		RiwayatPersalinan: riwayat, JarakKehamilanTh: jarak, Hpht: hpht,
		UkSource: ukSource, UkUsgT1Weeks: usgWeeks, Hpl: hpl, RowVersion: *req.RowVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "row_version konflik"})
		return
	}
	if err != nil {
		respondInternalError(c, err)
		return
	}
	latest, okL := kebLatestVisit(c, q, ep.ID)
	if !okL {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": kebEpisodeJSON(ep, updated, loc, latest), "meta": gin.H{}})
}

type closeKebidananEpisodeRequest struct {
	Confirm      *bool                `json:"confirm"`
	NewPregnancy *newPregnancyRequest `json:"new_pregnancy"`
}

// CloseKebidananEpisodeHandler godoc
// @Summary Close a pregnancy episode (step-2 confirm) + optional new pregnancy
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param id path string true "Care episode UUID"
// @Param request body closeKebidananEpisodeRequest true "Close payload"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/midwifery/episode/{id}/close [post]
func CloseKebidananEpisodeHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Param("id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}
	var req closeKebidananEpisodeRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	// Step-2 guard: konfirmasi wajib (kontrak Task 2 step 4).
	if req.Confirm == nil || !*req.Confirm {
		c.JSON(http.StatusBadRequest, gin.H{"error": "konfirmasi wajib"})
		return
	}
	var newHPHT time.Time
	if req.NewPregnancy != nil {
		t, okH := kebCoreFields(c, req.NewPregnancy.Gravida, req.NewPregnancy.Para, req.NewPregnancy.Abortus, req.NewPregnancy.Hpht)
		if !okH {
			return
		}
		newHPHT = t
	}
	q := sqlcgen.New(TxFromContext(c))
	loc, err := merchantLocation(c.Request.Context(), q, merchantID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ep, okE := kebEpisodeByID(c, merchantID, episodeID)
	if !okE {
		return
	}
	if ep.Status != "active" {
		c.JSON(http.StatusConflict, gin.H{"error": "episode tidak aktif"})
		return
	}
	// Write-1: close (close_context {"confirm":true} — spec §5). Semua error
	// path setelah ini = respondInternalError (rollback ambient tx).
	closed, err := q.CloseCareEpisode(c.Request.Context(), sqlcgen.CloseCareEpisodeParams{
		ID: ep.ID, Status: "closed", ClosedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		CloseContext: []byte(`{"confirm":true}`), RowVersion: ep.RowVersion,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	preg, err := q.GetMidwiferyPregnancy(c.Request.Context(), ep.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	latest, okL := kebLatestVisit(c, q, ep.ID)
	if !okL {
		return
	}
	data := gin.H{"closed": kebEpisodeJSON(closed, preg, loc, latest)}
	if req.NewPregnancy != nil {
		// Write-2 (ordinal+1 via count dalam tx — spec §4.1).
		newEp, newPreg, okC := kebCreateEpisode(c, q, ep.CompanyID, ep.MerchantID, ep.PersonID,
			*req.NewPregnancy.Gravida, *req.NewPregnancy.Para, *req.NewPregnancy.Abortus,
			req.NewPregnancy.RiwayatPersalinan, req.NewPregnancy.JarakKehamilanTh, newHPHT)
		if !okC {
			return
		}
		newLatest, okNL := kebLatestVisit(c, q, newEp.ID)
		if !okNL {
			return
		}
		data["new_episode"] = kebEpisodeJSON(newEp, newPreg, loc, newLatest)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// ListKebidananEpisodesHandler godoc
// @Summary Pregnancy episode history of one person (riwayat, desc ordinal)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /clinical/midwifery/episodes [get]
func ListKebidananEpisodesHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	personID, okID := parseUUID(c.Query("person_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListCareEpisodesByPerson(c.Request.Context(), personID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		// Cek merchant eksplisit (pola giziAdmission) — pool test superuser
		// mem-bypass RLS; jangan andalkan RLS sebagai satu-satunya guard.
		if r.MerchantID != merchantID {
			continue
		}
		visits, err := q.ListAncVisitsByEpisode(c.Request.Context(), r.ID)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		var outcome any
		var ctxMap map[string]any
		if json.Unmarshal(r.CloseContext, &ctxMap) == nil {
			if s, ok := ctxMap["outcome"].(string); ok {
				outcome = s
			}
		}
		data = append(data, gin.H{
			"id": uuidJSON(r.ID), "ordinal_no": r.OrdinalNo, "status": r.Status,
			"started_at": tsJSON(r.StartedAt), "closed_at": tsJSON(r.ClosedAt),
			"kunjungan_count": len(visits), "outcome": outcome,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
