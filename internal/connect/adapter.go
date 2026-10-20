// Package connect holds the outbound integration layer (Permenkes 6/2026
// Fase C1): provider-agnostic send interface plus the adapter registry.
package connect

import (
	"context"
	"sync"
)

// ProviderSatusehat is the integration_provider.code of the SATUSEHAT (Kemenkes) adapter.
const ProviderSatusehat = "satusehat"

// SendRequest is everything an adapter needs to push one FHIR resource.
type SendRequest struct {
	CompanyID        string
	MerchantID       string
	BaseURL          string // integration_config.base_url
	Config           []byte // integration_config.config (non-secret)
	Credential       string // JSON kredensial terdekripsi; "" bila tidak ada
	ResourceType     string // "Organization","Location","Patient","Encounter"
	KnownResourceID  string // "" = create (POST), terisi = update (PUT)
	IdentifierSystem string // pencarian by identifier (Patient, C3)
	IdentifierValue  string
	Payload          []byte // body FHIR JSON
}

// SendResponse is the adapter-level result of one push.
type SendResponse struct {
	ResourceID string // id resource dari server; "" bila tidak ada
	HTTPStatus int
	Body       []byte
}

// IntegrationAdapter pushes local resources to one integration provider.
type IntegrationAdapter interface {
	Send(ctx context.Context, req SendRequest) (SendResponse, error)
}

// Registry maps provider codes to adapters; safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]IntegrationAdapter
}

// NewRegistry returns an empty adapter registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]IntegrationAdapter)}
}

// Register adds an adapter under code; a duplicate code is a programmer error.
func (r *Registry) Register(code string, a IntegrationAdapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[code]; ok {
		panic("connect: duplicate adapter " + code)
	}
	r.adapters[code] = a
}

// Adapter returns the adapter registered under code, or nil when unknown.
func (r *Registry) Adapter(code string) IntegrationAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adapters[code]
}
