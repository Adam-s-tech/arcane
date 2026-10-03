package backup

import (
	"testing"

	"github.com/getarcaneapp/arcane/types/v2/backup"
	"github.com/libtnb/sqlite"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/config"
	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/s3"
)

func TestRecoveryHelperExecutableInternal(t *testing.T) {
	path, executableMount, err := recoveryHelperExecutableInternal(nil, "/app/arcane")
	require.NoError(t, err)
	require.Equal(t, "/app/arcane", path)
	require.Nil(t, executableMount)

	path, executableMount, err = recoveryHelperExecutableInternal([]container.MountPoint{{
		Type: mount.TypeBind, Source: "/workspace/backend", Destination: "/app/backend", RW: true,
	}}, "/app/backend/.bin/arcane")
	require.NoError(t, err)
	require.Equal(t, systemRecoveryHelperPath, path)
	require.Equal(t, mount.TypeBind, executableMount.Type)
	require.Equal(t, "/workspace/backend/.bin/arcane", executableMount.Source)
	require.Equal(t, systemRecoveryHelperPath, executableMount.Target)
	require.True(t, executableMount.ReadOnly)
}

func TestRecoveryEnvironmentInternalIncludesRuntimeSecrets(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:         "jwt-secret",
		EncryptionKey:     "encryption-secret",
		AdminStaticAPIKey: "admin-key",
		OidcClientSecret:  "oidc-secret",
		FilePerm:          0o640,
	}
	environment := (&Service{config: cfg}).recoveryEnvironmentInternal(t.Context())
	require.Equal(t, "jwt-secret", environment["JWT_SECRET"])
	require.Equal(t, "encryption-secret", environment["ENCRYPTION_KEY"])
	require.Equal(t, "admin-key", environment["ADMIN_STATIC_API_KEY"])
	require.Equal(t, "oidc-secret", environment["OIDC_CLIENT_SECRET"])
	require.Equal(t, "0640", environment["FILE_PERM"])
}

func TestProjectsDirectoryInternalUsesContainerPathOfMapping(t *testing.T) {
	service := &Service{config: &config.Config{ProjectsDirectory: "/app/data/projects:/host/projects"}}
	require.Equal(t, "/app/data/projects", service.projectsDirectoryInternal(t.Context()))
	require.Equal(t, "/app/data/projects:/host/projects", service.projectsSettingInternal(t.Context()))
}

func TestHistoryDestinationDecorationInternal(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&s3.S3Destination{}))
	destination := s3.S3Destination{Name: "Offsite", Bucket: "backups"}
	require.NoError(t, db.Create(&destination).Error)
	service := s3.NewS3DestinationService(&database.DB{DB: db}, nil)
	history := []backup.HistoryEntry{{S3DestinationID: destination.ID}, {S3DestinationID: "missing"}}
	decorateHistoryDestinationsInternal(t.Context(), service, history)
	require.Equal(t, "Offsite", history[0].S3DestinationName)
	require.Empty(t, history[1].S3DestinationName)
	require.NoError(t, db.Migrator().DropTable(&s3.S3Destination{}))
	decorateHistoryDestinationsInternal(t.Context(), service, history)
	require.Equal(t, "Offsite", history[0].S3DestinationName)
	decorateHistoryDestinationsInternal(t.Context(), nil, history)
	require.Equal(t, "Offsite", history[0].S3DestinationName)
}
