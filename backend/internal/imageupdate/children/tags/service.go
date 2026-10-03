package tags

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/containerregistry"
	"github.com/getarcaneapp/arcane/types/v2/imageupdate"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"go.getarcane.app/updater"
	"go.getarcane.app/updater/pkg/utils/tagpolicy"
	"go.getarcane.app/updater/refs"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/getarcaneapp/arcane/backend/v2/internal/docker"
	"github.com/getarcaneapp/arcane/backend/v2/internal/registry"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/ratelimit"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/timeouts"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/imageref"
)

const (
	// tagCheckWorkers bounds concurrent container policy evaluations per scan; the
	// registry limiter still caps concurrent requests per registry host.
	tagCheckWorkers = 10
)

// Service evaluates container tag policies against registry listings.
type Service struct {
	registry *registry.ContainerRegistryService
	docker   *docker.DockerClientService
	settings *settings.SettingsService
	limiter  *ratelimit.RegistryRateLimiter
}

func NewService(
	registryService *registry.ContainerRegistryService,
	dockerService *docker.DockerClientService,
	settingsService *settings.SettingsService,
	limiter *ratelimit.RegistryRateLimiter,
) *Service {
	return &Service{registry: registryService, docker: dockerService, settings: settingsService, limiter: limiter}
}

// Check evaluates the tag policy of each listed container running a wanted
// image. Digest-tracked containers are handed to clearDigestTracked and each
// finished check is handed to save as soon as it completes.
func (s *Service) Check(
	ctx context.Context,
	listed []container.Summary,
	wanted map[string]bool,
	credentials []containerregistry.Credential,
	clearDigestTracked func(context.Context, []string) error,
	save func(context.Context, container.Summary, *imageupdate.Response) error,
) (map[string]*imageupdate.Response, error) {
	results := map[string]*imageupdate.Response{}
	// This engine only reads. Containers excluded from automatic installation
	// (updater label or UI exclusion) are still checked; only the update-check
	// label opts a container out of monitoring, so no Settings provider is wired.
	adapter := newTagRegistryInternal(s.registry, credentials, s.docker, s.settings)
	checkPolicy := updater.LabelPolicy{IsUpdateDisabledFunc: imageref.IsUpdateCheckDisabled}
	engine, err := updater.New(updater.Config{RegistryTagLister: adapter, RegistryDigestResolver: adapter, DockerClientProvider: adapter, LabelPolicy: checkPolicy})
	if err != nil {
		return results, err
	}
	defer func() {
		if closeErr := engine.Close(); closeErr != nil {
			slog.WarnContext(ctx, "close tag checker", "error", closeErr)
		}
	}()
	policy := updater.DefaultLabelPolicy()
	candidates := make([]container.Summary, 0, len(listed))
	var staleDigestContainerIDs []string
	for _, cnt := range listed {
		if !wanted[refs.NormalizeImageUpdateRef(cnt.Image)] || imageref.IsUpdateCheckDisabled(cnt.Labels) {
			continue
		}
		tagPolicy, policyErr := tagpolicy.Resolve(cnt.Image, policy.TagPolicy(cnt.Labels))
		if policyErr == nil && tagPolicy.Strategy == "digest" {
			staleDigestContainerIDs = append(staleDigestContainerIDs, cnt.ID)
			continue
		}
		candidates = append(candidates, cnt)
	}
	if clearErr := clearDigestTracked(ctx, staleDigestContainerIDs); clearErr != nil {
		return results, clearErr
	}

	// Policies are evaluated concurrently; the shared adapter dedupes registry
	// requests across replicas and the registry limiter caps them per host.
	// Each result is persisted as soon as its check completes, so a scan that
	// reaches its deadline keeps every check that finished before it. Saves are
	// serialized and run on the group context: once the scan is cancelled the
	// remaining writes fail instead of replacing stored results with
	// cancellation errors.
	checks := make([]*imageupdate.Response, len(candidates))
	var saveMu sync.Mutex
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(tagCheckWorkers)
	for i, cnt := range candidates {
		g.Go(func() (workerErr error) {
			defer utils.RecoverToError(&workerErr, "container tag check worker", "containerId", cnt.ID)
			checks[i] = s.checkContainerTagInternal(groupCtx, engine, cnt)
			saveMu.Lock()
			defer saveMu.Unlock()
			return save(groupCtx, cnt, checks[i])
		})
	}
	waitErr := g.Wait()
	for i, cnt := range candidates {
		if checks[i] != nil {
			results[cnt.ID] = checks[i]
		}
	}
	return results, waitErr
}

// tagRegistryInternal adapts the registry service for one container tag scan.
// Successful tag listings are shared per repository and digest lookups per
// reference so replicas of the same image cost one registry request, and the
// scan's manager-provided credentials never leak into the service caches.
type tagRegistryInternal struct {
	service     *registry.ContainerRegistryService
	credentials []containerregistry.Credential
	docker      *docker.DockerClientService
	settings    *settings.SettingsService
	tags        *scanRegistryMemoInternal[[]string]
	digests     *scanRegistryMemoInternal[string]
}

func newTagRegistryInternal(
	service *registry.ContainerRegistryService,
	credentials []containerregistry.Credential,
	dockerService *docker.DockerClientService,
	settingsService *settings.SettingsService,
) tagRegistryInternal {
	return tagRegistryInternal{
		service:     service,
		credentials: credentials,
		docker:      dockerService,
		settings:    settingsService,
		tags:        newScanRegistryMemoInternal[[]string](),
		digests:     newScanRegistryMemoInternal[string](),
	}
}

// scanRegistryMemoInternal memoizes successful registry lookups for one scan
// and coalesces concurrent misses for the same key into a single request.
// Failures are not stored: each waiting caller receives the shared error and
// the next caller retries, so a transient fault never sticks for the scan.
type scanRegistryMemoInternal[V any] struct {
	flight singleflight.Group
	mu     sync.Mutex
	values map[string]V
}

func newScanRegistryMemoInternal[V any]() *scanRegistryMemoInternal[V] {
	return &scanRegistryMemoInternal[V]{values: map[string]V{}}
}

func (m *scanRegistryMemoInternal[V]) doInternal(key string, fetch func() (V, error)) (V, error) {
	m.mu.Lock()
	value, ok := m.values[key]
	m.mu.Unlock()
	if ok {
		return value, nil
	}
	shared, err, _ := m.flight.Do(key, func() (any, error) {
		fetched, fetchErr := fetch()
		if fetchErr != nil {
			return nil, fetchErr
		}
		m.mu.Lock()
		m.values[key] = fetched
		m.mu.Unlock()
		return fetched, nil
	})
	var zero V
	if err != nil {
		return zero, err
	}
	value, ok = shared.(V)
	if !ok {
		return zero, errors.New("registry lookup returned an unexpected result type")
	}
	return value, nil
}

func (r tagRegistryInternal) ListTags(ctx context.Context, imageRef string) ([]string, error) {
	if r.service == nil {
		return nil, errors.New("registry service unavailable")
	}
	fetch := func() ([]string, error) { return r.service.ListImageTags(ctx, imageRef, r.credentials) }
	parsed, err := refs.NormalizeReference(imageRef)
	if err != nil || r.tags == nil {
		return fetch()
	}
	tags, err := r.tags.doInternal(parsed.RegistryHost+"/"+parsed.Repository, fetch)
	if err != nil {
		return nil, err
	}
	// Callers may sort or filter the listing in place.
	return slices.Clone(tags), nil
}

func (r tagRegistryInternal) ImageDigest(ctx context.Context, imageRef string) (string, error) {
	if r.service == nil {
		return "", errors.New("registry service unavailable")
	}
	fetch := func() (string, error) {
		timeoutSeconds := 0
		if r.settings != nil {
			timeoutSeconds = r.settings.GetSettingsConfig().RegistryTimeout.AsInt()
		}
		digestCtx, cancel := context.WithTimeout(ctx, timeouts.GetDuration(timeoutSeconds, timeouts.DefaultRegistry))
		defer cancel()
		result, err := r.service.InspectImageDigest(digestCtx, imageRef, r.credentials)
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", errors.New("registry returned no digest")
		}
		return result.Digest, nil
	}
	parsed, err := refs.NormalizeReference(imageRef)
	if err != nil || r.digests == nil {
		return fetch()
	}
	return r.digests.doInternal(parsed.NormalizedRef, fetch)
}

func (s *Service) checkContainerTagInternal(ctx context.Context, engine *updater.Service, cnt container.Summary) *imageupdate.Response {
	start := time.Now()
	result := &imageupdate.Response{CheckTime: time.Now().UTC(), UpdateType: string(updater.UpdateTypeTag), ImageRef: cnt.Image}
	defer func() { result.ResponseTimeMs = int(time.Since(start).Milliseconds()) }()
	parsed, err := refs.NormalizeReference(cnt.Image)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if s.limiter != nil {
		if acquireErr := s.limiter.Acquire(ctx, parsed.RegistryHost); acquireErr != nil {
			result.Error = acquireErr.Error()
			return result
		}
		defer s.limiter.Release(parsed.RegistryHost)
	}
	timeoutSeconds := 0
	if s.settings != nil {
		timeoutSeconds = s.settings.GetSettingsConfig().RegistryTagTimeout.AsInt()
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeouts.GetDuration(timeoutSeconds, timeouts.DefaultRegistryTags))
	defer cancel()
	check, err := engine.CheckContainerUpdate(checkCtx, cnt.ID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.HasUpdate = check.UpdateAvailable
	result.UpdateType = check.UpdateType
	if result.UpdateType == "" {
		result.UpdateType = string(updater.UpdateTypeTag)
	}
	result.CurrentVersion = check.CurrentVersion
	result.CurrentDigest = check.CurrentDigest
	result.LatestDigest = check.TargetDigest
	if check.TargetRef != "" {
		target, normalizeReferenceErr := refs.NormalizeReference(check.TargetRef)
		if normalizeReferenceErr != nil {
			result.Error = normalizeReferenceErr.Error()
			result.HasUpdate = false
			return result
		}
		result.LatestVersion = target.Tag
	}
	return result
}

func (r tagRegistryInternal) DockerClient(ctx context.Context) (*client.Client, error) {
	if r.docker == nil {
		return nil, errors.New("docker service unavailable")
	}
	return r.docker.GetClient(ctx)
}

// AttachContainerUpdates binds each container's policy result to the
// image result sharing its normalized reference. The image-only snapshot is
// taken before the first container result is attached so it stays untouched.
// A container candidate promotes the image result to its version update when
// the image itself has none.
func AttachContainerUpdates(results, containerUpdates map[string]*imageupdate.Response) {
	containerIDsByRef := make(map[string][]string, len(containerUpdates))
	for id, update := range containerUpdates {
		normalized := refs.NormalizeImageUpdateRef(update.ImageRef)
		containerIDsByRef[normalized] = append(containerIDsByRef[normalized], id)
	}
	for _, ids := range containerIDsByRef {
		slices.Sort(ids)
	}
	for imageRef, result := range results {
		for _, id := range containerIDsByRef[refs.NormalizeImageUpdateRef(imageRef)] {
			update := containerUpdates[id]
			if result.ImageUpdate == nil {
				imageOnly := *result
				imageOnly.ContainerUpdates = nil
				result.ImageUpdate = &imageOnly
			}
			if result.ContainerUpdates == nil {
				result.ContainerUpdates = map[string]*imageupdate.Response{}
			}
			result.ContainerUpdates[id] = update
			if !update.HasUpdate {
				continue
			}
			if !result.HasUpdate {
				result.UpdateType = update.UpdateType
				result.CurrentVersion = update.CurrentVersion
				result.LatestVersion = update.LatestVersion
			}
			result.HasUpdate = true
		}
	}
}
