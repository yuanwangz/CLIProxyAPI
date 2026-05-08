package usagepersist

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	defaultQueryLimit = 50000
	queueSize         = 2048
	cooldownQueueSize = 1024
)

type Store struct {
	db *sql.DB
}

type Plugin struct {
	ch   chan event
	stop chan struct{}
}

type CooldownState struct {
	AuthID         string
	AuthIndex      string
	Provider       string
	Model          string
	Reason         string
	StatusMessage  string
	HTTPStatus     int
	NextRetryAfter time.Time
	QuotaExceeded  bool
	BackoffLevel   int
	UpdatedAt      time.Time
}

type Event struct {
	RequestID       string `json:"request_id,omitempty"`
	EventHash       string `json:"event_hash"`
	TimestampMS     int64  `json:"timestamp_ms"`
	Timestamp       string `json:"timestamp"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model"`
	Endpoint        string `json:"endpoint,omitempty"`
	Method          string `json:"method,omitempty"`
	Path            string `json:"path,omitempty"`
	AuthType        string `json:"auth_type,omitempty"`
	AuthIndex       string `json:"auth_index,omitempty"`
	Source          string `json:"source,omitempty"`
	SourceHash      string `json:"source_hash,omitempty"`
	APIKeyHash      string `json:"api_key_hash,omitempty"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	CacheTokens     int64  `json:"cache_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	LatencyMS       *int64 `json:"latency_ms,omitempty"`
	Failed          bool   `json:"failed"`
	RawJSON         string `json:"raw_json,omitempty"`
	CreatedAtMS     int64  `json:"created_at_ms"`
}

type event = Event

type Tokens struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	CacheTokens     int64 `json:"cache_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type Detail struct {
	Timestamp string `json:"timestamp"`
	Source    string `json:"source"`
	AuthIndex string `json:"auth_index,omitempty"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Tokens    Tokens `json:"tokens"`
	Failed    bool   `json:"failed"`
}

type ModelAggregate struct {
	Details []Detail `json:"details"`
}

type APIAggregate struct {
	Models map[string]*ModelAggregate `json:"models"`
}

type Payload struct {
	TotalRequests int64                    `json:"total_requests"`
	SuccessCount  int64                    `json:"success_count"`
	FailureCount  int64                    `json:"failure_count"`
	TotalTokens   int64                    `json:"total_tokens"`
	APIs          map[string]*APIAggregate `json:"apis"`
}

type ImportResult struct {
	Added       int      `json:"added"`
	Skipped     int      `json:"skipped"`
	Total       int      `json:"total"`
	Failed      int      `json:"failed"`
	Unsupported int      `json:"unsupported"`
	Warnings    []string `json:"warnings,omitempty"`
}

var (
	enabled      atomic.Bool
	defaultMu    sync.RWMutex
	defaultStore *Store
	pluginOnce   sync.Once
	cooldownOnce sync.Once
	cooldownCh   chan cooldownCommand

	errUnsupportedImportEvent = errors.New("unsupported usage import event")
)

type cooldownCommand struct {
	action string
	state  CooldownState
	authID string
	model  string
}

func DefaultDBPath(wd string, writableBase string) string {
	if trimmed := strings.TrimSpace(writableBase); trimmed != "" {
		return filepath.Join(filepath.Clean(trimmed), "usage.sqlite")
	}
	if strings.TrimSpace(wd) == "" {
		wd = "."
	}
	return filepath.Join(filepath.Clean(wd), "data", "usage.sqlite")
}

func Init(path string, initiallyEnabled bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("usage persistence path is empty")
	}
	store, err := Open(path)
	if err != nil {
		return err
	}

	defaultMu.Lock()
	if defaultStore != nil {
		_ = defaultStore.Close()
	}
	defaultStore = store
	defaultMu.Unlock()

	enabled.Store(initiallyEnabled)
	pluginOnce.Do(func() {
		plugin := &Plugin{
			ch:   make(chan event, queueSize),
			stop: make(chan struct{}),
		}
		go plugin.run()
		coreusage.RegisterPlugin(plugin)
	})
	cooldownOnce.Do(func() {
		cooldownCh = make(chan cooldownCommand, cooldownQueueSize)
		go runCooldownCommands()
	})
	return nil
}

func SetEnabled(value bool) {
	enabled.Store(value)
}

func Enabled() bool {
	return enabled.Load()
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	statements := []string{
		`pragma journal_mode = WAL`,
		`pragma synchronous = NORMAL`,
		`pragma busy_timeout = 5000`,
		`create table if not exists usage_events (
			id integer primary key autoincrement,
			request_id text,
			event_hash text not null unique,
			timestamp_ms integer not null,
			timestamp text not null,
			provider text,
			model text not null,
			endpoint text,
			method text,
			path text,
			auth_type text,
			auth_index text,
			source text,
			source_hash text,
			api_key_hash text,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			reasoning_tokens integer not null default 0,
			cached_tokens integer not null default 0,
			cache_tokens integer not null default 0,
			total_tokens integer not null default 0,
			latency_ms integer,
			failed integer not null default 0,
			raw_json text,
			created_at_ms integer not null
		)`,
		`create index if not exists idx_usage_events_timestamp on usage_events(timestamp_ms)`,
		`create index if not exists idx_usage_events_request_id on usage_events(request_id)`,
		`create index if not exists idx_usage_events_model on usage_events(model)`,
		`create index if not exists idx_usage_events_auth_index on usage_events(auth_index)`,
		`create index if not exists idx_usage_events_endpoint on usage_events(endpoint)`,
		`create table if not exists auth_cooldowns (
			auth_id text not null,
			auth_index text,
			provider text,
			model text not null,
			reason text,
			status_message text,
			http_status integer not null default 0,
			next_retry_after_ms integer not null,
			quota_exceeded integer not null default 0,
			backoff_level integer not null default 0,
			updated_at_ms integer not null,
			primary key(auth_id, model)
		)`,
		`create index if not exists idx_auth_cooldowns_auth_index on auth_cooldowns(auth_index)`,
		`create index if not exists idx_auth_cooldowns_next_retry on auth_cooldowns(next_retry_after_ms)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !Enabled() {
		return
	}
	event := BuildEvent(ctx, record)
	select {
	case p.ch <- event:
	default:
		log.Warn("usage persistence queue is full; dropping usage event")
	}
}

func (p *Plugin) run() {
	for {
		select {
		case <-p.stop:
			return
		case event := <-p.ch:
			store := DefaultStore()
			if store == nil {
				continue
			}
			if _, err := store.InsertEvents(context.Background(), []Event{event}); err != nil {
				log.WithError(err).Warn("failed to persist usage event")
			}
		}
	}
}

func runCooldownCommands() {
	for command := range cooldownCh {
		store := DefaultStore()
		if store == nil {
			continue
		}
		ctx := context.Background()
		var err error
		switch command.action {
		case "upsert":
			err = store.UpsertCooldown(ctx, command.state)
		case "delete":
			err = store.DeleteCooldown(ctx, command.authID, command.model)
		case "delete-auth":
			err = store.DeleteAuthCooldowns(ctx, command.authID)
		}
		if err != nil {
			log.WithError(err).Warn("failed to update persisted auth cooldown")
		}
	}
}

func PersistCooldownAsync(state CooldownState) {
	enqueueCooldownCommand(cooldownCommand{action: "upsert", state: state})
}

func ClearCooldownAsync(authID, model string) {
	enqueueCooldownCommand(cooldownCommand{
		action: "delete",
		authID: strings.TrimSpace(authID),
		model:  strings.TrimSpace(model),
	})
}

func ClearAuthCooldownsAsync(authID string) {
	enqueueCooldownCommand(cooldownCommand{
		action: "delete-auth",
		authID: strings.TrimSpace(authID),
	})
}

func enqueueCooldownCommand(command cooldownCommand) {
	if cooldownCh == nil {
		return
	}
	select {
	case cooldownCh <- command:
	default:
		log.Warn("auth cooldown persistence queue is full; dropping cooldown update")
	}
}

func DefaultStore() *Store {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultStore
}

func BuildEvent(ctx context.Context, record coreusage.Record) Event {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	timestamp = timestamp.UTC()

	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "-"
	}
	provider := strings.TrimSpace(record.Provider)
	endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx))
	if endpoint == "" {
		endpoint = "-"
	}
	method, path := splitEndpoint(endpoint)

	tokens := normalizeTokens(record.Detail)
	latency := record.Latency.Milliseconds()
	var latencyPtr *int64
	if latency > 0 {
		latencyPtr = &latency
	}

	failed := record.Failed
	if !failed {
		failed = !responseSucceeded(ctx)
	}

	sourceRaw := strings.TrimSpace(record.Source)
	apiKey := strings.TrimSpace(record.APIKey)
	event := Event{
		RequestID:       strings.TrimSpace(internallogging.GetRequestID(ctx)),
		TimestampMS:     timestamp.UnixMilli(),
		Timestamp:       timestamp.Format(time.RFC3339Nano),
		Provider:        provider,
		Model:           model,
		Endpoint:        endpoint,
		Method:          method,
		Path:            path,
		AuthType:        strings.TrimSpace(record.AuthType),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		Source:          maskSource(sourceRaw),
		SourceHash:      hashString(sourceRaw),
		APIKeyHash:      hashString(apiKey),
		InputTokens:     tokens.InputTokens,
		OutputTokens:    tokens.OutputTokens,
		ReasoningTokens: tokens.ReasoningTokens,
		CachedTokens:    tokens.CachedTokens,
		CacheTokens:     tokens.CacheTokens,
		TotalTokens:     tokens.TotalTokens,
		LatencyMS:       latencyPtr,
		Failed:          failed,
		CreatedAtMS:     time.Now().UnixMilli(),
	}
	event.RawJSON = rawEventJSON(event, record.Alias)
	event.EventHash = buildEventHash(event)
	return event
}

func InsertEvents(ctx context.Context, events []Event) (ImportResult, error) {
	store := DefaultStore()
	if store == nil {
		return ImportResult{}, errors.New("usage persistence is not initialized")
	}
	return store.InsertEvents(ctx, events)
}

func (s *Store) InsertEvents(ctx context.Context, events []Event) (ImportResult, error) {
	if s == nil || s.db == nil {
		return ImportResult{}, errors.New("usage store is closed")
	}
	result := ImportResult{Total: len(events)}
	if len(events) == 0 {
		return result, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	stmt, err := tx.PrepareContext(ctx, `insert or ignore into usage_events (
		request_id, event_hash, timestamp_ms, timestamp, provider, model, endpoint, method, path,
		auth_type, auth_index, source, source_hash, api_key_hash, input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, cache_tokens, total_tokens, latency_ms, failed, raw_json,
		created_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return result, err
	}
	defer stmt.Close()

	for _, event := range events {
		event = normalizeEvent(event)
		res, errExec := stmt.ExecContext(ctx,
			event.RequestID,
			event.EventHash,
			event.TimestampMS,
			event.Timestamp,
			event.Provider,
			event.Model,
			event.Endpoint,
			event.Method,
			event.Path,
			event.AuthType,
			event.AuthIndex,
			event.Source,
			event.SourceHash,
			event.APIKeyHash,
			event.InputTokens,
			event.OutputTokens,
			event.ReasoningTokens,
			event.CachedTokens,
			event.CacheTokens,
			event.TotalTokens,
			nullInt64(event.LatencyMS),
			boolToInt(event.Failed),
			event.RawJSON,
			event.CreatedAtMS,
		)
		if errExec != nil {
			return result, errExec
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			result.Skipped++
		} else {
			result.Added++
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func CurrentPayload(ctx context.Context) (Payload, error) {
	store := DefaultStore()
	if store == nil {
		return BuildPayload(nil), nil
	}
	events, err := store.RecentEvents(ctx, defaultQueryLimit)
	if err != nil {
		return Payload{}, err
	}
	return BuildPayload(events), nil
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage store is closed")
	}
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	rows, err := s.db.QueryContext(ctx, `select
		request_id, event_hash, timestamp_ms, timestamp, provider, model, endpoint, method, path,
		auth_type, auth_index, source, source_hash, api_key_hash, input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, cache_tokens, total_tokens, latency_ms, failed, raw_json,
		created_at_ms
		from usage_events order by timestamp_ms desc, id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var latency sql.NullInt64
		var failed int
		if err := rows.Scan(
			&event.RequestID,
			&event.EventHash,
			&event.TimestampMS,
			&event.Timestamp,
			&event.Provider,
			&event.Model,
			&event.Endpoint,
			&event.Method,
			&event.Path,
			&event.AuthType,
			&event.AuthIndex,
			&event.Source,
			&event.SourceHash,
			&event.APIKeyHash,
			&event.InputTokens,
			&event.OutputTokens,
			&event.ReasoningTokens,
			&event.CachedTokens,
			&event.CacheTokens,
			&event.TotalTokens,
			&latency,
			&failed,
			&event.RawJSON,
			&event.CreatedAtMS,
		); err != nil {
			return nil, err
		}
		if latency.Valid {
			value := latency.Int64
			event.LatencyMS = &value
		}
		event.Failed = failed != 0
		events = append(events, event)
	}
	return events, rows.Err()
}

func ActiveCooldownsByAuth(ctx context.Context, authID string, now time.Time) ([]CooldownState, error) {
	store := DefaultStore()
	if store == nil {
		return nil, nil
	}
	return store.ActiveCooldownsByAuth(ctx, authID, now)
}

func (s *Store) UpsertCooldown(ctx context.Context, state CooldownState) error {
	if s == nil || s.db == nil {
		return errors.New("usage store is closed")
	}
	state = normalizeCooldownState(state)
	if strings.TrimSpace(state.AuthID) == "" {
		return errors.New("auth cooldown auth_id is empty")
	}
	if state.NextRetryAfter.IsZero() {
		return s.DeleteCooldown(ctx, state.AuthID, state.Model)
	}
	if !state.NextRetryAfter.After(time.Now()) {
		return s.DeleteCooldown(ctx, state.AuthID, state.Model)
	}
	_, err := s.db.ExecContext(ctx, `insert into auth_cooldowns (
		auth_id, auth_index, provider, model, reason, status_message, http_status,
		next_retry_after_ms, quota_exceeded, backoff_level, updated_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	on conflict(auth_id, model) do update set
		auth_index = excluded.auth_index,
		provider = excluded.provider,
		reason = excluded.reason,
		status_message = excluded.status_message,
		http_status = excluded.http_status,
		next_retry_after_ms = excluded.next_retry_after_ms,
		quota_exceeded = excluded.quota_exceeded,
		backoff_level = excluded.backoff_level,
		updated_at_ms = excluded.updated_at_ms`,
		state.AuthID,
		state.AuthIndex,
		state.Provider,
		state.Model,
		state.Reason,
		state.StatusMessage,
		state.HTTPStatus,
		state.NextRetryAfter.UnixMilli(),
		boolToInt(state.QuotaExceeded),
		state.BackoffLevel,
		state.UpdatedAt.UnixMilli(),
	)
	return err
}

func (s *Store) DeleteCooldown(ctx context.Context, authID, model string) error {
	if s == nil || s.db == nil {
		return errors.New("usage store is closed")
	}
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `delete from auth_cooldowns where auth_id = ? and model = ?`, authID, model)
	return err
}

func (s *Store) DeleteAuthCooldowns(ctx context.Context, authID string) error {
	if s == nil || s.db == nil {
		return errors.New("usage store is closed")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `delete from auth_cooldowns where auth_id = ?`, authID)
	return err
}

func (s *Store) ActiveCooldownsByAuth(ctx context.Context, authID string, now time.Time) ([]CooldownState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage store is closed")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if _, err := s.db.ExecContext(ctx, `delete from auth_cooldowns where next_retry_after_ms <= ?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select
		auth_id, auth_index, provider, model, reason, status_message, http_status,
		next_retry_after_ms, quota_exceeded, backoff_level, updated_at_ms
		from auth_cooldowns
		where auth_id = ? and next_retry_after_ms > ?
		order by next_retry_after_ms desc`, authID, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CooldownState, 0)
	for rows.Next() {
		var state CooldownState
		var nextRetryAfterMS int64
		var updatedAtMS int64
		var quotaExceeded int
		if err := rows.Scan(
			&state.AuthID,
			&state.AuthIndex,
			&state.Provider,
			&state.Model,
			&state.Reason,
			&state.StatusMessage,
			&state.HTTPStatus,
			&nextRetryAfterMS,
			&quotaExceeded,
			&state.BackoffLevel,
			&updatedAtMS,
		); err != nil {
			return nil, err
		}
		state.NextRetryAfter = time.UnixMilli(nextRetryAfterMS).UTC()
		state.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
		state.QuotaExceeded = quotaExceeded != 0
		out = append(out, normalizeCooldownState(state))
	}
	return out, rows.Err()
}

func BuildPayload(events []Event) Payload {
	payload := Payload{APIs: map[string]*APIAggregate{}}
	for _, event := range events {
		payload.TotalRequests++
		if event.Failed {
			payload.FailureCount++
		} else {
			payload.SuccessCount++
		}
		payload.TotalTokens += event.TotalTokens

		endpoint := strings.TrimSpace(event.Endpoint)
		if endpoint == "" {
			endpoint = "-"
		}
		apiEntry := payload.APIs[endpoint]
		if apiEntry == nil {
			apiEntry = &APIAggregate{Models: map[string]*ModelAggregate{}}
			payload.APIs[endpoint] = apiEntry
		}
		model := strings.TrimSpace(event.Model)
		if model == "" {
			model = "-"
		}
		modelEntry := apiEntry.Models[model]
		if modelEntry == nil {
			modelEntry = &ModelAggregate{}
			apiEntry.Models[model] = modelEntry
		}
		modelEntry.Details = append(modelEntry.Details, Detail{
			Timestamp: event.Timestamp,
			Source:    event.Source,
			AuthIndex: event.AuthIndex,
			LatencyMS: event.LatencyMS,
			Failed:    event.Failed,
			Tokens: Tokens{
				InputTokens:     event.InputTokens,
				OutputTokens:    event.OutputTokens,
				ReasoningTokens: event.ReasoningTokens,
				CachedTokens:    event.CachedTokens,
				CacheTokens:     event.CacheTokens,
				TotalTokens:     event.TotalTokens,
			},
		})
	}
	return payload
}

func ExportJSONL(ctx context.Context) ([]byte, error) {
	store := DefaultStore()
	if store == nil {
		return nil, errors.New("usage persistence is not initialized")
	}
	return store.ExportJSONL(ctx)
}

func (s *Store) ExportJSONL(ctx context.Context) ([]byte, error) {
	events, err := s.RecentEvents(ctx, int(^uint(0)>>1))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for i := len(events) - 1; i >= 0; i-- {
		if err := encoder.Encode(events[i]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func Import(ctx context.Context, data []byte) (ImportResult, error) {
	store := DefaultStore()
	if store == nil {
		return ImportResult{}, errors.New("usage persistence is not initialized")
	}
	events, result, err := parseImportPayload(data)
	if err != nil {
		return result, err
	}
	inserted, err := store.InsertEvents(ctx, events)
	if err != nil {
		return result, err
	}
	result.Added += inserted.Added
	result.Skipped += inserted.Skipped
	return result, nil
}

func parseImportPayload(data []byte) ([]Event, ImportResult, error) {
	result := ImportResult{}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, result, errors.New("empty import payload")
	}
	if trimmed[0] == '{' && json.Valid(trimmed) {
		result.Total = 1
		event, err := parseImportEvent(trimmed)
		if err != nil {
			appendImportError(&result, err)
			return nil, result, err
		}
		return []Event{event}, result, nil
	}
	if trimmed[0] == '[' {
		var rawEvents []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawEvents); err != nil {
			result.Failed = 1
			return nil, result, err
		}
		result.Total = len(rawEvents)
		events := make([]Event, 0, len(rawEvents))
		for _, raw := range rawEvents {
			event, err := parseImportEvent(raw)
			if err != nil {
				appendImportError(&result, err)
				continue
			}
			events = append(events, event)
		}
		return events, result, importParseErrorIfEmpty(events, result)
	}

	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 1024), 16*1024*1024)
	events := make([]Event, 0)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		result.Total++
		event, err := parseImportEvent(line)
		if err != nil {
			appendImportError(&result, err)
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return events, result, err
	}
	return events, result, importParseErrorIfEmpty(events, result)
}

func parseImportEvent(raw []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, err
	}
	if !looksLikeImportEvent(event) {
		return Event{}, errUnsupportedImportEvent
	}
	return normalizeEvent(event), nil
}

func looksLikeImportEvent(event Event) bool {
	return strings.TrimSpace(event.RequestID) != "" ||
		strings.TrimSpace(event.Timestamp) != "" ||
		event.TimestampMS != 0 ||
		strings.TrimSpace(event.Model) != "" ||
		strings.TrimSpace(event.Endpoint) != "" ||
		event.InputTokens != 0 ||
		event.OutputTokens != 0 ||
		event.ReasoningTokens != 0 ||
		event.CachedTokens != 0 ||
		event.CacheTokens != 0 ||
		event.TotalTokens != 0 ||
		strings.TrimSpace(event.RawJSON) != ""
}

func appendImportError(result *ImportResult, err error) {
	if errors.Is(err, errUnsupportedImportEvent) {
		result.Unsupported++
	} else {
		result.Failed++
	}
	result.Warnings = append(result.Warnings, err.Error())
}

func importParseErrorIfEmpty(events []Event, result ImportResult) error {
	if len(events) != 0 {
		return nil
	}
	if result.Unsupported != 0 && result.Failed == 0 {
		return errUnsupportedImportEvent
	}
	return nil
}

func normalizeEvent(event Event) Event {
	if event.TimestampMS == 0 {
		if event.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
				event.TimestampMS = parsed.UnixMilli()
			}
		}
		if event.TimestampMS == 0 {
			event.TimestampMS = time.Now().UnixMilli()
		}
	}
	if event.Timestamp == "" {
		event.Timestamp = time.UnixMilli(event.TimestampMS).UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(event.Model) == "" {
		event.Model = "-"
	}
	if strings.TrimSpace(event.Endpoint) == "" {
		event.Endpoint = "-"
	}
	if event.Method == "" || event.Path == "" {
		event.Method, event.Path = splitEndpoint(event.Endpoint)
	}
	if event.TotalTokens == 0 {
		event.TotalTokens = event.InputTokens + event.OutputTokens + event.ReasoningTokens
	}
	if event.CacheTokens == 0 {
		event.CacheTokens = event.CachedTokens
	}
	if event.TotalTokens == 0 {
		event.TotalTokens = event.InputTokens + event.OutputTokens + event.ReasoningTokens + maxInt64(event.CachedTokens, event.CacheTokens)
	}
	if event.CreatedAtMS == 0 {
		event.CreatedAtMS = time.Now().UnixMilli()
	}
	if event.SourceHash == "" {
		event.SourceHash = hashString(event.Source)
	}
	if event.EventHash == "" {
		event.EventHash = buildEventHash(event)
	}
	return event
}

func normalizeCooldownState(state CooldownState) CooldownState {
	state.AuthID = strings.TrimSpace(state.AuthID)
	state.AuthIndex = strings.TrimSpace(state.AuthIndex)
	state.Provider = strings.TrimSpace(state.Provider)
	state.Model = strings.TrimSpace(state.Model)
	state.Reason = strings.TrimSpace(state.Reason)
	state.StatusMessage = strings.TrimSpace(state.StatusMessage)
	if state.NextRetryAfter.IsZero() {
		return state
	}
	state.NextRetryAfter = state.NextRetryAfter.UTC()
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	} else {
		state.UpdatedAt = state.UpdatedAt.UTC()
	}
	if state.HTTPStatus < 0 {
		state.HTTPStatus = 0
	}
	if state.BackoffLevel < 0 {
		state.BackoffLevel = 0
	}
	return state
}

func normalizeTokens(detail coreusage.Detail) Tokens {
	tokens := Tokens{
		InputTokens:     detail.InputTokens,
		OutputTokens:    detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens,
		CachedTokens:    detail.CachedTokens,
		CacheTokens:     detail.CachedTokens,
		TotalTokens:     detail.TotalTokens,
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func responseSucceeded(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	if status == 0 {
		return true
	}
	return status < 400
}

func splitEndpoint(endpoint string) (string, string) {
	parts := strings.Fields(endpoint)
	if len(parts) < 2 {
		return "", ""
	}
	method := strings.ToUpper(parts[0])
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD":
		return method, parts[1]
	default:
		return "", ""
	}
}

func rawEventJSON(event Event, alias string) string {
	payload := map[string]any{
		"request_id":       event.RequestID,
		"timestamp":        event.Timestamp,
		"provider":         event.Provider,
		"model":            event.Model,
		"alias":            strings.TrimSpace(alias),
		"endpoint":         event.Endpoint,
		"method":           event.Method,
		"path":             event.Path,
		"auth_type":        event.AuthType,
		"auth_index":       event.AuthIndex,
		"source":           event.Source,
		"source_hash":      event.SourceHash,
		"api_key_hash":     event.APIKeyHash,
		"input_tokens":     event.InputTokens,
		"output_tokens":    event.OutputTokens,
		"reasoning_tokens": event.ReasoningTokens,
		"cached_tokens":    event.CachedTokens,
		"cache_tokens":     event.CacheTokens,
		"total_tokens":     event.TotalTokens,
		"latency_ms":       event.LatencyMS,
		"failed":           event.Failed,
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func buildEventHash(event Event) string {
	parts := []string{
		event.RequestID,
		event.Timestamp,
		event.Endpoint,
		event.Model,
		event.AuthIndex,
		event.SourceHash,
		strconv.FormatInt(event.InputTokens, 10),
		strconv.FormatInt(event.OutputTokens, 10),
		strconv.FormatInt(event.ReasoningTokens, 10),
		strconv.FormatInt(maxInt64(event.CachedTokens, event.CacheTokens), 10),
		strconv.FormatBool(event.Failed),
	}
	if event.LatencyMS != nil {
		parts = append(parts, strconv.FormatInt(*event.LatencyMS, 10))
	}
	return hashString(strings.Join(parts, "|"))
}

func maskSource(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "@") {
		parts := strings.SplitN(trimmed, "@", 2)
		prefix := parts[0]
		if len(prefix) > 3 {
			prefix = prefix[:3]
		}
		return prefix + "***@" + parts[1]
	}
	if looksSecret(trimmed) {
		if len(trimmed) <= 8 {
			return "m:****"
		}
		return "m:" + trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
	}
	return trimmed
}

func looksSecret(value string) bool {
	if strings.ContainsAny(value, " /\\") {
		return false
	}
	return strings.HasPrefix(value, "sk-") || strings.HasPrefix(value, "AIza") || len(value) >= 32
}

func hashString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (r ImportResult) Error() string {
	return fmt.Sprintf("added=%d skipped=%d failed=%d unsupported=%d", r.Added, r.Skipped, r.Failed, r.Unsupported)
}
