package usagepersist

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestBuildEventNormalizesUsageRecord(t *testing.T) {
	ctx := internallogging.WithRequestID(context.Background(), "req-1")
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
	ctx = internallogging.WithResponseStatusHolder(ctx)
	internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

	event := BuildEvent(ctx, coreusage.Record{
		Provider:    "openai",
		Model:       "gpt-5",
		Alias:       "client-gpt",
		APIKey:      "sk-test-1234567890abcdefghijklmnopqrstuvwxyz",
		AuthType:    "oauth",
		AuthIndex:   "account-1",
		Source:      "person@example.com",
		RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:     10,
			OutputTokens:    20,
			ReasoningTokens: 5,
			CachedTokens:    2,
		},
	})

	if event.RequestID != "req-1" {
		t.Fatalf("request id = %q, want req-1", event.RequestID)
	}
	if event.Endpoint != "POST /v1/chat/completions" || event.Method != "POST" || event.Path != "/v1/chat/completions" {
		t.Fatalf("endpoint fields = %q %q %q", event.Endpoint, event.Method, event.Path)
	}
	if !event.Failed {
		t.Fatal("failed = false, want true")
	}
	if event.Source != "per***@example.com" {
		t.Fatalf("source = %q, want masked email", event.Source)
	}
	if event.SourceFull != "person@example.com" {
		t.Fatalf("source full = %q, want full email", event.SourceFull)
	}
	if event.APIKey != "sk-t...wxyz" {
		t.Fatalf("api key = %q, want masked client key", event.APIKey)
	}
	if event.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want %d", event.StatusCode, http.StatusInternalServerError)
	}
	if event.SourceHash == "" || event.APIKeyHash == "" || event.EventHash == "" {
		t.Fatalf("hash fields must be populated: source=%q api=%q event=%q", event.SourceHash, event.APIKeyHash, event.EventHash)
	}
	if event.TotalTokens != 35 {
		t.Fatalf("total tokens = %d, want 35", event.TotalTokens)
	}
	if event.CachedTokens != 2 || event.CacheTokens != 2 {
		t.Fatalf("cache tokens = %d/%d, want 2/2", event.CachedTokens, event.CacheTokens)
	}
	if event.LatencyMS == nil || *event.LatencyMS != 1500 {
		t.Fatalf("latency = %v, want 1500ms", event.LatencyMS)
	}
	if !json.Valid([]byte(event.RawJSON)) {
		t.Fatalf("raw json is invalid: %s", event.RawJSON)
	}
}

func TestStoreAggregatesAndExportsEvents(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	latency := int64(42)

	result, err := store.InsertEvents(ctx, []Event{
		{
			RequestID:    "req-1",
			Timestamp:    "2026-01-02T03:04:05Z",
			Endpoint:     "POST /v1/chat/completions",
			Model:        "gpt-5",
			Source:       "local",
			SourceFull:   "local-full",
			APIKey:       "client-key-123456",
			APIKeyHash:   "client-key-hash",
			AuthIndex:    "account-1",
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
			LatencyMS:    &latency,
		},
		{
			RequestID:    "req-2",
			Timestamp:    "2026-01-02T03:05:05Z",
			Endpoint:     "POST /v1/chat/completions",
			Model:        "gpt-5",
			Source:       "local",
			SourceFull:   "local-full",
			APIKey:       "client-key-123456",
			APIKeyHash:   "client-key-hash",
			OutputTokens: 4,
			TotalTokens:  4,
			Failed:       true,
			StatusCode:   http.StatusTooManyRequests,
		},
	})
	if err != nil {
		t.Fatalf("insert events: %v", err)
	}
	if result.Added != 2 || result.Skipped != 0 {
		t.Fatalf("insert result = %+v, want 2 added", result)
	}

	duplicate, err := store.InsertEvents(ctx, []Event{
		{
			RequestID:    "req-1",
			Timestamp:    "2026-01-02T03:04:05Z",
			Endpoint:     "POST /v1/chat/completions",
			Model:        "gpt-5",
			Source:       "local",
			SourceFull:   "local-full",
			APIKey:       "client-key-123456",
			APIKeyHash:   "client-key-hash",
			AuthIndex:    "account-1",
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
			LatencyMS:    &latency,
		},
	})
	if err != nil {
		t.Fatalf("insert duplicate: %v", err)
	}
	if duplicate.Added != 0 || duplicate.Skipped != 1 {
		t.Fatalf("duplicate result = %+v, want skipped duplicate", duplicate)
	}

	events, err := store.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	payload := BuildPayload(events)
	if payload.TotalRequests != 2 || payload.SuccessCount != 1 || payload.FailureCount != 1 || payload.TotalTokens != 7 {
		t.Fatalf("payload totals = %+v", payload)
	}
	apiEntry := payload.APIs["POST /v1/chat/completions"]
	if apiEntry == nil || apiEntry.Models["gpt-5"] == nil || len(apiEntry.Models["gpt-5"].Details) != 2 {
		t.Fatalf("payload api entry = %+v", apiEntry)
	}
	detail := apiEntry.Models["gpt-5"].Details[0]
	if detail.SourceFull != "local-full" || detail.APIKey != "clie...3456" || detail.APIKeyHash != "client-key-hash" || detail.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("payload detail did not preserve extended fields: %+v", detail)
	}

	exported, err := store.ExportJSONL(ctx)
	if err != nil {
		t.Fatalf("export jsonl: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(exported), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("exported lines = %d, want 2; payload=%s", len(lines), exported)
	}
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("line %d is not an event: %v", i, err)
		}
	}
}

func TestRecentEventsHandlesNullOptionalTextFields(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	_, err := store.db.ExecContext(ctx, `insert into usage_events (
		event_hash, timestamp_ms, timestamp, model, source, input_tokens, output_tokens, total_tokens, created_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"null-source-full",
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli(),
		"2026-01-02T03:04:05Z",
		"gpt-5",
		"local",
		1,
		2,
		3,
		time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC).UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert raw event: %v", err)
	}

	events, err := store.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].SourceFull != "" || events[0].RawJSON != "" {
		t.Fatalf("nullable text fields were not normalized: %+v", events[0])
	}
}

func TestParseImportPayloadKeepsValidJSONLRecords(t *testing.T) {
	events, result, err := parseImportPayload([]byte(`{"timestamp":"2026-01-02T03:04:05Z","endpoint":"POST /v1/responses","model":"gpt-5","total_tokens":7}` + "\n" + `not-json`))
	if err != nil {
		t.Fatalf("parse jsonl: %v", err)
	}
	if result.Total != 2 || result.Failed != 1 || len(result.Warnings) != 1 {
		t.Fatalf("result = %+v, want one failed line", result)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].TimestampMS == 0 || events[0].EventHash == "" || events[0].Method != "POST" || events[0].Path != "/v1/responses" {
		t.Fatalf("event was not normalized: %+v", events[0])
	}
}

func TestParseImportPayloadRejectsUnsupportedObject(t *testing.T) {
	events, result, err := parseImportPayload([]byte(`{"usage":{"apis":{}}}`))
	if err == nil {
		t.Fatal("parse unsupported payload error = nil, want error")
	}
	if len(events) != 0 || result.Total != 1 || result.Unsupported != 1 {
		t.Fatalf("events=%d result=%+v, want one unsupported item", len(events), result)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	return store
}
