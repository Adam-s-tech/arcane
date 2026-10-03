package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getarcaneapp/arcane/types/v2/gitops"
	projecttypes "github.com/getarcaneapp/arcane/types/v2/project"
	usertypes "github.com/getarcaneapp/arcane/types/v2/user"
	"github.com/samber/mo"
	"go.getarcane.app/acfs"
	"go.getarcane.app/builds/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/projects"
)

type buildServiceInternal interface {
	BuildImage(ctx context.Context, environmentID string, req types.BuildRequest, progressWriter io.Writer, serviceName string, user *usertypes.Actor) (*types.BuildResult, error)
	BuildSettings() types.BuildSettings
}

type projectMetadataEnvInternal struct {
	projectsDirectory string
	autoInjectEnv     bool
	// settings is the snapshot shared by compose loads; nil resolves lazily.
	settings *settings.Settings
	// gitOpsComposePaths maps preloaded GitOps sync IDs to their configured
	// compose paths. Sync IDs that were preloaded but have no row map to "",
	// which resolves the same way as a missing row; sync IDs absent from the
	// map are queried per project.
	gitOpsComposePaths map[string]string

	// composeFiles memoizes successfully resolved compose files by project ID.
	// Failures are not stored so a later phase in the same request retries
	// resolution instead of replaying an error that may have been transient.
	composeFilesMu sync.Mutex
	composeFiles   map[string]string
}

func gitOpsSyncIDInternal(proj *Project) string {
	if proj == nil || proj.GitOpsManagedBy == nil {
		return ""
	}
	return strings.TrimSpace(*proj.GitOpsManagedBy)
}

// composeFileInternal memoizes a project's resolved compose file for the
// request so metadata and update enrichment share one resolution. Only
// successful resolutions are cached: a failure (for example a transient
// filesystem or GitOps lookup error) is returned as-is and the next caller
// resolves again, matching the per-phase retry of the unmemoized flow.
//
// The mutex is deliberately not held across resolve: it does filesystem and
// database work, and holding the lock would serialize the concurrent
// per-project workers. Callers run one worker per project within a phase and
// phases run back to back, so the same project is not resolved concurrently;
// if it ever were, both workers would compute the same path and the duplicate
// work is harmless.
func (env *projectMetadataEnvInternal) composeFileInternal(projectID string, resolve func() (string, error)) (string, error) {
	if env == nil || projectID == "" {
		return resolve()
	}
	env.composeFilesMu.Lock()
	cached, ok := env.composeFiles[projectID]
	env.composeFilesMu.Unlock()
	if ok {
		return cached, nil
	}
	path, err := resolve()
	if err != nil {
		return "", err
	}
	env.composeFilesMu.Lock()
	if env.composeFiles == nil {
		env.composeFiles = make(map[string]string)
	}
	env.composeFiles[projectID] = path
	env.composeFilesMu.Unlock()
	return path, nil
}

func getProjectsDirectoryOrDefaultInternal(ctx context.Context, cfg *settings.Settings) string {
	projectsDirectory, err := projects.GetProjectsDirectory(ctx, strings.TrimSpace(cfg.ProjectsDirectory.Value))
	if err != nil {
		slog.WarnContext(ctx, "unable to determine projects directory; using default", "error", err)
		return "/app/data/projects"
	}
	return projectsDirectory
}

// composeNameCacheInternal maps normalized compose project names to project
// IDs so a name lookup skips the projects table scan.
type composeNameCacheInternal struct {
	mu     sync.RWMutex
	byName map[string]string
}

func (c *composeNameCacheInternal) projectIDInternal(normalizedName string) mo.Option[string] {
	if normalizedName == "" {
		return mo.None[string]()
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.byName == nil {
		return mo.None[string]()
	}

	projectID, ok := c.byName[normalizedName]
	return mo.TupleToOption(projectID, ok)
}

func (c *composeNameCacheInternal) putInternal(normalizedName, projectID string) {
	if normalizedName == "" || projectID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.byName == nil {
		c.byName = make(map[string]string)
	}
	c.byName[normalizedName] = projectID
}

func (c *composeNameCacheInternal) invalidateInternal(normalizedName string) {
	if normalizedName == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.byName, normalizedName)
}

func (c *composeNameCacheInternal) replaceInternal(byName map[string]string) {
	c.mu.Lock()
	c.byName = byName
	c.mu.Unlock()
}

// loadGitOpsSyncForProjectInternal returns the GitOps sync linked to the
// given project, or nil if the project is not GitOps-managed. A nil sync
// with nil error is a normal outcome, not a failure.
func loadGitOpsSyncForProjectInternal(ctx context.Context, db *database.DB, projectID string) (*GitOpsSync, error) {
	if projectID == "" {
		return nil, nil
	}

	var syncRecord GitOpsSync
	err := db.WithContext(ctx).
		Where("project_id = ?", projectID).
		First(&syncRecord).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &syncRecord, nil
}

// projectCleanupDecision records a project the reconcile pass intends to delete,
// alongside the reason logged when the deletion is carried out.
type projectCleanupDecision struct {
	project Project
	reason  string
}

func skipProjectCleanupInternal(p Project, seen map[string]struct{}) bool {
	// Skip paths seen in this pass.
	if _, ok := seen[p.Path]; ok {
		return true
	}

	// Skip projects whose lifecycle is owned by the gitops system. Their compose
	// files may not exist on disk yet (e.g. during a sync or after an SSH/clone
	// failure) and should never be deleted here.
	return p.GitOpsManagedBy != nil && strings.TrimSpace(*p.GitOpsManagedBy) != ""
}

// isInternalScratchProjectInternal reports whether a project row was imported from
// one of Arcane's own scratch directories (project-update preview/backup, or GitOps
// sync-stage/backup). Such rows are never real user projects — a crash or restart
// mid-operation can leak the scratch dir, which the filesystem discovery then imports.
// They are force-removed during cleanup regardless of whether the dir still exists.
func isInternalScratchProjectInternal(p Project) bool {
	if projects.IsInternalScratchDirName(p.Name) || projects.IsInternalScratchDirName(filepath.Base(p.Path)) {
		return true
	}
	return p.DirName != nil && projects.IsInternalScratchDirName(*p.DirName)
}

func evaluateProjectPathErrorInternal(ctx context.Context, p Project, err error) mo.Option[projectCleanupDecision] {
	if os.IsNotExist(err) {
		return mo.Some(projectCleanupDecision{project: p, reason: "removed project: directory no longer exists"})
	}

	slog.WarnContext(ctx, "stat error during cleanup; keeping DB record", "path", p.Path, "error", err)
	return mo.None[projectCleanupDecision]()
}

func deleteProjectWithTagsInternal(tx *gorm.DB, projectID string) error {
	if err := tx.Where("project_id = ?", projectID).Delete(&ProjectTag{}).Error; err != nil {
		return fmt.Errorf("delete project tags: %w", err)
	}
	if err := tx.Where("mode = ? AND project_id = ?", gitops.SyncModeBackup, projectID).Delete(&GitOpsSync{}).Error; err != nil {
		return fmt.Errorf("delete project git backups: %w", err)
	}
	if err := tx.Delete(&Project{}, "id = ?", projectID).Error; err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return nil
}

// resolveAuthoritativeProjectNameInternal enforces that a top-level `name:` in
// the compose file is authoritative over the submitted project name. For
// name-only renames, it checks the compose file on disk so the lock can't be
// bypassed via the API.
func resolveAuthoritativeProjectNameInternal(ctx context.Context, proj *Project, name, composeContent *string) *string {
	if composeContent != nil {
		if yamlName := projects.ComposeContentProjectName(*composeContent); yamlName != "" {
			return &yamlName
		}
		return name
	}
	if name != nil {
		if onDiskCompose, _, readErr := projects.ReadProjectFiles(ctx, proj.Path, ""); readErr == nil {
			if yamlName := projects.ComposeContentProjectName(onDiskCompose); yamlName != "" {
				return &yamlName
			}
		}
	}
	return name
}

// gitSyncProjectContentInternal is the effective content compared before and
// after a git sync apply. Unreadable snapshots always compare as changed.
type gitSyncProjectContentInternal struct {
	compose, env, override string
	unreadable             bool
}

func projectRenameVolumeMigrationComposeNamesInternal(s *ProjectService, proj *Project, name *string) (string, string, bool) {
	if s == nil || s.dockerService == nil || proj == nil || name == nil {
		return "", "", false
	}

	newProjectName := strings.TrimSpace(*name)
	if newProjectName == "" || proj.Name == newProjectName || proj.Status != ProjectStatusStopped {
		return "", "", false
	}

	oldComposeName := projects.NormalizeProjectName(proj.Name)
	newComposeName := projects.NormalizeProjectName(newProjectName)
	if oldComposeName == "" || newComposeName == "" || oldComposeName == newComposeName {
		return "", "", false
	}

	return oldComposeName, newComposeName, true
}

func isProjectRenameRequestedInternal(proj *Project, name *string) bool {
	if proj == nil || name == nil {
		return false
	}
	newName := strings.TrimSpace(*name)
	return newName != "" && proj.Name != newName
}

func withProjectRenameRollbackInternal(ctx context.Context, proj *Project, projectStateCommitted *bool, run func() error) error {
	originalPath := proj.Path
	originalDirName := proj.DirName

	if err := run(); err != nil {
		if projectStateCommitted != nil && *projectStateCommitted {
			return err
		}
		if proj.Path != originalPath {
			// The rollback has to run even when the caller's context is already
			// cancelled, or a cancelled update leaves the directory renamed
			// with the database still pointing at the original path.
			rollbackCtx := context.WithoutCancel(ctx)

			// Both paths share a parent whenever the rename stayed inside the
			// projects directory; an imported project can sit elsewhere, in
			// which case the move crosses roots and cannot be confined.
			var renameErr error
			if parent := filepath.Dir(originalPath); parent == filepath.Dir(proj.Path) {
				renameErr = acfs.Rename(rollbackCtx, parent, "/"+filepath.Base(proj.Path), "/"+filepath.Base(originalPath))
			} else {
				renameErr = os.Rename(proj.Path, originalPath)
			}
			if renameErr != nil {
				slog.WarnContext(ctx, "failed to rollback project directory rename", "from", proj.Path, "to", originalPath, "error", renameErr)
				return err
			}
			proj.Path = originalPath
			proj.DirName = originalDirName
		}
		return err
	}

	return nil
}

// projectRecordInternal exposes a stored project's state to child features.
func projectRecordInternal(p Project) projecttypes.Record {
	return projecttypes.Record{
		ID:                 p.ID,
		Name:               p.Name,
		DirName:            p.DirName,
		Path:               p.Path,
		Status:             string(p.Status),
		StatusReason:       p.StatusReason,
		ServiceCount:       p.ServiceCount,
		RunningCount:       p.RunningCount,
		GitOpsManagedBy:    p.GitOpsManagedBy,
		ComposeProjectName: p.ComposeProjectName,
		BuildImageRefsJSON: p.BuildImageRefsJSON,
		IsArchived:         p.IsArchived,
		ArchivedAt:         p.ArchivedAt,
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

func projectRecordsInternal(projectsList []Project) []projecttypes.Record {
	records := make([]projecttypes.Record, len(projectsList))
	for i, p := range projectsList {
		records[i] = projectRecordInternal(p)
	}
	return records
}

// tagStoreInternal persists tag assignments for the tags child inside one transaction.
type tagStoreInternal struct {
	tx *gorm.DB
}

func (s tagStoreInternal) CountTag(projectID, name string, source projecttypes.TagSource) (int64, error) {
	var count int64
	err := s.tx.Model(&ProjectTag{}).Where("project_id = ? AND name = ? AND source = ?", projectID, name, source).Count(&count).Error
	return count, err
}

func (s tagStoreInternal) CountSource(projectID string, source projecttypes.TagSource) (int64, error) {
	var count int64
	err := s.tx.Model(&ProjectTag{}).Where("project_id = ? AND source = ?", projectID, source).Count(&count).Error
	return count, err
}

func (s tagStoreInternal) StoredColor(name string) (projecttypes.TagColor, bool, error) {
	var row ProjectTag
	err := s.tx.Where("name = ?", name).Order("source DESC, color").First(&row).Error
	switch {
	case err == nil:
		return projecttypes.TagColor(row.Color), true, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", false, nil
	default:
		return "", false, err
	}
}

func (s tagStoreInternal) Insert(rows []projecttypes.TagAssignment) error {
	return s.tx.Create(projectTagsInternal(rows)).Error
}

func (s tagStoreInternal) InsertIgnoringConflict(row projecttypes.TagAssignment) error {
	return s.tx.Clauses(clause.OnConflict{DoNothing: true}).Create(projectTagsInternal([]projecttypes.TagAssignment{row})).Error
}

func (s tagStoreInternal) DeleteTag(projectID, name string, source projecttypes.TagSource) error {
	return s.tx.Where("project_id = ? AND name = ? AND source = ?", projectID, name, source).Delete(&ProjectTag{}).Error
}

func (s tagStoreInternal) DeleteSource(projectID string, source projecttypes.TagSource) error {
	return s.tx.Where("project_id = ? AND source = ?", projectID, source).Delete(&ProjectTag{}).Error
}

func projectTagsInternal(rows []projecttypes.TagAssignment) []ProjectTag {
	tagRows := make([]ProjectTag, 0, len(rows))
	for _, row := range rows {
		tagRows = append(tagRows, ProjectTag{ProjectID: row.ProjectID, Name: row.Name, Source: string(row.Source), Color: string(row.Color)})
	}
	return tagRows
}

func tagAssignmentsInternal(rows []ProjectTag) []projecttypes.TagAssignment {
	assignments := make([]projecttypes.TagAssignment, 0, len(rows))
	for _, row := range rows {
		assignments = append(assignments, projecttypes.TagAssignment{ProjectID: row.ProjectID, Name: row.Name, Source: projecttypes.TagSource(row.Source), Color: projecttypes.TagColor(row.Color)})
	}
	return assignments
}
