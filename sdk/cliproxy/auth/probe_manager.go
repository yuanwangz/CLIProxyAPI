package auth

import internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"

// CloneForProbe returns a detached manager snapshot suitable for internal
// fallback probes. It shares executors and selector behavior with the source
// manager, but keeps auth state mutations isolated from the live manager.
func (m *Manager) CloneForProbe() *Manager {
	if m == nil {
		return NewManager(nil, nil, nil)
	}

	m.mu.RLock()
	selector := m.selector
	rtProvider := m.rtProvider
	executors := make(map[string]ProviderExecutor, len(m.executors))
	for provider, executor := range m.executors {
		executors[provider] = executor
	}
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		auths = append(auths, auth.Clone())
	}
	providerOffsets := make(map[string]int, len(m.providerOffsets))
	for key, value := range m.providerOffsets {
		providerOffsets[key] = value
	}
	modelPoolOffsets := make(map[string]int, len(m.modelPoolOffsets))
	for key, value := range m.modelPoolOffsets {
		modelPoolOffsets[key] = value
	}
	m.mu.RUnlock()

	clone := NewManager(nil, selector, NoopHook{})
	clone.SetRoundTripperProvider(rtProvider)
	clone.requestRetry.Store(m.requestRetry.Load())
	clone.maxRetryCredentials.Store(m.maxRetryCredentials.Load())
	clone.maxRetryInterval.Store(m.maxRetryInterval.Load())
	clone.providerOffsets = providerOffsets
	clone.modelPoolOffsets = modelPoolOffsets

	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil {
		clone.runtimeConfig.Store(cfg)
	} else {
		clone.runtimeConfig.Store(&internalconfig.Config{})
	}
	if table, ok := m.oauthModelAlias.Load().(*oauthModelAliasTable); ok && table != nil {
		clone.oauthModelAlias.Store(table)
	}

	clone.mu.Lock()
	for provider, executor := range executors {
		clone.executors[provider] = executor
	}
	for _, auth := range auths {
		if auth != nil && auth.ID != "" {
			clone.auths[auth.ID] = auth.Clone()
		}
	}
	clone.mu.Unlock()

	clone.rebuildAPIKeyModelAliasFromRuntimeConfig()
	clone.syncSchedulerFromSnapshot(auths)
	return clone
}
