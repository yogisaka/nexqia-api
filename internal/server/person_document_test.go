//go:build integration

package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// fakeObjectStorage is an in-memory ObjectStorage fake for handler tests —
// the real MinIO adapter is covered by TestS3ObjectStorageMinIO (Task 3).
type fakeObjectStorage struct {
	mu       sync.Mutex
	objects  map[string][]byte
	maxBytes int64
}

func newFakeObjectStorage(maxBytes int64) *fakeObjectStorage {
	return &fakeObjectStorage{objects: map[string][]byte{}, maxBytes: maxBytes}
}

func (f *fakeObjectStorage) Put(_ context.Context, key string, r io.Reader, _ string) (int64, string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, "", err
	}
	sum := sha256.Sum256(data)
	f.mu.Lock()
	f.objects[key] = data
	f.mu.Unlock()
	return int64(len(data)), hex.EncodeToString(sum[:]), nil
}

func (f *fakeObjectStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeObjectStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	delete(f.objects, key)
	f.mu.Unlock()
	return nil
}

func (f *fakeObjectStorage) MaxUploadBytes() int64 { return f.maxBytes }

// docEnv bundles one seeded tenant + the document router backed by the fake
// storage. Seeding (register/merchant/person) runs through the full
// NewRouter; document requests go through docRouter so the fake storage is
// injected without touching env.
type docEnv struct {
	pool          *pgxpool.Pool
	seedRouter    *gin.Engine
	router        *gin.Engine
	store         *fakeObjectStorage
	companyID     string
	merchantID    string
	userID        string
	token         string
	refreshCookie *http.Cookie
	personID      string
}

// docPhoneSeq generates unique valid phone numbers per seeded tenant.
var docPhoneSeq int64

func docPhone() string {
	docPhoneSeq++
	return fmt.Sprintf("08123456%04d", docPhoneSeq)
}

func setupDocEnv(t *testing.T, username string, maxBytes int64) *docEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	seedRouter := server.NewRouter(pool, redisClient, testConfig())
	store := newFakeObjectStorage(maxBytes)
	docRouter := gin.New()
	docRouter.Use(server.Recovery(os.Stdout))
	v1 := docRouter.Group("/api/v1")
	tenant := v1.Group("", server.TenantMiddleware(pool))
	protected := tenant.Group("", server.AuthMiddleware(testJWTSecret))
	server.RegisterPersonDocumentRoutes(protected, store)

	companyID, merchantID, userID, token, refreshCookie := seedAccountOwner(t, ctx, pool, seedRouter, username, docPhone())
	createPersonViaAPI(t, seedRouter, token, companyID, merchantID, "Doc Person "+username, "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Doc Person "+username)
	return &docEnv{pool: pool, seedRouter: seedRouter, router: docRouter, store: store,
		companyID: companyID, merchantID: merchantID, userID: userID, token: token,
		refreshCookie: refreshCookie, personID: personID}
}

// docRequest performs a document-route request against the fake-storage router.
func docRequest(e *docEnv, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", e.merchantID)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// docUpload uploads a document via multipart/form-data (label "" = omitted).
func docUpload(t *testing.T, e *docEnv, personID, documentType, label, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	mw.WriteField("document_type", documentType)
	if label != "" {
		mw.WriteField("label", label)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return docRequest(e, http.MethodPost, "/api/v1/persons/"+personID+"/documents", &buf, mw.FormDataContentType())
}

// testPDF is a tiny but content-sniffable PDF (DetectContentType needs %PDF-).
var testPDF = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func documentError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return body.Error
}

// TestPersonDocumentUpload — happy path upload returns 201 with the snake_case
// document shape (no storage_key) and the document appears in the list.
func TestPersonDocumentUpload(t *testing.T) {
	e := setupDocEnv(t, "docupload.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "ktp", "KTP scan", "ktp-scan.pdf", testPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Document map[string]any `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	doc := body.Document
	if doc["document_type"] != "ktp" || doc["label"] != "KTP scan" || doc["mime_type"] != "application/pdf" {
		t.Fatalf("unexpected document payload: %+v", doc)
	}
	if _, hasKey := doc["storage_key"]; hasKey {
		t.Fatalf("document response must not leak storage_key: %+v", doc)
	}
	if doc["document_type_name"] != "KTP" || doc["size_bytes"].(float64) != float64(len(testPDF)) {
		t.Fatalf("unexpected document payload: %+v", doc)
	}

	rec = docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0]["label"] != "KTP scan" {
		t.Fatalf("unexpected list: %+v", list.Data)
	}
}

// TestPersonDocumentUploadMimeRejected — a text/plain payload is refused.
func TestPersonDocumentUploadMimeRejected(t *testing.T) {
	e := setupDocEnv(t, "docmime.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "ktp", "", "notes.txt", []byte("just some plain text"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mime reject expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := documentError(t, rec); msg != "jenis file tidak didukung (hanya PDF/JPG/PNG)" {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

// TestPersonDocumentUploadSizeRejected — over the adapter's cap is refused
// before anything is stored (fake cap = 16 bytes here).
func TestPersonDocumentUploadSizeRejected(t *testing.T) {
	e := setupDocEnv(t, "docsize.owner", 16)
	rec := docUpload(t, e, e.personID, "ktp", "", "big.pdf", testPDF)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size reject expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := documentError(t, rec); msg != "ukuran file melebihi batas maksimum" {
		t.Fatalf("unexpected error message: %q", msg)
	}
	if len(e.store.objects) != 0 {
		t.Fatalf("rejected upload must not reach storage, got %d objects", len(e.store.objects))
	}
}

// TestPersonDocumentUploadUnknownType — document_type outside the catalog.
func TestPersonDocumentUploadUnknownType(t *testing.T) {
	e := setupDocEnv(t, "doctype.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "bukti_nikah_lama", "", "x.pdf", testPDF)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown type expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := documentError(t, rec); msg != "jenis dokumen tidak dikenal" {
		t.Fatalf("unexpected error message: %q", msg)
	}
	if len(e.store.objects) != 0 {
		t.Fatalf("type validation must happen before the object PUT")
	}
}

// TestPersonDocumentDocumentTypes — the catalog endpoint lists the migration
// seed (at least ktp and kartu_bpjs).
func TestPersonDocumentDocumentTypes(t *testing.T) {
	e := setupDocEnv(t, "doctypes.owner", 10<<20)
	rec := docRequest(e, http.MethodGet, "/api/v1/document-types", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("document-types expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			Code      string `json:"code"`
			Name      string `json:"name"`
			SortOrder int    `json:"sort_order"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode document-types: %v", err)
	}
	codes := map[string]bool{}
	for _, d := range body.Data {
		codes[d.Code] = true
	}
	if !codes["ktp"] || !codes["kartu_bpjs"] {
		t.Fatalf("document-types must contain ktp and kartu_bpjs, got %v", codes)
	}
	if !codes["hasil_lab_luar"] || !codes["surat_sehat"] {
		t.Fatalf("document-types must contain berkas RM catalog entries, got %v", codes)
	}
	for _, d := range body.Data {
		if d.Code == "lainnya" && d.SortOrder != 200 {
			t.Fatalf("lainnya must have sort_order 200, got %d", d.SortOrder)
		}
	}
}

// TestPersonDocumentUploadDedup — an identical file reuses the media row
// (checksum dedup, K5): two documents, one media row, one object.
func TestPersonDocumentUploadDedup(t *testing.T) {
	e := setupDocEnv(t, "docdedup.owner", 10<<20)
	if rec := docUpload(t, e, e.personID, "ktp", "pertama", "a.pdf", testPDF); rec.Code != http.StatusCreated {
		t.Fatalf("first upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := docUpload(t, e, e.personID, "surat_rujukan", "kedua", "b.pdf", testPDF); rec.Code != http.StatusCreated {
		t.Fatalf("second upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var mediaCount int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.media WHERE merchant_id = $1", e.merchantID).Scan(&mediaCount); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if mediaCount != 1 {
		t.Fatalf("dedup expected 1 media row, got %d", mediaCount)
	}
	key := "merchant/" + e.merchantID + "/" + sha256Hex(testPDF)
	if len(e.store.objects) != 1 {
		t.Fatalf("dedup expected 1 stored object, got %d", len(e.store.objects))
	}
	if _, ok := e.store.objects[key]; !ok {
		t.Fatalf("expected content-addressed key %q, got %v", key, e.store.objects)
	}
}

// TestPersonDocumentRequirements — default migration checklist for umum is
// just ktp; list ?service_type shows entries + complete; PUT replaces the set.
func TestPersonDocumentRequirements(t *testing.T) {
	e := setupDocEnv(t, "docreq.owner", 10<<20)
	// Default set mirrors migration 000073 (umum→ktp); merchants created AFTER
	// the migration ran get no seed rows, so seed via SQL like the migration.
	if _, err := e.pool.Exec(context.Background(),
		"INSERT INTO core.registration_document_requirement (company_id, merchant_id, service_type, document_type) VALUES ($1, $2, 'umum', 'ktp')",
		e.companyID, e.merchantID); err != nil {
		t.Fatalf("seed umum requirement: %v", err)
	}
	// No documents yet: umum requires ktp → incomplete.
	rec := docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents?service_type=umum", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Requirements struct {
			ServiceType string `json:"service_type"`
			Entries     []struct {
				DocumentType string `json:"document_type"`
				Name         string `json:"name"`
				Required     bool   `json:"required"`
				Present      bool   `json:"present"`
			} `json:"entries"`
			Complete bool `json:"complete"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if body.Requirements.ServiceType != "umum" || body.Requirements.Complete {
		t.Fatalf("expected umum incomplete, got %+v", body.Requirements)
	}
	if len(body.Requirements.Entries) != 1 || body.Requirements.Entries[0].DocumentType != "ktp" ||
		!body.Requirements.Entries[0].Required || body.Requirements.Entries[0].Present {
		t.Fatalf("unexpected entries: %+v", body.Requirements.Entries)
	}

	// Upload the ktp → complete flips to true.
	if rec := docUpload(t, e, e.personID, "ktp", "KTP", "ktp.pdf", testPDF); rec.Code != http.StatusCreated {
		t.Fatalf("upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents?service_type=umum", nil, "")
	body = struct {
		Requirements struct {
			ServiceType string `json:"service_type"`
			Entries     []struct {
				DocumentType string `json:"document_type"`
				Name         string `json:"name"`
				Required     bool   `json:"required"`
				Present      bool   `json:"present"`
			} `json:"entries"`
			Complete bool `json:"complete"`
		} `json:"requirements"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list 2: %v", err)
	}
	if !body.Requirements.Complete {
		t.Fatalf("umum must be complete after ktp upload: %+v", body.Requirements)
	}

	// PUT replaces the umum set; GET /document-requirements reflects it.
	putBody, _ := json.Marshal(map[string]any{"service_type": "umum", "document_types": []string{"ktp", "kartu_bpjs"}})
	rec = docRequest(e, http.MethodPut, "/api/v1/document-requirements", bytes.NewReader(putBody), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("put requirements expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = docRequest(e, http.MethodGet, "/api/v1/document-requirements", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get requirements expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var reqBody struct {
		Data []struct {
			ServiceType   string   `json:"service_type"`
			DocumentTypes []string `json:"document_types"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reqBody); err != nil {
		t.Fatalf("decode requirements: %v", err)
	}
	found := false
	for _, d := range reqBody.Data {
		if d.ServiceType == "umum" {
			found = true
			if len(d.DocumentTypes) != 2 {
				t.Fatalf("umum set must be replaced with 2 types, got %v", d.DocumentTypes)
			}
		}
	}
	if !found {
		t.Fatalf("umum set missing from %v", reqBody.Data)
	}

	// PUT on a service_type with NO existing rows (merchant is new, so no
	// migration seed for asuransi) must actually insert — regression guard for
	// the silent no-op CTE bug.
	insBody, _ := json.Marshal(map[string]any{"service_type": "asuransi", "document_types": []string{"ktp", "paspor"}})
	rec = docRequest(e, http.MethodPut, "/api/v1/document-requirements", bytes.NewReader(insBody), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("put empty-service_type requirements expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var asuransiSet []string
	if err := e.pool.QueryRow(context.Background(),
		"SELECT array_agg(document_type ORDER BY document_type) FROM core.registration_document_requirement WHERE merchant_id = $1 AND service_type = 'asuransi'",
		e.merchantID).Scan(&asuransiSet); err != nil {
		t.Fatalf("query asuransi set: %v", err)
	}
	if len(asuransiSet) != 2 || asuransiSet[0] != "ktp" || asuransiSet[1] != "paspor" {
		t.Fatalf("asuransi set must be [ktp paspor], got %v", asuransiSet)
	}
	rec = docRequest(e, http.MethodGet, "/api/v1/document-requirements", nil, "")
	var reqAll struct {
		Data []struct {
			ServiceType   string   `json:"service_type"`
			DocumentTypes []string `json:"document_types"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reqAll); err != nil {
		t.Fatalf("decode requirements after asuransi put: %v", err)
	}
	sawAsuransi := false
	for _, d := range reqAll.Data {
		if d.ServiceType == "asuransi" {
			sawAsuransi = true
			if len(d.DocumentTypes) != 2 {
				t.Fatalf("GET must return the exact new asuransi set, got %v", d.DocumentTypes)
			}
		}
	}
	if !sawAsuransi {
		t.Fatalf("asuransi set missing after PUT into empty service_type: %v", reqAll.Data)
	}

	// PUT with an unknown type is rejected with the literal message.
	badBody, _ := json.Marshal(map[string]any{"service_type": "bpjs", "document_types": []string{"npwp"}})
	rec = docRequest(e, http.MethodPut, "/api/v1/document-requirements", bytes.NewReader(badBody), "application/json")
	if rec.Code != http.StatusBadRequest || documentError(t, rec) != "jenis dokumen tidak dikenal" {
		t.Fatalf("unknown requirement type expected 400 jenis dokumen tidak dikenal, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonDocumentPatchLabel — PATCH updates the label (and row_version).
func TestPersonDocumentPatchLabel(t *testing.T) {
	e := setupDocEnv(t, "docpatch.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "ktp", "asal", "ktp.pdf", testPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	patchBody, _ := json.Marshal(map[string]string{"label": "KTP baru"})
	rec = docRequest(e, http.MethodPatch, "/api/v1/persons/"+e.personID+"/documents/"+created.Document.ID, bytes.NewReader(patchBody), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("patch expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var patched struct {
		Document struct {
			Label      string `json:"label"`
			RowVersion int    `json:"row_version"`
		} `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.Document.Label != "KTP baru" || patched.Document.RowVersion != 2 {
		t.Fatalf("unexpected patched document: %+v", patched.Document)
	}
}

// TestPersonDocumentDeleteSoft — DELETE soft-deletes the link only: GET 404
// with the literal message, media row and object stay (K5/STD-07).
func TestPersonDocumentDeleteSoft(t *testing.T) {
	e := setupDocEnv(t, "docdel.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "ktp", "KTP", "ktp.pdf", testPDF)
	var created struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	rec = docRequest(e, http.MethodDelete, "/api/v1/persons/"+e.personID+"/documents/"+created.Document.ID, nil, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents/"+created.Document.ID+"/content", nil, "")
	if rec.Code != http.StatusNotFound || documentError(t, rec) != "dokumen tidak ditemukan" {
		t.Fatalf("expected 404 dokumen tidak ditemukan, got %d: %s", rec.Code, rec.Body.String())
	}
	var mediaCount int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.media WHERE merchant_id = $1 AND deleted_at IS NULL", e.merchantID).Scan(&mediaCount); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if mediaCount != 1 {
		t.Fatalf("media row must survive soft-delete, got %d", mediaCount)
	}
	key := "merchant/" + e.merchantID + "/" + sha256Hex(testPDF)
	if _, ok := e.store.objects[key]; !ok {
		t.Fatalf("object must survive soft-delete")
	}
}

// TestPersonDocumentDownloadAccessLogged — download streams identical bytes
// and writes an access_log row with the person id.
func TestPersonDocumentDownloadAccessLogged(t *testing.T) {
	e := setupDocEnv(t, "docdl.owner", 10<<20)
	rec := docUpload(t, e, e.personID, "ktp", "KTP", "identitas.pdf", testPDF)
	var created struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	rec = docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents/"+created.Document.ID+"/content", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), testPDF) {
		t.Fatalf("downloaded bytes differ from uploaded payload")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("unexpected Content-Type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename*=UTF-8''identitas.pdf" {
		t.Fatalf("unexpected Content-Disposition %q", cd)
	}
	var logCount int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.access_log WHERE resource = 'person_document' AND action = 'view' AND person_id = $1",
		e.personID).Scan(&logCount); err != nil {
		t.Fatalf("count access_log: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("expected exactly 1 access_log row with person id, got %d", logCount)
	}
}

// TestPersonDocumentListTenantIsolation — merchant B (same company) sees no
// documents uploaded at merchant A (RLS, K11).
func TestPersonDocumentListTenantIsolation(t *testing.T) {
	e := setupDocEnv(t, "dociso.owner", 10<<20)
	if rec := docUpload(t, e, e.personID, "ktp", "KTP A", "ktp.pdf", testPDF); rec.Code != http.StatusCreated {
		t.Fatalf("upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	// Second merchant in the same company, created through the real endpoint
	// (needs the seed refresh cookie for the merchant-scoped reissued token).
	merchantB := createTenantMerchant(t, ctx, e.pool, e.seedRouter, e.token, e.refreshCookie, e.companyID, "Merchant Iso B")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/persons/"+e.personID+"/documents", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", merchantB)
	recB := httptest.NewRecorder()
	e.router.ServeHTTP(recB, req)
	if recB.Code != http.StatusOK {
		t.Fatalf("list at merchant B expected 200, got %d: %s", recB.Code, recB.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list at merchant B: %v", err)
	}
	if len(body.Data) != 0 {
		t.Fatalf("merchant B must not see merchant A documents, got %+v", body.Data)
	}
}

// TestPersonDocumentUploadOrphanCleanup — the object PUT succeeds, then the
// DB write fails (nonexistent person FK) → the tx rolls back and the orphaned
// object is deleted best-effort (K12 step 4).
func TestPersonDocumentUploadOrphanCleanup(t *testing.T) {
	e := setupDocEnv(t, "docorphan.owner", 10<<20)
	ghostPerson := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee" // valid UUID, no person row
	rec := docUpload(t, e, ghostPerson, "ktp", "orphan", "orphan.pdf", testPDF)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced tx failure expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	key := "merchant/" + e.merchantID + "/" + sha256Hex(testPDF)
	if _, err := e.store.Get(context.Background(), key); err == nil {
		t.Fatalf("orphaned object must be deleted best-effort after tx failure")
	}
	var docCount int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.person_document WHERE merchant_id = $1", e.merchantID).Scan(&docCount); err != nil {
		t.Fatalf("count person_document: %v", err)
	}
	if docCount != 0 {
		t.Fatalf("failed tx must leave no document rows, got %d", docCount)
	}
}

// TestPersonDocumentListAdmissionFilter — list accepts ?admission_id= and
// filters via core.media_link: (a) no param returns all, (b) with the param
// only the linked document, (c) non-UUID → 400, (d) soft-deleted linked
// document disappears from the filtered list.
func TestPersonDocumentListAdmissionFilter(t *testing.T) {
	e := setupDocEnv(t, "doclistadm.owner", 10<<20)
	ctx := context.Background()
	admissionID, _ := seedAuditPersonAdmission(t, ctx, e.pool, e.seedRouter, e.token, e.companyID, e.merchantID, e.personID, "doclistadm")
	linked := docUploadAdmission(t, e, e.personID, "ktp", "KTP kunjungan", admissionID, "linked.pdf", testPDF)
	if linked.Code != http.StatusCreated {
		t.Fatalf("linked upload expected 201, got %d: %s", linked.Code, linked.Body.String())
	}
	// Distinct payload so the plain upload gets its own media row (dedup would
	// reuse the linked media).
	plainPDF := append([]byte(nil), testPDF...)
	plainPDF = append(plainPDF, "\n% distinct payload for list filter\n"...)
	if rec := docUpload(t, e, e.personID, "ktp", "KTP polos", "plain.pdf", plainPDF); rec.Code != http.StatusCreated {
		t.Fatalf("plain upload expected 201, got %d", rec.Code)
	}
	getList := func(query string) []map[string]any {
		t.Helper()
		rec := docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents"+query, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list %q expected 200, got %d: %s", query, rec.Code, rec.Body.String())
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list %q: %v", query, err)
		}
		return body.Data
	}
	// (a) tanpa param → keduanya.
	if got := getList(""); len(got) != 2 {
		t.Fatalf("unfiltered list expected 2 documents, got %d: %+v", len(got), got)
	}
	// (b) dengan param → hanya yang ber-link.
	got := getList("?admission_id=" + admissionID)
	if len(got) != 1 || got[0]["label"] != "KTP kunjungan" {
		t.Fatalf("filtered list expected only the linked document, got %+v", got)
	}
	// (c) non-UUID → 400 invalid admission_id.
	rec := docRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/documents?admission_id=abc", nil, "")
	if rec.Code != http.StatusBadRequest || documentError(t, rec) != "invalid admission_id" {
		t.Fatalf("invalid admission_id expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// (d) soft-delete dokumen ber-link → hilang dari hasil filter.
	var created struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(linked.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode linked upload: %v", err)
	}
	rec = docRequest(e, http.MethodDelete, "/api/v1/persons/"+e.personID+"/documents/"+created.Document.ID, nil, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := getList("?admission_id=" + admissionID); len(got) != 0 {
		t.Fatalf("filtered list after soft-delete expected 0, got %+v", got)
	}
}

// docUploadAdmission is docUpload plus the optional admission_id field.
func docUploadAdmission(t *testing.T, e *docEnv, personID, documentType, label, admissionID, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	mw.WriteField("document_type", documentType)
	if label != "" {
		mw.WriteField("label", label)
	}
	if admissionID != "" {
		mw.WriteField("admission_id", admissionID)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return docRequest(e, http.MethodPost, "/api/v1/persons/"+personID+"/documents", &buf, mw.FormDataContentType())
}

// TestPersonDocumentUploadAdmissionContext — upload with admission_id links
// the media to the admission (core.media_link purpose 'berkas_rm'); the
// response carries original_filename/created_by_name/virus_scan_status
// (plan 2026-11-04-berkas-rm-a-backend Task 2).
func TestPersonDocumentUploadAdmissionContext(t *testing.T) {
	e := setupDocEnv(t, "docadm.owner", 10<<20)
	ctx := context.Background()
	// Admission for the document's own person (cfSeedAdmission pattern).
	admissionID, _ := seedAuditPersonAdmission(t, ctx, e.pool, e.seedRouter, e.token, e.companyID, e.merchantID, e.personID, "docadm")
	// Admission of ANOTHER person (same tenant) for the 400 case.
	_, otherAdmissionID := cfSeedAdmission(t, ctx, e.pool, e.seedRouter, e.token, e.companyID, e.merchantID, "docadm-other")

	// (a) valid admission_id → 201 with the new metadata fields.
	rec := docUploadAdmission(t, e, e.personID, "ktp", "KTP kunjungan", admissionID, "ktp-adm.pdf", testPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload with admission expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Document map[string]any `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	doc := body.Document
	if doc["original_filename"] != "ktp-adm.pdf" {
		t.Fatalf("expected original_filename ktp-adm.pdf, got %v", doc["original_filename"])
	}
	if name, _ := doc["created_by_name"].(string); name == "" {
		t.Fatalf("created_by_name must be non-empty, got %v", doc["created_by_name"])
	}
	if doc["virus_scan_status"] != "pending" {
		t.Fatalf("expected virus_scan_status pending, got %v", doc["virus_scan_status"])
	}

	// (b) the media_link row exists.
	docID, _ := doc["id"].(string)
	var linkCount int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM core.media_link ml
		 JOIN core.person_document d ON d.media_id = ml.media_id
		 WHERE d.id = $1 AND ml.owner_table = 'admission' AND ml.owner_id = $2 AND ml.purpose = 'berkas_rm'`,
		docID, admissionID).Scan(&linkCount); err != nil {
		t.Fatalf("count media_link: %v", err)
	}
	if linkCount != 1 {
		t.Fatalf("expected 1 media_link (admission, berkas_rm), got %d", linkCount)
	}

	// (c) re-upload the IDENTICAL file with the same admission → still 201
	// (media dedup + ON CONFLICT DO NOTHING on the link).
	if rec := docUploadAdmission(t, e, e.personID, "ktp", "KTP ulang", admissionID, "ktp-adm.pdf", testPDF); rec.Code != http.StatusCreated {
		t.Fatalf("re-upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// (d) admission of another person → 400.
	rec = docUploadAdmission(t, e, e.personID, "ktp", "", otherAdmissionID, "ktp-x.pdf", testPDF)
	if rec.Code != http.StatusBadRequest || documentError(t, rec) != "admission tidak ditemukan" {
		t.Fatalf("foreign admission expected 400 admission tidak ditemukan, got %d: %s", rec.Code, rec.Body.String())
	}

	// (e) non-UUID admission_id → 400.
	rec = docUploadAdmission(t, e, e.personID, "ktp", "", "abc", "ktp-y.pdf", testPDF)
	if rec.Code != http.StatusBadRequest || documentError(t, rec) != "invalid admission_id" {
		t.Fatalf("invalid admission_id expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// (f) without admission_id → 201 and NO new media_link (distinct payload
	// so a fresh media row is created; same file would dedup onto the linked
	// media above).
	otherPDF := append([]byte(nil), testPDF...)
	otherPDF = append(otherPDF, "\n% distinct payload for upload-f\n"...)
	var linksBefore, linksAfter int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM core.media_link").Scan(&linksBefore); err != nil {
		t.Fatalf("count media_link before: %v", err)
	}
	rec = docUpload(t, e, e.personID, "ktp", "", "ktp-plain.pdf", otherPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload without admission expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM core.media_link").Scan(&linksAfter); err != nil {
		t.Fatalf("count media_link after: %v", err)
	}
	if linksAfter != linksBefore {
		t.Fatalf("upload without admission_id must not create media_link rows, before=%d after=%d", linksBefore, linksAfter)
	}
}
