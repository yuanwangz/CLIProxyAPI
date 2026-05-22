package auth

import (
	"context"
	"net/http"
	"testing"
)

func TestManager_MarkResultUnauthorizedDisablesAuth(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name  string
		model string
	}{
		{name: "auth-level"},
		{name: "model-level", model: "gpt-5"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auth := &Auth{
				ID:       "unauthorized-" + tc.name,
				Provider: "codex",
				Status:   StatusActive,
			}
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			manager.MarkResult(ctx, Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    tc.model,
				Error: &Error{
					Code:       "unauthorized",
					Message:    "unauthorized",
					HTTPStatus: http.StatusUnauthorized,
				},
			})

			updated, ok := manager.GetByID(auth.ID)
			if !ok {
				t.Fatalf("expected auth %q after MarkResult", auth.ID)
			}
			if !updated.Disabled {
				t.Fatal("Disabled = false, want true")
			}
			if updated.Status != StatusDisabled {
				t.Fatalf("Status = %q, want %q", updated.Status, StatusDisabled)
			}
			if updated.StatusMessage != "unauthorized" {
				t.Fatalf("StatusMessage = %q, want unauthorized", updated.StatusMessage)
			}
			if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
				t.Fatalf("LastError = %#v, want HTTP 401", updated.LastError)
			}
			if !updated.NextRetryAfter.IsZero() {
				t.Fatalf("NextRetryAfter = %s, want zero for disabled auth", updated.NextRetryAfter)
			}
			if updated.Quota.Exceeded {
				t.Fatal("Quota.Exceeded = true, want false for unauthorized disable")
			}
			if tc.model != "" {
				state := updated.ModelStates[tc.model]
				if state == nil {
					t.Fatalf("expected model state for %q", tc.model)
				}
				if state.NextRetryAfter.IsZero() == false {
					t.Fatalf("model NextRetryAfter = %s, want zero for disabled auth", state.NextRetryAfter)
				}
				if state.LastError == nil || state.LastError.StatusCode() != http.StatusUnauthorized {
					t.Fatalf("model LastError = %#v, want HTTP 401", state.LastError)
				}
			}
		})
	}
}

func TestManager_MarkResultSuccessDoesNotReenableDisabledAuth(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:            "disabled-after-unauthorized",
		Provider:      "codex",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "unauthorized",
		LastError:     &Error{Code: "unauthorized", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  true,
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after MarkResult", auth.ID)
	}
	if !updated.Disabled {
		t.Fatal("Disabled = false, want true")
	}
	if updated.Status != StatusDisabled {
		t.Fatalf("Status = %q, want %q", updated.Status, StatusDisabled)
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want preserved HTTP 401", updated.LastError)
	}
}
