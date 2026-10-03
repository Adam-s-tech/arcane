package policies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	activitytypes "github.com/getarcaneapp/arcane/types/v2/activity"
	backuptypes "github.com/getarcaneapp/arcane/types/v2/backup"
	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	"github.com/getarcaneapp/arcane/types/v2/user"
	"github.com/getarcaneapp/arcane/types/v2/volume"
	"go.getarcane.app/kit/pkg"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/activity"
	"github.com/getarcaneapp/arcane/backend/v2/internal/backup"
	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/s3"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	activitylib "github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/activity"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/entityjobs"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/jobcontext"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
)

const defaultSchedule = "0 0 2 * * *"

// Dependencies are the backup operations scheduled policies run through.
type Dependencies struct {
	DB             *database.DB
	S3Destinations *s3.S3DestinationService
	Settings       *settings.SettingsService
	Activity       *activity.ActivityService
	AlreadyRunning error
	LatestRun      func(ctx context.Context, policyID string) (*volume.BackupEntry, error)
	CreateBackup   func(ctx context.Context, volumeName, policyID string) (*volume.Backup, error)
	Reconcile      func(ctx context.Context, previous scheduler.Run, volumeName string) (scheduler.Outcome, error)
}

// Service owns per-volume backup policies and their scheduled jobs.
type Service struct {
	deps Dependencies
	jobs *entityjobs.Registry
}

func NewService(deps Dependencies) *Service {
	return &Service{deps: deps, jobs: entityjobs.New("volume-backup:", backup.VolumeAdmissionScope)}
}

// SetScheduler injects the dynamic scheduler and admission gate for per-policy
// backup jobs. Agent mode passes them too: agents run their own volume backups.
func (s *Service) SetScheduler(ctx context.Context, dynamicScheduler scheduler.DynamicScheduler, admissionGate *runs.Admission) error {
	return s.jobs.SetScheduler(ctx, dynamicScheduler, admissionGate)
}

func (s *Service) policiesInternal(ctx context.Context, volumeName string) ([]VolumeBackupPolicy, error) {
	var policies []VolumeBackupPolicy
	if err := s.deps.DB.WithContext(ctx).Where("volume_name = ?", volumeName).Order("created_at ASC").Find(&policies).Error; err != nil {
		return nil, fmt.Errorf("failed to load volume backup policies: %w", err)
	}
	return policies,
		nil
}

// Policy loads one policy of the volume; an empty or unknown ID returns nil.
func (s *Service) Policy(ctx context.Context, volumeName, policyID string) (*VolumeBackupPolicy, error) {
	if strings.TrimSpace(policyID) == "" {
		return nil, nil
	}
	var policy VolumeBackupPolicy
	err := s.deps.DB.WithContext(ctx).Where("id = ? AND volume_name = ?", policyID, volumeName).First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load volume backup policy: %w", err)
	}
	return &policy, nil
}

func (s *Service) GetBackupPolicies(ctx context.Context, volumeName string) (*volume.BackupPolicyCollection, error) {
	policies, err := s.policiesInternal(ctx, volumeName)
	if err != nil {
		return nil, err
	}
	result := &volume.BackupPolicyCollection{Policies: make([]volume.BackupPolicy, 0, len(policies))}
	destinations := make(map[string]backuptypes.S3Destination)
	if s.deps.S3Destinations != nil {
		available, listErr := s.deps.S3Destinations.ListS3DestinationsByID(ctx)
		if listErr == nil {
			result.S3Available = len(available) > 0
			destinations = available
		}
	}
	for i := range policies {
		lastRun, runErr := s.deps.LatestRun(ctx, policies[i].ID)
		if runErr != nil {
			return nil, fmt.Errorf("failed to load latest volume backup: %w", runErr)
		}
		if lastRun != nil {
			if destination, ok := destinations[lastRun.S3DestinationID]; ok {
				lastRun.S3DestinationName = destination.Name
			}
		}
		dto := policies[i].ToDTO(lastRun)
		dto.S3Available = result.S3Available
		if destination, ok := destinations[policies[i].S3DestinationID]; ok {
			dto.S3Bucket = destination.Bucket
			dto.S3DestinationName = destination.Name
		}
		result.Policies = append(result.Policies, dto)
	}
	return result, nil
}

func (s *Service) UpdateBackupPolicies(ctx context.Context, volumeName string, updates []volume.UpdateBackupPolicy) (*volume.BackupPolicyCollection, error) {
	existing, err := s.policiesInternal(ctx, volumeName)
	if err != nil {
		return nil, err
	}
	reconcile := backup.PolicyReconciliation[VolumeBackupPolicy, volume.UpdateBackupPolicy]{
		Domain:   "volume",
		DB:       s.deps.DB,
		Existing: existing,
		ID:       func(policy *VolumeBackupPolicy) string { return policy.ID },
		UpdateID: func(update volume.UpdateBackupPolicy) string { return update.ID },
		New:      func() VolumeBackupPolicy { return VolumeBackupPolicy{VolumeName: volumeName} },
		Build: func(ctx context.Context, policy *VolumeBackupPolicy, update volume.UpdateBackupPolicy) error {
			normalized, validatePolicyUpdateErr := backup.ValidatePolicyUpdate(ctx, "volume", update, s.deps.S3Destinations)
			if validatePolicyUpdateErr != nil {
				return validatePolicyUpdateErr
			}
			update = normalized
			policy.Enabled, policy.Schedule, policy.RetentionCount = update.Enabled, update.Schedule, update.RetentionCount
			policy.StopContainers, policy.LocalEnabled, policy.S3Enabled = update.StopContainers, update.LocalEnabled, update.S3Enabled
			policy.S3DestinationID = update.S3DestinationID
			return nil
		},
		Unregister: s.jobs.Unregister,
		Reschedule: s.rescheduleInternal,
	}
	if runErr := reconcile.Run(ctx, updates); runErr != nil {
		return nil, runErr
	}
	return s.GetBackupPolicies(ctx, volumeName)
}

// HasEnabledBackupPolicy reports whether a volume-level schedule takes precedence over centralized backups.
func (s *Service) HasEnabledBackupPolicy(ctx context.Context, volumeName string) (bool, error) {
	var count int64
	if err := s.deps.DB.WithContext(ctx).Model(&VolumeBackupPolicy{}).
		Where("volume_name = ? AND enabled = ?", volumeName, true).Count(&count).Error; err != nil {
		return false, fmt.Errorf("failed to load volume backup policy override: %w", err)
	}
	return count > 0, nil
}

func (s *Service) runScheduledBackupInternal(ctx context.Context, policyID string) (scheduler.Outcome, error) {
	var policy VolumeBackupPolicy
	if err := s.deps.DB.WithContext(ctx).Where("id = ? AND enabled = ?", policyID, true).First(&policy).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return scheduler.Outcome{Status: scheduler.Canceled, Message: "Backup policy disabled or deleted"}, nil
		}
		return scheduler.Outcome{}, err
	}
	if previous, ok := jobcontext.Run(ctx); ok {
		outcome := jobcontext.ConfirmedTarget(previous, policy.VolumeName)
		if outcome.Status == scheduler.Succeeded {
			return outcome, nil
		}
	}
	remoteDisabled, checkErr := s.disableMissingS3Internal(ctx, &policy)
	if checkErr != nil {
		return scheduler.Outcome{}, checkErr
	}
	if remoteDisabled && !policy.LocalEnabled {
		return scheduler.Outcome{Status: scheduler.NeedsAttention, Message: backup.RemoteDisabledMessage}, nil
	}
	var entry *volume.Backup
	activityID, err := activitylib.RunHandlerActivity(ctx, s.deps.Activity, activitylib.HandlerOptions{
		EnvironmentID:  "0",
		Type:           activitytypes.TypeResourceAction,
		ResourceType:   "volume_backup",
		ResourceID:     policy.VolumeName,
		ResourceName:   policy.VolumeName,
		User:           &user.SystemUser,
		Step:           "Creating scheduled backup",
		Message:        "Creating scheduled volume backup",
		SuccessMessage: "Scheduled volume backup created successfully",
		Metadata: database.JSON{
			"action":          "scheduled_volume_backup",
			"policyId":        policy.ID,
			"schedule":        policy.Schedule,
			"volumeName":      policy.VolumeName,
			"retentionCount":  policy.RetentionCount,
			"stopContainers":  policy.StopContainers,
			"localEnabled":    policy.LocalEnabled,
			"s3Enabled":       policy.S3Enabled,
			"s3DestinationId": policy.S3DestinationID,
		},
	}, func(activityCtx context.Context) error {
		var backupErr error
		entry, backupErr = s.deps.CreateBackup(activityCtx, policy.VolumeName, policy.ID)
		return backupErr
	})
	if errors.Is(err, s.deps.AlreadyRunning) {
		slog.InfoContext(ctx, "Scheduled volume backup skipped; another backup is running", "volume", policy.VolumeName)
		return scheduler.Outcome{Status: scheduler.Skipped}, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "Scheduled volume backup failed", "volume", policy.VolumeName, "error", err)
		return scheduler.Outcome{}, err
	}
	slog.InfoContext(ctx, "Scheduled volume backup completed", "volume", policy.VolumeName, "backup_id", entry.ID, "remote_snapshot_id", entry.RemoteSnapshotID)
	if remoteDisabled {
		return scheduler.Outcome{Status: scheduler.Partial, ActivityID: activityID, Message: backup.RemoteDisabledMessage}, nil
	}
	return scheduler.Outcome{Status: scheduler.Succeeded, ActivityID: activityID}, nil
}

func (s *Service) rescheduleInternal(ctx context.Context, policy *VolumeBackupPolicy) {
	if policy == nil {
		return
	}
	if !policy.Enabled {
		s.jobs.Unregister(ctx, policy.ID)
		return
	}
	policyID := policy.ID
	s.jobs.Register(ctx, policyID,
		func(ctx context.Context) string {
			var current VolumeBackupPolicy
			if err := s.deps.DB.WithContext(ctx).Where("id = ?", policyID).First(&current).Error; err != nil {
				return defaultSchedule
			}
			return current.Schedule
		},
		func(ctx context.Context) (scheduler.Outcome, error) {
			return s.runScheduledBackupInternal(ctx, policyID)
		},
		func(ctx context.Context, previous scheduler.Run) (scheduler.Outcome, error) {
			return s.deps.Reconcile(ctx, previous, policy.VolumeName)
		},
	)
}

func (s *Service) RegisterJobsOnStartup(ctx context.Context) {
	if !s.jobs.Enabled() {
		return
	}
	var policies []VolumeBackupPolicy
	if err := s.deps.DB.WithContext(ctx).Where("enabled = ?", true).Find(&policies).Error; err != nil {
		slog.ErrorContext(ctx, "Failed to load scheduled volume backups", "error", err)
		return
	}
	for i := range policies {
		s.rescheduleInternal(ctx, &policies[i])
	}
	slog.InfoContext(ctx, "Registered scheduled volume backup jobs", "count", len(policies))
}

// Remove unregisters and deletes every policy of a removed volume.
func (s *Service) Remove(ctx context.Context, volumeName string) {
	policies, err := s.policiesInternal(ctx, volumeName)
	if err != nil {
		return
	}
	for i := range policies {
		s.jobs.Unregister(ctx, policies[i].ID)
	}
	if deleteBackupPolicyErr := s.deps.DB.WithContext(ctx).Where("volume_name = ?", volumeName).Delete(&VolumeBackupPolicy{}).Error; deleteBackupPolicyErr != nil {
		slog.WarnContext(ctx, "Failed to delete volume backup policy", "volume", volumeName, "error", deleteBackupPolicyErr)
	}
}

// Rename moves a renamed volume's policies inside the caller's transaction.
func (s *Service) Rename(tx *gorm.DB, oldName, newName string) error {
	return tx.Model(&VolumeBackupPolicy{}).Where("volume_name = ?", oldName).Update("volume_name", newName).Error
}

func (s *Service) disableMissingS3Internal(ctx context.Context, policy *VolumeBackupPolicy) (bool, error) {
	if !policy.S3Enabled {
		return false, nil
	}
	err := backup.CheckScheduledRemote(ctx, s.deps.DB, s.deps.S3Destinations, "volume_backups", policy.S3DestinationID, "arcane-volume-backups/"+s.deps.Settings.GetSettingsConfig().InstanceID.Value)
	if !errors.Is(err, backup.ErrRemoteRepositoryMissing) {
		return false, nil
	}
	column := kit.Ternary(policy.LocalEnabled, "s3_enabled", "enabled")
	result := s.deps.DB.WithContext(ctx).Model(&VolumeBackupPolicy{}).
		Where("id = ? AND s3_destination_id = ? AND enabled = ? AND s3_enabled = ? AND local_enabled = ?", policy.ID, policy.S3DestinationID, true, true, policy.LocalEnabled).
		Update(column, false)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	if policy.LocalEnabled {
		policy.S3Enabled = false
	} else {
		policy.Enabled = false
	}
	s.rescheduleInternal(ctx, policy)
	return true, nil
}
