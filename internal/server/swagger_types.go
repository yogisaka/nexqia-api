// internal/server/swagger_types.go
// Generic response envelope shapes for swag annotations — every handler wraps
// its payload as {"data": ..., "meta": {...}}, so swag docs reference these
// instead of redeclaring the envelope on every @Success/@Failure line.
package server

// apiResponse is the standard success envelope. Swag annotations narrow Data
// per-endpoint via generics syntax, e.g. `{object} apiResponse{data=sqlcgen.CoreDepartment}`.
type apiResponse struct {
	Data any `json:"data"`
	Meta any `json:"meta"`
}

// apiErrorResponse is the standard error envelope.
type apiErrorResponse struct {
	Error string `json:"error"`
}
