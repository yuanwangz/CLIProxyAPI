package imagesfallback

import (
	"fmt"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

	if ShouldUseCodexOAuthFallback(http.StatusInternalServerError, fmt.Errorf("Tool choice 'image_generation' not found in 'tools'"), auth) {
		t.Fatalf("expected non-400 errors to skip fallback")
	}
}
