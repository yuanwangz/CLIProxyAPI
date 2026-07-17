package auth

import (
	"context"
	"net/http"
	"testing"
)

func markForbidden(t *testing.T, manager *Manager, authID, provider, model string) {
	t.Helper()
	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Error: &Error{
			Code:       "forbidden",
			Message:    "forbidden",
			HTTPStatus: http.StatusForbidden,
		},
	})
}

func TestManager_MarkResult_XAIConsecutive403DisablesOnThird(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:       "xai-403",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai"},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := "grok-4"
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("missing auth after first 403")
	}
	if updated.Disabled {
		t.Fatal("disabled after first 403, want active")
	}
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 1 {
		t.Fatalf("count after first 403 = %d, want 1", got)
	}

	markForbidden(t, manager, auth.ID, auth.Provider, model)
	updated, _ = manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("disabled after second 403, want active")
	}
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 2 {
		t.Fatalf("count after second 403 = %d, want 2", got)
	}

	markForbidden(t, manager, auth.ID, auth.Provider, model)
	updated, _ = manager.GetByID(auth.ID)
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("want disabled after third 403, got disabled=%v status=%q", updated.Disabled, updated.Status)
	}
	if updated.StatusMessage != "forbidden" {
		t.Fatalf("StatusMessage = %q, want forbidden", updated.StatusMessage)
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusForbidden {
		t.Fatalf("LastError = %#v, want HTTP 403", updated.LastError)
	}
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 0 {
		t.Fatalf("count after disable = %d, want 0 (cleared after disable)", got)
	}
}

func TestManager_MarkResult_XAIConsecutive403ClearsOnSuccess(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "xai-403-success", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := "grok-4"
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true})

	updated, _ := manager.GetByID(auth.ID)
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 0 {
		t.Fatalf("count after success = %d, want 0", got)
	}

	markForbidden(t, manager, auth.ID, auth.Provider, model)
	updated, _ = manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("disabled after one 403 following success reset")
	}
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 1 {
		t.Fatalf("count after re-failure = %d, want 1", got)
	}
}

func TestManager_MarkResult_XAIConsecutive403ClearsOnNon403(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "xai-403-429", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := "grok-4"
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	updated, _ := manager.GetByID(auth.ID)
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 0 {
		t.Fatalf("count after 429 = %d, want 0", got)
	}

	markForbidden(t, manager, auth.ID, auth.Provider, model)
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	updated, _ = manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("disabled after only two 403s after reset")
	}
}

func TestManager_MarkResult_NonXAI403DoesNotDisable(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "claude-403", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"type": "claude"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := "claude-sonnet"
	for i := 0; i < 5; i++ {
		markForbidden(t, manager, auth.ID, auth.Provider, model)
	}

	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("non-xai auth disabled by consecutive 403 policy, want active")
	}
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 0 {
		t.Fatalf("non-xai count = %d, want 0", got)
	}
}

func TestManager_MarkResult_Cloudflare403DoesNotCount(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "xai-cf", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := "grok-4"
	markForbidden(t, manager, auth.ID, auth.Provider, model)
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    "cf-mitigated: challenge",
		},
	})

	updated, _ := manager.GetByID(auth.ID)
	if got := consecutiveStatusFailureCount(updated, http.StatusForbidden); got != 0 {
		t.Fatalf("count after CF challenge = %d, want 0", got)
	}
	if updated.Disabled {
		t.Fatal("disabled after CF challenge")
	}
}

func TestManager_MarkResult_XAIConsecutive403AuthLevelPath(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{ID: "xai-auth-level", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := 0; i < 3; i++ {
		manager.MarkResult(ctx, Result{
			AuthID:   auth.ID,
			Provider: auth.Provider,
			// empty model → auth-level failure path
			Error: &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
		})
	}

	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("want disabled after three auth-level 403s")
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusForbidden {
		t.Fatalf("LastError = %#v, want 403", updated.LastError)
	}
}

func TestConsecutiveStatusFailuresMetadataRoundTrip(t *testing.T) {
	auth := &Auth{
		ID:       "meta-roundtrip",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai", "access_token": "tok"},
	}
	auth.consecutiveStatusFailures = map[int]int{http.StatusForbidden: 2}

	SyncAuthStateToMetadata(auth)
	raw, ok := auth.Metadata[metadataConsecutiveStatusFailuresKey].(map[string]any)
	if !ok {
		t.Fatalf("metadata key missing or wrong type: %#v", auth.Metadata[metadataConsecutiveStatusFailuresKey])
	}
	if firstPositiveInt(raw["403"]) != 2 {
		t.Fatalf("metadata 403 count = %#v, want 2", raw["403"])
	}

	restored := &Auth{
		ID:       auth.ID,
		Provider: auth.Provider,
		Status:   StatusActive,
		Metadata: auth.Metadata,
	}
	RestoreAuthStateFromMetadata(restored)
	if got := consecutiveStatusFailureCount(restored, http.StatusForbidden); got != 2 {
		t.Fatalf("restored count = %d, want 2", got)
	}

	_ = ClearConsecutiveStatusFailures(restored)
	SyncAuthStateToMetadata(restored)
	if _, exists := restored.Metadata[metadataConsecutiveStatusFailuresKey]; exists {
		t.Fatalf("expected metadata key removed after clear, got %#v", restored.Metadata[metadataConsecutiveStatusFailuresKey])
	}
}

func TestSyncAuthStateToMetadata_PersistsForbiddenLastError(t *testing.T) {
	auth := &Auth{
		ID:            "forbidden-meta",
		Provider:      "xai",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "forbidden",
		LastError:     &Error{Code: "forbidden", Message: "forbidden", HTTPStatus: http.StatusForbidden},
		Metadata:      map[string]any{"type": "xai"},
	}
	SyncAuthStateToMetadata(auth)
	lastError := authErrorFromMetadata(auth.Metadata[metadataLastErrorKey])
	if lastError == nil || lastError.HTTPStatus != http.StatusForbidden {
		t.Fatalf("persisted last_error = %#v, want 403", lastError)
	}

	restored := &Auth{Metadata: auth.Metadata}
	RestoreAuthStateFromMetadata(restored)
	if !restored.Disabled || restored.LastError == nil || restored.LastError.HTTPStatus != http.StatusForbidden {
		t.Fatalf("restored = %#v, want disabled with 403 last_error", restored)
	}
}
