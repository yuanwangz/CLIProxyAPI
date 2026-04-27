package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type probeCloneTestExecutor struct{}

func (probeCloneTestExecutor) Identifier() string { return "codex" }

func (probeCloneTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (probeCloneTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (probeCloneTestExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }

func (probeCloneTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (probeCloneTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestCloneForProbeIsolatesAuthStateFromOriginalManager(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(probeCloneTestExecutor{})
	auth, err := manager.Register(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	probe := manager.CloneForProbe()
	if probe == manager {
		t.Fatal("expected detached clone, got same manager pointer")
	}
	if _, ok := probe.Executor("codex"); !ok {
		t.Fatal("expected probe manager to retain executor registration")
	}

	probe.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		Success:  false,
		Error: &Error{
			Message:    "probe failure",
			HTTPStatus: http.StatusBadRequest,
		},
	})

	originalAuth, ok := manager.GetByID(auth.ID)
	if !ok || originalAuth == nil {
		t.Fatalf("expected auth %q to exist on original manager", auth.ID)
	}
	if originalAuth.LastError != nil {
		t.Fatalf("original manager last error = %+v, want nil", originalAuth.LastError)
	}
	if len(originalAuth.ModelStates) != 0 {
		t.Fatalf("original manager model states = %+v, want empty", originalAuth.ModelStates)
	}

	probeAuth, ok := probe.GetByID(auth.ID)
	if !ok || probeAuth == nil {
		t.Fatalf("expected auth %q to exist on probe manager", auth.ID)
	}
	if probeAuth.LastError == nil {
		t.Fatal("expected probe manager to record probe failure")
	}
}
