//go:build integration

// internal/server/person_registration_test.go
// Spec 2026-10-07-registration-validation §4/§6: patient validation, NIK
// exemption, duplicate gate and warnings through POST/PATCH /persons.
package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/server"
)

type personRegistrationResponse struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
	Warnings []string          `json:"warnings"`
	Fields   map[string]string `json:"fields"`
}

type duplicateCandidatesResponse struct {
	Error      string `json:"error"`
	Candidates []struct {
		ID              string `json:"id"`
		MedicalRecordNo string `json:"medical_record_no"`
		FullName        string `json:"full_name"`
	} `json:"candidates"`
}

func personRegistrationPost(t *testing.T, router *gin.Engine, token, companyID, merchantID string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	return accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, payload)
}

// TestPersonRegistration_PatientRequired — patient profile without NIK and
// birth date must return 400 with per-field errors.
func TestPersonRegistration_PatientRequired(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.req", "081234580001")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Req Patient", "gender": "male", "profile": "patient",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp personRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Fields["nik"] == "" || resp.Fields["birth_date"] == "" {
		t.Fatalf("expected fields.nik and fields.birth_date, got %v", resp.Fields)
	}
}

// TestPersonRegistration_StaffLenient — staff profile accepts name+gender only.
func TestPersonRegistration_StaffLenient(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.staff", "081234580002")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Staff Lenient", "gender": "female",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonRegistration_Exemption — newborn without NIK passes with an
// exemption reason.
func TestPersonRegistration_Exemption(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.exemp", "081234580003")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Newborn Exempt", "gender": "male", "profile": "patient",
		"birth_date": "2026-10-17", "nik_exemption": "newborn",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPersonRegistration_ExemptionNoteSpaces — an "other" exemption note that
// is blank after trimming must fail validation with 400, not reach the DB
// CHECK chk_person_nik_exemption_note (review fix 2026-10-17).
func TestPersonRegistration_ExemptionNoteSpaces(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.note", "081234580007")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Note Spaces Patient", "gender": "male", "profile": "patient",
		"birth_date": "1990-01-01", "nik_exemption": "other", "nik_exemption_note": "   ",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp personRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Fields["nik_exemption_note"] != "Jelaskan alasan tanpa NIK" {
		t.Fatalf("fields[nik_exemption_note] = %q, want %q", resp.Fields["nik_exemption_note"], "Jelaskan alasan tanpa NIK")
	}
}

// TestPersonRegistration_DuplicateGate — a strong candidate (birth date +
// gender + similar name) blocks with 409 possible_duplicate; resubmitting with
// a duplicate_override_reason creates the person and records a dismissed pair.
func TestPersonRegistration_DuplicateGate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.dup", "081234580004")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Gate Patient Alpha", "gender": "male", "profile": "patient",
		"birth_date": "1990-01-01", "nik_exemption": "unidentified",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed person A expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Similar name + same birth date + gender → score 80 candidate.
	rec = personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Gate Patient Alpha II", "gender": "male", "profile": "patient",
		"birth_date": "1990-01-01", "nik_exemption": "unidentified",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var dup duplicateCandidatesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &dup); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if dup.Error != "possible_duplicate" || len(dup.Candidates) == 0 {
		t.Fatalf("expected possible_duplicate with candidates, got %s", rec.Body.String())
	}
	if dup.Candidates[0].FullName != "Reg Gate Patient Alpha" {
		t.Fatalf("candidate should be person A, got %+v", dup.Candidates[0])
	}
	personA := dup.Candidates[0].ID

	// Override with a reason → created, pair recorded dismissed.
	rec = personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		"full_name": "Reg Gate Patient Alpha II", "gender": "male", "profile": "patient",
		"birth_date": "1990-01-01", "nik_exemption": "unidentified",
		"duplicate_override_reason": "bukan orang yang sama",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("override expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created personRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM core.person_duplicate_pair
		 WHERE (person_a_id, person_b_id) = (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid))`,
		personA, created.Data.ID).Scan(&status); err != nil {
		t.Fatalf("query duplicate pair: %v", err)
	}
	if status != "dismissed" {
		t.Fatalf("pair status = %q, want dismissed", status)
	}
}

// TestPersonRegistration_Warnings — NIK whose embedded DDMMYY does not match
// the birth date yields a non-blocking nik_birthdate_mismatch warning.
func TestPersonRegistration_Warnings(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	// NIK is submitted, so the router needs pgcrypto keys (see mergeTestRouter).
	router := mergeTestRouter(t, ctx, pool)
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.warn", "081234580005")

	rec := personRegistrationPost(t, router, token, companyID, merchantID, map[string]string{
		// NIK DDMMYY = 01-12-90, birth date 1991-02-02 → mismatch.
		"full_name": "Reg Warn Patient", "gender": "female", "profile": "patient",
		"birth_date": "1991-02-02", "nik": "3201010112900001",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 (warning only), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp personRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, w := range resp.Warnings {
		if w == "nik_birthdate_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings should contain nik_birthdate_mismatch, got %v", resp.Warnings)
	}
}

// TestPersonRegistration_UpdatePatient — PATCH with profile patient and no
// birth date fails validation.
func TestPersonRegistration_UpdatePatient(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "regpat.upd", "081234580006")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Reg Update Patient", "male")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Reg Update Patient")

	body, _ := json.Marshal(map[string]string{
		"full_name": "Reg Update Patient", "gender": "male",
		"medical_record_no": mrn, "profile": "patient",
	})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
