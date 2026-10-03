package health

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// RegisterRoutes registers the health check routes.
func RegisterRoutes(api huma.API) {
	handler := &healthHandler{}

	huma.Register(api, huma.Operation{
		OperationID: "health-check",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Health check",
		Description: "Check if the API is healthy",
		Tags:        []string{"Health"},
		Security:    []map[string][]string{},
	}, handler.getHealth)

	huma.Register(api, huma.Operation{
		OperationID: "health-check-head",
		Method:      http.MethodHead,
		Path:        "/health",
		Summary:     "Health check (HEAD)",
		Description: "Check if the API is healthy (HEAD request)",
		Tags:        []string{"Health"},
		Security:    []map[string][]string{},
	}, handler.headHealth)
}
