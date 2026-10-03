package recovery

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	arcaneupdater "github.com/getarcaneapp/arcane/types/v2/updater"
	"github.com/moby/moby/client"
	"go.getarcane.app/docker/compat"
	"go.getarcane.app/updater"
	"go.getarcane.app/updater/refs"

	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/jobcontext"
)

// Service freezes update targets before any pull, pins pulls to the frozen
// digests, and confirms or resumes interrupted updates.
type Service struct {
	dockerClient   func(ctx context.Context) (*client.Client, error)
	digestResolver func() updater.RegistryDigestResolver
	pendingUpdates func(ctx context.Context) ([]updater.ImageUpdateRecord, error)
	activityID     func(ctx context.Context) string
	acquireUpdate  func(ctx context.Context) (context.Context, func(), error)
	applyPending   func(ctx context.Context, options arcaneupdater.Options) (*arcaneupdater.Result, error)
}

func NewService(
	dockerClient func(ctx context.Context) (*client.Client, error),
	digestResolver func() updater.RegistryDigestResolver,
	pendingUpdates func(ctx context.Context) ([]updater.ImageUpdateRecord, error),
	activityID func(ctx context.Context) string,
	acquireUpdate func(ctx context.Context) (context.Context, func(), error),
	applyPending func(ctx context.Context, options arcaneupdater.Options) (*arcaneupdater.Result, error),
) *Service {
	return &Service{
		dockerClient:   dockerClient,
		digestResolver: digestResolver,
		pendingUpdates: pendingUpdates,
		activityID:     activityID,
		acquireUpdate:  acquireUpdate,
		applyPending:   applyPending,
	}
}

// FrozenRecords returns the frozen batch records carried by ctx, if any.
func (s *Service) FrozenRecords(ctx context.Context) ([]updater.ImageUpdateRecord, bool) {
	plan, ok := ctx.Value(frozenPendingKeyInternal{}).(*frozenUpdatePlanInternal)
	if !ok {
		return nil, false
	}
	return slices.Clone(plan.Records), true
}

// WithSingleTarget scopes pulls in ctx to one accepted container update.
func (s *Service) WithSingleTarget(ctx context.Context, target *arcaneupdater.FrozenUpdateTarget, persist func() error) context.Context {
	return context.WithValue(ctx, frozenSingleKeyInternal{}, &frozenSingleInternal{Target: target, Persist: persist})
}

// VerifyResult confirms a reported success against the frozen batch plan.
func (s *Service) VerifyResult(ctx context.Context, resourceID string) error {
	plan, ok := ctx.Value(frozenPendingKeyInternal{}).(*frozenUpdatePlanInternal)
	if !ok {
		return nil
	}
	for _, target := range plan.Targets {
		if target.ContainerID != resourceID {
			continue
		}
		confirmed, _, err := s.ConfirmTarget(ctx, target)
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("updater result does not confirm the frozen desired image")
		}
		return nil
	}
	return nil
}

type (
	frozenPendingKeyInternal struct{}
	frozenSingleKeyInternal  struct{}
	frozenUpdatePlanInternal struct {
		Records []updater.ImageUpdateRecord
		Targets []arcaneupdater.FrozenUpdateTarget
	}
)

type frozenSingleInternal struct {
	Target  *arcaneupdater.FrozenUpdateTarget
	Persist func() error
}

// FreezePending persists identities and desired images before any pull.
func (s *Service) FreezePending(ctx context.Context) (context.Context, error) {
	if _, ok := ctx.Value(frozenPendingKeyInternal{}).(*frozenUpdatePlanInternal); ok {
		return ctx, nil
	}
	run, ok := jobcontext.Run(ctx)
	if !ok {
		return ctx, nil
	}
	for _, target := range run.Outcome.Targets {
		if target.ID == "auto-update" && len(target.RecoveryData) > 0 {
			var plan frozenUpdatePlanInternal
			if err := json.Unmarshal(target.RecoveryData, &plan); err != nil {
				return ctx, err
			}
			return context.WithValue(ctx, frozenPendingKeyInternal{}, &plan), nil
		}
	}
	records, err := s.pendingUpdates(ctx)
	if err != nil {
		return ctx, err
	}
	plan, err := s.buildFrozenPlanInternal(ctx, records)
	if err != nil {
		return ctx, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return ctx, err
	}
	if progressErr := jobcontext.Progress(
		ctx,
		scheduler.TargetOutcome{
			ID:           "auto-update",
			ResourceType: "update-batch",
			Status:       scheduler.Running,
			RecoveryData: raw,
			ActivityID: s.activityID(
				ctx,
			),
		},
	); progressErr != nil {
		return ctx, progressErr
	}
	for _, target := range plan.Targets {
		if queuedProgressErr := jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: target.ContainerID, ResourceType: "container", Status: scheduler.Queued}); queuedProgressErr != nil {
			return ctx, queuedProgressErr
		}
	}
	return context.WithValue(ctx, frozenPendingKeyInternal{}, &plan), nil
}

func (s *Service) buildFrozenPlanInternal(ctx context.Context, records []updater.ImageUpdateRecord) (frozenUpdatePlanInternal, error) {
	plan := frozenUpdatePlanInternal{Records: make([]updater.ImageUpdateRecord, 0, len(records))}
	if len(records) == 0 {
		return plan, nil
	}
	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return plan, err
	}
	listed, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: false})
	if err != nil {
		return plan, err
	}
	for _, record := range records {
		if !record.HasUpdate {
			continue
		}
		for _, candidate := range listed.Items {
			if record.ContainerID != "" && record.ContainerID != candidate.ID {
				continue
			}
			if record.ContainerID == "" && refs.NormalizeImageUpdateRef(candidate.Image) != refs.NormalizeImageUpdateRef(record.ImageRef()) {
				continue
			}
			target, selected, freezeRecordTargetErr := s.freezeRecordTargetInternal(ctx, record, candidate.ID)
			if freezeRecordTargetErr != nil {
				return plan, freezeRecordTargetErr
			}
			plan.Records = append(plan.Records, selected)
			plan.Targets = append(plan.Targets, *target)
		}
	}
	return plan, nil
}

func (s *Service) freezeRecordTargetInternal(ctx context.Context, record updater.ImageUpdateRecord, containerID string) (*arcaneupdater.FrozenUpdateTarget, updater.ImageUpdateRecord, error) {
	target, err := s.FreezeContainer(ctx, containerID)
	if err != nil {
		return nil, record, err
	}
	target.DesiredImageRef = record.NewImageRef()
	if record.LatestDigest != nil && !record.IsTagUpdate() {
		target.DesiredDigest = *record.LatestDigest
	}
	if target.DesiredDigest == "" {
		resolver := s.digestResolver()
		if resolver == nil {
			return nil, record, errors.New("cannot freeze update without registry digest resolver")
		}
		target.DesiredDigest, err = resolver.ImageDigest(ctx, target.DesiredImageRef)
		if err != nil {
			return nil, record, err
		}
	}
	if target.DesiredDigest == "" {
		return nil, record, errors.New("registry returned no desired digest")
	}
	record.ContainerID = containerID
	digest := target.DesiredDigest
	record.LatestDigest = &digest
	return target, record, nil
}

// FreezeContainer captures the identity evidence of one container.
func (s *Service) FreezeContainer(ctx context.Context, id string) (*arcaneupdater.FrozenUpdateTarget, error) {
	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, err
	}
	result, err := compat.ContainerInspectWithCompatibility(ctx, dockerClient, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	inspected := result.Container
	target := &arcaneupdater.FrozenUpdateTarget{
		ContainerID: inspected.ID,
		ContainerName: strings.TrimPrefix(
			inspected.Name,
			"/",
		),
		BaselineImageID:      inspected.Image,
		BaselineRestartCount: inspected.RestartCount,
	}
	if inspected.State != nil {
		target.BaselineStartedAt = inspected.State.StartedAt
	}
	if inspected.Config != nil {
		target.ComposeProject = inspected.Config.Labels["com.docker.compose.project"]
		target.ComposeService = inspected.Config.Labels["com.docker.compose.service"]
		target.ComposeNumber = inspected.Config.Labels["com.docker.compose.container-number"]
	}
	return target, nil
}

// PreparePull commits the exact image before the engine changes Docker.
func (s *Service) PreparePull(ctx context.Context, imageRef string) (string, error) {
	if single, ok := ctx.Value(frozenSingleKeyInternal{}).(*frozenSingleInternal); ok {
		return s.prepareSinglePullInternal(ctx, imageRef, single)
	}
	if plan, ok := ctx.Value(frozenPendingKeyInternal{}).(*frozenUpdatePlanInternal); ok {
		return prepareBatchPullInternal(ctx, imageRef, plan)
	}
	return imageRef, nil
}

func (s *Service) prepareSinglePullInternal(ctx context.Context, imageRef string, single *frozenSingleInternal) (string, error) {
	target := single.Target
	if target.DesiredImageRef == "" {
		target.DesiredImageRef = imageRef
		resolver := s.digestResolver()
		if resolver == nil {
			return "", errors.New("cannot freeze update without registry digest resolver")
		}
		digest, err := resolver.ImageDigest(ctx, imageRef)
		if err != nil {
			return "", err
		}
		target.DesiredDigest = digest
		if persistErr := single.Persist(); persistErr != nil {
			return "", persistErr
		}
	}
	if refs.NormalizeImageUpdateRef(target.DesiredImageRef) != refs.NormalizeImageUpdateRef(imageRef) {
		return "", errors.New("selected image changed after the update was accepted")
	}
	return immutableImageInternal(*target)
}

func prepareBatchPullInternal(ctx context.Context, imageRef string, plan *frozenUpdatePlanInternal) (string, error) {
	immutable := ""
	for _, target := range plan.Targets {
		if refs.NormalizeImageUpdateRef(target.DesiredImageRef) != refs.NormalizeImageUpdateRef(imageRef) {
			continue
		}
		selected, err := immutableImageInternal(target)
		if err != nil {
			return "", err
		}
		if immutable != "" && immutable != selected {
			return "", errors.New("conflicting frozen digests for one image reference")
		}
		immutable = selected
		if progressErr := jobcontext.Progress(ctx, scheduler.TargetOutcome{ID: target.ContainerID, ResourceType: "container", Status: scheduler.Running}); progressErr != nil {
			return "", progressErr
		}
	}
	if immutable == "" {
		return "", errors.New("updater attempted to pull an image outside its frozen plan")
	}
	return immutable, nil
}

// ConfirmTarget returns whether the desired effect is confirmed,
// or whether the exact original container is unchanged and safe to resume.
func (s *Service) ConfirmTarget(ctx context.Context, target arcaneupdater.FrozenUpdateTarget) (bool, bool, error) {
	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return false, false, err
	}
	listed, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return false, false, err
	}
	immutable, imageErr := immutableImageInternal(target)
	desiredID := ""
	if imageErr == nil {
		image, imageInspectErr := dockerClient.ImageInspect(ctx, immutable)
		if imageInspectErr == nil {
			desiredID = image.ID
		}
	}
	for _, candidate := range listed.Items {
		named := false
		for _, name := range candidate.Names {
			if strings.TrimPrefix(name, "/") == target.ContainerName {
				named = true
				break
			}
		}
		if !named {
			continue
		}
		result, containerInspectWithCompatibilityErr := compat.ContainerInspectWithCompatibility(ctx, dockerClient, candidate.ID, client.ContainerInspectOptions{})
		if containerInspectWithCompatibilityErr != nil {
			return false, false, containerInspectWithCompatibilityErr
		}
		inspected := result.Container
		if inspected.Config == nil {
			return false, false, nil
		}
		labels := inspected.Config.Labels
		if labels["com.docker.compose.project"] != target.ComposeProject ||
			labels["com.docker.compose.service"] != target.ComposeService ||
			labels["com.docker.compose.container-number"] != target.ComposeNumber {
			return false, false, nil
		}
		if desiredID != "" && inspected.Image == desiredID {
			return true, false, nil
		}
		unchanged := inspected.ID == target.ContainerID &&
			inspected.Image == target.BaselineImageID &&
			inspected.RestartCount == target.BaselineRestartCount &&
			inspected.State != nil &&
			inspected.State.StartedAt == target.BaselineStartedAt
		return false, unchanged, nil
	}
	return false, false, nil
}

// ReconcilePending resumes only frozen targets whose original container is unchanged.
func (s *Service) ReconcilePending(ctx context.Context, run scheduler.Run) (scheduler.Outcome, error) {
	ctx, release, err := s.acquireUpdate(ctx)
	if err != nil {
		return scheduler.Outcome{Status: scheduler.Waiting}, err
	}
	defer release()
	var plan frozenUpdatePlanInternal
	found := false
	for _, target := range run.Outcome.Targets {
		if target.ID == "auto-update" && len(target.RecoveryData) > 0 {
			if unmarshalErr := json.Unmarshal(target.RecoveryData, &plan); unmarshalErr != nil {
				return scheduler.Outcome{Status: scheduler.Failed}, unmarshalErr
			}
			found = true
		}
	}
	if !found {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Interrupted update has no frozen target plan", Targets: run.Outcome.Targets}, nil
	}
	remaining := frozenUpdatePlanInternal{}
	unresolved := false
	for _, target := range plan.Targets {
		if frozenTargetSettledInternal(run, target.ContainerID) {
			continue
		}
		confirmed, unchanged, confirmFrozenTargetErr := s.ConfirmTarget(ctx, target)
		if confirmFrozenTargetErr != nil {
			return scheduler.Outcome{Status: scheduler.Waiting}, confirmFrozenTargetErr
		}
		if confirmed {
			if progressErr := jobcontext.Progress(
				ctx,
				scheduler.TargetOutcome{
					ID:           target.ContainerID,
					ResourceType: "container",
					Status:       scheduler.Succeeded,
					Message:      "Frozen desired image confirmed after restart",
				},
			); progressErr != nil {
				return scheduler.Outcome{}, progressErr
			}
			continue
		}
		if !unchanged {
			unresolved = true
			continue
		}
		remaining.Targets = append(remaining.Targets, target)
		remaining.Records = appendFrozenRecordInternal(remaining.Records, plan.Records, target.ContainerID)
	}
	if len(remaining.Records) > 0 {
		result, applyPendingErr := s.applyPending(context.WithValue(ctx, frozenPendingKeyInternal{}, &remaining), arcaneupdater.Options{})
		if applyPendingErr != nil || result == nil || result.Failed > 0 {
			return scheduler.Outcome{Status: scheduler.Failed, Message: "Frozen update recovery could not confirm completion"}, applyPendingErr
		}
	}
	status := scheduler.Succeeded
	message := "Frozen update targets confirmed"
	if unresolved {
		status = scheduler.Failed
		message = "Some update effects could not be confirmed"
	}
	return scheduler.Outcome{Status: status, Message: message}, nil
}

func appendFrozenRecordInternal(remaining, records []updater.ImageUpdateRecord, id string) []updater.ImageUpdateRecord {
	for _, record := range records {
		if record.ContainerID == id {
			return append(remaining, record)
		}
	}
	return remaining
}

func frozenTargetSettledInternal(run scheduler.Run, id string) bool {
	for _, target := range run.Outcome.Targets {
		if target.ID == id && (target.Status == scheduler.Succeeded || target.Status == scheduler.Skipped) {
			return true
		}
	}
	return false
}

func immutableImageInternal(target arcaneupdater.FrozenUpdateTarget) (string, error) {
	reference, err := refs.NormalizeReference(target.DesiredImageRef)
	if err != nil {
		return "", err
	}
	if target.DesiredDigest == "" {
		return "", errors.New("desired update digest is unavailable")
	}
	return reference.RegistryHost + "/" + reference.Repository + "@" + target.DesiredDigest, nil
}
