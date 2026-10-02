package job

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	st "github.com/getarcaneapp/arcane/types/v2/scheduler"

	"github.com/getarcaneapp/arcane/backend/v2/pkg/remenv"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
)

// ListRuns merges agent history with requests accepted by this manager.
func (s *JobService) ListRuns(ctx context.Context, environmentID, jobID string, page, limit int) (st.RunList, error) {
	if environmentID == "0" {
		return s.runs.List(ctx, environmentID, jobID, page, limit)
	}
	if _, err := s.environment.GetEnvironmentByID(ctx, environmentID); err != nil {
		return st.RunList{}, err
	}
	records, err := s.runs.Records(ctx)
	if err != nil {
		return st.RunList{}, err
	}
	merged, err := s.remoteHistoryInternal(ctx, environmentID, jobID)
	if err != nil {
		var status *remenv.StatusError
		if errors.As(err, &status) && (status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusUnauthorized) {
			return st.RunList{}, err
		}
		merged = make(map[string]st.Run)
	}
	for _, record := range records {
		if record.EnvironmentID != environmentID || record.JobID != jobID {
			continue
		}
		for _, run := range record.Runs {
			merged[run.ID] = run
		}
	}
	localRuns := make([]st.Run, 0, len(merged))
	for _, run := range merged {
		localRuns = append(localRuns, projectRunOutcomeInternal(run))
	}
	sort.Slice(localRuns, func(i, j int) bool {
		if localRuns[i].CreatedAt.Equal(localRuns[j].CreatedAt) {
			return localRuns[i].ID < localRuns[j].ID
		}
		return localRuns[i].CreatedAt.After(localRuns[j].CreatedAt)
	})
	page = max(page, 1)
	limit = min(max(limit, 1), 100)
	start := len(localRuns)
	if page-1 <= len(localRuns)/limit {
		start = min((page-1)*limit, len(localRuns))
	}
	return st.RunList{Runs: localRuns[start:min(start+limit, len(localRuns))], Total: len(localRuns), Page: page, Limit: limit}, nil
}

func (s *JobService) remoteHistoryInternal(ctx context.Context, environmentID, jobID string) (map[string]st.Run, error) {
	localRuns := make(map[string]st.Run)
	for page := 1; ; page++ {
		path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs?page=" + strconv.Itoa(page) + "&limit=100"
		var response st.RunList
		if err := s.environment.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &response); err != nil {
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

// GetRun prefers the manager's durable delivery record, then queries agent-owned history.
func (s *JobService) GetRun(ctx context.Context, environmentID, jobID, runID string) (st.Run, error) {
	run, err := s.runs.Get(ctx, environmentID, jobID, runID)
	if environmentID == "0" || !errors.Is(err, runs.ErrRunNotFound) {
		return projectRunOutcomeInternal(run), err
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	if proxyJSONRequestErr := s.environment.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &run); proxyJSONRequestErr != nil {
		return st.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return st.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	run.EnvironmentID = environmentID
	if run.ActivityID != "" {
		run.ActivityEnvironmentID = environmentID
	}
	return projectRunOutcomeInternal(run), nil
}

// CancelRun cancels manager admission or explicitly forwards an agent-owned cancellation.
func (s *JobService) CancelRun(ctx context.Context, environmentID, jobID, runID string) (st.Run, error) {
	_, err := s.runs.Get(ctx, environmentID, jobID, runID)
	if environmentID != "0" && errors.Is(err, runs.ErrRunNotFound) {
		return s.mutateAgentRunInternal(ctx, environmentID, jobID, runID, "cancel")
	}
	if err != nil {
		return st.Run{}, err
	}
	return s.runs.Cancel(ctx, environmentID, jobID, runID)
}

func (s *JobService) mutateAgentRunInternal(ctx context.Context, environmentID, jobID, runID, action string) (st.Run, error) {
	environment, err := s.environment.GetEnvironmentByID(ctx, environmentID)
	if err != nil {
		return st.Run{}, err
	}
	if !environment.Enabled {
		return st.Run{}, errors.New("environment is disabled")
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/" + action
	var run st.Run
	// This is an explicit operator action. Never replay a lost mutation response.
	if proxyJSONRequestErr := s.environment.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path, nil, &run); proxyJSONRequestErr != nil {
		return st.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return st.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	run.EnvironmentID = environmentID
	if run.ActivityID != "" {
		run.ActivityEnvironmentID = environmentID
	}
	return run, nil
}
