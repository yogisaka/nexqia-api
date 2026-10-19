// internal/server/person_document.go
// Patient document API (spec 2026-10-17-patient-documents-design §5.1–§5.2):
// upload/list/patch/delete person documents over core.media + object storage,
// proxy download with access logging, and per-merchant document requirement
// checklists.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/storage"
)

// PermPatientDocumentView guards reading/downloading patient documents;
// PermPatientDocumentManage guards uploading/editing/deleting them
// (migration 000073).
const (
	PermPatientDocumentView   = "patient.document.view"
	PermPatientDocumentManage = "patient.document.manage"
)

// allowedDocumentMimes is the content-sniff whitelist (K13).
var allowedDocumentMimes = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/png":       true,
}

// validServiceTypes mirrors the registration_document_requirement CHECK
// (migration 000073, enum from 000057).
var documentServiceTypes = map[string]bool{"umum": true, "bpjs": true, "asuransi": true}

// RegisterPersonDocumentRoutes wires the patient document endpoints. The
// person path param is `:id` (not the spec's `:personId`) because gin panics
// on conflicting wildcard names in the same segment and RegisterPersonRoutes
// already owns `/persons/:id`.
func RegisterPersonDocumentRoutes(rg *gin.RouterGroup, store storage.ObjectStorage) {
	rg.GET("/document-types", GetDocumentTypesHandler)
	rg.GET("/document-requirements", GetDocumentRequirementsHandler)
	rg.PUT("/document-requirements", PutDocumentRequirementsHandler)
	rg.POST("/persons/:id/documents", UploadPersonDocumentHandler(store))
	rg.GET("/persons/:id/documents", ListPersonDocumentsHandler)
	rg.GET("/persons/:id/documents/:documentId/content", AccessLog("person_document", "view", "documentId"), DownloadPersonDocumentHandler(store))
	rg.PATCH("/persons/:id/documents/:documentId", UpdatePersonDocumentHandler)
	rg.DELETE("/persons/:id/documents/:documentId", DeletePersonDocumentHandler)
}

// maxUploadBytes returns the size cap advertised by the storage adapter; the
// storage.ObjectStorage interface itself has no MaxUploadBytes method, so use
// one when the concrete adapter provides it (tests' fakes do) and fall back to
// the package-level env parser otherwise.
func maxUploadBytes(store storage.ObjectStorage) int64 {
	if m, ok := store.(interface{ MaxUploadBytes() int64 }); ok {
		return m.MaxUploadBytes()
	}
	return storage.MaxUploadBytes()
}

// documentJSON maps the document fields to the snake_case response shape of
// spec §5.1 — no storage_key ever leaves the server.
func documentJSON(id, personID pgtype.UUID, documentType, documentTypeName, label string, issuedDate, expiredDate pgtype.Date, mimeType string, sizeBytes int64, createdBy pgtype.UUID, createdAt, updatedAt pgtype.Timestamptz, rowVersion int32) gin.H {
	return gin.H{
		"id":                 uuidJSON(id),
		"person_id":          uuidJSON(personID),
		"document_type":      documentType,
		"document_type_name": documentTypeName,
		"label":              label,
		"issued_date":        dateJSON(issuedDate),
		"expired_date":       dateJSON(expiredDate),
		"mime_type":          mimeType,
		"size_bytes":         sizeBytes,
		"created_by":         uuidJSON(createdBy),
		"created_at":         tsJSON(createdAt),
		"updated_at":         tsJSON(updatedAt),
		"row_version":        rowVersion,
	}
}

// dateJSON renders an optional pgtype.Date as YYYY-MM-DD (nil when NULL).
func dateJSON(d pgtype.Date) any {
	if !d.Valid {
		return nil
	}
	return d.Time.Format("2006-01-02")
}

// documentTypeCatalog loads the document_type catalog as a code→name map.
func documentTypeCatalog(ctx context.Context, q *sqlcgen.Queries) (map[string]string, error) {
	types, err := q.ListDocumentTypes(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(types))
	for _, t := range types {
		names[t.Code] = t.Name
	}
	return names, nil
}

// fetchPersonDocument loads a document and applies the 404 contract when it
// is missing, soft-deleted, or belongs to another person/merchant (isolation
// is enforced in the handler too — RLS alone is not enough for pool roles
// that bypass it).
func fetchPersonDocument(c *gin.Context, q *sqlcgen.Queries, documentID, personID, merchantID pgtype.UUID) (sqlcgen.GetPersonDocumentByIDRow, bool) {
	doc, err := q.GetPersonDocumentByID(c.Request.Context(), documentID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (doc.PersonID != personID || doc.MerchantID != merchantID)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "dokumen tidak ditemukan"})
		return doc, false
	}
	if err != nil {
		respondInternalError(c, err)
		return doc, false
	}
	return doc, true
}

// UploadPersonDocumentHandler godoc
// @Summary Upload a patient document
// @Description multipart/form-data: file (wajib), document_type (kode katalog), label (opsional), issued_date/expired_date (opsional).
// @Tags patient-document
// @Accept multipart/form-data
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Success 201 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /persons/{id}/documents [post]
func UploadPersonDocumentHandler(store storage.ObjectStorage) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPatientDocumentManage) {
			return
		}
		personID, ok := parseUUID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		fh, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "file wajib"})
			return
		}
		maxBytes := maxUploadBytes(store)
		f, err := fh.Open()
		if err != nil {
			respondInternalError(c, err)
			return
		}
		defer f.Close()
		// Buffered read capped at max+1: an over-limit upload never reaches
		// storage or the sha256 stage (K12 step 1–2).
		buf, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if int64(len(buf)) > maxBytes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ukuran file melebihi batas maksimum"})
			return
		}
		mimeType := http.DetectContentType(buf)
		if !allowedDocumentMimes[mimeType] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "jenis file tidak didukung (hanya PDF/JPG/PNG)"})
			return
		}
		documentType := c.PostForm("document_type")
		label := c.PostForm("label")
		if label == "" {
			label = fh.Filename
		}
		issuedDate, err := optDate(c.PostForm("issued_date"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid issued_date, expected YYYY-MM-DD"})
			return
		}
		expiredDate, err := optDate(c.PostForm("expired_date"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expired_date, expected YYYY-MM-DD"})
			return
		}
		sum := sha256.Sum256(buf)
		checksum := hex.EncodeToString(sum[:])
		key := "merchant/" + merchantID.String() + "/" + checksum

		tx := TxFromContext(c)
		q := sqlcgen.New(tx)
		ctx := c.Request.Context()
		catalog, err := documentTypeCatalog(ctx, q)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		if _, known := catalog[documentType]; !known {
			c.JSON(http.StatusBadRequest, gin.H{"error": "jenis dokumen tidak dikenal"})
			return
		}

		// K12 step 4: if anything fails after the object was PUT, the tx rolls
		// back (response >= 400) — drop the now-orphaned object best-effort.
		putKey := ""
		defer func() {
			if putKey != "" && (c.IsAborted() || c.Writer.Status() >= http.StatusBadRequest) {
				_ = store.Delete(context.WithoutCancel(c.Request.Context()), putKey)
			}
		}()

		media, err := q.GetMediaByChecksum(ctx, sqlcgen.GetMediaByChecksumParams{
			MerchantID: merchantID, ChecksumSha256: checksum,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// K12 step 2: PUT object (content-addressed key, K6), then INSERT
			// media two-step in the same tx: CreateMedia defaults status
			// 'uploaded', UpdateMediaStatus promotes it to 'ready'.
			if _, _, err := store.Put(ctx, key, bytes.NewReader(buf), mimeType); err != nil {
				respondInternalError(c, err)
				return
			}
			putKey = key
			media, err = q.CreateMedia(ctx, sqlcgen.CreateMediaParams{
				CompanyID: AuthCompanyID(c), MerchantID: merchantID,
				StorageProvider: "s3_cdn", StorageKey: key,
				OriginalFilename: fh.Filename, MimeType: mimeType,
				SizeBytes: int64(len(buf)), ChecksumSha256: checksum,
				UploadedBy: AuthUserID(c),
			})
			if err != nil {
				respondInternalError(c, err)
				return
			}
			if _, err := q.UpdateMediaStatus(ctx, sqlcgen.UpdateMediaStatusParams{
				ID: media.ID, Status: "ready", VirusScanStatus: "pending",
			}); err != nil {
				respondInternalError(c, err)
				return
			}
		case err != nil:
			respondInternalError(c, err)
			return
		}
		doc, err := q.CreatePersonDocument(ctx, sqlcgen.CreatePersonDocumentParams{
			CompanyID: AuthCompanyID(c), MerchantID: merchantID,
			PersonID: personID, MediaID: media.ID,
			DocumentType: documentType, Label: label,
			IssuedDate: issuedDate, ExpiredDate: expiredDate,
			CreatedBy: AuthUserID(c),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"document": documentJSON(
			doc.ID, doc.PersonID, doc.DocumentType, catalog[doc.DocumentType], doc.Label,
			doc.IssuedDate, doc.ExpiredDate, mimeType, int64(len(buf)),
			doc.CreatedBy, doc.CreatedAt, doc.UpdatedAt, doc.RowVersion,
		)})
	}
}

// ListPersonDocumentsHandler godoc
// @Summary List a person's active documents
// @Description With ?service_type=umum|bpjs|asuransi the response adds the requirement checklist (non-blocking, K8).
// @Tags patient-document
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param service_type query string false "umum|bpjs|asuransi"
// @Success 200 {object} apiResponse
// @Router /persons/{id}/documents [get]
func ListPersonDocumentsHandler(c *gin.Context) {
	if !RequirePermission(c, PermPatientDocumentView) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()
	rows, err := q.ListPersonDocumentsByPerson(ctx, sqlcgen.ListPersonDocumentsByPersonParams{
		PersonID: personID, MerchantID: merchantID,
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, len(rows))
	present := make(map[string]bool, len(rows))
	for i, r := range rows {
		items[i] = documentJSON(r.ID, r.PersonID, r.DocumentType, r.DocumentTypeName, r.Label,
			r.IssuedDate, r.ExpiredDate, r.MimeType, r.SizeBytes,
			r.CreatedBy, r.CreatedAt, r.UpdatedAt, r.RowVersion)
		present[r.DocumentType] = true
	}
	resp := gin.H{"data": items, "meta": gin.H{}}
	if st := c.Query("service_type"); st != "" {
		if !documentServiceTypes[st] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "jenis layanan tidak dikenal"})
			return
		}
		reqs, err := q.ListDocumentRequirements(ctx, sqlcgen.ListDocumentRequirementsParams{
			MerchantID: merchantID, ServiceType: optText(st),
		})
		if err != nil {
			respondInternalError(c, err)
			return
		}
		catalog, err := documentTypeCatalog(ctx, q)
		if err != nil {
			respondInternalError(c, err)
			return
		}
		entries := make([]gin.H, 0, len(reqs))
		complete := true
		for _, r := range reqs {
			p := present[r.DocumentType]
			if !p {
				complete = false
			}
			entries = append(entries, gin.H{
				"document_type": r.DocumentType,
				"name":          catalog[r.DocumentType],
				"required":      true,
				"present":       p,
			})
		}
		resp["requirements"] = gin.H{"service_type": st, "entries": entries, "complete": complete}
	}
	c.JSON(http.StatusOK, resp)
}

type updatePersonDocumentRequest struct {
	Label       *string `json:"label"`
	IssuedDate  *string `json:"issued_date"`
	ExpiredDate *string `json:"expired_date"`
}

// UpdatePersonDocumentHandler godoc
// @Summary Patch a patient document (label/dates, partial)
// @Tags patient-document
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param documentId path string true "Document UUID"
// @Success 200 {object} apiResponse
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id}/documents/{documentId} [patch]
func UpdatePersonDocumentHandler(c *gin.Context) {
	if !RequirePermission(c, PermPatientDocumentManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	documentID, ok := parseUUID(c.Param("documentId"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
		return
	}
	var req updatePersonDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()
	existing, ok := fetchPersonDocument(c, q, documentID, personID, merchantID)
	if !ok {
		return
	}
	label := existing.Label
	if req.Label != nil {
		label = *req.Label
	}
	issuedDate := existing.IssuedDate
	if req.IssuedDate != nil {
		d, derr := optDate(*req.IssuedDate)
		if derr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid issued_date, expected YYYY-MM-DD"})
			return
		}
		issuedDate = d
	}
	expiredDate := existing.ExpiredDate
	if req.ExpiredDate != nil {
		d, derr := optDate(*req.ExpiredDate)
		if derr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expired_date, expected YYYY-MM-DD"})
			return
		}
		expiredDate = d
	}
	if _, err := q.UpdatePersonDocument(ctx, sqlcgen.UpdatePersonDocumentParams{
		ID: documentID, Label: label, IssuedDate: issuedDate, ExpiredDate: expiredDate,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	updated, err := q.GetPersonDocumentByID(ctx, documentID)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"document": documentJSON(
		updated.ID, updated.PersonID, updated.DocumentType, updated.DocumentTypeName, updated.Label,
		updated.IssuedDate, updated.ExpiredDate, updated.MimeType, updated.SizeBytes,
		updated.CreatedBy, updated.CreatedAt, updated.UpdatedAt, updated.RowVersion,
	), "meta": gin.H{}})
}

// DeletePersonDocumentHandler godoc
// @Summary Soft-delete a patient document (media and object stay, STD-07/K5)
// @Tags patient-document
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param documentId path string true "Document UUID"
// @Success 204 "No Content"
// @Failure 404 {object} apiErrorResponse
// @Router /persons/{id}/documents/{documentId} [delete]
func DeletePersonDocumentHandler(c *gin.Context) {
	if !RequirePermission(c, PermPatientDocumentManage) {
		return
	}
	personID, ok := parseUUID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	documentID, ok := parseUUID(c.Param("documentId"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	ctx := c.Request.Context()
	if _, ok := fetchPersonDocument(c, q, documentID, personID, merchantID); !ok {
		return
	}
	if err := q.SoftDeletePersonDocument(ctx, sqlcgen.SoftDeletePersonDocumentParams{
		ID: documentID, DeletedBy: AuthUserID(c),
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// DownloadPersonDocumentHandler godoc
// @Summary Download a patient document via API proxy (K1)
// @Description Streams the stored object; every download is access-logged with the person id (STD-09/K1).
// @Tags patient-document
// @Produce octet-stream
// @Security BearerAuth
// @Param id path string true "Person UUID"
// @Param documentId path string true "Document UUID"
// @Success 200 {string} string
// @Failure 404 {object} apiErrorResponse
// @Failure 503 {object} apiErrorResponse
// @Router /persons/{id}/documents/{documentId}/content [get]
func DownloadPersonDocumentHandler(store storage.ObjectStorage) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !RequirePermission(c, PermPatientDocumentView) {
			return
		}
		personID, ok := parseUUID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		documentID, ok := parseUUID(c.Param("documentId"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document id"})
			return
		}
		merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
		q := sqlcgen.New(TxFromContext(c))
		ctx := c.Request.Context()
		doc, ok := fetchPersonDocument(c, q, documentID, personID, merchantID)
		if !ok {
			return
		}
		setAccessLogPersonID(c, doc.PersonID)
		rc, err := store.Get(ctx, doc.StorageKey)
		if errors.Is(err, storage.ErrNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "penyimpanan objek belum dikonfigurasi"})
			return
		}
		if err != nil {
			respondInternalError(c, err)
			return
		}
		defer rc.Close()
		c.Header("Content-Type", doc.MimeType)
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(doc.OriginalFilename))
		c.Status(http.StatusOK)
		_, _ = io.Copy(c.Writer, rc)
	}
}

// GetDocumentTypesHandler godoc
// @Summary List the document type catalog
// @Description The single source of document types for the FE upload form and requirement settings (spec §5.2).
// @Tags patient-document
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /document-types [get]
func GetDocumentTypesHandler(c *gin.Context) {
	if !RequirePermission(c, PermPatientDocumentView) {
		return
	}
	q := sqlcgen.New(TxFromContext(c))
	types, err := q.ListDocumentTypes(c.Request.Context())
	if err != nil {
		respondInternalError(c, err)
		return
	}
	items := make([]gin.H, len(types))
	for i, t := range types {
		items[i] = gin.H{"code": t.Code, "name": t.Name, "sort_order": t.SortOrder}
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

// GetDocumentRequirementsHandler godoc
// @Summary List document requirement sets for the active merchant
// @Tags patient-document
// @Produce json
// @Security BearerAuth
// @Success 200 {object} apiResponse
// @Router /document-requirements [get]
func GetDocumentRequirementsHandler(c *gin.Context) {
	if !RequirePermission(c, "core.merchant.manage") {
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	q := sqlcgen.New(TxFromContext(c))
	rows, err := q.ListDocumentRequirements(c.Request.Context(), sqlcgen.ListDocumentRequirementsParams{
		MerchantID: merchantID, ServiceType: pgtype.Text{},
	})
	if err != nil {
		respondInternalError(c, err)
		return
	}
	ordered := []string{}
	byService := map[string][]string{}
	for _, r := range rows {
		if _, seen := byService[r.ServiceType]; !seen {
			ordered = append(ordered, r.ServiceType)
		}
		byService[r.ServiceType] = append(byService[r.ServiceType], r.DocumentType)
	}
	items := make([]gin.H, 0, len(ordered))
	for _, st := range ordered {
		items = append(items, gin.H{"service_type": st, "document_types": byService[st]})
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{}})
}

type putDocumentRequirementsRequest struct {
	ServiceType   string   `json:"service_type" binding:"required"`
	DocumentTypes []string `json:"document_types"`
}

// PutDocumentRequirementsHandler godoc
// @Summary Replace the requirement set for one service_type (single tx)
// @Tags patient-document
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body putDocumentRequirementsRequest true "Requirement set"
// @Success 200 {object} apiResponse
// @Failure 400 {object} apiErrorResponse
// @Router /document-requirements [put]
func PutDocumentRequirementsHandler(c *gin.Context) {
	if !RequirePermission(c, "core.merchant.manage") {
		return
	}
	var req putDocumentRequirementsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !documentServiceTypes[req.ServiceType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jenis layanan tidak dikenal"})
		return
	}
	merchantID, _ := parseUUID(c.GetHeader("X-Merchant-ID"))
	tx := TxFromContext(c)
	q := sqlcgen.New(tx)
	ctx := c.Request.Context()
	catalog, err := documentTypeCatalog(ctx, q)
	if err != nil {
		respondInternalError(c, err)
		return
	}
	for _, code := range req.DocumentTypes {
		if _, known := catalog[code]; !known {
			c.JSON(http.StatusBadRequest, gin.H{"error": "jenis dokumen tidak dikenal"})
			return
		}
	}
	// Replace = delete old set + insert new set, both inside this request tx
	// (RunRequestTx rolls both back together on any failure).
	if err := q.DeleteDocumentRequirementsByServiceType(ctx, sqlcgen.DeleteDocumentRequirementsByServiceTypeParams{
		MerchantID: merchantID, ServiceType: req.ServiceType,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	if err := q.ReplaceDocumentRequirements(ctx, sqlcgen.ReplaceDocumentRequirementsParams{
		MerchantID: merchantID, ServiceType: req.ServiceType,
		CompanyID: AuthCompanyID(c), Column4: req.DocumentTypes,
	}); err != nil {
		respondInternalError(c, err)
		return
	}
	codes := req.DocumentTypes
	if codes == nil {
		codes = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"service_type": req.ServiceType, "document_types": codes,
	}, "meta": gin.H{}})
}
