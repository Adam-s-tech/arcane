package system

import (
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/getarcaneapp/arcane/types/v2/base"
	"github.com/getarcaneapp/arcane/types/v2/container"
	"github.com/getarcaneapp/arcane/types/v2/dockerinfo"
	"github.com/getarcaneapp/arcane/types/v2/system"
	dockersystem "github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"go.getarcane.app/docker/convert"
	"go.getarcane.app/docker/convert/types"
	"go.getarcane.app/sys/cgroup"

	"github.com/getarcaneapp/arcane/backend/v2/internal/docker"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/handlerutil"
)

// SystemHandler handles system management endpoints.
type SystemHandler struct {
	dockerService *docker.DockerClientService
	systemService *SystemService
	appCtx        context.Context
}

type SystemHealthInput struct {
	EnvironmentID string `path:"id" doc:"Environment ID"`
}

type GetDockerInfoInput struct {
	EnvironmentID string `path:"id" doc:"Environment ID"`
}

type GetDockerInfoOutput struct {
	Body dockerinfo.Info
}

type StartAllContainersInput struct {
	EnvironmentID string `path:"id" doc:"Environment ID"`
}

type StartAllStoppedContainersInput struct {
	EnvironmentID string `path:"id" doc:"Environment ID"`
}

type StopAllContainersInput struct {
	EnvironmentID string `path:"id" doc:"Environment ID"`
}

type ConvertDockerRunInput struct {
	EnvironmentID string                         `path:"id" doc:"Environment ID"`
	Body          system.ConvertDockerRunRequest `doc:"Docker run command"`
}

type ConvertDockerRunOutput struct {
	Body system.ConvertDockerRunResponse
}

func NewHandler(dockerService *docker.DockerClientService, systemService *SystemService, appCtx context.Context) *SystemHandler {
	return &SystemHandler{dockerService: dockerService, systemService: systemService, appCtx: appCtx}
}

// Health checks if the Docker daemon is responsive.
func (h *SystemHandler) Health(ctx context.Context, input *SystemHealthInput) (*struct{}, error) {
	dockerClient, err := h.dockerService.GetClient(ctx)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("Failed to connect to Docker: " + err.Error())
	}

	_, err = dockerClient.Ping(ctx, client.PingOptions{})
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("Docker is not responsive: " + err.Error())
	}

	return nil, nil
}

// GetDockerInfo returns Docker daemon version and system information.
func (h *SystemHandler) GetDockerInfo(ctx context.Context, input *GetDockerInfoInput) (*GetDockerInfoOutput, error) {
	dockerClient, err := h.dockerService.GetClient(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to connect to Docker: " + err.Error())
	}

	version, err := dockerClient.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to get Docker version: " + err.Error())
	}

	infoResult, err := dockerClient.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to get Docker info: " + err.Error())
	}
	info := infoResult.Info

	cpuCount := info.NCPU
	memTotal := info.MemTotal

	// Apply cgroup limits only when running outside Docker (e.g. in LXC).
	// In Docker, --cpus/--memory are artificial operator constraints that
	// should not cap the host totals shown in the dashboard. The Docker
	// daemon's NCPU/MemTotal already reflect the real host. In LXC the
	// daemon may report the physical machine's full capacity while the
	// LXC guest has a smaller cgroup budget — apply those limits so the
	// dashboard shows what Arcane's host actually has available.
	if !cgroup.IsDockerContainer() {
		if cgroupLimits, detectLimitsErr := cgroup.DetectLimits(); detectLimitsErr == nil {
			if limit := cgroupLimits.MemoryLimit; limit > 0 {
				limitInt := limit
				if memTotal == 0 || limitInt < memTotal {
					memTotal = limitInt
				}
			}
			if cgroupLimits.CPUCount > 0 && (cpuCount == 0 || cgroupLimits.CPUCount < cpuCount) {
				cpuCount = cgroupLimits.CPUCount
			}
		}
	}

	info.NCPU = cpuCount
	info.MemTotal = memTotal

	gitCommit, goVersion, buildTime := extractVersionDetailsFromComponents(version.Components)

	return &GetDockerInfoOutput{
		Body: dockerinfo.Info{
			Success:    true,
			APIVersion: version.APIVersion,
			GitCommit:  gitCommit,
			GoVersion:  goVersion,
			Os:         version.Os,
			Arch:       version.Arch,
			BuildTime:  buildTime,
			Info:       info,
		},
	}, nil
}

func extractVersionDetailsFromComponents(components []dockersystem.ComponentVersion) (gitCommit, goVersion, buildTime string) {
	for _, component := range components {
		if component.Details == nil {
			continue
		}

		for key, value := range component.Details {
			switch strings.ToLower(key) {
			case "gitcommit":
				gitCommit = cmp.Or(gitCommit, value)
			case "goversion":
				goVersion = cmp.Or(goVersion, value)
			case "buildtime":
				buildTime = cmp.Or(buildTime, value)
			}
		}
	}

	return gitCommit, goVersion, buildTime
}

// StartAllContainers starts all Docker containers.
func (h *SystemHandler) StartAllContainers(ctx context.Context, input *StartAllContainersInput) (*handlerutil.Out[container.ActionResult], error) {
	runtimeCtx := utils.ActivityRuntimeContext(ctx, h.appCtx)
	result, err := h.systemService.StartAllContainers(runtimeCtx, input.EnvironmentID)
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to start containers: " + err.Error())
	}

	return &handlerutil.Out[container.ActionResult]{
		Body: base.ApiResponse[container.ActionResult]{
			Success: true,
			Data:    *result,
		},
	}, nil
}

// StartAllStoppedContainers starts all stopped Docker containers.
func (h *SystemHandler) StartAllStoppedContainers(ctx context.Context, input *StartAllStoppedContainersInput) (*handlerutil.Out[container.ActionResult], error) {
	runtimeCtx := utils.ActivityRuntimeContext(ctx, h.appCtx)
	result, err := h.systemService.StartAllStoppedContainers(runtimeCtx, input.EnvironmentID)
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to start stopped containers: " + err.Error())
	}

	return &handlerutil.Out[container.ActionResult]{
		Body: base.ApiResponse[container.ActionResult]{
			Success: true,
			Data:    *result,
		},
	}, nil
}

// StopAllContainers stops all running Docker containers.
func (h *SystemHandler) StopAllContainers(ctx context.Context, input *StopAllContainersInput) (*handlerutil.Out[container.ActionResult], error) {
	runtimeCtx := utils.ActivityRuntimeContext(ctx, h.appCtx)
	result, err := h.systemService.StopAllContainers(runtimeCtx, input.EnvironmentID)
	if err != nil {
		return nil, huma.Error500InternalServerError("Failed to stop containers: " + err.Error())
	}

	return &handlerutil.Out[container.ActionResult]{
		Body: base.ApiResponse[container.ActionResult]{
			Success: true,
			Data:    *result,
		},
	}, nil
}

// ConvertDockerRun converts a docker run command to docker-compose format.
func (h *SystemHandler) ConvertDockerRun(ctx context.Context, input *ConvertDockerRunInput) (*ConvertDockerRunOutput, error) {
	result, err := convert.Convert(input.Body.DockerRunCommand, types.Options{})
	if err != nil {
		if errors.Is(err, types.ErrParse) {
			return nil, huma.Error400BadRequest("Failed to parse docker run command. Please check the syntax.")
		}
		return nil, huma.Error500InternalServerError("Failed to convert to Docker Compose format.")
	}

	serviceName := ""
	if len(result.Services) > 0 {
		serviceName = result.Services[0].Name
	}

	return &ConvertDockerRunOutput{
		Body: system.ConvertDockerRunResponse{
			Success:       true,
			DockerCompose: string(result.YAML),
			EnvVars:       strings.TrimSuffix(string(result.EnvFile), "\n"),
			ServiceName:   serviceName,
		},
	}, nil
}
