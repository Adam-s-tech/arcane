package search

import (
	"context"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/getarcaneapp/arcane/types/v2/category"
	"github.com/getarcaneapp/arcane/types/v2/search"

	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
)

// CustomizeHandler handles customization search endpoints.
type CustomizeHandler struct {
	customizeSearchService *CustomizeSearchService
}

type SearchCustomizeInput struct {
	Body search.Request
}

type SearchCustomizeOutput struct {
	Body search.Response
}

type GetCustomizeCategoriesInput struct{}

type GetCustomizeCategoriesOutput struct {
	Body []category.Category
}

func filterCustomizeCategoriesInternal(ps *authz.PermissionSet, categories []category.Category) []category.Category {
	if ps == nil {
		return []category.Category{}
	}
	filtered := make([]category.Category, 0, len(categories))
	for _, cat := range categories {
		if authz.CanAccessCustomizeCategory(ps, cat.ID, "") {
			filtered = append(filtered, cat)
		}
	}
	return filtered
}

// Search searches customization options by query.
func (h *CustomizeHandler) Search(ctx context.Context, input *SearchCustomizeInput) (*SearchCustomizeOutput, error) {
	if strings.TrimSpace(input.Body.Query) == "" {
		return nil, huma.Error400BadRequest("Query parameter is required")
	}

	ps, _ := middleware.PermissionsFromContext(ctx)
	results := Search(h.customizeSearchService.GetCustomizeCategories(), input.Body.Query, search.CustomizeProfile)
	results.Results = filterCustomizeCategoriesInternal(ps, results.Results)
	results.Count = len(results.Results)

	return &SearchCustomizeOutput{
		Body: results,
	}, nil
}

// GetCategories returns all available customization categories.
func (h *CustomizeHandler) GetCategories(ctx context.Context, input *GetCustomizeCategoriesInput) (*GetCustomizeCategoriesOutput, error) {
	ps, _ := middleware.PermissionsFromContext(ctx)
	categories := filterCustomizeCategoriesInternal(ps, h.customizeSearchService.GetCustomizeCategories())

	return &GetCustomizeCategoriesOutput{
		Body: categories,
	}, nil
}
