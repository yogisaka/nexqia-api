//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// consentEnv bundles one seeded tenant + a router exposing only the consent
// routes (pattern: setupDocEnv in person_document_test.go).
type consentEnv struct {
	pool          *pgxpool.Pool
	seedRouter    *gin.Engine
	router        *gin.Engine
	companyID     string
	merchantID    string
	userID        string
	token         string
	refreshCookie *http.Cookie
	personID      string
}

func setupConsentEnv(t *testing.T, username string) *consentEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	seedRouter := server.NewRouter(pool, redisClient, testConfig())
	router := gin.New()
	v1 := router.Group("/api/v1")
	tenant := v1.Group("", server.TenantMiddleware(pool))
	protected := tenant.Group("", server.AuthMiddleware(testJWTSecret))
	server.RegisterPersonConsentRoutes(protected)
	// Document routes with a fake store, so tests can upload a referenced
	// person_document (pattern: setupDocEnv in person_document_test.go).
	server.RegisterPersonDocumentRoutes(protected, newFakeObjectStorage(1<<20))

	companyID, merchantID, userID, token, refreshCookie := seedAccountOwner(t, ctx, pool, seedRouter, username, docPhone())
	createPersonViaAPI(t, seedRouter, token, companyID, merchantID, "Consent Person "+username, "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Consent Person "+username)
	return &consentEnv{pool: pool, seedRouter: seedRouter, router: router,
		companyID: companyID, merchantID: merchantID, userID: userID, token: token,
		refreshCookie: refreshCookie, personID: personID}
}

func consentRequest(e *consentEnv, method, path string, body []byte) *httptest.ResponseRecorder {
	var r *bytes.Reader
	if body == nil {
		r = bytes.NewReader(nil)
	} else {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", e.merchantID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func consentPost(t *testing.T, e *consentEnv, consentType, textVersion, method string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"consent_type": consentType, "text_version": textVersion, "method": method,
	})
	return consentRequest(e, http.MethodPost, "/api/v1/persons/"+e.personID+"/consents", body)
}

func consentError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response %d: %v: %s", rec.Code, err, rec.Body.String())
	}
	return body.Error
}

// validConsentBody is the seeded active consent text (migration 000074).
const validConsentVersion = "2026-10-v1"

// TestPersonConsentRecord — POST 201 echoes the snake_case consent shape and
// the row is readable via list (newest first) and latest.
func TestPersonConsentRecord(t *testing.T) {
	e := setupConsentEnv(t, "consent.owner")
	rec := consentPost(t, e, "data_processing", validConsentVersion, "form_signed")
	if rec.Code != http.StatusCreated {
		t.Fatalf("record expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Consent map[string]any `json:"consent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	c := created.Consent
	if c["consent_type"] != "data_processing" || c["text_version"] != validConsentVersion || c["method"] != "form_signed" {
		t.Fatalf("unexpected consent payload: %+v", c)
	}
	if c["person_id"] != e.personID {
		t.Fatalf("unexpected person_id: %+v", c)
	}
	if name, _ := c["granted_by_name"].(string); name == "" {
		t.Fatalf("granted_by_name must be present, got %+v", c)
	}
	if _, ok := c["granted_by"].(string); !ok || c["granted_by"].(string) == "" {
		t.Fatalf("granted_by must be present, got %+v", c)
	}
	// granted_at must be RFC3339 (contract example 2026-10-17T08:30:00+07:00).
	at, err := time.Parse(time.RFC3339, c["granted_at"].(string))
	if err != nil {
		t.Fatalf("granted_at not RFC3339: %v (%q)", err, c["granted_at"])
	}
	if at.IsZero() {
		t.Fatalf("granted_at must be non-zero")
	}

	rec = consentRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/consents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	// Creation (Task 3) already wrote one data_processing consent; the list
	// must hold it plus the row just recorded, newest first.
	if len(list.Data) != 2 || list.Data[0]["id"] != c["id"] {
		t.Fatalf("unexpected list: %+v", list.Data)
	}

	rec = consentRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/consents/latest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("latest expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var latest struct {
		Consent map[string]any `json:"consent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &latest); err != nil {
		t.Fatalf("decode latest: %v", err)
	}
	if latest.Consent["id"] != c["id"] {
		t.Fatalf("latest must return the recorded consent: %+v vs %+v", latest.Consent, c)
	}
}

// TestPersonConsentLatestEmpty — create-person (Task 3) auto-records a
// data_processing consent, so latest returns that creation row.
func TestPersonConsentLatestEmpty(t *testing.T) {
	e := setupConsentEnv(t, "consentempty.owner")
	rec := consentRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/consents/latest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("latest expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Consent map[string]any `json:"consent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode latest: %v", err)
	}
	if body.Consent == nil {
		t.Fatalf("expected the creation consent, got null")
	}
	if body.Consent["consent_type"] != "data_processing" ||
		body.Consent["text_version"] != validConsentVersion || body.Consent["method"] != "form_signed" {
		t.Fatalf("unexpected creation consent: %+v", body.Consent)
	}
}

// TestPersonConsentRecordValidation — unknown text_version → 400 with the
// literal message; unknown method and unknown consent_type → 400.
func TestPersonConsentRecordValidation(t *testing.T) {
	e := setupConsentEnv(t, "consentval.owner")

	rec := consentPost(t, e, "data_processing", "1999-01-tidak-ada", "form_signed")
	if rec.Code != http.StatusBadRequest || consentError(t, rec) != "versi teks persetujuan tidak dikenal" {
		t.Fatalf("unknown text_version expected 400 versi teks persetujuan tidak dikenal, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := consentPost(t, e, "data_processing", validConsentVersion, "sms"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown method expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := consentPost(t, e, "marketing", validConsentVersion, "form_signed"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown consent_type expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Only the creation consent (Task 3) exists; rejected attempts must add
	// nothing.
	var n int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.person_consent WHERE person_id = $1", e.personID).Scan(&n); err != nil {
		t.Fatalf("count consents: %v", err)
	}
	if n != 1 {
		t.Fatalf("rejected attempts must write nothing beyond the creation row, got %d rows", n)
	}
}

// consentUploadDoc uploads a 'ktp' document for personID via the seed router
// (multipart pattern: docUpload in person_document_test.go) and returns its id.
func consentUploadDoc(t *testing.T, e *consentEnv, personID string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "ktp.pdf")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(testPDF); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.WriteField("document_type", "ktp"); err != nil {
		t.Fatalf("write document_type: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/persons/"+personID+"/documents", &buf)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", e.merchantID)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("document upload expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return body.Document.ID
}

// TestPersonConsentRecordForeignDocument — person_document_id belonging to
// another person of the same merchant → 400 and nothing is written.
func TestPersonConsentRecordForeignDocument(t *testing.T) {
	e := setupConsentEnv(t, "consentdoc.owner")
	ctx := context.Background()
	createPersonViaAPI(t, e.seedRouter, e.token, e.companyID, e.merchantID, "Consent Person B consentdoc.owner", "female")
	personB, _ := latestPerson(t, ctx, e.pool, e.companyID, "Consent Person B consentdoc.owner")
	foreignDocID := consentUploadDoc(t, e, personB)

	body, _ := json.Marshal(map[string]string{
		"consent_type": "data_processing", "text_version": validConsentVersion,
		"method": "form_signed", "person_document_id": foreignDocID,
	})
	rec := consentRequest(e, http.MethodPost, "/api/v1/persons/"+e.personID+"/consents", body)
	if rec.Code != http.StatusBadRequest || consentError(t, rec) != "dokumen tidak valid untuk pasien ini" {
		t.Fatalf("foreign document expected 400 dokumen tidak valid untuk pasien ini, got %d: %s", rec.Code, rec.Body.String())
	}

	// The creation consent remains the only row — the rejected attempt wrote nothing.
	var n int
	if err := e.pool.QueryRow(ctx,
		"SELECT count(*) FROM core.person_consent WHERE person_id = $1", e.personID).Scan(&n); err != nil {
		t.Fatalf("count consents: %v", err)
	}
	if n != 1 {
		t.Fatalf("rejected attempt must write nothing beyond the creation row, got %d rows", n)
	}
}

// TestPersonConsentText — GET /consent-text?code=data_processing returns the
// seeded active version.
func TestPersonConsentText(t *testing.T) {
	e := setupConsentEnv(t, "consenttext.owner")
	rec := consentRequest(e, http.MethodGet, "/api/v1/consent-text?code=data_processing", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("consent-text expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code    string `json:"code"`
		Version string `json:"version"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode consent-text: %v", err)
	}
	if body.Code != "data_processing" || body.Version != validConsentVersion || body.Body == "" {
		t.Fatalf("unexpected consent-text payload: %+v", body)
	}
}

// TestPersonConsentImmutable — consent rows are append-only: app_runtime may
// neither UPDATE nor DELETE them (pattern: TestBillingDB_ConsentImmutable,
// billing_db_test.go:91-119).
func TestPersonConsentImmutable(t *testing.T) {
	e := setupConsentEnv(t, "consentimm.owner")
	ctx := context.Background()

	tx := runtimeTenantTx(t, ctx, e.pool, e.companyID, e.merchantID)
	var consentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.person_consent
			(company_id, merchant_id, person_id, consent_type, text_version, method, granted_by)
		VALUES ($1,$2,$3,'data_processing',$4,'verbal',$5) RETURNING id::text`,
		e.companyID, e.merchantID, e.personID, validConsentVersion, e.userID).Scan(&consentID); err != nil {
		t.Fatalf("insert person_consent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit consent: %v", err)
	}

	tx = runtimeTenantTx(t, ctx, e.pool, e.companyID, e.merchantID)
	savepointExec(t, ctx, tx, "update consent", "permission denied",
		"UPDATE core.person_consent SET method = 'form_signed' WHERE id = $1", consentID)
	savepointExec(t, ctx, tx, "delete consent", "permission denied",
		"DELETE FROM core.person_consent WHERE id = $1", consentID)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback probe tx: %v", err)
	}
}

// TestPersonConsentPermission — a user without patient.consent.record gets 403
// with the literal message on every consent endpoint.
func TestPersonConsentPermission(t *testing.T) {
	e := setupConsentEnv(t, "consentperm.owner")
	ctx := context.Background()
	// This test owns its fresh Postgres container: dropping the grant from the
	// system Owner role cannot leak into other tests.
	if _, err := e.pool.Exec(ctx, `
		DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'patient.consent.record'`); err != nil {
		t.Fatalf("revoke patient.consent.record from Owner: %v", err)
	}

	rec := consentPost(t, e, "data_processing", validConsentVersion, "form_signed")
	if rec.Code != http.StatusForbidden || consentError(t, rec) != "missing permission: patient.consent.record" {
		t.Fatalf("record expected 403 missing permission: patient.consent.record, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = consentRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/consents", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("list expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = consentRequest(e, http.MethodGet, "/api/v1/persons/"+e.personID+"/consents/latest", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("latest expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = consentRequest(e, http.MethodGet, "/api/v1/consent-text?code=data_processing", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("consent-text expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}
