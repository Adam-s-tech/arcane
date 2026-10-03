package browse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/containerregistry"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getarcaneapp/arcane/backend/v2/internal/common"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/pagination"
)

// newTestRegistryInternal starts an in-memory OCI registry and returns its
// host as "localhost:PORT" so go-containerregistry resolves it over plain HTTP.
func newTestRegistryInternal(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	return "localhost:" + serverURL.Port()
}

// testRegistriesInternal is an in-memory registry store keyed by ID.
type testRegistriesInternal map[string]containerregistry.Credential

func (r testRegistriesInternal) add(registryURL, username, token string) string {
	id := fmt.Sprintf("registry-%d", len(r)+1)
	r[id] = containerregistry.Credential{URL: registryURL, Username: username, Token: token, Enabled: true}
	return id
}

func newTestServiceInternal(registries testRegistriesInternal) *Service {
	return NewService(
		func(_ context.Context, id string) (containerregistry.ContainerRegistry, *containerregistry.Credential, error) {
			credential, ok := registries[id]
			if !ok {
				return containerregistry.ContainerRegistry{}, nil, common.Classify(common.ErrNotFound, errors.New("registry not found"))
			}
			registryRecord := containerregistry.ContainerRegistry{ID: id, URL: credential.URL}
			if credential.Username == "" {
				return registryRecord, nil, nil
			}
			return registryRecord, &credential, nil
		},
		func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, time.Minute)
		},
		nil,
	)
}

func platformImageInternal(t *testing.T, platform v1.Platform, created time.Time) v1.Image {
	t.Helper()

	img, err := random.Image(128, 2)
	require.NoError(t, err)
	cfg, err := img.ConfigFile()
	require.NoError(t, err)
	cfg = cfg.DeepCopy()
	cfg.OS = platform.OS
	cfg.Architecture = platform.Architecture
	cfg.Variant = platform.Variant
	cfg.Created = v1.Time{Time: created}
	img, err = mutate.ConfigFile(img, cfg)
	require.NoError(t, err)
	return img
}

func mustParseReferenceInternal(t *testing.T, imageRef string) name.Reference {
	t.Helper()
	ref, err := name.ParseReference(imageRef)
	require.NoError(t, err)
	return ref
}

func writeImageInternal(t *testing.T, imageRef string, img v1.Image, options ...remote.Option) {
	t.Helper()
	require.NoError(t, remote.Write(mustParseReferenceInternal(t, imageRef), img, options...))
}

func imageSizeInternal(t *testing.T, img v1.Image) int64 {
	t.Helper()
	digest, err := img.Digest()
	require.NoError(t, err)
	platform, err := tagPlatformInternal(img, nil, digest)
	require.NoError(t, err)
	return platform.Size
}

func browseParamsInternal(search string) pagination.QueryParams {
	return pagination.QueryParams{
		Search: search,
		Start:  0, Limit: 20,
	}
}

func TestService_ListRepositoriesInternal(t *testing.T) {
	host := newTestRegistryInternal(t)
	img := platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "amd64"}, time.Now())
	writeImageInternal(t, host+"/team/api:1.0", img)
	writeImageInternal(t, host+"/team/web:1.0", img)
	writeImageInternal(t, host+"/other/tool:1.0", img)

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	ctx := t.Context()

	id := registries.add(host, "", "")
	repositories, page, err := svc.ListRepositories(ctx, id, browseParamsInternal(""))
	require.NoError(t, err)
	assert.Equal(t, []string{"other/tool", "team/api", "team/web"}, repositoryNamesInternal(repositories))
	assert.EqualValues(t, 3, page.TotalItems)

	repositories, _, err = svc.ListRepositories(ctx, id, browseParamsInternal("web"))
	require.NoError(t, err)
	assert.Equal(t, []string{"team/web"}, repositoryNamesInternal(repositories))

	namespacedID := registries.add("http://"+host+"/team/", "", "")
	repositories, _, err = svc.ListRepositories(ctx, namespacedID, browseParamsInternal(""))
	require.NoError(t, err)
	assert.Equal(t, []string{"team/api", "team/web"}, repositoryNamesInternal(repositories))
}

func TestService_ListRepositoryTagsSingleImageInternal(t *testing.T) {
	host := newTestRegistryInternal(t)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	img := platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, created)
	writeImageInternal(t, host+"/team/api:1.0", img)
	writeImageInternal(t, host+"/team/api:2.0", img)

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	id := registries.add(host, "", "")

	tags, page, err := svc.ListRepositoryTags(t.Context(), id, "team/api", pagination.QueryParams{Start: 0, Limit: 1})
	require.NoError(t, err)
	assert.EqualValues(t, 2, page.TotalItems)
	require.Len(t, tags, 1)

	digest, err := img.Digest()
	require.NoError(t, err)
	size := imageSizeInternal(t, img)

	tag := tags[0]
	assert.Equal(t, "1.0", tag.Name)
	assert.Empty(t, tag.Error)
	assert.Equal(t, digest.String(), tag.Digest)
	assert.Equal(t, size, tag.Size)
	require.NotNil(t, tag.Created)
	assert.True(t, created.Equal(*tag.Created))
	require.Len(t, tag.Platforms, 1)
	assert.Equal(t, "linux", tag.Platforms[0].OS)
	assert.Equal(t, "arm64", tag.Platforms[0].Architecture)
	assert.Equal(t, "v8", tag.Platforms[0].Variant)
}

func TestService_ListRepositoryTagsIndexInternal(t *testing.T) {
	host := newTestRegistryInternal(t)
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amd64Image := platformImageInternal(t, amd64, time.Now())
	arm64Image := platformImageInternal(t, arm64, time.Now())
	attestation, err := random.Image(32, 1)
	require.NoError(t, err)

	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: arm64Image, Platform: &arm64},
		mutate.IndexAddendum{Add: amd64Image, Platform: &amd64},
		mutate.IndexAddendum{Add: attestation, Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"}},
	)
	ref, err := name.ParseReference(host + "/team/api:multi")
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(ref, index))

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	id := registries.add(host, "", "")

	tags, _, err := svc.ListRepositoryTags(t.Context(), id, "team/api", browseParamsInternal(""))
	require.NoError(t, err)
	require.Len(t, tags, 1)

	amd64Size := imageSizeInternal(t, amd64Image)
	arm64Size := imageSizeInternal(t, arm64Image)

	tag := tags[0]
	assert.Empty(t, tag.Error)
	assert.Nil(t, tag.Created)
	assert.Equal(t, amd64Size+arm64Size, tag.Size)
	require.Len(t, tag.Platforms, 2)
	assert.Equal(t, "amd64", tag.Platforms[0].Architecture)
	assert.Equal(t, amd64Size, tag.Platforms[0].Size)
	assert.Equal(t, "arm64", tag.Platforms[1].Architecture)
}

func TestService_DeleteRepositoryTagInternal(t *testing.T) {
	host := newTestRegistryInternal(t)
	deleted := platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "amd64"}, time.Now())
	writeImageInternal(t, host+"/team/api:1.0", deleted)
	writeImageInternal(t, host+"/team/api:2.0", platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "amd64"}, time.Now()))

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	id := registries.add(host, "", "")
	ctx := t.Context()

	digest, err := svc.DeleteRepositoryTag(ctx, id, "team/api", "1.0")
	require.NoError(t, err)
	deletedDigest, err := deleted.Digest()
	require.NoError(t, err)
	assert.Equal(t, deletedDigest.String(), digest)

	_, err = remote.Head(mustParseReferenceInternal(t, host+"/team/api@"+digest))
	require.Error(t, err)
	_, err = remote.Head(mustParseReferenceInternal(t, host+"/team/api:2.0"))
	require.NoError(t, err)
}

func TestService_BrowseUsesStoredCredentialsInternal(t *testing.T) {
	registryHandler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "robot" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		registryHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	host := "localhost:" + serverURL.Port()

	writeImageInternal(t, host+"/private/app:1.0", platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "amd64"}, time.Now()),
		remote.WithAuth(&authn.Basic{Username: "robot", Password: "secret"}))

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	ctx := t.Context()

	id := registries.add(host, "robot", "secret")
	repositories, _, err := svc.ListRepositories(ctx, id, browseParamsInternal(""))
	require.NoError(t, err)
	assert.Equal(t, []string{"private/app"}, repositoryNamesInternal(repositories))

	anonymousID := registries.add(host, "", "")
	_, _, err = svc.ListRepositories(ctx, anonymousID, browseParamsInternal(""))
	assert.ErrorIs(t, err, common.ErrBadRequest)
}

func TestService_BrowseErrorsInternal(t *testing.T) {
	host := newTestRegistryInternal(t)
	writeImageInternal(t, host+"/team/api:1.0", platformImageInternal(t, v1.Platform{OS: "linux", Architecture: "amd64"}, time.Now()))

	registries := testRegistriesInternal{}
	svc := newTestServiceInternal(registries)
	id := registries.add(host, "", "")
	namespacedID := registries.add("http://"+host+"/team/", "", "")

	tests := []struct {
		name    string
		call    func(ctx context.Context) error
		wantErr error
	}{
		{
			name: "unknown registry",
			call: func(ctx context.Context) error {
				_, _, err := svc.ListRepositories(ctx, "missing", browseParamsInternal(""))
				return err
			},
			wantErr: common.ErrNotFound,
		},
		{
			name: "empty repository",
			call: func(ctx context.Context) error {
				_, _, err := svc.ListRepositoryTags(ctx, id, " / ", browseParamsInternal(""))
				return err
			},
			wantErr: common.ErrValidation,
		},
		{
			name: "tags outside registry namespace",
			call: func(ctx context.Context) error {
				_, _, err := svc.ListRepositoryTags(ctx, namespacedID, "other/tool", browseParamsInternal(""))
				return err
			},
			wantErr: common.ErrValidation,
		},
		{
			name: "delete outside registry namespace",
			call: func(ctx context.Context) error {
				_, err := svc.DeleteRepositoryTag(ctx, namespacedID, "other/tool", "1.0")
				return err
			},
			wantErr: common.ErrValidation,
		},
		{
			name: "delete missing tag",
			call: func(ctx context.Context) error {
				_, err := svc.DeleteRepositoryTag(ctx, id, "team/api", "missing")
				return err
			},
			wantErr: common.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.call(t.Context()), tt.wantErr)
		})
	}
}

func repositoryNamesInternal(repositories []containerregistry.Repository) []string {
	names := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		names = append(names, repository.Name)
	}
	return names
}
