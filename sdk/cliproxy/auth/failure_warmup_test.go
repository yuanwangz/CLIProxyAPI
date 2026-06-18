package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type failureWarmupTestExecutor struct {
	id string

	mu          sync.Mutex
	calls       int
	requests    []cliproxyexecutor.Request
	options     []cliproxyexecutor.Options
	suppressed  []bool
	warmupDone  chan struct{}
	closeWarmup sync.Once
}

func (e *failureWarmupTestExecutor) Identifier() string {
	return e.id
}

func (e *failureWarmupTestExecutor) Execute(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.requests = append(e.requests, cloneFailureWarmupRequest(req))
	e.options = append(e.options, cloneFailureWarmupOptions(opts))
	e.suppressed = append(e.suppressed, cliproxyusage.UsageSuppressedFromContext(ctx))
	e.mu.Unlock()

	if call == 1 {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "warming required"}
	}
	if e.warmupDone != nil {
		e.closeWarmup.Do(func() { close(e.warmupDone) })
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *failureWarmupTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "not implemented"}
}

func (e *failureWarmupTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *failureWarmupTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *failureWarmupTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *failureWarmupTestExecutor) snapshot() (int, []cliproxyexecutor.Request, []cliproxyexecutor.Options, []bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	requests := make([]cliproxyexecutor.Request, len(e.requests))
	for i := range e.requests {
		requests[i] = cloneFailureWarmupRequest(e.requests[i])
	}
	options := make([]cliproxyexecutor.Options, len(e.options))
	for i := range e.options {
		options[i] = cloneFailureWarmupOptions(e.options[i])
	}
	suppressed := append([]bool(nil), e.suppressed...)
	return e.calls, requests, options, suppressed
}

func TestFailureWarmup_ExecuteFailureQueuesAsyncRetryAndClearsState(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	m.failureWarmup.interval = time.Millisecond

	executor := &failureWarmupTestExecutor{
		id:         "claude",
		warmupDone: make(chan struct{}),
	}
	m.RegisterExecutor(executor)

	model := "claude-warmup-test"
	auth := &Auth{
		ID:       "warmup-auth",
		Provider: "claude",
		Attributes: map[string]string{
			FailureWarmupEnabledAttribute:     "true",
			FailureWarmupStatusCodesAttribute: "429",
			FailureWarmupMaxAttemptsAttribute: "1",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	interceptorCalls := 0
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Test": []string{"original"}},
		Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(string) {},
		},
		RequestAfterAuthInterceptor: func(_ context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			interceptorCalls++
			if string(req.Body) != "original" {
				t.Fatalf("interceptor body = %q, want original", string(req.Body))
			}
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{
				Headers: http.Header{"X-Test": []string{"rewritten"}},
				Body:    []byte("rewritten"),
			}
		},
	}
	_, errExecute := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte("original"),
	}, opts)
	if errExecute == nil {
		t.Fatalf("expected first execution to fail")
	}

	select {
	case <-executor.warmupDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for warmup retry")
	}

	if interceptorCalls != 1 {
		t.Fatalf("interceptor calls = %d, want 1", interceptorCalls)
	}

	calls, requests, options, suppressed := executor.snapshot()
	if calls != 2 {
		t.Fatalf("execute calls = %d, want 2", calls)
	}
	if len(suppressed) != 2 || suppressed[0] || !suppressed[1] {
		t.Fatalf("usage suppression flags = %v, want [false true]", suppressed)
	}
	if len(requests) != 2 || string(requests[1].Payload) != "rewritten" {
		t.Fatalf("warmup request payload = %q, want rewritten", string(requests[1].Payload))
	}
	if got := options[1].Headers.Get("X-Test"); got != "rewritten" {
		t.Fatalf("warmup header X-Test = %q, want rewritten", got)
	}
	if options[1].RequestAfterAuthInterceptor != nil {
		t.Fatalf("warmup should not keep request interceptor")
	}
	if _, ok := options[1].Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey]; ok {
		t.Fatalf("warmup should not keep selected auth callback metadata")
	}

	var updated *Auth
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := m.GetByID(auth.ID)
		if ok && current != nil && current.Status == StatusActive && current.LastError == nil {
			updated = current
			break
		}
		time.Sleep(time.Millisecond)
	}
	if updated == nil {
		current, _ := m.GetByID(auth.ID)
		t.Fatalf("expected auth state to be cleared, got %#v", current)
	}
	if updated.Failed != 1 || updated.Success != 0 {
		t.Fatalf("auth counters success=%d failed=%d, want success=0 failed=1", updated.Success, updated.Failed)
	}
	if updated.Status != StatusActive {
		t.Fatalf("auth status = %s, want active", updated.Status)
	}
	if updated.LastError != nil {
		t.Fatalf("auth last error = %#v, want nil", updated.LastError)
	}
	if state := updated.ModelStates[model]; state != nil && !modelStateIsClean(state) {
		t.Fatalf("model state was not cleared: %#v", state)
	}
}

func TestFailureWarmupQueue_DeduplicatesByAuth(t *testing.T) {
	q := newFailureWarmupQueue(nil)
	q.interval = time.Millisecond
	block := make(chan struct{})
	executor := &blockingFailureWarmupExecutor{block: block}
	task := failureWarmupTask{
		key:         "auth-a",
		auth:        &Auth{ID: "auth-a", Provider: "claude"},
		executor:    executor,
		provider:    "claude",
		maxAttempts: 1,
	}

	if !q.enqueue(task) {
		t.Fatalf("first enqueue returned false")
	}
	if q.enqueue(task) {
		t.Fatalf("second enqueue returned true while first task was in flight")
	}
	close(block)

	deadline := time.After(2 * time.Second)
	for {
		q.mu.Lock()
		_, exists := q.inFlight[task.key]
		q.mu.Unlock()
		if !exists {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for warmup task to finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

type blockingFailureWarmupExecutor struct {
	block <-chan struct{}
}

func (e *blockingFailureWarmupExecutor) Identifier() string { return "claude" }

func (e *blockingFailureWarmupExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	<-e.block
	return cliproxyexecutor.Response{}, nil
}

func (e *blockingFailureWarmupExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	<-e.block
	return &cliproxyexecutor.StreamResult{}, nil
}

func (e *blockingFailureWarmupExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *blockingFailureWarmupExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *blockingFailureWarmupExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
