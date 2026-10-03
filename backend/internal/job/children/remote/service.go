// Package remote owns manager-side delivery, history and review of job runs
// executed on remote agents.
package remote

import (
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/jobschedule"
	"github.com/getarcaneapp/arcane/types/v2/meta"
	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	"github.com/getarcaneapp/arcane/types/v2/user"
	"go.getarcane.app/kit/pkg"

	"github.com/getarcaneapp/arcane/backend/v2/internal/environment"
	"github.com/getarcaneapp/arcane/backend/v2/internal/kv"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/remenv"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
)

// Service talks to agents through the environment proxy and persists delivery
// state in the shared run coordinator.
type Service struct {
	runs         *runs.Coordinator
	store        *kv.KVService
	environments *environment.EnvironmentService
}

func New(coordinator *runs.Coordinator, store *kv.KVService, environments *environment.EnvironmentService) *Service {
	return &Service{runs: coordinator, store: store, environments: environments}
}

// Deliver drives one manager-owned run through the remote delivery protocol.
func (s *Service) Deliver(ctx context.Context, run scheduler.Run) (scheduler.Outcome, error) {
	env, err := s.environments.GetEnvironmentByID(ctx, run.EnvironmentID)
	if err != nil {
		return scheduler.Outcome{Status: scheduler.Failed}, err
	}
	if !env.Enabled {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Environment is disabled"}, nil
	}
	basePath := "/api/environments/0/jobs/" + url.PathEscape(run.JobID)
	runPath := basePath + "/runs/" + url.PathEscape(run.ID)
	// A previously confirmed terminal result needs only acknowledgement, never execution.
	if !run.RemoteRetryRequested && run.RemoteAccepted && run.RemoteOutcome != nil && run.RemoteOutcome.Status.Terminal() {
		return s.acknowledgeRemoteInternal(ctx, run, runPath, *run.RemoteOutcome)
	}
	var catalog jobschedule.JobListResponse
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodGet, "/api/environments/0/jobs", nil, &catalog); proxyJSONRequestErr != nil {
		return remoteFailureInternal(proxyJSONRequestErr)
	}
	if !catalog.DurableRuns {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Upgrade required: agent does not support durable job runs"}, nil
	}
	var remoteRun scheduler.Run
	err = s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodGet, runPath, nil, &remoteRun)
	if err != nil {
		var status *remenv.StatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
			return remoteFailureInternal(err)
		}
		remoteRun, err = s.admitRemoteInternal(ctx, run, catalog)
		if err != nil {
			if errors.Is(err, errRemoteIneligibleInternal) {
				return scheduler.Outcome{Status: scheduler.Canceled, Message: err.Error()}, nil
			}
			if errors.Is(err, errRemoteReceiptMissingInternal) {
				return scheduler.Outcome{Status: scheduler.Failed, Message: err.Error()}, nil
			}
			return remoteFailureInternal(err)
		}
	}
	if remoteRun.ID != run.ID || remoteRun.JobID != run.JobID {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Agent returned an inconsistent run identity"}, nil
	}
	if run.RemoteRetryRequested && (remoteRun.Status.Terminal() || remoteRun.Status == scheduler.NeedsAttention) && remoteRun.AttemptCount <= run.RemoteAttemptCount {
		remoteRun, err = s.retryDeliveryInternal(ctx, run, runPath)
		if err != nil {
			if errors.Is(err, errRemoteRetryUncertainInternal) {
				return scheduler.Outcome{Status: scheduler.Failed, Message: err.Error()}, nil
			}
			return remoteFailureInternal(err)
		}
	}
	return s.Confirm(ctx, run, remoteRun, runPath)
}

// Confirm records the agent-reported state of a delivered run.
func (s *Service) Confirm(ctx context.Context, run, remoteRun scheduler.Run, runPath string) (scheduler.Outcome, error) {
	if remoteRun.Status == scheduler.NeedsAttention || (remoteRun.Status == scheduler.Canceled && remoteRun.Resolution != nil && remoteRun.Resolution.ResolvedBy == user.SystemUser.Username) {
		settled, err := s.resolveAgentReviewInternal(ctx, run.EnvironmentID, run.JobID, run.ID, user.SystemUser.Username)
		if err != nil {
			return scheduler.Outcome{
				Status:     scheduler.Waiting,
				Message:    "Waiting for the agent to acknowledge the failed run",
				Targets:    remoteRun.Outcome.Targets,
				ActivityID: remoteRun.Outcome.ActivityID,
			}, err
		}
		remoteRun = settled
		if remoteRun.Status == scheduler.Canceled && remoteRun.Resolution != nil && remoteRun.Resolution.ResolvedBy == user.SystemUser.Username {
			remoteRun.Status = scheduler.Failed
		}
	}
	now := time.Now().UTC()
	outcome := remoteRun.Outcome
	outcome.Status = remoteRun.Status
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		current.RemoteAccepted = true
		current.RemoteDeliveryAttempted = true
		current.LastConfirmedAt = &now
		current.RemoteOutcome = &outcome
		current.RemoteAttemptCount = remoteRun.AttemptCount
		current.RemoteRetryRequested = false
		current.RemoteSettled = remoteRun.RemoteSettled
		current.Resolution = remoteRun.Resolution
		current.StartedAt = remoteRun.StartedAt
		current.FinishedAt = remoteRun.FinishedAt
		current.Outcome = outcome
		return nil
	}); err != nil {
		return scheduler.Outcome{Status: scheduler.Retrying}, err
	}
	if outcome.Status.Terminal() {
		if remoteRun.RemoteSettled {
			return outcome, nil
		}
		return s.acknowledgeRemoteInternal(ctx, run, runPath, outcome)
	}
	return scheduler.Outcome{Status: scheduler.Waiting, Message: "Waiting for the agent to complete this run", Targets: outcome.Targets, ActivityID: outcome.ActivityID}, nil
}

func (s *Service) acknowledgeRemoteInternal(ctx context.Context, run scheduler.Run, path string, outcome scheduler.Outcome) (scheduler.Outcome, error) {
	var response scheduler.Run
	if err := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, path+"/ack", nil, &response); err != nil {
		return scheduler.Outcome{Status: scheduler.Waiting, Message: "Remote operation completed; waiting for delivery acknowledgement", Targets: outcome.Targets, ActivityID: outcome.ActivityID}, err
	}
	status := response.Status
	if status == scheduler.Canceled && response.Resolution != nil && response.Resolution.ResolvedBy == user.SystemUser.Username {
		status = scheduler.Failed
	}
	if response.ID != run.ID || response.JobID != run.JobID || response.EnvironmentID != "0" || status != outcome.Status || !response.RemoteSettled {
		return scheduler.Outcome{Status: scheduler.Waiting, Message: "Remote operation completed; acknowledgement not confirmed"}, nil
	}
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		current.RemoteSettled = true
		current.StartedAt = response.StartedAt
		current.FinishedAt = response.FinishedAt
		return nil
	}); err != nil {
		return scheduler.Outcome{Status: scheduler.Retrying}, err
	}
	return outcome, nil
}

// Catalog retains the last observed agent catalog while its environment is offline.
func (s *Service) Catalog(ctx context.Context, environmentID string) (*jobschedule.JobListResponse, error) {
	if _, err := s.environments.GetEnvironmentByID(ctx, environmentID); err != nil {
		return nil, err
	}
	key := "jobs-catalog/" + kit.SHA256Hex(environmentID)
	var catalog jobschedule.JobListResponse
	remoteErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, "/api/environments/0/jobs", nil, &catalog)
	if remoteErr == nil {
		catalog.ObservedAt = time.Now().UTC()
		catalog.Offline = false
		if catalog.DurableRuns {
			if err := s.ReconcileLegacyCatalog(ctx, environmentID, catalog.Jobs); err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(catalog)
		if err != nil {
			return nil, err
		}
		if setErr := s.store.Set(ctx, key, string(raw)); setErr != nil {
			return nil, setErr
		}
	} else {
		var status *remenv.StatusError
		if errors.As(remoteErr, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden) {
			return nil, remoteErr
		}
		raw, found, err := s.store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if found {
			if unmarshalErr := json.Unmarshal([]byte(raw), &catalog); unmarshalErr != nil {
				return nil, unmarshalErr
			}
		} else {
			catalog.IsAgent = true
			for _, metadata := range meta.GetAllJobMetadata() {
				if !metadata.ManagerOnly {
					catalog.Jobs = append(catalog.Jobs, metadata.ToJobStatus("", nil, metadata.CanRunManually, []jobschedule.JobPrerequisite{}))
				}
			}
		}
		catalog.Offline = true
	}
	return &catalog, nil
}

// RetryRun queues one explicit retry of an authorized manager-owned run while
// retaining the agent's target progress.
func (s *Service) RetryRun(ctx context.Context, run scheduler.Run) (scheduler.Run, error) {
	environmentID, jobID, runID := run.EnvironmentID, run.JobID, run.ID
	if run.Resolution != nil && run.Resolution.ResolvedBy == user.SystemUser.Username {
		var remote scheduler.Run
		path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
		if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &remote); proxyJSONRequestErr != nil {
			return run, proxyJSONRequestErr
		}
		if remote.ID != runID || remote.JobID != jobID || remote.EnvironmentID != "0" || remote.Status != scheduler.Failed {
			return run, errors.New("upgrade the agent before retrying a legacy settled run")
		}
	}
	err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		if current.Status != scheduler.Failed && current.Status != scheduler.Partial && current.Status != scheduler.NeedsAttention {
			return errors.New("run is not eligible for retry")
		}
		if !current.RemoteAccepted {
			if current.RemoteDeliveryAttempted {
				return errors.New("remote delivery is uncertain; inspect the agent before submitting new work")
			}
		} else {
			if current.RemoteOutcome == nil || (!current.RemoteOutcome.Status.Terminal() && current.RemoteOutcome.Status != scheduler.NeedsAttention) {
				return errors.New("remote outcome must be confirmed before retry")
			}
			current.RemoteRetryRequested = true
			current.RemoteRetryAttempted = false
			current.RemoteSettled = false
		}
		current.Status = scheduler.Queued
		current.Owner = ""
		current.NextAttempt = nil
		current.FinishedAt = nil
		current.UpdatedAt = time.Now().UTC()
		run = *current
		return nil
	})
	return run, err
}

var (
	errRemoteIneligibleInternal     = errors.New("Remote job is disabled or no longer eligible")                                   //nolint:staticcheck // Preserve the existing error message.
	errRemoteReceiptMissingInternal = errors.New("Agent no longer has the delivery receipt; verify the operation before retrying") //nolint:staticcheck // Preserve the existing error message.
	errRemoteRetryUncertainInternal = errors.New("Remote retry acknowledgement was lost; inspect the agent before retrying again") //nolint:staticcheck // Preserve the existing error message.
)

func (s *Service) admitRemoteInternal(ctx context.Context, run scheduler.Run, catalog jobschedule.JobListResponse) (scheduler.Run, error) {
	if run.RemoteAccepted {
		return scheduler.Run{}, errRemoteReceiptMissingInternal
	}
	if !remoteJobEligibleInternal(catalog.Jobs, run.JobID) {
		return scheduler.Run{}, errRemoteIneligibleInternal
	}
	// A confirmed 404 permits another delivery of the same deduplicated ID.
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		current.RemoteDeliveryAttempted = true
		return nil
	}); err != nil {
		return scheduler.Run{}, err
	}
	body, err := json.Marshal(map[string]string{"runId": run.ID})
	if err != nil {
		return scheduler.Run{}, err
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(run.JobID) + "/run"
	var accepted jobschedule.JobRunResponse
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, path, body, &accepted); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if accepted.RunID != run.ID {
		return scheduler.Run{}, errors.New("Agent returned an inconsistent delivery receipt") //nolint:staticcheck // Preserve the existing error message.
	}
	return scheduler.Run{ID: run.ID, JobID: run.JobID, Status: accepted.Status, Outcome: scheduler.Outcome{Status: accepted.Status}}, nil
}

func (s *Service) retryDeliveryInternal(ctx context.Context, run scheduler.Run, path string) (scheduler.Run, error) {
	if run.RemoteRetryAttempted {
		return scheduler.Run{}, errRemoteRetryUncertainInternal
	}
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		current.RemoteRetryAttempted = true
		return nil
	}); err != nil {
		return scheduler.Run{}, err
	}
	var retried scheduler.Run
	if err := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, path+"/retry", nil, &retried); err != nil {
		return scheduler.Run{}, err
	}
	if retried.ID != run.ID || retried.JobID != run.JobID {
		return scheduler.Run{}, errors.New("Agent returned an inconsistent retry receipt") //nolint:staticcheck // Preserve the existing error message.
	}
	return retried, nil
}

// Runs merges agent history with requests accepted by this manager.
func (s *Service) Runs(ctx context.Context, environmentID, jobID string) ([]scheduler.Run, error) {
	if _, err := s.environments.GetEnvironmentByID(ctx, environmentID); err != nil {
		return nil, err
	}
	records, err := s.runs.Records(ctx)
	if err != nil {
		return nil, err
	}
	merged, err := s.remoteHistoryInternal(ctx, environmentID, jobID)
	if err != nil {
		var status *remenv.StatusError
		if errors.As(err, &status) && (status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusUnauthorized) {
			return nil, err
		}
		merged = make(map[string]scheduler.Run)
	}
	for _, record := range records {
		if record.EnvironmentID != environmentID || record.JobID != jobID {
			continue
		}
		for _, run := range record.Runs {
			merged[run.ID] = run
		}
	}
	return slices.Collect(maps.Values(merged)), nil
}

func (s *Service) remoteHistoryInternal(ctx context.Context, environmentID, jobID string) (map[string]scheduler.Run, error) {
	localRuns := make(map[string]scheduler.Run)
	for page := 1; ; page++ {
		path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs?page=" + strconv.Itoa(page) + "&limit=100"
		var response scheduler.RunList
		if err := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		previous := len(localRuns)
		for _, run := range response.Runs {
			run.EnvironmentID = environmentID
			if run.ActivityID != "" {
				run.ActivityEnvironmentID = environmentID
			}
			localRuns[run.ID] = run
		}
		if len(localRuns) >= response.Total || len(localRuns) == previous {
			return localRuns, nil
		}
	}
}

// AgentRun queries agent-owned history for a run the manager never accepted.
func (s *Service) AgentRun(ctx context.Context, environmentID, jobID, runID string) (scheduler.Run, error) {
	var run scheduler.Run
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &run); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	run.EnvironmentID = environmentID
	if run.ActivityID != "" {
		run.ActivityEnvironmentID = environmentID
	}
	return run, nil
}

// MutateAgentRun forwards an explicit operator action to an agent-owned run.
func (s *Service) MutateAgentRun(ctx context.Context, environmentID, jobID, runID, action string) (scheduler.Run, error) {
	targetEnvironment, err := s.environments.GetEnvironmentByID(ctx, environmentID)
	if err != nil {
		return scheduler.Run{}, err
	}
	if !targetEnvironment.Enabled {
		return scheduler.Run{}, errors.New("environment is disabled")
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/" + action
	var run scheduler.Run
	// This is an explicit operator action. Never replay a lost mutation response.
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path, nil, &run); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	run.EnvironmentID = environmentID
	if run.ActivityID != "" {
		run.ActivityEnvironmentID = environmentID
	}
	return run, nil
}

// ReconcileLegacyCatalog adopts legacy agent-owned runs needing attention.
// TODO(v3): remove adoption of legacy agent-owned runs.
func (s *Service) ReconcileLegacyCatalog(ctx context.Context, environmentID string, jobs []jobschedule.JobStatus) error {
	for _, job := range jobs {
		for _, remote := range []*scheduler.Run{job.CurrentRun, job.LastRun} {
			if remote == nil || remote.Status != scheduler.NeedsAttention {
				continue
			}
			if remote.JobID != job.ID || remote.EnvironmentID != "0" {
				return errors.New("agent returned an inconsistent run identity")
			}
			_, err := s.runs.Get(ctx, environmentID, job.ID, remote.ID)
			if err == nil {
				continue
			}
			if !errors.Is(err, runs.ErrRunNotFound) {
				return err
			}
			_, err = s.runs.Submit(ctx, scheduler.Request{RunID: remote.ID, JobID: job.ID, EnvironmentID: environmentID, Trigger: "recovery", ObservedAgentRun: remote})
			if err != nil {
				return err
			}
		}
		if err := s.ReconcileLegacyCatalog(ctx, environmentID, job.Children); err != nil {
			return err
		}
	}
	return nil
}

// ResolveRun settles a remote run that needs operator review.
func (s *Service) ResolveRun(ctx context.Context, environmentID, jobID, runID, actor string) (scheduler.Run, error) {
	local, localErr := s.runs.Get(ctx, environmentID, jobID, runID)
	if localErr != nil && !errors.Is(localErr, runs.ErrRunNotFound) {
		return scheduler.Run{}, localErr
	}
	if localErr == nil {
		if local.Status == scheduler.Canceled && local.Resolution != nil {
			return local, nil
		}
		if local.Status != scheduler.NeedsAttention {
			return local, errors.New("only runs needing attention can be resolved")
		}
		if !local.RemoteDeliveryAttempted && !local.RemoteAccepted {
			return s.runs.Resolve(ctx, environmentID, jobID, runID, actor)
		}
	}
	acknowledged, err := s.resolveAgentReviewInternal(ctx, environmentID, jobID, runID, actor)
	if err != nil {
		return scheduler.Run{}, err
	}
	if localErr != nil {
		acknowledged.EnvironmentID = environmentID
		if acknowledged.ActivityID != "" {
			acknowledged.ActivityEnvironmentID = environmentID
		}
		return acknowledged, nil
	}
	now := time.Now().UTC()
	if updateRunErr := s.runs.UpdateRun(ctx, local, func(current *scheduler.Run) error {
		if current.Status != scheduler.NeedsAttention {
			return runs.ErrRunConflict
		}
		outcome := acknowledged.Outcome
		outcome.Status = acknowledged.Status
		current.RemoteOutcome = &outcome
		current.RemoteAccepted = true
		current.RemoteSettled = true
		current.LastConfirmedAt = &now
		return nil
	}); updateRunErr != nil {
		return scheduler.Run{}, updateRunErr
	}
	return s.runs.Resolve(ctx, environmentID, jobID, runID, acknowledged.Resolution.ResolvedBy)
}

// resolveAgentReviewInternal settles legacy inactive runs on their owning agent.
// TODO(v3): remove the legacy resolution protocol.
func (s *Service) resolveAgentReviewInternal(ctx context.Context, environmentID, jobID, runID, actor string) (scheduler.Run, error) {
	env, err := s.environments.GetEnvironmentByID(ctx, environmentID)
	if err != nil {
		return scheduler.Run{}, err
	}
	if !env.Enabled {
		return scheduler.Run{}, errors.New("environment is disabled")
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	var remote scheduler.Run
	// Read the owner on every request, including retries after a lost response.
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &remote); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if remote.ID != runID || remote.JobID != jobID || remote.EnvironmentID != "0" {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	if remote.Status == scheduler.NeedsAttention {
		body, marshalErr := json.Marshal(map[string]string{"resolvedBy": actor})
		if marshalErr != nil {
			return scheduler.Run{}, marshalErr
		}
		if resolveRemoteJobErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path+"/resolve", body, &remote); resolveRemoteJobErr != nil {
			return scheduler.Run{}, resolveRemoteJobErr
		}
	} else if actor != user.SystemUser.Username {
		if remote.Status != scheduler.Canceled || remote.Resolution == nil {
			return scheduler.Run{}, errors.New("agent run must need attention before review resolution")
		}
	}
	if remote.ID != runID || remote.JobID != jobID || remote.EnvironmentID != "0" || !remote.Status.Terminal() ||
		(actor != user.SystemUser.Username && (remote.Status != scheduler.Canceled || remote.Resolution == nil)) {
		return scheduler.Run{}, errors.New("agent resolution was not confirmed")
	}
	var acknowledged scheduler.Run
	if acknowledgeRemoteResolutionErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path+"/ack", nil, &acknowledged); acknowledgeRemoteResolutionErr != nil {
		return scheduler.Run{}, acknowledgeRemoteResolutionErr
	}
	if acknowledged.ID != runID || acknowledged.JobID != jobID || acknowledged.EnvironmentID != "0" || acknowledged.Status != remote.Status || !acknowledged.RemoteSettled ||
		(remote.Resolution != nil && (acknowledged.Resolution == nil || acknowledged.Resolution.ResolvedBy != remote.Resolution.ResolvedBy)) {
		return scheduler.Run{}, errors.New("agent resolution acknowledgement was not confirmed")
	}
	return acknowledged, nil
}

func remoteJobEligibleInternal(jobs []jobschedule.JobStatus, id string) bool {
	for _, job := range jobs {
		if job.ID != id {
			if remoteJobEligibleInternal(job.Children, id) {
				return true
			}
			continue
		}
		if !job.Enabled || !job.CanRunManually || job.ManagerOnly {
			return false
		}
		for _, prerequisite := range job.Prerequisites {
			if !prerequisite.IsMet {
				return false
			}
		}
		return true
	}
	return false
}

func remoteFailureInternal(err error) (scheduler.Outcome, error) {
	var status *remenv.StatusError
	rejected := errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 &&
		status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusTooManyRequests
	if rejected {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Remote agent rejected the request; check configuration and permissions"}, err
	}
	return scheduler.Outcome{Status: scheduler.Waiting, Message: "Waiting for environment"}, err
}
