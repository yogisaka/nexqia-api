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
