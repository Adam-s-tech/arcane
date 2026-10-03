package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/base"
	"github.com/getarcaneapp/arcane/types/v2/gitops"
	"go.getarcane.app/kit/pkg"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/common"
	"github.com/getarcaneapp/arcane/backend/v2/internal/project"
)

// marshalSyncedFiles converts a list of file paths to JSON for storage
func MarshalSyncedFiles(files []string) *string {
	if len(files) == 0 {
		return nil
	}
	data, err := json.Marshal(files)
	if err != nil {
		return nil
	}
	return new(string(data))
}

// parseSyncedFiles parses the JSON array of synced file paths from the database
func ParseSyncedFiles(syncedFilesJSON *string) []string {
	if syncedFilesJSON == nil || *syncedFilesJSON == "" {
		return nil
	}
	var files []string
	if err := json.Unmarshal([]byte(*syncedFilesJSON), &files); err != nil {
		return nil
	}
	return files
}

func isUniqueViolation(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate key")
}

func isGitOpsSyncOverdue(sync *project.GitOpsSync) bool {
	if sync.LastSyncAt == nil {
		return true
	}
	interval := max(sync.SyncInterval, 1)
	return time.Now().After(sync.LastSyncAt.Add(time.Duration(interval) * time.Minute))
}

func megabytesToBytes(value int) int64 {
	return int64(value) * 1024 * 1024
}

func normalizeSyncLimitSetting(value, defaultValue int) int {
	return kit.Ternary(value < 0, defaultValue, value)
}

func nullableUpdateStringValue(p *string) any {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*p)
	return kit.Ternary[any](trimmed == "", nil, trimmed)
}

func nullableTrimmedString(p *string) *string {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*p)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// normalizeLifecycleNetworkMode returns the canonical network mode
// string for a user-supplied value. Empty / whitespace / unset all map to
// "none" so the secure-by-default behaviour is restored when the user clears
// the field.
func normalizeLifecycleNetworkMode(p *string) string {
	if p == nil {
		return "none"
	}
	trimmed := strings.TrimSpace(*p)
	return kit.Ternary(trimmed == "", "none", trimmed)
}

// resolveEffectiveSyncDirectory mirrors the string resolver but for
// the SyncDirectory bool: a nil update keeps the existing value, a non-nil
// update overrides it. On create (current=nil) and no update, defaults to
// false to match the model's create-time default.
func resolveEffectiveSyncDirectory(current *project.GitOpsSync, update *bool) bool {
	if update != nil {
		return *update
	}
	if current != nil {
		return current.SyncDirectory
	}
	return false
}

func resolveLifecycleEffectiveTargetType(current *project.GitOpsSync, update *string) string {
	if update != nil && strings.TrimSpace(*update) != "" {
		return strings.TrimSpace(*update)
	}
	if current != nil && strings.TrimSpace(current.TargetType) != "" {
		return strings.TrimSpace(current.TargetType)
	}
	return "project"
}

func currentString(sync *project.GitOpsSync, accessor func(*project.GitOpsSync) *string) string {
	if sync == nil {
		return ""
	}
	if p := accessor(sync); p != nil {
		return *p
	}
	return ""
}

// resolveLifecycleEffectiveString computes the value a string field
// will take after an update: a nil update leaves the existing value, a
// non-nil update (including empty string, which clears) overrides it. Used
// to validate the post-update state of a sync record.
func resolveLifecycleEffectiveString(existing string, update *string) string {
	if update != nil {
		return strings.TrimSpace(*update)
	}
	return strings.TrimSpace(existing)
}

func validateSyncLimits(maxFiles *int, maxTotalSize, maxBinarySize *int64) error {
	if maxFiles != nil && *maxFiles < 0 {
		return errors.New("maxSyncFiles must be non-negative")
	}
	if maxTotalSize != nil && *maxTotalSize < 0 {
		return errors.New("maxSyncTotalSize must be non-negative")
	}
	if maxBinarySize != nil && *maxBinarySize < 0 {
		return errors.New("maxSyncBinarySize must be non-negative")
	}
	return nil
}

func normalizeSyncMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", gitops.SyncModeDeploy:
		return gitops.SyncModeDeploy, nil
	case gitops.SyncModeBackup:
		return gitops.SyncModeBackup, nil
	default:
		return "", common.Classify(common.ErrValidation, &base.FieldError{Field: "mode", Err: fmt.Errorf("unsupported sync mode %q", mode)})
	}
}
