package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
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
	usages, errUsage := usagepersist.CredentialTokenUsages(c.Request.Context())
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

	snapshot, errUpsert := usagepersist.UpsertQuotaSnapshot(c.Request.Context(), usagepersist.QuotaSnapshot{
		Provider:  body.Provider,
		AuthID:    firstNonEmptyValue(body.AuthID, body.AuthIDCamel),
		AuthIndex: firstNonEmptyValue(body.AuthIndex, body.AuthIndexCamel),
		FileName:  firstNonEmptyValue(body.FileName, body.FileNameCamel),
		QuotaJSON: quota,
	})
	if errUpsert != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errUpsert.Error()})
		return
	}

	c.JSON(http.StatusOK, quotaSnapshotToResponse(snapshot))
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
