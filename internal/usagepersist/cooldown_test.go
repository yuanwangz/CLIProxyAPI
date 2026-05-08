package usagepersist

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCooldownLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("close store: %v", errClose)
		}
	}()

	nextRetry := time.Now().Add(time.Hour).UTC()
	state := CooldownState{
		AuthID:         "auth-1",
		AuthIndex:      "idx-1",
		Provider:       "codex",
		Model:          "gpt-image-2",
		Reason:         "quota",
		StatusMessage:  "image generation limit",
		HTTPStatus:     429,
		NextRetryAfter: nextRetry,
		QuotaExceeded:  true,
		BackoffLevel:   2,
	}
	if err := store.UpsertCooldown(ctx, state); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}

	active, err := store.ActiveCooldownsByAuth(ctx, "auth-1", time.Now())
	if err != nil {
		t.Fatalf("active cooldowns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active cooldowns len = %d, want 1", len(active))
	}
	got := active[0]
	if got.Model != state.Model || got.Reason != state.Reason || !got.QuotaExceeded || got.BackoffLevel != state.BackoffLevel {
		t.Fatalf("cooldown = %+v, want model/reason/quota/backoff from %+v", got, state)
	}

	if err := store.DeleteCooldown(ctx, "auth-1", "gpt-image-2"); err != nil {
		t.Fatalf("delete cooldown: %v", err)
	}
	active, err = store.ActiveCooldownsByAuth(ctx, "auth-1", time.Now())
	if err != nil {
		t.Fatalf("active cooldowns after delete: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active cooldowns after delete len = %d, want 0", len(active))
	}
}

func TestStoreCooldownIgnoresExpiredRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("close store: %v", errClose)
		}
	}()

	if err := store.UpsertCooldown(ctx, CooldownState{
		AuthID:         "auth-1",
		Provider:       "codex",
		Model:          "gpt-5.4-mini",
		NextRetryAfter: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("upsert expired cooldown: %v", err)
	}
	active, err := store.ActiveCooldownsByAuth(ctx, "auth-1", time.Now())
	if err != nil {
		t.Fatalf("active cooldowns: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active cooldowns len = %d, want 0", len(active))
	}
}

func TestCooldownAsyncPersistsWhenUsageStatisticsDisabled(t *testing.T) {
	ctx := context.Background()
	if err := Init(filepath.Join(t.TempDir(), "usage.sqlite"), false); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}

	PersistCooldownAsync(CooldownState{
		AuthID:         "auth-disabled-stats",
		Provider:       "codex",
		Model:          "gpt-5.4-mini",
		Reason:         "quota",
		NextRetryAfter: time.Now().Add(time.Hour),
		QuotaExceeded:  true,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store := DefaultStore()
		if store == nil {
			t.Fatalf("default store is not initialized")
		}
		active, err := store.ActiveCooldownsByAuth(ctx, "auth-disabled-stats", time.Now())
		if err != nil {
			t.Fatalf("active cooldowns: %v", err)
		}
		if len(active) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cooldown was not persisted while usage statistics were disabled")
}
