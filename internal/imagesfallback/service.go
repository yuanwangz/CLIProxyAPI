package imagesfallback

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

type Service struct {
	cfg         *sdkconfig.SDKConfig
	authManager *coreauth.Manager
}

func NewService(cfg *sdkconfig.SDKConfig, authManager *coreauth.Manager) *Service {
	return &Service{
		cfg:         cfg,
		authManager: authManager,
	}
}

func (s *Service) Execute(ctx context.Context, authID string, req Request) (*Result, error) {
	if s == nil || s.authManager == nil {
		return nil, newStatusError(http.StatusInternalServerError, "image fallback service is unavailable")
	}

	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, newStatusError(http.StatusUnauthorized, "selected auth is required for image fallback")
	}

	auth, ok := s.authManager.GetByID(authID)
	if !ok || auth == nil {
		return nil, newStatusError(http.StatusUnauthorized, "selected auth not found for image fallback")
	}
	return s.executeWithAuth(ctx, auth, req)
}

func (s *Service) ExecuteWithAuthManager(ctx context.Context, req Request, selectedCallback func(string)) (*Result, error) {
	if s == nil || s.authManager == nil {
		return nil, newStatusError(http.StatusInternalServerError, "image fallback service is unavailable")
	}

	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = "gpt-image-2"
	}
	metadata := map[string]any{
		coreexecutor.RequestedModelMetadataKey: model,
		// Web-image authorization failures are endpoint-specific and must not disable text credentials.
		coreexecutor.SkipSelectedAuthResultMetadataKey: true,
	}
	if selectedCallback != nil {
		metadata[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	opts := coreexecutor.Options{Metadata: metadata}
	value, err := s.authManager.ExecuteSelectedAuth(ctx, []string{"codex"}, TextAuthSelectionModel, opts, func(execCtx context.Context, auth *coreauth.Auth, _ string) (any, error) {
		if !IsCodexOAuthAuth(auth) {
			return nil, &coreauth.SkipSelectedAuthError{Reason: "image fallback requires a Codex OAuth auth"}
		}
		return s.executeWithAuth(execCtx, auth, req)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*Result)
	if !ok || result == nil {
		return nil, newStatusError(http.StatusBadGateway, "image fallback returned an invalid result")
	}
	return result, nil
}

func (s *Service) executeWithAuth(ctx context.Context, auth *coreauth.Auth, req Request) (*Result, error) {
	if !IsCodexOAuthAuth(auth) {
		return nil, newStatusError(http.StatusBadRequest, "image fallback requires a Codex OAuth auth")
	}

	auth, err := RefreshAccessTokenIfNeeded(ctx, s.authManager, auth, false)
	if err != nil {
		return nil, fmt.Errorf("refresh codex oauth token: %w", err)
	}

	result, err := s.executeWithChatGPTImage(ctx, auth, req)
	if err == nil {
		return result, nil
	}
	err = NormalizeExecutionError(err)
	if status := StatusCode(err); status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil, err
	}

	log.WithField("auth_id", auth.ID).Debug("images fallback: retrying after codex oauth token refresh")
	auth, errRefresh := RefreshAccessTokenIfNeeded(ctx, s.authManager, auth, true)
	if errRefresh != nil {
		return nil, fmt.Errorf("refresh codex oauth token after fallback auth failure: %w", errRefresh)
	}
	result, err = s.executeWithChatGPTImage(ctx, auth, req)
	if err != nil {
		return nil, NormalizeExecutionError(err)
	}
	return result, nil
}
