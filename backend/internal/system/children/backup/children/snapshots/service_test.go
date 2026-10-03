package snapshots

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/getarcaneapp/arcane/types/v2/backup"
	"github.com/getarcaneapp/arcane/types/v2/recovery"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/stretchr/testify/require"
)

func TestStageSystemDatabaseExcludesLiveFilesInternal(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".arcane-snapshot-stage")
	require.NoError(t, os.Mkdir(stage, 0o700))
	for _, name := range []string{RecoveryRequestName, "settings.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("data"), 0o600))
	}
	require.NoError(t, os.Symlink("settings.txt", filepath.Join(root, "link")))
	db, err := sql.Open("sqlite", filepath.Join(root, "arcane.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; CREATE TABLE evidence (id TEXT); INSERT INTO evidence VALUES ('committed')")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(filepath.Join(root, "arcane.db"), 0o640))
	projects := t.TempDir()
	composePath := filepath.Join(projects, "compose.yaml")
	require.NoError(t, os.WriteFile(composePath, []byte("services: {}"), 0o600))
	layout := backupSourceLayoutInternal{
		dataDirectory:     root,
		databaseName:      "arcane.db",
		projectsDirectory: projects,
		projectsPath:      snapshotProjectsPath,
		excludes:          []string{".arcane-snapshot-*", RecoveryRequestName, "arcane.db-wal", "arcane.db-shm", "arcane.db-journal"},
	}
	files, err := snapshotSourceFilesInternal(t.Context(), layout)
	require.NoError(t, err)
	require.NoError(t, stageSystemDatabaseInternal(t.Context(), db, filepath.Join(root, "arcane.db"), filepath.Join(stage, "arcane.db")))
	require.NoError(t, validateSnapshotSourcesInternal(t.Context(), layout, files))
	require.NoError(t, os.WriteFile(composePath, []byte("services: {app: {image: nginx}}"), 0o600))
	require.ErrorContains(t, validateSnapshotSourcesInternal(t.Context(), layout, files), "changed during capture")
	entries, err := os.ReadDir(stage)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "arcane.db", entries[0].Name())
	info, err := entries[0].Info()
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	original, err := os.Stat(filepath.Join(root, "arcane.db"))
	require.NoError(t, err)
	require.Equal(t, original.Sys().(*syscall.Stat_t).Uid, info.Sys().(*syscall.Stat_t).Uid)
	require.Equal(t, original.Sys().(*syscall.Stat_t).Gid, info.Sys().(*syscall.Stat_t).Gid)
	snapshot, err := sql.Open("sqlite", filepath.Join(stage, "arcane.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = snapshot.Close() })
	var id string
	require.NoError(t, snapshot.QueryRowContext(t.Context(), "SELECT id FROM evidence").Scan(&id))
	require.Equal(t, "committed", id)
}

func TestProjectFilesFromSnapshotInternal(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		layout   snapshotLayoutInternal
		expected []string
	}{
		{
			name: "version 1 snapshot root",
			files: []string{
				"/.arcane-recovery.json", "/arcane.db", "/arcane.db-wal", "/templates/demo.yaml",
				"/projects/demo/docker-compose.yaml", "/projects/demo/.env", "/projects/demo/data/", "/projects/demo/.env",
			},
			layout:   snapshotLayoutInternal{dataPath: "/", projectsPath: "/projects", databaseName: "arcane.db"},
			expected: []string{"demo/.env", "demo/docker-compose.yaml"},
		},
		{
			name: "legacy snapshot and custom projects root",
			files: []string{
				"/app/data/.arcane-recovery.json", "/app/data/arcane.db", "/app/data/custom/projects/nested/app/compose.yaml",
				"/app/data/projects/ignored/compose.yaml",
			},
			layout:   snapshotLayoutInternal{dataPath: "/app/data", projectsPath: "/app/data/custom/projects", databaseName: "arcane.db"},
			expected: []string{"nested/app/compose.yaml"},
		},
		{
			name: "projects directory is data root",
			files: []string{
				"/data/.arcane-recovery.json", "/data/.arcane-recovery-request.json", "/data/custom.db", "/data/custom.db-shm", "/data/demo/config.yaml",
			},
			layout:   snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data", databaseName: "custom.db"},
			expected: []string{"demo/config.yaml"},
		},
		{
			name: "external projects keep database-like names",
			files: []string{
				"/data/.arcane-recovery.json", "/data/arcane.db", "/projects/arcane.db", "/projects/demo/compose.yaml",
			},
			layout:   snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"},
			expected: []string{"arcane.db", "demo/compose.yaml"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := projectEntriesFromSnapshotInternal(test.files, test.layout, "", true)
			actual := make([]string, 0, len(entries))
			for _, entry := range entries {
				if !entry.IsDirectory {
					actual = append(actual, entry.Path)
				}
			}
			slices.Sort(actual)
			require.Equal(t, test.expected, actual)
		})
	}
}

func TestProjectsRelativePathFromManifestInternal(t *testing.T) {
	tests := []struct {
		name              string
		databaseURL       string
		projectsDirectory string
		expected          string
		errorContains     string
	}{
		{name: "absolute database path", databaseURL: "file:/app/data/arcane.db", projectsDirectory: "/app/data/historical", expected: "historical"},
		{name: "relative default database path", databaseURL: "file:data/arcane.db", projectsDirectory: "/app/data/historical", expected: "historical"},
		{name: "projects mapping", databaseURL: "file:/app/data/arcane.db", projectsDirectory: "/app/data/historical:/host/projects", expected: "historical"},
		{name: "data root", databaseURL: "file:/app/data/arcane.db", projectsDirectory: "/app/data", expected: ""},
		{name: "outside data", databaseURL: "file:/app/data/arcane.db", projectsDirectory: "/srv/projects", errorContains: errProjectsOutsideDataInternal.Error()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := recovery.Manifest{Environment: map[string]string{
				"DATABASE_URL":       test.databaseURL,
				"PROJECTS_DIRECTORY": test.projectsDirectory,
			}}
			relative, err := projectsRelativePathFromManifestInternal(manifest)
			if test.errorContains != "" {
				require.ErrorContains(t, err, test.errorContains)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, relative)
		})
	}
}

func TestProjectEntriesFromSnapshotSynthesizesFoldersAndExcludesProtectedDataInternal(t *testing.T) {
	entries := projectEntriesFromSnapshotInternal([]string{
		"/.arcane-recovery.json",
		"/.arcane-recovery-request.json",
		"/arcane.db",
		"/arcane.db-wal",
		"/arcane.db-shm",
		"/arcane.db-journal",
		"/demo/nested/compose.yaml",
		"/z.txt",
	}, snapshotLayoutInternal{dataPath: "/", projectsPath: "/", databaseName: "arcane.db"}, "", false)
	require.Equal(t, []backup.BackupFileEntry{
		{Path: "demo", Name: "demo", IsDirectory: true},
		{Path: "z.txt", Name: "z.txt"},
	}, entries)
}

func TestProjectEntriesFromSnapshotNestedBrowseInternal(t *testing.T) {
	entries := projectEntriesFromSnapshotInternal([]string{
		"/app/data/custom/projects/demo/nested/",
		"/app/data/custom/projects/demo/compose.yaml",
	}, snapshotLayoutInternal{dataPath: "/app/data", projectsPath: "/app/data/custom/projects", databaseName: "arcane.db"}, "demo", false)
	require.Equal(t, []backup.BackupFileEntry{
		{Path: "demo/nested", Name: "nested", IsDirectory: true},
		{Path: "demo/compose.yaml", Name: "compose.yaml"},
	}, entries)
}

func TestNormalizeSystemBackupSelectionInternal(t *testing.T) {
	snapshot := systemBackupSnapshotInternal{
		layout: snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data/historical", databaseName: "arcane.db"},
		entries: []backup.BackupFileEntry{
			{Path: "demo", Name: "demo", IsDirectory: true},
			{Path: "demo/compose.yaml", Name: "compose.yaml"},
		},
	}
	selected, err := normalizeSystemBackupSelectionInternal(backup.RestoreSelection{SelectAll: true}, snapshot)
	require.NoError(t, err)
	require.Equal(t, []backup.BackupFileEntry{{Path: "", Name: "historical", IsDirectory: true}}, selected)

	_, err = normalizeSystemBackupSelectionInternal(
		backup.RestoreSelection{SelectAll: true, Paths: []string{"demo"}},
		snapshot,
	)
	require.ErrorContains(t, err, "cannot be combined")

	selected, err = normalizeSystemBackupSelectionInternal(
		backup.RestoreSelection{Paths: []string{"demo/compose.yaml", "demo"}},
		snapshot,
	)
	require.NoError(t, err)
	require.Equal(t, []backup.BackupFileEntry{{Path: "demo", Name: "demo", IsDirectory: true}}, selected)

	snapshot.layout.projectsPath = snapshot.layout.dataPath
	selected, err = normalizeSystemBackupSelectionInternal(backup.RestoreSelection{SelectAll: true}, snapshot)
	require.NoError(t, err)
	require.Equal(t, []backup.BackupFileEntry{{Path: "demo", Name: "demo", IsDirectory: true}}, selected)
}

func TestSnapshotLayoutFromManifestInternal(t *testing.T) {
	legacy := map[string]string{"DATABASE_URL": "file:/app/data/arcane.db", "PROJECTS_DIRECTORY": "/app/data/projects"}
	tests := []struct {
		name          string
		manifest      recovery.Manifest
		root          string
		expected      snapshotLayoutInternal
		errorContains string
	}{
		{
			name: "version 1 projects inside data", manifest: recovery.Manifest{FormatVersion: 1, Environment: legacy}, root: "/",
			expected: snapshotLayoutInternal{dataPath: "/", projectsPath: "/projects", databaseName: "arcane.db"},
		},
		{
			name:     "version 1 projects outside data are omitted",
			manifest: recovery.Manifest{FormatVersion: 1, Environment: map[string]string{"DATABASE_URL": "file:/app/data/arcane.db", "PROJECTS_DIRECTORY": "/srv/projects"}},
			root:     "/app/data", expected: snapshotLayoutInternal{dataPath: "/app/data", databaseName: "arcane.db"},
		},
		{
			name: "version 2 external projects", manifest: recovery.Manifest{FormatVersion: 2, DataPath: "/data", ProjectsPath: "/projects", DatabasePath: "arcane.db"}, root: "/data",
			expected: snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"},
		},
		{
			name: "version 2 mismatched root", manifest: recovery.Manifest{FormatVersion: 2, DataPath: "/data", ProjectsPath: "/projects", DatabasePath: "arcane.db"}, root: "/",
			errorContains: "records data path /data but was found at /",
		},
		{
			name: "version 2 traversal", manifest: recovery.Manifest{FormatVersion: 2, DataPath: "/data", ProjectsPath: "/../etc", DatabasePath: "arcane.db"}, root: "/data",
			errorContains: "invalid projects path",
		},
		{
			name: "version 2 malformed database path", manifest: recovery.Manifest{FormatVersion: 2, DataPath: "/data", ProjectsPath: "/data", DatabasePath: ""}, root: "/data",
			errorContains: "invalid database path",
		},
		{name: "unsupported version", manifest: recovery.Manifest{FormatVersion: 3}, root: "/data", errorContains: "unsupported format 3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout, err := snapshotLayoutFromManifestInternal(test.manifest, test.root)
			if test.errorContains != "" {
				require.ErrorContains(t, err, test.errorContains)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, layout)
		})
	}
}

func TestSnapshotLayoutProtectsDataFilesInternal(t *testing.T) {
	overlapping := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data", databaseName: "arcane.db"}
	require.True(t, overlapping.protectedInternal("arcane.db-wal"))
	require.True(t, overlapping.protectedInternal(".arcane-recovery.json"))
	require.False(t, overlapping.protectedInternal("demo/arcane.db"))
	nested := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data/projects", databaseName: "arcane.db"}
	require.False(t, nested.protectedInternal("arcane.db"))
	external := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"}
	require.False(t, external.protectedInternal(".arcane-recovery.json"))
	require.False(t, snapshotLayoutInternal{dataPath: "/data"}.projectsIncludedInternal())
}

func TestProjectsSnapshotPathInternal(t *testing.T) {
	tests := []struct {
		name, data, projects, expected string
		external                       bool
		errorContains                  string
	}{
		{name: "inside data", data: "/app/data", projects: "/app/data/projects", expected: "/data/projects"},
		{name: "nested deeper", data: "/app/data", projects: "/app/data/custom/projects/", expected: "/data/custom/projects"},
		{name: "equal to data", data: "/app/data", projects: "/app/data", expected: "/data"},
		{name: "external bind", data: "/app/data", projects: "/host/path/to/projects", expected: "/projects", external: true},
		{name: "sibling with shared prefix", data: "/app/data", projects: "/app/datasets", expected: "/projects", external: true},
		{name: "ancestor of data", data: "/app/data", projects: "/app", errorContains: "contains Arcane's data directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectsPath, external, err := projectsSnapshotPathInternal(test.data, test.projects)
			if test.errorContains != "" {
				require.ErrorContains(t, err, test.errorContains)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, projectsPath)
			require.Equal(t, test.external, external)
		})
	}
}

func TestSourceMountsInternal(t *testing.T) {
	mounts := []container.MountPoint{
		{Type: mount.TypeVolume, Name: "arcane-data", Destination: "/app/data", RW: true},
		{Type: mount.TypeBind, Source: "/host/projects", Destination: "/app/data/projects", RW: true},
		{Type: mount.TypeBind, Source: "/host/external", Destination: "/srv/projects", RW: true},
	}
	data, err := sourceMountsInternal(mounts, true, "/app/data", "/data")
	require.NoError(t, err)
	require.Equal(t, []mount.Mount{
		{Type: mount.TypeVolume, Source: "arcane-data", Target: "/data", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/host/projects", Target: "/data/projects", ReadOnly: true},
	}, data)

	external, err := sourceMountsInternal(mounts, true, "/srv/projects", "/projects")
	require.NoError(t, err)
	require.Equal(t, []mount.Mount{{Type: mount.TypeBind, Source: "/host/external", Target: "/projects", ReadOnly: true}}, external)

	_, err = sourceMountsInternal(mounts, true, "/opt/projects", "/projects")
	require.ErrorContains(t, err, "/opt/projects must be mounted into the Arcane container")

	host, err := sourceMountsInternal(nil, false, "/tmp/data", "/data")
	require.NoError(t, err)
	require.Equal(t, []mount.Mount{{Type: mount.TypeBind, Source: "/tmp/data", Target: "/data", ReadOnly: true}}, host)
}

func TestWriteManifestInternalRecordsVersionTwoLayout(t *testing.T) {
	dataDirectory := t.TempDir()
	service := &Service{recoveryEnvironment: func(context.Context) map[string]string {
		return map[string]string{"PROJECTS_DIRECTORY": "/srv/projects"}
	}}
	layout := backupSourceLayoutInternal{dataDirectory: dataDirectory, projectsDirectory: "/srv/projects", databaseName: "arcane.db", projectsPath: "/projects"}
	require.NoError(t, service.writeManifestInternal(t.Context(), "backup-1", layout))
	data, err := os.ReadFile(filepath.Join(dataDirectory, RecoveryManifestName))
	require.NoError(t, err)
	var manifest recovery.Manifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.Equal(t, recovery.ManifestFormatVersion, manifest.FormatVersion)
	require.Equal(t, "/srv/projects", manifest.Environment["PROJECTS_DIRECTORY"])
	resolved, err := snapshotLayoutFromManifestInternal(manifest, "/data")
	require.NoError(t, err)
	require.Equal(t, snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"}, resolved)
}

func TestRestoreTargetInternal(t *testing.T) {
	mounts := []container.MountPoint{
		{Type: mount.TypeVolume, Name: "arcane-data", Destination: "/app/data", RW: true},
		{Type: mount.TypeBind, Source: "/host/projects", Destination: "/app/data/projects", RW: true},
		{Type: mount.TypeBind, Source: "/host/templates", Destination: "/app/data/templates", RW: true},
		{Type: mount.TypeBind, Source: "/host/ro", Destination: "/mnt/ro", RW: false},
	}
	target, err := restoreTargetInternal(mounts, "/app/data/custom/projects", "/restore-projects", "")
	require.NoError(t, err)
	require.Equal(t, "/restore-projects/custom/projects", target.Path)
	require.Equal(t, []mount.Mount{{Type: mount.TypeVolume, Source: "arcane-data", Target: "/restore-projects"}}, target.Mounts)

	data, err := restoreTargetInternal(mounts, "/app/data", "/restore", "/app/data/projects")
	require.NoError(t, err)
	require.Equal(t, "/restore", data.Path)
	require.Equal(t, []mount.Mount{
		{Type: mount.TypeVolume, Source: "arcane-data", Target: "/restore"},
		{Type: mount.TypeBind, Source: "/host/templates", Target: "/restore/templates"},
	}, data.Mounts)

	_, err = restoreTargetInternal(mounts, "/mnt/ro/projects", "/restore-projects", "")
	require.ErrorContains(t, err, "mounted read-only")
	_, err = restoreTargetInternal(mounts, "/opt/projects", "/restore-projects", "")
	require.ErrorContains(t, err, "must be mounted into the Arcane container")
}

func TestRestoreStagesInternal(t *testing.T) {
	mounts := []container.MountPoint{
		{Type: mount.TypeVolume, Name: "arcane-data", Destination: "/app/data", RW: true},
		{Type: mount.TypeBind, Source: "/host/nested", Destination: "/app/data/projects", RW: true},
		{Type: mount.TypeBind, Source: "/host/external", Destination: "/srv/projects", RW: true},
	}
	repository := recovery.RestoreRepository{Environment: []string{"RUSTIC_REPOSITORY=/repository"}}
	dataVolume := mount.Mount{Type: mount.TypeVolume, Source: "arcane-data", Target: "/restore"}
	nested := mount.Mount{Type: mount.TypeBind, Source: "/host/nested", Target: "/restore/projects"}

	t.Run("projects covered by data restore", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data/projects", databaseName: "arcane.db"}
		stages, err := restoreStagesInternal(mounts, "/app/data", "/app/data/projects", repository, "snap", layout)
		require.NoError(t, err)
		require.Equal(
			t,
			[]recovery.RestoreStage{
				{
					Repository: repository,
					SnapshotID: "snap",
					SourcePath: "/data",
					Target: recovery.RestoreTarget{
						Mounts: []mount.Mount{
							dataVolume,
							nested,
						},
						Path: "/restore",
					},
				},
			},
			stages,
		)
	})
	t.Run("external projects restore separately", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"}
		stages, err := restoreStagesInternal(mounts, "/app/data", "/srv/projects", repository, "snap", layout)
		require.NoError(t, err)
		require.Len(t, stages, 2)
		require.Equal(t, "/data", stages[0].SourcePath)
		require.Equal(t, []mount.Mount{dataVolume, nested}, stages[0].Target.Mounts)
		require.Equal(t, "/projects", stages[1].SourcePath)
		require.Equal(
			t,
			recovery.RestoreTarget{
				Mounts: []mount.Mount{
					{
						Type:   mount.TypeBind,
						Source: "/host/external",
						Target: "/restore-projects",
					},
				},
				Path: "/restore-projects",
			},
			stages[1].Target,
		)
	})
	t.Run("changed projects directory excludes the nested mount from the data stage", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/", projectsPath: "/projects", databaseName: "arcane.db"}
		stages, err := restoreStagesInternal(mounts, "/app/data", "/app/data/projects", repository, "snap", layout)
		require.NoError(t, err)
		require.Len(t, stages, 1)
		require.Equal(t, []mount.Mount{dataVolume, nested}, stages[0].Target.Mounts)

		external := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"}
		stages, err = restoreStagesInternal(mounts, "/app/data", "/app/data/projects", repository, "snap", external)
		require.NoError(t, err)
		require.Len(t, stages, 2)
		require.Equal(t, []mount.Mount{dataVolume}, stages[0].Target.Mounts)
		require.Equal(
			t,
			recovery.RestoreTarget{
				Mounts: []mount.Mount{
					{
						Type:   mount.TypeBind,
						Source: "/host/nested",
						Target: "/restore-projects",
					},
				},
				Path: "/restore-projects",
			},
			stages[1].Target,
		)

		stages, err = restoreStagesInternal(mounts, "/app/data", "/app/data/custom/projects", repository, "snap", external)
		require.NoError(t, err)
		require.Len(t, stages, 2)
		require.Equal(t, []mount.Mount{dataVolume, nested}, stages[0].Target.Mounts)
		require.Equal(
			t,
			recovery.RestoreTarget{
				Mounts: []mount.Mount{
					{
						Type:   mount.TypeVolume,
						Source: "arcane-data",
						Target: "/restore-projects",
					},
				},
				Path: "/restore-projects/custom/projects",
			},
			stages[1].Target,
		)
	})
	t.Run("version 1 backup without projects restores data only", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/app/data", databaseName: "arcane.db"}
		stages, err := restoreStagesInternal(mounts, "/app/data", "/srv/projects", repository, "snap", layout)
		require.NoError(t, err)
		require.Len(t, stages, 1)
		require.Equal(t, "/app/data", stages[0].SourcePath)
	})
	t.Run("projects at data root cannot move", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/data", databaseName: "arcane.db"}
		_, err := restoreStagesInternal(mounts, "/app/data", "/srv/projects", repository, "snap", layout)
		require.ErrorContains(t, err, "keeps projects in Arcane's data directory")
		stages, err := restoreStagesInternal(mounts, "/app/data", "/app/data", repository, "snap", layout)
		require.NoError(t, err)
		require.Len(t, stages, 1)
	})
	t.Run("unmounted destination fails", func(t *testing.T) {
		layout := snapshotLayoutInternal{dataPath: "/data", projectsPath: "/projects", databaseName: "arcane.db"}
		_, err := restoreStagesInternal(mounts, "/app/data", "/opt/projects", repository, "snap", layout)
		require.ErrorContains(t, err, "/opt/projects must be mounted")
	})
}

func TestSafetySnapshotContainsPathInternal(t *testing.T) {
	safety := systemBackupSafetySnapshotInternal{paths: map[string]struct{}{"demo": {}, "demo/compose.yaml": {}}}
	require.True(t, safetySnapshotContainsPathInternal(safety, ""))
	require.True(t, safetySnapshotContainsPathInternal(safety, "demo"))
	require.True(t, safetySnapshotContainsPathInternal(safety, "demo/compose.yaml"))
	require.False(t, safetySnapshotContainsPathInternal(safety, "other"))
	require.ErrorContains(t, removeProjectFileInternal(t.Context(), t.TempDir(), ""), "refusing to remove")
}

func TestLegacyLayoutOmittedProjectsIsActionable(t *testing.T) {
	require.Contains(t, errProjectsNotInBackupInternal.Error(), "create a new system backup")
}
