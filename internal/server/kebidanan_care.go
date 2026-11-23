// internal/server/kebidanan_care.go
// Kebidanan (ANC F1): episode kehamilan per person — plan
// 2026-11-22-kebidanan-anc-f1 Task 2, spec §5. Pola gizi_care.go: tx ambient
// RunRequestTx (respons ≥400 rollback), semua error path setelah write pertama
// = respondInternalError (rollback ambient, profil).
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Task 3: kunjungan ANC, USG, skrining PE.
	rg.POST("/clinical/midwifery/anc-visit", CreateAncVisitHandler)
	rg.GET("/clinical/midwifery/anc-visit", ListAncVisitsHandler)
	rg.POST("/clinical/midwifery/usg", CreateKebidananUsgHandler)
	rg.GET("/clinical/midwifery/usg", ListKebidananUsgHandler)
	rg.POST("/clinical/midwifery/pe-screen", CreatePeScreenHandler)
	rg.GET("/clinical/midwifery/pe-screen", ListPeScreensHandler)
	// Task 4: imunisasi Td person + anc-summary read-model.
	rg.POST("/clinical/person-vaccination", CreatePersonVaccinationHandler)
	rg.GET("/clinical/person-vaccination", ListPersonVaccinationHandler)
	rg.GET("/clinical/midwifery/anc-summary", GetMidwiferyAncSummaryHandler)
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
		// Partial unique uq_care_episode_active (person_id, kind) WHERE active
		// tidak menyertakan merchant: guard GetActive (RLS per merchant) bisa
		// melewatkan episode aktif person yang sama di merchant lain satu
		// company → 409 pesan kontrak guard, bukan 500 (pola pgconn 23505 +
		// ConstraintName di person.go).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_care_episode_active" {
			c.JSON(http.StatusConflict, gin.H{"error": "episode aktif masih ada"})
			return sqlcgen.OperationsCareEpisode{}, sqlcgen.ClinicalMidwiferyPregnancy{}, false
		}
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

// ---- Task 3: kunjungan ANC, USG, skrining PE (spec §5, pola gizi_care) ----

type createAncVisitRequest struct {
	EpisodeID        string          `json:"episode_id" binding:"required"`
	AdmissionID      string          `json:"admission_id" binding:"required"`
	TfuCm            *float64        `json:"tfu_cm"`
	Djj              *int            `json:"djj"`
	Presentation     *string         `json:"presentation"`
	PlacentaLocation *string         `json:"placenta_location"`
	Inspekulo        json.RawMessage `json:"inspekulo"`
	TandaBahaya      json.RawMessage `json:"tanda_bahaya"`
	Jiwa             json.RawMessage `json:"jiwa"`
	ImunisasiLainnya *string         `json:"imunisasi_lainnya"`
	NextVisitDate    *string         `json:"next_visit_date"`
}

// kebValidJSON menolak jsonb request yang bukan JSON valid.
func kebValidJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || json.Valid(raw)
}

// kebValidateJiwa — kontrak Task 3 step 1: bentuk jiwa = {q1..q4:bool,
// tindak_lanjut ∈ 'edukasi','konseling','rujukan', null}; key lain ditolak.
func kebValidateJiwa(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	tl := map[string]bool{"edukasi": true, "konseling": true, "rujukan": true}
	for k, v := range m {
		switch {
		case k == "q1" || k == "q2" || k == "q3" || k == "q4":
			if _, ok := v.(bool); !ok {
				return false
			}
		case k == "tindak_lanjut":
			if v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok || !tl[s] {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// kebVisitJSON — snake_case (pola kebEpisodeJSON).
func kebVisitJSON(v sqlcgen.ClinicalMidwiferyAncVisit) gin.H {
	return gin.H{
		"id": uuidJSON(v.ID), "episode_id": uuidJSON(v.CareEpisodeID),
		"admission_id": uuidJSON(v.AdmissionID), "visited_at": tsJSON(v.VisitedAt),
		"tfu_cm": numericJSON(v.TfuCm), "djj": intText(v.Djj),
		"presentation": textJSON(v.Presentation), "placenta_location": textJSON(v.PlacentaLocation),
		"inspekulo": json.RawMessage(v.Inspekulo), "tanda_bahaya": json.RawMessage(v.TandaBahaya), "jiwa": json.RawMessage(v.Jiwa),
		"imunisasi_lainnya": textJSON(v.ImunisasiLainnya), "next_visit_date": dateJSON(v.NextVisitDate),
		"row_version": v.RowVersion,
	}
}

// CreateAncVisitHandler godoc
// @Summary Record one ANC visit (kunjungan) of a pregnancy episode
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createAncVisitRequest true "Visit data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 409 {object} apiErrorResponse
// @Router /clinical/midwifery/anc-visit [post]
func CreateAncVisitHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	var req createAncVisitRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	episodeID, okE := parseUUID(req.EpisodeID)
	if !okE {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	admissionID, okA := parseUUID(req.AdmissionID)
	if !okA {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid admission_id"})
		return
	}
	// Bentuk jsonb (kontrak): jiwa keys literal q1..q4 + tindak_lanjut.
	if !kebValidateJiwa(req.Jiwa) || !kebValidJSON(req.Inspekulo) || !kebValidJSON(req.TandaBahaya) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bentuk jsonb tidak valid"})
		return
	}
	if req.Presentation != nil && *req.Presentation != "kepala" && *req.Presentation != "sungsang" &&
		*req.Presentation != "melintang" && *req.Presentation != "belumdapat" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "presentation tidak valid"})
		return
	}
	if req.PlacentaLocation != nil && *req.PlacentaLocation != "fundus" && *req.PlacentaLocation != "corpus" &&
		*req.PlacentaLocation != "letak_rendah" && *req.PlacentaLocation != "previa" &&
		*req.PlacentaLocation != "belumdapat" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "placenta_location tidak valid"})
		return
	}
	var nextVisit pgtype.Date
	if req.NextVisitDate != nil {
		t, err := time.Parse("2006-01-02", *req.NextVisitDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "next_visit_date tidak valid"})
			return
		}
		nextVisit = pgtype.Date{Time: t, Valid: true}
	}
	q := sqlcgen.New(TxFromContext(c))
	admission, okAd := giziAdmission(c, q, merchantID, admissionID)
	if !okAd {
		return
	}
	ep, okEp := kebEpisodeByID(c, merchantID, episodeID)
	if !okEp {
		return
	}
	// Kontrak: episode milik person admission (mismatch → 409).
	if ep.PersonID != admission.PersonID {
		c.JSON(http.StatusConflict, gin.H{"error": "episode bukan milik person admission"})
		return
	}
	visit, err := q.CreateAncVisit(c.Request.Context(), sqlcgen.CreateAncVisitParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID,
		CareEpisodeID: episodeID, AdmissionID: admissionID,
		VisitedAt:        pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		TfuCm:            numericFromFloat(req.TfuCm),
		Djj:              kebIntPtr(req.Djj),
		Presentation:     kebTextPtr(req.Presentation),
		PlacentaLocation: kebTextPtr(req.PlacentaLocation),
		Inspekulo:        req.Inspekulo, TandaBahaya: req.TandaBahaya, Jiwa: req.Jiwa,
		ImunisasiLainnya: kebTextPtr(req.ImunisasiLainnya), NextVisitDate: nextVisit,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kebVisitJSON(visit), "meta": gin.H{}})
}

// ListAncVisitsHandler godoc
// @Summary ANC visits of an episode (visited_at DESC)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param episode_id query string true "Care episode UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/anc-visit [get]
func ListAncVisitsHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Query("episode_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okEp := kebEpisodeByID(c, merchantID, episodeID); !okEp {
		return
	}
	rows, err := q.ListAncVisitsByEpisode(c.Request.Context(), episodeID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, kebVisitJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

type createKebidananUsgRequest struct {
	EpisodeID string   `json:"episode_id" binding:"required"`
	Trimester string   `json:"trimester" binding:"required"`
	UsgDate   string   `json:"usg_date" binding:"required"`
	CrlMm     *float64 `json:"crl_mm"`
	GsMm      *float64 `json:"gs_mm"`
	BpdMm     *float64 `json:"bpd_mm"`
	HcMm      *float64 `json:"hc_mm"`
	AcMm      *float64 `json:"ac_mm"`
	FlMm      *float64 `json:"fl_mm"`
	EfwG      *int     `json:"efw_g"`
	Sdp       *string  `json:"sdp"`
}

func kebUsgJSON(u sqlcgen.ClinicalMidwiferyUsg) gin.H {
	return gin.H{
		"id": uuidJSON(u.ID), "episode_id": uuidJSON(u.CareEpisodeID),
		"trimester": u.Trimester, "usg_date": dateJSON(u.UsgDate),
		"crl_mm": numericJSON(u.CrlMm), "gs_mm": numericJSON(u.GsMm),
		"bpd_mm": numericJSON(u.BpdMm), "hc_mm": numericJSON(u.HcMm),
		"ac_mm": numericJSON(u.AcMm), "fl_mm": numericJSON(u.FlMm),
		"efw_g": intText(u.EfwG), "sdp": textJSON(u.Sdp), "row_version": u.RowVersion,
	}
}

// CreateKebidananUsgHandler godoc
// @Summary Record one USG examination of a pregnancy episode
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createKebidananUsgRequest true "USG data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/usg [post]
func CreateKebidananUsgHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	var req createKebidananUsgRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	episodeID, okID := parseUUID(req.EpisodeID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	if req.Trimester != "t1" && req.Trimester != "t3" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trimester tidak valid"})
		return
	}
	usgDate, err := time.Parse("2006-01-02", req.UsgDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usg_date tidak valid"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okEp := kebEpisodeByID(c, merchantID, episodeID); !okEp {
		return
	}
	usg, err := q.CreateMidwiferyUsg(c.Request.Context(), sqlcgen.CreateMidwiferyUsgParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, CareEpisodeID: episodeID,
		Trimester: req.Trimester, UsgDate: pgtype.Date{Time: usgDate, Valid: true},
		CrlMm: numericFromFloat(req.CrlMm), GsMm: numericFromFloat(req.GsMm),
		BpdMm: numericFromFloat(req.BpdMm), HcMm: numericFromFloat(req.HcMm),
		AcMm: numericFromFloat(req.AcMm), FlMm: numericFromFloat(req.FlMm),
		EfwG: kebIntPtr(req.EfwG), Sdp: kebTextPtr(req.Sdp),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kebUsgJSON(usg), "meta": gin.H{}})
}

// ListKebidananUsgHandler godoc
// @Summary USG records of an episode (usg_date DESC)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param episode_id query string true "Care episode UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/usg [get]
func ListKebidananUsgHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Query("episode_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okEp := kebEpisodeByID(c, merchantID, episodeID); !okEp {
		return
	}
	rows, err := q.ListUsgByEpisode(c.Request.Context(), episodeID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, kebUsgJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// kebPeFactors — 5 faktor literal Task 1 (spec §4.4). Missing key = false.
type kebPeFactors struct {
	Primigravida      *bool `json:"primigravida"`
	JarakGt10Th       *bool `json:"jarak_gt10th"`
	RiwayatPeKeluarga *bool `json:"riwayat_pe_keluarga"`
	ImtGt30           *bool `json:"imt_gt30"`
	Multipel          *bool `json:"multipel"`
}

type createPeScreenRequest struct {
	EpisodeID string        `json:"episode_id" binding:"required"`
	Factors   *kebPeFactors `json:"factors"`
}

func (f *kebPeFactors) countTrue() int {
	n := 0
	for _, b := range []*bool{f.Primigravida, f.JarakGt10Th, f.RiwayatPeKeluarga, f.ImtGt30, f.Multipel} {
		if b != nil && *b {
			n++
		}
	}
	return n
}

func (f *kebPeFactors) json() []byte {
	out, _ := json.Marshal(map[string]bool{
		"primigravida":        f.Primigravida != nil && *f.Primigravida,
		"jarak_gt10th":        f.JarakGt10Th != nil && *f.JarakGt10Th,
		"riwayat_pe_keluarga": f.RiwayatPeKeluarga != nil && *f.RiwayatPeKeluarga,
		"imt_gt30":            f.ImtGt30 != nil && *f.ImtGt30,
		"multipel":            f.Multipel != nil && *f.Multipel,
	})
	return out
}

// kebPeBand — RULE LITERAL kontrak Task 3 step 3: 0 faktor = 'rendah',
// 1 = 'sedang', ≥2 = 'tinggi'; rujukan_flag = band != 'rendah'.
func kebPeBand(n int) string {
	switch {
	case n >= 2:
		return "tinggi"
	case n == 1:
		return "sedang"
	default:
		return "rendah"
	}
}

func kebPeScreenJSON(s sqlcgen.ClinicalMidwiferyPeScreen) gin.H {
	var factors map[string]bool
	_ = json.Unmarshal(s.Factors, &factors)
	return gin.H{
		"id": uuidJSON(s.ID), "episode_id": uuidJSON(s.CareEpisodeID),
		"screened_at": tsJSON(s.ScreenedAt), "factors": factors,
		"band": s.Band, "rujukan_flag": s.RujukanFlag, "row_version": s.RowVersion,
	}
}

// CreatePeScreenHandler godoc
// @Summary PE risk screening of a pregnancy episode (band computed server-side)
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createPeScreenRequest true "Screening factors"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/pe-screen [post]
func CreatePeScreenHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	var req createPeScreenRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	episodeID, okID := parseUUID(req.EpisodeID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	if req.Factors == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factors wajib"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okEp := kebEpisodeByID(c, merchantID, episodeID); !okEp {
		return
	}
	n := req.Factors.countTrue()
	band := kebPeBand(n)
	screen, err := q.CreatePeScreen(c.Request.Context(), sqlcgen.CreatePeScreenParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, CareEpisodeID: episodeID,
		ScreenedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Factors:    req.Factors.json(), Band: band, RujukanFlag: band != "rendah",
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kebPeScreenJSON(screen), "meta": gin.H{}})
}

// ListPeScreensHandler godoc
// @Summary PE screenings of an episode (screened_at DESC)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param episode_id query string true "Care episode UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Router /clinical/midwifery/pe-screen [get]
func ListPeScreensHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalRecordView, merchantID) {
		return
	}
	episodeID, okID := parseUUID(c.Query("episode_id"))
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okEp := kebEpisodeByID(c, merchantID, episodeID); !okEp {
		return
	}
	rows, err := q.ListPeScreensByEpisode(c.Request.Context(), episodeID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		data = append(data, kebPeScreenJSON(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}

// ---- Task 4: imunisasi Td person + anc-summary read-model (spec §5) ----

type createPersonVaccinationRequest struct {
	PersonID         string  `json:"person_id" binding:"required"`
	AntigenSlug      string  `json:"antigen_slug" binding:"required"`
	GivenDate        string  `json:"given_date" binding:"required"`
	Tempat           string  `json:"tempat" binding:"required"`
	TempatKeterangan *string `json:"tempat_keterangan"`
	NoBatch          *string `json:"no_batch"`
	Pemberi          *string `json:"pemberi"`
}

// kebVaksinSchedule — ref.immunization_schedule rows of one antigen for
// kategori 'ibu_hamil' ordered by dosis_ke (seed migration 000109: Td 1–5).
func kebVaksinSchedule(c *gin.Context, q *sqlcgen.Queries, antigen string) ([]sqlcgen.RefImmunizationSchedule, bool) {
	rows, err := q.GetImmunizationSchedule(c.Request.Context(), sqlcgen.GetImmunizationScheduleParams{
		AntigenSlug: antigen, Kategori: "ibu_hamil",
	})
	if err != nil {
		respondInternalError(c, err)
		return nil, false
	}
	return rows, true
}

// kebVaksinBand — kontrak Task 4 step 1: dosis terakhir N → status 'aktif' +
// protect_until = year(given_date)+masa_perlindungan_tahun(N); riwayat kosong
// → 'belum_lengkap' + next.dosis_ke=1. next (jika jadwal punya dosis N+1) =
// {dosis_ke, min_date = last.given_date + interval_hari(N+1)}.
func kebVaksinBand(rows []sqlcgen.ClinicalPersonRiwayatVaksin, sched []sqlcgen.RefImmunizationSchedule) gin.H {
	band := gin.H{"status": "belum_lengkap", "protect_until": nil, "next": nil}
	if len(rows) == 0 {
		band["next"] = gin.H{"dosis_ke": 1, "min_date": nil}
		return band
	}
	last := rows[0]
	for _, r := range rows[1:] {
		if r.DosisKe > last.DosisKe {
			last = r
		}
	}
	var cur sqlcgen.RefImmunizationSchedule
	for _, s := range sched {
		if s.DosisKe == last.DosisKe {
			cur = s
		}
	}
	if cur.MasaPerlindunganTahun.Valid {
		band["status"] = "aktif"
		band["protect_until"] = fmt.Sprintf("%04d", last.GivenDate.Time.Year()+int(cur.MasaPerlindunganTahun.Int32))
	}
	if !cur.MasaPerlindunganTahun.Valid {
		// dosis 1 tanpa masa perlindungan di jadwal → belum_lengkap (KEPUTUSAN).
		next := gin.H{"dosis_ke": last.DosisKe + 1, "min_date": nil}
		for _, s := range sched {
			if s.DosisKe == last.DosisKe+1 {
				if s.IntervalHariDariDosisSebelumnya.Valid {
					minDate := last.GivenDate.Time.AddDate(0, 0, int(s.IntervalHariDariDosisSebelumnya.Int32))
					next["min_date"] = minDate.Format("2006-01-02")
				}
				break
			}
		}
		band["next"] = next
		return band
	}
	for _, s := range sched {
		if s.DosisKe == last.DosisKe+1 {
			next := gin.H{"dosis_ke": s.DosisKe, "min_date": nil}
			if s.IntervalHariDariDosisSebelumnya.Valid {
				minDate := last.GivenDate.Time.AddDate(0, 0, int(s.IntervalHariDariDosisSebelumnya.Int32))
				next["min_date"] = minDate.Format("2006-01-02")
			}
			band["next"] = next
			break
		}
	}
	return band
}

func kebVaksinJSON(v sqlcgen.ClinicalPersonRiwayatVaksin) gin.H {
	return gin.H{
		"id": uuidJSON(v.ID), "person_id": uuidJSON(v.PersonID), "antigen_slug": v.AntigenSlug,
		"dosis_ke": v.DosisKe, "given_date": dateJSON(v.GivenDate), "no_batch": textJSON(v.NoBatch),
		"tempat": textJSON(v.Tempat), "tempat_keterangan": textJSON(v.TempatKeterangan),
		"pemberi": textJSON(v.Pemberi), "row_version": v.RowVersion,
	}
}

// kebPersonByCompany — person ada + milik company token (pola guard tenant
// eksplisit giziAdmission; pool test superuser mem-bypass RLS).
func kebPersonByCompany(c *gin.Context, q *sqlcgen.Queries, personID pgtype.UUID) (sqlcgen.CorePerson, bool) {
	p, err := q.GetPersonByID(c.Request.Context(), personID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.CompanyID != AuthCompanyID(c)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "person tidak ditemukan"})
		return sqlcgen.CorePerson{}, false
	}
	if err != nil {
		respondInternalError(c, err)
		return sqlcgen.CorePerson{}, false
	}
	return p, true
}

// CreatePersonVaccinationHandler godoc
// @Summary Record one Td vaccination of a person (dosis_ke computed server-side)
// @Description dosis_ke = ordinal berikutnya dari riwayat person+antigen; given_date < last.given_date + interval_hari → 422 tanpa tersimpan (spec §5).
// @Tags clinical
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param request body createPersonVaccinationRequest true "Vaccination data"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Failure 404 {object} apiErrorResponse
// @Failure 422 {object} apiErrorResponse
// @Router /clinical/person-vaccination [post]
func CreatePersonVaccinationHandler(c *gin.Context) {
	merchantID, ok := requireMerchantHeader(c)
	if !ok {
		return
	}
	if !RequirePermissionForMerchant(c, PermClinicalMidwiferyWrite, merchantID) {
		return
	}
	var req createPersonVaccinationRequest
	if !bindJSONOrFields(c, &req) {
		return
	}
	if req.AntigenSlug != "td" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "antigen_slug hanya td"})
		return
	}
	if req.Tempat != "faskes_ini" && req.Tempat != "luar" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tempat tidak valid"})
		return
	}
	givenDate, err := time.Parse("2006-01-02", req.GivenDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "given_date tidak valid"})
		return
	}
	personID, okID := parseUUID(req.PersonID)
	if !okID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid person_id"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	if _, okP := kebPersonByCompany(c, q, personID); !okP {
		return
	}
	sched, okS := kebVaksinSchedule(c, q, req.AntigenSlug)
	if !okS {
		return
	}
	// dosis_ke server: ordinal berikutnya (lompat tidak mungkin — bukan input).
	rows, err := q.ListPersonVaksin(c.Request.Context(), sqlcgen.ListPersonVaksinParams{
		PersonID: personID, AntigenSlug: req.AntigenSlug,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	dosisKe := int32(len(rows) + 1)
	if len(rows) > 0 {
		// Guard keras interval (spec §5): given_date < last.given_date +
		// interval_hari(dosis_ke) → 422, TIDAK tersimpan (respons ≥400
		// auto-rollback ambient RunRequestTx).
		var cur sqlcgen.RefImmunizationSchedule
		for _, s := range sched {
			if s.DosisKe == dosisKe {
				cur = s
				break
			}
		}
		if cur.IntervalHariDariDosisSebelumnya.Valid {
			last := rows[0]
			minDate := last.GivenDate.Time.AddDate(0, 0, int(cur.IntervalHariDariDosisSebelumnya.Int32))
			if givenDate.Before(minDate) {
				remaining := int(minDate.Sub(givenDate).Hours() / 24)
				c.JSON(http.StatusUnprocessableEntity, gin.H{
					"error": fmt.Sprintf("interval Td belum terpenuhi (min %d hari)", remaining)})
				return
			}
		}
	}
	v, err := q.CreatePersonVaksin(c.Request.Context(), sqlcgen.CreatePersonVaksinParams{
		CompanyID: AuthCompanyID(c), MerchantID: merchantID, PersonID: personID,
		AntigenSlug: req.AntigenSlug, DosisKe: dosisKe,
		GivenDate: pgtype.Date{Time: givenDate, Valid: true},
		NoBatch:   kebTextPtr(req.NoBatch), Tempat: pgtype.Text{String: req.Tempat, Valid: true},
		TempatKeterangan: kebTextPtr(req.TempatKeterangan), Pemberi: kebTextPtr(req.Pemberi),
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": kebVaksinJSON(v), "meta": gin.H{}})
}

// ListPersonVaccinationHandler godoc
// @Summary Td vaccination history of a person + protection band (server)
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Param antigen query string true "Antigen slug (td)"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /clinical/person-vaccination [get]
func ListPersonVaccinationHandler(c *gin.Context) {
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
	antigen := c.Query("antigen")
	if antigen != "td" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "antigen hanya td"})
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	sched, okS := kebVaksinSchedule(c, q, antigen)
	if !okS {
		return
	}
	rows, err := q.ListPersonVaksin(c.Request.Context(), sqlcgen.ListPersonVaksinParams{
		PersonID: personID, AntigenSlug: antigen,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	data := make([]gin.H, 0, len(rows))
	own := make([]sqlcgen.ClinicalPersonRiwayatVaksin, 0, len(rows))
	for _, r := range rows {
		if r.MerchantID != merchantID {
			continue
		}
		own = append(own, r)
		// Kontrak dosis entry: dosis_ke, given_date, tempat, tempat_keterangan.
		data = append(data, gin.H{
			"dosis_ke": r.DosisKe, "given_date": dateJSON(r.GivenDate),
			"tempat": textJSON(r.Tempat), "tempat_keterangan": textJSON(r.TempatKeterangan),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"dosis": data, "band": kebVaksinBand(own, sched),
	}, "meta": gin.H{}})
}

// GetMidwiferyAncSummaryHandler godoc
// @Summary ANC summary read-model of a person's active pregnancy (all null-able)
// @Description Untuk tab Gizi (G4Bumil G4AncData): pregnancy aktif + latest visit + latest USG; tanpa episode aktif → 200 semua null.
// @Tags clinical
// @Produce json
// @Security BearerAuth
// @Param X-Merchant-ID header string true "Active merchant UUID"
// @Param person_id query string true "Person UUID"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /clinical/midwifery/anc-summary [get]
func GetMidwiferyAncSummaryHandler(c *gin.Context) {
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
	data := gin.H{
		"gravida": nil, "para": nil, "tfu_cm": nil, "tfu_at": nil,
		"djj": nil, "djj_at": nil, "usg_lk_mm": nil, "usg_fl_mm": nil,
		"usg_at": nil, "episode_ordinal": nil,
	}
	ep, err := q.GetActiveCareEpisodeByPerson(c.Request.Context(), personID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Tanpa episode aktif → 200 semua null (FE fallback slot lama).
		c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
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
	data["gravida"] = preg.Gravida
	data["para"] = preg.Para
	data["episode_ordinal"] = ep.OrdinalNo
	visit, okV := kebLatestVisit(c, q, ep.ID)
	if !okV {
		return
	}
	if visit != nil {
		data["tfu_cm"] = numericJSON(visit.TfuCm)
		data["djj"] = intText(visit.Djj)
		if visit.VisitedAt.Valid {
			data["tfu_at"] = visit.VisitedAt.Time.Format("2006-01-02")
			data["djj_at"] = visit.VisitedAt.Time.Format("2006-01-02")
		}
	}
	usgRows, err := q.ListUsgByEpisode(c.Request.Context(), ep.ID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	if len(usgRows) > 0 {
		u := usgRows[0]
		// Kontrak G4AncData: usg_lk = lingkar kepala (HC) — satu baris USG terakhir.
		data["usg_lk_mm"] = numericJSON(u.HcMm)
		data["usg_fl_mm"] = numericJSON(u.FlMm)
		data["usg_at"] = dateJSON(u.UsgDate)
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{}})
}
