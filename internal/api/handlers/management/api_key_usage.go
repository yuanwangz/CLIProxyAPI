package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type apiKeyUsageEntry struct {
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests"`
	AuthID         string                         `json:"auth_id,omitempty"`
	AuthIndex      string                         `json:"auth_index,omitempty"`
	Status         coreauth.Status                `json:"status,omitempty"`
	StatusMessage  string                         `json:"status_message,omitempty"`
	StatusCode     int                            `json:"status_code,omitempty"`
	Disabled       bool                           `json:"disabled,omitempty"`
	Archived       bool                           `json:"archived,omitempty"`
	Unavailable    bool                           `json:"unavailable,omitempty"`
	Blocked        bool                           `json:"blocked,omitempty"`
	Cooling        bool                           `json:"cooling,omitempty"`
	BlockReason    string                         `json:"block_reason,omitempty"`
	NextRetryAfter string                         `json:"next_retry_after,omitempty"`
	NextRetryMS    int64                          `json:"next_retry_after_ms,omitempty"`
	TotalAuths     int                            `json:"total_auths,omitempty"`
	DisabledCount  int                            `json:"disabled_count,omitempty"`
	ArchivedCount  int                            `json:"archived_count,omitempty"`
	BlockedCount   int                            `json:"blocked_count,omitempty"`
	CoolingCount   int                            `json:"cooling_count,omitempty"`
	ModelStates    []apiKeyUsageModelState        `json:"model_states,omitempty"`
	Auths          []apiKeyUsageAuthStatus        `json:"auths,omitempty"`
}

type apiKeyUsageAuth struct {
	auth         *coreauth.Auth
	provider     string
	compositeKey string
	authIndex    string
}

type apiKeyUsageQuotaState struct {
	Exceeded      bool   `json:"exceeded,omitempty"`
	Reason        string `json:"reason,omitempty"`
	NextRecoverAt string `json:"next_recover_at,omitempty"`
	NextRecoverMS int64  `json:"next_recover_at_ms,omitempty"`
	BackoffLevel  int    `json:"backoff_level,omitempty"`
}

type apiKeyUsageModelState struct {
	Model          string                 `json:"model"`
	Status         coreauth.Status        `json:"status,omitempty"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	StatusCode     int                    `json:"status_code,omitempty"`
	Unavailable    bool                   `json:"unavailable,omitempty"`
	Blocked        bool                   `json:"blocked,omitempty"`
	Cooling        bool                   `json:"cooling,omitempty"`
	BlockReason    string                 `json:"block_reason,omitempty"`
	NextRetryAfter string                 `json:"next_retry_after,omitempty"`
	NextRetryMS    int64                  `json:"next_retry_after_ms,omitempty"`
	Quota          *apiKeyUsageQuotaState `json:"quota,omitempty"`
}

type apiKeyUsageAuthStatus struct {
	AuthID         string                  `json:"auth_id,omitempty"`
	AuthIndex      string                  `json:"auth_index,omitempty"`
	Provider       string                  `json:"provider,omitempty"`
	Status         coreauth.Status         `json:"status,omitempty"`
	StatusMessage  string                  `json:"status_message,omitempty"`
	StatusCode     int                     `json:"status_code,omitempty"`
	Disabled       bool                    `json:"disabled,omitempty"`
	Archived       bool                    `json:"archived,omitempty"`
	Unavailable    bool                    `json:"unavailable,omitempty"`
	Blocked        bool                    `json:"blocked,omitempty"`
	Cooling        bool                    `json:"cooling,omitempty"`
	BlockReason    string                  `json:"block_reason,omitempty"`
	NextRetryAfter string                  `json:"next_retry_after,omitempty"`
	NextRetryMS    int64                   `json:"next_retry_after_ms,omitempty"`
	Quota          *apiKeyUsageQuotaState  `json:"quota,omitempty"`
	ModelStates    []apiKeyUsageModelState `json:"model_states,omitempty"`
}

type clearAPIKeyUsageCooldownRequest struct {
	Provider       string `json:"provider"`
	AuthID         string `json:"auth_id"`
	AuthIDCamel    string `json:"authId"`
	AuthIndex      string `json:"auth_index"`
	AuthIndexCamel string `json:"authIndex"`
	APIKey         string `json:"api_key"`
	APIKeyCamel    string `json:"apiKey"`
	BaseURL        string `json:"base_url"`
	BaseURLCamel   string `json:"baseUrl"`
}

func mergeRecentRequestBuckets(dst, src []coreauth.RecentRequestBucket) []coreauth.RecentRequestBucket {
	if len(dst) == 0 {
		return src
	}
	if len(src) == 0 {
		return dst
	}
	if len(dst) != len(src) {
		n := len(dst)
		if len(src) < n {
			n = len(src)
		}
		for i := 0; i < n; i++ {
			dst[i].Success += src[i].Success
			dst[i].Failed += src[i].Failed
		}
		return dst
	}
	for i := range dst {
		dst[i].Success += src[i].Success
		dst[i].Failed += src[i].Failed
	}
	return dst
}

func coreRecentRequestBuckets(buckets []usagepersist.RecentRequestBucket) []coreauth.RecentRequestBucket {
	if len(buckets) == 0 {
		return nil
	}
	out := make([]coreauth.RecentRequestBucket, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, coreauth.RecentRequestBucket{
			Time:    bucket.Time,
			Success: bucket.Success,
			Failed:  bucket.Failed,
		})
	}
	return out
}

func formatAPIKeyUsageTime(value time.Time) (string, int64) {
	if value.IsZero() {
		return "", 0
	}
	return value.UTC().Format(time.RFC3339), value.UnixMilli()
}

func buildAPIKeyUsageQuotaState(quota coreauth.QuotaState) *apiKeyUsageQuotaState {
	recoverAt, recoverMS := formatAPIKeyUsageTime(quota.NextRecoverAt)
	if !quota.Exceeded && strings.TrimSpace(quota.Reason) == "" && recoverAt == "" && quota.BackoffLevel == 0 {
		return nil
	}
	return &apiKeyUsageQuotaState{
		Exceeded:      quota.Exceeded,
		Reason:        strings.TrimSpace(quota.Reason),
		NextRecoverAt: recoverAt,
		NextRecoverMS: recoverMS,
		BackoffLevel:  quota.BackoffLevel,
	}
}

func apiKeyUsageStatusCode(err *coreauth.Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func apiKeyUsageBlockReason(quota coreauth.QuotaState, err *coreauth.Error, message string) string {
	if reason := strings.TrimSpace(quota.Reason); reason != "" {
		return reason
	}
	if err != nil {
		if code := strings.TrimSpace(err.Code); code != "" {
			return code
		}
		switch err.StatusCode() {
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
		}
		if msg := strings.TrimSpace(err.Message); msg != "" {
			return msg
		}
	}
	if msg := strings.TrimSpace(message); msg != "" {
		return msg
	}
	return ""
}

func apiKeyUsageTextIndicatesQuota(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "quota") ||
		strings.Contains(normalized, "rate limit") ||
		strings.Contains(normalized, "rate_limit") ||
		strings.Contains(normalized, "limit reached") ||
		strings.Contains(normalized, "limit_reached")
}

func apiKeyUsageIsQuotaCooldown(quota coreauth.QuotaState, err *coreauth.Error, message string) bool {
	if quota.Exceeded ||
		strings.EqualFold(strings.TrimSpace(quota.Reason), "quota") ||
		!quota.NextRecoverAt.IsZero() ||
		quota.BackoffLevel > 0 {
		return true
	}
	if err != nil {
		if err.StatusCode() == http.StatusTooManyRequests {
			return true
		}
		if apiKeyUsageTextIndicatesQuota(err.Code) || apiKeyUsageTextIndicatesQuota(err.Message) {
			return true
		}
	}
	return apiKeyUsageTextIndicatesQuota(message)
}

func apiKeyUsageModelStatus(model string, state *coreauth.ModelState, now time.Time) apiKeyUsageModelState {
	out := apiKeyUsageModelState{Model: strings.TrimSpace(model)}
	if state == nil {
		return out
	}
	out.Status = state.Status
	out.StatusMessage = strings.TrimSpace(state.StatusMessage)
	out.StatusCode = apiKeyUsageStatusCode(state.LastError)
	out.Unavailable = state.Unavailable
	next := state.NextRetryAfter
	if state.Unavailable && !next.IsZero() && !next.After(now) {
		out.Unavailable = false
		next = time.Time{}
		if out.Status == coreauth.StatusError {
			out.Status = coreauth.StatusActive
			out.StatusMessage = ""
			out.StatusCode = 0
		}
	}
	if !next.IsZero() {
		out.NextRetryAfter, out.NextRetryMS = formatAPIKeyUsageTime(next)
	}
	out.Quota = buildAPIKeyUsageQuotaState(state.Quota)
	out.Blocked = state.Status == coreauth.StatusDisabled || state.Status == coreauth.StatusArchived || (out.Unavailable && next.After(now))
	out.Cooling = out.Blocked && apiKeyUsageIsQuotaCooldown(state.Quota, state.LastError, out.StatusMessage)
	if out.Blocked {
		out.BlockReason = apiKeyUsageBlockReason(state.Quota, state.LastError, out.StatusMessage)
	}
	return out
}

func apiKeyUsageStatusFromAuth(auth *coreauth.Auth, now time.Time) apiKeyUsageAuthStatus {
	if auth == nil {
		return apiKeyUsageAuthStatus{}
	}
	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		authIndex = strings.TrimSpace(auth.EnsureIndex())
	}
	status, statusMessage, unavailable, nextRetryAfter, statusCode := effectiveAuthFileStatus(auth, now)
	nextRetry, nextRetryMS := formatAPIKeyUsageTime(nextRetryAfter)
	out := apiKeyUsageAuthStatus{
		AuthID:         strings.TrimSpace(auth.ID),
		AuthIndex:      authIndex,
		Provider:       strings.TrimSpace(auth.Provider),
		Status:         status,
		StatusMessage:  strings.TrimSpace(statusMessage),
		StatusCode:     statusCode,
		Disabled:       auth.Disabled || status == coreauth.StatusDisabled,
		Archived:       auth.Archived || status == coreauth.StatusArchived,
		Unavailable:    unavailable,
		NextRetryAfter: nextRetry,
		NextRetryMS:    nextRetryMS,
		Quota:          buildAPIKeyUsageQuotaState(auth.Quota),
	}
	if unavailable && nextRetryAfter.After(now) {
		out.Blocked = true
	}
	if out.Disabled || out.Archived {
		out.Blocked = true
	}
	out.Cooling = out.Blocked && apiKeyUsageIsQuotaCooldown(auth.Quota, auth.LastError, out.StatusMessage)
	if out.Blocked {
		out.BlockReason = apiKeyUsageBlockReason(auth.Quota, auth.LastError, out.StatusMessage)
	}

	for model, state := range auth.ModelStates {
		modelStatus := apiKeyUsageModelStatus(model, state, now)
		if strings.TrimSpace(modelStatus.Model) == "" {
			continue
		}
		out.ModelStates = append(out.ModelStates, modelStatus)
		if modelStatus.Blocked {
			out.Blocked = true
			if modelStatus.Unavailable {
				out.Unavailable = true
			}
			if shouldPromoteAPIKeyUsageStatus(out.Cooling, out.NextRetryMS, modelStatus.Cooling, modelStatus.NextRetryMS) {
				out.Status = modelStatus.Status
				out.StatusMessage = modelStatus.StatusMessage
				out.StatusCode = modelStatus.StatusCode
				out.NextRetryAfter = modelStatus.NextRetryAfter
				out.NextRetryMS = modelStatus.NextRetryMS
				out.BlockReason = modelStatus.BlockReason
			}
			if modelStatus.Cooling {
				out.Cooling = true
			}
			if out.BlockReason == "" {
				out.BlockReason = modelStatus.BlockReason
			}
		}
	}
	return out
}

func shouldPromoteAPIKeyUsageStatus(currentCooling bool, currentRetryMS int64, candidateCooling bool, candidateRetryMS int64) bool {
	if candidateCooling && !currentCooling {
		return true
	}
	if candidateRetryMS == 0 {
		return false
	}
	if currentRetryMS == 0 {
		return true
	}
	return candidateRetryMS < currentRetryMS
}

func mergeAPIKeyUsageStatus(entry apiKeyUsageEntry, status apiKeyUsageAuthStatus) apiKeyUsageEntry {
	if strings.TrimSpace(status.AuthID) == "" && strings.TrimSpace(status.AuthIndex) == "" {
		return entry
	}
	entry.TotalAuths++
	if status.Disabled {
		entry.DisabledCount++
	}
	if status.Archived {
		entry.ArchivedCount++
	}
	if status.Blocked {
		entry.BlockedCount++
	}
	if status.Cooling {
		entry.CoolingCount++
	}
	entry.Auths = append(entry.Auths, status)
	entry.ModelStates = append(entry.ModelStates, status.ModelStates...)

	if entry.AuthID == "" || shouldPromoteAPIKeyUsageStatus(entry.Cooling, entry.NextRetryMS, status.Cooling, status.NextRetryMS) {
		entry.AuthID = status.AuthID
		entry.AuthIndex = status.AuthIndex
		entry.Status = status.Status
		entry.StatusMessage = status.StatusMessage
		entry.StatusCode = status.StatusCode
		entry.NextRetryAfter = status.NextRetryAfter
		entry.NextRetryMS = status.NextRetryMS
		entry.BlockReason = status.BlockReason
	}
	if entry.AuthIndex == "" {
		entry.AuthIndex = status.AuthIndex
	}
	entry.Disabled = entry.TotalAuths > 0 && entry.DisabledCount == entry.TotalAuths
	entry.Archived = entry.TotalAuths > 0 && entry.ArchivedCount == entry.TotalAuths
	entry.Unavailable = entry.Unavailable || status.Unavailable
	entry.Blocked = entry.Blocked || status.Blocked
	entry.Cooling = entry.Cooling || status.Cooling
	if entry.BlockReason == "" {
		entry.BlockReason = status.BlockReason
	}
	return entry
}

// GetAPIKeyUsage returns recent request buckets for all in-memory api_key auths,
// grouped by provider and keyed by "base_url|api_key".
func (h *Handler) GetAPIKeyUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	now := time.Now()
	auths := make([]apiKeyUsageAuth, 0)
	authIndexes := make([]string, 0)
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		kind, apiKey := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				baseURL = strings.TrimSpace(auth.Attributes["base-url"])
			}
		}
		compositeKey := baseURL + "|" + apiKey
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "" {
			provider = "unknown"
		}
		authIndex := strings.TrimSpace(auth.EnsureIndex())
		auths = append(auths, apiKeyUsageAuth{
			auth:         auth,
			provider:     provider,
			compositeKey: compositeKey,
			authIndex:    authIndex,
		})
		if authIndex != "" {
			authIndexes = append(authIndexes, authIndex)
		}
	}

	persisted, errPersisted := usagepersist.APIKeyUsageStatsByAuthIndex(c.Request.Context(), authIndexes, now)
	if errPersisted != nil {
		log.WithError(errPersisted).Warn("failed to load persisted api key usage stats")
		persisted = nil
	}

	out := make(map[string]map[string]apiKeyUsageEntry)
	seenPersisted := make(map[string]map[string]struct{})
	for _, item := range auths {
		auth := item.auth
		recent := auth.RecentRequestsSnapshot(now)
		success := auth.Success
		failed := auth.Failed
		usePersisted := false
		includeCounts := true
		if stats, ok := persisted[item.authIndex]; ok {
			usageKey := item.provider + "|" + item.compositeKey
			if seenPersisted[usageKey] == nil {
				seenPersisted[usageKey] = make(map[string]struct{})
			}
			if _, seen := seenPersisted[usageKey][item.authIndex]; seen {
				includeCounts = false
			} else {
				seenPersisted[usageKey][item.authIndex] = struct{}{}
				success = stats.Success
				failed = stats.Failed
				recent = coreRecentRequestBuckets(stats.RecentRequests)
				usePersisted = true
			}
		}
		if !usePersisted && strings.TrimSpace(item.authIndex) == "" {
			recent = auth.RecentRequestsSnapshot(now)
		}

		providerBucket, ok := out[item.provider]
		if !ok {
			providerBucket = make(map[string]apiKeyUsageEntry)
			out[item.provider] = providerBucket
		}
		if existing, exists := providerBucket[item.compositeKey]; exists {
			if includeCounts {
				existing.Success += success
				existing.Failed += failed
				existing.RecentRequests = mergeRecentRequestBuckets(existing.RecentRequests, recent)
			}
			existing = mergeAPIKeyUsageStatus(existing, apiKeyUsageStatusFromAuth(auth, now))
			providerBucket[item.compositeKey] = existing
			continue
		}
		entry := apiKeyUsageEntry{}
		if includeCounts {
			entry.Success = success
			entry.Failed = failed
			entry.RecentRequests = recent
		}
		entry = mergeAPIKeyUsageStatus(entry, apiKeyUsageStatusFromAuth(auth, now))
		providerBucket[item.compositeKey] = entry
	}

	c.JSON(http.StatusOK, out)
}

// ClearAPIKeyUsageCooldown clears operator-retryable availability blocks for
// API-key-backed auths. It keeps disabled and 401 unauthorized states intact.
func (h *Handler) ClearAPIKeyUsageCooldown(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var body clearAPIKeyUsageCooldownRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	targets := apiKeyUsageCooldownTargets(manager, body)
	if len(targets) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key auth not found"})
		return
	}

	statuses := make([]apiKeyUsageAuthStatus, 0, len(targets))
	for _, auth := range targets {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if h.canClearRecoverableAvailabilityState(auth.ID) {
			_ = usagepersist.ClearAuthQuotaCooldowns(c.Request.Context(), auth.ID)
			for _, model := range manager.ClearRecoverableAvailabilityState(c.Request.Context(), auth.ID) {
				_ = usagepersist.ClearCooldown(c.Request.Context(), auth.ID, model)
			}
		}
		if updated, ok := manager.GetByID(auth.ID); ok && updated != nil {
			statuses = append(statuses, apiKeyUsageStatusFromAuth(updated, time.Now()))
			continue
		}
		statuses = append(statuses, apiKeyUsageStatusFromAuth(auth, time.Now()))
	}

	c.JSON(http.StatusOK, gin.H{
		"cleared": len(statuses),
		"auths":   statuses,
	})
}

func apiKeyUsageCooldownTargets(manager *coreauth.Manager, body clearAPIKeyUsageCooldownRequest) []*coreauth.Auth {
	if manager == nil {
		return nil
	}
	authID := firstNonEmptyValue(body.AuthID, body.AuthIDCamel)
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if authID != "" {
		auth, ok := manager.GetByID(authID)
		if !ok || auth == nil || !apiKeyUsageAuthMatchesClearRequest(auth, body) {
			return nil
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			return nil
		}
		return []*coreauth.Auth{auth}
	}

	out := make([]*coreauth.Auth, 0)
	for _, auth := range manager.List() {
		if auth == nil || !apiKeyUsageAuthMatchesClearRequest(auth, body) {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		out = append(out, auth)
	}
	return out
}

func apiKeyUsageAuthMatchesClearRequest(auth *coreauth.Auth, body clearAPIKeyUsageCooldownRequest) bool {
	if auth == nil {
		return false
	}
	kind, apiKey := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return false
	}
	requestAuthID := firstNonEmptyValue(body.AuthID, body.AuthIDCamel)
	if requestAuthID != "" && strings.TrimSpace(auth.ID) != requestAuthID {
		return false
	}
	authIndex := firstNonEmptyValue(body.AuthIndex, body.AuthIndexCamel)
	if authIndex != "" {
		liveIndex := strings.TrimSpace(auth.Index)
		if liveIndex == "" {
			liveIndex = strings.TrimSpace(auth.EnsureIndex())
		}
		if liveIndex != authIndex {
			return false
		}
	}
	requestAPIKey := firstNonEmptyValue(body.APIKey, body.APIKeyCamel)
	if requestAPIKey != "" && strings.TrimSpace(apiKey) != requestAPIKey {
		return false
	}
	requestBaseURL := firstNonEmptyValue(body.BaseURL, body.BaseURLCamel)
	if requestBaseURL != "" || body.BaseURL != "" || body.BaseURLCamel != "" {
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				baseURL = strings.TrimSpace(auth.Attributes["base-url"])
			}
		}
		if baseURL != requestBaseURL {
			return false
		}
	}
	if requestAuthID == "" && authIndex == "" && requestAPIKey == "" {
		return false
	}
	return true
}
