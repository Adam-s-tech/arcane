package volumes

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	activitytypes "github.com/getarcaneapp/arcane/types/v2/activity"
	backuptypes "github.com/getarcaneapp/arcane/types/v2/backup"
	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	usertypes "github.com/getarcaneapp/arcane/types/v2/user"
	"go.getarcane.app/kit/pkg"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/activity"
	"github.com/getarcaneapp/arcane/backend/v2/internal/backup"
	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/internal/s3"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/internal/volume"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
	activitylib "github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/activity"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/entityjobs"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/jobcontext"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils"
)

// Dependencies are the services and system admission hooks volume backups run on.
type Dependencies struct {
	DB                *database.DB
	Engine            *backup.Engine
	Volumes           *volume.VolumeService
	S3Destinations    *s3.S3DestinationService
	Activity          *activity.ActivityService
	Settings          *settings.SettingsService
	Jobs              *entityjobs.Registry
	AcquireRun        func(ctx context.Context) (*runs.Lease, error)
	AcquireDurableRun func(ctx context.Context, runID string) (*runs.Lease, error)
	AlreadyRunning    error
}

// Service owns system-managed volume backup policies and their runs.
type Service struct {
	db                *database.DB
	engine            *backup.Engine
	volumes           *volume.VolumeService
	s3Destinations    *s3.S3DestinationService
	activity          *activity.ActivityService
	settings          *settings.SettingsService
	jobs              *entityjobs.Registry
	acquireRun        func(ctx context.Context) (*runs.Lease, error)
	acquireDurableRun func(ctx context.Context, runID string) (*runs.Lease, error)
	alreadyRunning    error
}

func NewService(deps Dependencies) *Service {
	return &Service{
		db:                deps.DB,
		engine:            deps.Engine,
		volumes:           deps.Volumes,
		s3Destinations:    deps.S3Destinations,
		activity:          deps.Activity,
		settings:          deps.Settings,
		jobs:              deps.Jobs,
		acquireRun:        deps.AcquireRun,
		acquireDurableRun: deps.AcquireDurableRun,
		alreadyRunning:    deps.AlreadyRunning,
	}
}

func (s *Service) loadSystemVolumeBackupPoliciesInternal() (*backuptypes.SystemVolumeBackupPolicyCollection, error) {
	collection := &backuptypes.SystemVolumeBackupPolicyCollection{Policies: []backuptypes.SystemVolumeBackupPolicy{}}
	if s.settings == nil {
		return collection, nil
	}
	raw := strings.TrimSpace(s.settings.GetSettingsConfig().SystemVolumeBackupConfig.Value)
	if raw == "" {
		return collection, nil
	}
	if err := json.Unmarshal([]byte(raw), collection); err != nil {
		return nil, fmt.Errorf("decode system-managed volume backup policies: %w", err)
	}
	if collection.Policies == nil {
		collection.Policies = []backuptypes.SystemVolumeBackupPolicy{}
	}
	return collection, nil
}

func (s *Service) systemVolumeBackupPolicyInternal(policyID string) (*backuptypes.SystemVolumeBackupPolicy, error) {
	collection, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		return nil, err
	}
	for i := range collection.Policies {
		if collection.Policies[i].ID == policyID {
			policy := collection.Policies[i]
			return &policy, nil
		}
	}
	return nil, nil
}

func (s *Service) GetConfig(ctx context.Context) (*backuptypes.SystemVolumeBackupPolicyCollection, error) {
	collection, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		return nil, err
	}
	destinations := make(map[string]backuptypes.S3Destination)
	if s.s3Destinations != nil {
		if available, listErr := s.s3Destinations.ListS3DestinationsByID(ctx); listErr == nil {
			destinations = available
		}
	}
	for i := range collection.Policies {
		policy := &collection.Policies[i]
		policy.S3DestinationName = destinations[policy.S3DestinationID].Name
		if s.db == nil {
			continue
		}
		var lastRun volume.VolumeBackup
		runErr := s.db.WithContext(ctx).
			Where("policy_id LIKE ?", backuptypes.SystemVolumePolicyPrefix+policy.ID+":%").
			Order("created_at DESC").First(&lastRun).Error
		if runErr == nil {
			policy.LastRun = &backuptypes.SystemBackupRun{
				ID: lastRun.ID, Size: lastRun.Size, CreatedAt: lastRun.CreatedAt, Status: string(lastRun.Status),
				Trigger: string(lastRun.Trigger), Destination: backuptypes.SystemBackupDestination(lastRun.Destination),
				LocalSnapshotID: lastRun.LocalSnapshotID, RemoteSnapshotID: lastRun.RemoteSnapshotID,
				S3DestinationID: lastRun.S3DestinationID, S3DestinationName: destinations[lastRun.S3DestinationID].Name,
				PolicyID: lastRun.PolicyID, Error: lastRun.Error,
			}
		} else if !errors.Is(runErr, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("load latest system-managed volume backup: %w", runErr)
		}
	}
	return collection, nil
}

func (s *Service) normalizeSystemVolumePolicyUpdateInternal(ctx context.Context, input backuptypes.UpdateSystemVolumeBackupPolicy) (backuptypes.SystemVolumeBackupPolicy, error) {
	if err := validateSystemVolumeSelectionInternal(input.SelectionMode); err != nil {
		return backuptypes.SystemVolumeBackupPolicy{}, err
	}
	update, err := backup.ValidatePolicyUpdate(ctx, "system-managed volume", input.UpdateBackupPolicy, s.s3Destinations)
	if err != nil {
		return backuptypes.SystemVolumeBackupPolicy{}, err
	}
	names := kit.Unique(kit.TrimNonEmpty(input.VolumeNames))
	slices.Sort(names)
	if input.SelectionMode == backuptypes.SystemVolumeSelectionAll || names == nil {
		names = []string{}
	}
	return backuptypes.SystemVolumeBackupPolicy{
		ID: input.ID, Enabled: update.Enabled, Schedule: update.Schedule, RetentionCount: update.RetentionCount,
		StopContainers: update.StopContainers, LocalEnabled: update.LocalEnabled, S3Enabled: update.S3Enabled,
		S3DestinationID: update.S3DestinationID, SelectionMode: input.SelectionMode,
		VolumeNames: names, IgnoreAnonymous: input.IgnoreAnonymous,
	}, nil
}

func (s *Service) UpdateConfig(ctx context.Context, updates []backuptypes.UpdateSystemVolumeBackupPolicy) (*backuptypes.SystemVolumeBackupPolicyCollection, error) {
	if s.settings == nil {
		return nil, errors.New("settings service is unavailable")
	}
	existing, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		return nil, err
	}
	reconcile := backup.PolicyReconciliation[backuptypes.SystemVolumeBackupPolicy, backuptypes.UpdateSystemVolumeBackupPolicy]{
		Domain:   "system-managed volume",
		Existing: existing.Policies,
		ID:       func(policy *backuptypes.SystemVolumeBackupPolicy) string { return policy.ID },
		UpdateID: func(update backuptypes.UpdateSystemVolumeBackupPolicy) string { return update.ID },
		New: func() backuptypes.SystemVolumeBackupPolicy {
			return backuptypes.SystemVolumeBackupPolicy{ID: uuid.New().String()}
		},
		Build: func(ctx context.Context, policy *backuptypes.SystemVolumeBackupPolicy, update backuptypes.UpdateSystemVolumeBackupPolicy) error {
			normalized, normalizeErr := s.normalizeSystemVolumePolicyUpdateInternal(ctx, update)
			if normalizeErr != nil {
				return normalizeErr
			}
			normalized.ID = policy.ID
			*policy = normalized
			return nil
		},
		Persist: s.saveSystemVolumeBackupPoliciesInternal,
		Unregister: func(ctx context.Context, policyID string) {
			s.jobs.Unregister(ctx, systemVolumeBackupJobPrefix+policyID)
		},
		Reschedule: s.rescheduleSystemVolumeBackupInternal,
	}
	if runErr := reconcile.Run(ctx, updates); runErr != nil {
		return nil, runErr
	}
	return s.GetConfig(ctx)
}

// ListOptions returns live choices plus configured names that are currently unavailable.
func (s *Service) ListOptions(ctx context.Context) ([]backuptypes.SystemVolumeBackupOption, error) {
	if s.volumes == nil {
		return nil, errors.New("volume service is unavailable")
	}
	options, err := s.volumes.ListBackupVolumeOptions(ctx)
	if err != nil {
		return nil, err
	}
	collection, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(options))
	for _, option := range options {
		known[option.Name] = struct{}{}
	}
	for _, policy := range collection.Policies {
		for _, name := range policy.VolumeNames {
			if _, ok := known[name]; !ok {
				options = append(options, backuptypes.SystemVolumeBackupOption{Name: name})
				known[name] = struct{}{}
			}
		}
	}
	slices.SortFunc(options, func(a, b backuptypes.SystemVolumeBackupOption) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return options, nil
}

func (s *Service) resolveSystemVolumeRunPolicyInternal(ctx context.Context, request backuptypes.RunSystemVolumeBackupsRequest) (backuptypes.SystemVolumeBackupPolicy, bool, error) {
	if request.PolicyID != "" {
		if request.Custom != nil {
			return backuptypes.SystemVolumeBackupPolicy{}, false, errors.New("select a saved policy or custom configuration, not both")
		}
		policy, err := s.systemVolumeBackupPolicyInternal(request.PolicyID)
		if err != nil {
			return backuptypes.SystemVolumeBackupPolicy{}, false, err
		}
		if policy == nil {
			return backuptypes.SystemVolumeBackupPolicy{}, false, errors.New("system-managed volume backup policy not found")
		}
		return *policy, false, nil
	}
	custom := request.Custom
	if custom != nil &&
		custom.Destination != backuptypes.SystemBackupDestinationLocal &&
		custom.Destination != backuptypes.SystemBackupDestinationS3 &&
		custom.Destination != backuptypes.SystemBackupDestinationLocalS3 {
		return backuptypes.SystemVolumeBackupPolicy{}, false, errors.New("destination must be local, s3, or local_s3")
	}
	policy, err := s.normalizeSystemVolumePolicyUpdateInternal(ctx, customSystemVolumePolicyInternal(custom))
	return policy, true, err
}

type preparedSystemVolumeBackupInternal struct {
	policy       backuptypes.SystemVolumeBackupPolicy
	manualPolicy bool
	candidates   []backuptypes.SystemVolumeBackupOption
	lease        *runs.Lease
}

func (
	s *Service,
) runSystemVolumeBackupsInternal(
	ctx context.Context,
	request backuptypes.RunSystemVolumeBackupsRequest,
	trigger volume.VolumeBackupTrigger,
) (
	*backuptypes.SystemVolumeBackupRunResult,
	error,
) {
	prepared, err := s.prepareSystemVolumeBackupsInternal(ctx, request)
	if err != nil {
		return nil, err
	}
	defer prepared.lease.Release(ctx)
	frozen, err := json.Marshal(manualSystemVolumesInternal{Policy: prepared.policy, ManualPolicy: prepared.manualPolicy, Candidates: prepared.candidates})
	if err != nil {
		return nil, err
	}
	if progressErr := jobcontext.Progress(
		ctx,
		scheduler.TargetOutcome{
			ResourceType: "backup_plan",
			ID:           "system-volume-plan",
			Status:       scheduler.Succeeded,
			RecoveryData: frozen,
		},
	); progressErr != nil {
		return nil, progressErr
	}
	return s.executeSystemVolumeBackupsInternal(ctx, prepared.policy, prepared.manualPolicy, prepared.candidates, trigger, "")
}

func (s *Service) prepareSystemVolumeBackupsInternal(ctx context.Context, request backuptypes.RunSystemVolumeBackupsRequest) (*preparedSystemVolumeBackupInternal, error) {
	if s.volumes == nil {
		return nil, errors.New("volume service is unavailable")
	}
	policyConfig, manualPolicy, err := s.resolveSystemVolumeRunPolicyInternal(ctx, request)
	if err != nil {
		return nil, err
	}
	lease, err := s.acquireRun(ctx)
	if err != nil {
		return nil, err
	}
	options, err := s.volumes.ListBackupVolumeOptions(ctx)
	if err != nil {
		lease.Release(ctx)
		return nil, err
	}
	candidates := selectSystemVolumeBackupCandidatesInternal(policyConfig, options)
	return &preparedSystemVolumeBackupInternal{policy: policyConfig, manualPolicy: manualPolicy, candidates: candidates, lease: lease}, nil
}

func (
	s *Service,
) executeSystemVolumeBackupsInternal(
	ctx context.Context,
	policyConfig backuptypes.SystemVolumeBackupPolicy,
	manualPolicy bool,
	candidates []backuptypes.SystemVolumeBackupOption,
	trigger volume.VolumeBackupTrigger,
	activityID string,
) (
	result *backuptypes.SystemVolumeBackupRunResult,
	err error,
) {
	result = &backuptypes.SystemVolumeBackupRunResult{
		Matched: len(candidates), Failures: make([]backuptypes.SystemVolumeBackupFailure, 0),
	}
	defer s.updateSystemVolumeProgressInternal(context.WithoutCancel(ctx), activityID, policyConfig.ID, candidates, result)
	defer utils.RecoverToError(&err, "system-managed volume backup")

	policy := backuptypes.UpdateBackupPolicy{
		Enabled: true, Schedule: policyConfig.Schedule, RetentionCount: policyConfig.RetentionCount,
		StopContainers: policyConfig.StopContainers, LocalEnabled: policyConfig.LocalEnabled, S3Enabled: policyConfig.S3Enabled,
		S3DestinationID: policyConfig.S3DestinationID,
	}
	for _, candidate := range candidates {
		status := completedVolumeBackupStatusInternal(ctx, candidate.Name)
		if status == scheduler.Succeeded {
			result.Succeeded++
			continue
		}
		if status == scheduler.Skipped {
			result.Skipped++
			continue
		}
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return result, cancellationErr
		}
		s.updateSystemVolumeProgressInternal(ctx, activityID, policyConfig.ID, candidates, result)
		overridden, policyErr := s.volumes.HasEnabledBackupPolicy(ctx, candidate.Name)
		if policyErr != nil {
			result.Failed++
			result.Failures = append(result.Failures, backuptypes.SystemVolumeBackupFailure{VolumeName: candidate.Name, Error: policyErr.Error()})
			continue
		}
		if overridden {
			if skipProgressErr := jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: candidate.Name, Status: scheduler.Skipped}); skipProgressErr != nil {
				return result, skipProgressErr
			}
			result.Skipped++
			continue
		}
		seriesID := systemVolumePolicyIDInternal(policyConfig.ID, candidate.Name)
		if manualPolicy {
			seriesID = systemVolumeManualPolicyIDInternal(candidate.Name)
		}
		_, backupErr := s.volumes.CreateSystemManagedBackup(ctx, candidate.Name, usertypes.SystemUser, trigger, seriesID, policy)
		if errors.Is(backupErr, volume.ErrVolumeBackupAlreadyRunning) {
			result.Skipped++
			continue
		}
		if backupErr != nil {
			result.Failed++
			result.Failures = append(result.Failures, backuptypes.SystemVolumeBackupFailure{VolumeName: candidate.Name, Error: backupErr.Error()})
			continue
		}
		if progressErr := jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: candidate.Name, Status: scheduler.Succeeded}); progressErr != nil {
			return result, progressErr
		}
		result.Succeeded++
	}
	return result, nil
}

func (s *Service) runScheduledSystemVolumeBackupInternal(ctx context.Context, policyID string) (scheduler.Outcome, error) {
	policy, err := s.systemVolumeBackupPolicyInternal(policyID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if policy == nil || !policy.Enabled {
		return scheduler.Outcome{Status: scheduler.Skipped}, nil
	}
	remoteDisabled, checkErr := s.disableMissingVolumeS3Internal(ctx, policy)
	if checkErr != nil {
		return scheduler.Outcome{}, checkErr
	}
	if remoteDisabled && !policy.LocalEnabled {
		return scheduler.Outcome{Status: scheduler.NeedsAttention, Message: backup.RemoteDisabledMessage}, nil
	}
	result, err := s.runSystemVolumeBackupsInternal(ctx, backuptypes.RunSystemVolumeBackupsRequest{PolicyID: policyID}, volume.VolumeBackupTriggerScheduled)
	if errors.Is(err, s.alreadyRunning) {
		slog.InfoContext(ctx, "Scheduled system-managed volume backups skipped; a system backup is running", "policyId", policyID)
		return scheduler.Outcome{Status: scheduler.Skipped}, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "Scheduled system-managed volume backups failed", "policyId", policyID, "error", err)
		return scheduler.Outcome{}, err
	}
	slog.InfoContext(
		ctx,
		"Scheduled system-managed volume backups completed",
		"policyId",
		policyID,
		"matched",
		result.Matched,
		"succeeded",
		result.Succeeded,
		"failed",
		result.Failed,
		"skipped",
		result.Skipped,
	)
	outcome := scheduler.Outcome{Status: scheduler.Succeeded}
	if remoteDisabled {
		outcome.Message = backup.RemoteDisabledMessage
	}
	if result.Failed > 0 || remoteDisabled {
		outcome.Status = scheduler.Partial
	}
	for _, failure := range result.Failures {
		outcome.Targets = append(outcome.Targets, scheduler.TargetOutcome{ID: failure.VolumeName, Status: scheduler.Failed, Message: failure.Error})
	}
	return outcome, nil
}

func (s *Service) rescheduleSystemVolumeBackupInternal(ctx context.Context, policy *backuptypes.SystemVolumeBackupPolicy) {
	if policy == nil {
		return
	}
	jobID := systemVolumeBackupJobPrefix + policy.ID
	if !policy.Enabled {
		s.jobs.Unregister(ctx, jobID)
		return
	}
	policyID := policy.ID
	s.jobs.Register(ctx, jobID, func(context.Context) string {
		current, err := s.systemVolumeBackupPolicyInternal(policyID)
		if err != nil || current == nil {
			return defaultSystemVolumeSchedule
		}
		return current.Schedule
	}, func(ctx context.Context) (scheduler.Outcome, error) {
		return s.runScheduledSystemVolumeBackupInternal(ctx, policyID)
	}, func(ctx context.Context, previous scheduler.Run) (scheduler.Outcome, error) {
		for _, target := range previous.Outcome.Targets {
			if target.ID != "system-volume-plan" || len(target.RecoveryData) == 0 {
				continue
			}
			if err := s.ExecuteDurable(ctx, previous.ID, target.RecoveryData, true); err != nil {
				return scheduler.Outcome{Status: scheduler.NeedsAttention, Message: err.Error()}, err
			}
			return scheduler.Outcome{Status: scheduler.Succeeded}, nil
		}
		return scheduler.Outcome{Status: scheduler.NeedsAttention, Message: "The interrupted volume backup has no frozen selection"}, nil
	})
}

func (s *Service) saveSystemVolumeBackupPoliciesInternal(ctx context.Context, policies []backuptypes.SystemVolumeBackupPolicy) error {
	encoded, err := json.Marshal(backuptypes.SystemVolumeBackupPolicyCollection{Policies: policies})
	if err != nil {
		return fmt.Errorf("encode policies: %w", err)
	}
	return s.settings.UpdateSetting(ctx, systemVolumeBackupConfigKey, string(encoded))
}

func (s *Service) disableMissingVolumeS3Internal(ctx context.Context, policy *backuptypes.SystemVolumeBackupPolicy) (bool, error) {
	if !policy.S3Enabled {
		return false, nil
	}
	root := "arcane-volume-backups/" + s.settings.GetSettingsConfig().InstanceID.Value
	err := backup.CheckScheduledRemote(ctx, s.db, s.s3Destinations, "volume_backups", policy.S3DestinationID, root)
	if !errors.Is(err, backup.ErrRemoteRepositoryMissing) {
		return false, nil
	}
	collection, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		return false, err
	}
	for i := range collection.Policies {
		current := &collection.Policies[i]
		if current.ID != policy.ID {
			continue
		}
		if current.S3DestinationID != policy.S3DestinationID || current.S3Enabled != policy.S3Enabled || current.Enabled != policy.Enabled || current.LocalEnabled != policy.LocalEnabled {
			return false, nil
		}
		if current.LocalEnabled {
			current.S3Enabled = false
		} else {
			current.Enabled = false
		}
		if saveSystemVolumeBackupPoliciesErr := s.saveSystemVolumeBackupPoliciesInternal(ctx, collection.Policies); saveSystemVolumeBackupPoliciesErr != nil {
			return false, saveSystemVolumeBackupPoliciesErr
		}
		*policy = *current
		s.rescheduleSystemVolumeBackupInternal(ctx, policy)
		return true, nil
	}
	return false, nil
}

// Start freezes the selected policy and volumes before returning.
func (s *Service) Start(ctx context.Context, user usertypes.Actor, request backuptypes.RunSystemVolumeBackupsRequest) (*backuptypes.BackupRunAccepted, error) {
	prepared, err := s.prepareSystemVolumeBackupsInternal(ctx, request)
	if err != nil {
		return nil, err
	}
	policy, manualPolicy, candidates, lease := prepared.policy, prepared.manualPolicy, prepared.candidates, prepared.lease

	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.Name
	}
	activityID, workCtx := activitylib.StartHandlerActivity(ctx, s.activity, "0", activitytypes.TypeResourceAction, "system_backup", "volumes", "Volumes", &user,
		"Backing up volumes", "Creating system-managed volume backups", database.JSON{
			"action":      "run_system_volume_backups",
			"policyId":    policy.ID,
			"volumeNames": names,
			"matched": len(
				candidates,
			),
			"succeeded": 0,
			"failed":    0,
			"skipped":   0,
			"failures":  []backuptypes.SystemVolumeBackupFailure{},
		}, false)
	if activityID == "" {
		lease.Release(ctx)
		return nil, errors.New("failed to create system-managed volume backup activity")
	}
	finish := func(runErr error) {
		defer lease.Release(ctx)
		activitylib.CompleteHandlerActivity(workCtx, s.activity, activityID, "System-managed volume backups completed", runErr)
	}
	keyID, _ := ctx.Value(middleware.ContextKeyApiKeyID).(string)
	payload, err := json.Marshal(manualSystemVolumesInternal{Policy: policy, ManualPolicy: manualPolicy, Candidates: candidates, ActivityID: activityID, UserID: user.ID})
	if err == nil {
		err = s.engine.SubmitDurableRun(
			workCtx,
			backuptypes.DurableRunCommand{
				Kind:             "system-volumes",
				RunID:            activityID,
				ActivityID:       activityID,
				Payload:          payload,
				UserID:           user.ID,
				EnvironmentID:    "0",
				Permission:       authz.PermSystemBackupsManage,
				RequestedWithKey: keyID,
			},
			lease,
		)
	}

	if err != nil {
		finish(err)
		return nil, err
	}
	return &backuptypes.BackupRunAccepted{ActivityID: activityID, Status: "running"}, nil
}

func (
	s *Service,
) updateSystemVolumeProgressInternal(
	ctx context.Context,
	activityID, policyID string,
	candidates []backuptypes.SystemVolumeBackupOption,
	result *backuptypes.SystemVolumeBackupRunResult,
) {
	if activityID == "" {
		return
	}
	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.Name
	}
	progress := 100
	if result.Matched > 0 {
		progress = 100 * (result.Succeeded + result.Failed + result.Skipped) / result.Matched
	}
	_, err := s.activity.UpdateActivity(ctx, activityID, activitylib.UpdateRequest{Progress: &progress, Metadata: database.JSON{
		"action": "run_system_volume_backups", "policyId": policyID, "volumeNames": names,
		"matched": result.Matched, "succeeded": result.Succeeded, "failed": result.Failed, "skipped": result.Skipped, "failures": result.Failures,
	}})
	if err != nil {
		slog.WarnContext(ctx, "Failed to report system-managed volume backup progress", "activityId", activityID, "policyId", policyID, "error", err)
	}
}

type manualSystemVolumesInternal struct {
	Policy       backuptypes.SystemVolumeBackupPolicy   `json:"policy"`
	ManualPolicy bool                                   `json:"manualPolicy"`
	Candidates   []backuptypes.SystemVolumeBackupOption `json:"candidates"`
	ActivityID   string                                 `json:"activityId"`
	UserID       string                                 `json:"userId"`
}

// ExecuteDurable runs a submitted or interrupted system-volumes run.
func (s *Service) ExecuteDurable(ctx context.Context, runID string, payload []byte, interrupted bool) (err error) {
	var command manualSystemVolumesInternal
	if decodeErr := json.Unmarshal(payload, &command); decodeErr != nil {
		return decodeErr
	}
	defer func() {
		if ctx.Err() == nil {
			activitylib.CompleteHandlerActivity(ctx, s.activity, command.ActivityID, "System-managed volume backups completed", err)
		}
	}()
	lease, err := s.acquireDurableRun(ctx, runID)
	if err != nil {
		return err
	}
	defer lease.Release(ctx)
	if interrupted {
		ctx, err = s.reconcileVolumeCandidatesInternal(ctx, command.Candidates)
		if err != nil {
			return err
		}
	}
	result, err := s.executeSystemVolumeBackupsInternal(ctx, command.Policy, command.ManualPolicy, command.Candidates, volume.VolumeBackupTriggerManual, command.ActivityID)
	if err != nil {
		return err
	}
	if result.Failed > 0 {
		return fmt.Errorf("%d volume backups failed", result.Failed)
	}
	return nil
}

func (s *Service) reconcileVolumeCandidatesInternal(ctx context.Context, candidates []backuptypes.SystemVolumeBackupOption) (context.Context, error) {
	previous, _ := jobcontext.Run(ctx)
	for _, candidate := range candidates {
		pending := false
		for _, target := range previous.Outcome.Targets {
			if target.ID == candidate.Name && target.Status != scheduler.Succeeded && target.Status != scheduler.Skipped {
				pending = true
				break
			}
		}
		if !pending {
			continue
		}
		outcome, recoveryErr := s.volumes.ReconcileBackup(ctx, previous, candidate.Name)
		if recoveryErr != nil {
			return ctx, recoveryErr
		}
		if outcome.Status != scheduler.Succeeded {
			return ctx, errors.New(outcome.Message)
		}
		for i := range previous.Outcome.Targets {
			if previous.Outcome.Targets[i].ID == candidate.Name {
				previous.Outcome.Targets[i].Status = scheduler.Succeeded
			}
		}
	}
	progressCtx := ctx
	ctx = jobcontext.WithExecution(ctx, previous, func(target scheduler.TargetOutcome) error { return jobcontext.Progress(progressCtx, target) })
	return ctx, nil
}

// FailDurable completes the activity of a system-volumes run that cannot continue.
func (s *Service) FailDurable(ctx context.Context, _ string, payload []byte, runErr error) error {
	var command manualSystemVolumesInternal
	if err := json.Unmarshal(payload, &command); err != nil {
		return err
	}
	activitylib.CompleteHandlerActivity(ctx, s.activity, command.ActivityID, "System-managed volume backups completed", runErr)
	return nil
}

// RegisterJobsOnStartup schedules every saved policy and returns how many were loaded.
func (s *Service) RegisterJobsOnStartup(ctx context.Context) int {
	policies, err := s.loadSystemVolumeBackupPoliciesInternal()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to load system-managed volume backup policies", "error", err)
		return 0
	}
	for i := range policies.Policies {
		s.rescheduleSystemVolumeBackupInternal(ctx, &policies.Policies[i])
	}
	return len(policies.Policies)
}

func completedVolumeBackupStatusInternal(ctx context.Context, volumeName string) scheduler.RunStatus {
	previous, ok := jobcontext.Run(ctx)
	if !ok {
		return ""
	}
	for _, target := range previous.Outcome.Targets {
		if target.ID == volumeName && (target.Status == scheduler.Succeeded || target.Status == scheduler.Skipped) {
			return target.Status
		}
	}
	return ""
}

func customSystemVolumePolicyInternal(custom *backuptypes.SystemVolumeBackupCustomRun) backuptypes.UpdateSystemVolumeBackupPolicy {
	if custom == nil {
		return backuptypes.UpdateSystemVolumeBackupPolicy{
			Enabled: true, Schedule: defaultSystemVolumeSchedule, LocalEnabled: true,
			SelectionMode: backuptypes.SystemVolumeSelectionAll, VolumeNames: []string{}, IgnoreAnonymous: true,
		}
	}
	localEnabled := custom.Destination == backuptypes.SystemBackupDestinationLocal || custom.Destination == backuptypes.SystemBackupDestinationLocalS3
	s3Enabled := custom.Destination == backuptypes.SystemBackupDestinationS3 || custom.Destination == backuptypes.SystemBackupDestinationLocalS3
	return backuptypes.UpdateSystemVolumeBackupPolicy{
		Enabled: true, Schedule: defaultSystemVolumeSchedule, RetentionCount: 0, StopContainers: custom.StopContainers,
		LocalEnabled: localEnabled, S3Enabled: s3Enabled, S3DestinationID: custom.S3DestinationID,
		SelectionMode: custom.SelectionMode, VolumeNames: custom.VolumeNames, IgnoreAnonymous: custom.IgnoreAnonymous,
	}
}

const (
	systemVolumeBackupConfigKey = "systemVolumeBackupConfig"
	systemVolumeBackupJobPrefix = "volumes:"
	defaultSystemVolumeSchedule = "0 0 2 * * *"
)

func systemVolumeManualPolicyIDInternal(volumeName string) string {
	return backuptypes.SystemVolumePolicyPrefix + "manual:" + kit.SHA256Hex(volumeName)[:16]
}

func systemVolumePolicyIDInternal(policyID, volumeName string) string {
	return backuptypes.SystemVolumePolicyPrefix + policyID + ":" + kit.SHA256Hex(volumeName)[:16]
}

func selectSystemVolumeBackupCandidatesInternal(policy backuptypes.SystemVolumeBackupPolicy, options []backuptypes.SystemVolumeBackupOption) []backuptypes.SystemVolumeBackupOption {
	configured := make(map[string]struct{}, len(policy.VolumeNames))
	for _, name := range policy.VolumeNames {
		configured[name] = struct{}{}
	}
	result := make([]backuptypes.SystemVolumeBackupOption, 0, len(options))
	for _, option := range options {
		if !option.Available {
			continue
		}
		_, selected := configured[option.Name]
		matches := policy.SelectionMode == backuptypes.SystemVolumeSelectionAll ||
			(policy.SelectionMode == backuptypes.SystemVolumeSelectionAllowlist && selected) ||
			(policy.SelectionMode == backuptypes.SystemVolumeSelectionBlocklist && !selected)
		if matches && (!policy.IgnoreAnonymous || !option.Anonymous) {
			result = append(result, option)
		}
	}
	return result
}

func validateSystemVolumeSelectionInternal(mode backuptypes.SystemVolumeSelectionMode) error {
	if mode != backuptypes.SystemVolumeSelectionAll && mode != backuptypes.SystemVolumeSelectionAllowlist && mode != backuptypes.SystemVolumeSelectionBlocklist {
		return errors.New("selectionMode must be all, allowlist, or blocklist")
	}
	return nil
}
