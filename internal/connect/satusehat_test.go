package connect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// jsonMap unmarshals b for comparison against expected literal maps.
func jsonMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}

func assertMapEqual(t *testing.T, got, want map[string]any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("payload mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestSatusehatTokenAndSend: stub token + POST; assert Authorization header,
// body passthrough, and ResourceID from the response "id".
func TestSatusehatTokenAndSend(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/v1/accesstoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": 3600})
		case "/fhir-r4/v1/Organization":
			gotAuth = r.Header.Get("Authorization")
			gotCT = r.Header.Get("Content-Type")
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"resourceType":"Organization","id":"srv-1"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := NewSatusehatAdapter()
	resp, err := a.Send(context.Background(), SendRequest{
		MerchantID:   "m-1",
		BaseURL:      srv.URL,
		Credential:   `{"client_id":"cid","client_secret":"csec"}`,
		ResourceType: "Organization",
		Payload:      []byte(`{"resourceType":"Organization","name":"X"}`),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer tok-1")
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	if string(gotBody) != `{"resourceType":"Organization","name":"X"}` {
		t.Fatalf("body not passed through: %s", gotBody)
	}
	if resp.ResourceID != "srv-1" || resp.HTTPStatus != 200 {
		t.Fatalf("resp = %+v", resp)
	}
}

// TestSatusehatUpdateById: KnownResourceID set means PUT to the resource URL
// and the request body must carry the same "id".
func TestSatusehatUpdateById(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/v1/accesstoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-2", "expires_in": 3600})
		default:
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"id":"org-1"}`))
		}
	}))
	defer srv.Close()

	a := NewSatusehatAdapter()
	resp, err := a.Send(context.Background(), SendRequest{
		MerchantID:      "m-1",
		BaseURL:         srv.URL,
		Credential:      `{"client_id":"cid","client_secret":"csec"}`,
		ResourceType:    "Organization",
		KnownResourceID: "org-1",
		Payload:         []byte(`{"resourceType":"Organization","id":"org-1"}`),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/fhir-r4/v1/Organization/org-1" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["id"] != "org-1" {
		t.Fatalf("body id = %v, want org-1", body["id"])
	}
	if resp.ResourceID != "org-1" {
		t.Fatalf("ResourceID = %q", resp.ResourceID)
	}
}

// TestSatusehatPutInjectsId: worker PUT re-sends a stored payload built
// WITHOUT "id" (Patient/Encounter builders, temuan review); Send must inject
// "id" = KnownResourceID into the PUT body. POST (no KnownResourceID) must
// NOT gain an "id".
func TestSatusehatPutInjectsId(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v1/accesstoken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-3", "expires_in": 3600})
			return
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	a := NewSatusehatAdapter()

	// Patient PUT cannot be reached through Send (search-first always resets
	// KnownResourceID for Patient), so the injection is asserted via Send on
	// Encounter/Organization and directly on the helper for Patient.
	if got := string(injectPayloadID([]byte(`{"resourceType":"Patient","gender":"male"}`), "ihs-123")); !strings.Contains(got, `"id":"ihs-123"`) {
		t.Fatalf("injectPayloadID Patient = %s", got)
	}

	// Encounter PUT from a stored payload without "id" (worker re-sync path).
	if _, err := a.Send(context.Background(), SendRequest{
		MerchantID:      "m-1",
		BaseURL:         srv.URL,
		Credential:      `{"client_id":"cid","client_secret":"csec"}`,
		ResourceType:    "Encounter",
		KnownResourceID: "enc-9",
		Payload:         []byte(`{"resourceType":"Encounter","status":"finished","class":{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"AMB","display":"ambulatory"}}`),
	}); err != nil {
		t.Fatalf("Send Encounter PUT: %v", err)
	}
	body := jsonMap(t, gotBody)
	if body["id"] != "enc-9" {
		t.Fatalf("Encounter PUT body id = %v, want enc-9", body["id"])
	}

	// POST create: no "id" injected.
	if _, err := a.Send(context.Background(), SendRequest{
		MerchantID:   "m-1",
		BaseURL:      srv.URL,
		Credential:   `{"client_id":"cid","client_secret":"csec"}`,
		ResourceType: "Patient",
		Payload:      []byte(`{"resourceType":"Patient","identifier":[{"system":"https://fhir.kemkes.go.id/id/nik","value":"3273011205780001"}],"gender":"male"}`),
	}); err != nil {
		t.Fatalf("Send Patient POST: %v", err)
	}
	body = jsonMap(t, gotBody)
	if _, has := body["id"]; has {
		t.Fatalf("POST body must not carry id: %s", gotBody)
	}
}

// TestSatusehatTokenCacheKey: same merchant, two base_urls — the token
// endpoint must be called twice, not once.
func TestSatusehatTokenCacheKey(t *testing.T) {
	var tokenCalls int64
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v1/accesstoken" {
			atomic.AddInt64(&tokenCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t1", "expires_in": 3600})
			return
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v1/accesstoken" {
			atomic.AddInt64(&tokenCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t2", "expires_in": 3600})
			return
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv2.Close()

	a := NewSatusehatAdapter()
	for _, base := range []string{srv1.URL, srv2.URL} {
		if _, err := a.Send(context.Background(), SendRequest{
			MerchantID:   "m-same",
			BaseURL:      base,
			Credential:   `{"client_id":"cid","client_secret":"csec"}`,
			ResourceType: "Organization",
			Payload:      []byte(`{}`),
		}); err != nil {
			t.Fatalf("Send %s: %v", base, err)
		}
	}
	if n := atomic.LoadInt64(&tokenCalls); n != 2 {
		t.Fatalf("token endpoint called %d times, want 2", n)
	}
}

// TestSatusehatMissingBaseURL: empty base_url errors before any HTTP call.
func TestSatusehatMissingBaseURL(t *testing.T) {
	a := NewSatusehatAdapter()
	_, err := a.Send(context.Background(), SendRequest{
		MerchantID:   "m-1",
		ResourceType: "Organization",
		Payload:      []byte(`{}`),
	})
	if err == nil || err.Error() != "satusehat: base_url not configured" {
		t.Fatalf("err = %v", err)
	}
}

// TestBuildOrganizationPayload: literal shape from the plan, with/without id
// and with the Code fallback for the identifier value.
func TestBuildOrganizationPayload(t *testing.T) {
	// With FHIRID and Kemenkes facility code.
	got := jsonMap(t, BuildOrganizationPayload(OrganizationInput{
		MerchantID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Code:               "RS-DEMO-001",
		KemkesFacilityCode: "RS-DEMO-001",
		Name:               "Klinik Demo",
		FHIRID:             "org-123",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Organization",
		"id":           "org-123",
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/organization/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "RS-DEMO-001",
		}},
		"name":   "Klinik Demo",
		"active": true,
	})

	// Without FHIRID: "id" absent; identifier falls back to Code.
	got = jsonMap(t, BuildOrganizationPayload(OrganizationInput{
		MerchantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Code:       "CODE-2",
		Name:       "Klinik Dua",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Organization",
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/organization/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "CODE-2",
		}},
		"name":   "Klinik Dua",
		"active": true,
	})
}

// TestBuildLocationPayload: literal shape from the plan across the three
// variants: full (id + org ref + position), no position, minimal.
func TestBuildLocationPayload(t *testing.T) {
	lat := -6.9175
	lng := 107.6191

	// Full variant.
	got := jsonMap(t, BuildLocationPayload(LocationInput{
		MerchantID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		LocationCode: "R1",
		Name:         "Ruang Praktik 1",
		Latitude:     &lat,
		Longitude:    &lng,
		OrgFHIRID:    "org-123",
		FHIRID:       "loc-456",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Location",
		"id":           "loc-456",
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/location/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "R1",
		}},
		"name":                 "Ruang Praktik 1",
		"mode":                 "instance",
		"managingOrganization": map[string]any{"reference": "Organization/org-123"},
		"position":             map[string]any{"longitude": 107.6191, "latitude": -6.9175},
	})

	// Without position and without managingOrganization; id present.
	got = jsonMap(t, BuildLocationPayload(LocationInput{
		MerchantID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		LocationCode: "R2",
		Name:         "Ruang Dua",
		FHIRID:       "loc-789",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Location",
		"id":           "loc-789",
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/location/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "R2",
		}},
		"name": "Ruang Dua",
		"mode": "instance",
	})

	// Minimal: no id, no org ref, no position.
	got = jsonMap(t, BuildLocationPayload(LocationInput{
		MerchantID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		LocationCode: "R3",
		Name:         "Ruang Tiga",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Location",
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/location/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "R3",
		}},
		"name": "Ruang Tiga",
		"mode": "instance",
	})
}

// TestBuildPatientPayload: literal shape from the plan, plus the variant
// without birthDate (omitted key, not empty string).
func TestBuildPatientPayload(t *testing.T) {
	got := jsonMap(t, BuildPatientPayload(PatientInput{
		NIK:       "3273011205780001",
		FullName:  "Pasien Demo",
		Gender:    "male",
		BirthDate: "1990-01-01",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Patient",
		"identifier": []any{map[string]any{
			"system": "https://fhir.kemkes.go.id/id/nik",
			"value":  "3273011205780001",
		}},
		"name":      []any{map[string]any{"use": "official", "text": "Pasien Demo"}},
		"gender":    "male",
		"birthDate": "1990-01-01",
	})

	// Without birthDate: the key must be absent entirely.
	got = jsonMap(t, BuildPatientPayload(PatientInput{
		NIK:      "3273016008820002",
		FullName: "Pasien Tanpa Tanggal",
		Gender:   "female",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Patient",
		"identifier": []any{map[string]any{
			"system": "https://fhir.kemkes.go.id/id/nik",
			"value":  "3273016008820002",
		}},
		"name":   []any{map[string]any{"use": "official", "text": "Pasien Tanpa Tanggal"}},
		"gender": "female",
	})
}

// TestBuildEncounterPayload: literal shape from the plan, plus the variant
// without period.end and serviceProvider (omitted keys, not empty strings).
func TestBuildEncounterPayload(t *testing.T) {
	got := jsonMap(t, BuildEncounterPayload(EncounterInput{
		MerchantID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		VisitNo:     "RJ-20261020-0001",
		PatientIHS:  "ihs-123",
		OrgFHIRID:   "org-123",
		StatusFHIR:  "in-progress",
		PeriodStart: "2026-10-20T08:00:00+07:00",
		PeriodEnd:   "2026-10-20T11:30:00+07:00",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Encounter",
		"status":       "in-progress",
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    "AMB",
			"display": "ambulatory",
		},
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/encounter/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "RJ-20261020-0001",
		}},
		"subject":         map[string]any{"reference": "Patient/ihs-123"},
		"serviceProvider": map[string]any{"reference": "Organization/org-123"},
		"period": map[string]any{
			"start": "2026-10-20T08:00:00+07:00",
			"end":   "2026-10-20T11:30:00+07:00",
		},
	})

	// Variant: no OrgFHIRID and no PeriodEnd — keys absent entirely.
	got = jsonMap(t, BuildEncounterPayload(EncounterInput{
		MerchantID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		VisitNo:     "RJ-20261020-0002",
		PatientIHS:  "ihs-456",
		StatusFHIR:  "finished",
		PeriodStart: "2026-10-20T09:00:00+07:00",
	}))
	assertMapEqual(t, got, map[string]any{
		"resourceType": "Encounter",
		"status":       "finished",
		"class": map[string]any{
			"system":  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			"code":    "AMB",
			"display": "ambulatory",
		},
		"identifier": []any{map[string]any{
			"system": "http://sys-ids.kemkes.go.id/encounter/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"value":  "RJ-20261020-0002",
		}},
		"subject": map[string]any{"reference": "Patient/ihs-456"},
		"period": map[string]any{
			"start": "2026-10-20T09:00:00+07:00",
		},
	})
}
