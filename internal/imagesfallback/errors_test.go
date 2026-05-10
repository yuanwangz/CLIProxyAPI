package imagesfallback

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestShouldUseCodexOAuthFallback(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "tester@example.com",
		},
	}

	if !ShouldUseCodexOAuthFallback(http.StatusBadRequest, fmt.Errorf(`{"error":{"message":"400 Tool choice 'image_generation' not found in 'tools'"}}`), auth) {
		t.Fatalf("expected fallback trigger for codex oauth missing image tool error")
	}

	if ShouldUseCodexOAuthFallback(http.StatusBadRequest, fmt.Errorf("Tool choice 'image_generation' not found in 'tools'"), &coreauth.Auth{Provider: "codex"}) {
		t.Fatalf("expected api-key style codex auth to skip fallback")
	}

	if !ShouldUseCodexOAuthFallback(http.StatusInternalServerError, fmt.Errorf("upstream did not return image output"), auth) {
		t.Fatalf("expected 5xx upstream image errors to trigger fallback")
	}

	if ShouldUseCodexOAuthFallback(http.StatusBadRequest, fmt.Errorf("unrelated bad request"), auth) {
		t.Fatalf("expected unrelated 400 errors to skip fallback")
	}
}

func TestNormalizeExecutionErrorRecognizesFreePlanImageGenerationRequests(t *testing.T) {
	err := newStatusError(http.StatusUnprocessableEntity, "You've hit the free plan limit for image generation requests. You can create more images when the limit resets in 17 hours and 49 minutes.")

	normalized := NormalizeExecutionError(err)

	if status := StatusCode(normalized); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", status, http.StatusTooManyRequests)
	}
	retryAfterProvider, ok := normalized.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("expected normalized error to expose RetryAfter")
	}
	retryAfter := retryAfterProvider.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected retryAfter")
	}
	want := 17*time.Hour + 49*time.Minute
	if *retryAfter != want {
		t.Fatalf("retryAfter = %v, want %v", *retryAfter, want)
	}
}
