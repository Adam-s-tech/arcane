package health

import (
	"context"

	"github.com/getarcaneapp/arcane/types/v2/system"
)

type healthHandler struct{}

type healthOutput struct {
	Body system.HealthResponse
}

func (h *healthHandler) getHealth(context.Context, *struct{}) (*healthOutput, error) {
	return &healthOutput{
		Body: system.HealthResponse{
			Status: "UP",
		},
	}, nil
}

func (h *healthHandler) headHealth(context.Context, *struct{}) (*struct{}, error) {
	return nil, nil
}
