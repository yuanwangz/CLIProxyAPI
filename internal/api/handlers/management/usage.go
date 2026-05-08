package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usagepersist"
)

const maxUsageImportBytes int64 = 64 * 1024 * 1024

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

func (h *Handler) GetUsageStatistics(c *gin.Context) {
	payload, err := usagepersist.CurrentPayload(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	data, err := usagepersist.ExportJSONL(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", `attachment; filename="usage-events.jsonl"`)
	c.Data(http.StatusOK, "application/x-ndjson", data)
}

func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxUsageImportBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := usagepersist.Import(c.Request.Context(), data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":       err.Error(),
			"added":       result.Added,
			"skipped":     result.Skipped,
			"total":       result.Total,
			"failed":      result.Failed,
			"unsupported": result.Unsupported,
			"warnings":    result.Warnings,
		})
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}
