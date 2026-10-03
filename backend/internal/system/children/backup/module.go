package backup

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/internal/system/children/backup/children/volumes"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
)

// RegisterRoutes registers the system backup endpoints, including the
// system-managed volume backup endpoints.
func RegisterRoutes(api huma.API, h *Handler) {
	adminOnly := middleware.RequireGlobalAdmin(api)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "list-system-backups",
			Method:      http.MethodGet,
			Path:        "/backups",
			Summary:     "List Arcane system backups",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRead,
		h.List,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "list-backup-history",
			Method:      http.MethodGet,
			Path:        "/backups/history",
			Summary:     "List unified backup history",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRead,
		h.ListHistory,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "get-system-backup-policies",
			Method:      http.MethodGet,
			Path:        "/backups/policies",
			Summary:     "Get Arcane system backup policies",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRead,
		h.GetPolicies,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "update-system-backup-policies",
			Method:      http.MethodPut,
			Path:        "/backups/policies",
			Summary:     "Update Arcane system backup policies",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsManage,
		h.UpdatePolicies,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "generate-system-backup-recovery-key",
			Method:      http.MethodPost,
			Path:        "/backups/recovery-key/generate",
			Summary:     "Generate an Arcane system backup recovery key",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRecoveryKey,
		h.GenerateRecoveryKey,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "set-system-backup-recovery-key",
			Method:      http.MethodPut,
			Path:        "/backups/recovery-key",
			Summary:     "Configure Arcane system backup recovery key",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRecoveryKey,
		h.SetRecoveryKey,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID:   "create-system-backup",
			DefaultStatus: http.StatusAccepted,
			Method:        http.MethodPost,
			Path:          "/backups",
			Summary:       "Create Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsManage,
		h.Create,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "discover-system-backups",
			Method:      http.MethodPost,
			Path:        "/backups/discover",
			Summary:     "Discover Arcane system backups in S3",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsManage,
		h.Discover,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "restore-system-backup",
			Method:      http.MethodPost,
			Path:        "/backups/{id}/restore",
			Summary:     "Restore Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRestore,
		h.Restore,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "browse-system-backup-files",
			Method:      http.MethodPost,
			Path:        "/backups/{id}/files/browse",
			Summary:     "Browse project files in an Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRead,
		h.BrowseFiles,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "restore-system-backup-files",
			Method:      http.MethodPost,
			Path:        "/backups/{id}/restore-files",
			Summary:     "Restore project files from an Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsRestore,
		h.RestoreFiles,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "upload-system-backup",
			Method:      http.MethodPost,
			Path:        "/backups/{id}/upload",
			Summary:     "Upload Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsManage,
		h.Upload,
	)
	middleware.RegisterWithPermission(
		api,
		huma.Operation{
			OperationID: "delete-system-backup",
			Method:      http.MethodDelete,
			Path:        "/backups/{id}",
			Summary:     "Delete Arcane system backup",
			Tags: []string{
				"System Backups",
			},
			Middlewares: adminOnly,
		},
		authz.PermSystemBackupsManage,
		h.Delete,
	)

	var volumeService *volumes.Service
	if h.service != nil {
		volumeService = h.service.volumes
	}
	volumes.RegisterRoutes(api, volumes.NewHandler(volumeService, h.appCtx))
}
