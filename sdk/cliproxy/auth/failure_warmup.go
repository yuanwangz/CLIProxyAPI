package auth

import (
	"bytes"
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	FailureWarmupEnabledAttribute     = "failure_warmup_enabled"
	FailureWarmupStatusCodesAttribute = "failure_warmup_status_codes"
	FailureWarmupMaxAttemptsAttribute = "failure_warmup_max_attempts"

	failureWarmupRetryInterval = 3 * time.Second
	failureWarmupConcurrency   = 2
)

type failureWarmupSettings struct {
	statusCodes map[int]struct{}
	maxAttempts int
}

type failureWarmupQueue struct {
	manager *Manager

	mu       sync.Mutex
	inFlight map[string]struct{}
	sem      chan struct{}
	interval time.Duration
}

type failureWarmupTask struct {
	key         string
	auth        *Auth
	executor    ProviderExecutor
	provider    string
	model       string
	statusCode  int
	request     cliproxyexecutor.Request
	options     cliproxyexecutor.Options
	stream      bool
	maxAttempts int
}

func newFailureWarmupQueue(manager *Manager) *failureWarmupQueue {
	return &failureWarmupQueue{
		manager:  manager,
		inFlight: make(map[string]struct{}),
		sem:      make(chan struct{}, failureWarmupConcurrency),
		interval: failureWarmupRetryInterval,
	}
}

func (m *Manager) enqueueFailureWarmup(ctx context.Context, auth *Auth, executor ProviderExecutor, provider string, result Result, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) {
	if m == nil || m.failureWarmup == nil || auth == nil || executor == nil || result.Success {
		return
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == 0 {
		return
	}
	settings, ok := failureWarmupSettingsFromAuth(auth)
	if !ok {
		return
	}
	if _, allowed := settings.statusCodes[statusCode]; !allowed {
		return
	}

	task := failureWarmupTask{
		key:         failureWarmupKey(auth),
		auth:        auth.Clone(),
		executor:    executor,
		provider:    strings.ToLower(strings.TrimSpace(provider)),
		model:       strings.TrimSpace(result.Model),
		statusCode:  statusCode,
		request:     cloneFailureWarmupRequest(req),
		options:     cloneFailureWarmupOptions(opts),
		stream:      stream,
		maxAttempts: settings.maxAttempts,
	}
	if task.key == "" || task.maxAttempts <= 0 {
		return
	}
	if m.failureWarmup.enqueue(task) {
		logEntryWithRequestID(ctx).WithFields(log.Fields{
			"auth_id":  auth.ID,
			"provider": task.provider,
			"model":    task.model,
			"status":   statusCode,
			"attempts": task.maxAttempts,
		}).Debug("queued failure warmup retry")
	}
}

func (q *failureWarmupQueue) enqueue(task failureWarmupTask) bool {
	if q == nil || task.key == "" || task.auth == nil || task.executor == nil {
		return false
	}
	q.mu.Lock()
	if _, exists := q.inFlight[task.key]; exists {
		q.mu.Unlock()
		return false
	}
	q.inFlight[task.key] = struct{}{}
	q.mu.Unlock()

	go q.run(task)
	return true
}

func (q *failureWarmupQueue) run(task failureWarmupTask) {
	q.sem <- struct{}{}
	defer func() {
		<-q.sem
		q.mu.Lock()
		delete(q.inFlight, task.key)
		q.mu.Unlock()
	}()

	for attempt := 1; attempt <= task.maxAttempts; attempt++ {
		if attempt > 1 && q.interval > 0 {
			time.Sleep(q.interval)
		}
		err := q.executeAttempt(task)
		if err == nil {
			if q.manager != nil {
				q.manager.ClearRecoverableAvailabilityState(context.Background(), task.auth.ID)
			}
			log.WithFields(log.Fields{
				"auth_id":  task.auth.ID,
				"provider": task.provider,
				"model":    task.model,
				"status":   task.statusCode,
				"attempt":  attempt,
			}).Debug("failure warmup retry succeeded")
			return
		}
		log.WithFields(log.Fields{
			"auth_id":  task.auth.ID,
			"provider": task.provider,
			"model":    task.model,
			"status":   task.statusCode,
			"attempt":  attempt,
		}).Debugf("failure warmup retry failed: %v", err)
	}
}

func (q *failureWarmupQueue) executeAttempt(task failureWarmupTask) error {
	auth := task.auth.Clone()
	req := cloneFailureWarmupRequest(task.request)
	opts := cloneFailureWarmupOptions(task.options)

	ctx := cliproxyusage.WithUsageSuppressed(context.Background())
	if q != nil && q.manager != nil {
		if rt := q.manager.roundTripperFor(auth); rt != nil {
			ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
		}
	}

	if task.stream {
		streamResult, err := task.executor.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			return err
		}
		return drainFailureWarmupStream(streamResult)
	}
	_, err := task.executor.Execute(ctx, auth, req, opts)
	return err
}

func drainFailureWarmupStream(result *cliproxyexecutor.StreamResult) error {
	if result == nil || result.Chunks == nil {
		return nil
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

func failureWarmupKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

func failureWarmupSettingsFromAuth(auth *Auth) (failureWarmupSettings, bool) {
	if auth == nil || len(auth.Attributes) == 0 {
		return failureWarmupSettings{}, false
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(auth.Attributes[FailureWarmupEnabledAttribute]))
	if err != nil || !enabled {
		return failureWarmupSettings{}, false
	}

	statusCodes := parseFailureWarmupStatusCodes(auth.Attributes[FailureWarmupStatusCodesAttribute])
	if len(statusCodes) == 0 {
		statusCodes = internalconfig.DefaultFailureWarmupStatusCodes
	}
	codeSet := make(map[int]struct{}, len(statusCodes))
	for _, code := range statusCodes {
		codeSet[code] = struct{}{}
	}

	maxAttempts := internalconfig.DefaultFailureWarmupMaxAttempts
	if raw := strings.TrimSpace(auth.Attributes[FailureWarmupMaxAttemptsAttribute]); raw != "" {
		if parsed, errParse := strconv.Atoi(raw); errParse == nil && parsed > 0 {
			maxAttempts = parsed
		}
	}
	return failureWarmupSettings{
		statusCodes: codeSet,
		maxAttempts: maxAttempts,
	}, true
}

func parseFailureWarmupStatusCodes(raw string) []int {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	codes := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		code, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || code < 100 || code > 599 {
			continue
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes
}

func cloneFailureWarmupRequest(req cliproxyexecutor.Request) cliproxyexecutor.Request {
	out := req
	out.Payload = bytes.Clone(req.Payload)
	out.Metadata = cloneAnyMap(req.Metadata)
	return out
}

func cloneFailureWarmupOptions(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	out := opts
	out.Headers = cloneHTTPHeader(opts.Headers)
	out.Query = cloneURLValues(opts.Query)
	out.OriginalRequest = bytes.Clone(opts.OriginalRequest)
	out.Metadata = cloneAnyMap(opts.Metadata)
	delete(out.Metadata, cliproxyexecutor.SelectedAuthCallbackMetadataKey)
	out.RequestAfterAuthInterceptor = nil
	return out
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneURLValues(src url.Values) url.Values {
	if src == nil {
		return nil
	}
	out := make(url.Values, len(src))
	for key, values := range src {
		out[key] = append([]string(nil), values...)
	}
	return out
}
