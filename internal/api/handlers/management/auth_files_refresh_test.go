package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type refreshAuthFileTestExecutor struct {
	provider string
}

func (e refreshAuthFileTestExecutor) Identifier() string { return e.provider }

func (e refreshAuthFileTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e refreshAuthFileTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e refreshAuthFileTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["refreshed"] = true
	return auth, nil
}

func (e refreshAuthFileTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e refreshAuthFileTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRefreshAuthFile_ReenablesUnauthorizedDisabledAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "refresh.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	manager.RegisterExecutor(refreshAuthFileTestExecutor{provider: "codex"})
	record := &coreauth.Auth{
		ID:            "refresh-id",
		FileName:      "refresh.json",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "unauthorized",
		LastError:     &coreauth.Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
		Attributes: map[string]string{
			"path": authPath,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh", strings.NewReader(`{"name":"refresh.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record %q after refresh", record.ID)
	}
	if updated.Disabled {
		t.Fatal("Disabled = true, want false after manual refresh")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.LastError != nil {
		t.Fatalf("LastError = %#v, want nil", updated.LastError)
	}
}
