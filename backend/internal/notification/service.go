package notification

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/imageupdate"
	"github.com/getarcaneapp/arcane/types/v2/notification"
	"github.com/getarcaneapp/arcane/types/v2/system"
	"go.getarcane.app/kit/pkg"
	"go.getarcane.app/sys/crypto"
	"golang.org/x/sync/errgroup"

	"github.com/getarcaneapp/arcane/backend/v2/internal/apns"
	"github.com/getarcaneapp/arcane/backend/v2/internal/common"
	"github.com/getarcaneapp/arcane/backend/v2/internal/config"
	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/environment"
	"github.com/getarcaneapp/arcane/backend/v2/internal/event"
	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/internal/notification/children/templates"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/edge"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/httpx"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/notifications"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/validation"
)

var notificationCredentialFieldsByProviderInternal = map[notifications.NotificationProvider][]string{
	notifications.NotificationProviderDiscord:  {"token"},
	notifications.NotificationProviderEmail:    {"smtpPassword"},
	notifications.NotificationProviderTelegram: {"botToken"},
	notifications.NotificationProviderSignal:   {"password", "token"},
	notifications.NotificationProviderSlack:    {"token"},
	notifications.NotificationProviderNtfy:     {"password"},
	notifications.NotificationProviderPushover: {"token"},
	notifications.NotificationProviderGotify:   {"token"},
	notifications.NotificationProviderMatrix:   {"password"},
	// The Google Chat incoming webhook URL embeds the key and token query
	// parameters, so the whole URL is treated as a credential.
	notifications.NotificationProviderGoogleChat: {"webhookUrl"},
}

var notificationTargetFieldByProviderInternal = map[notifications.NotificationProvider]string{
	notifications.NotificationProviderEmail:  "smtpHost",
	notifications.NotificationProviderSignal: "host",
	notifications.NotificationProviderNtfy:   "host",
	notifications.NotificationProviderGotify: "host",
	notifications.NotificationProviderMatrix: "host",
}

const (
	notificationDispatchConcurrencyInternal = 4

	notificationTestTypeSimple           = "simple"
	notificationTestTypeImageUpdate      = "image-update"
	notificationTestTypeBatchImageUpdate = "batch-image-update"
	notificationTestTypeVulnerability    = "vulnerability-found"
	notificationTestTypePruneReport      = "prune-report"
	notificationTestTypeAutoHeal         = "auto-heal"
)

var (
	ErrUnauthorizedNotificationDispatch = errors.New("unauthorized notification dispatch")
	ErrUnsupportedDispatchKind          = errors.New("unsupported notification dispatch kind")
)

type NotificationService struct {
	db             *database.DB
	config         *config.Config
	environmentSvc *environment.EnvironmentService
	eventSvc       *event.EventService
	apnsSvc        *apns.ApnsService
	templates      *templates.Service
	httpClient     *http.Client
}

type NotificationTarget struct {
	EnvironmentID   string
	EnvironmentName string
}

func logManagerDispatchNotificationInternal(ctx context.Context, target NotificationTarget, kind notification.DispatchKind) {
	slog.InfoContext(ctx,
		"Manager dispatching notification on behalf of agent",
		"environmentId", target.EnvironmentID,
		"environmentName", target.EnvironmentName,
		"kind", string(kind),
	)
}

func (s *NotificationService) ResolveNotificationTarget(ctx context.Context, environmentID string) (NotificationTarget, error) {
	return s.resolveNotificationTargetInternal(ctx, environmentID)
}

func NewNotificationService(db *database.DB, cfg *config.Config, environmentSvc *environment.EnvironmentService, eventSvc *event.EventService, apnsSvc *apns.ApnsService) *NotificationService {
	return &NotificationService{
		db:             db,
		config:         cfg,
		environmentSvc: environmentSvc,
		eventSvc:       eventSvc,
		apnsSvc:        apnsSvc,
		templates:      templates.NewService(cfg.GetAppURL),
		httpClient:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *NotificationService) resolveNotificationTargetInternal(ctx context.Context, environmentID string) (NotificationTarget, error) {
	trimmedEnvironmentID := cmp.Or(strings.TrimSpace(environmentID), "0")

	if s.environmentSvc != nil {
		env, err := s.environmentSvc.GetEnvironmentByID(ctx, trimmedEnvironmentID)
		if err == nil && env != nil {
			environmentName := strings.TrimSpace(env.Name)
			if environmentName == "" && trimmedEnvironmentID == "0" {
				environmentName = "Local Docker"
			}
			return NotificationTarget{
				EnvironmentID:   env.ID,
				EnvironmentName: environmentName,
			}, nil
		}
		if trimmedEnvironmentID != "0" {
			if err != nil {
				return NotificationTarget{}, fmt.Errorf("failed to resolve notification environment: %w", err)
			}
			return NotificationTarget{}, nil
		}
		if err != nil {
			slog.WarnContext(ctx, "Failed to resolve local environment, falling back to 'Local Docker'", "error", err)
		}
	}

	return NotificationTarget{
		EnvironmentID:   "0",
		EnvironmentName: "Local Docker",
	}, nil
}

func (s *NotificationService) resolveNotificationTargetForAccessTokenInternal(ctx context.Context, accessToken string) (NotificationTarget, error) {
	if s.environmentSvc == nil {
		return NotificationTarget{}, errors.New("environment service not initialized")
	}

	env, err := s.environmentSvc.ResolveEnvironmentByAccessToken(ctx, accessToken)
	if err != nil {
		if errors.Is(err, environment.ErrEnvironmentAccessTokenRequired) || errors.Is(err, environment.ErrInvalidEnvironmentAccessToken) {
			return NotificationTarget{}, ErrUnauthorizedNotificationDispatch
		}
		return NotificationTarget{}, err
	}

	environmentName := strings.TrimSpace(env.Name)
	if environmentName == "" && env.ID == "0" {
		environmentName = "Local Docker"
	}

	return NotificationTarget{
		EnvironmentID:   env.ID,
		EnvironmentName: environmentName,
	}, nil
}

func (s *NotificationService) dispatchNotificationToManagerInternal(ctx context.Context, payload notification.DispatchRequest) (notification.DispatchResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return notification.DispatchResponse{}, fmt.Errorf("failed to marshal notification dispatch payload: %w", err)
	}

	publishViaTunnel := func() error {
		return edge.PublishEventToManager(&edge.TunnelEvent{
			Type:         edge.TunnelEventTypeNotificationDispatch,
			Title:        string(payload.Kind),
			MetadataJSON: body,
		})
	}

	// Tunnel-connected edge agents typically have neither MANAGER_API_URL nor
	// AGENT_TOKEN configured for HTTP; ride the agent-to-manager event channel
	// instead so their notifications are not silently lost (#3002).
	if s.config == nil || strings.TrimSpace(httpx.ManagerBaseURL(s.config.ManagerApiUrl)) == "" || strings.TrimSpace(s.config.AgentToken) == "" {
		if publishViaTunnelErr := publishViaTunnel(); publishViaTunnelErr != nil {
			return notification.DispatchResponse{}, fmt.Errorf("notification dispatch needs either MANAGER_API_URL + AGENT_TOKEN or a connected edge tunnel: %w", publishViaTunnelErr)
		}
		return notification.DispatchResponse{Message: "notification dispatched via edge tunnel"}, nil
	}

	dispatchURL := strings.TrimRight(httpx.ManagerBaseURL(s.config.ManagerApiUrl), "/") + "/api/notifications/dispatch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dispatchURL, bytes.NewReader(body))
	if err != nil {
		return notification.DispatchResponse{}, fmt.Errorf("failed to create notification dispatch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(middleware.HeaderApiKey, s.config.AgentToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// HTTP unreachable does not mean the manager is: an agent configured for
		// HTTP may still hold a healthy edge tunnel, so ride that before giving up.
		if tunnelErr := publishViaTunnel(); tunnelErr == nil {
			return notification.DispatchResponse{Message: "notification dispatched via edge tunnel"}, nil
		}
		return notification.DispatchResponse{}, fmt.Errorf("failed to dispatch notification to manager: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var apiResponse struct {
			Data notification.DispatchResponse `json:"data"`
		}
		if unmarshalReadErr := json.UnmarshalRead(resp.Body, &apiResponse); unmarshalReadErr != nil {
			return notification.DispatchResponse{}, fmt.Errorf("failed to decode manager notification dispatch response: %w", unmarshalReadErr)
		}
		return apiResponse.Data, nil
	}

	// A non-2xx also means the notification was not dispatched, and the HTTP
	// path can fail independently of the tunnel (stale AGENT_TOKEN, wrong
	// MANAGER_API_URL, intermediate proxy), so the tunnel fallback applies here
	// too before surfacing the status error.
	responseBody, _ := io.ReadAll(resp.Body)
	if tunnelErr := publishViaTunnel(); tunnelErr == nil {
		return notification.DispatchResponse{Message: "notification dispatched via edge tunnel"}, nil
	}
	return notification.DispatchResponse{}, fmt.Errorf("manager notification dispatch failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
}

func (s *NotificationService) DispatchNotification(ctx context.Context, accessToken string, payload notification.DispatchRequest) (notification.DispatchResponse, error) {
	if s.config != nil && s.config.AgentMode {
		return notification.DispatchResponse{}, errors.New("notification dispatch is manager-only")
	}

	target, err := s.resolveNotificationTargetForAccessTokenInternal(ctx, accessToken)
	if err != nil {
		return notification.DispatchResponse{}, err
	}

	return s.dispatchForTargetInternal(ctx, target, payload)
}

// DispatchNotificationForEnvironment dispatches on behalf of an agent whose
// identity was already established by its edge tunnel session, so no access
// token is exchanged (#3002).
func (s *NotificationService) DispatchNotificationForEnvironment(ctx context.Context, environmentID string, payload notification.DispatchRequest) (notification.DispatchResponse, error) {
	if s.config != nil && s.config.AgentMode {
		return notification.DispatchResponse{}, errors.New("notification dispatch is manager-only")
	}

	target, err := s.resolveNotificationTargetInternal(ctx, environmentID)
	if err != nil {
		return notification.DispatchResponse{}, err
	}

	return s.dispatchForTargetInternal(ctx, target, payload)
}

func (s *NotificationService) dispatchForTargetInternal(ctx context.Context, target NotificationTarget, payload notification.DispatchRequest) (notification.DispatchResponse, error) {
	var err error
	dispatchResponse := notification.DispatchResponse{Message: "Notification dispatched successfully"}
	switch payload.Kind {
	case notification.DispatchKindImageUpdate:
		if payload.ImageUpdate == nil {
			return notification.DispatchResponse{}, errors.New("image update payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		dispatchResponse.Delivered, err = s.sendImageUpdateNotificationForTargetInternal(
			ctx,
			target,
			payload.ImageUpdate.ImageRef,
			&payload.ImageUpdate.UpdateInfo,
			notifications.NotificationEventImageUpdate,
		)
		return dispatchResponse, err
	case notification.DispatchKindBatchImageUpdate:
		if payload.BatchImageUpdate == nil {
			return notification.DispatchResponse{}, errors.New("batch image update payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		dispatchResponse.Delivered, err = s.sendBatchImageUpdateNotificationForTargetInternal(ctx, target, payload.BatchImageUpdate.Updates)
		return dispatchResponse, err
	case notification.DispatchKindContainerUpdate:
		if payload.ContainerUpdate == nil {
			return notification.DispatchResponse{}, errors.New("container update payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		return dispatchResponse, s.sendContainerUpdateNotificationForTargetInternal(
			ctx,
			target,
			payload.ContainerUpdate.ContainerName,
			payload.ContainerUpdate.ImageRef,
			payload.ContainerUpdate.OldDigest,
			payload.ContainerUpdate.NewDigest,
		)
	case notification.DispatchKindBatchContainerUpdate:
		if payload.BatchContainerUpdate == nil {
			return notification.DispatchResponse{}, errors.New("batch container update payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		entries := make([]notifications.ContainerUpdateBatchEntry, 0, len(payload.BatchContainerUpdate.Updates))
		for _, update := range payload.BatchContainerUpdate.Updates {
			entries = append(entries, notifications.ContainerUpdateBatchEntry{
				ContainerName: update.ContainerName,
				ImageRef:      update.ImageRef,
				OldDigest:     update.OldDigest,
				NewDigest:     update.NewDigest,
			})
		}
		return dispatchResponse, s.sendBatchContainerUpdateNotificationForTargetInternal(ctx, target, entries)
	case notification.DispatchKindVulnerabilityFound:
		if payload.VulnerabilityFound == nil {
			return notification.DispatchResponse{}, errors.New("vulnerability payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		return dispatchResponse, s.sendVulnerabilityNotificationForTargetInternal(ctx, target, VulnerabilityNotificationPayload{
			CVEID:            payload.VulnerabilityFound.CVEID,
			CVELink:          payload.VulnerabilityFound.CVELink,
			Severity:         payload.VulnerabilityFound.Severity,
			ImageName:        payload.VulnerabilityFound.ImageName,
			FixedVersion:     payload.VulnerabilityFound.FixedVersion,
			PkgName:          payload.VulnerabilityFound.PkgName,
			InstalledVersion: payload.VulnerabilityFound.InstalledVersion,
		})
	case notification.DispatchKindPruneReport:
		if payload.PruneReport == nil {
			return notification.DispatchResponse{}, errors.New("prune report payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		return dispatchResponse, s.sendPruneReportNotificationForTargetInternal(ctx, target, &payload.PruneReport.Result)
	case notification.DispatchKindAutoHeal:
		if payload.AutoHeal == nil {
			return notification.DispatchResponse{}, errors.New("auto-heal payload is required")
		}
		logManagerDispatchNotificationInternal(ctx, target, payload.Kind)
		return dispatchResponse, s.sendAutoHealNotificationForTargetInternal(ctx, target, payload.AutoHeal.ContainerName, payload.AutoHeal.ContainerID)
	default:
		return notification.DispatchResponse{}, fmt.Errorf("%s: %w", payload.Kind, ErrUnsupportedDispatchKind)
	}
}

func (s *NotificationService) GetAllSettings(ctx context.Context) ([]NotificationSettings, error) {
	var settings []NotificationSettings
	if err := s.db.WithContext(ctx).Find(&settings).Error; err != nil {
		return nil, fmt.Errorf("failed to get notification settings: %w", err)
	}
	return settings, nil
}

func (s *NotificationService) GetSettingsByProvider(ctx context.Context, provider notifications.NotificationProvider) (*NotificationSettings, error) {
	var setting NotificationSettings
	if err := s.db.WithContext(ctx).Where("provider = ?", provider).First(&setting).Error; err != nil {
		return nil, err
	}
	return &setting, nil
}

func (s *NotificationService) CreateOrUpdateSettings(ctx context.Context, provider notifications.NotificationProvider, enabled bool, localConfig database.JSON) (*NotificationSettings, error) {
	if provider == notifications.NotificationProviderGeneric {
		genericConfig, decodeErr := notifications.DecodeConfig[notifications.GenericConfig](localConfig, "Generic")
		if decodeErr != nil {
			return nil, decodeErr
		}
		// Shoutrrr renders the payload template internally at send time, so a
		// broken template is validated here where the error surfaces against
		// the user's own configuration instead of as an opaque send failure.
		if validateErr := notifications.ValidateGenericPayloadTemplate(genericConfig); validateErr != nil {
			return nil, common.Classify(common.ErrInvalidNotificationPayloadTemplate, validateErr)
		}
	}

	var setting NotificationSettings

	err := s.db.WithContext(ctx).Where("provider = ?", provider).First(&setting).Error
	existingConfig := kit.Ternary(err == nil, setting.Config, database.JSON(nil))

	encryptedConfig, encryptErr := encryptNotificationConfigCredentialsInternal(provider, localConfig, existingConfig)
	if encryptErr != nil {
		return nil, encryptErr
	}
	localConfig = encryptedConfig
	if _, ok := localConfig["events"]; !ok {
		if existingEvents, localOk := existingConfig["events"]; localOk {
			localConfig["events"] = existingEvents
		}
	}

	if err != nil {
		setting = NotificationSettings{
			Provider: provider,
			Enabled:  enabled,
			Config:   localConfig,
		}
		if createSettingsErr := s.db.WithContext(ctx).Create(&setting).Error; createSettingsErr != nil {
			return nil, fmt.Errorf("failed to create notification settings: %w", createSettingsErr)
		}
	} else {
		setting.Enabled = enabled
		setting.Config = localConfig
		if updateSettingsErr := s.db.WithContext(ctx).Save(&setting).Error; updateSettingsErr != nil {
			return nil, fmt.Errorf("failed to update notification settings: %w", updateSettingsErr)
		}
	}

	return &setting, nil
}

// RedactNotificationConfigCredentials returns a copy of config with provider credential fields blanked for API responses.
func RedactNotificationConfigCredentials(provider notifications.NotificationProvider, localConfig database.JSON) database.JSON {
	redacted := cloneNotificationConfigInternal(localConfig)
	for _, field := range notificationCredentialFieldsByProviderInternal[provider] {
		value, ok := redacted[field]
		if !ok {
			continue
		}
		if value == "" {
			delete(redacted, field)
			continue
		}
		redacted[field] = ""
	}
	return redacted
}

func encryptNotificationConfigCredentialsInternal(provider notifications.NotificationProvider, localConfig, existingConfig database.JSON) (database.JSON, error) {
	encryptedConfig := cloneNotificationConfigInternal(localConfig)
	preserveConfig := existingConfig
	if provider == notifications.NotificationProviderSignal {
		preserveConfig = signalCredentialPreservationConfigInternal(localConfig, existingConfig)
	}
	if provider == notifications.NotificationProviderEmail {
		preserveConfig = emailCredentialPreservationConfigInternal(localConfig, existingConfig)
	}
	if targetField := notificationTargetFieldByProviderInternal[provider]; targetField != "" {
		currentTarget, _ := existingConfig[targetField].(string)
		nextTarget, _ := encryptedConfig[targetField].(string)
		if strings.TrimSpace(nextTarget) == "" && strings.TrimSpace(currentTarget) != "" {
			nextTarget = currentTarget
			encryptedConfig[targetField] = currentTarget
		}

		storedCredentials := make(map[string]bool, len(notificationCredentialFieldsByProviderInternal[provider]))
		updatedCredentials := make(map[string]bool, len(notificationCredentialFieldsByProviderInternal[provider]))
		for _, field := range notificationCredentialFieldsByProviderInternal[provider] {
			preservedValue, _ := preserveConfig[field].(string)
			updatedValue, _ := encryptedConfig[field].(string)
			storedCredentials[field] = preservedValue != ""
			updatedCredentials[field] = updatedValue != ""
		}

		if err := validation.ValidateCredentialTargetChange(
			targetField,
			currentTarget,
			new(nextTarget),
			func(value string) string {
				return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
			},
			storedCredentials,
			updatedCredentials,
		); err != nil {
			return nil, err
		}
	}
	for _, field := range notificationCredentialFieldsByProviderInternal[provider] {
		value, _ := encryptedConfig[field].(string)
		if value == "" {
			if existingValue, ok := preserveConfig[field].(string); ok && existingValue != "" {
				encryptedConfig[field] = existingValue
			}
			continue
		}

		encrypted, err := encryptNotificationCredentialInternal(value)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt notification credential %q: %w", field, err)
		}
		encryptedConfig[field] = encrypted
	}
	return encryptedConfig, nil
}

func signalCredentialPreservationConfigInternal(localConfig, existingConfig database.JSON) database.JSON {
	preserveConfig := cloneNotificationConfigInternal(existingConfig)
	user, _ := localConfig["user"].(string)
	password, _ := localConfig["password"].(string)
	token, _ := localConfig["token"].(string)

	if strings.TrimSpace(token) != "" {
		delete(preserveConfig, "password")
	}
	if strings.TrimSpace(user) != "" || strings.TrimSpace(password) != "" {
		delete(preserveConfig, "token")
	}

	return preserveConfig
}

func emailCredentialPreservationConfigInternal(localConfig, existingConfig database.JSON) database.JSON {
	preserveConfig := cloneNotificationConfigInternal(existingConfig)
	if authMode, _ := localConfig["authMode"].(string); authMode == string(notifications.EmailAuthModeNone) {
		delete(preserveConfig, "smtpPassword")
	}
	return preserveConfig
}

func encryptNotificationCredentialInternal(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if _, err := crypto.Decrypt(value); err == nil {
		return value, nil
	}
	return crypto.Encrypt(value)
}

func cloneNotificationConfigInternal(localConfig database.JSON) database.JSON {
	if localConfig == nil {
		return database.JSON{}
	}
	cloned := make(database.JSON, len(localConfig))
	maps.Copy(cloned, localConfig)
	return cloned
}

func (s *NotificationService) DeleteSettings(ctx context.Context, provider notifications.NotificationProvider) error {
	if err := s.db.WithContext(ctx).Where("provider = ?", provider).Delete(&NotificationSettings{}).Error; err != nil {
		return fmt.Errorf("failed to delete notification settings: %w", err)
	}
	return nil
}

func (s *NotificationService) isEventEnabled(localConfig database.JSON, eventType notifications.NotificationEventType) bool {
	events, ok := localConfig["events"].(map[string]any)
	if !ok {
		return true // If no events config, default to enabled
	}

	enabled, ok := events[string(eventType)].(bool)
	if !ok {
		return true // If event type not specified, default to enabled
	}

	return enabled
}

// logNotificationInternal records a delivery attempt in the event log so sends and
// failures are visible alongside every other Arcane event.
func (
	s *NotificationService,
) logNotificationInternal(
	ctx context.Context,
	environmentID string,
	provider notifications.NotificationProvider,
	subject, status string,
	errMsg *string,
	metadata database.JSON,
) {
	if s.eventSvc == nil {
		return
	}

	severity := event.EventSeveritySuccess
	title := fmt.Sprintf("Notification sent via %s", provider)
	description := subject
	if errMsg != nil {
		severity = event.EventSeverityError
		title = fmt.Sprintf("Notification failed via %s", provider)
		description = fmt.Sprintf("%s: %s", subject, *errMsg)
	}

	eventMetadata := cloneNotificationConfigInternal(metadata)
	eventMetadata["provider"] = string(provider)
	eventMetadata["status"] = status

	resourceType := "notification"
	providerName := string(provider)
	req := event.CreateEventRequest{
		Type:          event.EventTypeNotificationSend,
		Severity:      severity,
		Title:         title,
		Description:   description,
		ResourceType:  &resourceType,
		ResourceName:  &providerName,
		EnvironmentID: &environmentID,
		Metadata:      eventMetadata,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer utils.RecoverToError(nil, "notification event logging", "provider", providerName)
		logCtx, cancelLog := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelLog()
		if _, err := s.eventSvc.CreateEvent(logCtx, req); err != nil {
			slog.WarnContext(logCtx, "Failed to log notification event", "provider", providerName, "error", err.Error())
		}
	}()

	// Cancellation ends the caller's wait, not persistence of a completed attempt.
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// notifyEnabledProvidersInternal is the single fan-out loop behind every
// notification event: it walks all provider settings, skips disabled providers
// and providers with the event unsubscribed, dispatches to the rest, logs each
// attempt, and aggregates send errors. It returns how many providers the
// notification was actually delivered to.
func (s *NotificationService) notifyEnabledProvidersInternal(
	ctx context.Context,
	target NotificationTarget,
	eventType notifications.NotificationEventType,
	logRef string,
	metadata database.JSON,
	dispatch func(ctx context.Context, provider notifications.NotificationProvider, config database.JSON) (handled bool, err error),
) (int, error) {
	settings, err := s.GetAllSettings(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get notification settings: %w", err)
	}

	eligible := make([]NotificationSettings, 0, len(settings))
	for _, setting := range settings {
		if setting.Enabled && s.isEventEnabled(setting.Config, eventType) {
			eligible = append(eligible, setting)
		}
	}

	handled := make([]bool, len(eligible))
	sendErrs := make([]error, len(eligible))
	var g errgroup.Group
	g.SetLimit(notificationDispatchConcurrencyInternal)
	for i, setting := range eligible {
		g.Go(func() error {
			defer utils.RecoverToError(&sendErrs[i], "notification dispatch", "provider", setting.Provider)
			handled[i] = true
			handled[i], sendErrs[i] = dispatch(ctx, setting.Provider, setting.Config)
			return nil
		})
	}
	var errs []string
	if waitErr := g.Wait(); waitErr != nil {
		errs = append(errs, fmt.Sprintf("notification dispatch: %v", waitErr))
	}

	delivered := 0
	for i, setting := range eligible {
		if !handled[i] {
			slog.WarnContext(ctx, "Unknown notification provider", "provider", setting.Provider)
			continue
		}
		if sendErrs[i] == nil {
			delivered++
		}
		status, errMsg := collectNotificationSendResultInternal(&errs, setting.Provider, sendErrs[i])
		s.logNotificationInternal(ctx, target.EnvironmentID, setting.Provider, logRef, status, errMsg, metadata)
	}

	if s.apnsSvc != nil {
		if enqueueErr := s.apnsSvc.Enqueue(ctx, target.EnvironmentID, target.EnvironmentName, eventType, logRef, metadata); enqueueErr != nil {
			slog.WarnContext(ctx, "Failed to enqueue mobile push notification", "error", enqueueErr)
		}
	}

	if len(errs) > 0 {
		return delivered, fmt.Errorf("notification errors: %s", strings.Join(errs, "; "))
	}
	return delivered, nil
}

func collectNotificationSendResultInternal(localErrors *[]string, provider notifications.NotificationProvider, sendErr error) (string, *string) {
	if sendErr == nil {
		return "success", nil
	}

	msg := sendErr.Error()
	*localErrors = append(*localErrors, fmt.Sprintf("%s: %s", provider, msg))
	return "failed", &msg
}

var supportedNotificationTestTypes = map[string]struct{}{
	notificationTestTypeSimple:           {},
	notificationTestTypeImageUpdate:      {},
	notificationTestTypeBatchImageUpdate: {},
	notificationTestTypeVulnerability:    {},
	notificationTestTypePruneReport:      {},
	notificationTestTypeAutoHeal:         {},
}

// VulnerabilityNotificationPayload is the data sent to all providers for vulnerability_found events.
// Only vulnerabilities with a fixed version should trigger this notification.
type VulnerabilityNotificationPayload struct {
	CVEID            string // e.g. CVE-2024-1234
	CVELink          string // e.g. https://nvd.nist.gov/vuln/detail/CVE-2024-1234
	Severity         string // CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN
	ImageName        string // e.g. nginx:latest
	FixedVersion     string
	PkgName          string // optional
	InstalledVersion string // optional
}

// SendBatchContainerUpdateNotification dispatches a single batched
// notification covering all containers updated in one auto-update run.
func (s *NotificationService) SendBatchContainerUpdateNotification(ctx context.Context, entries []notifications.ContainerUpdateBatchEntry) error {
	if len(entries) == 0 {
		return nil
	}

	if s.config != nil && s.config.AgentMode {
		updates := make([]notification.DispatchContainerUpdate, 0, len(entries))
		for _, entry := range entries {
			updates = append(updates, notification.DispatchContainerUpdate{
				ContainerName: entry.ContainerName,
				ImageRef:      entry.ImageRef,
				OldDigest:     entry.OldDigest,
				NewDigest:     entry.NewDigest,
			})
		}
		_, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind:                 notification.DispatchKindBatchContainerUpdate,
			BatchContainerUpdate: &notification.DispatchBatchContainerUpdate{Updates: updates},
		})
		return err
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return err
	}

	return s.sendBatchContainerUpdateNotificationForTargetInternal(ctx, target, entries)
}

func (s *NotificationService) sendBatchContainerUpdateNotificationForTargetInternal(ctx context.Context, target NotificationTarget, entries []notifications.ContainerUpdateBatchEntry) error {
	containerNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		containerNames = append(containerNames, entry.ContainerName)
	}

	metadata := database.JSON{
		"containerNames": containerNames,
		"updateCount":    len(entries),
		"eventType":      string(notifications.NotificationEventContainerUpdate),
		"batch":          true,
	}
	content := s.templates.BatchContainerUpdate(target.EnvironmentName, entries)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventContainerUpdate)
	_, err := s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventContainerUpdate, strings.Join(
		containerNames,
		", ",
	), metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
	return err
}

// --- Event entry points ---

// SendImageUpdateNotification dispatches a single-image update notification and
// returns the number of eligible providers it was delivered to (0 means no
// provider has this event enabled, so callers must not mark the update notified).
func (s *NotificationService) SendImageUpdateNotification(ctx context.Context, imageRef string, updateInfo *imageupdate.Response, eventType notifications.NotificationEventType) (int, error) {
	if updateInfo == nil {
		return 0, errors.New("updateInfo is required")
	}

	if s.config != nil && s.config.AgentMode {
		dispatchResponse, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindImageUpdate,
			ImageUpdate: &notification.DispatchImageUpdate{
				ImageRef:   imageRef,
				UpdateInfo: *updateInfo,
			},
		})
		if err != nil {
			return 0, err
		}
		return dispatchResponse.Delivered, nil
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return 0, err
	}

	return s.sendImageUpdateNotificationForTargetInternal(ctx, target, imageRef, updateInfo, eventType)
}

func (
	s *NotificationService,
) sendImageUpdateNotificationForTargetInternal(
	ctx context.Context,
	target NotificationTarget,
	imageRef string,
	updateInfo *imageupdate.Response,
	eventType notifications.NotificationEventType,
) (
	int,
	error,
) {
	metadata := database.JSON{
		"hasUpdate":     updateInfo.HasUpdate,
		"currentDigest": updateInfo.CurrentDigest,
		"latestDigest":  updateInfo.LatestDigest,
		"updateType":    updateInfo.UpdateType,
		"eventType":     string(eventType),
	}
	content := s.templates.ImageUpdate(target.EnvironmentName, imageRef, updateInfo)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, eventType)
	return s.notifyEnabledProvidersInternal(ctx, target, eventType, imageRef, metadata, func(ctx context.Context, provider notifications.NotificationProvider, config database.JSON) (bool, error) {
		return notifications.Deliver(ctx, provider, config, content)
	})
}

func (s *NotificationService) SendContainerUpdateNotification(ctx context.Context, containerName, imageRef, oldDigest, newDigest string) error {
	if s.config != nil && s.config.AgentMode {
		_, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindContainerUpdate,
			ContainerUpdate: &notification.DispatchContainerUpdate{
				ContainerName: containerName,
				ImageRef:      imageRef,
				OldDigest:     oldDigest,
				NewDigest:     newDigest,
			},
		})
		return err
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return err
	}

	return s.sendContainerUpdateNotificationForTargetInternal(ctx, target, containerName, imageRef, oldDigest, newDigest)
}

func (s *NotificationService) sendContainerUpdateNotificationForTargetInternal(ctx context.Context, target NotificationTarget, containerName, imageRef, oldDigest, newDigest string) error {
	metadata := database.JSON{
		"containerName": containerName,
		"oldDigest":     oldDigest,
		"newDigest":     newDigest,
		"eventType":     string(notifications.NotificationEventContainerUpdate),
	}
	content := s.templates.ContainerUpdate(target.EnvironmentName, containerName, imageRef, oldDigest, newDigest)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventContainerUpdate)
	_, err := s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventContainerUpdate, imageRef, metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
	return err
}

func isVulnerabilitySummaryPayload(payload VulnerabilityNotificationPayload) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(payload.CVEID)), "DAILY SUMMARY")
}

// SendVulnerabilityNotification notifies all enabled providers that have vulnerability_found event enabled.
// Only daily summary payloads are sent; legacy per-CVE payloads are ignored.
func (s *NotificationService) SendVulnerabilityNotification(ctx context.Context, payload VulnerabilityNotificationPayload) error {
	if !isVulnerabilitySummaryPayload(payload) {
		slog.InfoContext(ctx, "skipping legacy individual vulnerability notification payload", "cve", payload.CVEID)
		return nil
	}

	if s.config != nil && s.config.AgentMode {
		_, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindVulnerabilityFound,
			VulnerabilityFound: &notification.DispatchVulnerabilityFound{
				CVEID:            payload.CVEID,
				CVELink:          payload.CVELink,
				Severity:         payload.Severity,
				ImageName:        payload.ImageName,
				FixedVersion:     payload.FixedVersion,
				PkgName:          payload.PkgName,
				InstalledVersion: payload.InstalledVersion,
			},
		})
		return err
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return err
	}

	return s.sendVulnerabilityNotificationForTargetInternal(ctx, target, payload)
}

func (s *NotificationService) sendVulnerabilityNotificationForTargetInternal(ctx context.Context, target NotificationTarget, payload VulnerabilityNotificationPayload) error {
	metadata := database.JSON{
		"cveId":        payload.CVEID,
		"severity":     payload.Severity,
		"fixedVersion": payload.FixedVersion,
		"eventType":    string(notifications.NotificationEventVulnerabilityFound),
	}
	content := s.templates.Vulnerability(target.EnvironmentName, notification.DispatchVulnerabilityFound(payload))
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventVulnerabilityFound)
	_, err := s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventVulnerabilityFound, payload.ImageName, metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
	return err
}

// SendBatchImageUpdateNotification dispatches a batched image-update notification
// and returns the number of eligible providers it was delivered to (0 means no
// provider has this event enabled, so callers must not mark the updates notified).
func (s *NotificationService) SendBatchImageUpdateNotification(ctx context.Context, updates map[string]*imageupdate.Response) (int, error) {
	updatesWithChanges := filterUpdatesWithChangesInternal(updates)
	if len(updatesWithChanges) == 0 {
		return 0, nil
	}

	if s.config != nil && s.config.AgentMode {
		dispatchResponse, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindBatchImageUpdate,
			BatchImageUpdate: &notification.DispatchBatchImageUpdate{
				Updates: updatesWithChanges,
			},
		})
		if err != nil {
			return 0, err
		}
		return dispatchResponse.Delivered, nil
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return 0, err
	}

	return s.sendBatchImageUpdateNotificationForTargetInternal(ctx, target, updatesWithChanges)
}

func filterUpdatesWithChangesInternal(updates map[string]*imageupdate.Response) map[string]*imageupdate.Response {
	updatesWithChanges := make(map[string]*imageupdate.Response, len(updates))
	for imageRef, update := range updates {
		if update != nil && update.HasUpdate {
			updatesWithChanges[imageRef] = update
		}
	}
	return updatesWithChanges
}

func (s *NotificationService) sendBatchImageUpdateNotificationForTargetInternal(ctx context.Context, target NotificationTarget, updates map[string]*imageupdate.Response) (int, error) {
	updatesWithChanges := filterUpdatesWithChangesInternal(updates)

	if len(updatesWithChanges) == 0 {
		return 0, nil
	}

	imageRefs := slices.Collect(maps.Keys(updatesWithChanges))

	metadata := database.JSON{
		"updateCount": len(updatesWithChanges),
		"eventType":   string(notifications.NotificationEventImageUpdate),
		"batch":       true,
	}
	content := s.templates.BatchImageUpdate(target.EnvironmentName, updatesWithChanges)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventImageUpdate)
	return s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventImageUpdate, strings.Join(
		imageRefs,
		", ",
	), metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
}

func (s *NotificationService) SendPruneReportNotification(ctx context.Context, result *system.PruneAllResult) error {
	if result == nil {
		slog.InfoContext(ctx, "skipping prune report notification because no prune result was reported")
		return nil
	}

	hasChanges := pruneResultHasChangesInternal(result)
	hasErrors := len(result.Errors) > 0
	if !hasChanges && !hasErrors {
		slog.InfoContext(ctx, "skipping prune report notification because no resources were pruned and no errors were reported")
		return nil
	}

	if s.config != nil && s.config.AgentMode {
		_, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindPruneReport,
			PruneReport: &notification.DispatchPruneReport{
				Result: *result,
			},
		})
		return err
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return err
	}

	return s.sendPruneReportNotificationForTargetInternal(ctx, target, result)
}

func (s *NotificationService) sendPruneReportNotificationForTargetInternal(ctx context.Context, target NotificationTarget, result *system.PruneAllResult) error {
	hasChanges := pruneResultHasChangesInternal(result)
	hasErrors := len(result.Errors) > 0

	metadata := database.JSON{
		"spaceReclaimed": result.SpaceReclaimed,
		"eventType":      string(notifications.NotificationEventPruneReport),
	}
	content := s.templates.PruneReport(target.EnvironmentName, result)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventPruneReport)
	_, err := s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventPruneReport, "System Prune Report", metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
	if err != nil {
		return err
	}
	if hasErrors && !hasChanges {
		slog.WarnContext(ctx, "sending prune report notification with errors but no resources were pruned", "errorCount", len(result.Errors))
	}

	return nil
}

func pruneResultHasChangesInternal(result *system.PruneAllResult) bool {
	if result == nil {
		return false
	}

	if result.SpaceReclaimed > 0 {
		return true
	}

	return len(result.ContainersPruned) > 0 ||
		len(result.ImagesDeleted) > 0 ||
		len(result.VolumesDeleted) > 0 ||
		len(result.NetworksDeleted) > 0
}

// SendAutoHealNotification sends a notification when a container is auto-healed.
func (s *NotificationService) SendAutoHealNotification(ctx context.Context, containerName, containerID string) error {
	if s.config != nil && s.config.AgentMode {
		_, err := s.dispatchNotificationToManagerInternal(ctx, notification.DispatchRequest{
			Kind: notification.DispatchKindAutoHeal,
			AutoHeal: &notification.DispatchAutoHeal{
				ContainerName: containerName,
				ContainerID:   containerID,
			},
		})
		return err
	}

	target, err := s.resolveNotificationTargetInternal(ctx, "")
	if err != nil {
		return err
	}

	return s.sendAutoHealNotificationForTargetInternal(ctx, target, containerName, containerID)
}

func (s *NotificationService) sendAutoHealNotificationForTargetInternal(ctx context.Context, target NotificationTarget, containerName, containerID string) error {
	metadata := database.JSON{
		"containerID": containerID,
		"eventType":   string(notifications.NotificationEventAutoHeal),
	}
	content := s.templates.AutoHeal(target.EnvironmentName, containerName)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, notifications.NotificationEventAutoHeal)
	_, err := s.notifyEnabledProvidersInternal(ctx, target, notifications.NotificationEventAutoHeal, containerName, metadata, func(
		ctx context.Context,
		provider notifications.NotificationProvider,
		config database.JSON,
	) (
		bool,
		error,
	) {
		return notifications.Deliver(ctx, provider, config, content)
	})
	return err
}

// --- Test notifications ---

// notificationEventTypeForTestTypeInternal maps a test type to the event type a
// real notification of that kind would be gated on ("" = no event gate).
func notificationEventTypeForTestTypeInternal(testType string) notifications.NotificationEventType {
	switch testType {
	case notificationTestTypeImageUpdate, notificationTestTypeBatchImageUpdate:
		return notifications.NotificationEventImageUpdate
	case notificationTestTypeVulnerability:
		return notifications.NotificationEventVulnerabilityFound
	case notificationTestTypePruneReport:
		return notifications.NotificationEventPruneReport
	case notificationTestTypeAutoHeal:
		return notifications.NotificationEventAutoHeal
	default:
		return ""
	}
}

// testNotificationWarningInternal reports why a real notification would not send
// even though the test did: the provider is disabled, or the tested event type is
// unsubscribed. Empty means real notifications would send.
func (s *NotificationService) testNotificationWarningInternal(setting *NotificationSettings, testType string) string {
	if !setting.Enabled {
		return fmt.Sprintf("%s is disabled, so real notifications will not send", setting.Provider)
	}
	eventType := notificationEventTypeForTestTypeInternal(testType)
	if eventType != "" && !s.isEventEnabled(setting.Config, eventType) {
		return fmt.Sprintf("%s events are disabled for %s, so real notifications will not send", eventType, setting.Provider)
	}
	if eventType == "" && !slices.ContainsFunc(notifications.AllNotificationEventTypes, func(candidate notifications.NotificationEventType) bool {
		return s.isEventEnabled(setting.Config, candidate)
	}) {
		return fmt.Sprintf("no events are subscribed for %s, so real notifications will not send", setting.Provider)
	}
	return ""
}

func (s *NotificationService) testNotificationContentInternal(environmentName, testType string) notifications.Content {
	switch testType {
	case notificationTestTypeVulnerability:
		return s.templates.Vulnerability(environmentName, notification.DispatchVulnerabilityFound{
			CVEID:        "Daily Summary - " + time.Now().UTC().Format("2006-01-02"),
			Severity:     "Critical:1 High:3 Medium:2 Low:1 Unknown:0",
			ImageName:    "5 image(s) scanned, 2 with fixable vulnerabilities",
			FixedVersion: "7 fixable vulnerability record(s)",
			PkgName:      "CVE-2025-1234, CVE-2025-5678, CVE-2026-0001",
		})
	case notificationTestTypeAutoHeal:
		return s.templates.AutoHeal(environmentName, "test-container")
	case notificationTestTypePruneReport:
		return s.templates.PruneReport(environmentName, &system.PruneAllResult{
			Success:                  true,
			ContainersPruned:         []string{"a1b2c3d4e5f6", "f6e5d4c3b2a1"},
			ImagesDeleted:            []string{"sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			VolumesDeleted:           []string{"arcane_test_volume"},
			NetworksDeleted:          []string{"arcane_test_network"},
			SpaceReclaimed:           3825205248,
			ContainerSpaceReclaimed:  503316480,
			ImageSpaceReclaimed:      2449473536,
			VolumeSpaceReclaimed:     641728512,
			BuildCacheSpaceReclaimed: 230162432,
			Errors:                   []string{},
		})
	case notificationTestTypeBatchImageUpdate:
		return s.templates.BatchImageUpdate(environmentName, map[string]*imageupdate.Response{
			"nginx:latest": {
				HasUpdate:      true,
				UpdateType:     "digest",
				CurrentDigest:  "sha256:abc123def456789012345678901234567890",
				LatestDigest:   "sha256:xyz789ghi012345678901234567890123456",
				CheckTime:      time.Now(),
				ResponseTimeMs: 100,
			},
			"postgres:16-alpine": {
				HasUpdate:      true,
				UpdateType:     "digest",
				CurrentDigest:  "sha256:def456abc123789012345678901234567890",
				LatestDigest:   "sha256:ghi789xyz012345678901234567890123456",
				CheckTime:      time.Now(),
				ResponseTimeMs: 120,
			},
			"redis:7.2-alpine": {
				HasUpdate:      true,
				UpdateType:     "digest",
				CurrentDigest:  "sha256:123456789abc012345678901234567890def",
				LatestDigest:   "sha256:456789012def345678901234567890123abc",
				CheckTime:      time.Now(),
				ResponseTimeMs: 95,
			},
		})
	default: // simple and image-update
		imageRef := kit.Ternary(testType == notificationTestTypeSimple, "test/image:latest", "nginx:latest")
		return s.templates.ImageUpdate(environmentName, imageRef, &imageupdate.Response{
			HasUpdate:      true,
			UpdateType:     "digest",
			CurrentDigest:  "sha256:abc123def456789012345678901234567890",
			LatestDigest:   "sha256:xyz789ghi012345678901234567890123456",
			CheckTime:      time.Now(),
			ResponseTimeMs: 100,
		})
	}
}

// TestNotification sends a test message to the provider regardless of its enabled
// state (testing before enabling is legitimate) and returns a warning when a real
// notification of the tested kind would not send.
func (s *NotificationService) TestNotification(ctx context.Context, environmentID string, provider notifications.NotificationProvider, testType string) (string, error) {
	setting, err := s.GetSettingsByProvider(ctx, provider)
	if err != nil {
		return "", fmt.Errorf("please save your %s settings before testing", provider)
	}
	testType = cmp.Or(strings.TrimSpace(testType), notificationTestTypeSimple)
	if _, ok := supportedNotificationTestTypes[testType]; !ok {
		return "", fmt.Errorf("unsupported notification test type: %s", testType)
	}
	warning := s.testNotificationWarningInternal(setting, testType)

	target, err := s.resolveNotificationTargetInternal(ctx, environmentID)
	if err != nil {
		return "", err
	}

	if provider == notifications.NotificationProviderEmail && testType == notificationTestTypeSimple {
		_, sendErr := notifications.Deliver(ctx, notifications.NotificationProviderEmail, setting.Config, s.templates.TestEmail(target.EnvironmentName))
		return warning, sendErr
	}

	content := s.testNotificationContentInternal(target.EnvironmentName, testType)
	// Stamp the event vars so the Test button exercises a configured generic
	// payload template. The simple test renders image-update content, so it
	// reports that event type.
	testEventType := cmp.Or(notificationEventTypeForTestTypeInternal(testType), notifications.NotificationEventImageUpdate)
	content.Vars = notifications.EventVars(target.EnvironmentName, target.EnvironmentID, testEventType)
	handled, sendErr := notifications.Deliver(ctx, provider, setting.Config, content)
	if !handled {
		return "", fmt.Errorf("unknown provider: %s", provider)
	}
	return warning, sendErr
}
