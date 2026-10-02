//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestClinicalConfig_OwnPermissionAndResponseShape covers the clinical-config
// settings UI contract (2026-09-25): a tenant admin holding only
// core.company.manage.own (never the platform-wide core.company.manage) can
// read/write the company defaults, every mrn/diagnosis-config response is
// snake_case with is_default/is_override inside data, and diagnosis standards
// are restricted to code systems tagged "diagnosis".
func TestClinicalConfig_OwnPermissionAndResponseShape(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1"
	merchantID := "c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2"
	userID := "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	roleID := "c4c4c4c4-c4c4-c4c4-c4c4-c4c4c4c4c4c4"

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed (%s): %v", sql, err)
		}
	}
	mustExec("INSERT INTO core.company (id, code, name) VALUES ($1, 'CCFG01', 'Clinical Config Co')", companyID)
	mustExec("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MCFG01', 'Klinik Config')", merchantID, companyID)
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Config Admin')", roleID, companyID)
	for _, code := range []string{"core.company.manage.own", "core.merchant.manage", "core.person.manage"} {
		mustExec("INSERT INTO core.permission (code, description, module) VALUES ($1, 'test', 'core') ON CONFLICT (code) DO NOTHING", code)
		mustExec("INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, code)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'config.admin', $3, true)", userID, companyID, passwordHash)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID)
	mustExec("INSERT INTO terminology.code_system (system_uri, name, version, tags) VALUES ('urn:test:icd10', 'ICD-10-TEST', '1', '{diagnosis}'), ('urn:test:religion', 'RELIGION-TEST', '1', '{}')")

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "config.admin", "config-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	call := func(method, path string, body any, wantStatus int) map[string]any {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, "/api/v1"+path, reader)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Company-ID", companyID)
		req.Header.Set("X-Merchant-ID", merchantID)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("%s %s: expected %d, got %d: %s", method, path, wantStatus, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusNoContent {
			return nil
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s %s: decode: %v (%s)", method, path, err, rec.Body.String())
		}
		return env.Data
	}
	expect := func(data map[string]any, key string, want any) {
		t.Helper()
		if data[key] != want {
			t.Fatalf("expected data.%s = %v, got %v (data=%v)", key, want, data[key], data)
		}
	}

	// MRN — company default (built-in until first PUT), then merchant override lifecycle.
	d := call(http.MethodGet, "/companies/"+companyID+"/mrn-config", nil, http.StatusOK)
	expect(d, "format", "RM-{YYYY}-{SEQ:5}")
	expect(d, "is_default", true)
	d = call(http.MethodPut, "/companies/"+companyID+"/mrn-config", map[string]string{"format": "KL-{YY}-{SEQ:4}"}, http.StatusOK)
	expect(d, "format", "KL-{YY}-{SEQ:4}")
	expect(d, "is_default", false)
	d = call(http.MethodGet, "/merchants/"+merchantID+"/mrn-config", nil, http.StatusOK)
	expect(d, "format", "KL-{YY}-{SEQ:4}")
	expect(d, "is_override", false)
	d = call(http.MethodPut, "/merchants/"+merchantID+"/mrn-config", map[string]string{"format": "{MERCHANT_CODE}-{SEQ:5}"}, http.StatusOK)
	expect(d, "is_override", true)
	d = call(http.MethodGet, "/merchants/"+merchantID+"/mrn-config", nil, http.StatusOK)
	expect(d, "format", "{MERCHANT_CODE}-{SEQ:5}")
	expect(d, "is_override", true)
	call(http.MethodDelete, "/merchants/"+merchantID+"/mrn-config", nil, http.StatusNoContent)
	d = call(http.MethodGet, "/merchants/"+merchantID+"/mrn-config", nil, http.StatusOK)
	expect(d, "is_override", false)

	// Diagnosis — only tagged code systems are listed and accepted.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/terminology/code-systems?tag=diagnosis", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listReq.Header.Set("X-Company-ID", companyID)
	listReq.Header.Set("X-Merchant-ID", merchantID)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list code systems: expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Data []struct {
			Name string `json:"Name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode code systems: %v", err)
	}
	names := map[string]bool{}
	for _, s := range list.Data {
		names[s.Name] = true
	}
	if !names["ICD-10-TEST"] || names["RELIGION-TEST"] {
		t.Fatalf("expected ICD-10-TEST listed and RELIGION-TEST filtered out, got %v", names)
	}
	call(http.MethodPut, "/merchants/"+merchantID+"/diagnosis-config", map[string]string{"code_system": "RELIGION-TEST"}, http.StatusBadRequest)
	d = call(http.MethodPut, "/merchants/"+merchantID+"/diagnosis-config", map[string]string{"code_system": "ICD-10-TEST"}, http.StatusOK)
	expect(d, "code_system", "ICD-10-TEST")
	expect(d, "is_override", true)
	d = call(http.MethodGet, "/companies/"+companyID+"/diagnosis-config", nil, http.StatusOK)
	expect(d, "code_system", "ICD-10")
	expect(d, "is_default", true)
}
