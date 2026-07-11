package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesRecentRequestsBuckets(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       "runtime-only-auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", payload)
	}
	if len(filesRaw) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(filesRaw))
	}

	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if _, ok := fileEntry["success"].(float64); !ok {
		t.Fatalf("expected success number, got %#v", fileEntry["success"])
	}
	if _, ok := fileEntry["failed"].(float64); !ok {
		t.Fatalf("expected failed number, got %#v", fileEntry["failed"])
	}

	recentRaw, ok := fileEntry["recent_requests"].([]any)
	if !ok {
		t.Fatalf("expected recent_requests array, got %#v", fileEntry["recent_requests"])
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets, got %d", len(recentRaw))
	}
	for idx, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected bucket object at %d, got %#v", idx, item)
		}
		if _, ok := bucket["time"].(string); !ok {
			t.Fatalf("expected bucket time string at %d, got %#v", idx, bucket["time"])
		}
		if _, ok := bucket["success"].(float64); !ok {
			t.Fatalf("expected bucket success number at %d, got %#v", idx, bucket["success"])
		}
		if _, ok := bucket["failed"].(float64); !ok {
			t.Fatalf("expected bucket failed number at %d, got %#v", idx, bucket["failed"])
		}
	}
}

func TestListAuthFiles_ExposesXAIIdentityWithoutTokens(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:         "xai-runtime-auth",
		Provider:   "xai",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"type":          "xai",
			"sub":           "xai-user-123",
			"access_token":  "secret-access-token",
			"refresh_token": "secret-refresh-token",
			"id_token":      "secret-id-token",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register xAI auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}
	entry := h.buildAuthFileEntry(record)

	if subject, _ := entry["sub"].(string); subject != "xai-user-123" {
		t.Fatalf("sub = %#v, want xai-user-123", entry["sub"])
	}
	for _, secretField := range []string{"access_token", "refresh_token", "id_token"} {
		if _, exists := entry[secretField]; exists {
			t.Fatalf("auth file entry exposed secret field %q: %#v", secretField, entry)
		}
	}
}

func TestListAuthFiles_HidesExpiredCooldownFromManagementStatus(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:              "expired-cooldown-auth",
		Provider:        "codex",
		Status:          coreauth.StatusError,
		StatusMessage:   "quota exhausted",
		Unavailable:     true,
		NextRetryAfter:  time.Now().Add(-time.Minute),
		Quota:           coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: time.Now().Add(-time.Minute)},
		Attributes:      map[string]string{"runtime_only": "true"},
		Metadata:        map[string]any{"type": "codex"},
		LastRefreshedAt: time.Now().Add(-time.Hour),
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one file entry, payload: %#v", payload)
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if unavailable, _ := fileEntry["unavailable"].(bool); unavailable {
		t.Fatalf("expected expired cooldown to be exposed as available, entry: %#v", fileEntry)
	}
	if status, _ := fileEntry["status"].(string); status != string(coreauth.StatusActive) {
		t.Fatalf("status = %q, want %q", status, coreauth.StatusActive)
	}
	if message, _ := fileEntry["status_message"].(string); message != "" {
		t.Fatalf("status_message = %q, want empty", message)
	}
	if _, ok := fileEntry["next_retry_after"]; ok {
		t.Fatalf("next_retry_after should be omitted for expired cooldown, entry: %#v", fileEntry)
	}
	if _, ok := fileEntry["status_code"]; ok {
		t.Fatalf("status_code should be omitted for expired cooldown, entry: %#v", fileEntry)
	}
}

func TestListAuthFiles_ExposesUnauthorizedStatusCode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:             "unauthorized-auth",
		Provider:       "claude",
		Status:         coreauth.StatusError,
		StatusMessage:  "unauthorized",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(30 * time.Minute),
		LastError:      &coreauth.Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
		Attributes:     map[string]string{"runtime_only": "true"},
		Metadata:       map[string]any{"type": "claude"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one file entry, payload: %#v", payload)
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if statusCode, _ := fileEntry["status_code"].(float64); int(statusCode) != http.StatusUnauthorized {
		t.Fatalf("status_code = %#v, want %d; entry: %#v", fileEntry["status_code"], http.StatusUnauthorized, fileEntry)
	}
}
