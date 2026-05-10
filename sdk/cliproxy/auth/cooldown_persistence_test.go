package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerMarkResultPersistsAndClearsModelCooldown(t *testing.T) {
	ctx := context.Background()
	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "cooldown-auth", Provider: "codex"}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	retryAfter := time.Hour
	manager.MarkResult(ctx, Result{
		AuthID:     auth.ID,
		Provider:   "codex",
		Model:      "gpt-image-2",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			HTTPStatus: 429,
			Message:    "image generation limit",
		},
	})

	cooldowns := waitForPersistedCooldownCount(t, auth.ID, 1)
	if cooldowns[0].Model != "gpt-image-2" || !cooldowns[0].QuotaExceeded {
		t.Fatalf("cooldown = %+v, want image model quota cooldown", cooldowns[0])
	}

	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		Success:  true,
	})
	cooldowns = waitForPersistedCooldownCount(t, auth.ID, 1)
	if cooldowns[0].Model != "gpt-image-2" {
		t.Fatalf("cooldown model after text success = %q, want gpt-image-2", cooldowns[0].Model)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-image-2",
		Success:  true,
	})
	waitForPersistedCooldownCount(t, auth.ID, 0)
}

func TestManagerRestorePersistedCooldownKeepsImageAndTextModelsSeparate(t *testing.T) {
	ctx := context.Background()
	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), true); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "codex"})

	badAuth := &Auth{ID: "aa-image-cooldown", Provider: "codex"}
	goodAuth := &Auth{ID: "bb-ready", Provider: "codex"}
	if _, err := manager.Register(ctx, badAuth); err != nil {
		t.Fatalf("register bad auth: %v", err)
	}
	if _, err := manager.Register(ctx, goodAuth); err != nil {
		t.Fatalf("register good auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: "gpt-image-2"}, {ID: "gpt-5.4-mini"}}
	reg.RegisterClient(badAuth.ID, "codex", models)
	reg.RegisterClient(goodAuth.ID, "codex", models)
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})
	manager.RefreshSchedulerEntry(badAuth.ID)
	manager.RefreshSchedulerEntry(goodAuth.ID)

	badSnapshot, ok := manager.GetByID(badAuth.ID)
	if !ok {
		t.Fatalf("missing bad auth")
	}
	if err := usagepersist.DefaultStore().UpsertCooldown(ctx, usagepersist.CooldownState{
		AuthID:         badSnapshot.ID,
		AuthIndex:      badSnapshot.EnsureIndex(),
		Provider:       "codex",
		Model:          "gpt-image-2",
		Reason:         "quota",
		StatusMessage:  "image generation limit",
		HTTPStatus:     429,
		NextRetryAfter: time.Now().Add(time.Hour),
		QuotaExceeded:  true,
		BackoffLevel:   1,
	}); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}

	manager.RestorePersistedCooldowns(ctx, badAuth.ID)

	imageValue, err := manager.ExecuteSelectedAuth(ctx, []string{"codex"}, "gpt-image-2", cliproxyexecutor.Options{}, func(_ context.Context, auth *Auth, _ string) (any, error) {
		return auth.ID, nil
	})
	if err != nil {
		t.Fatalf("execute image selected auth: %v", err)
	}
	if imageValue != goodAuth.ID {
		t.Fatalf("image selected auth = %v, want %s", imageValue, goodAuth.ID)
	}

	textValue, err := manager.ExecuteSelectedAuth(ctx, []string{"codex"}, "gpt-5.4-mini", cliproxyexecutor.Options{}, func(_ context.Context, auth *Auth, _ string) (any, error) {
		return auth.ID, nil
	})
	if err != nil {
		t.Fatalf("execute text selected auth: %v", err)
	}
	if textValue != badAuth.ID {
		t.Fatalf("text selected auth = %v, want %s", textValue, badAuth.ID)
	}
}

func TestManagerRestorePersistedCooldownMigratesLegacyAuthIndex(t *testing.T) {
	ctx := context.Background()
	if err := usagepersist.Init(filepath.Join(t.TempDir(), "usage.sqlite"), true); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	manager := NewManager(nil, nil, nil)
	authPath := filepath.Join(t.TempDir(), "codex-user.json")
	auth := &Auth{
		ID:       "legacy-codex-user.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":   authPath,
			"source": authPath,
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-image-2"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	currentSnapshot, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("missing auth")
	}
	currentIndex := currentSnapshot.EnsureIndex()
	legacyIndex := stableAuthIndex("config:" + strings.Join([]string{
		"provider=codex",
		"source=" + authPath,
	}, "\x00"))
	if legacyIndex == "" || legacyIndex == currentIndex {
		t.Fatalf("legacy index = %q, current index = %q; want distinct non-empty indexes", legacyIndex, currentIndex)
	}

	nextRetry := time.Now().Add(time.Hour)
	if err := usagepersist.DefaultStore().UpsertCooldown(ctx, usagepersist.CooldownState{
		AuthID:         auth.ID,
		AuthIndex:      legacyIndex,
		Provider:       "codex",
		Model:          "gpt-image-2",
		Reason:         "quota",
		StatusMessage:  "image generation limit",
		HTTPStatus:     429,
		NextRetryAfter: nextRetry,
		QuotaExceeded:  true,
		BackoffLevel:   1,
	}); err != nil {
		t.Fatalf("upsert legacy cooldown: %v", err)
	}

	manager.RestorePersistedCooldowns(ctx, auth.ID)

	restored, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("missing restored auth")
	}
	state := restored.ModelStates["gpt-image-2"]
	if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
		t.Fatalf("model state = %#v, want restored cooldown", state)
	}

	waitForPersistedCooldownAuthIndex(t, auth.ID, "gpt-image-2", currentIndex)
}

func TestPersistedCooldownMatchesLegacyConfigAuthIndex(t *testing.T) {
	auth := &Auth{
		ID:       "gemini-key-auth",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[token]",
		},
	}
	currentIndex := auth.EnsureIndex()
	legacyIndex := stableAuthIndex("config:" + strings.Join([]string{
		"provider=gemini",
		"api_key=shared-key",
		"source=config:gemini[token]",
	}, "\x00"))
	if legacyIndex == "" || legacyIndex == currentIndex {
		t.Fatalf("legacy index = %q, current index = %q; want distinct non-empty indexes", legacyIndex, currentIndex)
	}

	matched, migrate := persistedCooldownMatchesAuth(auth, currentIndex, usagepersist.CooldownState{
		AuthID:    auth.ID,
		AuthIndex: legacyIndex,
		Provider:  "gemini",
	})
	if !matched || !migrate {
		t.Fatalf("legacy config auth index matched=%v migrate=%v, want true/true", matched, migrate)
	}
}

func waitForPersistedCooldownCount(t *testing.T, authID string, want int) []usagepersist.CooldownState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last []usagepersist.CooldownState
	var lastErr error
	for time.Now().Before(deadline) {
		store := usagepersist.DefaultStore()
		if store == nil {
			t.Fatalf("usage persistence store is not initialized")
		}
		last, lastErr = store.ActiveCooldownsByAuth(context.Background(), authID, time.Now())
		if lastErr == nil && len(last) == want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("cooldown count = %d, want %d, last error: %v", len(last), want, lastErr)
	}
	t.Fatalf("cooldown count = %d, want %d; last=%+v", len(last), want, last)
	return nil
}

func waitForPersistedCooldownAuthIndex(t *testing.T, authID string, model string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last []usagepersist.CooldownState
	var lastErr error
	for time.Now().Before(deadline) {
		store := usagepersist.DefaultStore()
		if store == nil {
			t.Fatalf("usage persistence store is not initialized")
		}
		last, lastErr = store.ActiveCooldownsByAuth(context.Background(), authID, time.Now())
		if lastErr == nil {
			for _, cooldown := range last {
				if cooldown.Model == model && cooldown.AuthIndex == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("cooldown auth index not migrated to %q, last error: %v", want, lastErr)
	}
	t.Fatalf("cooldown auth index not migrated to %q; last=%+v", want, last)
}
