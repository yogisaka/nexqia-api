// SATUSEHAT (Kemenkes) FHIR R4 adapter (Permenkes 6/2026 Fase C2): OAuth2
// token with per merchant+base_url caching plus Organization/Location payload
// builders with stable JSON key order.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// satusehatResourceTypes is the closed set of FHIR resource types this
// adapter is allowed to push.
var satusehatResourceTypes = map[string]bool{
	"Organization": true,
	"Location":     true,
	"Patient":      true,
	"Encounter":    true,
}

// tokenEntry is one cached OAuth2 access token; expiry already includes the
// 60s safety margin.
type tokenEntry struct {
	accessToken string
	expiry      time.Time
}

// SatusehatAdapter pushes FHIR resources to a SATUSEHAT (Kemenkes) endpoint.
type SatusehatAdapter struct {
	httpClient *http.Client
	mu         sync.Mutex
	tokens     map[string]tokenEntry
}

// NewSatusehatAdapter returns an adapter with a 30s HTTP client and an empty
// token cache.
func NewSatusehatAdapter() *SatusehatAdapter {
	return &SatusehatAdapter{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		tokens:     make(map[string]tokenEntry),
	}
}

// satusehatCredential is the decrypted integration credential JSON.
type satusehatCredential struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Send pushes one FHIR resource: token acquisition (when a credential is
// configured) followed by a POST (create) or PUT (update by known id).
func (a *SatusehatAdapter) Send(ctx context.Context, req SendRequest) (SendResponse, error) {
	if req.BaseURL == "" {
		return SendResponse{}, fmt.Errorf("satusehat: base_url not configured")
	}
	if !satusehatResourceTypes[req.ResourceType] {
		return SendResponse{}, fmt.Errorf("satusehat: unsupported resource type %s", req.ResourceType)
	}

	token := ""
	if req.Credential != "" {
		var err error
		token, err = a.token(ctx, req)
		if err != nil {
			return SendResponse{}, err
		}
	}

	if req.ResourceType == "Patient" {
		found, resp, err := a.findPatientByIDentifier(ctx, req, token)
		if err != nil {
			return SendResponse{}, err
		}
		if found {
			return resp, nil
		}
		// Bundle kosong (atau search gagal) ⇒ lanjut POST create: id lokal
		// yang sudah dikenal tidak dipakai untuk PUT karena IHS-nya tidak ada.
		req.KnownResourceID = ""
	}

	var url string
	method := http.MethodPost
	if req.KnownResourceID == "" {
		url = fmt.Sprintf("%s/fhir-r4/v1/%s", req.BaseURL, req.ResourceType)
	} else {
		method = http.MethodPut
		url = fmt.Sprintf("%s/fhir-r4/v1/%s/%s", req.BaseURL, req.ResourceType, req.KnownResourceID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(req.Payload))
	if err != nil {
		return SendResponse{}, err
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return SendResponse{}, err
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return SendResponse{}, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return SendResponse{}, fmt.Errorf("satusehat: http %d: %s", httpResp.StatusCode, truncate(string(body), 500))
	}

	resourceID := req.KnownResourceID
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.ID != "" {
		resourceID = parsed.ID
	}
	return SendResponse{ResourceID: resourceID, HTTPStatus: httpResp.StatusCode, Body: body}, nil
}

// findPatientByIDentifier resolves an existing Patient IHS via the FHIR
// search endpoint (GET ?identifier=<system>|<value>) BEFORE creating: 2xx +
// non-empty Bundle entry ⇒ found (ResourceID = entry[0].resource.id, no POST).
// The identifier comes from req.IdentifierSystem/IdentifierValue when set,
// otherwise it is parsed from req.Payload (identifier[0]) — the C1 worker
// does not set the fields explicitly.
func (a *SatusehatAdapter) findPatientByIDentifier(ctx context.Context, req SendRequest, token string) (bool, SendResponse, error) {
	system, value := req.IdentifierSystem, req.IdentifierValue
	if system == "" || value == "" {
		var p struct {
			Identifier []satusehatIdentifier `json:"identifier"`
		}
		if err := json.Unmarshal(req.Payload, &p); err == nil && len(p.Identifier) > 0 {
			if system == "" {
				system = p.Identifier[0].System
			}
			if value == "" {
				value = p.Identifier[0].Value
			}
		}
	}
	if system == "" || value == "" {
		return false, SendResponse{}, fmt.Errorf("satusehat: patient identifier required")
	}
	searchURL := fmt.Sprintf("%s/fhir-r4/v1/Patient?identifier=%s", req.BaseURL, url.QueryEscape(system+"|"+value))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return false, SendResponse{}, err
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return false, SendResponse{}, err
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return false, SendResponse{}, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return false, SendResponse{}, nil
	}
	var bundle struct {
		Entry []struct {
			Resource struct {
				ID string `json:"id"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &bundle); err != nil || len(bundle.Entry) == 0 ||
		bundle.Entry[0].Resource.ID == "" {
		return false, SendResponse{}, nil
	}
	return true, SendResponse{
		ResourceID: bundle.Entry[0].Resource.ID,
		HTTPStatus: httpResp.StatusCode,
		Body:       body,
	}, nil
}

// token returns a cached access token or fetches a new one; the cache key
// includes base_url so switching realm invalidates old tokens.
func (a *SatusehatAdapter) token(ctx context.Context, req SendRequest) (string, error) {
	key := req.MerchantID + "|" + req.BaseURL
	a.mu.Lock()
	if entry, ok := a.tokens[key]; ok && time.Now().Before(entry.expiry) {
		a.mu.Unlock()
		return entry.accessToken, nil
	}
	a.mu.Unlock()

	var cred satusehatCredential
	if err := json.Unmarshal([]byte(req.Credential), &cred); err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]string{
		"client_id":     cred.ClientID,
		"client_secret": cred.ClientSecret,
	})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		req.BaseURL+"/oauth2/v1/accesstoken", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return "", fmt.Errorf("satusehat: token http %d", httpResp.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}

	entry := tokenEntry{
		accessToken: parsed.AccessToken,
		expiry:      time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second).Add(-60 * time.Second),
	}
	a.mu.Lock()
	a.tokens[key] = entry
	a.mu.Unlock()
	return entry.accessToken, nil
}

// OrganizationInput is the local data needed for one Organization payload.
type OrganizationInput struct {
	MerchantID         string
	Code               string
	KemkesFacilityCode string
	Name               string
	FHIRID             string
}

type satusehatIdentifier struct {
	System string `json:"system"`
	Value  string `json:"value"`
}

type organizationPayload struct {
	ResourceType string                `json:"resourceType"`
	ID           string                `json:"id,omitempty"`
	Identifier   []satusehatIdentifier `json:"identifier"`
	Name         string                `json:"name"`
	Active       bool                  `json:"active"`
}

// BuildOrganizationPayload builds the FHIR Organization JSON; "id" is only
// present when FHIRID is set, and identifier falls back to Code when no
// Kemenkes facility code exists.
func BuildOrganizationPayload(in OrganizationInput) []byte {
	value := in.KemkesFacilityCode
	if value == "" {
		value = in.Code
	}
	p := organizationPayload{
		ResourceType: "Organization",
		ID:           in.FHIRID,
		Identifier: []satusehatIdentifier{{
			System: "http://sys-ids.kemkes.go.id/organization/" + in.MerchantID,
			Value:  value,
		}},
		Name:   in.Name,
		Active: true,
	}
	b, _ := json.Marshal(p)
	return b
}

// LocationInput is the local data needed for one Location payload.
type LocationInput struct {
	MerchantID   string
	LocationCode string
	Name         string
	Latitude     *float64
	Longitude    *float64
	OrgFHIRID    string
	FHIRID       string
}

type satusehatReference struct {
	Reference string `json:"reference"`
}

type satusehatPosition struct {
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
}

type locationPayload struct {
	ResourceType         string                `json:"resourceType"`
	ID                   string                `json:"id,omitempty"`
	Identifier           []satusehatIdentifier `json:"identifier"`
	Name                 string                `json:"name"`
	Mode                 string                `json:"mode"`
	ManagingOrganization *satusehatReference   `json:"managingOrganization,omitempty"`
	Position             *satusehatPosition    `json:"position,omitempty"`
}

// BuildLocationPayload builds the FHIR Location JSON; "id" only with FHIRID,
// position only with both coordinates, managingOrganization only with an
// Organization FHIR id.
func BuildLocationPayload(in LocationInput) []byte {
	p := locationPayload{
		ResourceType: "Location",
		ID:           in.FHIRID,
		Identifier: []satusehatIdentifier{{
			System: "http://sys-ids.kemkes.go.id/location/" + in.MerchantID,
			Value:  in.LocationCode,
		}},
		Name: in.Name,
		Mode: "instance",
	}
	if in.OrgFHIRID != "" {
		p.ManagingOrganization = &satusehatReference{Reference: "Organization/" + in.OrgFHIRID}
	}
	if in.Latitude != nil && in.Longitude != nil {
		p.Position = &satusehatPosition{Longitude: *in.Longitude, Latitude: *in.Latitude}
	}
	b, _ := json.Marshal(p)
	return b
}

// PatientIdentifierSystemNik is the FHIR identifier.system for the Indonesian
// NIK (Permenkes 6/2026 Fase C3).
const PatientIdentifierSystemNik = "https://fhir.kemkes.go.id/id/nik"

// PatientInput is the local data needed for one Patient payload. NIK must
// already be the DECRYPTED plaintext (handler-side decryptNIK); ciphertext is
// never accepted here (temuan review 1).
type PatientInput struct {
	NIK       string // 16 digit plaintext
	FullName  string
	Gender    string // "male"/"female" langsung dari kolom
	BirthDate string // YYYY-MM-DD atau ""
}

type patientName struct {
	Use  string `json:"use"`
	Text string `json:"text"`
}

type patientPayload struct {
	ResourceType string                `json:"resourceType"`
	Identifier   []satusehatIdentifier `json:"identifier"`
	Name         []patientName         `json:"name"`
	Gender       string                `json:"gender"`
	BirthDate    string                `json:"birthDate,omitempty"`
}

// BuildPatientPayload builds the FHIR Patient JSON; birthDate is omitted when
// empty, gender passes through as-is (male/female).
func BuildPatientPayload(in PatientInput) []byte {
	p := patientPayload{
		ResourceType: "Patient",
		Identifier: []satusehatIdentifier{{
			System: PatientIdentifierSystemNik,
			Value:  in.NIK,
		}},
		Name:   []patientName{{Use: "official", Text: in.FullName}},
		Gender: in.Gender,
	}
	if in.BirthDate != "" {
		p.BirthDate = in.BirthDate
	}
	b, _ := json.Marshal(p)
	return b
}

// EncounterInput is the local data needed for one outpatient Encounter
// payload (Permenkes 6/2026 Fase C3). StatusFHIR is the ALREADY MAPPED FHIR
// status ("in-progress"/"finished"); the operations.admission → FHIR mapping
// lives in the server handler. PeriodStart/PeriodEnd are RFC3339 timestamps
// in the merchant timezone ("" = omit).
type EncounterInput struct {
	MerchantID  string
	VisitNo     string
	PatientIHS  string
	OrgFHIRID   string
	StatusFHIR  string
	PeriodStart string
	PeriodEnd   string
}

type encounterClass struct {
	System  string `json:"system"`
	Code    string `json:"code"`
	Display string `json:"display"`
}

type encounterPeriod struct {
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
}

type encounterPayload struct {
	ResourceType    string                `json:"resourceType"`
	Status          string                `json:"status"`
	Class           encounterClass        `json:"class"`
	Identifier      []satusehatIdentifier `json:"identifier"`
	Subject         satusehatReference    `json:"subject"`
	ServiceProvider *satusehatReference   `json:"serviceProvider,omitempty"`
	Period          encounterPeriod       `json:"period"`
}

// BuildEncounterPayload builds the FHIR Encounter JSON (ambulatory class);
// period.end and serviceProvider are omitted when the source string is empty.
func BuildEncounterPayload(in EncounterInput) []byte {
	p := encounterPayload{
		ResourceType: "Encounter",
		Status:       in.StatusFHIR,
		Class: encounterClass{
			System:  "http://terminology.hl7.org/CodeSystem/v3-ActCode",
			Code:    "AMB",
			Display: "ambulatory",
		},
		Identifier: []satusehatIdentifier{{
			System: "http://sys-ids.kemkes.go.id/encounter/" + in.MerchantID,
			Value:  in.VisitNo,
		}},
		Subject: satusehatReference{Reference: "Patient/" + in.PatientIHS},
		Period:  encounterPeriod{Start: in.PeriodStart, End: in.PeriodEnd},
	}
	if in.OrgFHIRID != "" {
		p.ServiceProvider = &satusehatReference{Reference: "Organization/" + in.OrgFHIRID}
	}
	b, _ := json.Marshal(p)
	return b
}
