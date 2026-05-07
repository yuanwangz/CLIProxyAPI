package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
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
