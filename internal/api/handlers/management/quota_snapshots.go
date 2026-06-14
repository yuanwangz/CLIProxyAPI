package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type quotaSnapshotRequest struct {
	Provider       string          `json:"provider"`
	AuthID         string          `json:"auth_id"`
	AuthIDCamel    string          `json:"authId"`
	AuthIndex      string          `json:"auth_index"`
	AuthIndexCamel string          `json:"authIndex"`
	FileName       string          `json:"file_name"`
	FileNameCamel  string          `json:"fileName"`
	Quota          json.RawMessage `json:"quota"`
}

type quotaSnapshotResponse struct {
	Provider      string          `json:"provider"`
	AuthID        string          `json:"auth_id,omitempty"`
	AuthIndex     string          `json:"auth_index"`
	FileName      string          `json:"file_name,omitempty"`
	Quota         json.RawMessage `json:"quota"`
	RefreshedAt   string          `json:"refreshed_at"`
	RefreshedAtMS int64           `json:"refreshed_at_ms"`
	UpdatedAt     string          `json:"updated_at"`
	UpdatedAtMS   int64           `json:"updated_at_ms"`
}

// ListQuotaSnapshots returns persisted quota snapshots and credential token totals.
func (h *Handler) ListQuotaSnapshots(c *gin.Context) {
	snapshots, errSnapshots := usagepersist.QuotaSnapshots(c.Request.Context())
	if errSnapshots != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSnapshots.Error()})
		return
	}
	usages, errUsage := usagepersist.CredentialTokenUsagesForQuotaSnapshots(c.Request.Context(), snapshots)
	if errUsage != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errUsage.Error()})
		return
	}

	usageByAuthIndex := make(map[string]usagepersist.CredentialTokenUsage, len(usages))
	for _, usage := range usages {
		if authIndex := strings.TrimSpace(usage.AuthIndex); authIndex != "" {
			usageByAuthIndex[authIndex] = usage
		}
	}

	out := make([]quotaSnapshotResponse, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, quotaSnapshotToResponse(snapshot))
	}

	c.JSON(http.StatusOK, gin.H{
		"snapshots":   out,
		"token_usage": usageByAuthIndex,
	})
}

// PutQuotaSnapshot upserts one successful quota snapshot captured by the management UI.
func (h *Handler) PutQuotaSnapshot(c *gin.Context) {
	var body quotaSnapshotRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	quota := strings.TrimSpace(string(body.Quota))
	if quota == "" || !json.Valid([]byte(quota)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota must be a valid JSON value"})
		return
	}

	snapshotInput := usagepersist.QuotaSnapshot{
		Provider:  body.Provider,
		AuthID:    firstNonEmptyValue(body.AuthID, body.AuthIDCamel),
		AuthIndex: firstNonEmptyValue(body.AuthIndex, body.AuthIndexCamel),
		FileName:  firstNonEmptyValue(body.FileName, body.FileNameCamel),
		QuotaJSON: quota,
	}
	if strings.TrimSpace(snapshotInput.AuthID) == "" {
		snapshotInput.AuthID = h.authIDByQuotaSnapshot(snapshotInput)
	}

	snapshot, errUpsert := usagepersist.UpsertQuotaSnapshot(c.Request.Context(), snapshotInput)
	if errUpsert != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errUpsert.Error()})
		return
	}
	codexquota.PublishRoutingHintFromSnapshot(snapshot.Provider, snapshot.AuthID, snapshot.QuotaJSON)
	if quotaSnapshotHasAvailableCapacity(snapshot) {
		h.clearQuotaCooldownState(c.Request.Context(), snapshot)
	}

	c.JSON(http.StatusOK, quotaSnapshotToResponse(snapshot))
}

func (h *Handler) clearQuotaCooldownState(ctx context.Context, snapshot usagepersist.QuotaSnapshot) {
	authID := strings.TrimSpace(snapshot.AuthID)
	if authID == "" {
		authID = h.authIDByQuotaSnapshot(snapshot)
	}
	if authID == "" {
		return
	}
	if !h.canClearQuotaCooldownState(authID) {
		return
	}
	// The refreshed quota snapshot is already saved. Stale cooldown cleanup is best-effort.
	_ = usagepersist.ClearAuthQuotaCooldowns(ctx, authID)
	if h != nil && h.authManager != nil {
		h.authManager.ClearQuotaCooldownState(ctx, authID)
	}
}

func (h *Handler) canClearQuotaCooldownState(authID string) bool {
	return h.canClearRecoverableAvailabilityState(authID)
}

func (h *Handler) canClearRecoverableAvailabilityState(authID string) bool {
	if h == nil || h.authManager == nil {
		return true
	}
	auth, ok := h.authManager.GetByID(strings.TrimSpace(authID))
	if !ok || auth == nil {
		return true
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	return auth.LastError == nil || auth.LastError.StatusCode() != http.StatusUnauthorized
}

func (h *Handler) authIDByQuotaSnapshot(snapshot usagepersist.QuotaSnapshot) string {
	if h == nil || h.authManager == nil {
		return ""
	}
	authIndex := strings.TrimSpace(snapshot.AuthIndex)
	if authIndex == "" {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(snapshot.Provider))
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		if strings.TrimSpace(auth.Index) == authIndex || strings.TrimSpace(auth.EnsureIndex()) == authIndex {
			return strings.TrimSpace(auth.ID)
		}
	}
	return ""
}

type quotaAvailability int

const (
	quotaAvailabilityUnknown quotaAvailability = iota
	quotaAvailabilityAvailable
	quotaAvailabilityExhausted
)

func quotaSnapshotHasAvailableCapacity(snapshot usagepersist.QuotaSnapshot) bool {
	var root any
	if err := json.Unmarshal([]byte(snapshot.QuotaJSON), &root); err != nil {
		return false
	}
	record, ok := root.(map[string]any)
	if !ok {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(quotaStringValue(record["status"])), "success") {
		return false
	}

	for _, key := range []string{"windows", "rows"} {
		if availability := quotaAllLimitsAvailable(record[key]); availability != quotaAvailabilityUnknown {
			return availability == quotaAvailabilityAvailable
		}
	}
	if availability := quotaValueAvailability(record["billing"]); availability != quotaAvailabilityUnknown {
		return availability == quotaAvailabilityAvailable
	}
	for _, key := range []string{"groups", "buckets"} {
		if availability := quotaAnyLimitAvailable(record[key]); availability != quotaAvailabilityUnknown {
			return availability == quotaAvailabilityAvailable
		}
	}
	return quotaAnyLimitAvailable(record) == quotaAvailabilityAvailable
}

func quotaAllLimitsAvailable(value any) quotaAvailability {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return quotaAvailabilityUnknown
	}
	found := false
	for _, item := range values {
		switch quotaValueAvailability(item) {
		case quotaAvailabilityExhausted:
			return quotaAvailabilityExhausted
		case quotaAvailabilityAvailable:
			found = true
		}
	}
	if found {
		return quotaAvailabilityAvailable
	}
	return quotaAvailabilityUnknown
}

func quotaAnyLimitAvailable(value any) quotaAvailability {
	switch typed := value.(type) {
	case []any:
		foundExhausted := false
		for _, item := range typed {
			switch quotaValueAvailability(item) {
			case quotaAvailabilityAvailable:
				return quotaAvailabilityAvailable
			case quotaAvailabilityExhausted:
				foundExhausted = true
			}
		}
		if foundExhausted {
			return quotaAvailabilityExhausted
		}
	case map[string]any:
		foundExhausted := false
		if availability := quotaValueAvailability(typed); availability != quotaAvailabilityUnknown {
			if availability == quotaAvailabilityAvailable {
				return quotaAvailabilityAvailable
			}
			foundExhausted = true
		}
		for _, child := range typed {
			switch quotaAnyLimitAvailable(child) {
			case quotaAvailabilityAvailable:
				return quotaAvailabilityAvailable
			case quotaAvailabilityExhausted:
				foundExhausted = true
			}
		}
		if foundExhausted {
			return quotaAvailabilityExhausted
		}
	}
	return quotaAvailabilityUnknown
}

func quotaValueAvailability(value any) quotaAvailability {
	record, ok := value.(map[string]any)
	if !ok {
		return quotaAvailabilityUnknown
	}
	if boolValue(record["unlimited"]) {
		return quotaAvailabilityAvailable
	}
	if allowed, okBool := optionalBool(record["allowed"]); okBool {
		if allowed {
			return quotaAvailabilityAvailable
		}
		return quotaAvailabilityExhausted
	}
	for _, key := range []string{"limitReached", "limit_reached"} {
		if value, okBool := optionalBool(record[key]); okBool {
			if value {
				return quotaAvailabilityExhausted
			}
			return quotaAvailabilityAvailable
		}
	}
	for _, key := range []string{"usedPercent", "used_percent", "utilization"} {
		if number, okNumber := quotaNumberValue(record[key]); okNumber {
			if number >= 100 {
				return quotaAvailabilityExhausted
			}
			return quotaAvailabilityAvailable
		}
	}
	for _, key := range []string{
		"remainingFraction", "remaining_fraction",
		"remainingAmount", "remaining_amount",
		"remaining",
	} {
		if number, okNumber := quotaNumberValue(record[key]); okNumber {
			if number > 0 {
				return quotaAvailabilityAvailable
			}
			return quotaAvailabilityExhausted
		}
	}
	used, hasUsed := quotaNumberValue(record["used"])
	limit, hasLimit := quotaNumberValue(record["limit"])
	if hasUsed && hasLimit && limit > 0 {
		if used < limit {
			return quotaAvailabilityAvailable
		}
		return quotaAvailabilityExhausted
	}
	return quotaAvailabilityUnknown
}

func quotaNumberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func optionalBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return false, false
	}
}

func boolValue(value any) bool {
	parsed, ok := optionalBool(value)
	return ok && parsed
}

func quotaStringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func quotaSnapshotToResponse(snapshot usagepersist.QuotaSnapshot) quotaSnapshotResponse {
	quota := json.RawMessage(snapshot.QuotaJSON)
	if !json.Valid(quota) {
		quota = json.RawMessage(`null`)
	}
	return quotaSnapshotResponse{
		Provider:      snapshot.Provider,
		AuthID:        snapshot.AuthID,
		AuthIndex:     snapshot.AuthIndex,
		FileName:      snapshot.FileName,
		Quota:         quota,
		RefreshedAt:   formatQuotaSnapshotTime(snapshot.RefreshedAt),
		RefreshedAtMS: snapshot.RefreshedAt.UnixMilli(),
		UpdatedAt:     formatQuotaSnapshotTime(snapshot.UpdatedAt),
		UpdatedAtMS:   snapshot.UpdatedAt.UnixMilli(),
	}
}

func formatQuotaSnapshotTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
