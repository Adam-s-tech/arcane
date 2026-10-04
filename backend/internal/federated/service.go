package federated

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getarcaneapp/arcane/types/v2/federated"
	httpxtypes "github.com/getarcaneapp/arcane/types/v2/httpx"
	"github.com/samber/mo"
	"go.getarcane.app/kit/pkg"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/auth"
	"github.com/getarcaneapp/arcane/backend/v2/internal/common"
	"github.com/getarcaneapp/arcane/backend/v2/internal/database"
	"github.com/getarcaneapp/arcane/backend/v2/internal/event"
	"github.com/getarcaneapp/arcane/backend/v2/internal/role"
	"github.com/getarcaneapp/arcane/backend/v2/internal/session"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/internal/user"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/pagination"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/dbutil"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/httpx"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/jwtclaims"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils/oidcjwk"
)

const (
	federatedCredentialLastUsedWriteWindow = 5 * time.Minute
	defaultFederatedSubjectClaim           = "sub"
)

type FederatedCredentialService struct {
	db              *database.DB
	authService     *auth.AuthService
	userService     *user.UserService
	settingsService *settings.SettingsService
	eventService    *event.EventService
	roleService     *role.RoleService
	httpClient      *http.Client
	keySetManager   *oidcjwk.KeySetManager
	providerMu      sync.RWMutex
	keySets         map[string]oidc.KeySet
	providerGroup   singleflight.Group
}

func NewFederatedCredentialService(
	db *database.DB,
	authService *auth.AuthService,
	userService *user.UserService,
	settingsService *settings.SettingsService,
	eventService *event.EventService,
	httpClient *http.Client,
	keySetManager *oidcjwk.KeySetManager,
	roleService *role.RoleService,
) *FederatedCredentialService {
	if httpClient == nil {
		httpClient = httpx.NewHTTPClient(httpxtypes.ClientOptions{Timeout: 15 * time.Second, TLSHandshakeTimeout: 10 * time.Second})
	}

	return &FederatedCredentialService{
		db:              db,
		authService:     authService,
		userService:     userService,
		settingsService: settingsService,
		eventService:    eventService,
		httpClient:      httpClient,
		keySetManager:   keySetManager,
		roleService:     roleService,
		keySets:         make(map[string]oidc.KeySet),
	}
}

func (s *FederatedCredentialService) Create(ctx context.Context, callerUserID string, req federated.CreateFederatedCredential) (*federated.FederatedCredential, error) {
	normalized, err := normalizeCreateFederatedCredentialInternal(req)
	if err != nil {
		return nil, err
	}
	if validateRoleGrantAgainstUserErr := s.validateRoleGrantAgainstUserInternal(ctx, callerUserID, normalized.RoleID, normalized.EnvironmentID); validateRoleGrantAgainstUserErr != nil {
		return nil, validateRoleGrantAgainstUserErr
	}

	var created FederatedCredential
	err = dbutil.WithTx(ctx, s.db.DB, func(tx *gorm.DB) error {
		serviceUser := user.User{
			Username:         "svc_federated_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
			DisplayName:      mo.EmptyableToOption(strings.TrimSpace("Federated: " + normalized.Name)).ToPointer(),
			IsServiceAccount: true,
		}
		if createServiceUserErr := tx.Create(&serviceUser).Error; createServiceUserErr != nil {
			return fmt.Errorf("failed to create federated service user: %w", createServiceUserErr)
		}

		created = FederatedCredential{
			Name:            normalized.Name,
			Description:     normalized.Description,
			Enabled:         normalized.Enabled,
			IssuerURL:       normalized.IssuerURL,
			Audiences:       normalized.Audiences,
			SubjectClaim:    normalized.SubjectClaim,
			SubjectMatch:    normalized.SubjectMatch,
			MatchType:       normalized.MatchType,
			RoleID:          normalized.RoleID,
			EnvironmentID:   normalized.EnvironmentID,
			IdentityUserID:  serviceUser.ID,
			TokenTTLSeconds: normalized.TokenTTLSeconds,
			ExpiresAt:       normalized.ExpiresAt,
		}
		if createCredentialErr := tx.Create(&created).Error; createCredentialErr != nil {
			return fmt.Errorf("failed to create federated credential: %w", createCredentialErr)
		}

		assignment := role.UserRoleAssignment{
			UserID:        serviceUser.ID,
			RoleID:        normalized.RoleID,
			EnvironmentID: normalized.EnvironmentID,
			Source:        role.RoleAssignmentSourceManual,
		}
		if createRoleAssignmentErr := tx.Create(&assignment).Error; createRoleAssignmentErr != nil {
			return fmt.Errorf("failed to create federated role assignment: %w", createRoleAssignmentErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.roleService != nil {
		s.roleService.InvalidateUser(created.IdentityUserID)
	}

	reloaded, err := s.Get(ctx, created.ID)
	if err != nil {
		return nil, err
	}
	return reloaded, nil
}

func (s *FederatedCredentialService) List(ctx context.Context, params pagination.QueryParams) ([]federated.FederatedCredential, pagination.Response, error) {
	var credentials []FederatedCredential
	query := s.db.WithContext(ctx).
		Model(&FederatedCredential{}).
		Preload("IdentityUser").
		Preload("Role").
		Preload("Environment")

	if term := strings.TrimSpace(params.Search); term != "" {
		pattern := "%" + term + "%"
		query = query.Where("name LIKE ? OR COALESCE(description, '') LIKE ? OR issuer_url LIKE ? OR subject_match LIKE ?", pattern, pattern, pattern, pattern)
	}

	resp, err := pagination.PaginateAndSortDB(params, query, &credentials)
	if err != nil {
		return nil, pagination.Response{}, fmt.Errorf("failed to paginate federated credentials: %w", err)
	}

	result := make([]federated.FederatedCredential, len(credentials))
	for i := range credentials {
		result[i] = toFederatedCredentialDTOInternal(&credentials[i])
	}
	return result, resp, nil
}

func (s *FederatedCredentialService) Get(ctx context.Context, id string) (*federated.FederatedCredential, error) {
	var credential FederatedCredential
	if err := s.db.WithContext(ctx).
		Preload("IdentityUser").
		Preload("Role").
		Preload("Environment").
		Where("id = ?", id).
		First(&credential).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, common.Classify(common.ErrFederatedCredentialNotFound, errors.New("federated credential not found"))
		}
		return nil, fmt.Errorf("failed to get federated credential: %w", err)
	}
	return new(toFederatedCredentialDTOInternal(&credential)), nil
}

func (s *FederatedCredentialService) Update(ctx context.Context, callerUserID, id string, req federated.UpdateFederatedCredential) (*federated.FederatedCredential, error) {
	var credential FederatedCredential
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&credential).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, common.Classify(common.ErrFederatedCredentialNotFound, errors.New("federated credential not found"))
		}
		return nil, fmt.Errorf("failed to load federated credential: %w", err)
	}

	updated, roleChanged, err := applyFederatedCredentialUpdateInternal(credential, req)
	if err != nil {
		return nil, err
	}
	revokeActiveSessions := credential.Enabled && !updated.Enabled
	if roleChanged {
		if validateRoleGrantAgainstUserErr := s.validateRoleGrantAgainstUserInternal(ctx, callerUserID, updated.RoleID, updated.EnvironmentID); validateRoleGrantAgainstUserErr != nil {
			return nil, validateRoleGrantAgainstUserErr
		}
	}

	err = dbutil.WithTx(ctx, s.db.DB, func(tx *gorm.DB) error {
		if updateCredentialErr := tx.Save(&updated).Error; updateCredentialErr != nil {
			return fmt.Errorf("failed to update federated credential: %w", updateCredentialErr)
		}
		if revokeActiveSessions {
			now := time.Now()
			if revokeCredentialSessionsErr := tx.Model(&session.UserSession{}).
				Where("federated_credential_id = ? AND revoked_at IS NULL", updated.ID).
				Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error; revokeCredentialSessionsErr != nil {
				return fmt.Errorf("failed to revoke federated credential sessions: %w", revokeCredentialSessionsErr)
			}
		}
		if roleChanged {
			if clearRoleAssignmentErr := tx.Where("user_id = ? AND source = ?", updated.IdentityUserID, role.RoleAssignmentSourceManual).
				Delete(&role.UserRoleAssignment{}).Error; clearRoleAssignmentErr != nil {
				return fmt.Errorf("failed to clear federated role assignment: %w", clearRoleAssignmentErr)
			}
			assignment := role.UserRoleAssignment{
				UserID:        updated.IdentityUserID,
				RoleID:        updated.RoleID,
				EnvironmentID: updated.EnvironmentID,
				Source:        role.RoleAssignmentSourceManual,
			}
			if updateRoleAssignmentErr := tx.Create(&assignment).Error; updateRoleAssignmentErr != nil {
				return fmt.Errorf("failed to update federated role assignment: %w", updateRoleAssignmentErr)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if roleChanged && s.roleService != nil {
		s.roleService.InvalidateUser(updated.IdentityUserID)
	}
	if (revokeActiveSessions || roleChanged) && s.authService != nil {
		s.authService.InvalidateUserTokenCache(updated.IdentityUserID)
	}
	return s.Get(ctx, id)
}

func (s *FederatedCredentialService) Delete(ctx context.Context, id string) error {
	var credential FederatedCredential
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&credential).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return common.Classify(common.ErrFederatedCredentialNotFound, errors.New("federated credential not found"))
		}
		return fmt.Errorf("failed to load federated credential: %w", err)
	}

	err := dbutil.WithTx(ctx, s.db.DB, func(tx *gorm.DB) error {
		if err := tx.Delete(&FederatedCredential{}, "id = ?", credential.ID).Error; err != nil {
			return fmt.Errorf("failed to delete federated credential: %w", err)
		}
		if err := tx.Delete(&user.User{}, "id = ?", credential.IdentityUserID).Error; err != nil {
			return fmt.Errorf("failed to delete federated service user: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.roleService != nil {
		s.roleService.InvalidateUser(credential.IdentityUserID)
	}
	if s.authService != nil {
		s.authService.InvalidateUserTokenCache(credential.IdentityUserID)
	}
	return nil
}

func (s *FederatedCredentialService) ExchangeToken(ctx context.Context, req federated.TokenExchangeRequest) (*federated.FederatedTokenResponse, error) {
	claims := jwtclaims.ParseJWTClaims(req.SubjectToken)
	issuer := ""
	subject := ""
	var audiences []string
	if claims != nil {
		issuer = kit.ToString(jwtclaims.GetByPath(claims, "iss").OrEmpty())
		subject = kit.ToString(jwtclaims.GetByPath(claims, "sub").OrEmpty())
		audiences = kit.Unique(kit.TrimNonEmpty(kit.Collect(jwtclaims.GetByPath(claims, "aud").OrEmpty(), func(item any) string { return kit.As(item, "") })))
	}

	logResult := "failure"
	logReason := ""
	var matchedCredential *FederatedCredential
	var matchedUser *user.User
	defer func() {
		s.logExchangeInternal(ctx, logResult, logReason, issuer, subject, audiences, matchedCredential, matchedUser)
	}()

	if req.GrantType != federated.TokenExchangeGrantType || strings.TrimSpace(req.SubjectToken) == "" {
		logReason = "invalid_request"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidRequest, errors.New("invalid federated token exchange request"))
	}
	switch req.SubjectTokenType {
	case federated.SubjectTokenTypeJWT, federated.SubjectTokenTypeIDToken:
	default:
		logReason = "invalid_request"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidRequest, errors.New("invalid federated token exchange request"))
	}
	if req.RequestedTokenType != "" && req.RequestedTokenType != federated.RequestedTokenTypeAccessJWT {
		logReason = "invalid_request"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidRequest, errors.New("invalid federated token exchange request"))
	}
	if issuer == "" {
		logReason = "missing_issuer"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidGrant, errors.New("invalid federated token grant"))
	}

	var credentials []FederatedCredential
	if err := s.db.WithContext(ctx).
		Where("issuer_url = ? AND enabled = ?", issuer, true).
		Order("created_at ASC").
		Order("id ASC").
		Find(&credentials).Error; err != nil {
		logReason = "credential_lookup_failed"
		return nil, fmt.Errorf("failed to list federated credentials for issuer: %w", err)
	}
	now := time.Now()
	active := credentials[:0]
	for _, credential := range credentials {
		if credential.ExpiresAt == nil || !now.After(*credential.ExpiresAt) {
			active = append(active, credential)
		}
	}
	credentials = active
	if len(credentials) == 0 {
		logReason = "issuer_not_allowed"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidGrant, errors.New("invalid federated token grant"))
	}

	verifiedToken, verifiedClaims, err := s.verifySubjectTokenInternal(ctx, issuer, req.SubjectToken)
	if err != nil {
		logReason = "token_verification_failed"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidGrant, fmt.Errorf("invalid federated token grant: %w", err))
	}
	if subject == "" {
		subject = kit.ToString(jwtclaims.GetByPath(verifiedClaims, defaultFederatedSubjectClaim).OrEmpty())
	}
	if len(audiences) == 0 {
		audiences = append([]string{}, verifiedToken.Audience...)
	}

	credential := selectMatchingCredentialInternal(credentials, verifiedToken.Audience, verifiedClaims)
	if credential == nil {
		logReason = "no_matching_credential"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidGrant, errors.New("invalid federated token grant"))
	}
	matchedCredential = credential
	if recordTokenReplayGuardErr := s.recordTokenReplayGuardInternal(ctx, issuer, req.SubjectToken, verifiedClaims, verifiedToken.Expiry); recordTokenReplayGuardErr != nil {
		logReason = "token_replay_rejected"
		return nil, recordTokenReplayGuardErr
	}

	identityUser, err := s.userService.GetUserByID(ctx, credential.IdentityUserID)
	if err != nil {
		logReason = "identity_user_missing"
		return nil, common.Classify(common.ErrFederatedCredentialInvalidGrant, fmt.Errorf("invalid federated token grant: %w", err))
	}
	matchedUser = identityUser

	tokenPair, err := s.authService.IssueFederatedToken(ctx, identityUser, credential.ID, credential.TokenTTLSeconds)
	if err != nil {
		logReason = "token_issue_failed"
		return nil, err
	}

	go func() {
		bgCtx := context.WithoutCancel(ctx)
		localNow := time.Now()
		cutoff := localNow.Add(-federatedCredentialLastUsedWriteWindow)
		if updateLastUsedErr := s.db.WithContext(bgCtx).
			Model(&FederatedCredential{}).
			Where("id = ? AND (last_used_at IS NULL OR last_used_at < ?)", credential.ID, cutoff).
			Update("last_used_at", localNow).Error; updateLastUsedErr != nil {
			slog.WarnContext(bgCtx, "failed to update federated credential last_used_at", "credentialId", credential.ID, "error", updateLastUsedErr)
		}
	}()

	logResult = "success"
	logReason = "matched"
	return &federated.FederatedTokenResponse{
		AccessToken:     tokenPair.AccessToken,
		TokenType:       "Bearer",
		ExpiresIn:       max(int(time.Until(tokenPair.ExpiresAt).Seconds()), 0),
		IssuedTokenType: federated.IssuedTokenTypeAccessToken,
	}, nil
}

func (s *FederatedCredentialService) verifySubjectTokenInternal(ctx context.Context, issuer, rawToken string) (*oidc.IDToken, map[string]any, error) {
	keySet, err := s.keySetForIssuerInternal(ctx, issuer)
	if err != nil {
		return nil, nil, err
	}

	providerCtx := oidc.ClientContext(ctx, s.httpClient)
	verifier := oidc.NewVerifier(issuer, keySet, &oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: oidcjwk.SupportedSigningAlgs(),
	})
	idToken, err := verifier.Verify(providerCtx, rawToken)
	if err != nil {
		return nil, nil, err
	}

	claims := map[string]any{}
	if claimsErr := idToken.Claims(&claims); claimsErr != nil {
		return nil, nil, claimsErr
	}
	return idToken, claims, nil
}

func (s *FederatedCredentialService) recordTokenReplayGuardInternal(ctx context.Context, issuer, rawToken string, claims map[string]any, expiresAt time.Time) error {
	if expiresAt.IsZero() || time.Now().After(expiresAt) {
		return common.Classify(common.ErrFederatedCredentialInvalidGrant, errors.New("invalid federated token grant"))
	}

	now := time.Now()
	if err := s.db.WithContext(ctx).
		Where("expires_at < ?", now).
		Delete(&FederatedTokenReplay{}).Error; err != nil {
		return fmt.Errorf("failed to prune federated token replay records: %w", err)
	}

	tokenID := strings.TrimSpace(kit.ToString(jwtclaims.GetByPath(claims, "jti").OrEmpty()))
	tokenKind := "jti"
	if tokenID == "" {
		tokenID = rawToken
		tokenKind = "token"
	}
	replay := FederatedTokenReplay{
		TokenHash: kit.SHA256Hex(issuer + "\x00" + tokenKind + "\x00" + tokenID),
		IssuerURL: issuer,
		ExpiresAt: expiresAt,
	}
	if err := s.db.WithContext(ctx).Create(&replay).Error; err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "unique") || strings.Contains(message, "duplicate key") {
			return common.Classify(common.ErrFederatedCredentialInvalidGrant, errors.New("invalid federated token grant"))
		}
		return fmt.Errorf("failed to record federated token replay guard: %w", err)
	}
	return nil
}

func (s *FederatedCredentialService) keySetForIssuerInternal(ctx context.Context, issuer string) (oidc.KeySet, error) {
	s.providerMu.RLock()
	if keySet := s.keySets[issuer]; keySet != nil {
		s.providerMu.RUnlock()
		return keySet, nil
	}
	s.providerMu.RUnlock()

	value, err, _ := s.providerGroup.Do(issuer, func() (any, error) {
		providerCtx := oidc.ClientContext(context.WithoutCancel(ctx), s.httpClient)
		provider, err := oidc.NewProvider(providerCtx, issuer)
		if err != nil {
			return nil, fmt.Errorf("failed to discover federated issuer: %w", err)
		}

		var metadata struct {
			JWKSURL string `json:"jwks_uri"`
		}
		if claimsErr := provider.Claims(&metadata); claimsErr != nil {
			return nil, fmt.Errorf("failed to read federated issuer metadata: %w", claimsErr)
		}
		if metadata.JWKSURL == "" {
			return nil, errors.New("federated issuer metadata is missing jwks_uri")
		}
		if s.keySetManager == nil {
			return nil, errors.New("JWK set manager is not configured")
		}

		keySet, err := s.keySetManager.KeySet(context.WithoutCancel(ctx), s.httpClient, metadata.JWKSURL)
		if err != nil {
			return nil, fmt.Errorf("failed to configure federated issuer JWK set: %w", err)
		}
		s.providerMu.Lock()
		s.keySets[issuer] = keySet
		s.providerMu.Unlock()
		return keySet, nil
	})
	if err != nil {
		return nil, err
	}

	keySet, ok := value.(oidc.KeySet)
	if !ok || keySet == nil {
		return nil, errors.New("federated issuer discovery returned invalid key set")
	}
	return keySet, nil
}

func selectMatchingCredentialInternal(credentials []FederatedCredential, tokenAudiences []string, claims map[string]any) *FederatedCredential {
	for i := range credentials {
		credential := &credentials[i]
		if credentialMatchesTokenInternal(credential, tokenAudiences, claims) {
			return credential
		}
	}
	return nil
}

func credentialMatchesTokenInternal(credential *FederatedCredential, tokenAudiences []string, claims map[string]any) bool {
	audiences := make(map[string]struct{}, len(credential.Audiences))
	for _, audience := range credential.Audiences {
		if audience = strings.TrimSpace(audience); audience != "" {
			audiences[audience] = struct{}{}
		}
	}
	audienceMatched := false
	for _, audience := range tokenAudiences {
		if _, audienceMatched = audiences[audience]; audienceMatched {
			break
		}
	}
	if !audienceMatched {
		return false
	}

	subjectClaim := cmp.Or(strings.TrimSpace(credential.SubjectClaim), defaultFederatedSubjectClaim)
	subject := kit.ToString(jwtclaims.GetByPath(claims, subjectClaim).OrEmpty())
	if subject == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(credential.MatchType), federated.MatchTypeGlob) {
		return subject == credential.SubjectMatch
	}

	var expression strings.Builder
	expression.WriteString("^")
	for _, character := range credential.SubjectMatch {
		switch character {
		case '*':
			expression.WriteString(".*")
		case '?':
			expression.WriteByte('.')
		default:
			expression.WriteString(regexp.QuoteMeta(string(character)))
		}
	}
	expression.WriteString("$")
	matched, err := regexp.MatchString(expression.String(), subject)
	return err == nil && matched
}

func (s *FederatedCredentialService) logExchangeInternal(ctx context.Context, result, reason, issuer, subject string, audiences []string, credential *FederatedCredential, identityUser *user.User) {
	credentialID := ""
	credentialName := ""
	if credential != nil {
		credentialID = credential.ID
		credentialName = credential.Name
	}
	slog.InfoContext(ctx, "Federated credential token exchange",
		"result", result,
		"reason", reason,
		"issuer", issuer,
		"subject", subject,
		"audiences", audiences,
		"credentialId", credentialID,
	)

	if s.eventService == nil {
		return
	}

	metadata := database.JSON{
		"action":       "federated_token_exchange",
		"result":       result,
		"reason":       reason,
		"issuer":       issuer,
		"subject":      subject,
		"audiences":    audiences,
		"credentialId": credentialID,
	}

	userID := ""
	username := ""
	if identityUser != nil {
		userID = identityUser.ID
		username = identityUser.Username
	}
	severity := event.EventSeverityInfo
	title := "Federated credential token exchange"
	if result != "success" {
		severity = event.EventSeverityWarning
		title = "Federated credential token exchange rejected"
	}

	go func() {
		bgCtx := context.WithoutCancel(ctx)
		_, err := s.eventService.CreateEvent(bgCtx, event.CreateEventRequest{
			Type:         event.EventTypeFederatedExchange,
			Severity:     severity,
			Title:        title,
			Description:  "Workload identity federation token exchange",
			ResourceType: mo.EmptyableToOption(strings.TrimSpace("federated_credential")).ToPointer(),
			ResourceID:   mo.EmptyableToOption(strings.TrimSpace(credentialID)).ToPointer(),
			ResourceName: mo.EmptyableToOption(strings.TrimSpace(credentialName)).ToPointer(),
			UserID:       mo.EmptyableToOption(strings.TrimSpace(userID)).ToPointer(),
			Username:     mo.EmptyableToOption(strings.TrimSpace(username)).ToPointer(),
			Metadata:     metadata,
		})
		if err != nil {
			slog.WarnContext(bgCtx, "failed to audit federated credential token exchange", "error", err)
		}
	}()
}
