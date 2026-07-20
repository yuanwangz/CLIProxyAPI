package auth

import (
	"context"
	"errors"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// SelectedAuthExecutor runs custom provider-specific work with an auth selected by Manager.
// It exists as a small extension point for features that are not implemented as normal
// ProviderExecutor requests but still need the same auth rotation, retry, and model-scoped
// cooldown behavior.
type SelectedAuthExecutor func(ctx context.Context, auth *Auth, provider string) (any, error)

// SkipSelectedAuthError tells ExecuteSelectedAuth to skip the selected auth without
// marking it as failed. It is useful when custom work has narrower eligibility than
// a provider as a whole, such as OAuth-only operations.
type SkipSelectedAuthError struct {
	Reason string
}

func (e *SkipSelectedAuthError) Error() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return "selected auth is not eligible"
	}
	return strings.TrimSpace(e.Reason)
}

// ExecuteSelectedAuth executes custom work with an auth selected through the normal Manager
// scheduler. Failures are recorded against model, so callers should pass a model-specific
// identifier when the error must not cool down the whole credential.
func (m *Manager) ExecuteSelectedAuth(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, run SelectedAuthExecutor) (any, error) {
	if run == nil {
		return nil, &Error{Code: "executor_not_found", Message: "selected auth executor is nil"}
	}

	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()
	var lastErr error
	for attempt := 0; ; attempt++ {
		value, errExec := m.executeSelectedAuthOnce(ctx, normalized, model, opts, maxRetryCredentials, run)
		if errExec == nil {
			return value, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) executeSelectedAuthOnce(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, maxRetryCredentials int, run SelectedAuthExecutor) (any, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	opts = ensureRequestedModelMetadata(opts, model)
	skipResultTracking, _ := opts.Metadata[cliproxyexecutor.SkipSelectedAuthResultMetadataKey].(bool)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	var lastErr error
	for {
		if maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}

		auth, _, provider, errPick := m.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, model)
		publishSelectedAuthMetadata(opts.Metadata, auth)

		tried[auth.ID] = struct{}{}
		attempted[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		value, errExec := run(execCtx, auth, provider)
		if errExec == nil {
			if !skipResultTracking {
				m.MarkResult(execCtx, Result{AuthID: auth.ID, Provider: provider, Model: model, Success: true})
			}
			return value, nil
		}
		var skipErr *SkipSelectedAuthError
		if errors.As(errExec, &skipErr) {
			continue
		}
		if errCtx := execCtx.Err(); errCtx != nil {
			return nil, errCtx
		}

		rerr := &Error{Message: errExec.Error()}
		if status := statusCodeFromError(errExec); status > 0 {
			rerr.HTTPStatus = status
		}
		result := Result{AuthID: auth.ID, Provider: provider, Model: model, Success: false, Error: rerr}
		result.RetryAfter = retryAfterFromError(errExec)
		if !skipResultTracking {
			m.MarkResult(execCtx, result)
		}
		if isRequestInvalidError(errExec) {
			return nil, errExec
		}
		lastErr = errExec
	}
}
