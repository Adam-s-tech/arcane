// Package sync manages the project env files a git sync writes: the git
// source, the Arcane override and the merged effective .env.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/getarcaneapp/arcane/types/v2/project"

	"github.com/getarcaneapp/arcane/backend/v2/pkg/projects"
)

func PersistEffectiveEnvContent(ctx context.Context, projectPath, projectsDirectory, envContent string) error {
	state, err := projects.ReadProjectEnvState(projectPath)
	if err != nil {
		return fmt.Errorf("read project env state: %w", err)
	}

	// WriteManagedEnvFile skips unreadable paths so git sync keeps working; an
	// explicit env save would then be dropped without any feedback.
	targets := []string{projects.EffectiveEnvFileName}
	if state.HasGitSource {
		targets = append(targets, projects.OverrideEnvFileName)
	}
	for _, name := range targets {
		if info, statErr := os.Stat(filepath.Join(projectPath, name)); statErr == nil && info.IsDir() {
			return fmt.Errorf("cannot save environment: %s is a directory", name)
		}
	}

	if state.HasGitSource && state.HasEffective && envContent == state.EffectiveContent {
		storedEffectiveContent, buildErr := projects.BuildEffectiveEnvContent(state.GitContent, state.OverrideContent)
		if buildErr == nil && envContent == storedEffectiveContent {
			return nil
		}
	}

	if !state.HasGitSource {
		if state.HasOverride {
			if removeProjectFileErr := projects.RemoveProjectFile(ctx, projectsDirectory, projectPath, projects.OverrideEnvFileName); removeProjectFileErr != nil {
				return removeProjectFileErr
			}
		}
		return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.EffectiveEnvFileName, state.EffectiveUnreadable, envContent)
	}

	overrideContent, err := projects.BuildOverrideEnvContent(state.GitContent, envContent)
	if err != nil {
		return fmt.Errorf("build override env content: %w", err)
	}

	effectiveContent, err := projects.BuildEffectiveEnvContent(state.GitContent, overrideContent)
	if err != nil {
		return fmt.Errorf("build effective env content: %w", err)
	}

	if writeManagedEnvFileErr := projects.WriteManagedEnvFile(
		ctx,
		projectsDirectory,
		projectPath,
		projects.EffectiveEnvFileName,
		state.EffectiveUnreadable,
		effectiveContent,
	); writeManagedEnvFileErr != nil {
		return writeManagedEnvFileErr
	}

	return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.OverrideEnvFileName, state.OverrideUnreadable, overrideContent)
}

func ResolveStoredEffectiveEnvContent(state projects.ProjectEnvState) (string, error) {
	if state.HasEffective {
		return state.EffectiveContent, nil
	}
	if state.HasGitSource || state.HasOverride {
		effectiveContent, err := projects.BuildEffectiveEnvContent(state.GitContent, state.OverrideContent)
		if err != nil {
			return "", fmt.Errorf("build effective env content: %w", err)
		}
		return effectiveContent, nil
	}
	return state.DirectContent, nil
}

// cleanupWouldMassWipeInternal reports whether the pending deletions look like an
// accidental mass wipe rather than legitimate removals. It engages when a single
// pass would prune more than one project AND more than half of the cleanup
// candidates — so the table cannot be near-emptied at once (e.g. when the projects
// directory is unmounted or mis-mapped and every path goes missing), no matter how
// few projects the deployment has. A single removal is always allowed: it is
// indistinguishable from a legitimate "deleted my only project" and is not a mass
// wipe. When the guard engages it logs a WARN pointing the operator at the likely
// volume/mount misconfiguration and the caller skips every deletion in the pass.
func CleanupWouldMassWipe(ctx context.Context, candidates, deleteCount int, projectsDir string) bool {
	if deleteCount <= 1 || deleteCount*2 <= candidates {
		return false
	}

	slog.WarnContext(ctx,
		"skipping project cleanup: this reconcile would delete most projects in a single pass, which usually "+
			"means the projects directory is empty, unmounted, or mis-mapped; preserving DB records — check the "+
			"projects volume is mounted and mapped correctly",
		"wouldDelete", deleteCount,
		"cleanupCandidates", candidates,
		"projectsDir", projectsDir,
	)
	return true
}

// EffectiveEnvContentForUpdate returns the env content a project update keeps
// when the caller supplied none.
func EffectiveEnvContentForUpdate(projectPath string, envContent *string) (*string, error) {
	if envContent != nil {
		return envContent, nil
	}

	state, err := projects.ReadProjectEnvState(projectPath)
	if err != nil {
		return nil, fmt.Errorf("read project env state: %w", err)
	}

	effectiveContent, err := ResolveStoredEffectiveEnvContent(state)
	if err != nil {
		return nil, err
	}
	if effectiveContent == "" && !state.HasEffective && !state.HasGitSource && !state.HasOverride {
		return nil, nil
	}

	return &effectiveContent, nil
}

// EnsureEffectiveEnvFile rebuilds .env from the managed env sources.
func EnsureEffectiveEnvFile(ctx context.Context, projectPath, projectsDirectory string) error {
	state, err := projects.ReadProjectEnvState(projectPath)
	if err != nil {
		return fmt.Errorf("read project env state: %w", err)
	}

	if !state.HasGitSource {
		if state.HasOverride {
			if removeProjectFileErr := projects.RemoveProjectFile(ctx, projectsDirectory, projectPath, projects.OverrideEnvFileName); removeProjectFileErr != nil {
				return removeProjectFileErr
			}
			effectiveContent, resolveStoredEffectiveEnvContentErr := ResolveStoredEffectiveEnvContent(state)
			if resolveStoredEffectiveEnvContentErr != nil {
				return resolveStoredEffectiveEnvContentErr
			}
			return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.EffectiveEnvFileName, state.EffectiveUnreadable, effectiveContent)
		}
		return projects.EnsureEnvFile(ctx, projectsDirectory, projectPath)
	}

	effectiveContent, err := projects.BuildEffectiveEnvContent(state.GitContent, state.OverrideContent)
	if err != nil {
		return fmt.Errorf("build effective env content: %w", err)
	}

	return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.EffectiveEnvFileName, state.EffectiveUnreadable, effectiveContent)
}

// PrepareGitSyncEnvUpdate resolves the env merge for incoming git env content.
func PrepareGitSyncEnvUpdate(projectPath string, gitEnvContent *string) (GitSyncEnvUpdate, error) {
	state, err := projects.ReadProjectEnvState(projectPath)
	if err != nil {
		return GitSyncEnvUpdate{}, fmt.Errorf("read project env state: %w", err)
	}

	update := GitSyncEnvUpdate{
		state:         state,
		gitEnvContent: gitEnvContent,
	}

	if gitEnvContent == nil {
		effectiveContent, resolveStoredEffectiveEnvContentErr := ResolveStoredEffectiveEnvContent(state)
		if resolveStoredEffectiveEnvContentErr != nil {
			return GitSyncEnvUpdate{}, resolveStoredEffectiveEnvContentErr
		}
		if effectiveContent == "" && !state.HasEffective && !state.HasGitSource && !state.HasOverride {
			return update, nil
		}
		update.effectiveContent = &effectiveContent
		return update, nil
	}

	overrideContent, err := resolveOverrideContentForGitSyncInternal(state, *gitEnvContent)
	if err != nil {
		return GitSyncEnvUpdate{}, err
	}
	update.overrideContent = overrideContent

	effectiveContent, err := projects.BuildEffectiveEnvContent(*gitEnvContent, overrideContent)
	if err != nil {
		return GitSyncEnvUpdate{}, fmt.Errorf("build effective env content: %w", err)
	}
	update.effectiveContent = &effectiveContent

	return update, nil
}

func resolveOverrideContentForGitSyncInternal(state projects.ProjectEnvState, gitEnvContent string) (string, error) {
	switch {
	case state.HasGitSource:
		overrideContent, err := projects.BuildOverrideEnvContent(state.GitContent, state.OverrideContent)
		if err != nil {
			return "", fmt.Errorf("build override env content: %w", err)
		}
		return overrideContent, nil
	case state.HasOverride:
		effectiveContent, err := ResolveStoredEffectiveEnvContent(state)
		if err != nil {
			return "", err
		}
		overrideContent, err := projects.BuildOverrideEnvContent(gitEnvContent, effectiveContent)
		if err != nil {
			return "", fmt.Errorf("build override env content: %w", err)
		}
		return overrideContent, nil
	case strings.TrimSpace(state.DirectContent) != "":
		overrideContent, err := projects.BuildAdditiveOverrideEnvContent(gitEnvContent, state.DirectContent)
		if err != nil {
			return "", fmt.Errorf("build override env content: %w", err)
		}
		return overrideContent, nil
	default:
		return "", nil
	}
}

// GitSyncEnvUpdate is the resolved three-file env change for one git sync.
type GitSyncEnvUpdate struct {
	state            projects.ProjectEnvState
	gitEnvContent    *string
	overrideContent  string
	effectiveContent *string
}

// PersistGitSyncEnvFiles writes a prepared env merge.
func PersistGitSyncEnvFiles(ctx context.Context, projectPath, projectsDirectory string, update GitSyncEnvUpdate) error {
	if update.gitEnvContent == nil {
		if update.state.HasGitSource {
			if err := projects.RemoveProjectFile(ctx, projectsDirectory, projectPath, projects.GitSourceEnvFileName); err != nil {
				return err
			}
		}
		if update.state.HasOverride {
			if err := projects.RemoveProjectFile(ctx, projectsDirectory, projectPath, projects.OverrideEnvFileName); err != nil {
				return err
			}
		}
		if update.effectiveContent != nil || update.state.HasEffective || update.state.HasGitSource || update.state.HasOverride {
			effectiveContent := ""
			if update.effectiveContent != nil {
				effectiveContent = *update.effectiveContent
			}
			return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.EffectiveEnvFileName, update.state.EffectiveUnreadable, effectiveContent)
		}
		if update.state.EffectiveUnreadable {
			slog.Warn("skipping permission-locked .env file; leaving it untouched", "projectPath", projectPath)
			return nil
		}
		return projects.EnsureEnvFile(ctx, projectsDirectory, projectPath)
	}

	if update.effectiveContent == nil {
		return errors.New("missing effective env content for git sync update")
	}

	if err := projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.EffectiveEnvFileName, update.state.EffectiveUnreadable, *update.effectiveContent); err != nil {
		return err
	}
	if err := projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.GitSourceEnvFileName, update.state.GitSourceUnreadable, *update.gitEnvContent); err != nil {
		return err
	}
	return projects.WriteManagedEnvFile(ctx, projectsDirectory, projectPath, projects.OverrideEnvFileName, update.state.OverrideUnreadable, update.overrideContent)
}

// EffectiveContent is the merged .env content, or nil when none is kept.
func (u GitSyncEnvUpdate) EffectiveContent() *string { return u.effectiveContent }

// HadGitSource reports whether the project tracked a git env source before.
func (u GitSyncEnvUpdate) HadGitSource() bool { return u.state.HasGitSource }

// ApplyGitSyncEnv applies the managed three-file environment merge and returns
// the effective content before and after the update.
func ApplyGitSyncEnv(ctx context.Context, projectPath, projectsDirectory string, gitEnvContent *string) (before, after string, err error) {
	update, err := PrepareGitSyncEnvUpdate(projectPath, gitEnvContent)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve git env state: %w", err)
	}
	before = update.state.DirectContent
	if update.effectiveContent != nil {
		after = *update.effectiveContent
	}
	if persistErr := PersistGitSyncEnvFiles(ctx, projectPath, projectsDirectory, update); persistErr != nil {
		return "", "", fmt.Errorf("failed to sync git env files: %w", persistErr)
	}
	return before, after, nil
}

// LoadComposeMetadata loads a discovered project's compose file once and
// returns the service count plus compose-go's effective project name.
func LoadComposeMetadata(ctx context.Context, dirPath, dirName, projectsDirectory string, autoInjectEnv bool, pathMapper *projects.PathMapper) (project.ComposeIdentity, error) {
	normName := projects.NormalizeProjectName(dirName)
	meta := project.ComposeIdentity{
		ResolvedProjectName: normName,
	}

	// First, try loading without forcing a project name so compose-go can
	// resolve COMPOSE_PROJECT_NAME from the .env file. If this fails (e.g.
	// no .env and directory name is not a valid compose project name), fall
	// back to the normalized directory name.
	proj, _, err := projects.LoadComposeProjectFromDir(ctx, dirPath, "", projectsDirectory, autoInjectEnv, pathMapper)
	if err != nil {
		proj, _, err = projects.LoadComposeProjectFromDir(ctx, dirPath, normName, projectsDirectory, autoInjectEnv, pathMapper)
		if err != nil {
			return meta, err
		}
	} else if proj.Name != "" && proj.Name != normName {
		meta.ExplicitProjectName = true
	}

	meta.ServiceCount = len(proj.Services)
	if proj.Name != "" {
		meta.ResolvedProjectName = proj.Name
	}

	// If compose-go resolved a different name (from COMPOSE_PROJECT_NAME),
	// store it so we can match containers correctly.
	if proj.Name != "" && proj.Name != normName {
		meta.ComposeProjectName = new(proj.Name)
	}

	return meta, nil
}
