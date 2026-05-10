package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
)

// RestorePersistedCooldowns overlays unexpired auth/model cooldowns from the
// local persistence store after model registration has rebuilt the registry
// snapshot. It does not write back to auth files and only affects runtime state.
func (m *Manager) RestorePersistedCooldowns(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}
	now := time.Now()
	cooldowns, err := usagepersist.ActiveCooldownsByAuth(ctx, authID, now)
	if err != nil {
		logEntryWithRequestID(ctx).WithField("auth_id", authID).Warnf("failed to load persisted auth cooldowns: %v", err)
		return
	}
	if len(cooldowns) == 0 {
		return
	}

	supported := supportedModelKeysForAuth(authID)
	var snapshot *Auth
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil {
		authIndex := auth.EnsureIndex()
		changed := false
		for _, cooldown := range cooldowns {
			if !persistedCooldownMatchesAuth(auth, authIndex, cooldown) {
				continue
			}
			model := strings.TrimSpace(cooldown.Model)
			if model != "" && !restoredCooldownModelSupported(supported, model) {
				continue
			}
			if applyPersistedCooldown(auth, cooldown, now) {
				changed = true
			}
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			auth.Status = StatusError
			auth.UpdatedAt = now
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

type cooldownPersistenceUpdate struct {
	upsert    *usagepersist.CooldownState
	clearID   string
	clearMode string
}

func (u cooldownPersistenceUpdate) apply() {
	if u.upsert != nil {
		usagepersist.PersistCooldownAsync(*u.upsert)
		return
	}
	if strings.TrimSpace(u.clearID) != "" {
		usagepersist.ClearCooldownAsync(u.clearID, u.clearMode)
	}
}

func buildCooldownPersistenceUpdate(auth *Auth, result Result, now time.Time) cooldownPersistenceUpdate {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return cooldownPersistenceUpdate{}
	}
	model := strings.TrimSpace(result.Model)
	if result.Success {
		return cooldownPersistenceUpdate{clearID: auth.ID, clearMode: model}
	}

	if model == "" {
		if auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now) {
			return cooldownPersistenceUpdate{clearID: auth.ID}
		}
		return cooldownPersistenceUpdate{upsert: buildAuthLevelCooldownState(auth, result, now)}
	}

	state := modelStateForResult(auth, model)
	if state == nil || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return cooldownPersistenceUpdate{clearID: auth.ID, clearMode: model}
	}
	return cooldownPersistenceUpdate{upsert: buildModelCooldownState(auth, result, state, model, now)}
}

func modelStateForResult(auth *Auth, model string) *ModelState {
	if auth == nil || strings.TrimSpace(model) == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	if state := auth.ModelStates[model]; state != nil {
		return state
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" || modelKey == model {
		return nil
	}
	return auth.ModelStates[modelKey]
}

func buildModelCooldownState(auth *Auth, result Result, state *ModelState, model string, now time.Time) *usagepersist.CooldownState {
	if auth == nil || state == nil {
		return nil
	}
	reason := cooldownReasonFromResult(result, state)
	message := cooldownStatusMessage(result, state, reason)
	return &usagepersist.CooldownState{
		AuthID:         auth.ID,
		AuthIndex:      auth.EnsureIndex(),
		Provider:       firstNonEmptyCooldownReason(result.Provider, auth.Provider),
		Model:          strings.TrimSpace(model),
		Reason:         reason,
		StatusMessage:  message,
		HTTPStatus:     statusCodeFromResult(result.Error),
		NextRetryAfter: state.NextRetryAfter,
		QuotaExceeded:  state.Quota.Exceeded,
		BackoffLevel:   state.Quota.BackoffLevel,
		UpdatedAt:      firstNonZeroTime(state.UpdatedAt, now),
	}
}

func buildAuthLevelCooldownState(auth *Auth, result Result, now time.Time) *usagepersist.CooldownState {
	if auth == nil {
		return nil
	}
	reason := cooldownReasonFromAuthResult(result, auth)
	message := cooldownStatusMessage(result, nil, reason)
	return &usagepersist.CooldownState{
		AuthID:         auth.ID,
		AuthIndex:      auth.EnsureIndex(),
		Provider:       firstNonEmptyCooldownReason(result.Provider, auth.Provider),
		Model:          "",
		Reason:         reason,
		StatusMessage:  message,
		HTTPStatus:     statusCodeFromResult(result.Error),
		NextRetryAfter: auth.NextRetryAfter,
		QuotaExceeded:  auth.Quota.Exceeded,
		BackoffLevel:   auth.Quota.BackoffLevel,
		UpdatedAt:      firstNonZeroTime(auth.UpdatedAt, now),
	}
}

func supportedModelKeysForAuth(authID string) map[string]struct{} {
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey != "" {
			out[modelKey] = struct{}{}
		}
	}
	return out
}

func persistedCooldownMatchesAuth(auth *Auth, authIndex string, cooldown usagepersist.CooldownState) bool {
	if auth == nil {
		return false
	}
	if strings.TrimSpace(cooldown.AuthID) != strings.TrimSpace(auth.ID) {
		return false
	}
	if cooldown.Provider != "" && !strings.EqualFold(strings.TrimSpace(cooldown.Provider), strings.TrimSpace(auth.Provider)) {
		return false
	}
	cooldownIndex := strings.TrimSpace(cooldown.AuthIndex)
	return cooldownIndex == "" || authIndex == "" || cooldownIndex == authIndex
}

func restoredCooldownModelSupported(supported map[string]struct{}, model string) bool {
	if len(supported) == 0 {
		return false
	}
	modelKey := canonicalModelKey(model)
	if modelKey == "" {
		modelKey = strings.TrimSpace(model)
	}
	_, ok := supported[modelKey]
	return ok
}

func applyPersistedCooldown(auth *Auth, cooldown usagepersist.CooldownState, now time.Time) bool {
	if auth == nil || cooldown.NextRetryAfter.IsZero() || !cooldown.NextRetryAfter.After(now) {
		return false
	}
	message := persistedCooldownMessage(cooldown)
	updatedAt := cooldown.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	lastError := &Error{
		Code:       strings.TrimSpace(cooldown.Reason),
		Message:    message,
		Retryable:  true,
		HTTPStatus: cooldown.HTTPStatus,
	}
	if lastError.Code == "" {
		lastError.Code = "persisted_cooldown"
	}

	model := strings.TrimSpace(cooldown.Model)
	if model == "" {
		auth.Unavailable = true
		auth.Status = StatusError
		auth.StatusMessage = message
		auth.NextRetryAfter = cooldown.NextRetryAfter
		auth.LastError = lastError
		auth.UpdatedAt = updatedAt
		if cooldown.QuotaExceeded {
			auth.Quota = QuotaState{
				Exceeded:      true,
				Reason:        firstNonEmptyCooldownReason(cooldown.Reason, "quota"),
				NextRecoverAt: cooldown.NextRetryAfter,
				BackoffLevel:  cooldown.BackoffLevel,
			}
		}
		return true
	}

	state := ensureModelState(auth, model)
	if state == nil {
		return false
	}
	state.Unavailable = true
	state.Status = StatusError
	state.StatusMessage = message
	state.NextRetryAfter = cooldown.NextRetryAfter
	state.LastError = lastError
	state.UpdatedAt = updatedAt
	state.Quota = QuotaState{}
	if cooldown.QuotaExceeded {
		state.Quota = QuotaState{
			Exceeded:      true,
			Reason:        firstNonEmptyCooldownReason(cooldown.Reason, "quota"),
			NextRecoverAt: cooldown.NextRetryAfter,
			BackoffLevel:  cooldown.BackoffLevel,
		}
	}
	auth.LastError = lastError
	auth.StatusMessage = message
	return true
}

func cooldownReasonFromResult(result Result, state *ModelState) string {
	if state != nil && state.Quota.Exceeded {
		return firstNonEmptyCooldownReason(state.Quota.Reason, "quota")
	}
	if isModelSupportResultError(result.Error) {
		return "model_not_supported"
	}
	return cooldownReasonFromStatus(statusCodeFromResult(result.Error))
}

func cooldownReasonFromAuthResult(result Result, auth *Auth) string {
	if auth != nil && auth.Quota.Exceeded {
		return firstNonEmptyCooldownReason(auth.Quota.Reason, "quota")
	}
	return cooldownReasonFromStatus(statusCodeFromResult(result.Error))
}

func cooldownReasonFromStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "payment_required"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "quota"
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "upstream_error"
	default:
		return "cooldown"
	}
}

func cooldownStatusMessage(result Result, state *ModelState, reason string) string {
	if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
		return strings.TrimSpace(result.Error.Message)
	}
	if state != nil && strings.TrimSpace(state.StatusMessage) != "" {
		return strings.TrimSpace(state.StatusMessage)
	}
	return strings.TrimSpace(reason)
}

func persistedCooldownMessage(cooldown usagepersist.CooldownState) string {
	if message := strings.TrimSpace(cooldown.StatusMessage); message != "" {
		return message
	}
	if reason := strings.TrimSpace(cooldown.Reason); reason != "" {
		return reason
	}
	if cooldown.HTTPStatus > 0 {
		return http.StatusText(cooldown.HTTPStatus)
	}
	return "persisted cooldown"
}

func firstNonEmptyCooldownReason(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}
