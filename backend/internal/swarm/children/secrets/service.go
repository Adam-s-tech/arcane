package secrets

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

// Service manages swarm secrets.
type Service struct {
	dockerClient  func(ctx context.Context) (*client.Client, error)
	ensureManager func(ctx context.Context) error
}

func NewService(dockerClient func(ctx context.Context) (*client.Client, error), ensureManager func(ctx context.Context) error) *Service {
	return &Service{dockerClient: dockerClient, ensureManager: ensureManager}
}

func (s *Service) ListSecrets(ctx context.Context) ([]swarmtypes.SecretSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	secretsResult, err := dockerClient.SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list swarm secrets: %w", err)
	}

	items := make([]swarmtypes.SecretSummary, 0, len(secretsResult.Items))
	for _, secret := range secretsResult.Items {
		items = append(items, swarmtypes.NewSecretSummary(secret))
	}
	return items, nil
}

func (s *Service) GetSecret(ctx context.Context, secretID string) (*swarmtypes.SecretSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	secretResult, err := dockerClient.SecretInspect(ctx, secretID, client.SecretInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect swarm secret: %w", err)
	}

	return new(swarmtypes.NewSecretSummary(secretResult.Secret)), nil
}

func (s *Service) CreateSecret(ctx context.Context, req swarmtypes.SecretCreateRequest) (*swarmtypes.SecretSummary, error) {
	if err := s.ensureManager(ctx); err != nil {
		return nil, err
	}

	spec, err := decodeSecretSpecInternal(req.Spec)
	if err != nil {
		return nil, err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}

	createResult, err := dockerClient.SecretCreate(ctx, client.SecretCreateOptions{Spec: spec})
	if err != nil {
		return nil, fmt.Errorf("failed to create swarm secret: %w", err)
	}

	return s.GetSecret(ctx, createResult.ID)
}

func (s *Service) RemoveSecret(ctx context.Context, secretID string) error {
	if err := s.ensureManager(ctx); err != nil {
		return err
	}

	dockerClient, err := s.dockerClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Docker: %w", err)
	}

	if _, secretRemoveErr := dockerClient.SecretRemove(ctx, secretID, client.SecretRemoveOptions{}); secretRemoveErr != nil {
		return fmt.Errorf("failed to remove swarm secret: %w", secretRemoveErr)
	}

	return nil
}

func decodeSecretSpecInternal(raw stdjson.RawMessage) (swarm.SecretSpec, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return swarm.SecretSpec{}, errors.New("secret spec is required")
	}

	var spec swarm.SecretSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return swarm.SecretSpec{}, fmt.Errorf("failed to parse secret spec: %w", err)
	}

	if strings.TrimSpace(spec.Name) == "" {
		return swarm.SecretSpec{}, errors.New("secret spec name is required")
	}

	return spec, nil
}
