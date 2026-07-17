package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const metadataConsecutiveStatusFailuresKey = "consecutive_status_failures"

// ConsecutiveStatusDisablePolicy describes when repeated HTTP status failures
// permanently disable a credential. Policies are evaluated in order; the first
// match for (provider, statusCode) wins.
//
// Future providers/status codes can be added here without changing MarkResult.
type ConsecutiveStatusDisablePolicy struct {
	StatusCode int
	Threshold  int
	// MatchProvider returns true when the policy applies to the auth provider.
	// nil means the policy applies to every provider.
	MatchProvider func(provider string) bool
}

// consecutiveStatusDisablePolicies is the durable consecutive-failure disable table.
// v1: only xAI (Grok) credentials disable after 3 consecutive 403 responses.
var consecutiveStatusDisablePolicies = []ConsecutiveStatusDisablePolicy{
	{
		StatusCode: http.StatusForbidden,
		Threshold:  3,
		MatchProvider: func(provider string) bool {
			return strings.EqualFold(strings.TrimSpace(provider), "xai")
		},
	},
}

func findConsecutiveStatusDisablePolicy(provider string, statusCode int) *ConsecutiveStatusDisablePolicy {
	if statusCode <= 0 {
		return nil
	}
	for i := range consecutiveStatusDisablePolicies {
		policy := &consecutiveStatusDisablePolicies[i]
		if policy.StatusCode != statusCode || policy.Threshold <= 0 {
			continue
		}
		if policy.MatchProvider != nil && !policy.MatchProvider(provider) {
			continue
		}
		return policy
	}
	return nil
}

// ClearConsecutiveStatusFailures resets in-memory consecutive failure progress.
// Returns true when any progress was present (caller may rely on persist+Sync to
// drop the durable metadata key).
func ClearConsecutiveStatusFailures(auth *Auth) bool {
	if auth == nil || len(auth.consecutiveStatusFailures) == 0 {
		return false
	}
	auth.consecutiveStatusFailures = nil
	return true
}

// consecutiveStatusFailureCount returns the current in-memory count for statusCode.
func consecutiveStatusFailureCount(auth *Auth, statusCode int) int {
	if auth == nil || statusCode <= 0 || len(auth.consecutiveStatusFailures) == 0 {
		return 0
	}
	return auth.consecutiveStatusFailures[statusCode]
}

// recordConsecutiveStatusFailure increments the consecutive counter for statusCode
// after clearing counters for any other status. Returns the new count.
func recordConsecutiveStatusFailure(auth *Auth, statusCode int) int {
	if auth == nil || statusCode <= 0 {
		return 0
	}
	if auth.consecutiveStatusFailures == nil {
		auth.consecutiveStatusFailures = make(map[int]int, 1)
	}
	for code := range auth.consecutiveStatusFailures {
		if code != statusCode {
			delete(auth.consecutiveStatusFailures, code)
		}
	}
	auth.consecutiveStatusFailures[statusCode]++
	return auth.consecutiveStatusFailures[statusCode]
}

// tryApplyConsecutiveStatusDisable records a policy-matching failure and disables
// the auth when the threshold is reached. Returns true when the auth was disabled.
func tryApplyConsecutiveStatusDisable(auth *Auth, resultErr *Error, now time.Time) bool {
	if auth == nil {
		return false
	}
	statusCode := statusCodeFromResult(resultErr)
	policy := findConsecutiveStatusDisablePolicy(auth.Provider, statusCode)
	if policy == nil {
		return false
	}
	count := recordConsecutiveStatusFailure(auth, statusCode)
	if count < policy.Threshold {
		return false
	}
	applyForbiddenDisabledState(auth, resultErr, now)
	// Disabled + last_error is the durable outcome; drop the progress counter.
	_ = ClearConsecutiveStatusFailures(auth)
	return true
}

// applyForbiddenDisabledState mirrors applyUnauthorizedDisabledState for HTTP 403.
func applyForbiddenDisabledState(auth *Auth, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = false
	auth.Status = StatusDisabled
	auth.StatusMessage = "forbidden"
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now

	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
	} else {
		auth.LastError = &Error{}
	}
	if auth.LastError.HTTPStatus == 0 {
		auth.LastError.HTTPStatus = http.StatusForbidden
	}
	if strings.TrimSpace(auth.LastError.Code) == "" {
		auth.LastError.Code = "forbidden"
	}
	if strings.TrimSpace(auth.LastError.Message) == "" {
		auth.LastError.Message = "forbidden"
	}
	auth.LastError.Retryable = false
}

func syncConsecutiveStatusFailuresToMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		if len(auth.consecutiveStatusFailures) == 0 {
			return
		}
		auth.Metadata = make(map[string]any)
	}
	if len(auth.consecutiveStatusFailures) == 0 {
		delete(auth.Metadata, metadataConsecutiveStatusFailuresKey)
		return
	}
	serialized := make(map[string]any, len(auth.consecutiveStatusFailures))
	for code, count := range auth.consecutiveStatusFailures {
		if code <= 0 || count <= 0 {
			continue
		}
		serialized[strconv.Itoa(code)] = count
	}
	if len(serialized) == 0 {
		delete(auth.Metadata, metadataConsecutiveStatusFailuresKey)
		return
	}
	auth.Metadata[metadataConsecutiveStatusFailuresKey] = serialized
}

func restoreConsecutiveStatusFailuresFromMetadata(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	raw, ok := auth.Metadata[metadataConsecutiveStatusFailuresKey]
	if !ok || raw == nil {
		auth.consecutiveStatusFailures = nil
		return
	}
	parsed := consecutiveStatusFailuresFromMetadataValue(raw)
	if len(parsed) == 0 {
		auth.consecutiveStatusFailures = nil
		delete(auth.Metadata, metadataConsecutiveStatusFailuresKey)
		return
	}
	auth.consecutiveStatusFailures = parsed
}

func consecutiveStatusFailuresFromMetadataValue(value any) map[int]int {
	switch v := value.(type) {
	case map[int]int:
		if len(v) == 0 {
			return nil
		}
		out := make(map[int]int, len(v))
		for code, count := range v {
			if code > 0 && count > 0 {
				out[code] = count
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case map[string]any:
		return consecutiveStatusFailuresFromStringAnyMap(v)
	case map[string]int:
		out := make(map[int]int, len(v))
		for key, count := range v {
			code, err := strconv.Atoi(strings.TrimSpace(key))
			if err != nil || code <= 0 || count <= 0 {
				continue
			}
			out[code] = count
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func consecutiveStatusFailuresFromStringAnyMap(values map[string]any) map[int]int {
	if len(values) == 0 {
		return nil
	}
	out := make(map[int]int, len(values))
	for key, raw := range values {
		code, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil || code <= 0 {
			continue
		}
		count := firstPositiveInt(raw)
		if count <= 0 {
			continue
		}
		out[code] = count
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneConsecutiveStatusFailures(src map[int]int) map[int]int {
	if len(src) == 0 {
		return nil
	}
	out := make(map[int]int, len(src))
	for code, count := range src {
		out[code] = count
	}
	return out
}
