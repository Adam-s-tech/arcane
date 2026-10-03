// Package stats owns container resource sampling, resource sorting and the
// live stats stream.
package stats

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	containertypes "github.com/getarcaneapp/arcane/types/v2/container"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/samber/hot"
	"go.getarcane.app/kit/pkg"
	"go.getarcane.app/streams/stats"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/timeouts"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/pagination"
)

const (
	ContainerResourceSampleTTL          = 5 * time.Second
	ContainerResourceSampleCacheSize    = 4096
	ContainerResourceCollectConcurrency = 16
	ContainerResourceCollectTimeout     = 3 * time.Second
)

// Service collects container stats through the parent's Docker client.
type Service struct {
	getClient     func(context.Context) (*client.Client, error)
	dockerHost    func() string
	history       stats.Store
	cache         *hot.HotCache[string, containertypes.ResourceSample]
	flight        singleflight.Group
	sampleTimeout time.Duration
	batchTimeout  time.Duration
}

// New builds a stats service. Non-positive timeouts fall back to defaults.
func New(getClient func(context.Context) (*client.Client, error), dockerHost func() string, cacheTTL, sampleTimeout, batchTimeout time.Duration) *Service {
	if sampleTimeout <= 0 {
		sampleTimeout = ContainerResourceCollectTimeout
	}
	if batchTimeout <= 0 {
		batchTimeout = timeouts.DefaultDockerAPI
	}
	return &Service{
		getClient:  getClient,
		dockerHost: dockerHost,
		cache: hot.NewHotCache[string, containertypes.ResourceSample](hot.LRU, ContainerResourceSampleCacheSize).
			WithTTL(cacheTTL).
			WithJanitor().
			Build(),
		sampleTimeout: sampleTimeout,
		batchTimeout:  batchTimeout,
	}
}

// Stream decodes the Docker stats stream and forwards payloads with history.
func (s *Service) Stream(ctx context.Context, containerID string, statsChan chan<- any) error {
	dockerClient, err := s.getClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Docker: %w", err)
	}

	statsResponse, err := dockerClient.ContainerStats(ctx, containerID, client.ContainerStatsOptions{Stream: true})
	if err != nil {
		return fmt.Errorf("failed to start stats stream: %w", err)
	}
	defer func() { _ = statsResponse.Body.Close() }()

	decoder := jsontext.NewDecoder(statsResponse.Body)
	historySent := false

	for {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return cancellationErr
		}

		var statsData container.StatsResponse
		if unmarshalDecodeErr := json.UnmarshalDecode(decoder, &statsData); unmarshalDecodeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(unmarshalDecodeErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("failed to decode stats: %w", unmarshalDecodeErr)
		}

		recordedAt := statsData.Read
		if recordedAt.IsZero() {
			recordedAt = time.Now()
		}

		payload := stats.StatsStreamPayload{
			StatsResponse:        statsData,
			CurrentHistorySample: stats.BuildSample(statsData),
		}
		payload.StatsHistory = s.history.Record(
			containerID,
			payload.CurrentHistorySample,
			!historySent,
			recordedAt,
		)
		historySent = true

		select {
		case statsChan <- payload:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Collect fetches one sample per running container. Individual failures leave
// the container unsampled; a batch timeout or cancellation fails the request
// so partial lists never sort as global.
func (s *Service) Collect(ctx context.Context, items []containertypes.Summary) (map[string]*containertypes.ResourceSample, error) {
	batchCtx, cancel := context.WithTimeout(ctx, s.batchTimeout)
	defer cancel()

	g, groupCtx := errgroup.WithContext(batchCtx)
	g.SetLimit(ContainerResourceCollectConcurrency)

	samples := make(map[string]*containertypes.ResourceSample, len(items))
	var mu sync.Mutex

	for i := range items {
		if items[i].State != string(container.StateRunning) {
			continue
		}
		id := items[i].ID
		g.Go(func() error {
			sample, err := s.fetchInternal(groupCtx, id)
			if err != nil {
				if groupCtx.Err() != nil {
					return groupCtx.Err()
				}
				slog.WarnContext(groupCtx, "Failed to collect container resource sample", "container_id", id, "error", err)
				return nil
			}
			mu.Lock()
			samples[id] = sample
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("failed to collect container resource samples: %w", err)
	}
	return samples, nil
}

// cacheKeyInternal isolates cache and coalescing entries by Docker daemon and
// container ID.
func (s *Service) cacheKeyInternal(containerID string) string {
	return s.dockerHost() + "\x00" + containerID
}

// fetchInternal returns a cached sample when fresh and coalesces concurrent
// fetches for the same container.
func (s *Service) fetchInternal(ctx context.Context, containerID string) (*containertypes.ResourceSample, error) {
	key := s.cacheKeyInternal(containerID)
	if sample, ok, _ := s.cache.Get(key); ok {
		return &sample, nil
	}

	ch := s.flight.DoChan(key, func() (any, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.sampleTimeout)
		defer cancel()
		return s.sampleInternal(flightCtx, containerID)
	})

	fetchCtx, cancel := context.WithTimeout(ctx, s.sampleTimeout)
	defer cancel()

	select {
	case <-fetchCtx.Done():
		return nil, fetchCtx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		sample, ok := res.Val.(*containertypes.ResourceSample)
		if !ok {
			return nil, errors.New("resource sample flight returned unexpected type")
		}
		return sample, nil
	}
}

// sampleInternal performs the non-streaming Docker stats call.
// IncludePreviousSample gives the CPU delta a valid previous sample.
func (s *Service) sampleInternal(ctx context.Context, containerID string) (*containertypes.ResourceSample, error) {
	dockerClient, err := s.getClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	statsResponse, err := dockerClient.ContainerStats(ctx, containerID, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: true})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch container stats: %w", err)
	}
	defer func() { _ = statsResponse.Body.Close() }()

	var statsData container.StatsResponse
	if unmarshalDecodeErr := json.UnmarshalDecode(jsontext.NewDecoder(statsResponse.Body), &statsData); unmarshalDecodeErr != nil {
		return nil, fmt.Errorf("failed to decode container stats: %w", unmarshalDecodeErr)
	}

	built := stats.BuildSample(statsData)
	sample := &containertypes.ResourceSample{
		CPUPercent:       float64(built.CPUTenths) / 10,
		MemoryUsageBytes: built.MemoryUsageBytes,
		MemoryLimitBytes: statsData.MemoryStats.Limit,
		SampleTime:       statsData.Read,
	}
	if sample.SampleTime.IsZero() {
		sample.SampleTime = time.Now()
	}

	s.cache.Set(s.cacheKeyInternal(containerID), *sample)
	return sample, nil
}

// ContainerResourceSampleSort orders summaries by their collected sample
// value, keeping unsampled containers last in both directions and breaking
// ties by name, then ID. Valid zero values sort as zero, not as unavailable.
func ContainerResourceSampleSort(sort string, descending bool) pagination.SortOption[containertypes.Summary] {
	value := kit.Ternary(
		sort == containertypes.SortCPUUsage,
		func(sample *containertypes.ResourceSample) float64 { return sample.CPUPercent },
		func(sample *containertypes.ResourceSample) float64 { return float64(sample.MemoryUsageBytes) },
	)

	tieBreak := func(a, b containertypes.Summary) int {
		if c := CompareContainerNamesForSort(a, b); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	}

	return func(a, b containertypes.Summary) int {
		if a.ResourceSample == nil && b.ResourceSample == nil {
			return tieBreak(a, b)
		}
		if a.ResourceSample == nil {
			return 1
		}
		if b.ResourceSample == nil {
			return -1
		}

		valueA, valueB := value(a.ResourceSample), value(b.ResourceSample)
		if valueA == valueB {
			return tieBreak(a, b)
		}
		less := valueA < valueB
		if descending {
			less = !less
		}
		return kit.Ternary(less, -1, 1)
	}
}

func CompareContainerNamesForSort(a, b containertypes.Summary) int {
	nameA, nameB := "", ""
	if len(a.Names) > 0 {
		nameA = a.Names[0]
	}
	if len(b.Names) > 0 {
		nameB = b.Names[0]
	}
	return strings.Compare(nameA, nameB)
}

// ContainerResourceSortPermissionDenied enforces the extra containers:read
// requirement on resource-sorted list requests.
func ContainerResourceSortPermissionDenied(ps *authz.PermissionSet, envID, sort string) bool {
	if !containertypes.IsResourceSort(sort) {
		return false
	}
	return !ps.Allows(authz.PermContainersRead, envID)
}
