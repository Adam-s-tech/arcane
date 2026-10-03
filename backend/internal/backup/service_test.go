package backup

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/backup"
	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	"github.com/libtnb/sqlite"
	"github.com/moby/moby/api/types/mount"
	"github.com/stretchr/testify/require"
	"go.getarcane.app/sys/crypto"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/jobcontext"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
	francistest "github.com/getarcaneapp/arcane/backend/v2/pkg/utils/francis/testing"
)

func TestMarkSnapshotDirectoriesInternal(t *testing.T) {
	files := []string{"/volume", "/volume/folder", "/volume/file.txt", "/volume/link"}
	longOutput := "drwxr-xr-x root root 0 1 Jan 2026 00:00 \"/volume\"\r\ndrwxr-xr-x root root 0 1 Jan 2026 00:00 \"/volume/" +
		"folder\"\r\n-rw-r--r-- root root 5 1 Jan 2026 00:00 \"/volume/file.txt\"\r\nlrwxrwxrwx root root 4 1 Jan 20" +
		"26 00:00 \"/volume/link\" -> \"file.txt\""

	marked, err := markSnapshotDirectoriesInternal(files, longOutput)
	require.NoError(t, err)
	require.Equal(t, []string{"/volume/", "/volume/folder/", "/volume/file.txt", "/volume/link"}, marked)
	require.Equal(t, []string{"/volume", "/volume/folder", "/volume/file.txt", "/volume/link"}, files)
}

func TestMarkSnapshotDirectoriesRejectsMismatchedListingsInternal(t *testing.T) {
	_, err := markSnapshotDirectoriesInternal([]string{"/volume", "/volume/file.txt"}, "drwxr-xr-x root root 0 1 Jan 2026 00:00 \"/volume\"")
	require.ErrorContains(t, err, "different lengths")
}

func TestQualifySnapshotListingInternal(t *testing.T) {
	tests := []struct {
		name         string
		files        []string
		snapshotPath string
		expected     []string
	}{
		{
			name:         "root listing",
			files:        []string{"folder/", "file.txt"},
			snapshotPath: "",
			expected:     []string{"folder/", "file.txt"},
		},
		{
			name:         "empty listing",
			files:        []string{},
			snapshotPath: "folder",
			expected:     []string{},
		},
		{
			name:         "nested listing",
			files:        []string{"nested/", "file.txt", "./link"},
			snapshotPath: "folder/",
			expected:     []string{"folder/nested/", "folder/file.txt", "folder/link"},
		},
		{
			name:         "legacy system project listing",
			files:        []string{"demo/compose.yaml"},
			snapshotPath: "/app/data/projects/",
			expected:     []string{"app/data/projects/demo/compose.yaml"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			qualified := qualifySnapshotListingInternal(test.files, test.snapshotPath)
			require.Equal(t, test.expected, qualified)
			if len(test.files) > 0 {
				require.NotSame(t, &test.files[0], &qualified[0])
			}
		})
	}
}

func TestRecoveryKeyStoreRoundTrip(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open("file:recovery-key-store?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&SystemBackupRecoveryConfig{}))
	crypto.InitEncryption(&crypto.Config{EncryptionKey: "recovery-key-store-test-key-32bytes", Environment: "test"})
	store := NewRecoveryKeyStore(&database.DB{DB: gormDB})

	configured, err := store.Configured(t.Context())
	require.NoError(t, err)
	require.False(t, configured)

	_, err = store.Get(t.Context())
	require.ErrorIs(t, err, ErrRecoveryKeyNotConfigured)

	key, err := GenerateRecoveryKey()
	require.NoError(t, err)
	require.NoError(t, store.Set(t.Context(), key))

	configured, err = store.Configured(t.Context())
	require.NoError(t, err)
	require.True(t, configured)

	stored, err := store.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, key, stored)
}

func TestRecoveryKeyValidation(t *testing.T) {
	require.NoError(t, ValidateRecoveryKey("QWERTY-ABCDEF-234567-GHIJKL-MNOPQR-STUVWX-YZ2345-ZXCVBN"))
	require.Error(t, ValidateRecoveryKey("too-short"))
	require.Error(t, ValidateRecoveryKey(""))
	require.Error(t, ValidateRecoveryKey("QWERTY-ABCDEF-234567-GHIJKL-MNOPQR-STUVWX-YZ2345-ZXCVBN-EXTRA"))

	key, err := GenerateRecoveryKey()
	require.NoError(t, err)
	require.NoError(t, ValidateRecoveryKey(key))
}

func TestSnapshotCommandInternal(t *testing.T) {
	single, err := snapshotCommandInternal("volume", RootSnapshotInput(mount.Mount{Type: mount.TypeVolume, Source: "data", Target: "/volume"}))
	require.NoError(t, err)
	require.Equal(t, []string{"backup", "--init", "--json", "--host", "arcane", "--label", "volume", "--as-path", "/", "--", "/volume"}, single)

	multi, err := snapshotCommandInternal("arcane-system-recovery", CreateSnapshotInput{Sources: []string{"/data", "/projects"}, Globs: []string{"!/data/arcane.db-wal"}})
	require.NoError(t, err)
	require.Equal(
		t,
		[]string{
			"backup",
			"--init",
			"--json",
			"--host",
			"arcane",
			"--label",
			"arcane-system-recovery",
			"--glob",
			"!/data/arcane.db-wal",
			"--",
			"/data",
			"/projects",
		},
		multi,
	)

	_, err = snapshotCommandInternal("x", CreateSnapshotInput{Sources: []string{"/data", "/projects"}, AsPath: "/"})
	require.ErrorContains(t, err, "single source")
	_, err = snapshotCommandInternal("x", CreateSnapshotInput{})
	require.ErrorContains(t, err, "at least one snapshot source")
}

func TestDurableBackupRestartKeepsCompletionInternal(t *testing.T) {
	url := "file:" + filepath.Join(t.TempDir(), "actors.db")
	var calls atomic.Int32
	runtime := francistest.New(t, url)
	engine := NewEngine(t.Context(), nil, nil)
	engine.RegisterRunKind("test", func(ctx context.Context, _ string, _ []byte, interrupted bool) error {
		calls.Add(1)
		require.False(t, interrupted)
		return jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: "snapshot", Status: scheduler.Succeeded})
	})
	require.NoError(t, engine.Register(runtime))
	francistest.Start(t, runtime)
	command := backup.DurableRunCommand{Kind: "test", RunID: "run", ActivityID: "activity"}
	require.NoError(t, engine.SubmitDurableRun(t.Context(), command, nil))
	require.Eventually(t, func() bool {
		states, err := engine.activeRunsInternal(t.Context())
		return err == nil && len(states) == 0 && calls.Load() == 1
	}, 10*time.Second, 20*time.Millisecond)
	stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, runtime.Stop(stopCtx))
	restarted := francistest.New(t, url)
	next := NewEngine(t.Context(), nil, nil)
	next.RegisterRunKind("test", func(context.Context, string, []byte, bool) error { calls.Add(1); return nil })
	require.NoError(t, next.Register(restarted))
	francistest.Start(t, restarted)
	require.NoError(t, next.SubmitDurableRun(t.Context(), command, nil))
	require.NoError(t, next.ReconcileDispatches(t.Context()))
	require.EqualValues(t, 1, calls.Load())
}

func TestDurableBackupShutdownKeepsInterruptedEvidenceInternal(t *testing.T) {
	url := "file:" + filepath.Join(t.TempDir(), "actors.db")
	runtime := francistest.New(t, url)
	admission := runs.NewAdmission(runtime.Service(), t.Name())
	require.NoError(t, admission.Register(runtime))
	engine := NewEngine(t.Context(), admission, nil)
	entered := make(chan struct{})
	drain := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-drain:
		default:
			close(drain)
		}
	})
	engine.RegisterRunKind("test", func(ctx context.Context, _ string, _ []byte, _ bool) error {
		if err := jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: "snapshot", Status: scheduler.Running, RecoveryData: []byte(`{"backupId":"frozen"}`)}); err != nil {
			return err
		}
		close(entered)
		<-ctx.Done()
		<-drain
		return ctx.Err()
	})
	require.NoError(t, engine.Register(runtime))
	francistest.Start(t, runtime)
	lease, acquired, err := engine.TryAcquireRun(t.Context(), "backup", "data")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, engine.SubmitDurableRun(t.Context(), backup.DurableRunCommand{Kind: "test", RunID: "interrupted"}, lease))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("backup did not start")
	}
	stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- engine.Stop(stopCtx) }()
	require.Eventually(t, func() bool { return engine.lifecycleCtx.Err() != nil }, time.Second, time.Millisecond)
	_, acquired, err = engine.TryAcquireRun(t.Context(), "backup", "data")
	require.NoError(t, err)
	require.False(t, acquired)
	close(drain)
	require.NoError(t, <-stopped)
	lease, acquired, err = engine.TryAcquireRun(t.Context(), "backup", "data")
	require.NoError(t, err)
	require.True(t, acquired)
	lease.Release(t.Context())
	require.NoError(t, runtime.Stop(stopCtx))
	restarted := francistest.New(t, url)
	next := NewEngine(t.Context(), nil, nil)
	resumed := make(chan struct{})
	next.RegisterRunKind("test", func(ctx context.Context, _ string, _ []byte, interrupted bool) error {
		require.True(t, interrupted)
		previous, ok := jobcontext.Run(ctx)
		require.True(t, ok)
		require.Len(t, previous.Outcome.Targets, 1)
		require.JSONEq(t, `{"backupId":"frozen"}`, string(previous.Outcome.Targets[0].RecoveryData))
		close(resumed)
		return nil
	})
	require.NoError(t, next.Register(restarted))
	francistest.Start(t, restarted)
	require.NoError(t, next.ReconcileDispatches(t.Context()))
	select {
	case <-resumed:
	case <-time.After(15 * time.Second):
		t.Fatal("durable backup did not resume")
	}
}
