package imagesfallback

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
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
	if status := StatusCode(err); status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil, err
	}

	log.WithField("auth_id", authID).Debug("images fallback: retrying after codex oauth token refresh")
	auth, errRefresh := RefreshAccessTokenIfNeeded(ctx, s.authManager, auth, true)
	if errRefresh != nil {
		return nil, fmt.Errorf("refresh codex oauth token after fallback auth failure: %w", errRefresh)
	}
	return s.executeWithChatGPTImage(ctx, auth, req)
}
