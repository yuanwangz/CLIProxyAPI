package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestExecuteSelectedAuth_ModelScopedCooldownDoesNotBlockOtherModels(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&authFallbackExecutor{id: "codex"})

	imageModel := "gpt-image-2"
	textModel := "gpt-5.4-mini"
	badAuth := &Auth{ID: "aa-image-quota", Provider: "codex"}
	goodAuth := &Auth{ID: "bb-image-ready", Provider: "codex"}

	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{badAuth, goodAuth} {
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: imageModel}, {ID: textModel}})
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	t.Cleanup(func() {
		reg.UnregisterClient(badAuth.ID)
		reg.UnregisterClient(goodAuth.ID)
	})

	gotImage, err := manager.ExecuteSelectedAuth(context.Background(), []string{"codex"}, imageModel, cliproxyexecutor.Options{}, func(_ context.Context, auth *Auth, _ string) (any, error) {
		if auth.ID == badAuth.ID {
			return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "image quota"}
		}
		return auth.ID, nil
	})
	if err != nil {
		t.Fatalf("image selected auth execution: %v", err)
	}
	if gotImage != goodAuth.ID {
		t.Fatalf("image selected auth = %v, want %s", gotImage, goodAuth.ID)
	}

	updatedBad, ok := manager.GetByID(badAuth.ID)
	if !ok {
		t.Fatalf("missing bad auth")
	}
	imageState := updatedBad.ModelStates[imageModel]
	if imageState == nil || !imageState.Quota.Exceeded || imageState.NextRetryAfter.IsZero() {
		t.Fatalf("expected image model quota cooldown, got %#v", imageState)
	}
	if textState := updatedBad.ModelStates[textModel]; textState != nil && !textState.NextRetryAfter.IsZero() {
		t.Fatalf("text model state was cooled down: %#v", textState)
	}

	gotText, err := manager.ExecuteSelectedAuth(context.Background(), []string{"codex"}, textModel, cliproxyexecutor.Options{}, func(_ context.Context, auth *Auth, _ string) (any, error) {
		return auth.ID, nil
	})
	if err != nil {
		t.Fatalf("text selected auth execution: %v", err)
	}
	if gotText != badAuth.ID {
		t.Fatalf("text selected auth = %v, want %s", gotText, badAuth.ID)
	}
}

func TestExecuteSelectedAuth_SkipResultTrackingRotatesWithoutChangingAuthHealth(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&authFallbackExecutor{id: "codex"})

	model := "gpt-image-2"
	firstAuth := &Auth{ID: "aa-web-image-unauthorized", Provider: "codex"}
	secondAuth := &Auth{ID: "bb-web-image-ready", Provider: "codex"}
	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{firstAuth, secondAuth} {
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	t.Cleanup(func() {
		reg.UnregisterClient(firstAuth.ID)
		reg.UnregisterClient(secondAuth.ID)
	})

	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SkipSelectedAuthResultMetadataKey: true,
	}}
	value, errExecute := manager.ExecuteSelectedAuth(context.Background(), []string{"codex"}, model, opts, func(_ context.Context, auth *Auth, _ string) (any, error) {
		if auth.ID == firstAuth.ID {
			return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "web image unauthorized"}
		}
		return auth.ID, nil
	})
	if errExecute != nil {
		t.Fatalf("ExecuteSelectedAuth() error = %v", errExecute)
	}
	if value != secondAuth.ID {
		t.Fatalf("ExecuteSelectedAuth() value = %v, want %s", value, secondAuth.ID)
	}

	for _, authID := range []string{firstAuth.ID, secondAuth.ID} {
		updated, ok := manager.GetByID(authID)
		if !ok || updated == nil {
			t.Fatalf("missing auth %s", authID)
		}
		if updated.Disabled || updated.Status == StatusDisabled || updated.LastError != nil {
			t.Fatalf("auth %s health changed: %#v", authID, updated)
		}
		if updated.Success != 0 || updated.Failed != 0 {
			t.Fatalf("auth %s counters = success:%d failed:%d, want zero", authID, updated.Success, updated.Failed)
		}
	}
}
