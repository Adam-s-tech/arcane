package configs

import (
	"context"
	stdjson "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	swarmtypes "github.com/getarcaneapp/arcane/types/v2/swarm"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Service manages swarm configs.
type Service struct {
	dockerClient  func(ctx context.Context) (*client.Client, error)
	ensureManager func(ctx context.Context) error
}

func NewService(dockerClient func(ctx context.Context) (*client.Client, error), ensureManager func(ctx context.Context) error) *Service {
	return &Service{dockerClient: dockerClient, ensureManager: ensureManager}
}

func (s *Service) ListConfigs(ctx context.Context) ([]swarmtypes.ConfigSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	configsResult, err := dockerClient.ConfigList(ctx, client.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list swarm configs: %w", err)
	}

	items := make([]swarmtypes.ConfigSummary, 0, len(configsResult.Items))
	for _, cfg := range configsResult.Items {
		items = append(items, swarmtypes.NewConfigSummary(cfg))
	}
	return items, nil
}

func (s *Service) GetConfig(ctx context.Context, configID string) (*swarmtypes.ConfigSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	cfgResult, err := dockerClient.ConfigInspect(ctx, configID, client.ConfigInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect swarm config: %w", err)
	}

	return new(swarmtypes.NewConfigSummary(cfgResult.Config)), nil
}

func (s *Service) CreateConfig(ctx context.Context, req swarmtypes.ConfigCreateRequest) (*swarmtypes.ConfigSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	spec, err := decodeConfigSpecInternal(req.Spec)
	if err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	createResult, err := dockerClient.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: spec})
	if err != nil {
		return nil, fmt.Errorf("failed to create swarm config: %w", err)
	}

	return s.GetConfig(ctx, createResult.ID)
}

func (s *Service) RemoveConfig(ctx context.Context, configID string) error {
	if err := s.ensureManager(ctx); err != nil {
		return err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Docker: %w", err)
	}

	if _, configRemoveErr := dockerClient.ConfigRemove(ctx, configID, client.ConfigRemoveOptions{}); configRemoveErr != nil {
		return fmt.Errorf("failed to remove swarm config: %w", configRemoveErr)
	}

	return nil
}

func decodeConfigSpecInternal(raw stdjson.RawMessage) (swarm.ConfigSpec, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return swarm.ConfigSpec{}, errors.New("config spec is required")
	}

	var spec swarm.ConfigSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return swarm.ConfigSpec{}, fmt.Errorf("failed to parse config spec: %w", err)
	}

	if strings.TrimSpace(spec.Name) == "" {
		return swarm.ConfigSpec{}, errors.New("config spec name is required")
	}

	return spec, nil
}
