package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/imagesfallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func performImagesEndpointRequest(t *testing.T, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func assertUnsupportedImagesModelResponse(t *testing.T, resp *httptest.ResponseRecorder, model string) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model " + model + " is not supported on " + imagesGenerationsPath + " or " + imagesEditsPath + ". Use " + gptImage15Model + ", " + defaultImagesToolModel + ", " + defaultXAIImagesModel + ", " + xaiImagesQualityModel + ", " + xaiImages20Model + ", or a configured openai-compatibility image model."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func TestImagesModelValidationAllowsGPTImageAndXAIModels(t *testing.T) {
	for _, model := range []string{"gpt-image-1.5", "codex/gpt-image-1.5", "gpt-image-2", "codex/gpt-image-2", "grok-imagine-image", "xai/grok-imagine-image", "grok-imagine-image-quality", "xai/grok-imagine-image-quality", "grok-imagine-image-2.0", "xai/grok-imagine-image-2.0"} {
		if !isSupportedImagesModel(model) {
			t.Fatalf("expected %s to be supported", model)
		}
	}
	if isSupportedImagesModel("gpt-5.4-mini") {
		t.Fatal("expected gpt-5.4-mini to be rejected")
	}
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
		{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestCanonicalXAIImagesModelPreservesImage20(t *testing.T) {
	for _, model := range []string{"grok-imagine-image-2.0", "xai/grok-imagine-image-2.0", "XAI/Grok-Imagine-Image-2.0"} {
		if got := canonicalXAIImagesModel(model); got != xaiImages20Model {
			t.Fatalf("canonicalXAIImagesModel(%q) = %q, want %s", model, got, xaiImages20Model)
		}
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","aspect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"data:image/png;base64,AA==", "https://example.com/image.png"}, "b64_json", "3:2", "1k", 0)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", 0)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","mime_type":"image/png"}],"usage":{"total_tokens":0}}`)

	out, err := buildImagesAPIResponseFromXAI(payload, "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
	}
}

func TestImagesGenerationsDefaultModelFallsBackAfterRoutedProviderFailure(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	primaryExecutor := &failingImagesExecutor{id: "openai-compatibility", status: http.StatusInternalServerError}
	codexExecutor := &failingImagesExecutor{id: "codex", status: http.StatusInternalServerError}
	manager.RegisterExecutor(primaryExecutor)
	manager.RegisterExecutor(codexExecutor)

	primaryAuth := &coreauth.Auth{ID: "test-images-primary-auth", Provider: "openai-compatibility"}
	fallbackAuth := &coreauth.Auth{
		ID:       "test-images-codex-oauth",
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "images@example.com",
			"access_token": "token",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(primaryAuth.ID, "openai-compatibility", []*registry.ModelInfo{
		{ID: defaultImagesToolModel, Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
	})
	reg.RegisterClient(fallbackAuth.ID, "codex", []*registry.ModelInfo{
		{ID: defaultImagesToolModel, Object: "model", OwnedBy: "codex", Type: registry.OpenAIImageModelType},
		{ID: imagesfallback.TextAuthSelectionModel, Object: "model", OwnedBy: "codex", Type: "codex"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(primaryAuth.ID)
		reg.UnregisterClient(fallbackAuth.ID)
	})

	if _, err := manager.Register(context.Background(), primaryAuth); err != nil {
		t.Fatalf("register primary auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), fallbackAuth); err != nil {
		t.Fatalf("register fallback auth: %v", err)
	}

	fallbackService := &fakeImagesFallbackService{
		result: &imagesfallback.Result{
			CreatedAt: time.Now().Unix(),
			Images: []imagesfallback.GeneratedImage{
				{Data: []byte("image"), MIMEType: "image/png", RevisedPrompt: "refined"},
			},
		},
	}
	previousFactory := newImageFallbackService
	newImageFallbackService = func(*sdkconfig.SDKConfig, *coreauth.Manager) imageFallbackService {
		return fallbackService
	}
	t.Cleanup(func() {
		newImageFallbackService = previousFactory
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a square","response_format":"b64_json"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if primaryExecutor.Calls() != 1 {
		t.Fatalf("primary executor calls = %d, want 1", primaryExecutor.Calls())
	}
	if fallbackService.Calls() != 1 {
		t.Fatalf("fallback service calls = %d, want 1", fallbackService.Calls())
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "data.0.b64_json").String(); got != "aW1hZ2U=" {
		t.Fatalf("fallback b64_json = %q, want image payload", got)
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("revised_prompt = %q, want refined", got)
	}
}

func TestRoutedImageFallbackWebUnauthorizedKeepsAuthEnabledAndRestartsPrimaryWithNextAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	primaryExecutor := &authAwareImagesExecutor{
		failures: map[string]error{
			"aa-web-unauthorized": &testImagesStatusError{status: http.StatusInternalServerError, message: "primary image failed"},
		},
		response: []byte(`{"created":123,"data":[{"b64_json":"AA=="}]}`),
	}
	manager.RegisterExecutor(primaryExecutor)

	auths := []*coreauth.Auth{
		{ID: "aa-web-unauthorized", Provider: "codex", Metadata: map[string]any{"email": "a@example.com", "access_token": "token-a", "plan_type": "plus"}},
		{ID: "bb-primary-ready", Provider: "codex", Metadata: map[string]any{"email": "b@example.com", "access_token": "token-b", "plan_type": "plus"}},
	}
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
			{ID: defaultImagesToolModel, Object: "model", OwnedBy: "codex", Type: registry.OpenAIImageModelType},
			{ID: imagesfallback.TextAuthSelectionModel, Object: "model", OwnedBy: "codex", Type: "codex"},
		})
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			reg.UnregisterClient(auth.ID)
		}
	})

	fallbackService := &fakeImagesFallbackService{
		execute: func(authID string) (*imagesfallback.Result, error) {
			if authID != auths[0].ID {
				t.Fatalf("web fallback auth = %s, want only %s", authID, auths[0].ID)
			}
			return nil, &testImagesStatusError{status: http.StatusUnauthorized, message: "web image unauthorized"}
		},
	}
	previousFactory := newImageFallbackService
	newImageFallbackService = func(*sdkconfig.SDKConfig, *coreauth.Manager) imageFallbackService {
		return fallbackService
	}
	t.Cleanup(func() {
		newImageFallbackService = previousFactory
	})

	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	attempt, errMsg := handler.executeRoutedImageAttemptsAcrossCodexAuths(
		context.Background(),
		[]byte(`{"model":"gpt-image-2","prompt":"draw"}`),
		defaultImagesToolModel,
		imagesfallback.Request{RequestedModel: defaultImagesToolModel, Prompt: "draw"},
		nil,
	)
	if errMsg != nil {
		t.Fatalf("executeRoutedImageAttemptsAcrossCodexAuths() error = %v", errMsg.Error)
	}
	if attempt == nil || attempt.usedFallback {
		t.Fatalf("attempt = %#v, want next auth primary result", attempt)
	}
	if got := string(attempt.payload); got != string(primaryExecutor.response) {
		t.Fatalf("payload = %s, want %s", got, primaryExecutor.response)
	}
	if got := strings.Join(primaryExecutor.authIDs, ","); got != "aa-web-unauthorized,bb-primary-ready" {
		t.Fatalf("primary auth sequence = %s", got)
	}
	if got := strings.Join(fallbackService.authIDs, ","); got != "aa-web-unauthorized" {
		t.Fatalf("web fallback auth sequence = %s", got)
	}

	firstUpdated, ok := manager.GetByID(auths[0].ID)
	if !ok || firstUpdated == nil {
		t.Fatalf("missing auth %s", auths[0].ID)
	}
	if firstUpdated.Disabled || firstUpdated.Status == coreauth.StatusDisabled {
		t.Fatalf("web 401 disabled auth: %#v", firstUpdated)
	}
}

func TestRoutedImageStreamFallbackWebUnauthorizedRestartsPrimaryWithNextAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	primaryExecutor := &authAwareImagesExecutor{
		failures: map[string]error{
			"aa-stream-web-unauthorized": &testImagesStatusError{status: http.StatusInternalServerError, message: "primary image stream failed"},
		},
		response: []byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\"}\n\n"),
	}
	manager.RegisterExecutor(primaryExecutor)

	auths := []*coreauth.Auth{
		{ID: "aa-stream-web-unauthorized", Provider: "codex", Metadata: map[string]any{"email": "a@example.com", "access_token": "token-a", "plan_type": "plus"}},
		{ID: "bb-stream-primary-ready", Provider: "codex", Metadata: map[string]any{"email": "b@example.com", "access_token": "token-b", "plan_type": "plus"}},
	}
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
			{ID: defaultImagesToolModel, Object: "model", OwnedBy: "codex", Type: registry.OpenAIImageModelType},
			{ID: imagesfallback.TextAuthSelectionModel, Object: "model", OwnedBy: "codex", Type: "codex"},
		})
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	t.Cleanup(func() {
		for _, auth := range auths {
			reg.UnregisterClient(auth.ID)
		}
	})

	fallbackService := &fakeImagesFallbackService{
		execute: func(authID string) (*imagesfallback.Result, error) {
			if authID != auths[0].ID {
				t.Fatalf("web fallback auth = %s, want only %s", authID, auths[0].ID)
			}
			return nil, &testImagesStatusError{status: http.StatusUnauthorized, message: "web image unauthorized"}
		},
	}
	previousFactory := newImageFallbackService
	newImageFallbackService = func(*sdkconfig.SDKConfig, *coreauth.Manager) imageFallbackService {
		return fallbackService
	}
	t.Cleanup(func() {
		newImageFallbackService = previousFactory
	})

	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	attempt, errMsg := handler.executeRoutedImageStreamAttemptsAcrossCodexAuths(
		context.Background(),
		[]byte(`{"model":"gpt-image-2","prompt":"draw","stream":true}`),
		defaultImagesToolModel,
		imagesfallback.Request{RequestedModel: defaultImagesToolModel, Prompt: "draw"},
		nil,
	)
	if errMsg != nil {
		t.Fatalf("executeRoutedImageStreamAttemptsAcrossCodexAuths() error = %v", errMsg.Error)
	}
	if attempt == nil || attempt.usedFallback {
		t.Fatalf("attempt = %#v, want next auth primary stream", attempt)
	}
	if got := string(attempt.firstChunk); got != string(primaryExecutor.response) {
		t.Fatalf("first chunk = %q, want %q", got, primaryExecutor.response)
	}
	if got := strings.Join(primaryExecutor.streamAuthIDs, ","); got != "aa-stream-web-unauthorized,bb-stream-primary-ready" {
		t.Fatalf("primary stream auth sequence = %s", got)
	}
	if got := strings.Join(fallbackService.authIDs, ","); got != "aa-stream-web-unauthorized" {
		t.Fatalf("web fallback auth sequence = %s", got)
	}

	firstUpdated, ok := manager.GetByID(auths[0].ID)
	if !ok || firstUpdated == nil || firstUpdated.Disabled || firstUpdated.Status == coreauth.StatusDisabled {
		t.Fatalf("web stream 401 changed auth health: %#v", firstUpdated)
	}
}

func TestImagesPrimaryUnauthorizedRotatesWithoutDisablingCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetRetryConfig(0, 0, 0)
			firstAuthID := "aa-primary-image-unauthorized-" + name
			secondAuthID := "bb-primary-image-ready-" + name
			primaryExecutor := &authAwareImagesExecutor{
				failures: map[string]error{
					firstAuthID: &testImagesStatusError{status: http.StatusUnauthorized, message: "primary image unauthorized"},
				},
				refreshErr: &testImagesStatusError{status: http.StatusUnauthorized, message: "refresh unauthorized"},
				response:   []byte(`{"created":123,"data":[{"b64_json":"AA=="}]}`),
			}
			if stream {
				primaryExecutor.response = []byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\"}\n\n")
			}
			manager.RegisterExecutor(primaryExecutor)

			auths := []*coreauth.Auth{
				{ID: firstAuthID, Provider: "codex", Metadata: map[string]any{"email": "a@example.com", "access_token": "token-a", "refresh_token": "refresh-a", "plan_type": "plus"}},
				{ID: secondAuthID, Provider: "codex", Metadata: map[string]any{"email": "b@example.com", "access_token": "token-b", "refresh_token": "refresh-b", "plan_type": "plus"}},
			}
			reg := registry.GetGlobalRegistry()
			for _, auth := range auths {
				reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
					{ID: defaultImagesToolModel, Object: "model", OwnedBy: "codex", Type: registry.OpenAIImageModelType},
					{ID: imagesfallback.TextAuthSelectionModel, Object: "model", OwnedBy: "codex", Type: "codex"},
				})
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("register %s: %v", auth.ID, errRegister)
				}
			}
			t.Cleanup(func() {
				for _, auth := range auths {
					reg.UnregisterClient(auth.ID)
				}
			})

			fallbackService := &fakeImagesFallbackService{}
			previousFactory := newImageFallbackService
			newImageFallbackService = func(*sdkconfig.SDKConfig, *coreauth.Manager) imageFallbackService {
				return fallbackService
			}
			t.Cleanup(func() {
				newImageFallbackService = previousFactory
			})

			handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
			requestBody := `{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json"}`
			if stream {
				requestBody = `{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json","stream":true}`
			}
			resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", strings.NewReader(requestBody), handler.ImagesGenerations)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
			}

			primarySequence := primaryExecutor.authIDs
			if stream {
				primarySequence = primaryExecutor.streamAuthIDs
			}
			if got := strings.Join(primarySequence, ","); got != firstAuthID+","+secondAuthID {
				t.Fatalf("primary auth sequence = %s, want %s,%s", got, firstAuthID, secondAuthID)
			}
			if len(primaryExecutor.refreshAuthIDs) != 0 {
				t.Fatalf("image 401 triggered OAuth refresh: %v", primaryExecutor.refreshAuthIDs)
			}
			if fallbackService.Calls() != 0 {
				t.Fatalf("fallback calls = %d, want 0 after next primary succeeds", fallbackService.Calls())
			}

			firstUpdated, ok := manager.GetByID(firstAuthID)
			if !ok || firstUpdated == nil || firstUpdated.Disabled || firstUpdated.Status == coreauth.StatusDisabled {
				t.Fatalf("primary image 401 disabled auth: %#v", firstUpdated)
			}
		})
	}
}

func TestImagesGenerationsRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsJSONRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit this"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesGenerations_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesGenerations_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

type failingImagesExecutor struct {
	id     string
	status int
	calls  int
}

type authAwareImagesExecutor struct {
	failures       map[string]error
	refreshErr     error
	response       []byte
	authIDs        []string
	streamAuthIDs  []string
	refreshAuthIDs []string
}

func (e *authAwareImagesExecutor) Identifier() string { return "codex" }

func (e *authAwareImagesExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authIDs = append(e.authIDs, auth.ID)
	if err := e.failures[auth.ID]; err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: e.response}, nil
}

func (e *authAwareImagesExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamAuthIDs = append(e.streamAuthIDs, auth.ID)
	if err := e.failures[auth.ID]; err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: e.response}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *authAwareImagesExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refreshAuthIDs = append(e.refreshAuthIDs, auth.ID)
	if e.refreshErr != nil {
		return nil, e.refreshErr
	}
	return auth, nil
}

func (e *authAwareImagesExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *authAwareImagesExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *failingImagesExecutor) Identifier() string {
	return e.id
}

func (e *failingImagesExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls++
	return cliproxyexecutor.Response{}, &testImagesStatusError{status: e.status, message: "primary image provider failed"}
}

func (e *failingImagesExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls++
	return nil, &testImagesStatusError{status: e.status, message: "primary image provider failed"}
}

func (e *failingImagesExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *failingImagesExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &testImagesStatusError{status: e.status, message: "primary image provider failed"}
}

func (e *failingImagesExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *failingImagesExecutor) Calls() int {
	return e.calls
}

type testImagesStatusError struct {
	status  int
	message string
}

func (e *testImagesStatusError) Error() string {
	return e.message
}

func (e *testImagesStatusError) StatusCode() int {
	return e.status
}

type fakeImagesFallbackService struct {
	result  *imagesfallback.Result
	err     error
	execute func(string) (*imagesfallback.Result, error)
	authIDs []string
	calls   int
}

func (s *fakeImagesFallbackService) Execute(_ context.Context, authID string, _ imagesfallback.Request) (*imagesfallback.Result, error) {
	s.calls++
	s.authIDs = append(s.authIDs, authID)
	if s.execute != nil {
		return s.execute(authID)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *fakeImagesFallbackService) ExecuteWithAuthManager(_ context.Context, _ imagesfallback.Request, selectedCallback func(string)) (*imagesfallback.Result, error) {
	s.calls++
	if selectedCallback != nil {
		selectedCallback("test-images-codex-oauth")
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *fakeImagesFallbackService) Calls() int {
	return s.calls
}

func TestSSEFrameAccumulatorFlushesDataOnlyFrame(t *testing.T) {
	accumulator := &sseFrameAccumulator{}
	chunk := []byte(`data: {"type":"image_generation.partial","partial_image_index":0}`)

	if frames := accumulator.AddChunk(chunk); len(frames) != 0 {
		t.Fatalf("AddChunk() emitted an unterminated data-only frame: %q", frames)
	}
	frames := accumulator.Flush()
	if len(frames) != 1 || string(frames[0]) != string(chunk) {
		t.Fatalf("Flush() frames = %q, want [%q]", frames, chunk)
	}
}

func TestWriteImagesStreamErrorEventSanitizesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	raw := `{"error":{"code":"upstream_failed","message":"token=image-secret"},"debug":"` + strings.Repeat("x", 8192) + `"}`
	writeImagesStreamErrorEvent(c, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New(raw)})

	body := recorder.Body.String()
	if strings.Contains(body, "image-secret") || len(body) > 4096 || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("image stream error was not safely bounded: len=%d body=%q", len(body), body)
	}
}

func TestCollectImagesRejectsPayloadErrorBeforeCompleted(t *testing.T) {
	data := make(chan []byte, 1)
	data <- []byte("event: error\ndata: {\"type\":\"provider.error\",\"error\":{\"code\":\"failed\",\"message\":\"token=image-secret\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}]}}\n\n")
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
	if len(out) != 0 || errMsg == nil || errMsg.Error == nil {
		t.Fatalf("payload error result out=%q err=%#v", out, errMsg)
	}
	if strings.Contains(errMsg.Error.Error(), "image-secret") || !strings.Contains(errMsg.Error.Error(), "[REDACTED]") {
		t.Fatalf("payload error was not sanitized: %q", errMsg.Error.Error())
	}
}

func TestForwardImagesStreamCancelsWithPayloadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}
	data := make(chan []byte)
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)
	var canceled error
	firstChunk := []byte("event: error\ndata: {\"error\":{\"message\":\"token=image-secret\"}}\n\n")

	h.forwardImagesStream(context.Background(), c, flusher, func(err error) { canceled = err }, data, errs, firstChunk, "b64_json", "image_generation", func(string, []byte) {})
	if canceled == nil || strings.Contains(canceled.Error(), "image-secret") || !strings.Contains(canceled.Error(), "[REDACTED]") {
		t.Fatalf("payload error cancel = %v body=%q", canceled, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("payload error event missing: %q", recorder.Body.String())
	}
}

func TestForwardRawImageStreamPrefersPendingErrorOnClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	for i := 0; i < 100; i++ {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		data := make(chan []byte)
		close(data)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("image upstream busy")}
		close(errs)
		var canceled error

		h.forwardRawImageStream(context.Background(), c, func(err error) { canceled = err }, data, errs)
		if canceled == nil || !strings.Contains(canceled.Error(), "image upstream busy") {
			t.Fatalf("iteration %d: cancel=%v body=%q", i, canceled, recorder.Body.String())
		}
	}
}

func TestCollectImagesPrefersPendingErrorWhenDataChannelCloses(t *testing.T) {
	for i := 0; i < 100; i++ {
		data := make(chan []byte)
		close(data)
		errs := make(chan *interfaces.ErrorMessage, 1)
		want := &interfaces.ErrorMessage{
			StatusCode:     http.StatusTooManyRequests,
			Error:          errors.New("image upstream busy"),
			DirectResponse: true,
			Headers:        http.Header{"Retry-After": []string{"9"}},
		}
		errs <- want
		close(errs)

		_, got := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
		if got != want {
			t.Fatalf("iteration %d: pending error = %#v, want original %#v", i, got, want)
		}
	}
}

func TestCollectImagesAllowsMultilineSSEData(t *testing.T) {
	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\n" +
		"data: \"response\":{\"created_at\":1,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}]}}\n\n")
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesStream() error = %v", errMsg.Error)
	}
	if !strings.Contains(string(out), `"b64_json":"aW1hZ2U="`) {
		t.Fatalf("multiline image response = %q", out)
	}
}

func TestSSEFrameAccumulatorKeepsMultipleFramesDistinct(t *testing.T) {
	accumulator := &sseFrameAccumulator{}
	first := "event: first\ndata: {\"type\":\"first\"}\n\n"
	second := "event: second\ndata: {\"type\":\"second\"}\n\n"

	frames := accumulator.AddChunk([]byte(first + second))
	if len(frames) != 2 {
		t.Fatalf("AddChunk() returned %d frames, want 2: %q", len(frames), frames)
	}
	if string(frames[0]) != first || string(frames[1]) != second {
		t.Fatalf("frames were overwritten during buffer compaction: %q", frames)
	}
}
