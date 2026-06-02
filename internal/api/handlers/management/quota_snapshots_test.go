package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPutQuotaSnapshotClearsAvailableQuotaCooldown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	nextRetry := time.Now().Add(time.Hour).UTC()
	authRecord := &coreauth.Auth{
		ID:               "quota-auth",
		Provider:         "codex",
		Status:           coreauth.StatusError,
		StatusMessage:    "quota exhausted",
		Unavailable:      true,
		NextRetryAfter:   nextRetry,
		Quota:            coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: nextRetry, BackoffLevel: 2},
		LastError:        &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		NextRefreshAfter: time.Now().Add(time.Hour),
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: nextRetry, BackoffLevel: 2},
			},
		},
	}
	authRecord.EnsureIndex()
	if _, err := manager.Register(ctx, authRecord); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	store := usagepersist.DefaultStore()
	if store == nil {
		t.Fatal("default usage store is nil")
	}
	if err := store.UpsertCooldown(ctx, usagepersist.CooldownState{
		AuthID:         authRecord.ID,
		AuthIndex:      authRecord.Index,
		Provider:       "codex",
		Model:          "gpt-5",
		Reason:         "quota",
		HTTPStatus:     http.StatusTooManyRequests,
		NextRetryAfter: nextRetry,
		QuotaExceeded:  true,
	}); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	body := `{"provider":"codex","auth_id":"quota-auth","auth_index":"` + authRecord.Index + `","quota":{"status":"success","windows":[{"id":"five-hour","usedPercent":20,"resetAt":` + strconvFormatUnix(time.Now().Add(2*time.Hour)) + `,"windowMinutes":300}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PutQuotaSnapshot(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	active, err := store.ActiveCooldownsByAuth(ctx, authRecord.ID, time.Now())
	if err != nil {
		t.Fatalf("active cooldowns: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active cooldowns = %+v, want none", active)
	}

	updated, ok := manager.GetByID(authRecord.ID)
	if !ok {
		t.Fatalf("expected auth %q", authRecord.ID)
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || updated.LastError != nil || len(updated.ModelStates) != 0 {
		t.Fatalf("updated auth = %+v, want active with quota state cleared", updated)
	}
}

func TestPutQuotaSnapshotDoesNotClearDisabledUnauthorizedAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	nextRetry := time.Now().Add(time.Hour).UTC()
	authRecord := &coreauth.Auth{
		ID:            "disabled-auth",
		Provider:      "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "unauthorized",
		LastError:     &coreauth.Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	}
	authRecord.EnsureIndex()
	if _, err := manager.Register(ctx, authRecord); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	store := usagepersist.DefaultStore()
	if store == nil {
		t.Fatal("default usage store is nil")
	}
	if err := store.UpsertCooldown(ctx, usagepersist.CooldownState{
		AuthID:         authRecord.ID,
		AuthIndex:      authRecord.Index,
		Provider:       "codex",
		Model:          "gpt-5",
		Reason:         "quota",
		HTTPStatus:     http.StatusTooManyRequests,
		NextRetryAfter: nextRetry,
		QuotaExceeded:  true,
	}); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	body := `{"provider":"codex","auth_id":"disabled-auth","auth_index":"` + authRecord.Index + `","quota":{"status":"success","windows":[{"id":"five-hour","usedPercent":20,"resetAt":` + strconvFormatUnix(time.Now().Add(2*time.Hour)) + `,"windowMinutes":300}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PutQuotaSnapshot(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	active, err := store.ActiveCooldownsByAuth(ctx, authRecord.ID, time.Now())
	if err != nil {
		t.Fatalf("active cooldowns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active cooldowns = %+v, want disabled auth cooldown preserved", active)
	}

	updated, ok := manager.GetByID(authRecord.ID)
	if !ok {
		t.Fatalf("expected auth %q", authRecord.ID)
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled || updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("updated disabled auth = %+v, want unauthorized disabled state preserved", updated)
	}
}

func TestPutQuotaSnapshotDoesNotClearExhaustedQuotaCooldown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	nextRetry := time.Now().Add(time.Hour).UTC()
	authRecord := &coreauth.Auth{
		ID:             "exhausted-auth",
		Provider:       "codex",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: nextRetry,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: nextRetry, BackoffLevel: 2},
		LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
	}
	authRecord.EnsureIndex()
	if _, err := manager.Register(ctx, authRecord); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	store := usagepersist.DefaultStore()
	if store == nil {
		t.Fatal("default usage store is nil")
	}
	if err := store.UpsertCooldown(ctx, usagepersist.CooldownState{
		AuthID:         authRecord.ID,
		AuthIndex:      authRecord.Index,
		Provider:       "codex",
		Model:          "gpt-5",
		Reason:         "quota",
		HTTPStatus:     http.StatusTooManyRequests,
		NextRetryAfter: nextRetry,
		QuotaExceeded:  true,
	}); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	body := `{"provider":"codex","auth_id":"exhausted-auth","auth_index":"` + authRecord.Index + `","quota":{"status":"success","windows":[{"id":"five-hour","allowed":false,"unlimited":false,"resetAt":` + strconvFormatUnix(time.Now().Add(2*time.Hour)) + `,"windowMinutes":300}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PutQuotaSnapshot(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	active, err := store.ActiveCooldownsByAuth(ctx, authRecord.ID, time.Now())
	if err != nil {
		t.Fatalf("active cooldowns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active cooldowns = %+v, want exhausted cooldown preserved", active)
	}

	updated, ok := manager.GetByID(authRecord.ID)
	if !ok {
		t.Fatalf("expected auth %q", authRecord.ID)
	}
	if updated.Status != coreauth.StatusError || !updated.Unavailable || updated.LastError == nil {
		t.Fatalf("updated exhausted auth = %+v, want quota state preserved", updated)
	}
}

func TestPutQuotaSnapshotResolvesAuthIDFromAuthIndexForRoutingHint(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	ctx := context.Background()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	authRecord := &coreauth.Auth{
		ID:       "quota-auth-index-only",
		Provider: "codex",
		FileName: "quota-auth-index-only.json",
	}
	authRecord.EnsureIndex()
	t.Cleanup(func() { coreauth.ClearQuotaRoutingHintForTest(authRecord.ID) })
	if _, err := manager.Register(ctx, authRecord); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	resetAt := time.Now().Add(90 * time.Minute).UTC()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	body := `{"provider":"codex","auth_index":"` + authRecord.Index + `","quota":{"status":"success","windows":[{"id":"five-hour","usedPercent":20,"resetAt":` + strconvFormatUnix(resetAt) + `,"windowMinutes":300}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota-snapshots", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ginCtx.Request = req
	h.PutQuotaSnapshot(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	snapshots, errSnapshots := usagepersist.QuotaSnapshots(ctx)
	if errSnapshots != nil {
		t.Fatalf("quota snapshots: %v", errSnapshots)
	}
	if len(snapshots) != 1 {
		t.Fatalf("quota snapshots = %+v, want one snapshot", snapshots)
	}
	if snapshots[0].AuthID != authRecord.ID {
		t.Fatalf("snapshot AuthID = %q, want %q", snapshots[0].AuthID, authRecord.ID)
	}

	hint, ok := coreauth.GetQuotaRoutingHint(authRecord.ID)
	if !ok {
		t.Fatalf("expected routing hint for %q", authRecord.ID)
	}
	if !hint.ResetAt.Equal(time.Unix(resetAt.Unix(), 0).UTC()) {
		t.Fatalf("routing hint ResetAt = %v, want %v", hint.ResetAt, time.Unix(resetAt.Unix(), 0).UTC())
	}
	if hint.Window != 5*time.Hour {
		t.Fatalf("routing hint Window = %v, want 5h", hint.Window)
	}
}

func strconvFormatUnix(value time.Time) string {
	return strconv.FormatInt(value.Unix(), 10)
}
