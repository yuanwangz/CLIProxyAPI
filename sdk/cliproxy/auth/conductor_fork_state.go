package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// ClearQuotaCooldownState clears quota/429 runtime cooldowns after a quota refresh proves capacity.
func (m *Manager) ClearQuotaCooldownState(ctx context.Context, authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	authID = strings.TrimSpace(authID)
	now := time.Now()
	var snapshot *Auth
	var clearedModels []string
	m.mu.Lock()
	auth := m.auths[authID]
	if auth != nil && !auth.Archived && auth.Status != StatusArchived && !auth.Disabled && auth.Status != StatusDisabled {
		changed := false
		if authHasQuotaCooldown(auth) {
			auth.Quota = QuotaState{}
			auth.NextRetryAfter = time.Time{}
			auth.Unavailable = false
			if errorIsQuotaCooldown(auth.LastError) {
				auth.LastError = nil
			}
			if statusMessageIsQuotaCooldown(auth.StatusMessage) {
				auth.StatusMessage = ""
			}
			changed = true
		}
		for model, state := range auth.ModelStates {
			if !modelStateHasQuotaCooldown(state) {
				continue
			}
			resetModelState(state, now)
			clearedModels = append(clearedModels, strings.TrimSpace(model))
			if modelStateIsClean(state) {
				delete(auth.ModelStates, model)
			}
			changed = true
		}
		if changed {
			if len(auth.ModelStates) == 0 {
				auth.ModelStates = nil
				clearAggregatedAvailability(auth)
			} else {
				updateAggregatedAvailability(auth, now)
			}
			if !hasModelError(auth, now) && auth.LastError == nil {
				auth.Status = StatusActive
				auth.StatusMessage = ""
			}
			auth.UpdatedAt = now
			_ = m.persist(ctx, auth)
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()
	if snapshot == nil {
		return
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	for _, model := range clearedModels {
		if model != "" {
			registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, model)
		}
	}
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
}

// ClearRecoverableAvailabilityState clears operator-retryable cooldowns and model pauses.
func (m *Manager) ClearRecoverableAvailabilityState(ctx context.Context, authID string) []string {
	if m == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	authID = strings.TrimSpace(authID)
	now := time.Now()
	var snapshot *Auth
	var clearedModels []string
	m.mu.Lock()
	auth := m.auths[authID]
	if auth != nil && !auth.Archived && auth.Status != StatusArchived && !auth.Disabled && auth.Status != StatusDisabled {
		changed := false
		if authHasRecoverableAvailabilityBlock(auth) {
			auth.Quota = QuotaState{}
			auth.NextRetryAfter = time.Time{}
			auth.Unavailable = false
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
			clearedModels = append(clearedModels, "")
			changed = true
		}
		for model, state := range auth.ModelStates {
			if !modelStateHasRecoverableAvailabilityBlock(state) {
				continue
			}
			resetModelState(state, now)
			clearedModels = append(clearedModels, strings.TrimSpace(model))
			if modelStateIsClean(state) {
				delete(auth.ModelStates, model)
			}
			changed = true
		}
		if changed {
			if len(auth.ModelStates) == 0 {
				auth.ModelStates = nil
				clearAggregatedAvailability(auth)
			} else {
				updateAggregatedAvailability(auth, now)
			}
			auth.UpdatedAt = now
			_ = m.persist(ctx, auth)
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()
	if snapshot == nil {
		return nil
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	for _, model := range clearedModels {
		if model != "" {
			registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, model)
			registry.GetGlobalRegistry().ResumeClientModel(authID, model)
		}
	}
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return clearedModels
}

func authHasQuotaCooldown(auth *Auth) bool {
	return auth != nil && (quotaStateHasCooldown(auth.Quota) || errorIsQuotaCooldown(auth.LastError) || statusMessageIsQuotaCooldown(auth.StatusMessage))
}

func modelStateHasQuotaCooldown(state *ModelState) bool {
	return state != nil && (quotaStateHasCooldown(state.Quota) || errorIsQuotaCooldown(state.LastError) || statusMessageIsQuotaCooldown(state.StatusMessage))
}

func authHasRecoverableAvailabilityBlock(auth *Auth) bool {
	if auth == nil || auth.Archived || auth.Status == StatusArchived || auth.Disabled || auth.Status == StatusDisabled || errorIsUnauthorized(auth.LastError) {
		return false
	}
	return authHasQuotaCooldown(auth) || auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Status == StatusError && (auth.StatusMessage != "" || auth.LastError != nil)
}

func modelStateHasRecoverableAvailabilityBlock(state *ModelState) bool {
	if state == nil || state.Status == StatusDisabled || errorIsUnauthorized(state.LastError) {
		return false
	}
	return modelStateHasQuotaCooldown(state) || state.Unavailable || !state.NextRetryAfter.IsZero() || state.Status == StatusError && (state.StatusMessage != "" || state.LastError != nil)
}

func quotaStateHasCooldown(quota QuotaState) bool {
	return quota.Exceeded || strings.EqualFold(strings.TrimSpace(quota.Reason), "quota") || !quota.NextRecoverAt.IsZero() || quota.BackoffLevel > 0
}

func errorIsQuotaCooldown(err *Error) bool {
	return err != nil && (err.StatusCode() == http.StatusTooManyRequests || quotaTextIndicatesCooldown(err.Code) || quotaTextIndicatesCooldown(err.Message))
}

func errorIsUnauthorized(err *Error) bool {
	return err != nil && err.StatusCode() == http.StatusUnauthorized
}

func isDurableUnauthorizedResultError(err *Error) bool {
	if err == nil || err.StatusCode() != http.StatusUnauthorized {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(err.Code), "unauthorized") || strings.EqualFold(strings.TrimSpace(err.Message), "unauthorized")
}

func statusMessageIsQuotaCooldown(message string) bool { return quotaTextIndicatesCooldown(message) }

func quotaTextIndicatesCooldown(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return normalized != "" && (strings.Contains(normalized, "quota") || strings.Contains(normalized, "rate limit") || strings.Contains(normalized, "rate_limit") || strings.Contains(normalized, "limit reached") || strings.Contains(normalized, "limit_reached"))
}

func applyUnauthorizedDisabledState(auth *Auth, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = false
	auth.Status = StatusDisabled
	auth.StatusMessage = "unauthorized"
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	auth.LastError = cloneError(resultErr)
	if auth.LastError == nil {
		auth.LastError = &Error{}
	}
	auth.LastError.HTTPStatus = http.StatusUnauthorized
	if strings.TrimSpace(auth.LastError.Code) == "" {
		auth.LastError.Code = "unauthorized"
	}
	if strings.TrimSpace(auth.LastError.Message) == "" {
		auth.LastError.Message = "unauthorized"
	}
	auth.LastError.Retryable = false
}

func shouldEnableAfterUnauthorizedRefresh(auth *Auth) bool {
	return auth != nil && !auth.Archived && auth.Status != StatusArchived && (auth.Disabled || auth.Status == StatusDisabled) && (hasUnauthorizedAuthFailure(auth) || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "unauthorized"))
}

func modelStateHasUnauthorizedFailure(state *ModelState) bool {
	return state != nil && (errorIsUnauthorized(state.LastError) || strings.EqualFold(strings.TrimSpace(state.StatusMessage), "unauthorized"))
}

func clearUnauthorizedDisabledStateAfterRefresh(auth *Auth, now time.Time) []string {
	if auth == nil {
		return nil
	}
	auth.Disabled = false
	resumed := clearUnauthorizedModelStates(auth, now)
	clearAuthStateOnSuccess(auth, now)
	updateAggregatedAvailability(auth, now)
	if !auth.Unavailable {
		auth.Status = StatusActive
		auth.StatusMessage = ""
	}
	return resumed
}
