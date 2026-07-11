package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestAPICallUnauthorizedFromUntrustedHostDoesNotDisableAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "api-call-auth",
		FileName: "api-call.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "test-token",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	authIndex := auth.EnsureIndex()

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	payload := map[string]any{
		"auth_index": authIndex,
		"method":     http.MethodGet,
		"url":        upstream.URL,
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
		},
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.APICall(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after api call", auth.ID)
	}
	if updated.Disabled {
		t.Fatal("Disabled = true, want false after local API call 401")
	}
	if updated.Status == coreauth.StatusDisabled {
		t.Fatalf("Status = %q, want non-disabled after local API call 401", updated.Status)
	}
	if updated.LastError != nil {
		t.Fatalf("LastError = %#v, want nil after local API call 401", updated.LastError)
	}
}

func TestAPICallCustomAuthorizationOverridesTokenPlaceholder(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer custom-token" {
			t.Fatalf("Authorization = %q, want Bearer custom-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "api-call-custom-header-auth",
		FileName: "api-call-custom-header.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "default-token",
			"headers": map[string]any{
				"authorization": "Bearer custom-token",
			},
		},
	}
	coreauth.ApplyCustomHeadersFromMetadata(auth)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	payload := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     http.MethodGet,
		"url":        upstream.URL,
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
		},
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.APICall(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestShouldMarkAPICallUnauthorizedRequiresInjectedTokenAndTrustedProviderURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		provider      string
		rawURL        string
		tokenInjected bool
		want          bool
	}{
		{
			name:          "codex chatgpt trusted",
			provider:      "codex",
			rawURL:        "https://chatgpt.com/backend-api/wham/usage",
			tokenInjected: true,
			want:          true,
		},
		{
			name:          "openai api trusted",
			provider:      "openai",
			rawURL:        "https://api.openai.com/v1/responses",
			tokenInjected: true,
			want:          true,
		},
		{
			name:          "codex without injected token",
			provider:      "codex",
			rawURL:        "https://chatgpt.com/backend-api/wham/usage",
			tokenInjected: false,
			want:          false,
		},
		{
			name:          "codex http rejected",
			provider:      "codex",
			rawURL:        "http://chatgpt.com/backend-api/wham/usage",
			tokenInjected: true,
			want:          false,
		},
		{
			name:          "codex local management rejected",
			provider:      "codex",
			rawURL:        "https://127.0.0.1/v0/management/api-call",
			tokenInjected: true,
			want:          false,
		},
		{
			name:          "claude trusted",
			provider:      "claude",
			rawURL:        "https://api.anthropic.com/v1/organizations/usage_report/messages",
			tokenInjected: true,
			want:          true,
		},
		{
			name:          "gemini trusted",
			provider:      "gemini-cli",
			rawURL:        "https://cloudcode-pa.googleapis.com/v1internal:countTokens",
			tokenInjected: true,
			want:          true,
		},
		{
			name:          "kimi trusted",
			provider:      "kimi",
			rawURL:        "https://api.kimi.com/v1/users/me",
			tokenInjected: true,
			want:          true,
		},
		{
			name:          "xai trusted",
			provider:      "xai",
			rawURL:        "https://cli-chat-proxy.grok.com/v1/me",
			tokenInjected: true,
			want:          true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			parsedURL, errParse := url.Parse(tc.rawURL)
			if errParse != nil {
				t.Fatalf("parse url: %v", errParse)
			}
			auth := &coreauth.Auth{Provider: tc.provider}
			got := shouldMarkAPICallUnauthorized(auth, parsedURL, tc.tokenInjected)
			if got != tc.want {
				t.Fatalf("shouldMarkAPICallUnauthorized() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAPICallUnauthorizedFromTrustedProviderMarksAuthDisabled(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "trusted-api-call-auth",
		FileName: "trusted-api-call.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := &Handler{authManager: manager}
	requestURL, errParse := url.Parse("https://chatgpt.com/backend-api/wham/usage")
	if errParse != nil {
		t.Fatalf("parse url: %v", errParse)
	}
	h.markAPICallUnauthorized(context.Background(), auth, http.StatusUnauthorized, []byte("bad token"), requestURL, true)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after trusted api call", auth.ID)
	}
	if !updated.Disabled {
		t.Fatal("Disabled = false, want true after trusted provider API call 401")
	}
	if updated.Status != coreauth.StatusDisabled {
		t.Fatalf("Status = %q, want %q", updated.Status, coreauth.StatusDisabled)
	}
	if updated.LastError == nil || updated.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want HTTP 401", updated.LastError)
	}
}
