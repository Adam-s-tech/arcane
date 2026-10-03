package execution

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/updater"
	"github.com/stretchr/testify/require"

	francistest "github.com/getarcaneapp/arcane/backend/v2/pkg/utils/francis/testing"
)

// testDependenciesInternal wires delivery to stand-ins: freezing fails as it
// does without Docker.
func testDependenciesInternal() Dependencies {
	return Dependencies{
		Logger:        slog.Default,
		AcquireUpdate: func(ctx context.Context) (context.Context, func(), error) { return ctx, func() {}, nil },
		UpdateBusy:    errors.New("busy"),
		TrackActivity: func(ctx context.Context, _ string) context.Context { return ctx },
		RunUpdate: func(context.Context, string, string) (*updater.Result, error) {
			return nil, errors.New("unexpected update run")
		},
		FinishUpdate: func(context.Context, string, *updater.Result, error) {},
		FreezeContainer: func(context.Context, string) (*updater.FrozenUpdateTarget, error) {
			return nil, errors.New("docker service unavailable")
		},
		ConfirmTarget: func(context.Context, updater.FrozenUpdateTarget) (bool, bool, error) { return false, false, nil },
		WithFrozenTarget: func(ctx context.Context, _ *updater.FrozenUpdateTarget, _ func() error) context.Context {
			return ctx
		},
	}
}

func TestService_RepairsAcceptedIntentBeforeDispatch(t *testing.T) {
	svc := NewService(testDependenciesInternal())
	runtime := francistest.New(t)
	require.NoError(t, svc.RegisterActors(runtime))
	francistest.Start(t, runtime)
	command := updater.SingleUpdateCommand{ContainerID: "missing", ActivityID: "accepted"}
	require.NoError(
		t,
		runtime.Service().SetState(
			t.Context(),
			StateType,
			command.ActivityID,
			updater.SingleUpdateState{
				Command: command,
				Status:  "queued",
			},
			nil,
		),
	)
	require.NoError(t, svc.Start(t.Context()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		require.NoError(t, svc.Stop(ctx))
	})
	require.Eventually(t, func() bool {
		var state updater.SingleUpdateState
		return runtime.Service().GetState(t.Context(), StateType, command.ActivityID, &state) == nil && state.Status == "needs_attention"
	}, 3*time.Second, 10*time.Millisecond)
}
