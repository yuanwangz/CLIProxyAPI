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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	defaultQueryLimit              = 50000
	queueSize                      = 2048
	cooldownQueueSize              = 1024
	quotaSnapshotQueueSize         = 1024
	apiKeyUsageBucketSeconds int64 = 10 * 60
	apiKeyUsageBucketCount         = 20
	maxFailureBodyBytes            = 16 * 1024
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
	SourceFull      string `json:"source_full,omitempty"`
	SourceHash      string `json:"source_hash,omitempty"`
	APIKey          string `json:"api_key,omitempty"`
	APIKeyHash      string `json:"api_key_hash,omitempty"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	CacheTokens     int64  `json:"cache_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	LatencyMS       *int64 `json:"latency_ms,omitempty"`
	Failed          bool   `json:"failed"`
	StatusCode      int    `json:"status_code,omitempty"`
	Error           string `json:"error,omitempty"`
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
	Timestamp  string `json:"timestamp"`
	Source     string `json:"source"`
	SourceFull string `json:"source_full,omitempty"`
	SourceHash string `json:"source_hash,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	APIKeyHash string `json:"api_key_hash,omitempty"`
	AuthIndex  string `json:"auth_index,omitempty"`
	LatencyMS  *int64 `json:"latency_ms,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
	Tokens     Tokens `json:"tokens"`
	Failed     bool   `json:"failed"`
}

type QuotaSnapshot struct {
	Provider    string
	AuthID      string
	AuthIndex   string
	FileName    string
	QuotaJSON   string
	RefreshedAt time.Time
	UpdatedAt   time.Time
}

type CredentialTokenUsage struct {
	AuthIndex       string `json:"auth_index"`
	RequestCount    int64  `json:"request_count"`
	SuccessCount    int64  `json:"success_count"`
	FailureCount    int64  `json:"failure_count"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	CacheTokens     int64  `json:"cache_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	LastUsedAt      string `json:"last_used_at,omitempty"`
	LastUsedAtMS    int64  `json:"last_used_at_ms,omitempty"`
	CycleStartAt    string `json:"cycle_start_at,omitempty"`
	CycleStartAtMS  int64  `json:"cycle_start_at_ms,omitempty"`
}

type RecentRequestBucket struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type APIKeyUsageStats struct {
	AuthIndex      string                `json:"auth_index"`
	Success        int64                 `json:"success"`
	Failed         int64                 `json:"failed"`
	RecentRequests []RecentRequestBucket `json:"recent_requests"`
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
	quotaOnce    sync.Once
	quotaCh      chan QuotaSnapshot

	errUnsupportedImportEvent = errors.New("unsupported usage import event")
)

type cooldownCommand struct {
	action string
	state  CooldownState
	authID string
	model  string
	done   chan error
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
	quotaOnce.Do(func() {
		quotaCh = make(chan QuotaSnapshot, quotaSnapshotQueueSize)
		go runQuotaSnapshots()
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
			source_full text,
			source_hash text,
			api_key text,
			api_key_hash text,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			reasoning_tokens integer not null default 0,
			cached_tokens integer not null default 0,
			cache_tokens integer not null default 0,
			total_tokens integer not null default 0,
			latency_ms integer,
			failed integer not null default 0,
			status_code integer not null default 0,
			raw_json text,
			created_at_ms integer not null
		)`,
		`create index if not exists idx_usage_events_timestamp on usage_events(timestamp_ms)`,
		`create index if not exists idx_usage_events_request_id on usage_events(request_id)`,
		`create index if not exists idx_usage_events_model on usage_events(model)`,
		`create index if not exists idx_usage_events_auth_index on usage_events(auth_index)`,
		`create index if not exists idx_usage_events_endpoint on usage_events(endpoint)`,
		`create index if not exists idx_usage_events_api_key_hash on usage_events(api_key_hash)`,
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
		`create table if not exists quota_snapshots (
			provider text not null,
			auth_index text not null,
			auth_id text,
			file_name text,
			quota_json text not null,
			refreshed_at_ms integer not null,
			updated_at_ms integer not null,
			primary key(provider, auth_index)
		)`,
		`create index if not exists idx_quota_snapshots_auth_id on quota_snapshots(auth_id)`,
		`create index if not exists idx_quota_snapshots_updated_at on quota_snapshots(updated_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	if err := s.ensureUsageEventColumns(); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureUsageEventColumns() error {
	columns := map[string]string{
		"source_full": `alter table usage_events add column source_full text`,
		"api_key":     `alter table usage_events add column api_key text`,
		"status_code": `alter table usage_events add column status_code integer not null default 0`,
	}
	rows, err := s.db.Query(`pragma table_info(usage_events)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	existing := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, statement := range columns {
		if _, ok := existing[name]; ok {
			continue
		}
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
		var err error
		if store != nil {
			ctx := context.Background()
			switch command.action {
			case "upsert":
				err = store.UpsertCooldown(ctx, command.state)
			case "delete":
				err = store.DeleteCooldown(ctx, command.authID, command.model)
			case "delete-auth":
				err = store.DeleteAuthCooldowns(ctx, command.authID)
			case "delete-auth-quota":
				err = store.DeleteAuthQuotaCooldowns(ctx, command.authID)
			}
		}
		if err != nil {
			log.WithError(err).Warn("failed to update persisted auth cooldown")
		}
		if command.done != nil {
			command.done <- err
			close(command.done)
		}
	}
}

func runQuotaSnapshots() {
	for snapshot := range quotaCh {
		store := DefaultStore()
		if store == nil {
			continue
		}
		if _, err := store.UpsertQuotaSnapshot(context.Background(), snapshot); err != nil {
			log.WithError(err).Warn("failed to persist quota snapshot")
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

func ClearCooldown(ctx context.Context, authID, model string) error {
	return enqueueCooldownCommandAndWait(ctx, cooldownCommand{
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

func ClearAuthQuotaCooldownsAsync(authID string) {
	enqueueCooldownCommand(cooldownCommand{
		action: "delete-auth-quota",
		authID: strings.TrimSpace(authID),
	})
}

func ClearAuthQuotaCooldowns(ctx context.Context, authID string) error {
	return enqueueCooldownCommandAndWait(ctx, cooldownCommand{
		action: "delete-auth-quota",
		authID: strings.TrimSpace(authID),
	})
}

func enqueueCooldownCommandAndWait(ctx context.Context, command cooldownCommand) error {
	if cooldownCh == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	command.done = make(chan error, 1)
	select {
	case cooldownCh <- command:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-command.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
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

func UpsertQuotaSnapshotAsync(snapshot QuotaSnapshot) {
	if quotaCh == nil {
		return
	}
	select {
	case quotaCh <- snapshot:
	default:
		log.Warn("quota snapshot persistence queue is full; dropping quota snapshot")
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
	statusCode := usageStatusCode(ctx, record)
	if !failed {
		failed = !responseSucceeded(statusCode)
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
		SourceFull:      displaySourceFull(sourceRaw),
		SourceHash:      hashString(sourceRaw),
		APIKey:          maskAPIKey(apiKey),
		APIKeyHash:      hashString(apiKey),
		InputTokens:     tokens.InputTokens,
		OutputTokens:    tokens.OutputTokens,
		ReasoningTokens: tokens.ReasoningTokens,
		CachedTokens:    tokens.CachedTokens,
		CacheTokens:     tokens.CacheTokens,
		TotalTokens:     tokens.TotalTokens,
		LatencyMS:       latencyPtr,
		Failed:          failed,
		StatusCode:      statusCode,
		Error:           failureBody(record.Fail.Body, failed),
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
		auth_type, auth_index, source, source_full, source_hash, api_key, api_key_hash, input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, cache_tokens, total_tokens, latency_ms, failed, status_code, raw_json,
		created_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			event.SourceFull,
			event.SourceHash,
			event.APIKey,
			event.APIKeyHash,
			event.InputTokens,
			event.OutputTokens,
			event.ReasoningTokens,
			event.CachedTokens,
			event.CacheTokens,
			event.TotalTokens,
			nullInt64(event.LatencyMS),
			boolToInt(event.Failed),
			event.StatusCode,
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
		coalesce(request_id, ''), event_hash, timestamp_ms, timestamp, coalesce(provider, ''), model,
		coalesce(endpoint, ''), coalesce(method, ''), coalesce(path, ''),
		coalesce(auth_type, ''), coalesce(auth_index, ''), coalesce(source, ''), coalesce(source_full, ''),
		coalesce(source_hash, ''), coalesce(api_key, ''), coalesce(api_key_hash, ''), input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, cache_tokens, total_tokens, latency_ms, failed, status_code, coalesce(raw_json, ''),
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
			&event.SourceFull,
			&event.SourceHash,
			&event.APIKey,
			&event.APIKeyHash,
			&event.InputTokens,
			&event.OutputTokens,
			&event.ReasoningTokens,
			&event.CachedTokens,
			&event.CacheTokens,
			&event.TotalTokens,
			&latency,
			&failed,
			&event.StatusCode,
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
		event.Error = failureBodyFromRawJSON(event.RawJSON)
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

func (s *Store) DeleteAuthQuotaCooldowns(ctx context.Context, authID string) error {
	if s == nil || s.db == nil {
		return errors.New("usage store is closed")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `delete from auth_cooldowns
		where auth_id = ? and (quota_exceeded != 0 or http_status = ?)`, authID, http.StatusTooManyRequests)
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

func UpsertQuotaSnapshot(ctx context.Context, snapshot QuotaSnapshot) (QuotaSnapshot, error) {
	store := DefaultStore()
	if store == nil {
		return QuotaSnapshot{}, errors.New("usage persistence is not initialized")
	}
	return store.UpsertQuotaSnapshot(ctx, snapshot)
}

func QuotaSnapshots(ctx context.Context) ([]QuotaSnapshot, error) {
	store := DefaultStore()
	if store == nil {
		return nil, nil
	}
	return store.QuotaSnapshots(ctx)
}

func CredentialTokenUsages(ctx context.Context) ([]CredentialTokenUsage, error) {
	store := DefaultStore()
	if store == nil {
		return nil, nil
	}
	return store.CredentialTokenUsages(ctx)
}

func CredentialTokenUsagesForQuotaSnapshots(ctx context.Context, snapshots []QuotaSnapshot) ([]CredentialTokenUsage, error) {
	store := DefaultStore()
	if store == nil {
		return nil, nil
	}
	return store.CredentialTokenUsagesForQuotaSnapshots(ctx, snapshots)
}

func (s *Store) UpsertQuotaSnapshot(ctx context.Context, snapshot QuotaSnapshot) (QuotaSnapshot, error) {
	if s == nil || s.db == nil {
		return QuotaSnapshot{}, errors.New("usage store is closed")
	}
	snapshot = normalizeQuotaSnapshot(snapshot)
	if snapshot.Provider == "" {
		return QuotaSnapshot{}, errors.New("quota snapshot provider is empty")
	}
	if snapshot.AuthIndex == "" {
		return QuotaSnapshot{}, errors.New("quota snapshot auth_index is empty")
	}
	if snapshot.QuotaJSON == "" || !json.Valid([]byte(snapshot.QuotaJSON)) {
		return QuotaSnapshot{}, errors.New("quota snapshot quota_json is invalid")
	}
	_, err := s.db.ExecContext(ctx, `insert into quota_snapshots (
		provider, auth_index, auth_id, file_name, quota_json, refreshed_at_ms, updated_at_ms
	) values (?, ?, ?, ?, ?, ?, ?)
	on conflict(provider, auth_index) do update set
		auth_id = excluded.auth_id,
		file_name = excluded.file_name,
		quota_json = excluded.quota_json,
		refreshed_at_ms = excluded.refreshed_at_ms,
		updated_at_ms = excluded.updated_at_ms`,
		snapshot.Provider,
		snapshot.AuthIndex,
		snapshot.AuthID,
		snapshot.FileName,
		snapshot.QuotaJSON,
		snapshot.RefreshedAt.UnixMilli(),
		snapshot.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) QuotaSnapshots(ctx context.Context) ([]QuotaSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `select
		provider, auth_index, coalesce(auth_id, ''), coalesce(file_name, ''),
		quota_json, refreshed_at_ms, updated_at_ms
		from quota_snapshots
		order by provider, file_name, auth_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]QuotaSnapshot, 0)
	for rows.Next() {
		var snapshot QuotaSnapshot
		var refreshedAtMS int64
		var updatedAtMS int64
		if err := rows.Scan(
			&snapshot.Provider,
			&snapshot.AuthIndex,
			&snapshot.AuthID,
			&snapshot.FileName,
			&snapshot.QuotaJSON,
			&refreshedAtMS,
			&updatedAtMS,
		); err != nil {
			return nil, err
		}
		snapshot.RefreshedAt = time.UnixMilli(refreshedAtMS).UTC()
		snapshot.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
		out = append(out, normalizeQuotaSnapshot(snapshot))
	}
	return out, rows.Err()
}

func (s *Store) CredentialTokenUsages(ctx context.Context) ([]CredentialTokenUsage, error) {
	return s.credentialTokenUsages(ctx, nil)
}

func APIKeyUsageStatsByAuthIndex(ctx context.Context, authIndexes []string, now time.Time) (map[string]APIKeyUsageStats, error) {
	store := DefaultStore()
	if store == nil {
		return map[string]APIKeyUsageStats{}, nil
	}
	return store.APIKeyUsageStatsByAuthIndex(ctx, authIndexes, now)
}

func (s *Store) APIKeyUsageStatsByAuthIndex(ctx context.Context, authIndexes []string, now time.Time) (map[string]APIKeyUsageStats, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage store is closed")
	}
	authIndexes = normalizeAuthIndexList(authIndexes)
	out := make(map[string]APIKeyUsageStats, len(authIndexes))
	if len(authIndexes) == 0 {
		return out, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	currentBucketID := now.Unix() / apiKeyUsageBucketSeconds

	placeholders := queryPlaceholders(len(authIndexes))
	args := make([]any, 0, len(authIndexes))
	for _, authIndex := range authIndexes {
		args = append(args, authIndex)
	}

	rows, err := s.db.QueryContext(ctx, `select
		auth_index,
		sum(case when failed = 0 then 1 else 0 end),
		sum(case when failed != 0 then 1 else 0 end)
		from usage_events
		where auth_index in (`+placeholders+`)
		group by auth_index`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var authIndex string
		var success int64
		var failed int64
		if errScan := rows.Scan(&authIndex, &success, &failed); errScan != nil {
			_ = rows.Close()
			return nil, errScan
		}
		authIndex = strings.TrimSpace(authIndex)
		out[authIndex] = APIKeyUsageStats{
			AuthIndex:      authIndex,
			Success:        success,
			Failed:         failed,
			RecentRequests: emptyAPIKeyUsageBuckets(currentBucketID),
		}
	}
	if errRows := rows.Close(); errRows != nil {
		return nil, errRows
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}

	bucketDurationMS := apiKeyUsageBucketSeconds * 1000
	startBucketID := currentBucketID - int64(apiKeyUsageBucketCount) + 1
	recentArgs := make([]any, 0, len(authIndexes)+4)
	recentArgs = append(recentArgs, bucketDurationMS, startBucketID*bucketDurationMS, (currentBucketID+1)*bucketDurationMS)
	for _, authIndex := range authIndexes {
		recentArgs = append(recentArgs, authIndex)
	}
	recentArgs = append(recentArgs, bucketDurationMS)
	rows, err = s.db.QueryContext(ctx, `select
		auth_index,
		cast(timestamp_ms / ? as integer),
		sum(case when failed = 0 then 1 else 0 end),
		sum(case when failed != 0 then 1 else 0 end)
		from usage_events
		where timestamp_ms >= ? and timestamp_ms < ? and auth_index in (`+placeholders+`)
		group by auth_index, cast(timestamp_ms / ? as integer)`, recentArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var authIndex string
		var bucketID int64
		var success int64
		var failed int64
		if errScan := rows.Scan(&authIndex, &bucketID, &success, &failed); errScan != nil {
			_ = rows.Close()
			return nil, errScan
		}
		authIndex = strings.TrimSpace(authIndex)
		stats, ok := out[authIndex]
		if !ok {
			continue
		}
		offset := int(bucketID - startBucketID)
		if offset < 0 || offset >= len(stats.RecentRequests) {
			continue
		}
		stats.RecentRequests[offset].Success = success
		stats.RecentRequests[offset].Failed = failed
		out[authIndex] = stats
	}
	if errRows := rows.Close(); errRows != nil {
		return nil, errRows
	}
	return out, rows.Err()
}

func normalizeAuthIndexList(authIndexes []string) []string {
	out := make([]string, 0, len(authIndexes))
	seen := make(map[string]struct{}, len(authIndexes))
	for _, authIndex := range authIndexes {
		authIndex = strings.TrimSpace(authIndex)
		if authIndex == "" {
			continue
		}
		if _, ok := seen[authIndex]; ok {
			continue
		}
		seen[authIndex] = struct{}{}
		out = append(out, authIndex)
	}
	return out
}

func queryPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	parts := make([]string, count)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func emptyAPIKeyUsageBuckets(currentBucketID int64) []RecentRequestBucket {
	out := make([]RecentRequestBucket, 0, apiKeyUsageBucketCount)
	for i := apiKeyUsageBucketCount - 1; i >= 0; i-- {
		bucketID := currentBucketID - int64(i)
		out = append(out, RecentRequestBucket{
			Time: formatAPIKeyUsageBucketLabel(bucketID),
		})
	}
	return out
}

func formatAPIKeyUsageBucketLabel(bucketID int64) string {
	start := time.Unix(bucketID*apiKeyUsageBucketSeconds, 0).In(time.Local)
	end := start.Add(time.Duration(apiKeyUsageBucketSeconds) * time.Second)
	return start.Format("15:04") + "-" + end.Format("15:04")
}

func (s *Store) CredentialTokenUsagesForQuotaSnapshots(ctx context.Context, snapshots []QuotaSnapshot) ([]CredentialTokenUsage, error) {
	cycleStartByAuthIndex := quotaCycleStartsByAuthIndex(snapshots, time.Now().UTC())
	return s.credentialTokenUsages(ctx, cycleStartByAuthIndex)
}

func (s *Store) credentialTokenUsages(ctx context.Context, cycleStartByAuthIndex map[string]int64) ([]CredentialTokenUsage, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage store is closed")
	}

	query := `select
		e.auth_index,
		count(*),
		sum(case when e.failed = 0 then 1 else 0 end),
		sum(case when e.failed != 0 then 1 else 0 end),
		coalesce(sum(e.input_tokens), 0),
		coalesce(sum(e.output_tokens), 0),
		coalesce(sum(e.reasoning_tokens), 0),
		coalesce(sum(e.cached_tokens), 0),
		coalesce(sum(e.cache_tokens), 0),
		coalesce(sum(e.total_tokens), 0),
		coalesce(max(e.timestamp_ms), 0),
		0
		from usage_events e
		where trim(coalesce(e.auth_index, '')) != ''
		group by e.auth_index
		order by e.auth_index`
	args := []any(nil)

	if len(cycleStartByAuthIndex) > 0 {
		values := make([]string, 0, len(cycleStartByAuthIndex))
		args = make([]any, 0, len(cycleStartByAuthIndex)*2)
		for authIndex, cycleStartMS := range cycleStartByAuthIndex {
			authIndex = strings.TrimSpace(authIndex)
			if authIndex == "" || cycleStartMS <= 0 {
				continue
			}
			values = append(values, "(?, ?)")
			args = append(args, authIndex, cycleStartMS)
		}
		if len(values) > 0 {
			query = `with cycle(auth_index, cycle_start_ms) as (values ` + strings.Join(values, ",") + `)
				select
				e.auth_index,
				count(*),
				sum(case when e.failed = 0 then 1 else 0 end),
				sum(case when e.failed != 0 then 1 else 0 end),
				coalesce(sum(e.input_tokens), 0),
				coalesce(sum(e.output_tokens), 0),
				coalesce(sum(e.reasoning_tokens), 0),
				coalesce(sum(e.cached_tokens), 0),
				coalesce(sum(e.cache_tokens), 0),
				coalesce(sum(e.total_tokens), 0),
				coalesce(max(e.timestamp_ms), 0),
				coalesce(max(c.cycle_start_ms), 0)
				from usage_events e
				left join cycle c on e.auth_index = c.auth_index
				where trim(coalesce(e.auth_index, '')) != ''
					and (coalesce(c.cycle_start_ms, 0) = 0 or e.timestamp_ms >= c.cycle_start_ms)
				group by e.auth_index
				order by e.auth_index`
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CredentialTokenUsage, 0)
	for rows.Next() {
		var usage CredentialTokenUsage
		var lastUsedAtMS int64
		var cycleStartAtMS int64
		if err := rows.Scan(
			&usage.AuthIndex,
			&usage.RequestCount,
			&usage.SuccessCount,
			&usage.FailureCount,
			&usage.InputTokens,
			&usage.OutputTokens,
			&usage.ReasoningTokens,
			&usage.CachedTokens,
			&usage.CacheTokens,
			&usage.TotalTokens,
			&lastUsedAtMS,
			&cycleStartAtMS,
		); err != nil {
			return nil, err
		}
		usage.AuthIndex = strings.TrimSpace(usage.AuthIndex)
		if lastUsedAtMS > 0 {
			usage.LastUsedAtMS = lastUsedAtMS
			usage.LastUsedAt = time.UnixMilli(lastUsedAtMS).UTC().Format(time.RFC3339)
		}
		if cycleStartAtMS > 0 {
			usage.CycleStartAtMS = cycleStartAtMS
			usage.CycleStartAt = time.UnixMilli(cycleStartAtMS).UTC().Format(time.RFC3339)
		}
		out = append(out, usage)
	}
	return out, rows.Err()
}

func quotaCycleStartsByAuthIndex(snapshots []QuotaSnapshot, now time.Time) map[string]int64 {
	if len(snapshots) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := make(map[string]int64)
	for _, snapshot := range snapshots {
		snapshot = normalizeQuotaSnapshot(snapshot)
		authIndex := strings.TrimSpace(snapshot.AuthIndex)
		if authIndex == "" {
			continue
		}
		cycleStart := quotaCycleStartMS(snapshot, now)
		if cycleStart <= 0 {
			continue
		}
		if existing := out[authIndex]; existing == 0 || cycleStart > existing {
			out[authIndex] = cycleStart
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func quotaCycleStartMS(snapshot QuotaSnapshot, now time.Time) int64 {
	if snapshot.QuotaJSON == "" {
		return 0
	}
	var root any
	if err := json.Unmarshal([]byte(snapshot.QuotaJSON), &root); err != nil {
		return 0
	}
	candidates := make([]int64, 0, 2)
	collectQuotaCycleStarts(root, snapshot.RefreshedAt, now, &candidates)
	if len(candidates) == 0 {
		return 0
	}
	best := int64(0)
	for _, candidate := range candidates {
		if candidate > best {
			best = candidate
		}
	}
	return best
}

func collectQuotaCycleStarts(value any, observedAt, now time.Time, out *[]int64) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectQuotaCycleStarts(item, observedAt, now, out)
		}
	case map[string]any:
		if startMS := explicitQuotaCycleStartMS(typed); startMS > 0 {
			*out = append(*out, startMS)
		}
		if resetMS := quotaResetMS(typed, observedAt); resetMS > 0 {
			if durationMS := quotaWindowDurationMS(typed); durationMS > 0 {
				nextReset := rollQuotaResetForward(resetMS, durationMS, now)
				if nextReset > durationMS {
					*out = append(*out, nextReset-durationMS)
				}
			}
		}
		for _, child := range typed {
			collectQuotaCycleStarts(child, observedAt, now, out)
		}
	}
}

func explicitQuotaCycleStartMS(record map[string]any) int64 {
	for _, key := range []string{
		"cycleStartAt", "cycle_start_at",
		"periodStart", "period_start",
		"billingPeriodStart", "billing_period_start",
		"startAt", "start_at",
	} {
		if ms := quotaTimeValueMS(record[key]); ms > 0 {
			return ms
		}
	}
	return 0
}

func quotaResetMS(record map[string]any, observedAt time.Time) int64 {
	for _, key := range []string{"resetAt", "reset_at", "resetTime", "reset_time", "resets_at"} {
		if ms := quotaTimeValueMS(record[key]); ms > 0 {
			return ms
		}
	}
	for _, key := range []string{"resetAfterSeconds", "reset_after_seconds", "resetIn", "reset_in", "ttl"} {
		seconds, ok := quotaNumberValue(record[key])
		if !ok || seconds <= 0 {
			continue
		}
		base := observedAt
		if base.IsZero() {
			base = time.Now().UTC()
		}
		return base.UnixMilli() + int64(seconds*1000)
	}
	return 0
}

func quotaWindowDurationMS(record map[string]any) int64 {
	for _, key := range []string{"windowMinutes", "window_minutes"} {
		if minutes, ok := quotaNumberValue(record[key]); ok && minutes > 0 {
			return int64(minutes * 60 * 1000)
		}
	}
	for _, key := range []string{
		"windowSeconds", "window_seconds",
		"limitWindowSeconds", "limit_window_seconds",
		"durationSeconds", "duration_seconds",
	} {
		if seconds, ok := quotaNumberValue(record[key]); ok && seconds > 0 {
			return int64(seconds * 1000)
		}
	}
	return 0
}

func quotaTimeValueMS(value any) int64 {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 {
			return 0
		}
		if typed > 1_000_000_000_000 {
			return int64(typed)
		}
		return int64(typed * 1000)
	case int:
		return quotaTimeValueMS(int64(typed))
	case int64:
		if typed <= 0 {
			return 0
		}
		if typed > 1_000_000_000_000 {
			return typed
		}
		return typed * 1000
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0
		}
		return quotaTimeValueMS(number)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" || trimmed == "-" {
			return 0
		}
		if numeric, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return quotaTimeValueMS(numeric)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
			return parsed.UTC().UnixMilli()
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.UTC().UnixMilli()
		}
	}
	return 0
}

func quotaNumberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func rollQuotaResetForward(resetMS, durationMS int64, now time.Time) int64 {
	if resetMS <= 0 || durationMS <= 0 {
		return 0
	}
	nowMS := now.UnixMilli()
	if resetMS > nowMS {
		return resetMS
	}
	behind := nowMS - resetMS
	steps := behind/durationMS + 1
	return resetMS + steps*durationMS
}

func normalizeQuotaSnapshot(snapshot QuotaSnapshot) QuotaSnapshot {
	now := time.Now().UTC()
	snapshot.Provider = strings.ToLower(strings.TrimSpace(snapshot.Provider))
	snapshot.AuthID = strings.TrimSpace(snapshot.AuthID)
	snapshot.AuthIndex = strings.TrimSpace(snapshot.AuthIndex)
	snapshot.FileName = strings.TrimSpace(snapshot.FileName)
	snapshot.QuotaJSON = strings.TrimSpace(snapshot.QuotaJSON)
	if snapshot.RefreshedAt.IsZero() {
		snapshot.RefreshedAt = now
	} else {
		snapshot.RefreshedAt = snapshot.RefreshedAt.UTC()
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = now
	} else {
		snapshot.UpdatedAt = snapshot.UpdatedAt.UTC()
	}
	return snapshot
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
		errorDetail := event.Error
		if strings.TrimSpace(errorDetail) == "" {
			errorDetail = failureBodyFromRawJSON(event.RawJSON)
		}
		modelEntry.Details = append(modelEntry.Details, Detail{
			Timestamp:  event.Timestamp,
			Source:     event.Source,
			SourceFull: event.SourceFull,
			SourceHash: event.SourceHash,
			APIKey:     event.APIKey,
			APIKeyHash: event.APIKeyHash,
			AuthIndex:  event.AuthIndex,
			LatencyMS:  event.LatencyMS,
			StatusCode: event.StatusCode,
			Error:      errorDetail,
			Failed:     event.Failed,
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
	event.SourceFull = displaySourceFull(event.SourceFull)
	event.APIKey = maskAPIKey(event.APIKey)
	if strings.TrimSpace(event.Source) == "" && event.SourceFull != "" {
		event.Source = maskSource(event.SourceFull)
	}
	if event.SourceHash == "" {
		if event.SourceFull != "" {
			event.SourceHash = hashString(event.SourceFull)
		} else {
			event.SourceHash = hashString(event.Source)
		}
	}
	if event.StatusCode < 0 {
		event.StatusCode = 0
	}
	event.Error = failureBody(event.Error, event.Failed)
	if event.Error == "" && event.RawJSON != "" {
		event.Error = failureBodyFromRawJSON(event.RawJSON)
	}
	if event.RawJSON == "" {
		event.RawJSON = rawEventJSON(event, "")
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

func responseSucceeded(status int) bool {
	if status == 0 {
		return true
	}
	return status < 400
}

func usageStatusCode(ctx context.Context, record coreusage.Record) int {
	responseStatus := internallogging.GetResponseStatus(ctx)
	failureStatus := record.Fail.StatusCode
	if record.Failed && failureStatus > 0 {
		return failureStatus
	}
	if responseStatus > 0 {
		return responseStatus
	}
	if failureStatus > 0 {
		return failureStatus
	}
	if !record.Failed {
		return http.StatusOK
	}
	return 0
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
		"source_full":      event.SourceFull,
		"source_hash":      event.SourceHash,
		"api_key":          event.APIKey,
		"api_key_hash":     event.APIKeyHash,
		"input_tokens":     event.InputTokens,
		"output_tokens":    event.OutputTokens,
		"reasoning_tokens": event.ReasoningTokens,
		"cached_tokens":    event.CachedTokens,
		"cache_tokens":     event.CacheTokens,
		"total_tokens":     event.TotalTokens,
		"latency_ms":       event.LatencyMS,
		"failed":           event.Failed,
		"status_code":      event.StatusCode,
	}
	if strings.TrimSpace(event.Error) != "" {
		payload["error"] = event.Error
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func failureBody(body string, failed bool) string {
	body = strings.TrimSpace(body)
	if body == "" || !failed {
		return ""
	}
	if len(body) <= maxFailureBodyBytes {
		return body
	}
	return body[:maxFailureBodyBytes] + "\n...[truncated]"
}

func failureBodyFromRawJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	for _, key := range []string{"error", "error_detail", "failure_body"} {
		if value := rawTextField(payload[key]); value != "" {
			return failureBody(value, true)
		}
	}
	if failRaw := payload["fail"]; len(failRaw) > 0 {
		var failPayload map[string]json.RawMessage
		if err := json.Unmarshal(failRaw, &failPayload); err == nil {
			if value := rawTextField(failPayload["body"]); value != "" {
				return failureBody(value, true)
			}
		}
	}
	return ""
}

func rawTextField(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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

func displaySourceFull(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || looksSecret(trimmed) {
		return ""
	}
	return trimmed
}

func maskAPIKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "...") || strings.Contains(trimmed, "***") {
		return trimmed
	}
	if len(trimmed) <= 8 {
		return "****"
	}
	return trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}

func looksSecret(value string) bool {
	if strings.Contains(value, "@") {
		return false
	}
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
