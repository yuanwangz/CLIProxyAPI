package imagesfallback

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type statusError struct {
	statusCode int
	message    string
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
