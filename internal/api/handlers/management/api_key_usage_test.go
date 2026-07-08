package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func sumRecentRequestBuckets(buckets []coreauth.RecentRequestBucket) (int64, int64) {
	var success int64
	var failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	return success, failed
}

func TestGetAPIKeyUsage_GroupsByProviderAndAPIKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "codex-key",
			"base_url": "https://codex.example.com",
		},
	}); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "claude-key",
			"base_url": "https://claude.example.com",
		},
	}); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}

	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: false})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "claude-auth", Provider: "claude", Model: "claude-4", Success: true})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	codexEntry := payload["codex"]["https://codex.example.com|codex-key"]
	if codexEntry.Success != 1 || codexEntry.Failed != 1 {
		t.Fatalf("codex totals = %d/%d, want 1/1", codexEntry.Success, codexEntry.Failed)
	}
	if len(codexEntry.RecentRequests) != 20 {
		t.Fatalf("codex buckets len = %d, want 20", len(codexEntry.RecentRequests))
	}
	codexSuccess, codexFailed := sumRecentRequestBuckets(codexEntry.RecentRequests)
	if codexSuccess != 1 || codexFailed != 1 {
		t.Fatalf("codex totals = %d/%d, want 1/1", codexSuccess, codexFailed)
	}

	claudeEntry := payload["claude"]["https://claude.example.com|claude-key"]
	if claudeEntry.Success != 1 || claudeEntry.Failed != 0 {
		t.Fatalf("claude totals = %d/%d, want 1/0", claudeEntry.Success, claudeEntry.Failed)
	}
	if len(claudeEntry.RecentRequests) != 20 {
		t.Fatalf("claude buckets len = %d, want 20", len(claudeEntry.RecentRequests))
	}
	claudeSuccess, claudeFailed := sumRecentRequestBuckets(claudeEntry.RecentRequests)
	if claudeSuccess != 1 || claudeFailed != 0 {
		t.Fatalf("claude totals = %d/%d, want 1/0", claudeSuccess, claudeFailed)
	}
}

func TestGetAPIKeyUsage_RestoresPersistedStatsAfterRestart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), true); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}
	usagepersist.SetEnabled(false)
	t.Cleanup(func() {
		usagepersist.SetEnabled(false)
	})

	authRecord := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "codex-key",
			"base_url": "https://codex.example.com",
		},
	}
	authIndex := authRecord.EnsureIndex()
	now := time.Now().UTC()
	_, err := usagepersist.InsertEvents(context.Background(), []usagepersist.Event{
		{
			RequestID:   "persisted-success",
			Timestamp:   now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			TimestampMS: now.Add(-2 * time.Minute).UnixMilli(),
			Provider:    "codex",
			Model:       "gpt-5",
			AuthType:    "api_key",
			AuthIndex:   authIndex,
			TotalTokens: 10,
		},
		{
			RequestID:   "persisted-failed",
			Timestamp:   now.Add(-1 * time.Minute).Format(time.RFC3339Nano),
			TimestampMS: now.Add(-1 * time.Minute).UnixMilli(),
			Provider:    "codex",
			Model:       "gpt-5",
			AuthType:    "api_key",
			AuthIndex:   authIndex,
			Failed:      true,
			TotalTokens: 2,
		},
	})
	if err != nil {
		t.Fatalf("insert persisted events: %v", err)
	}

	// Simulate a fresh process: the live auth has no in-memory success/failed counts.
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), authRecord); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}

	entry := payload["codex"]["https://codex.example.com|codex-key"]
	if entry.Success != 1 || entry.Failed != 1 {
		t.Fatalf("persisted totals = %d/%d, want 1/1", entry.Success, entry.Failed)
	}
	if len(entry.RecentRequests) != 20 {
		t.Fatalf("recent buckets len = %d, want 20", len(entry.RecentRequests))
	}
	success, failed := sumRecentRequestBuckets(entry.RecentRequests)
	if success != 1 || failed != 1 {
		t.Fatalf("persisted bucket totals = %d/%d, want 1/1", success, failed)
	}
}

func TestGetAPIKeyUsage_IncludesAPIKeyCooldownStatus(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	coreauth.SetQuotaCooldownDisabled(false)
	t.Cleanup(func() {
		coreauth.SetQuotaCooldownDisabled(false)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	authRecord := &coreauth.Auth{
		ID:       "claude-cooldown-auth",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "claude-key",
			"base_url": "https://claude.example.com",
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), authRecord); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}
	authIndex := authRecord.EnsureIndex()
	retryAfter := 5 * time.Minute
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:     "claude-cooldown-auth",
		Provider:   "claude",
		Model:      "claude-opus-4-8",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &coreauth.Error{
			Message:    "rate limit exceeded",
			HTTPStatus: http.StatusTooManyRequests,
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	entry := payload["claude"]["https://claude.example.com|claude-key"]
	if entry.AuthID != "claude-cooldown-auth" || entry.AuthIndex != authIndex {
		t.Fatalf("auth identity = %q/%q, want claude-cooldown-auth/%s", entry.AuthID, entry.AuthIndex, authIndex)
	}
	if !entry.Blocked || !entry.Cooling || entry.BlockReason != "quota" {
		t.Fatalf("cooldown status = blocked:%v cooling:%v reason:%q, want quota cooldown", entry.Blocked, entry.Cooling, entry.BlockReason)
	}
	if entry.NextRetryMS == 0 || entry.NextRetryAfter == "" {
		t.Fatalf("next retry not populated: %+v", entry)
	}
	if len(entry.ModelStates) != 1 || entry.ModelStates[0].Model != "claude-opus-4-8" {
		t.Fatalf("model states = %+v, want claude-opus-4-8", entry.ModelStates)
	}
	if !entry.ModelStates[0].Cooling || entry.ModelStates[0].StatusCode != http.StatusTooManyRequests {
		t.Fatalf("model cooldown = %+v, want 429 quota cooldown", entry.ModelStates[0])
	}
}

func TestClearAPIKeyUsageCooldown_ClearsRecoverableBlocks(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	coreauth.SetQuotaCooldownDisabled(false)
	t.Cleanup(func() {
		coreauth.SetQuotaCooldownDisabled(false)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	authRecord := &coreauth.Auth{
		ID:       "claude-clear-auth",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "claude-key",
			"base_url": "https://claude.example.com",
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), authRecord); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}
	authIndex := authRecord.EnsureIndex()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	retryAfter := 5 * time.Minute
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:     "claude-clear-auth",
		Provider:   "claude",
		Model:      "claude-opus-4-8",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &coreauth.Error{
			Message:    "rate limit exceeded",
			HTTPStatus: http.StatusTooManyRequests,
		},
	})

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	body := `{"provider":"claude","auth_index":"` + authIndex + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-key-usage/clear-cooldown", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.ClearAPIKeyUsageCooldown(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	updated, ok := manager.GetByID("claude-clear-auth")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if updated.Quota.Exceeded || updated.Unavailable || len(updated.ModelStates) != 0 {
		t.Fatalf("quota cooldown should be cleared, got %+v", updated)
	}

	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:   "claude-clear-auth",
		Provider: "claude",
		Model:    "claude-opus-4-8",
		Success:  false,
		Error: &coreauth.Error{
			Message:    "model not found",
			HTTPStatus: http.StatusNotFound,
		},
	})

	rec = httptest.NewRecorder()
	ginCtx, _ = gin.CreateTestContext(rec)
	req = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-usage/clear-cooldown", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.ClearAPIKeyUsageCooldown(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("clear not_found status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	updated, ok = manager.GetByID("claude-clear-auth")
	if !ok {
		t.Fatal("updated auth after not_found not found")
	}
	if len(updated.ModelStates) != 0 || updated.Unavailable || updated.Status != coreauth.StatusActive {
		t.Fatalf("not_found model state should be cleared, got %+v", updated)
	}

	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:   "claude-clear-auth",
		Provider: "claude",
		Model:    "claude-opus-4-8",
		Success:  false,
		Error: &coreauth.Error{
			Message:    "unauthorized",
			HTTPStatus: http.StatusUnauthorized,
		},
	})

	rec = httptest.NewRecorder()
	ginCtx, _ = gin.CreateTestContext(rec)
	req = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-usage/clear-cooldown", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.ClearAPIKeyUsageCooldown(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("clear unauthorized status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	updated, ok = manager.GetByID("claude-clear-auth")
	if !ok {
		t.Fatal("updated auth after unauthorized not found")
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled || updated.StatusMessage != "unauthorized" {
		t.Fatalf("unauthorized state should remain disabled, got %+v", updated)
	}
}
func TestGetAPIKeyUsage_GroupsOpenAICompatibleByCompatName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "vast-auth",
		Provider: "openai-compatible-vast",
		Attributes: map[string]string{
			"api_key":     "vast-key",
			"base_url":    "https://www.vastnum.com/v1",
			"compat_name": "VAST",
		},
	}); err != nil {
		t.Fatalf("register vast auth: %v", err)
	}

	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "vast-auth", Provider: "openai-compatible-vast", Model: "gpt-5", Success: true})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if _, exists := payload["openai-compatible-vast"]; exists {
		t.Fatalf("unexpected namespaced provider bucket in payload: %#v", payload)
	}
	vastBucket, exists := payload["vast"]
	if !exists {
		t.Fatalf("missing compat provider bucket in payload: %#v", payload)
	}
	vastEntry := vastBucket["https://www.vastnum.com/v1|vast-key"]
	if vastEntry.Success != 1 || vastEntry.Failed != 0 {
		t.Fatalf("vast totals = %d/%d, want 1/0", vastEntry.Success, vastEntry.Failed)
	}
}
