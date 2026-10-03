package tags

import (
	"testing"

	"github.com/getarcaneapp/arcane/types/v2/imageupdate"
	"github.com/stretchr/testify/require"
	"go.getarcane.app/updater"
)

func TestContainerAggregationPreservesImageResultInternal(t *testing.T) {
	original := &imageupdate.Response{HasUpdate: false, UpdateType: string(updater.UpdateTypeDigest), CurrentVersion: "3.20.0", LatestVersion: "3.20.0"}
	results := map[string]*imageupdate.Response{"alpine:3.20.0": original}
	scoped := &imageupdate.Response{ImageRef: "docker.io/library/alpine:3.20.0", HasUpdate: true, UpdateType: string(updater.UpdateTypeTag), CurrentVersion: "3.20.0", LatestVersion: "3.20.1"}
	AttachContainerUpdates(results, map[string]*imageupdate.Response{"tagged": scoped})
	require.True(t, original.HasUpdate)
	require.Equal(t, string(updater.UpdateTypeTag), original.UpdateType)
	require.Equal(t, "3.20.1", original.LatestVersion)
	require.Same(t, scoped, original.ContainerUpdates["tagged"])
	require.NotNil(t, original.ImageUpdate)
	require.False(t, original.ImageUpdate.HasUpdate, "untagged siblings must keep the original digest result")
	require.Equal(t, string(updater.UpdateTypeDigest), original.ImageUpdate.UpdateType)
	require.Nil(t, original.ImageUpdate.ContainerUpdates)
	require.Nil(t, original.ImageUpdate.ImageUpdate, "snapshot must not create recursive JSON")
}
