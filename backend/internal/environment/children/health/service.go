// Package health probes environment connectivity over direct HTTP, edge
// tunnels or the local Docker socket.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/getarcaneapp/arcane/backend/v2/internal/docker"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/edge"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/httpx"
)

// Service runs connectivity probes; the parent persists the resulting status.
type Service struct {
	httpClient    *http.Client
	dockerService *docker.DockerClientService
}

func New(httpClient *http.Client, dockerService *docker.DockerClientService) *Service {
	return &Service{httpClient: httpClient, dockerService: dockerService}
}

// Probe reports "online", "offline" or "error" for one environment. A custom
// API URL always probes directly over HTTP.
func (s *Service) Probe(ctx context.Context, id, apiURL string, isEdge, customURL bool) (string, error) {
	if id == "0" && !customURL {
		return s.probeLocalDockerInternal(ctx)
	}
	if isEdge && !customURL {
		return probeEdgeInternal(ctx, id)
	}

	healthURL, err := buildEndpointURLInternal(apiURL, "/api/health")
	if err != nil {
		return "offline", fmt.Errorf("invalid environment API URL: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, http.NoBody)
	if err != nil {
		return "offline", fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "offline", fmt.Errorf("connection failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return "online", nil
	}
	return "error", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// probeEdgeInternal checks an edge agent through its tunnel. The probe itself
// is standing demand for the tunnel, which keeps idle poll-mode tunnels open
// across check cycles instead of letting them flap between checks.
func probeEdgeInternal(ctx context.Context, id string) (string, error) {
	edge.TouchTunnelDemand(id, edge.DefaultTunnelDemandTTL)
	if !edge.HasActiveTunnel(id) {
		if _, ok := edge.RequestTunnelAndWait(ctx, id, edge.DefaultTunnelDemandTTL, edge.DefaultTunnelAcquireTimeout()).Get(); !ok {
			return "offline", errors.New("edge agent is not connected")
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	statusCode, _, err := edge.DoRequest(reqCtx, id, http.MethodGet, "/api/health", nil)
	if err != nil {
		return "offline", fmt.Errorf("health check via tunnel failed: %w", err)
	}
	if statusCode == http.StatusOK {
		return "online", nil
	}
	return "error", fmt.Errorf("unexpected status code: %d", statusCode)
}

func (s *Service) probeLocalDockerInternal(ctx context.Context) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dockerClient, err := s.dockerService.GetClient(ctx)
	if err != nil {
		return "offline", fmt.Errorf("failed to connect to Docker: %w", err)
	}
	if _, pingErr := dockerClient.Ping(reqCtx, client.PingOptions{}); pingErr != nil {
		return "offline", fmt.Errorf("docker ping failed: %w", pingErr)
	}
	return "online", nil
}

func buildEndpointURLInternal(apiURL, endpointPath string) (string, error) {
	baseURL, err := httpx.NormalizeBaseURL(apiURL)
	if err != nil {
		return "", err
	}

	return strings.TrimRight(baseURL, "/") + endpointPath, nil
}
