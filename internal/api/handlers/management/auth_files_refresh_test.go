package management

import (
	"context"
	"encoding/json"
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

func TestPatchAuthFileStatus_EnableClearsUnauthorizedStatusCode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "enable.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex","disabled":true}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID:            "enable-id",
		FileName:      "enable.json",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "unauthorized",
		LastError:     &coreauth.Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"enable.json","disabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record %q after status update", record.ID)
	}
	if updated.Disabled {
		t.Fatal("Disabled = true, want false after enabling")
	}
	if updated.LastError != nil {
		t.Fatalf("LastError = %#v, want nil after enabling", updated.LastError)
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(listCtx)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one file entry, payload: %#v", payload)
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}
	if _, ok := fileEntry["status_code"]; ok {
		t.Fatalf("status_code should be omitted after enabling, entry: %#v", fileEntry)
	}
}

func TestPatchAuthFileStatus_ArchivePreservesUnauthorizedDisabledState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "archive.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex","disabled":true}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID:            "archive-id",
		FileName:      "archive.json",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "unauthorized",
		LastError:     &coreauth.Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"archive.json","archived":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record %q after archive update", record.ID)
	}
	if !updated.Archived {
		t.Fatal("Archived=false, want true")
	}
	if !updated.Disabled {
		t.Fatal("Disabled=false, want true")
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("LastError=%#v, want HTTP 401", updated.LastError)
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(listCtx)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one file entry, payload: %#v", payload)
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}
	if archived, _ := fileEntry["archived"].(bool); !archived {
		t.Fatalf("archived=%#v, want true in list entry %#v", fileEntry["archived"], fileEntry)
	}
	if disabled, _ := fileEntry["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%#v, want true in list entry %#v", fileEntry["disabled"], fileEntry)
	}
	if status, _ := fileEntry["status"].(string); status != string(coreauth.StatusArchived) {
		t.Fatalf("status=%#v, want %q in list entry %#v", fileEntry["status"], coreauth.StatusArchived, fileEntry)
	}
	if statusCode, _ := fileEntry["status_code"].(float64); int(statusCode) != http.StatusUnauthorized {
		t.Fatalf("status_code=%#v, want %d in list entry %#v", fileEntry["status_code"], http.StatusUnauthorized, fileEntry)
	}

	unarchiveRec := httptest.NewRecorder()
	unarchiveCtx, _ := gin.CreateTestContext(unarchiveRec)
	unarchiveReq := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"archive.json","archived":false}`))
	unarchiveReq.Header.Set("Content-Type", "application/json")
	unarchiveCtx.Request = unarchiveReq
	h.PatchAuthFileStatus(unarchiveCtx)
	if unarchiveRec.Code != http.StatusOK {
		t.Fatalf("expected unarchive status %d, got %d with body %s", http.StatusOK, unarchiveRec.Code, unarchiveRec.Body.String())
	}
	updated, ok = manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record %q after unarchive update", record.ID)
	}
	if updated.Archived {
		t.Fatal("Archived=true, want false after unarchive")
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled {
		t.Fatalf("auth should remain disabled after unarchive, got Disabled=%v Status=%q", updated.Disabled, updated.Status)
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("LastError=%#v, want HTTP 401 after unarchive", updated.LastError)
	}
}

func TestRefreshAuthFile_ArchivedAuthIsRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "archived-refresh.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex","archived":true}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	manager.RegisterExecutor(refreshAuthFileTestExecutor{provider: "codex"})
	record := &coreauth.Auth{
		ID:       "archived-refresh-id",
		FileName: "archived-refresh.json",
		Provider: "codex",
		Archived: true,
		Status:   coreauth.StatusArchived,
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/refresh", strings.NewReader(`{"name":"archived-refresh.json"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshAuthFile(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record %q after refresh rejection", record.ID)
	}
	if refreshed, _ := updated.Metadata["refreshed"].(bool); refreshed {
		t.Fatal("archived auth should not be refreshed")
	}
}
