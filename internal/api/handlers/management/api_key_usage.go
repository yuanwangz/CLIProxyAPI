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
}

type apiKeyUsageAuth struct {
	auth         *coreauth.Auth
	provider     string
	compositeKey string
	authIndex    string
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
		if stats, ok := persisted[item.authIndex]; ok {
			usageKey := item.provider + "|" + item.compositeKey
			if seenPersisted[usageKey] == nil {
				seenPersisted[usageKey] = make(map[string]struct{})
			}
			if _, seen := seenPersisted[usageKey][item.authIndex]; seen {
				continue
			}
			seenPersisted[usageKey][item.authIndex] = struct{}{}
			success = stats.Success
			failed = stats.Failed
			recent = coreRecentRequestBuckets(stats.RecentRequests)
			usePersisted = true
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
			existing.Success += success
			existing.Failed += failed
			existing.RecentRequests = mergeRecentRequestBuckets(existing.RecentRequests, recent)
			providerBucket[item.compositeKey] = existing
			continue
		}
		providerBucket[item.compositeKey] = apiKeyUsageEntry{
			Success:        success,
			Failed:         failed,
			RecentRequests: recent,
		}
	}

	c.JSON(http.StatusOK, out)
}
