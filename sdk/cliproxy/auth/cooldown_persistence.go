package auth

import (
	"context"
	"net/http"
	"path/filepath"
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
	migrations := make([]usagepersist.CooldownState, 0)
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil {
		authIndex := auth.EnsureIndex()
		changed := false
		for _, cooldown := range cooldowns {
			matched, migrateIndex := persistedCooldownMatchesAuth(auth, authIndex, cooldown)
			if !matched {
				continue
			}
			model := strings.TrimSpace(cooldown.Model)
			if model != "" && !restoredCooldownModelSupported(supported, model) {
				continue
			}
			if applyPersistedCooldown(auth, cooldown, now) {
				changed = true
				if migrateIndex && strings.TrimSpace(authIndex) != "" {
					cooldown.AuthIndex = authIndex
					if strings.TrimSpace(cooldown.Provider) == "" {
						cooldown.Provider = auth.Provider
					}
					if cooldown.UpdatedAt.IsZero() {
						cooldown.UpdatedAt = now
					}
					migrations = append(migrations, cooldown)
				}
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

	for _, cooldown := range migrations {
		usagepersist.PersistCooldownAsync(cooldown)
	}

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

func persistedCooldownMatchesAuth(auth *Auth, authIndex string, cooldown usagepersist.CooldownState) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if strings.TrimSpace(cooldown.AuthID) != strings.TrimSpace(auth.ID) {
		return false, false
	}
	if cooldown.Provider != "" && !strings.EqualFold(strings.TrimSpace(cooldown.Provider), strings.TrimSpace(auth.Provider)) {
		return false, false
	}
	cooldownIndex := strings.TrimSpace(cooldown.AuthIndex)
	authIndex = strings.TrimSpace(authIndex)
	if cooldownIndex == "" || authIndex == "" || cooldownIndex == authIndex {
		return true, false
	}
	for _, legacyIndex := range legacyAuthIndexes(auth) {
		if cooldownIndex == legacyIndex {
			return true, true
		}
	}
	return false, false
}

func legacyAuthIndexes(auth *Auth) []string {
	seeds := legacyAuthIndexSeeds(auth)
	if len(seeds) == 0 {
		return nil
	}
	indexes := make([]string, 0, len(seeds))
	seen := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			continue
		}
		index := stableAuthIndex(seed)
		if index == "" {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	return indexes
}

func legacyAuthIndexSeeds(auth *Auth) []string {
	if auth == nil {
		return nil
	}

	seeds := make([]string, 0, 8)
	appendSeed := func(seed string) {
		if seed = strings.TrimSpace(seed); seed != "" {
			seeds = append(seeds, seed)
		}
	}
	appendLegacyFileSeed := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		appendSeed("file:" + value)
		if base := filepath.Base(value); base != "" && base != "." && base != string(filepath.Separator) && base != value {
			appendSeed("file:" + base)
		}
	}
	appendLegacyFileSeed(auth.FileName)
	appendLegacyFileSeed(auth.ID)

	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	compatName := ""
	baseURL := ""
	apiKey := ""
	source := ""
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["provider_key"]); value != "" {
			providerKey = strings.ToLower(value)
		}
		compatName = strings.ToLower(strings.TrimSpace(auth.Attributes["compat_name"]))
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
		source = strings.TrimSpace(auth.Attributes["source"])
		appendLegacyFileSeed(auth.Attributes["path"])
		appendLegacyFileSeed(source)
	}

	proxyURL := strings.TrimSpace(auth.ProxyURL)
	hasCredentialIdentity := compatName != "" || baseURL != "" || proxyURL != "" || apiKey != "" || source != ""
	if providerKey != "" && hasCredentialIdentity {
		parts := []string{"provider=" + providerKey}
		if compatName != "" {
			parts = append(parts, "compat="+compatName)
		}
		if baseURL != "" {
			parts = append(parts, "base="+baseURL)
		}
		if proxyURL != "" {
			parts = append(parts, "proxy="+proxyURL)
		}
		if apiKey != "" {
			parts = append(parts, "api_key="+apiKey)
		}
		if source != "" {
			parts = append(parts, "source="+source)
		}
		appendSeed("config:" + strings.Join(parts, "\x00"))
	}

	if id := strings.TrimSpace(auth.ID); id != "" {
		appendSeed("id:" + id)
	}
	return seeds
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
