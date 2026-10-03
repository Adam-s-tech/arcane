package workspace

import (
	"context"
	"mime/multipart"
	"testing"

	"github.com/getarcaneapp/arcane/types/v2/volume"
	"github.com/stretchr/testify/require"

	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/handlerutil"
)

func volumeWorkspacePermissionContextInternal(t *testing.T, environmentID string, permissions ...string) context.Context {
	permissionSet := authz.NewPermissionSet()
	permissionSet.AddEnv(environmentID, permissions...)
	return context.WithValue(t.Context(), middleware.ContextKeyUserPermissions, permissionSet)
}

func TestParseVolumeWorkspaceManifestInternal(t *testing.T) {
	_, err := handlerutil.ParseMultipartJSONPart[volume.WorkspaceUpdateManifest](multipart.Form{}, "manifest")
	require.Error(t, err)
	_, err = handlerutil.ParseMultipartJSONPart[volume.WorkspaceUpdateManifest](multipart.Form{Value: map[string][]string{"manifest": {"{"}}}, "manifest")
	require.Error(t, err)
	_, err = handlerutil.ParseMultipartJSONPart[volume.WorkspaceUpdateManifest](multipart.Form{Value: map[string][]string{"manifest": {"{}", "{}"}}}, "manifest")
	require.Error(t, err)

	manifest, err := handlerutil.ParseMultipartJSONPart[volume.WorkspaceUpdateManifest](multipart.Form{Value: map[string][]string{
		"manifest": {`{"fileTreeRevision":"revision","fileChanges":[{"operation":"delete","relativePath":"old.txt"}]}`},
	}}, "manifest")
	require.NoError(t, err)
	require.Equal(t, "revision", manifest.FileTreeRevision)
	require.Equal(t, volume.FileOpDelete, manifest.FileChanges[0].Operation)
}

func TestRequireVolumeWorkspacePermissionsInternal(t *testing.T) {
	const environmentID = "env-1"
	create := []volume.WorkspaceFileChange{{Operation: volume.FileOpCreateFile}}
	rename := []volume.WorkspaceFileChange{{Operation: volume.FileOpRename}}
	remove := []volume.WorkspaceFileChange{{Operation: volume.FileOpDelete}}
	restore := []volume.WorkspaceFileChange{{Operation: volume.FileOpRestoreFile}}

	require.Error(t, requireVolumeWorkspacePermissionsInternal(t.Context(), environmentID, create))
	require.NoError(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, environmentID, authz.PermVolumesUpload), environmentID, create,
	))
	require.Error(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, environmentID, authz.PermVolumesUpload), environmentID, rename,
	))
	require.NoError(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, environmentID, authz.PermVolumesUpload, authz.PermVolumesDelete), environmentID, rename,
	))
	require.NoError(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, environmentID, authz.PermVolumesDelete), environmentID, remove,
	))
	require.NoError(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, environmentID, authz.PermVolumesBackup), environmentID, restore,
	))
	require.Error(t, requireVolumeWorkspacePermissionsInternal(
		volumeWorkspacePermissionContextInternal(t, "env-2", authz.PermVolumesUpload), environmentID, create,
	))
}
