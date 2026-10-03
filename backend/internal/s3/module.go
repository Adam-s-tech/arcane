// Package s3 owns reusable S3-compatible backup destinations and their HTTP surface.
package s3

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/handlerutil"
)

type Module struct {
	service                *S3DestinationService
	syncRemoteDestinations func(context.Context) error
}

func New(service *S3DestinationService, syncRemoteDestinations func(context.Context) error) *Module {
	return &Module{service: service, syncRemoteDestinations: syncRemoteDestinations}
}

func (m *Module) Service() *S3DestinationService {
	if m == nil {
		return nil
	}
	return m.service
}

func (m *Module) RegisterRoutes(api huma.API) {
	if m == nil {
		RegisterS3Destinations(api, nil, nil)
		return
	}
	RegisterS3Destinations(api, m.service, m.syncRemoteDestinations)
}

func RegisterS3Destinations(api huma.API, service *S3DestinationService, syncRemoteDestinations func(context.Context) error) {
	handler := &s3DestinationHandlerInternal{service: service, syncRemoteDestinations: syncRemoteDestinations}

	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"list-s3-destinations",
			http.MethodGet,
			s3DestinationPathInternal,
			"List S3 destinations",
			"List saved S3-compatible backup destinations with search, sorting, and pagination",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsList, handler.listInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"list-all-s3-destinations",
			http.MethodGet,
			s3DestinationPathInternal+"/options",
			"List all S3 destination options",
			"List saved S3-compatible destinations for backup configuration selectors",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsList, handler.listAllInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"sync-s3-destinations",
			http.MethodPost,
			s3DestinationPathInternal+"/sync",
			"Sync S3 destinations",
			"Synchronize manager-owned S3 destinations to an agent",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsSync, handler.syncInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation("get-s3-destination", http.MethodGet, s3DestinationPathInternal+"/{id}", "Get S3 destination", "", s3DestinationTagInternal),
		authz.PermS3DestinationsRead, handler.getInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation("create-s3-destination", http.MethodPost, s3DestinationPathInternal, "Create S3 destination", "", s3DestinationTagInternal),
		authz.PermS3DestinationsCreate, handler.createInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation("update-s3-destination", http.MethodPut, s3DestinationPathInternal+"/{id}", "Update S3 destination", "", s3DestinationTagInternal),
		authz.PermS3DestinationsUpdate, handler.updateInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"test-s3-destination-configuration",
			http.MethodPost,
			s3DestinationPathInternal+"/test",
			"Test unsaved S3 destination configuration",
			"Verify upload, download, and delete access before saving an S3 destination",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsTest, handler.testConfigurationInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"test-s3-destination",
			http.MethodPost,
			s3DestinationPathInternal+"/{id}/test",
			"Test S3 destination",
			"Verify upload, download, and delete access using the saved or supplied S3 destination configuration",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsTest, handler.testInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation(
			"get-s3-destination-usage",
			http.MethodGet,
			s3DestinationPathInternal+"/{id}/in-use",
			"Check S3 destination references",
			"Report whether backup records, policies, or settings on this environment still reference the destination",
			s3DestinationTagInternal,
		),
		authz.PermS3DestinationsRead, handler.inUseInternal)
	handlerutil.RegisterSecured(api,
		handlerutil.Operation("delete-s3-destination", http.MethodDelete, s3DestinationPathInternal+"/{id}", "Delete S3 destination", "", s3DestinationTagInternal),
		authz.PermS3DestinationsDelete, handler.deleteInternal)
}
