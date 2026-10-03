package version

import (
	"context"
	"strings"

	"github.com/getarcaneapp/arcane/types/v2/version"
)

// VersionHandler handles version information endpoints.
type VersionHandler struct {
	versionService *VersionService
}

type GetVersionInput struct {
	Current string `query:"current" doc:"Current version to compare against"`
}

type GetVersionOutput struct {
	Body version.Check
}

type GetAppVersionInput struct{}

type GetAppVersionOutput struct {
	Body version.Info
}

// GetVersion returns version information with optional update check.
func (h *VersionHandler) GetVersion(ctx context.Context, input *GetVersionInput) (*GetVersionOutput, error) {
	current := strings.TrimSpace(input.Current)
	check, _ := h.versionService.GetVersionInformation(ctx, current)

	return &GetVersionOutput{
		Body: *check,
	}, nil
}

// GetAppVersion returns the current application version.
func (h *VersionHandler) GetAppVersion(ctx context.Context, _ *GetAppVersionInput) (*GetAppVersionOutput, error) {
	info := h.versionService.GetAppVersionInfo(ctx)

	return &GetAppVersionOutput{
		Body: *info,
	}, nil
}
