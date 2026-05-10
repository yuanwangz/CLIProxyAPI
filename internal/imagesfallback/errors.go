package imagesfallback

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type statusError struct {
	statusCode int
	message    string
	retryAfter *time.Duration
}

func (e *statusError) Error() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.message)
}

func (e *statusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func (e *statusError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter == nil || *e.retryAfter <= 0 {
		return nil
	}
	value := *e.retryAfter
	return &value
}

func newStatusError(statusCode int, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return &statusError{
		statusCode: statusCode,
		message:    message,
	}
}

func newStatusErrorWithRetryAfter(statusCode int, message string, retryAfter *time.Duration) error {
	err := newStatusError(statusCode, message)
	statusErr, ok := err.(*statusError)
	if !ok || retryAfter == nil || *retryAfter <= 0 {
		return err
	}
	value := *retryAfter
	statusErr.retryAfter = &value
	return statusErr
}

func StatusCode(err error) int {
	if err == nil {
		return 0
	}
	var withStatus interface{ StatusCode() int }
	if errors.As(err, &withStatus) {
		return withStatus.StatusCode()
	}
	return 0
}

func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	raw := strings.TrimSpace(err.Error())
	if raw == "" {
		return ""
	}
	if !json.Valid([]byte(raw)) {
		return raw
	}
	for _, path := range []string{"error.message", "message"} {
		if text := strings.TrimSpace(gjson.Get(raw, path).String()); text != "" {
			return text
		}
	}
	return raw
}

func IsMissingImageGenerationToolError(err error) bool {
	text := strings.ToLower(ErrorText(err))
	return strings.Contains(text, "tool choice 'image_generation'") &&
		strings.Contains(text, "not found in 'tools'")
}

func NormalizeExecutionError(err error) error {
	if err == nil {
		return nil
	}
	text := ErrorText(err)
	if IsImageQuotaExceededError(err) {
		retryAfter := ParseImageQuotaReset(text)
		return newStatusErrorWithRetryAfter(http.StatusTooManyRequests, text, retryAfter)
	}
	return err
}

func IsImageQuotaExceededError(err error) bool {
	text := strings.ToLower(ErrorText(err))
	if text == "" {
		return false
	}
	return strings.Contains(text, "free plan limit for image generations") ||
		strings.Contains(text, "free plan limit for image generation") ||
		strings.Contains(text, "image generation limit") ||
		(strings.Contains(text, "image") && strings.Contains(text, "quota")) ||
		(strings.Contains(text, "image") && strings.Contains(text, "rate limit"))
}

func ParseImageQuotaReset(message string) *time.Duration {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" || !strings.Contains(message, "reset") {
		return nil
	}

	var total time.Duration
	patterns := []struct {
		re *regexp.Regexp
		to func(int) time.Duration
	}{
		{regexp.MustCompile(`(\d+)\s*days?`), func(v int) time.Duration { return time.Duration(v) * 24 * time.Hour }},
		{regexp.MustCompile(`(\d+)\s*hours?`), func(v int) time.Duration { return time.Duration(v) * time.Hour }},
		{regexp.MustCompile(`(\d+)\s*minutes?`), func(v int) time.Duration { return time.Duration(v) * time.Minute }},
		{regexp.MustCompile(`(\d+)\s*seconds?`), func(v int) time.Duration { return time.Duration(v) * time.Second }},
	}
	for _, pattern := range patterns {
		match := pattern.re.FindStringSubmatch(message)
		if len(match) != 2 {
			continue
		}
		value, err := strconv.Atoi(match[1])
		if err != nil || value <= 0 {
			continue
		}
		total += pattern.to(value)
	}
	if total <= 0 {
		return nil
	}
	return &total
}

func ShouldUseCodexOAuthFallback(statusCode int, err error, auth *coreauth.Auth) bool {
	if !IsCodexOAuthAuth(auth) {
		return false
	}
	if IsMissingImageGenerationToolError(err) {
		return true
	}

	text := strings.ToLower(ErrorText(err))
	switch statusCode {
	case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return true
	}
	if statusCode >= http.StatusInternalServerError {
		return true
	}
	if strings.Contains(text, "upstream did not return image output") {
		return true
	}
	if strings.Contains(text, "stream disconnected before completion") {
		return true
	}
	return false
}
